package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTileIndex(t *testing.T) {
	root := t.TempDir()
	col := filepath.Join(root, "9", "380")
	if err := os.MkdirAll(col, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"187.jpg", "188.jpg", "189.png", "x.jpg", "190.jpg.tmp"} {
		if err := os.WriteFile(filepath.Join(col, f), nil, 0o644); err != nil { // 空文件也要算已存在
			t.Fatal(err)
		}
	}
	ix := newTileIndex(root, 9, "jpg")
	cases := []struct {
		x, y uint32
		want bool
	}{
		{380, 187, true},
		{380, 188, true},
		{380, 189, false}, // 格式不同
		{380, 190, false},
		{381, 187, false}, // 列目录不存在
	}
	for _, c := range cases {
		if got := ix.has(c.x, c.y); got != c.want {
			t.Errorf("has(%d,%d) = %v, want %v", c.x, c.y, got, c.want)
		}
	}
	if newTileIndex(root, 10, "jpg").has(380, 187) {
		t.Error("其他层级不应命中")
	}
}
