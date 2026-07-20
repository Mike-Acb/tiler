package nlscmodelset

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

type BuildOptions struct {
	InputDir     string
	OutputDir    string
	Workers      int
	MinLevel     int
	MaxLevel     int
	Limit        int
	FinalizeOnly bool
	// AllowMissingChildren permits whole branches to stop before MaxLevel. A
	// partially present sibling set is still rejected because REPLACE would
	// otherwise remove the parent and leave holes.
	AllowMissingChildren bool
	Format               RawFormat
	Progress             io.Writer
}

type BuildStats struct {
	Discovered int
	Converted  int
	Skipped    int
	NoModel    int
	Failed     int
}

type nodeRecord struct {
	Level          int      `json:"level"`
	Row            int      `json:"row"`
	Col            int      `json:"col"`
	Offset         Vec3     `json:"offset"`
	AABox          Box      `json:"aaBox"`
	ChildrenIDs    []string `json:"childrenIds,omitempty"`
	Resolution     float64  `json:"resolution"`
	EntityCount    int      `json:"entityCount"`
	ContentURIs    []string `json:"contentUris,omitempty"`
	AttributeBytes int      `json:"attributeBytes,omitempty"`
	SourceSize     int64    `json:"sourceSize"`
	SourceModTime  int64    `json:"sourceModTime"`
}

type buildConfig struct {
	Version              int    `json:"version"`
	SourceManifestSHA256 string `json:"sourceManifestSha256"`
	FormatVersion        int    `json:"formatVersion"`
	VertexSize           int    `json:"vertexSize"`
	MinLevel             int    `json:"minLevel"`
	MaxLevel             int    `json:"maxLevel"`
}

type buildJob struct {
	path   string
	level  int
	row    int
	col    int
	marker string
}

type buildResult struct {
	record  nodeRecord
	status  string
	err     error
	source  string
	written int64
}

