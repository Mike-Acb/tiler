package nlscdownload

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type atomicCounter int64

func (counter *atomicCounter) Add(delta int64) int64 {
	return atomic.AddInt64((*int64)(counter), delta)
}
func (counter *atomicCounter) Load() int64       { return atomic.LoadInt64((*int64)(counter)) }
func (counter *atomicCounter) Store(value int64) { atomic.StoreInt64((*int64)(counter), value) }

func TestDownloaderResumesCompletedAndEmptyNodes(t *testing.T) {
	t.Helper()
	var requestCount atomicCounter
	var queryError atomic.Value
	payloads := map[string][]byte{
		"1/0/0": gzipPayload(t, []byte("root-node")),
		"2/0/0": gzipPayload(t, []byte("child-node")),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		query := r.URL.Query()
		if query.Get("type") != "modelset" || query.Get("format") != "integrate" || query.Get("name") != "113_H" || query.Get("docname") != "NODE" || query.Get("epsg") != "4326" {
			queryError.Store(fmt.Errorf("unexpected query: %s", r.URL.RawQuery))
		}
		key := fmt.Sprintf("%s/%s/%s", query.Get("level"), query.Get("Row"), query.Get("Col"))
		payload, ok := payloads[key]
		if !ok {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	output := t.TempDir()
	config := testConfig(server, output)
	config.MaxLevel, config.Concurrency = 2, 2
	stats, err := Download(context.Background(), config)
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if errValue := queryError.Load(); errValue != nil {
		t.Fatal(errValue)
	}
	if stats.Downloaded != 2 || stats.Empty != 3 || stats.Failed != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	assertFileContent(t, filepath.Join(output, "raw", "1", "0", "0.response"), payloads["1/0/0"])
	assertFileContent(t, filepath.Join(output, "raw", "2", "0", "0.response"), payloads["2/0/0"])
	for _, path := range []string{
		filepath.Join(output, "empty", "2", "0", "1.empty"),
		filepath.Join(output, "empty", "2", "1", "0.empty"),
		filepath.Join(output, "empty", "2", "1", "1.empty"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("empty marker %s: %v", path, err)
		}
	}

	requestCount.Store(0)
	stats, err = Download(context.Background(), config)
	if err != nil {
		t.Fatalf("resumed Download() error = %v", err)
	}
	if got := requestCount.Load(); got != 0 {
		t.Fatalf("resume made %d HTTP requests, want 0", got)
	}
	if stats.Skipped != 5 || stats.Downloaded != 0 || stats.Empty != 0 {
		t.Fatalf("resumed stats = %+v", stats)
	}
}

func TestDownloaderOnlyDescendsIntoGeographicBounds(t *testing.T) {
	var requests atomicCounter
	target := GeographicBounds{West: 120.53, South: 24.311, East: 120.565, North: 24.329}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		query := r.URL.Query()
		level, row, col := query.Get("level"), query.Get("Row"), query.Get("Col")
		if level == "3" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		bounds := target
		if level == "2" && (row != "0" || col != "0") {
			bounds = GeographicBounds{West: 121, South: 25, East: 121.01, North: 25.01}
		}
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(gzipNodeBounds(t, bounds))
	}))
	defer server.Close()

	config := testConfig(server, t.TempDir())
	config.MaxLevel = 3
	config.Bounds = &target
	stats, err := Download(context.Background(), config)
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if got, want := requests.Load(), int64(9); got != want {
		t.Fatalf("HTTP requests = %d, want %d (root + four L2 + four intersecting L3)", got, want)
	}
	if stats.Downloaded != 5 || stats.Empty != 4 || stats.Visited != 9 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestGeographicBoundsRejectNonFiniteECEF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.response")
	payload := gzipNodeBounds(t, GeographicBounds{West: math.NaN(), South: 24.311, East: 120.565, North: 24.329})
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := responseIntersectsBounds(path, GeographicBounds{West: 120.53, South: 24.311, East: 120.565, North: 24.329}); err == nil {
		t.Fatal("non-finite ECEF boundary was silently treated as outside the requested bounds")
	}
}

