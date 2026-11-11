package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testRegistry() *Registry {
	return &Registry{
		Namespace:        "default",
		Images:           Images{Tiler: "tiler:latest", Sync: "sync:latest", Proxy: "proxy:latest"},
		OutputPVC:        "tiler-output-pvc",
		ProxyConfigMap:   "tiler-proxy-config",
		MinioConfigMap:   "pmtiles-sync-config",
		MinioSecret:      "pmtiles-sync-secret",
		GeojsonMountPath: "/app/geojson",
		JobTTLSeconds:    3600,
	}
}

func TestBuildJobOrdersProxyDownloadSync(t *testing.T) {
	conf, err := parseConf(sampleConf)
	if err != nil {
		t.Fatal(err)
	}
	l := &LayerEntry{ID: "tianditu-vector", ConfigMap: "cfg", GeojsonConfigMap: "geo", MinioPath: "map/tianditu", Proxy: true}
	job := buildJob(testRegistry(), l, conf, "tc-x", "test", ProxyOpt{Mode: "default"})

	b, _ := json.Marshal(job)
	var parsed struct {
		Metadata ObjectMeta `json:"metadata"`
		Spec     struct {
			Template struct {
				Spec struct {
					RestartPolicy  string `json:"restartPolicy"`
					InitContainers []struct {
						Name          string `json:"name"`
						RestartPolicy string `json:"restartPolicy"`
					} `json:"initContainers"`
					Containers []struct {
						Name string `json:"name"`
						Env  []struct {
							Name  string `json:"name"`
							Value string `json:"value"`
						} `json:"env"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatal(err)
	}
	ps := parsed.Spec.Template.Spec
	if len(ps.InitContainers) != 2 || ps.InitContainers[0].Name != "proxy" || ps.InitContainers[0].RestartPolicy != "Always" || ps.InitContainers[1].Name != "download" {
		t.Fatalf("initContainers = %+v", ps.InitContainers)
	}
	if len(ps.Containers) != 1 || ps.Containers[0].Name != "sync" {
		t.Fatalf("containers = %+v", ps.Containers)
	}
	env := map[string]string{}
	for _, e := range ps.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["TILE_NAME"] != "vector" || env["MINIO_PATH"] != "map/tianditu" || env["INPUT_DIR"] != "/data/output" {
		t.Fatalf("sync env = %v", env)
	}
	if parsed.Metadata.Labels[labelTileName] != "vector" || parsed.Metadata.Labels[labelManaged] != managedValue {
		t.Fatalf("labels = %v", parsed.Metadata.Labels)
	}
}

func TestBuildJobWithoutProxy(t *testing.T) {
	conf, _ := parseConf(sampleConf)
	l := &LayerEntry{ID: "gaode-vector", ConfigMap: "cfg", GeojsonConfigMap: "geo"}
	p, err := ProxyOpt{}.resolve(l, testRegistry())
	if err != nil || p.Mode != "direct" {
		t.Fatalf("未开代理的图层默认应直连: %+v %v", p, err)
	}
	b, _ := json.Marshal(buildJob(testRegistry(), l, conf, "tc-x", "", p))
	if strings.Contains(string(b), `"proxy"`) || strings.Contains(string(b), "HTTP_PROXY") {
		t.Fatalf("不需要代理的图层不应带 proxy: %s", b)
	}
}

func TestNormalizeRegion(t *testing.T) {
	ok := `{"type":"FeatureCollection","features":[{"type":"Feature","properties":{"name":"original"},"geometry":{"type":"Polygon","coordinates":[[[120,30],[121,30],[121,31],[120,30]]]}}]}`
	out, err := normalizeRegion(json.RawMessage(ok), "杭州")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "original") || !strings.Contains(string(out), `"name":"杭州"`) {
		t.Fatalf("out = %s", out)
	}
	bad := map[string]string{
		"point":    `{"type":"FeatureCollection","features":[{"type":"Feature","geometry":{"type":"Point","coordinates":[1,2]}}]}`,
		"unclosed": `{"type":"FeatureCollection","features":[{"type":"Feature","geometry":{"type":"Polygon","coordinates":[[[120,30],[121,30],[121,31],[120,31]]]}}]}`,
		"range":    `{"type":"FeatureCollection","features":[{"type":"Feature","geometry":{"type":"Polygon","coordinates":[[[200,30],[121,30],[121,31],[200,30]]]}}]}`,
		"empty":    `{"type":"FeatureCollection","features":[]}`,
	}
	for name, raw := range bad {
		if _, err := normalizeRegion(json.RawMessage(raw), "x"); err == nil {
			t.Errorf("%s 应被拒绝", name)
		}
	}
}

func TestRegionKeyAndNames(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 30, 0, 0, time.UTC)
	if k := regionKey("Hang Zhou", now); k != "hang-zhou-20261111-093000.geojson" {
		t.Fatalf("key = %s", k)
	}
	if k := regionKey("杭州", now); k != "region-20261111-093000.geojson" {
		t.Fatalf("key = %s", k)
	}
	if n := jobName("tianditu-satellite-annotation", now); len(n) > 63 || !dnsLabel.MatchString(n) {
		t.Fatalf("job name %q 非法", n)
	}
	if k := pmtilesKey("/map/tianditu/", "tianditu satelite"); k != "map/tianditu/tianditu_satelite.pmtiles" {
		t.Fatalf("pmtiles key = %s", k)
	}
}

func TestProxyModes(t *testing.T) {
	conf, _ := parseConf(sampleConf)
	r := testRegistry()
	l := &LayerEntry{ID: "a", ConfigMap: "cfg", GeojsonConfigMap: "geo", Proxy: true}

	p, err := ProxyOpt{Mode: "upstream", URL: "socks5://user:pw@10.0.0.1:1080"}.resolve(l, r)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(buildJob(r, l, conf, "tc-x", "", p))
	js := string(b)
	if !strings.Contains(js, `"UPSTREAM_PROXY","value":"socks5://user:pw@10.0.0.1:1080"`) || strings.Contains(js, "PROXY_POOL_API") {
		t.Fatalf("upstream env 不对: %s", js)
	}
	if strings.Contains(p.describe(), "pw") {
		t.Fatalf("描述泄露密码: %s", p.describe())
	}
	if !strings.Contains(js, `"emptyDir"`) || strings.Contains(js, "persistentVolumeClaim\":{\"claimName\":\"proxy") {
		t.Fatalf("/tmp/proxy 应为 emptyDir: %s", js)
	}

	p, _ = ProxyOpt{Mode: "pool", URL: "http://pool:5010"}.resolve(l, r)
	b, _ = json.Marshal(buildJob(r, l, conf, "tc-x", "", p))
	if !strings.Contains(string(b), `"PROXY_POOL_API","value":"http://pool:5010"`) {
		t.Fatalf("pool env 不对: %s", b)
	}

	p, _ = ProxyOpt{Mode: "direct"}.resolve(l, r)
	b, _ = json.Marshal(buildJob(r, l, conf, "tc-x", "", p))
	if strings.Contains(string(b), "HTTP_PROXY") {
		t.Fatalf("直连不应设 HTTP_PROXY: %s", b)
	}

	for _, bad := range []ProxyOpt{{Mode: "upstream", URL: "ftp://x:1"}, {Mode: "upstream"}, {Mode: "pool", URL: "socks5://x:1"}, {Mode: "weird"}} {
		if _, err := bad.resolve(l, r); err == nil {
			t.Errorf("%+v 应被拒绝", bad)
		}
	}
}
