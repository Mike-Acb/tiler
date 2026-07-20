package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreviewHandlerServesDemoAndTileset(t *testing.T) {
	tilesDir := t.TempDir()
	tileset := []byte(`{"asset":{"version":"1.1"}}`)
	if err := os.WriteFile(filepath.Join(tilesDir, "tileset.json"), tileset, 0644); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(newHandler(tilesDir))
	defer server.Close()

	response, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("demo status = %d", response.StatusCode)
	}
	page := string(body)
	for _, required := range []string{"Cesium3DTileset.fromUrl", "/tile-sources.json", "Taichung 3D Buildings"} {
		if !strings.Contains(page, required) {
			t.Fatalf("demo page does not contain %q", required)
		}
	}

	response, err = http.Get(server.URL + "/tiles/tileset.json")
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(body) != string(tileset) {
		t.Fatalf("tileset response: status=%d body=%q", response.StatusCode, body)
	}
	if got := response.Header.Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("tileset Cache-Control = %q, want no-cache so rebuilt manifests are visible", got)
	}
}

func TestPreviewHandlerDiscoversAndServesEastCompanionTileset(t *testing.T) {
	parent := t.TempDir()
	mainDir := filepath.Join(parent, "3d-tiles")
	eastDir := filepath.Join(parent, "3d-tiles-east")
	for _, dir := range []string{mainDir, eastDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	mainTileset := []byte(`{"asset":{"version":"1.1"},"name":"main"}`)
	eastTileset := []byte(`{"asset":{"version":"1.1"},"name":"east"}`)
	if err := os.WriteFile(filepath.Join(mainDir, "tileset.json"), mainTileset, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(eastDir, "tileset.json"), eastTileset, 0644); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(newHandler(mainDir))
	defer server.Close()

	response, err := http.Get(server.URL + "/tiles-east/tileset.json")
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if response.StatusCode != http.StatusOK || string(body) != string(eastTileset) {
		t.Fatalf("east tileset response: status=%d body=%q", response.StatusCode, body)
	}

	response, err = http.Get(server.URL + "/tile-sources.json")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var sources []string
	if err := json.NewDecoder(response.Body).Decode(&sources); err != nil {
		t.Fatal(err)
	}
	want := []string{"/tiles/tileset.json", "/tiles-east/tileset.json"}
	if len(sources) != len(want) || sources[0] != want[0] || sources[1] != want[1] {
		t.Fatalf("tile sources = %v, want %v", sources, want)
	}
}

func TestPreviewHandlerPrefersFullEastCompanion(t *testing.T) {
	parent := t.TempDir()
	mainDir := filepath.Join(parent, "3d-tiles")
	eastDir := mainDir + "-east"
	fullEastDir := mainDir + "-east-full"
	for _, dir := range []string{mainDir, eastDir, fullEastDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	for dir, name := range map[string]string{mainDir: "main", eastDir: "small", fullEastDir: "full"} {
		data := []byte(`{"asset":{"version":"1.1"},"name":"` + name + `"}`)
		if err := os.WriteFile(filepath.Join(dir, "tileset.json"), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(newHandler(mainDir))
	defer server.Close()

	response, err := http.Get(server.URL + "/tiles-east/tileset.json")
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(body), `"name":"full"`) {
		t.Fatalf("east route did not prefer full companion: %s", body)
	}
}

func TestPreviewDemoUsesSatelliteBasemapByDefault(t *testing.T) {
	page, err := assets.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(page)
	for _, required := range []string{
		"ArcGisMapServerImageryProvider.fromUrl",
		"World_Imagery/MapServer",
		"Satellite imagery",
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("demo page does not contain satellite default %q", required)
		}
	}
	if strings.Contains(html, "OpenStreetMapImageryProvider") {
		t.Fatal("demo still creates the OpenStreetMap provider")
	}
}

func TestPreviewDemoUsesFineLODDefaults(t *testing.T) {
	page, err := assets.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(page)
	for _, required := range []string{
		`NLSC · 113_B · L3–L11`,
		`min="0.25" max="32" step="0.25" value="0.5"`,
		`dynamicScreenSpaceError: false`,
		`search.get("lon")`,
		`search.get("lat")`,
		`search.get("height")`,
		`viewer.camera.lookAt`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("demo page does not contain fine LOD default %q", required)
		}
	}
}

func TestPreviewDemoUsesUnlitColorsByDefault(t *testing.T) {
	page, err := assets.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(page)
	for _, required := range []string{
		`id="unlit" type="checkbox" checked`,
		`lightingModel: Cesium.LightingModel.UNLIT`,
		`tileset.customShader = unlitShader`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("demo page does not contain unlit color default %q", required)
		}
	}
}

func TestPreviewDemoLoadsEveryDiscoveredTileset(t *testing.T) {
	page, err := assets.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(page)
	for _, required := range []string{
		`fetch("/tile-sources.json")`,
		`const tilesets = []`,
		`for (const tileset of tilesets)`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("demo page does not contain multi-tileset behavior %q", required)
		}
	}
}
