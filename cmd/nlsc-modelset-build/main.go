package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"tiler/nlscmodelset"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	options, err := parseArgs(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprintln(os.Stderr, "usage: nlsc-modelset-build -input DOWNLOAD_DIR -output TILESET_DIR [options]")
		return 2
	}
	options.Progress = os.Stderr
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	started := time.Now()
	stats, buildErr := nlscmodelset.BuildArchive(ctx, options)
	fmt.Fprintf(os.Stderr,
		"discovered=%d converted=%d skipped=%d noModel=%d failed=%d elapsed=%s output=%s\n",
		stats.Discovered, stats.Converted, stats.Skipped, stats.NoModel, stats.Failed,
		time.Since(started).Round(time.Millisecond), options.OutputDir)
	if buildErr != nil {
		fmt.Fprintln(os.Stderr, buildErr)
		return 1
	}
	return 0
}

func parseArgs(args []string) (nlscmodelset.BuildOptions, error) {
	var options nlscmodelset.BuildOptions
	flags := flag.NewFlagSet("nlsc-modelset-build", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.StringVar(&options.InputDir, "input", "", "download archive containing raw/ (required)")
	flags.StringVar(&options.OutputDir, "output", "", "custom 3D Tiles output directory (required)")
	flags.IntVar(&options.Workers, "workers", 2, "parallel decode/build workers")
	flags.IntVar(&options.MinLevel, "min-level", 1, "first source level to convert")
	flags.IntVar(&options.MaxLevel, "max-level", 9, "last source level to convert")
	flags.IntVar(&options.Limit, "limit", 0, "convert at most N nodes; 0 means all")
	flags.BoolVar(&options.FinalizeOnly, "finalize-only", false, "publish tileset.json from existing node markers without revalidating source files or GLBs")
	flags.BoolVar(&options.AllowMissingChildren, "allow-missing-children", false, "allow whole branches to stop before max level; partial sibling sets are rejected")
	flags.IntVar(&options.Format.Version, "format-version", nlscmodelset.Taoyuan113HFormat.Version, "MODELSET LAYER format version")
	flags.IntVar(&options.Format.VertexSize, "vertex-size", nlscmodelset.Taoyuan113HFormat.VertexSize, "raw MODELSET vertex size")
	if err := flags.Parse(args); err != nil {
		return options, err
	}
	if flags.NArg() != 0 {
		return options, fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	if options.InputDir == "" {
		return options, errors.New("-input is required")
	}
	if options.OutputDir == "" {
		return options, errors.New("-output is required")
	}
	if options.Workers <= 0 {
		return options, errors.New("-workers must be positive")
	}
	if options.MinLevel <= 0 || options.MaxLevel < options.MinLevel {
		return options, errors.New("invalid min/max level range")
	}
	if options.MaxLevel > 20 {
		return options, errors.New("-max-level above 20 is not supported")
	}
	if options.Limit < 0 {
		return options, errors.New("-limit cannot be negative")
	}
	return options, nil
}
