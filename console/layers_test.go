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
	job := buildJob(testRegistry(), l, conf, "tc-x", "test", KindAll, ProxyOpt{Mode: "default"}, "")

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
	b, _ := json.Marshal(buildJob(testRegistry(), l, conf, "tc-x", "", KindAll, p, ""))
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
	if n := jobName("tianditu-satellite-annotation", KindAll, now); len(n) > 63 || !dnsLabel.MatchString(n) {
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
	b, _ := json.Marshal(buildJob(r, l, conf, "tc-x", "", KindAll, p, ""))
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
	b, _ = json.Marshal(buildJob(r, l, conf, "tc-x", "", KindAll, p, ""))
	if !strings.Contains(string(b), `"PROXY_POOL_API","value":"http://pool:5010"`) {
		t.Fatalf("pool env 不对: %s", b)
	}

	p, _ = ProxyOpt{Mode: "direct"}.resolve(l, r)
	b, _ = json.Marshal(buildJob(r, l, conf, "tc-x", "", KindAll, p, ""))
	if strings.Contains(string(b), "HTTP_PROXY") {
		t.Fatalf("直连不应设 HTTP_PROXY: %s", b)
	}

	for _, bad := range []ProxyOpt{{Mode: "upstream", URL: "ftp://x:1"}, {Mode: "upstream"}, {Mode: "pool", URL: "socks5://x:1"}, {Mode: "weird"}} {
		if _, err := bad.resolve(l, r); err == nil {
			t.Errorf("%+v 应被拒绝", bad)
		}
	}
}