// BuildArchive converts downloaded NODE responses one at a time. Completed
// node records are durable resume markers, so interrupted SMB builds can be
// restarted without scanning or rewriting finished GLBs.
func BuildArchive(ctx context.Context, options BuildOptions) (BuildStats, error) {
	var stats BuildStats
	if options.InputDir == "" || options.OutputDir == "" {
		return stats, errors.New("input and output directories are required")
	}
	if options.Workers <= 0 {
		options.Workers = 2
	}
	if options.MinLevel <= 0 {
		options.MinLevel = 1
	}
	if options.MaxLevel <= 0 {
		options.MaxLevel = 9
	}
	if options.MaxLevel < options.MinLevel {
		return stats, errors.New("max level cannot be smaller than min level")
	}
	if options.Format.Version == 0 && options.Format.VertexSize == 0 {
		options.Format = Taoyuan113HFormat
	}
	if err := os.MkdirAll(options.OutputDir, 0755); err != nil {
		return stats, fmt.Errorf("create output directory: %w", err)
	}
	lock, err := acquireBuildLock(options.OutputDir)
	if err != nil {
		return stats, err
	}
	defer lock.close()
	if err := ensureBuildConfig(options); err != nil {
		return stats, err
	}
	if options.FinalizeOnly {
		return finalizeArchive(ctx, options)
	}
	jobs, err := discoverBuildJobs(ctx, options)
	if err != nil {
		return stats, err
	}
	stats.Discovered = len(jobs)
	if len(jobs) == 0 {
		return stats, errors.New("no NODE responses matched the requested levels")
	}

	jobChannel := make(chan buildJob)
	resultChannel := make(chan buildResult)
	var workers sync.WaitGroup
	for worker := 0; worker < options.Workers; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for job := range jobChannel {
				select {
				case <-ctx.Done():
					return
				default:
				}
				resultChannel <- buildOneNode(ctx, job, options)
			}
		}()
	}
	go func() {
		defer close(jobChannel)
		for _, job := range jobs {
			select {
			case jobChannel <- job:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		workers.Wait()
		close(resultChannel)
	}()

	records := make([]nodeRecord, 0, len(jobs))
	processed := 0
	var firstError error
	for result := range resultChannel {
		processed++
		switch result.status {
		case "converted":
			stats.Converted++
		case "skipped":
			stats.Skipped++
		case "no-model":
			stats.Converted++
			stats.NoModel++
		case "failed":
			stats.Failed++
			if firstError == nil {
				firstError = fmt.Errorf("%s: %w", result.source, result.err)
			}
		}
		if result.status != "failed" {
			records = append(records, result.record)
		}
		if options.Progress != nil && (processed%100 == 0 || processed == len(jobs)) {
			fmt.Fprintf(options.Progress, "processed=%d/%d converted=%d skipped=%d noModel=%d failed=%d\n",
				processed, len(jobs), stats.Converted, stats.Skipped, stats.NoModel, stats.Failed)
		}
	}
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	if firstError != nil {
		return stats, fmt.Errorf("conversion finished with %d failed nodes; first error: %w", stats.Failed, firstError)
	}
	tileset, err := buildArchiveTileset(records, options)
	if err != nil {
		return stats, err
	}
	if err := atomicWrite(filepath.Join(options.OutputDir, "tileset.json"), tileset, 0644); err != nil {
		return stats, fmt.Errorf("write tileset: %w", err)
	}
	return stats, nil
}

func finalizeArchive(ctx context.Context, options BuildOptions) (BuildStats, error) {
	var stats BuildStats
	jobs, err := discoverMarkerJobs(ctx, options)
	if err != nil {
		return stats, err
	}
	stats.Discovered = len(jobs)
	if len(jobs) == 0 {
		return stats, errors.New("no node markers found to finalize")
	}

	jobChannel := make(chan buildJob)
	resultChannel := make(chan buildResult)
	var workers sync.WaitGroup
	for worker := 0; worker < options.Workers; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for job := range jobChannel {
				data, err := os.ReadFile(job.marker)
				var record nodeRecord
				if err == nil {
					err = json.Unmarshal(data, &record)
				}
				if err == nil && (record.Level != job.level || record.Row != job.row || record.Col != job.col) {
					err = fmt.Errorf("marker address %d/%d/%d does not match path %d/%d/%d", record.Level, record.Row, record.Col, job.level, job.row, job.col)
				}
				resultChannel <- buildResult{record: record, source: job.marker, err: err}
			}
		}()
	}
	go func() {
		defer close(jobChannel)
		for _, job := range jobs {
			select {
			case jobChannel <- job:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		workers.Wait()
		close(resultChannel)
	}()

	records := make([]nodeRecord, 0, len(jobs))
	processed := 0
	var firstError error
	for result := range resultChannel {
		processed++
		if result.err != nil {
			stats.Failed++
			if firstError == nil {
				firstError = fmt.Errorf("%s: %w", result.source, result.err)
			}
		} else {
			stats.Skipped++
			if len(result.record.ContentURIs) == 0 {
				stats.NoModel++
			}
			records = append(records, result.record)
		}
		if options.Progress != nil && (processed%100 == 0 || processed == len(jobs)) {
			fmt.Fprintf(options.Progress, "finalized=%d/%d noModel=%d failed=%d\n", processed, len(jobs), stats.NoModel, stats.Failed)
		}
	}
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	if firstError != nil {
		return stats, fmt.Errorf("finalization found %d invalid markers; first error: %w", stats.Failed, firstError)
	}
	tileset, err := buildArchiveTileset(records, options)
	if err != nil {
		return stats, err
	}
	if err := atomicWrite(filepath.Join(options.OutputDir, "tileset.json"), tileset, 0644); err != nil {
		return stats, fmt.Errorf("write tileset: %w", err)
	}
	return stats, nil
}

func discoverMarkerJobs(ctx context.Context, options BuildOptions) ([]buildJob, error) {
	markerRoot := filepath.Join(options.OutputDir, "nodes")
	jobs := make([]buildJob, 0)
	err := filepath.WalkDir(markerRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		relative, err := filepath.Rel(markerRoot, path)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(relative), "/")
		if len(parts) != 3 {
			return fmt.Errorf("unexpected marker path %q", relative)
		}
		level, err := strconv.Atoi(parts[0])
		if err != nil {
			return fmt.Errorf("parse level in %q: %w", relative, err)
		}
		if level < options.MinLevel || level > options.MaxLevel {
			return nil
		}
		row, err := strconv.Atoi(parts[1])
		if err != nil {
			return fmt.Errorf("parse row in %q: %w", relative, err)
		}
		col, err := strconv.Atoi(strings.TrimSuffix(parts[2], ".json"))
		if err != nil {
			return fmt.Errorf("parse col in %q: %w", relative, err)
		}
		jobs = append(jobs, buildJob{level: level, row: row, col: col, marker: path})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan node markers: %w", err)
	}
	return jobs, nil
}