func TestDownloaderRetriesTruncatedResponseAndReplacesPartFile(t *testing.T) {
	payload := gzipPayload(t, bytes.Repeat([]byte("NLSC"), 1024))
	var attempts atomicCounter
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := attempts.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		w.WriteHeader(http.StatusOK)
		if attempt == 1 {
			_, _ = w.Write(payload[:len(payload)/2])
			return
		}
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	output := t.TempDir()
	partPath := filepath.Join(output, "raw", "1", "0", "0.response.part")

	config := testConfig(server, output)
	config.Retries, config.RetryDelay = 1, time.Millisecond
	stats, err := Download(context.Background(), config)
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
	if stats.Downloaded != 1 || stats.Retried != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	assertFileContent(t, filepath.Join(output, "raw", "1", "0", "0.response"), payload)
	if _, err := os.Stat(partPath); !os.IsNotExist(err) {
		t.Fatalf("part file still exists: %v", err)
	}
}

func TestDownloaderRejectsReusingOutputForDifferentDataset(t *testing.T) {
	payload := gzipPayload(t, []byte("node"))
	var requests atomicCounter
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	output := t.TempDir()
	config := testConfig(server, output)
	if _, err := Download(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	requests.Store(0)
	config.Layer = "different_layer"
	if _, err := Download(context.Background(), config); err == nil {
		t.Fatal("Download() reused an output directory belonging to a different layer")
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("dataset mismatch made %d HTTP requests, want 0", got)
	}
	if _, err := os.Stat(filepath.Join(output, "download-config.json")); err != nil {
		t.Fatalf("download config manifest: %v", err)
	}
}

func TestDownloaderCanExtendExistingArchiveFromDescendantRoot(t *testing.T) {
	payload := gzipPayload(t, []byte("node"))
	var requested atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		requested.Store(fmt.Sprintf("%s/%s/%s", query.Get("level"), query.Get("Row"), query.Get("Col")))
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	output := t.TempDir()
	config := testConfig(server, output)
	if _, err := Download(context.Background(), config); err != nil {
		t.Fatal(err)
	}

	config.Root = Node{Level: 5, Row: 2, Col: 15}
	config.MaxLevel = 5
	if _, err := Download(context.Background(), config); err != nil {
		t.Fatalf("extend archive from descendant root: %v", err)
	}
	if got := requested.Load(); got != "5/2/15" {
		t.Fatalf("last requested node = %v, want 5/2/15", got)
	}
	assertFileContent(t, filepath.Join(output, "raw", "5", "2", "15.response"), payload)
}

func TestDownloaderRejectsExtendingArchiveOutsideOriginalRoot(t *testing.T) {
	payload := gzipPayload(t, []byte("node"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	output := t.TempDir()
	config := testConfig(server, output)
	if _, err := Download(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	config.Root = Node{Level: 1, Row: 0, Col: 1}
	if _, err := Download(context.Background(), config); err == nil {
		t.Fatal("Download() extended an archive outside its original root")
	}
}

func TestDownloaderRejectsInvalidGzip(t *testing.T) {
	truncated := gzipPayload(t, bytes.Repeat([]byte("building"), 128))
	tests := map[string][]byte{
		"HTML response":   []byte("<html>access denied</html>"),
		"missing trailer": truncated[:len(truncated)-8],
	}
	for name, payload := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(payload)
			}))
			defer server.Close()
			output := t.TempDir()
			stats, err := Download(context.Background(), testConfig(server, output))
			if err == nil || stats.Failed != 1 {
				t.Fatalf("Download() error/stats = %v/%+v, want gzip failure", err, stats)
			}
			finalPath := filepath.Join(output, "raw", "1", "0", "0.response")
			if _, err := os.Stat(finalPath); !os.IsNotExist(err) {
				t.Fatalf("invalid response became a checkpoint: %v", err)
			}
			if info, err := os.Stat(finalPath + ".part"); err != nil || info.Size() == 0 {
				t.Fatalf("invalid response part was not preserved: info=%v err=%v", info, err)
			}
		})
	}
}