type jobShape struct {
	Spec struct {
		Template struct {
			Spec struct {
				InitContainers []struct {
					Name string `json:"name"`
				} `json:"initContainers"`
				Containers []struct {
					Name string `json:"name"`
					Env  []struct {
						Name  string `json:"name"`
						Value string `json:"value"`
					} `json:"env"`
				} `json:"containers"`
				Volumes []struct {
					Name string `json:"name"`
				} `json:"volumes"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

func shapeOf(t *testing.T, job map[string]any) (inits, mains, vols []string, env map[string]string) {
	t.Helper()
	b, _ := json.Marshal(job)
	var js jobShape
	if err := json.Unmarshal(b, &js); err != nil {
		t.Fatal(err)
	}
	ps := js.Spec.Template.Spec
	env = map[string]string{}
	for _, c := range ps.InitContainers {
		inits = append(inits, c.Name)
	}
	for _, c := range ps.Containers {
		mains = append(mains, c.Name)
		for _, e := range c.Env {
			env[c.Name+"."+e.Name] = e.Value
		}
	}
	for _, v := range ps.Volumes {
		vols = append(vols, v.Name)
	}
	return
}

func TestBuildJobKinds(t *testing.T) {
	conf, _ := parseConf(sampleConf)
	r := testRegistry()
	l := &LayerEntry{ID: "a", ConfigMap: "cfg", GeojsonConfigMap: "geo", MinioPath: "map/x", Proxy: true}

	inits, mains, _, _ := shapeOf(t, buildJob(r, l, conf, "n", "", KindDownload, ProxyOpt{Mode: "default"}, ""))
	if strings.Join(inits, ",") != "proxy" || strings.Join(mains, ",") != "download" {
		t.Fatalf("download job: init=%v main=%v", inits, mains)
	}

	inits, mains, vols, env := shapeOf(t, buildJob(r, l, conf, "n", "", KindPack, ProxyOpt{Mode: "direct"}, `{"name":"vector"}`))
	if len(inits) != 0 || strings.Join(mains, ",") != "sync" || strings.Join(vols, ",") != "output" {
		t.Fatalf("pack job: init=%v main=%v vols=%v", inits, mains, vols)
	}
	if env["sync.WAIT_FOR_DONE"] != "0" || env["sync.TILE_NAME"] != "vector" || env["sync.TILE_METADATA"] != `{"name":"vector"}` {
		t.Fatalf("pack env = %v", env)
	}

	inits, mains, _, env = shapeOf(t, buildJob(r, l, conf, "n", "", KindAll, ProxyOpt{Mode: "direct"}, ""))
	if strings.Join(inits, ",") != "download" || strings.Join(mains, ",") != "sync" {
		t.Fatalf("all job direct: init=%v main=%v", inits, mains)
	}
	if _, ok := env["sync.TILE_METADATA"]; ok {
		t.Fatal("没有元数据时不应设 TILE_METADATA")
	}
}

func TestPendingPack(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	mk := func(name, kind string, done time.Time, phase string) Job {
		var j Job
		j.Metadata.Name = name
		j.Metadata.Labels = map[string]string{labelManaged: managedValue, labelTileName: "vector", labelKind: kind}
		d := done
		j.Status.CompletionTime = &d
		j.Status.Conditions = append(j.Status.Conditions, struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		}{Type: phase, Status: "True"})
		return j
	}
	st := &layerState{TileName: "vector", PMTiles: &objInfo{Modified: t0}}

	if p := pendingFor(st, []Job{mk("dl1", KindDownload, t0.Add(time.Hour), "Complete")}); p == nil || p.Job != "dl1" {
		t.Fatalf("下载晚于 pmtiles 应待打包: %+v", p)
	}
	if p := pendingFor(st, []Job{mk("dl1", KindDownload, t0.Add(time.Hour), "Complete"), mk("pk1", KindPack, t0.Add(2*time.Hour), "Complete")}); p != nil {
		t.Fatalf("已打包不应待打包: %+v", p)
	}
	if p := pendingFor(st, []Job{mk("dl1", KindDownload, t0.Add(time.Hour), "Failed")}); p != nil {
		t.Fatalf("失败的下载不算: %+v", p)
	}
	if p := pendingFor(st, []Job{mk("dl1", KindDownload, t0.Add(-time.Hour), "Complete")}); p != nil {
		t.Fatalf("早于 pmtiles 的下载不算: %+v", p)
	}
	other := mk("dl2", KindDownload, t0.Add(time.Hour), "Complete")
	other.Metadata.Labels[labelTileName] = "satellite"
	if p := pendingFor(st, []Job{other}); p != nil {
		t.Fatalf("其他目录的下载不算: %+v", p)
	}
}

func TestPickRegionKey(t *testing.T) {
	now := time.Date(2026, 11, 11, 10, 30, 0, 0, time.UTC)
	geo := []byte(`{"type":"FeatureCollection"}`)
	from := &regionRef{Layer: "a", Key: "hangzhou.geojson"}

	if k, w := pickRegionKey(map[string]string{}, nil, geo, "Taipei 101", now); k != "taipei-101-20261111-103000.geojson" || !w {
		t.Fatalf("新画区域: %s %v", k, w)
	}
	if k, w := pickRegionKey(map[string]string{}, from, geo, "hangzhou", now); k != "hangzhou.geojson" || !w {
		t.Fatalf("目标没有该键应原样写入: %s %v", k, w)
	}
	if k, w := pickRegionKey(map[string]string{"hangzhou.geojson": string(geo)}, from, geo, "hangzhou", now); k != "hangzhou.geojson" || w {
		t.Fatalf("共用 ConfigMap、内容相同应直接引用: %s %v", k, w)
	}
	if k, w := pickRegionKey(map[string]string{"hangzhou.geojson": "{}"}, from, geo, "hangzhou", now); k != "hangzhou-20261111-103000.geojson" || !w {
		t.Fatalf("同名不同内容应另起新键: %s %v", k, w)
	}
}

func TestHasRegion(t *testing.T) {
	conf, _ := parseConf(sampleConf) // global 0-7, china 8-12
	if !hasRegion(conf, "china.geojson", 8, 12) || hasRegion(conf, "china.geojson", 8, 13) || hasRegion(conf, "hz.geojson", 8, 12) {
		t.Fatal("hasRegion 判断错误")
	}
}