func discoverBuildJobs(ctx context.Context, options BuildOptions) ([]buildJob, error) {
	rawRoot := filepath.Join(options.InputDir, "raw")
	jobs := make([]buildJob, 0)
	err := filepath.Walk(rawRoot, func(path string, info os.FileInfo, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() || filepath.Ext(path) != ".response" {
			return nil
		}
		relative, err := filepath.Rel(rawRoot, path)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(relative), "/")
		if len(parts) != 3 {
			return fmt.Errorf("unexpected NODE path %q", relative)
		}
		level, err := strconv.Atoi(parts[0])
		if err != nil {
			return fmt.Errorf("parse level in %q: %w", relative, err)
		}
		if level < options.MinLevel || level > options.MaxLevel {
			return nil
		}
		row, err := strconv.Atoi(parts[1])
		if err != nil {
			return fmt.Errorf("parse row in %q: %w", relative, err)
		}
		col, err := strconv.Atoi(strings.TrimSuffix(parts[2], ".response"))
		if err != nil {
			return fmt.Errorf("parse col in %q: %w", relative, err)
		}
		jobs = append(jobs, buildJob{
			path: path, level: level, row: row, col: col,
			marker: filepath.Join(options.OutputDir, "nodes", parts[0], parts[1], fmt.Sprintf("%d.json", col)),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan downloaded NODE responses: %w", err)
	}
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].level != jobs[j].level {
			return jobs[i].level < jobs[j].level
		}
		if jobs[i].row != jobs[j].row {
			return jobs[i].row < jobs[j].row
		}
		return jobs[i].col < jobs[j].col
	})
	if options.Limit > 0 && len(jobs) > options.Limit {
		jobs = jobs[:options.Limit]
	}
	return jobs, nil
}

