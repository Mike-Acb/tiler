// Package nlscdownload downloads NLSC MODELSET NODE responses into a
// resumable on-disk quadtree archive.
package nlscdownload

import (
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Node identifies one node in the MODELSET quadtree.
type Node struct {
	Level int `json:"level"`
	Row   int `json:"row"`
	Col   int `json:"col"`
}

// GeographicBounds limits recursive traversal to a longitude/latitude box.
// Nodes at the boundary are included so no requested geometry is clipped.
type GeographicBounds struct {
	West  float64
	South float64
	East  float64
	North float64
}

// Event reports the final result for one node.
type Event struct {
	Node    Node
	Status  string
	Bytes   int64
	Retries int
	Err     error
}

// Config controls a recursive MODELSET download.
type Config struct {
	BaseURL     string
	Layer       string
	OutputDir   string
	Root        Node
	MaxLevel    int
	Bounds      *GeographicBounds
	Concurrency int
	Retries     int
	RetryDelay  time.Duration
	Timeout     time.Duration // Used only when Client is nil.
	Client      *http.Client
	OnEvent     func(Event)
}

// Stats summarizes one download run.
type Stats struct {
	Visited    int
	Requested  int
	Downloaded int
	Empty      int
	Skipped    int
	Failed     int
	Retried    int
	Bytes      int64
}

type nodeResult struct {
	Event
	empty     bool
	skipped   bool
	requested bool
}

// Download traverses from Config.Root through Config.MaxLevel. HTTP 204 nodes
// are recorded as empty and prune their descendants. Existing response and
// empty-marker files are checkpoints and are not requested again.
func Download(ctx context.Context, config Config) (Stats, error) {
	var stats Stats
	config, err := normalizeConfig(config)
	if err != nil {
		return stats, err
	}
	releaseLock, err := acquireArchiveLock(config.OutputDir)
	if err != nil {
		return stats, err
	}
	defer releaseLock()
	if err := ensureArchiveConfig(config); err != nil {
		return stats, err
	}

	current := []Node{config.Root}
	failureCount := 0
	var firstFailure error
	for len(current) > 0 {
		results := downloadLevel(ctx, config, current)
		next := make([]Node, 0, len(current)*4)
		for result := range results {
			intersects := true
			if result.Err == nil && !result.empty && result.Node.Level < config.MaxLevel && config.Bounds != nil {
				responsePath, _ := nodePaths(config.OutputDir, result.Node)
				intersects, result.Err = responseIntersectsBounds(responsePath, *config.Bounds)
				if result.Err != nil {
					result.Err = fmt.Errorf("read geographic bounds: %w", result.Err)
				}
			}
			stats.Visited++
			if result.requested {
				stats.Requested++
			}
			stats.Retried += result.Retries
			stats.Bytes += result.Bytes
			result.Status = "downloaded"
			switch {
			case result.Err != nil:
				stats.Failed++
				result.Status = "failed"
				failureCount++
				if firstFailure == nil {
					firstFailure = fmt.Errorf("node %d/%d/%d: %w", result.Node.Level, result.Node.Row, result.Node.Col, result.Err)
				}
			case result.skipped:
				stats.Skipped++
				result.Status = "skipped"
			case result.empty:
				stats.Empty++
				result.Status = "empty"
			default:
				stats.Downloaded++
			}
			if config.OnEvent != nil {
				config.OnEvent(result.Event)
			}
			if result.Err == nil && !result.empty && intersects && result.Node.Level < config.MaxLevel {
				next = append(next, children(result.Node)...)
			}
		}
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		current = next
	}
	if failureCount > 0 {
		return stats, fmt.Errorf("%d nodes failed; rerun the same command to resume; first failure: %w", failureCount, firstFailure)
	}
	return stats, nil
}

type archiveConfig struct {
	Version int    `json:"version"`
	BaseURL string `json:"baseUrl"`
	Layer   string `json:"layer"`
	Root    Node   `json:"root"`
}

func ensureArchiveConfig(config Config) error {
	want := archiveConfig{Version: 1, BaseURL: config.BaseURL, Layer: config.Layer, Root: config.Root}
	path := filepath.Join(config.OutputDir, "download-config.json")
	data, err := os.ReadFile(path)
	if err == nil {
		var got archiveConfig
		if err := json.Unmarshal(data, &got); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
		if got.Version != want.Version || got.BaseURL != want.BaseURL || got.Layer != want.Layer || !isDescendantOrSelf(got.Root, config.Root) {
			return fmt.Errorf("output directory belongs to a different dataset: got layer=%q root=%+v base=%q", got.Layer, got.Root, got.BaseURL)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	entries, err := os.ReadDir(config.OutputDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != "download-config.json.part" && entry.Name() != ".download.lock" {
			return errors.New("output directory is not empty and has no download-config.json; use an empty directory or restore the matching manifest")
		}
	}
	_ = os.Remove(path + ".part")
	data, err = json.MarshalIndent(want, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(data, '\n'))
}

func isDescendantOrSelf(root, node Node) bool {
	if node.Level < root.Level {
		return false
	}
	shift := node.Level - root.Level
	return node.Row>>shift == root.Row && node.Col>>shift == root.Col
}

func acquireArchiveLock(outputDir string) (func(), error) {
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(outputDir, ".download.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("output archive is already being written or the filesystem does not support locking (%s): %w", outputDir, err)
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

func normalizeConfig(config Config) (Config, error) {
	if config.BaseURL == "" {
		return config, errors.New("base URL is required")
	}
	if _, err := url.ParseRequestURI(config.BaseURL); err != nil {
		return config, fmt.Errorf("invalid base URL: %w", err)
	}
	if config.Layer == "" {
		return config, errors.New("layer is required")
	}
	if config.OutputDir == "" {
		return config, errors.New("output directory is required")
	}
	if config.Root.Level < 1 || config.Root.Row < 0 || config.Root.Col < 0 {
		return config, errors.New("root level must be positive and row/col cannot be negative")
	}
	if config.MaxLevel < config.Root.Level {
		return config, errors.New("max level cannot be below the root level")
	}
	if config.Bounds != nil {
		bounds := *config.Bounds
		if bounds.West < -180 || bounds.East > 180 || bounds.South < -90 || bounds.North > 90 || bounds.West >= bounds.East || bounds.South >= bounds.North {
			return config, fmt.Errorf("invalid geographic bounds: west=%g south=%g east=%g north=%g", bounds.West, bounds.South, bounds.East, bounds.North)
		}
	}
	if config.Concurrency <= 0 {
		config.Concurrency = 1
	}
	if config.Retries < 0 {
		return config, errors.New("retries cannot be negative")
	}
	if config.RetryDelay < 0 {
		return config, errors.New("retry delay cannot be negative")
	}
	if config.Client == nil {
		if config.Timeout <= 0 {
			config.Timeout = 90 * time.Second
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.DisableCompression = true
		config.Client = &http.Client{Transport: transport, Timeout: config.Timeout}
	}
	return config, nil
}

func responseIntersectsBounds(path string, target GeographicBounds) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	reader, err := gzip.NewReader(file)
	if err != nil {
		return false, err
	}
	defer reader.Close()

	read := func(value interface{}) error { return binary.Read(reader, binary.LittleEndian, value) }
	var level, row, col int32
	if err := read(&level); err != nil {
		return false, err
	}
	if err := read(&row); err != nil {
		return false, err
	}
	if err := read(&col); err != nil {
		return false, err
	}
	if _, err := io.CopyN(io.Discard, reader, 3*8+8*3*8); err != nil {
		return false, err
	}
	var childCount int32
	if err := read(&childCount); err != nil {
		return false, err
	}
	if childCount < 0 || childCount > 4 {
		return false, fmt.Errorf("invalid child count %d", childCount)
	}
	if _, err := io.CopyN(io.Discard, reader, int64(childCount)*4); err != nil {
		return false, err
	}
	var entityCount uint16
	if err := read(&entityCount); err != nil {
		return false, err
	}
	if _, err := io.CopyN(io.Discard, reader, int64(entityCount)*4+8); err != nil {
		return false, err
	}

	node := GeographicBounds{West: math.Inf(1), South: math.Inf(1), East: math.Inf(-1), North: math.Inf(-1)}
	for point := 0; point < 4; point++ {
		var x, y, z float64
		if err := read(&x); err != nil {
			return false, err
		}
		if err := read(&y); err != nil {
			return false, err
		}
		if err := read(&z); err != nil {
			return false, err
		}
		if !finite(x) || !finite(y) || !finite(z) {
			return false, fmt.Errorf("boundary point %d contains a non-finite ECEF coordinate", point)
		}
		longitude, latitude := ecefLongitudeLatitude(x, y, z)
		if !finite(longitude) || !finite(latitude) {
			return false, fmt.Errorf("boundary point %d converts to a non-finite longitude/latitude", point)
		}
		if longitude < node.West {
			node.West = longitude
		}
		if longitude > node.East {
			node.East = longitude
		}
		if latitude < node.South {
			node.South = latitude
		}
		if latitude > node.North {
			node.North = latitude
		}
	}
	return node.East >= target.West && node.West <= target.East && node.North >= target.South && node.South <= target.North, nil
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func ecefLongitudeLatitude(x, y, z float64) (float64, float64) {
	const semiMajor = 6378137.0
	const eccentricitySquared = 6.69437999014e-3
	longitude := math.Atan2(y, x)
	horizontal := math.Hypot(x, y)
	latitude := math.Atan2(z, horizontal*(1-eccentricitySquared))
	for iteration := 0; iteration < 8; iteration++ {
		sine := math.Sin(latitude)
		normal := semiMajor / math.Sqrt(1-eccentricitySquared*sine*sine)
		latitude = math.Atan2(z+eccentricitySquared*normal*sine, horizontal)
	}
	return longitude * 180 / math.Pi, latitude * 180 / math.Pi
}

func downloadLevel(ctx context.Context, config Config, nodes []Node) <-chan nodeResult {
	jobs := make(chan Node)
	results := make(chan nodeResult, len(nodes))
	workerCount := config.Concurrency
	if workerCount > len(nodes) {
		workerCount = len(nodes)
	}
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for i := 0; i < workerCount; i++ {
		go func() {
			defer workers.Done()
			for node := range jobs {
				results <- downloadNode(ctx, config, node)
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, node := range nodes {
			select {
			case jobs <- node:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		workers.Wait()
		close(results)
	}()
	return results
}

func downloadNode(ctx context.Context, config Config, node Node) nodeResult {
	responsePath, emptyPath := nodePaths(config.OutputDir, node)
	if info, err := os.Stat(responsePath); err == nil && info.Size() > 0 {
		return nodeResult{Event: Event{Node: node}, skipped: true}
	}
	if _, err := os.Stat(emptyPath); err == nil {
		return nodeResult{Event: Event{Node: node}, empty: true, skipped: true}
	}

	result := nodeResult{Event: Event{Node: node}, requested: true}
	for attempt := 0; attempt <= config.Retries; attempt++ {
		if attempt > 0 {
			result.Retries++
			if err := waitForRetry(ctx, config.RetryDelay); err != nil {
				result.Err = err
				return result
			}
		}
		result.empty, result.Bytes, result.Err = requestNode(ctx, config, node, responsePath, emptyPath)
		if result.Err == nil {
			return result
		}
	}
	return result
}

func requestNode(ctx context.Context, config Config, node Node, responsePath, emptyPath string) (bool, int64, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, nodeURL(config.BaseURL, config.Layer, node), nil)
	if err != nil {
		return false, 0, err
	}
	// Explicitly requesting gzip preserves the original encoded NODE response;
	// Go only auto-decompresses when it adds this header itself.
	request.Header.Set("Accept-Encoding", "gzip")
	request.Header.Set("User-Agent", "tiler-nlsc-downloader/1.0")
	response, err := config.Client.Do(request)
	if err != nil {
		return false, 0, err
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusNoContent {
		if err := writeEmptyMarker(emptyPath); err != nil {
			return true, 0, err
		}
		_ = os.Remove(responsePath + ".part")
		return true, 0, nil
	}
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, response.Body)
		return false, 0, fmt.Errorf("HTTP %s", response.Status)
	}
	if err := os.MkdirAll(filepath.Dir(responsePath), 0o755); err != nil {
		return false, 0, err
	}
	partPath := responsePath + ".part"
	file, err := os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return false, 0, err
	}
	size, copyErr := io.Copy(file, response.Body)
	syncErr := file.Sync()
	closeErr := file.Close()
	if copyErr != nil {
		return false, size, copyErr
	}
	if syncErr != nil {
		return false, size, syncErr
	}
	if closeErr != nil {
		return false, size, closeErr
	}
	if size == 0 {
		return false, 0, errors.New("HTTP 200 response was empty")
	}
	if err := validateGzip(partPath); err != nil {
		return false, size, fmt.Errorf("invalid gzip NODE response: %w", err)
	}
	if err := os.Rename(partPath, responsePath); err != nil {
		return false, size, err
	}
	return false, size, nil
}

func validateGzip(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	reader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer reader.Close()
	_, err = io.Copy(io.Discard, reader)
	return err
}

func nodeURL(baseURL, layer string, node Node) string {
	parsed, _ := url.Parse(baseURL) // normalizeConfig already validates BaseURL.
	query := parsed.Query()
	query.Set("type", "modelset")
	query.Set("format", "integrate")
	query.Set("name", layer)
	query.Set("level", strconv.Itoa(node.Level))
	query.Set("Row", strconv.Itoa(node.Row))
	query.Set("Col", strconv.Itoa(node.Col))
	query.Set("docname", "NODE")
	query.Set("epsg", "4326")
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func nodePaths(outputDir string, node Node) (string, string) {
	level := strconv.Itoa(node.Level)
	row := strconv.Itoa(node.Row)
	col := strconv.Itoa(node.Col)
	return filepath.Join(outputDir, "raw", level, row, col+".response"),
		filepath.Join(outputDir, "empty", level, row, col+".empty")
}

func writeEmptyMarker(path string) error {
	return atomicWrite(path, nil)
}

func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path+".part", data, 0o644); err != nil {
		return err
	}
	return os.Rename(path+".part", path)
}

func children(parent Node) []Node {
	level := parent.Level + 1
	row := parent.Row * 2
	col := parent.Col * 2
	return []Node{
		{Level: level, Row: row, Col: col},
		{Level: level, Row: row, Col: col + 1},
		{Level: level, Row: row + 1, Col: col},
		{Level: level, Row: row + 1, Col: col + 1},
	}
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	select {
	case <-time.After(delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
