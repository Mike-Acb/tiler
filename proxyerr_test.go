package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// 走代理时代理回 407（短效代理到期）应判为代理错误，并通知边车换代理。
func TestFetchTileProxyAuthExpired(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	proxySwitchPath = filepath.Join(t.TempDir(), "switch")
	lastProxySwitch.Store(0)

	_, err := fetchTile("http://tiles.invalid/1/0/0.png")
	if !errors.Is(err, errProxy) {
		t.Fatalf("err = %v, want errProxy", err)
	}
	if b, err := os.ReadFile(proxySwitchPath); err != nil || string(b) != "407" {
		t.Fatalf("switch file = %q, %v; want \"407\"", b, err)
	}
}

func TestIsProxyStatus(t *testing.T) {
	for code, want := range map[int]bool{407: true, 418: true, 502: true, 503: true, 504: true, 200: false, 403: false, 404: false, 500: false} {
		if got := isProxyStatus(code); got != want {
			t.Errorf("isProxyStatus(%d) = %v, want %v", code, got, want)
		}
	}
}