func buildOneNode(ctx context.Context, job buildJob, options BuildOptions) buildResult {
	if record, ok := validResumeMarker(job, options.OutputDir); ok {
		return buildResult{record: record, status: "skipped", source: job.path}
	}
	if err := ctx.Err(); err != nil {
		return buildResult{status: "failed", source: job.path, err: err}
	}
	sourceInfo, err := os.Stat(job.path)
	if err != nil {
		return buildResult{status: "failed", source: job.path, err: fmt.Errorf("stat NODE: %w", err)}
	}
	response, err := os.ReadFile(job.path)
	if err != nil {
		return buildResult{status: "failed", source: job.path, err: fmt.Errorf("read NODE: %w", err)}
	}
	node, err := DecodeRawNode(response, options.Format)
	if err != nil {
		return buildResult{status: "failed", source: job.path, err: err}
	}
	if node.Level != job.level || node.Row != job.row || node.Col != job.col {
		return buildResult{status: "failed", source: job.path, err: fmt.Errorf("NODE address %d/%d/%d does not match path %d/%d/%d", node.Level, node.Row, node.Col, job.level, job.row, job.col)}
	}
	meshes, err := node.DecodeMeshes()
	if err != nil {
		return buildResult{status: "failed", source: job.path, err: err}
	}
	record := nodeRecord{
		Level: node.Level, Row: node.Row, Col: node.Col, Offset: node.Offset, AABox: node.AABox,
		ChildrenIDs: node.ChildrenIDs, Resolution: node.Resolution, EntityCount: len(node.EntityIDs),
		AttributeBytes: len(node.AttributeTable),
		SourceSize:     sourceInfo.Size(), SourceModTime: sourceInfo.ModTime().UnixNano(),
	}
	for meshIndex, mesh := range meshes {
		if err := ctx.Err(); err != nil {
			return buildResult{status: "failed", source: job.path, err: err}
		}
		glb, err := BuildGLB(mesh)
		if err != nil {
			return buildResult{status: "failed", source: job.path, err: fmt.Errorf("build mesh %d GLB: %w", meshIndex, err)}
		}
		name := fmt.Sprintf("%d.glb", node.Col)
		if len(meshes) > 1 {
			name = fmt.Sprintf("%d-%d.glb", node.Col, meshIndex)
		}
		path := filepath.Join(options.OutputDir, "content", strconv.Itoa(node.Level), strconv.Itoa(node.Row), name)
		if err := atomicWrite(path, glb, 0644); err != nil {
			return buildResult{status: "failed", source: job.path, err: fmt.Errorf("write mesh %d GLB: %w", meshIndex, err)}
		}
		relative, err := filepath.Rel(options.OutputDir, path)
		if err != nil {
			return buildResult{status: "failed", source: job.path, err: err}
		}
		record.ContentURIs = append(record.ContentURIs, filepath.ToSlash(relative))
	}
	markerData, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return buildResult{status: "failed", source: job.path, err: err}
	}
	markerData = append(markerData, '\n')
	if err := atomicWrite(job.marker, markerData, 0644); err != nil {
		return buildResult{status: "failed", source: job.path, err: fmt.Errorf("write resume marker: %w", err)}
	}
	status := "converted"
	if len(meshes) == 0 {
		status = "no-model"
	}
	return buildResult{record: record, status: status, source: job.path}
}

func validResumeMarker(job buildJob, outputRoot string) (nodeRecord, bool) {
	var record nodeRecord
	data, err := os.ReadFile(job.marker)
	if err != nil || json.Unmarshal(data, &record) != nil {
		return record, false
	}
	if record.Level != job.level || record.Row != job.row || record.Col != job.col {
		return nodeRecord{}, false
	}
	sourceInfo, err := os.Stat(job.path)
	if err != nil || sourceInfo.Size() != record.SourceSize || sourceInfo.ModTime().UnixNano() != record.SourceModTime {
		return nodeRecord{}, false
	}
	for _, uri := range record.ContentURIs {
		path := filepath.Clean(filepath.FromSlash(uri))
		if filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
			return nodeRecord{}, false
		}
		if !validGLB(filepath.Join(outputRoot, path)) {
			return nodeRecord{}, false
		}
	}
	return record, true
}

func validGLB(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() < 20 {
		return false
	}
	header := make([]byte, 12)
	if _, err := io.ReadFull(file, header); err != nil {
		return false
	}
	return string(header[:4]) == "glTF" && binary.LittleEndian.Uint32(header[4:8]) == 2 && int64(binary.LittleEndian.Uint32(header[8:12])) == info.Size()
}

type buildLock struct {
	file *os.File
}

func acquireBuildLock(outputDir string) (*buildLock, error) {
	path := filepath.Join(outputDir, ".build.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("open build lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("another builder is using output %s: %w", outputDir, err)
	}
	return &buildLock{file: file}, nil
}

func (lock *buildLock) close() {
	if lock == nil || lock.file == nil {
		return
	}
	_ = syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	_ = lock.file.Close()
}

