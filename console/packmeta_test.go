package main

import (
	"encoding/json"
	"testing"
)

func TestGeojsonBounds(t *testing.T) {
	fc := `{"type":"FeatureCollection","features":[
	  {"type":"Feature","properties":{"coordinates":[999,999]},"geometry":{"type":"Polygon","coordinates":[[[121.56,25.03],[121.57,25.03],[121.57,25.04],[121.56,25.03]]]}},
	  {"type":"Feature","geometry":{"type":"GeometryCollection","geometries":[{"type":"Point","coordinates":[120.1,30.2]}]}}]}`
	b, ok := geojsonBounds([]byte(fc))
	if !ok || b != [4]float64{120.1, 25.03, 121.57, 30.2} {
		t.Fatalf("bounds = %v ok=%v（properties 里的 coordinates 不该算）", b, ok)
	}
	if _, ok := geojsonBounds([]byte(`{"type":"FeatureCollection","features":[]}`)); ok {
		t.Fatal("没有坐标应返回 false")
	}
	if _, ok := geojsonBounds([]byte("not json")); ok {
		t.Fatal("非法 JSON 应返回 false")
	}
}

func TestPackMetadata(t *testing.T) {
	conf, _ := parseConf(sampleConf) // vector, png, z0-14, lrs: global + china
	geo := map[string]string{
		"global.geojson": `{"type":"Polygon","coordinates":[[[-180,-90],[180,-90],[180,90],[-180,-90]]]}`,
		// china.geojson 缺失：跳过
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(packMetadata(conf, geo)), &m); err != nil {
		t.Fatal(err)
	}
	if m["name"] != "vector" || m["format"] != "png" || m["minzoom"] != "0" || m["maxzoom"] != "14" {
		t.Fatalf("meta = %v", m)
	}
	if m["bounds"] != "-180.000000,-85.051129,180.000000,85.051129" || m["center"] != "0.000000,0.000000,7" {
		t.Fatalf("范围应裁到 Web 墨卡托: bounds=%q center=%q", m["bounds"], m["center"])
	}
	if _, ok := m["type"]; ok {
		t.Fatal("schema 为空时不应输出 type")
	}

	m = map[string]string{}
	json.Unmarshal([]byte(packMetadata(conf, nil)), &m)
	if _, ok := m["bounds"]; ok || m["name"] != "vector" {
		t.Fatalf("读不到 geojson 时只给基本字段: %v", m)
	}
}
