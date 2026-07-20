package main

import (
	"path/filepath"
	"testing"
	"time"

	"tiler/nlscdownload"
)

func TestParseArgsUsesCustomOutputAndDownloadOptions(t *testing.T) {
	output := filepath.Join(t.TempDir(), "custom", "taoyuan")
	options, err := parseArgs([]string{
		"-output", output,
		"-layer", "113_H",
		"-root-level", "3",
		"-root-row", "2",
		"-root-col", "3",
		"-max-level", "7",
		"-west", "120.530000",
		"-south", "24.311000",
		"-east", "120.565000",
		"-north", "24.329000",
		"-concurrency", "6",
		"-retries", "5",
		"-retry-delay", "1500ms",
		"-timeout", "2m",
		"-cdp-url", "http://localhost:29222/json/version",
		"-progress-every", "250",
	})
	if err != nil {
		t.Fatalf("parseArgs() error = %v", err)
	}
	if options.Config.OutputDir != output || options.Config.Layer != "113_H" {
		t.Fatalf("config path/layer = %q/%q", options.Config.OutputDir, options.Config.Layer)
	}
	if options.Config.Root.Level != 3 || options.Config.Root.Row != 2 || options.Config.Root.Col != 3 || options.Config.MaxLevel != 7 {
		t.Fatalf("tree options = root %+v max %d", options.Config.Root, options.Config.MaxLevel)
	}
	if options.Config.Bounds == nil || *options.Config.Bounds != (nlscdownload.GeographicBounds{West: 120.53, South: 24.311, East: 120.565, North: 24.329}) {
		t.Fatalf("geographic bounds = %+v", options.Config.Bounds)
	}
	if options.Config.Concurrency != 6 || options.Config.Retries != 5 || options.Config.RetryDelay != 1500*time.Millisecond {
		t.Fatalf("download options = concurrency %d retries %d delay %v", options.Config.Concurrency, options.Config.Retries, options.Config.RetryDelay)
	}
	if options.Config.Timeout != 2*time.Minute || options.CDPURL != "http://localhost:29222/json/version" || options.ProgressEvery != 250 {
		t.Fatalf("CLI options = timeout %v CDP %q progress %d", options.Config.Timeout, options.CDPURL, options.ProgressEvery)
	}
}

func TestParseArgsRequiresOutputAndValidCLIOptions(t *testing.T) {
	if _, err := parseArgs(nil); err == nil {
		t.Fatal("parseArgs() accepted a missing output directory")
	}
	if _, err := parseArgs([]string{"-output", t.TempDir(), "-progress-every", "0"}); err == nil {
		t.Fatal("parseArgs() accepted progress-every=0")
	}
}

func TestRunHelpReturnsSuccess(t *testing.T) {
	if code := run([]string{"-h"}); code != 0 {
		t.Fatalf("run(-h) exit code = %d, want 0", code)
	}
}
