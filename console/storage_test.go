package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPickPrecedence(t *testing.T) {
	t.Setenv("TC_TEST_KEY", "")
	if got := pick("TC_TEST_KEY", "", "/d"); got != (setting{"/d", fromDefault}) {
		t.Fatalf("默认值: %+v", got)
	}
	if got := pick("TC_TEST_KEY", "/j", "/d"); got != (setting{"/j", fromJSON}) {
		t.Fatalf("layers.json 优先于默认值: %+v", got)
	}
	t.Setenv("TC_TEST_KEY", "/e")
	if got := pick("TC_TEST_KEY", "/j", "/d"); got != (setting{"/e", fromEnv}) {
		t.Fatalf("环境变量最优先: %+v", got)
	}
	t.Setenv("TC_TEST_KEY", "")
	if got := pick("TC_TEST_KEY", "", ""); got.From != fromNone {
		t.Fatalf("都没有应为 none: %+v", got)
	}
}

func TestLoadStorageWithoutMinio(t *testing.T) {
	for _, k := range []string{"MINIO_ENDPOINT", "MINIO_BUCKET", "OUTPUT_DIR", "PMTILES_PUBLIC_URL", "TILE_SERVER_URL", "TILE_SERVER_PREFIX"} {
		t.Setenv(k, "")
	}
	r := testRegistry()
	st, err := loadStorage(r)
	if err != nil {
		t.Fatal(err)
	}
	if st.MinioReady || st.Upload || st.UploadReason == "" {
		t.Fatalf("没配 MinIO 应只下载: %+v", st)
	}
	if st.OutputDir.Value != "/data/output" || st.OutputDir.From != fromDefault {
		t.Fatalf("下载目录默认值: %+v", st.OutputDir)
	}

	t.Setenv("MINIO_ENDPOINT", "http://minio:9005")
	t.Setenv("PMTILES_PUBLIC_URL", "http://1.2.3.4:9005/open/")
	r.MinioConfigMap, r.MinioSecret = "", ""
	st, _ = loadStorage(r)
	if !st.MinioReady || st.Upload {
		t.Fatalf("Job 缺少 MinIO 凭证引用时不能打包: %+v", st)
	}
	if st.PMTilesPublicURL.Value != "http://1.2.3.4:9005/open" {
		t.Fatalf("公开地址应去掉末尾斜杠: %q", st.PMTilesPublicURL.Value)
	}
	r.MinioConfigMap, r.MinioSecret = "cm", "sec"
	st, _ = loadStorage(r)
	if !st.Upload || st.MinioBucket.Value != "open" {
		t.Fatalf("配齐后应可上传，bucket 默认 open: %+v", st)
	}
}

func TestOutputTilePathRejectsTraversal(t *testing.T) {
	if p, err := outputTilePath("/data/output", "satellite", 3, 6, 3, "jpg"); err != nil || p != "/data/output/satellite/3/6/3.jpg" {
		t.Fatalf("p=%q err=%v", p, err)
	}
	for _, bad := range []string{"", "..", "../etc", "a/b", `a\b`} {
		if _, err := outputTilePath("/data/output", bad, 0, 0, 0, "png"); err == nil {
			t.Errorf("目录名 %q 应被拒绝", bad)
		}
	}
	if _, err := outputTilePath("/data/output", "x", 0, 0, 0, "../png"); err == nil {
		t.Error("格式含路径应被拒绝")
	}
}

func newOutputServer(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "vector", "3", "6")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "3.png"), []byte("\x89PNGdata"), 0o644)
	os.WriteFile(filepath.Join(dir, "4.png"), nil, 0o644) // 404 标记
	reg := testRegistry()
	reg.Layers = []LayerEntry{{ID: "tianditu-vector", ConfigMap: "cfg", GeojsonConfigMap: "geo", MinioPath: "map/tianditu"}}
	s := &Server{reg: reg, store: &Storage{OutputDir: setting{root, fromEnv}}, keyCache: map[string]cachedKey{}}
	s.keyCache["tianditu-vector"] = cachedKey{tileName: "vector", format: "png", at: time.Now()}
	return s, root
}

func TestHandleOutputTile(t *testing.T) {
	s, _ := newOutputServer(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /tiles/output/{id}/{z}/{x}/{y}", s.handleOutputTile)
	cases := map[string]int{
		"/tiles/output/tianditu-vector/3/6/3": 200,
		"/tiles/output/tianditu-vector/3/6/4": 204, // 0 字节标记
		"/tiles/output/tianditu-vector/3/6/5": 204, // 不存在
		"/tiles/output/tianditu-vector/3/9/1": 400, // x 超出 2^z
		"/tiles/output/nope/3/6/3":            404,
	}
	for path, want := range cases {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != want {
			t.Errorf("%s -> %d, want %d", path, rec.Code, want)
		}
		if want == 200 && (rec.Header().Get("Content-Type") != "image/png" || rec.Body.String() != "\x89PNGdata") {
			t.Errorf("%s 内容不对: %q %q", path, rec.Header().Get("Content-Type"), rec.Body.String())
		}
	}
}

func TestTileProxyRewritesPathAndMaps404(t *testing.T) {
	var gotPath, gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		switch r.URL.Path {
		case "/base/tianditu/satellite/3/6/3.jpg":
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte("jpg"))
		case "/base/tianditu/satellite/metadata":
			w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL + "/base/")
	s := &Server{store: &Storage{tileProxy: newTileProxy(target)}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+tileServerRoute+"/{path...}", s.handleTileServer)

	do := func(p string) *http.Response {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		return rec.Result()
	}
	resp := do("/tile-server/tianditu/satellite/3/6/3.jpg?v=123")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "jpg" || gotPath != "/base/tianditu/satellite/3/6/3.jpg" || gotQuery != "" {
		t.Fatalf("瓦片转发: %d %q path=%q query=%q", resp.StatusCode, body, gotPath, gotQuery)
	}
	if resp := do("/tile-server/tianditu/satellite/9/0/0.jpg"); resp.StatusCode != 204 {
		t.Fatalf("范围外瓦片应 204，得到 %d", resp.StatusCode)
	}
	if resp := do("/tile-server/tianditu/satellite/metadata"); resp.StatusCode != 200 {
		t.Fatalf("非瓦片接口应原样转发，得到 %d", resp.StatusCode)
	}
	if resp := do("/tile-server/nope.json"); resp.StatusCode != 404 {
		t.Fatalf("非瓦片的 404 保持 404，得到 %d", resp.StatusCode)
	}
}

func TestTileServerPath(t *testing.T) {
	cases := []struct{ minioPath, prefix, name, want string }{
		{"map/tianditu", "map", "satellite", "tianditu/satellite"},
		{"/map/gaode/", "map", "gaode vector", "gaode/gaode_vector"},
		{"map", "map", "x", "x"},
		{"tianditu", "", "satellite", "tianditu/satellite"},
	}
	for _, c := range cases {
		got, err := tileServerPath(c.minioPath, c.prefix, c.name)
		if err != nil || got != c.want {
			t.Errorf("tileServerPath(%q,%q,%q) = %q, %v; want %q", c.minioPath, c.prefix, c.name, got, err, c.want)
		}
	}
	if _, err := tileServerPath("mapx/tianditu", "map", "x"); err == nil {
		t.Error("前缀只匹配字符串开头不算")
	}
}