func TestDownloaderRefusesArchiveWithoutDatasetManifest(t *testing.T) {
	var requests atomicCounter
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write(gzipPayload(t, []byte("node")))
	}))
	defer server.Close()

	output := t.TempDir()
	legacyPath := filepath.Join(output, "raw", "1", "0", "0.response")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, gzipPayload(t, []byte("unknown dataset")), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Download(context.Background(), testConfig(server, output))
	if err == nil {
		t.Fatal("Download() adopted a non-empty archive without a dataset manifest")
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("unclaimed archive made %d HTTP requests, want 0", got)
	}
}

func TestDownloaderPreventsConcurrentWritersInOneArchive(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	payload := gzipPayload(t, []byte("node"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	config := testConfig(server, t.TempDir())
	firstDone := make(chan error, 1)
	go func() {
		_, err := Download(context.Background(), config)
		firstDone <- err
	}()
	<-started
	secondDone := make(chan error, 1)
	go func() {
		_, err := Download(context.Background(), config)
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		if err == nil || !strings.Contains(err.Error(), "already being written") {
			close(release)
			<-firstDone
			t.Fatalf("second Download() error = %v, want archive lock error", err)
		}
	case <-time.After(100 * time.Millisecond):
		close(release)
		<-secondDone
		<-firstDone
		t.Fatal("second Download() blocked instead of rejecting a concurrent writer")
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Download() error = %v", err)
	}
}

func TestDownloaderRejectsMaxLevelBelowRoot(t *testing.T) {
	config := Config{BaseURL: "https://example.test", Layer: "113_H", OutputDir: t.TempDir(), Root: Node{Level: 2}, MaxLevel: 1}
	if _, err := Download(context.Background(), config); err == nil {
		t.Fatal("Download() accepted max level below root level")
	}
}

func assertFileContent(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}

func gzipPayload(t *testing.T, payload []byte) []byte {
	t.Helper()
	var encoded bytes.Buffer
	writer := gzip.NewWriter(&encoded)
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func gzipNodeBounds(t *testing.T, bounds GeographicBounds) []byte {
	t.Helper()
	var raw bytes.Buffer
	write := func(value interface{}) {
		t.Helper()
		if err := binary.Write(&raw, binary.LittleEndian, value); err != nil {
			t.Fatal(err)
		}
	}
	write(int32(1))
	write(int32(0))
	write(int32(0))
	for i := 0; i < 3; i++ {
		write(float64(0))
	}
	for i := 0; i < 8*3; i++ {
		write(float64(0))
	}
	write(int32(0))
	write(uint16(0))
	write(float64(1))
	for _, point := range [][2]float64{
		{bounds.West, bounds.North}, {bounds.East, bounds.North},
		{bounds.East, bounds.South}, {bounds.West, bounds.South},
	} {
		x, y, z := geodeticECEF(point[0], point[1])
		write(x)
		write(y)
		write(z)
	}
	return gzipPayload(t, raw.Bytes())
}

func geodeticECEF(longitude, latitude float64) (float64, float64, float64) {
	const semiMajor = 6378137.0
	const eccentricitySquared = 6.69437999014e-3
	lon, lat := longitude*math.Pi/180, latitude*math.Pi/180
	sinLat, cosLat := math.Sin(lat), math.Cos(lat)
	normal := semiMajor / math.Sqrt(1-eccentricitySquared*sinLat*sinLat)
	return normal * cosLat * math.Cos(lon), normal * cosLat * math.Sin(lon), normal * (1 - eccentricitySquared) * sinLat
}

func testConfig(server *httptest.Server, output string) Config {
	return Config{
		BaseURL: server.URL, Layer: "113_H", OutputDir: output,
		Root: Node{Level: 1}, MaxLevel: 1, Concurrency: 1, Client: server.Client(),
	}
}