func ensureBuildConfig(options BuildOptions) error {
	sourceManifest, err := os.ReadFile(filepath.Join(options.InputDir, "download-config.json"))
	if err != nil {
		return fmt.Errorf("read source download-config.json: %w", err)
	}
	digest := sha256.Sum256(sourceManifest)
	want := buildConfig{
		Version: 1, SourceManifestSHA256: fmt.Sprintf("%x", digest[:]),
		FormatVersion: options.Format.Version, VertexSize: options.Format.VertexSize,
		MinLevel: options.MinLevel, MaxLevel: options.MaxLevel,
	}
	path := filepath.Join(options.OutputDir, "build-config.json")
	data, err := os.ReadFile(path)
	if err == nil {
		var got buildConfig
		if err := json.Unmarshal(data, &got); err != nil {
			return fmt.Errorf("decode existing build-config.json: %w", err)
		}
		if got != want {
			return fmt.Errorf("output build configuration does not match source archive or format; use a different output directory")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("read build-config.json: %w", err)
	}
	entries, err := os.ReadDir(options.OutputDir)
	if err != nil {
		return fmt.Errorf("inspect output directory: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() != ".build.lock" && entry.Name() != "build-config.json.part" {
			return fmt.Errorf("non-empty output directory has no build-config.json; use a new directory")
		}
	}
	if err := os.Remove(path + ".part"); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove interrupted build config: %w", err)
	}
	encoded, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(encoded, '\n'), 0644)
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	temporary := path + ".part"
	if err := os.WriteFile(temporary, data, mode); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func buildArchiveTileset(records []nodeRecord, options BuildOptions) ([]byte, error) {
	if len(records) == 0 {
		return nil, errors.New("no converted node records")
	}
	recordByID := make(map[string]nodeRecord, len(records))
	minimumLevel := records[0].Level
	for _, record := range records {
		recordByID[nodeRecordID(record.Level, record.Row, record.Col)] = record
		if record.Level < minimumLevel {
			minimumLevel = record.Level
		}
	}
	roots := make([]nodeRecord, 0)
	for _, record := range records {
		if record.Level == minimumLevel {
			roots = append(roots, record)
		}
	}
	if len(roots) != 1 {
		return nil, fmt.Errorf("converted records contain %d disconnected roots at level %d", len(roots), minimumLevel)
	}
	visited := make(map[string]bool, len(recordByID))
	rootTile, rootError, ok, err := buildRecordTile(roots[0], nil, recordByID, options, visited)
	if err != nil {
		return nil, err
	}
	if len(visited) != len(recordByID) {
		for id, record := range recordByID {
			if !visited[id] && len(record.ContentURIs) > 0 {
				return nil, fmt.Errorf("converted node %s is not reachable from root %s", id, nodeRecordID(roots[0].Level, roots[0].Row, roots[0].Col))
			}
		}
	}
	if !ok {
		return nil, errors.New("converted hierarchy contains no renderable content")
	}
	tilesetError := rootError
	if tilesetError == 0 {
		// Cesium skips traversal when the tileset-level error is zero, even
		// though a leaf tile correctly has geometricError 0.
		tilesetError = 1
	}
	document := map[string]interface{}{
		"asset":          map[string]interface{}{"version": "1.1", "generator": "tiler/nlsc-modelset-build"},
		"geometricError": tilesetError,
		"root":           rootTile,
	}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode archive tileset: %w", err)
	}
	return append(data, '\n'), nil
}

