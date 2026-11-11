package main

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"strings"
	"testing"
)

// fakeHeader 构造一个 pmtiles v3 文件头。
func fakeHeader(tileType, minZ, maxZ byte, bounds [4]float64, tiles uint64, comp byte) []byte {
	h := make([]byte, 127)
	copy(h, "PMTiles")
	h[7] = 3
	le := binary.LittleEndian
	le.PutUint64(h[24:], 127) // metadata offset
	le.PutUint64(h[32:], 0)   // metadata length
	le.PutUint64(h[72:], tiles)
	h[97] = comp
	h[99] = tileType
	h[100], h[101] = minZ, maxZ
	for i, v := range bounds {
		le.PutUint32(h[102+4*i:], uint32(int32(v*1e7)))
	}
	return h
}

func TestParsePMHeader(t *testing.T) {
	m, _, _, _, err := parsePMHeader(fakeHeader(3, 2, 14, [4]float64{117, 29, 122.5, 33.5}, 100, 2))
	if err != nil {
		t.Fatal(err)
	}
	if m.Format != "jpg" || m.MinZoom != 2 || m.MaxZoom != 14 || m.TileCount != 100 {
		t.Fatalf("meta = %+v", m)
	}
	if len(m.Bounds) != 4 || m.Bounds[0] != 117 || m.Bounds[3] != 33.5 {
		t.Fatalf("bounds = %v", m.Bounds)
	}
	// 线上旧文件：类型 unknown、默认全球范围
	m, _, _, _, _ = parsePMHeader(fakeHeader(0, 0, 15, [4]float64{-180, -85, 180, 85}, 10, 2))
	if m.Format != "" || m.Bounds != nil {
		t.Fatalf("unknown 类型和默认范围应视为缺失: %+v", m)
	}
	if _, _, _, _, err := parsePMHeader([]byte("not a pmtiles")); err == nil {
		t.Fatal("非 pmtiles 应报错")
	}
}

func TestApplyPMMetadataGzip(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte(`{"name":"satellite","format":"jpg","description":"x"}`))
	zw.Close()
	m := PMMeta{}
	if err := applyPMMetadata(&m, buf.Bytes(), 2); err != nil {
		t.Fatal(err)
	}
	if m.Name != "satellite" || m.Format != "jpg" || m.Metadata != 3 {
		t.Fatalf("meta = %+v", m)
	}
	if err := applyPMMetadata(&PMMeta{}, []byte("x"), 3); err == nil {
		t.Fatal("brotli 等不支持的压缩应报错")
	}
}

func layerFor(t *testing.T, meta *PMMeta) *layerState {
	t.Helper()
	conf, err := parseConf(sampleConf) // tm: vector, png, z0-14
	if err != nil {
		t.Fatal(err)
	}
	st := &layerState{conf: conf, Meta: meta}
	resolveInfo(st)
	return st
}

func TestResolveInfoFallbackToConfigMap(t *testing.T) {
	// 线上现状：metadata 为空、格式 unknown、默认范围，只有层级
	st := layerFor(t, &PMMeta{MinZoom: 0, MaxZoom: 14, TileCount: 10})
	if st.Name != "vector" || st.NameFrom != "configmap" || st.Format != "png" || st.FormatFrom != "configmap" {
		t.Fatalf("应回落到 ConfigMap: %+v", st)
	}
	if st.ZoomFrom != "pmtiles" || st.Max != 14 {
		t.Fatalf("层级应取 pmtiles: %+v", st)
	}
	got := strings.Join(st.Missing, ",")
	if got != "名称,格式,范围（bounds）" {
		t.Fatalf("missing = %q", got)
	}
	if len(st.Mismatch) != 0 {
		t.Fatalf("不应有不一致: %v", st.Mismatch)
	}
}

func TestResolveInfoPrefersPMTilesAndReportsMismatch(t *testing.T) {
	st := layerFor(t, &PMMeta{Name: "vector", Format: "png", MinZoom: 0, MaxZoom: 12, TileCount: 5, Bounds: []float64{1, 2, 3, 4}})
	if st.NameFrom != "pmtiles" || st.FormatFrom != "pmtiles" || st.Max != 12 || len(st.Missing) != 0 {
		t.Fatalf("应全部取 pmtiles: %+v", st)
	}
	if len(st.Mismatch) != 1 || !strings.Contains(st.Mismatch[0], "z0-12") {
		t.Fatalf("层级不一致应提示: %v", st.Mismatch)
	}
	if st.ConfMax != 0 { // ConfMin/ConfMax 由 loadLayer 填，resolveInfo 不动
		t.Fatalf("ConfMax = %d", st.ConfMax)
	}
}

func TestResolveInfoWithoutPMTiles(t *testing.T) {
	st := layerFor(t, nil)
	if st.NameFrom != "configmap" || st.ZoomFrom != "configmap" || len(st.Missing) != 0 {
		t.Fatalf("没有 pmtiles 时只用 ConfigMap、不报缺失: %+v", st)
	}
}
