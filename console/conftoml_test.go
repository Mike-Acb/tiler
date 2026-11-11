package main

import (
	"strings"
	"testing"
)

const sampleConf = `[app]
  version = "v 0.1.0"
[output]
  format = "file"
  directory = "/data/output"
[task]
  workers = 3
[tm]
  name = "vector"
  min = 0
  max = 14 # 最大层级
  format = "png"
  url = "https://example.com/{z}/{x}/{y}"
  [[lrs]]
    min = 0
    max = 7
    geojson = "/app/geojson/global.geojson"
  [[lrs]]
    min = 8
    max = 12
    geojson = "/app/geojson/china.geojson"
`

func TestAppendLrsKeepsOriginalAndAppends(t *testing.T) {
	out, err := appendLrs(sampleConf, Lrs{Min: 13, Max: 14, Geojson: "/app/geojson/hz.geojson"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, sampleConf) {
		t.Fatalf("原文被改动:\n%s", out)
	}
	c, err := parseConf(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Lrs) != 3 || c.Lrs[2] != (Lrs{Min: 13, Max: 14, Geojson: "/app/geojson/hz.geojson"}) {
		t.Fatalf("lrs = %+v", c.Lrs)
	}
	if c.TM.Max != 14 || c.TM.Min != 0 {
		t.Fatalf("tm 层级不应变化: %d-%d", c.TM.Min, c.TM.Max)
	}
}

func TestAppendLrsWidensTmZoom(t *testing.T) {
	out, err := appendLrs(sampleConf, Lrs{Min: 15, Max: 17, Geojson: "/app/geojson/hz.geojson"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := parseConf(out)
	if err != nil {
		t.Fatal(err)
	}
	if c.TM.Max != 17 {
		t.Fatalf("tm.max = %d, want 17", c.TM.Max)
	}
	if !strings.Contains(out, "max = 17 # 最大层级") {
		t.Fatalf("行尾注释应保留:\n%s", out)
	}
	// lrs 里的 max 不能被误改
	if c.Lrs[0].Max != 7 || c.Lrs[1].Max != 12 {
		t.Fatalf("lrs 被误改: %+v", c.Lrs)
	}
}

func TestAppendLrsRejectsBadZoom(t *testing.T) {
	if _, err := appendLrs(sampleConf, Lrs{Min: 10, Max: 5, Geojson: "/app/geojson/x.geojson"}); err == nil {
		t.Fatal("min > max 应报错")
	}
}

func TestAppendLrsWithoutTrailingNewline(t *testing.T) {
	raw := strings.TrimRight(sampleConf, "\n")
	out, err := appendLrs(raw, Lrs{Min: 1, Max: 2, Geojson: "/app/geojson/a.geojson"})
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := parseConf(out); len(c.Lrs) != 3 {
		t.Fatalf("lrs 数量 = %d", len(c.Lrs))
	}
}