func buildRecordTile(record nodeRecord, parentOffset *Vec3, records map[string]nodeRecord, options BuildOptions, visited map[string]bool) (map[string]interface{}, float64, bool, error) {
	recordID := nodeRecordID(record.Level, record.Row, record.Col)
	if visited[recordID] {
		return nil, 0, false, fmt.Errorf("node %s is referenced more than once", recordID)
	}
	visited[recordID] = true
	if options.AllowMissingChildren && record.Level+1 <= options.MaxLevel {
		presentChildren, missingChildren := 0, 0
		for _, childID := range record.ChildrenIDs {
			quadrant, err := strconv.Atoi(childID)
			if err != nil || quadrant < 1 || quadrant > 4 {
				return nil, 0, false, fmt.Errorf("node %s has invalid child ID %q", recordID, childID)
			}
			childRow := record.Row*2 + (quadrant-1)/2
			childCol := record.Col*2 + (quadrant-1)%2
			if _, exists := records[nodeRecordID(record.Level+1, childRow, childCol)]; exists {
				presentChildren++
				continue
			}
			emptyPath := filepath.Join(options.InputDir, "empty", strconv.Itoa(record.Level+1), strconv.Itoa(childRow), fmt.Sprintf("%d.empty", childCol))
			if _, err := os.Stat(emptyPath); err != nil {
				missingChildren++
			}
		}
		if presentChildren > 0 && missingChildren > 0 {
			return nil, 0, false, fmt.Errorf("node %s has a partially downloaded sibling set; resume the download before building with REPLACE refinement", recordID)
		}
	}
	children := make([]interface{}, 0, len(record.ChildrenIDs))
	maximumChildError := 0.0
	for _, childID := range record.ChildrenIDs {
		quadrant, err := strconv.Atoi(childID)
		if err != nil || quadrant < 1 || quadrant > 4 {
			return nil, 0, false, fmt.Errorf("node %s has invalid child ID %q", nodeRecordID(record.Level, record.Row, record.Col), childID)
		}
		childRow := record.Row*2 + (quadrant-1)/2
		childCol := record.Col*2 + (quadrant-1)%2
		child, exists := records[nodeRecordID(record.Level+1, childRow, childCol)]
		if !exists {
			if record.Level+1 <= options.MaxLevel && !options.AllowMissingChildren {
				emptyPath := filepath.Join(options.InputDir, "empty", strconv.Itoa(record.Level+1), strconv.Itoa(childRow), fmt.Sprintf("%d.empty", childCol))
				if _, err := os.Stat(emptyPath); err != nil {
					return nil, 0, false, fmt.Errorf("node %s advertises missing child %s (no raw record or empty marker)", nodeRecordID(record.Level, record.Row, record.Col), nodeRecordID(record.Level+1, childRow, childCol))
				}
			}
			continue
		}
		childTile, childError, childOK, err := buildRecordTile(child, &record.Offset, records, options, visited)
		if err != nil {
			return nil, 0, false, err
		}
		if childOK {
			children = append(children, childTile)
			if childError > maximumChildError {
				maximumChildError = childError
			}
		}
	}
	if len(record.ContentURIs) == 0 && len(children) == 0 {
		return nil, 0, false, nil
	}
	capture := Capture{Offset: record.Offset, AABox: record.AABox}
	box, err := localBoundingBox(capture)
	if err != nil {
		return nil, 0, false, fmt.Errorf("node %s: %w", nodeRecordID(record.Level, record.Row, record.Col), err)
	}
	translation := record.Offset
	if parentOffset != nil {
		translation.X -= parentOffset.X
		translation.Y -= parentOffset.Y
		translation.Z -= parentOffset.Z
	}
	geometricError := 0.0
	if len(children) > 0 {
		if record.Resolution > 0 {
			geometricError = 1 / record.Resolution
		}
		if geometricError <= maximumChildError {
			geometricError = maximumChildError * 2
		}
		if geometricError == 0 {
			geometricError = 1
		}
	}
	tile := map[string]interface{}{
		"boundingVolume": map[string]interface{}{"box": box},
		"geometricError": geometricError,
		"refine":         "REPLACE",
		"transform": []float64{
			1, 0, 0, 0,
			0, 1, 0, 0,
			0, 0, 1, 0,
			translation.X, translation.Y, translation.Z, 1,
		},
	}
	if len(record.ContentURIs) == 1 {
		tile["content"] = map[string]interface{}{"uri": record.ContentURIs[0]}
	} else if len(record.ContentURIs) > 1 {
		contents := make([]interface{}, len(record.ContentURIs))
		for i, uri := range record.ContentURIs {
			contents[i] = map[string]interface{}{"uri": uri}
		}
		tile["contents"] = contents
	}
	if len(children) > 0 {
		tile["children"] = children
	}
	return tile, geometricError, true, nil
}

func nodeRecordID(level, row, col int) string {
	return fmt.Sprintf("%d/%d/%d", level, row, col)
}
