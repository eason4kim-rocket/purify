package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/use-agent/purify/api"
	"github.com/use-agent/purify/cache"
	"github.com/use-agent/purify/cleaner"
	"github.com/use-agent/purify/config"
	"github.com/use-agent/purify/engine"
	"github.com/use-agent/purify/llm"
	"github.com/use-agent/purify/receipts"
	"github.com/use-agent/purify/scrape"
	"github.com/use-agent/purify/scraper"
	"github.com/use-agent/purify/snapshot"
)

func main() {
	// ── 1. Load configuration ───────────────────────────────────────
	cfg := config.Load()

	// ── 2. Initialise structured logging ────────────────────────────
	initLogger(cfg.Log)
	slog.Info("purify starting",
		"host", cfg.Server.Host,
		"port", cfg.Server.Port,
		"mode", cfg.Server.Mode,
		"maxPages", cfg.Browser.MaxPages,
	)

	// ── 2a. Initialise durable receipt signing identity ─────────────
	receiptPrivateKey, receiptKID, err := receipts.LoadOrCreateKey(cfg.Storage)
	if err != nil {
		slog.Error("failed to initialise receipt signing key", "error", err)
		os.Exit(1)
	}
	receiptSigner, err := receipts.NewSigner(receiptPrivateKey)
	if err != nil {
		slog.Error("failed to initialise receipt signer", "error", err)
		os.Exit(1)
	}
	slog.Info("receipt signing enabled", "kid", receiptKID)

	// ── 3. Initialise scraper (launches browser) ────────────────────
	sc, err := scraper.NewScraper(cfg.Browser, cfg.Scraper)
	if err != nil {
		slog.Error("failed to initialise scraper", "error", err)
		os.Exit(1)
	}
	defer sc.Close()

	// ── 3a. Initialise the content-addressed snapshot store ─────────
	snapshotStore, err := openSnapshotStore(cfg.Storage)
	if err != nil {
		slog.Error("failed to initialise snapshot store", "error", err)
		sc.Close()
		os.Exit(1)
	}
	if snapshotStore != nil {
		defer snapshotStore.Close()
		sc.SetSnapshotStore(snapshotStore)
		slog.Info("snapshot store enabled", "dataDir", cfg.Storage.DataDir)
	} else {
		slog.Info("snapshot store disabled")
	}

	// ── 4. Initialise cleaner ───────────────────────────────────────
	cl := cleaner.NewCleaner()

	// ── 4b. Initialise cache ────────────────────────────────────────
	cc := cache.New(cfg.Cache.MaxEntries)
	defer cc.Close()

	// ── 4c. Initialise the canonical ordered scrape service ─────────
	scrapeService, err := newCanonicalScrapeService(sc, cl, cc, cfg)
	if err != nil {
		slog.Error("failed to initialise canonical scrape service", "error", err)
		os.Exit(1)
	}

	// ── 4d. Initialise LLM client ───────────────────────────────────
	llmClient := llm.NewClient(nil)

	// ── 5. Setup router ─────────────────────────────────────────────
	startTime := time.Now()
	router := api.NewRouter(sc, cl, llmClient, receiptSigner, cfg, cc, startTime, scrapeService)

	// ── 6. Start HTTP server ────────────────────────────────────────
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	srv := &http.Server{
		Addr:    addr,
		Handler: router,
	}

	go func() {
		slog.Info("HTTP server listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server error", "error", err)
			os.Exit(1)
		}
	}()

	// ── 7. Graceful shutdown ────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	slog.Info("shutdown signal received", "signal", sig.String())

	// Give in-flight requests 5 seconds to complete.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("HTTP server forced shutdown", "error", err)
	} else {
		slog.Info("HTTP server drained gracefully")
	}

	// sc.Close() runs via defer — drains page pool and kills Chrome.
	slog.Info("purify stopped")
}

func newCanonicalScrapeService(sc *scraper.Scraper, cl *cleaner.Cleaner, cc *cache.Cache, cfg *config.Config) (*scrape.Service, error) {
	if sc == nil || cl == nil || cfg == nil {
		return nil, fmt.Errorf("canonical scrape service requires scraper, cleaner, and config")
	}

	// Rod callbacks bypass the legacy dispatcher. The canonical service owns
	// ordered escalation and persists only the quality-selected candidate.
	rodFetch := func(ctx context.Context, request *engine.FetchRequest) (*engine.FetchResult, error) {
		scrapeRequest := scraper.ScrapeRequestFromFetchRequest(request)
		result, err := sc.DoScrapeRod(ctx, scrapeRequest)
		if err != nil {
			return nil, err
		}
		return &engine.FetchResult{
			HTML:        result.RawHTML,
			Title:       result.Title,
			StatusCode:  result.StatusCode,
			FinalURL:    result.FinalURL,
			ContentType: result.ContentType,
		}, nil
	}

	rodEngine := engine.NewRodEngine(rodFetch, false)
	stealthEngine := engine.NewRodEngine(rodFetch, true)
	backends := []engine.Engine{rodEngine, stealthEngine}
	if cfg.Engine.EnableMultiEngine {
		httpEngine := engine.NewHTTPEngine(cfg.Browser.DefaultProxy)
		backends = []engine.Engine{httpEngine, rodEngine, stealthEngine}

		// Batch/Crawl/Extract still call Scraper.DoScrape during their staged
		// migration. Keep their dispatcher configured until those adapters move
		// to the canonical service as well.
		memory := engine.NewDomainMemory(24 * time.Hour)
		sc.SetDispatcher(engine.NewDispatcher(backends, cfg.Engine.EscalationDelays, memory))
		slog.Info("legacy multi-engine dispatcher enabled during service migration",
			"engines", len(backends),
			"delays", cfg.Engine.EscalationDelays,
		)
	}

	fetchers := make([]scrape.Fetcher, 0, len(backends))
	for _, backend := range backends {
		fetcher, err := scrape.NewEngineFetcher(backend)
		if err != nil {
			return nil, err
		}
		fetchers = append(fetchers, fetcher)
	}

	return scrape.NewService(fetchers, cl, cc, sc, scrape.Config{
		MaximumTimeout: cfg.Scraper.MaxTimeout,
	})
}

func openSnapshotStore(cfg config.StorageConfig) (*snapshot.Store, error) {
	if !cfg.SnapshotEnabled {
		return nil, nil
	}
	return snapshot.NewStore(cfg.DataDir)
}

// initLogger configures slog based on the LogConfig.
func initLogger(cfg config.LogConfig) {
	var level slog.Level
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	if cfg.Format == "text" {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}

	slog.SetDefault(slog.New(handler))
}
