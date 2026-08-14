package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/use-agent/purify/discovery"
	"github.com/use-agent/purify/indexer"
	"github.com/use-agent/purify/searchindex"
)

func main() {
	seedsPath := flag.String("seeds", "", "JSON array of seed URLs")
	outPath := flag.String("out", "./data/index.db", "index database path")
	maxPages := flag.Int("max-pages", 500, "stop after this many indexed pages")
	maxPerHost := flag.Int("max-per-host", 10000, "per-domain page cap")
	maxFrontierPerHost := flag.Int("max-frontier-per-host", 0, "per-domain frontier row budget for link discovery (0 = 2x max-per-host)")
	reopenStarved := flag.Int("reopen-starved", 0, "refetch closed rows of domains holding fewer frontier rows than this (0 = off)")
	pruneFrontier := flag.Bool("prune-frontier", false, "drop queued URLs the current admission rules reject")
	allowPrivate := flag.Bool("allow-private", false, "allow loopback/private seed hosts")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *seedsPath, *outPath, *maxPages, *maxPerHost, *maxFrontierPerHost, *reopenStarved, *pruneFrontier, *allowPrivate); err != nil {
		fmt.Fprintf(os.Stderr, "purify-index: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, seedsPath, outPath string, maxPages, maxPerHost, maxFrontierPerHost, reopenStarved int, pruneFrontier, allowPrivate bool) error {
	if seedsPath == "" {
		return fmt.Errorf("-seeds is required")
	}
	seeds, err := indexer.LoadSeeds(seedsPath)
	if err != nil {
		return err
	}
	store, err := searchindex.Open(outPath)
	if err != nil {
		return err
	}
	defer store.Close()
	fetcher, err := indexer.NewFetcher(indexer.FetcherConfig{AllowPrivateNetworks: allowPrivate})
	if err != nil {
		return err
	}
	discoverer, err := discovery.NewService(discovery.Config{
		MaxURLs:              10000,
		UserAgent:            fetcher.UserAgent(),
		AllowPrivateNetworks: allowPrivate,
	})
	if err != nil {
		return err
	}
	stats, err := indexer.Run(ctx, store, fetcher, discoverer, seeds, indexer.RunConfig{
		MaxPages: maxPages, MaxPerHost: maxPerHost,
		MaxFrontierPerHost: maxFrontierPerHost,
		ReopenStarvedBelow: reopenStarved, PruneFrontier: pruneFrontier,
		AllowPrivate: allowPrivate,
	})
	if err != nil {
		return err
	}
	fmt.Printf("discovered=%d indexed=%d failed=%d robots_deny=%d not_modified=%d skipped_content=%d\n",
		stats.Discovered, stats.Indexed, stats.Failed, stats.RobotsDeny, stats.Skipped304, stats.SkippedContent)
	return nil
}
