package main

import (
	"path/filepath"
	"testing"
)

func TestParseArgsSupportsCustomSMBPathsAndResumeDefaults(t *testing.T) {
	input := filepath.Join(t.TempDir(), "download")
	output := filepath.Join(t.TempDir(), "tiles")
	options, err := parseArgs([]string{
		"-input", input,
		"-output", output,
		"-workers", "3",
		"-min-level", "2",
		"-max-level", "9",
		"-limit", "25",
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.InputDir != input || options.OutputDir != output || options.Workers != 3 {
		t.Fatalf("unexpected paths/workers: %+v", options)
	}
	if options.MinLevel != 2 || options.MaxLevel != 9 || options.Limit != 25 {
		t.Fatalf("unexpected range: %+v", options)
	}
	if options.Format.Version != 14 || options.Format.VertexSize != 64 {
		t.Fatalf("unexpected raw format: %+v", options.Format)
	}
}

func TestParseArgsRequiresInputAndOutput(t *testing.T) {
	if _, err := parseArgs(nil); err == nil {
		t.Fatal("accepted missing input and output")
	}
	if _, err := parseArgs([]string{"-input", t.TempDir()}); err == nil {
		t.Fatal("accepted missing output")
	}
}

func TestParseArgsAcceptsFinalizeOnly(t *testing.T) {
	_, err := parseArgs([]string{
		"-input", t.TempDir(),
		"-output", t.TempDir(),
		"-finalize-only",
	})
	if err != nil {
		t.Fatalf("parse finalize-only: %v", err)
	}
}

func TestParseArgsAcceptsStoppedDeepBranches(t *testing.T) {
	_, err := parseArgs([]string{
		"-input", t.TempDir(),
		"-output", t.TempDir(),
		"-allow-missing-children",
	})
	if err != nil {
		t.Fatalf("parse allow-missing-children: %v", err)
	}
}
