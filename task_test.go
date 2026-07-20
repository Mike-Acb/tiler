package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulmach/orb"
)

func encodedImage(t *testing.T, format string) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.White)

	var buf bytes.Buffer
	var err error
	switch format {
	case JPG:
		err = jpeg.Encode(&buf, img, nil)
	case PNG:
		err = png.Encode(&buf, img)
	default:
		t.Fatalf("unsupported test image format: %s", format)
	}
	if err != nil {
		t.Fatalf("encode %s test image: %v", format, err)
	}
	return buf.Bytes()
}

func TestValidateTileDataAcceptsJPEG(t *testing.T) {
	if err := validateTileData(JPG, encodedImage(t, JPG)); err != nil {
		t.Fatalf("expected valid JPEG tile to pass: %v", err)
	}
}

func TestValidateTileDataAcceptsPNG(t *testing.T) {
	if err := validateTileData(PNG, encodedImage(t, PNG)); err != nil {
		t.Fatalf("expected valid PNG tile to pass: %v", err)
	}
}

func TestValidateTileDataAcceptsWebP(t *testing.T) {
	webpHeader := []byte("RIFF\x00\x00\x00\x00WEBPVP8 ")
	if err := validateTileData(WEBP, webpHeader); err != nil {
		t.Fatalf("expected valid WebP tile to pass: %v", err)
	}
}

func TestValidateTileDataRejectsHTMLForJPEG(t *testing.T) {
	err := validateTileData(JPG, []byte("<!doctype html><title>rate limited</title>"))
	if err == nil {
		t.Fatal("expected HTML response to be rejected as a JPEG tile")
	}
}

func TestValidateTileDataRejectsMismatchedImageFormat(t *testing.T) {
	err := validateTileData(JPG, encodedImage(t, PNG))
	if err == nil {
		t.Fatal("expected PNG response to be rejected as a JPEG tile")
	}
}

func TestWriteFileMetadataForArchiveConversion(t *testing.T) {
	dir := t.TempDir()
	polygon := orb.Polygon{{
		{62.20, 24.99},
		{62.20, 25.25},
		{62.46, 25.25},
		{62.46, 24.99},
		{62.20, 24.99},
	}}
	task := Task{
		ID:   "smoke-test",
		Name: "google_indian_ocean_smoke",
		File: dir,
		Min:  0,
		Max:  8,
		Layers: []Layer{{
			Zoom:       8,
			Collection: orb.Collection{polygon},
		}},
		TileMap: TileMap{
			Name:   "google_indian_ocean_smoke",
			Format: JPG,
			Schema: "xyz",
		},
	}

	if err := task.writeFileMetadata(); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata.json: %v", err)
	}
	var metadata map[string]string
	if err := json.Unmarshal(data, &metadata); err != nil {
		t.Fatalf("decode metadata.json: %v", err)
	}

	for key, want := range map[string]string{
		"format":  JPG,
		"minzoom": "0",
		"maxzoom": "8",
		"bounds":  "62.200000,24.990000,62.460000,25.250000",
		"center":  "62.330000,25.120000,4",
	} {
		if got := metadata[key]; got != want {
			t.Errorf("metadata[%q] = %q, want %q", key, got, want)
		}
	}
}

func TestDetectTileFormatPrefersMetadata(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(`{"format":"jpg"}`), 0644); err != nil {
		t.Fatalf("write metadata: %v", err)
	}
	tileDir := filepath.Join(dir, "0", "0")
	if err := os.MkdirAll(tileDir, 0755); err != nil {
		t.Fatalf("create tile directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tileDir, "0.png"), encodedImage(t, PNG), 0644); err != nil {
		t.Fatalf("write tile: %v", err)
	}

	cmd := exec.Command("bash", "pmtiles-sync/detect-format.sh", dir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("detect tile format: %v: %s", err, output)
	}
	if got := strings.TrimSpace(string(output)); got != JPG {
		t.Fatalf("detected format = %q, want %q", got, JPG)
	}
}
