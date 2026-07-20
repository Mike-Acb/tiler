package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
)

//go:embed index.html
var assets embed.FS

func main() {
	listen := flag.String("listen", "127.0.0.1:18080", "HTTP listen address")
	tilesDir := flag.String("tiles-dir", "", "directory containing tileset.json")
	flag.Parse()
	if *tilesDir == "" {
		log.Fatal("-tiles-dir is required")
	}
	root, err := filepath.Abs(*tilesDir)
	if err != nil {
		log.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "tileset.json")); err != nil {
		log.Fatalf("tileset not found: %v", err)
	}
	log.Printf("Taichung Cesium preview: http://%s", *listen)
	log.Fatal(http.ListenAndServe(*listen, newHandler(root)))
}

func newHandler(tilesDir string) http.Handler {
	page, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	sources := []string{"/tiles/tileset.json"}
	registerTiles := func(prefix, dir string) {
		tiles := http.StripPrefix(prefix, http.FileServer(http.Dir(dir)))
		mux.Handle(prefix, http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			if request.URL.Path == prefix+"tileset.json" {
				response.Header().Set("Cache-Control", "no-cache")
			} else {
				response.Header().Set("Cache-Control", "public, max-age=3600")
			}
			tiles.ServeHTTP(response, request)
		}))
	}
	registerTiles("/tiles/", tilesDir)
	for _, eastDir := range []string{tilesDir + "-east-full", tilesDir + "-east"} {
		if _, err := os.Stat(filepath.Join(eastDir, "tileset.json")); err == nil {
			registerTiles("/tiles-east/", eastDir)
			sources = append(sources, "/tiles-east/tileset.json")
			break
		}
	}
	sourceData, err := json.Marshal(sources)
	if err != nil {
		panic(err)
	}
	mux.HandleFunc("/tile-sources.json", func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		response.Header().Set("Cache-Control", "no-cache")
		_, _ = response.Write(sourceData)
	})
	mux.HandleFunc("/healthz", func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(response, "ok")
	})
	mux.HandleFunc("/", func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/" && request.URL.Path != "/index.html" {
			http.NotFound(response, request)
			return
		}
		response.Header().Set("Content-Type", "text/html; charset=utf-8")
		response.Header().Set("Cache-Control", "no-store")
		_, _ = response.Write(page)
	})
	return mux
}
