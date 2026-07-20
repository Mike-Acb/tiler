package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"syscall"
	"time"

	"tiler/nlscdownload"
)

const defaultBaseURL = "https://mapserver01.nlsc.gov.tw/oview//oview"

type cliOptions struct {
	Config        nlscdownload.Config
	CDPURL        string
	ProgressEvery int
}

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
		fmt.Fprintln(os.Stderr, "usage: nlsc-modelset-download -output DIR [options]")
		return 2
	}
	if err := os.MkdirAll(options.Config.OutputDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if options.CDPURL != "" {
		client, closeClient, err := nlscdownload.NewCDPHTTPClient(options.CDPURL, options.Config.Concurrency, options.Config.Timeout)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer closeClient()
		options.Config.Client = client
	}

	processed := 0
	options.Config.OnEvent = func(event nlscdownload.Event) {
		processed++
		if event.Status == "failed" || processed%options.ProgressEvery == 0 {
			fmt.Fprintf(os.Stderr, "processed=%d node=%d/%d/%d status=%s bytes=%d retries=%d\n",
				processed, event.Node.Level, event.Node.Row, event.Node.Col, event.Status, event.Bytes, event.Retries)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	started := time.Now()
	stats, downloadErr := nlscdownload.Download(ctx, options.Config)
	fmt.Fprintf(os.Stderr,
		"visited=%d requested=%d downloaded=%d empty=%d skipped=%d failed=%d retried=%d bytes=%.1fMiB elapsed=%s output=%s\n",
		stats.Visited, stats.Requested, stats.Downloaded, stats.Empty, stats.Skipped, stats.Failed,
		stats.Retried, float64(stats.Bytes)/(1024*1024), time.Since(started).Round(time.Millisecond), options.Config.OutputDir)
	if downloadErr != nil {
		fmt.Fprintln(os.Stderr, downloadErr)
		return 1
	}
	return 0
}

func parseArgs(args []string) (cliOptions, error) {
	var options cliOptions
	west, south, east, north := math.NaN(), math.NaN(), math.NaN(), math.NaN()
	flags := flag.NewFlagSet("nlsc-modelset-download", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	flags.StringVar(&options.Config.BaseURL, "base-url", defaultBaseURL, "NLSC MODELSET endpoint")
	flags.StringVar(&options.Config.Layer, "layer", "113_H", "MODELSET layer name")
	flags.StringVar(&options.Config.OutputDir, "output", "", "custom output directory (required)")
	flags.IntVar(&options.Config.Root.Level, "root-level", 1, "root node level")
	flags.IntVar(&options.Config.Root.Row, "root-row", 0, "root node row")
	flags.IntVar(&options.Config.Root.Col, "root-col", 0, "root node column")
	flags.IntVar(&options.Config.MaxLevel, "max-level", 9, "deepest level to download")
	flags.Float64Var(&west, "west", west, "optional minimum longitude for recursive traversal")
	flags.Float64Var(&south, "south", south, "optional minimum latitude for recursive traversal")
	flags.Float64Var(&east, "east", east, "optional maximum longitude for recursive traversal")
	flags.Float64Var(&north, "north", north, "optional maximum latitude for recursive traversal")
	flags.IntVar(&options.Config.Concurrency, "concurrency", 4, "parallel HTTP requests")
	flags.IntVar(&options.Config.Retries, "retries", 3, "retries after the first attempt")
	flags.DurationVar(&options.Config.RetryDelay, "retry-delay", 2*time.Second, "delay between retries")
	flags.DurationVar(&options.Config.Timeout, "timeout", 90*time.Second, "timeout for one HTTP request")
	flags.StringVar(&options.CDPURL, "cdp-url", "", "Chrome/Edge remote debugging URL used to load NODE files")
	flags.IntVar(&options.ProgressEvery, "progress-every", 100, "print progress every N visited nodes")
	if err := flags.Parse(args); err != nil {
		return options, err
	}
	if flags.NArg() != 0 {
		return options, fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	if options.Config.OutputDir == "" {
		return options, errors.New("-output is required")
	}
	boundValues := []float64{west, south, east, north}
	provided := 0
	for _, value := range boundValues {
		if !math.IsNaN(value) {
			provided++
		}
	}
	if provided != 0 && provided != len(boundValues) {
		return options, errors.New("-west, -south, -east and -north must be provided together")
	}
	if provided == len(boundValues) {
		bounds := nlscdownload.GeographicBounds{West: west, South: south, East: east, North: north}
		options.Config.Bounds = &bounds
	}
	if options.Config.MaxLevel > 20 {
		return options, errors.New("-max-level above 20 is not supported")
	}
	if options.Config.Timeout <= 0 {
		return options, errors.New("-timeout must be positive")
	}
	if options.ProgressEvery <= 0 {
		return options, errors.New("-progress-every must be positive")
	}
	return options, nil
}
