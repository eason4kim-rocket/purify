package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	answerdomain "github.com/use-agent/purify/answer"
	"github.com/use-agent/purify/api"
	"github.com/use-agent/purify/api/handler"
	"github.com/use-agent/purify/batch"
	"github.com/use-agent/purify/cache"
	"github.com/use-agent/purify/cleaner"
	compilerdomain "github.com/use-agent/purify/compiler"
	"github.com/use-agent/purify/config"
	crawldomain "github.com/use-agent/purify/crawl"
	"github.com/use-agent/purify/discovery"
	"github.com/use-agent/purify/engine"
	extractdomain "github.com/use-agent/purify/extract"
	"github.com/use-agent/purify/jobs"
	"github.com/use-agent/purify/ledger"
	"github.com/use-agent/purify/llm"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/proxy"
	"github.com/use-agent/purify/publicnet"
	"github.com/use-agent/purify/receipts"
	"github.com/use-agent/purify/revisit"
	"github.com/use-agent/purify/scrape"
	"github.com/use-agent/purify/scraper"
	searchdomain "github.com/use-agent/purify/search"
	"github.com/use-agent/purify/snapshot"
	verifydomain "github.com/use-agent/purify/verify"
	"github.com/use-agent/purify/webhook"
)

const (
	defaultVerifyRevisitTimeout             = 30 * time.Second
	maximumVerifyRevisitTimeout             = 120 * time.Second
	maximumVerifyObservationBodyBytes int64 = 4 << 20
)

func main() {
	if err := run(); err != nil {
		slog.Error("purify stopped with an error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	// ── 1. Load configuration ───────────────────────────────────────
	cfg := config.Load()
	if err := config.ValidateCompilerConfig(cfg.Compiler, cfg.Storage.SnapshotEnabled); err != nil {
		return fmt.Errorf("validate managed compiler configuration: %w", err)
	}
	if err := config.ValidateEAVConfig(cfg.EAV); err != nil {
		return fmt.Errorf("validate entity attribution configuration: %w", err)
	}
	if err := validateManagedSearchConfig(cfg.Search); err != nil {
		return fmt.Errorf("validate managed search configuration: %w", err)
	}
	if err := validateManagedRerankConfig(cfg.Rerank); err != nil {
		return fmt.Errorf("validate managed reranker configuration: %w", err)
	}

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
		return fmt.Errorf("initialise receipt signing key: %w", err)
	}
	receiptSigner, err := receipts.NewSigner(receiptPrivateKey)
	if err != nil {
		return fmt.Errorf("initialise receipt signer: %w", err)
	}
	slog.Info("receipt signing enabled", "kid", receiptKID)

	// ── 2b. Initialise the verification ledger ─────────────────────
	ledgerStore, err := ledger.Open(cfg.Storage.DataDir)
	if err != nil {
		return fmt.Errorf("initialise verification ledger: %w", err)
	}
	defer ledgerStore.Close()
	compiledStore, err := compilerdomain.NewStore(ledgerStore)
	if err != nil {
		return fmt.Errorf("initialise compiled extractor store: %w", err)
	}

	// ── 3. Initialise scraper (launches browser) ────────────────────
	sc, err := scraper.NewScraper(cfg.Browser, cfg.Scraper)
	if err != nil {
		return fmt.Errorf("initialise scraper: %w", err)
	}
	defer sc.Close()

	// ── 3a. Initialise the content-addressed snapshot store ─────────
	snapshotStore, err := openSnapshotStore(cfg.Storage)
	if err != nil {
		return fmt.Errorf("initialise snapshot store: %w", err)
	}
	if snapshotStore != nil {
		defer snapshotStore.Close()
		sc.SetSnapshotStore(snapshotStore)
		slog.Info("snapshot store enabled", "dataDir", cfg.Storage.DataDir)
	} else {
		slog.Info("snapshot store disabled")
	}

	// ── 3b. Enforce one public-only outbound policy ─────────────────
	outboundPolicy, err := newOutboundPolicy(cfg.Browser.DefaultProxy)
	if err != nil {
		return fmt.Errorf("initialise outbound network policy: %w", err)
	}

	// ── 3b-1. Start the one process-owned safe egress relay ─────────
	// Verification revisit and multi-source extraction share this loopback
	// SOCKS5 boundary; both capabilities follow the snapshot capability.
	safeRelay, safeProxyURL, err := newManagedSafeRelay(snapshotStore, outboundPolicy)
	if err != nil {
		return fmt.Errorf("initialise safe egress relay: %w", err)
	}
	if safeRelay != nil {
		defer safeRelay.Close()
		slog.Info("safe egress relay enabled")
	} else {
		slog.Info("safe egress relay disabled because snapshots are disabled")
	}

	// ── 3d. Initialise optional process-owned compiler synthesis ─────
	managedCompiler, err := newManagedCompilerRuntime(cfg.Compiler, ledgerStore, compiledStore, snapshotStore, outboundPolicy)
	if err != nil {
		return fmt.Errorf("initialise managed compiler: %w", err)
	}
	if managedCompiler != nil {
		// Registered after snapshot and ledger cleanup so LIFO shutdown is:
		// coordinator -> managed HTTP idles -> snapshot -> ledger.
		defer managedCompiler.Close()
		slog.Info("managed compiler enabled")
	} else {
		slog.Info("managed compiler disabled")
	}
	compilerBindings := bindCompilerServices(compiledStore, managedCompiler)

	// ── 3d-1. Run the durable self-heal worker ──────────────────────
	managedHeal, err := newManagedHealRuntime(context.Background(), cfg.Heal, compiledStore, snapshotStore)
	if err != nil {
		return fmt.Errorf("initialise self-heal runtime: %w", err)
	}
	if managedHeal != nil {
		defer managedHeal.Close()
		slog.Info("self-heal runtime enabled")
	} else {
		slog.Info("self-heal runtime disabled because snapshots are disabled")
	}

	// ── 3e. Deliver transactionally queued verification webhooks ───
	webhookClient, err := webhook.NewPublicHTTPClient(outboundPolicy, webhook.DefaultOutboxHTTPTimeout)
	if err != nil {
		return fmt.Errorf("initialise webhook HTTP client: %w", err)
	}
	defer webhookClient.CloseIdleConnections()
	webhookDeliverer, err := webhook.NewRawDeliverer(webhookClient)
	if err != nil {
		return fmt.Errorf("initialise webhook deliverer: %w", err)
	}
	outboxWorker, err := webhook.NewOutboxWorker(context.Background(), ledgerStore, webhookDeliverer, webhook.OutboxWorkerOptions{})
	if err != nil {
		return fmt.Errorf("initialise webhook outbox worker: %w", err)
	}
	defer outboxWorker.Close()

	// Explicit post-drain shutdown order for background work; the per-resource
	// defers above remain idempotent initialization-failure fallbacks.
	backgroundLifecycle := &managedBackgroundLifecycle{
		outbox:      outboxWorker,
		closeOutbox: webhookClient.CloseIdleConnections,
	}
	if managedCompiler != nil {
		backgroundLifecycle.compiler = managedCompiler
	}
	if managedHeal != nil {
		backgroundLifecycle.heal = managedHeal
	}
	if safeRelay != nil {
		backgroundLifecycle.relay = safeRelay
	}

	// ── 3f. Build a provenance-preserving verification service ──────
	var verifyService handler.VerifyService
	var verifyRunner *verifydomain.Service
	if snapshotStore != nil {
		rodFetch := newRodFetch(sc)
		revisitService, revisitErr := revisit.New(revisit.Config{
			Engines: []engine.Engine{
				engine.NewHTTPEngine(""),
				engine.NewRodEngine(rodFetch, false),
				engine.NewRodEngine(rodFetch, true),
			},
			Finalizer:        sc,
			Policy:           outboundPolicy,
			SafeProxyURL:     safeProxyURL,
			Timeout:          verifyRevisitTimeout(cfg.Scraper),
			MaximumBodyBytes: maximumVerifyObservationBodyBytes,
		})
		if revisitErr != nil {
			return fmt.Errorf("initialise page revisit service: %w", revisitErr)
		}
		verifyCore, verifyErr := verifydomain.NewService(verifydomain.Config{
			Revisitor:          revisitService,
			Snapshots:          snapshotStore,
			Receipts:           receiptSigner,
			Recorder:           ledgerStore,
			ExtractorRevisions: compilerBindings.extractorRevisions,
		})
		if verifyErr != nil {
			return fmt.Errorf("initialise fact verification service: %w", verifyErr)
		}
		verifyService = verifyCore
		verifyRunner = verifyCore
		slog.Info("fact verification enabled")
	} else {
		slog.Info("fact verification unavailable because snapshots are disabled")
	}

	// ── 4. Initialise cleaner ───────────────────────────────────────
	cl := cleaner.NewCleaner()

	// ── 4b. Initialise cache ────────────────────────────────────────
	cc := cache.New(cfg.Cache.MaxEntries)
	defer cc.Close()

	// ── 4c. Initialise the canonical ordered scrape service ─────────
	scrapeService, err := newCanonicalScrapeService(sc, cl, cc, cfg)
	if err != nil {
		return fmt.Errorf("initialise canonical scrape service: %w", err)
	}

	// ── 4d. Initialise process-wide bounded background work ─────────
	jobWorkers := cfg.Browser.MaxPages
	if jobWorkers <= 0 {
		jobWorkers = 5
	}
	jobExecutor, err := jobs.NewExecutor(jobWorkers, 500)
	if err != nil {
		return fmt.Errorf("initialise background job executor: %w", err)
	}
	defer jobExecutor.Close()

	batchService, err := batch.NewService(scrapeService, jobExecutor, batch.Config{})
	if err != nil {
		return fmt.Errorf("initialise batch service: %w", err)
	}
	defer batchService.Close()

	crawlService, err := crawldomain.NewService(scrapeService, jobExecutor, crawldomain.Config{})
	if err != nil {
		return fmt.Errorf("initialise crawl service: %w", err)
	}
	defer crawlService.Close()

	mapService, err := discovery.NewService(discovery.Config{})
	if err != nil {
		return fmt.Errorf("initialise map service: %w", err)
	}

	// ── 4e. Initialise LLM client ───────────────────────────────────
	requestLLMHTTPClient, err := llm.NewPublicHTTPClient(outboundPolicy, 0)
	if err != nil {
		return fmt.Errorf("initialise request LLM HTTP client: %w", err)
	}
	defer requestLLMHTTPClient.CloseIdleConnections()
	requestLLMClient := llm.NewClient(requestLLMHTTPClient)
	var sourceJudge extractdomain.SourceJudge
	if cfg.EAV.Enabled {
		managedEAVPolicy, policyErr := newManagedEAVPolicy(cfg.Browser.DefaultProxy, cfg.EAV.AllowPrivate)
		if policyErr != nil {
			return fmt.Errorf("initialise entity-attribution network policy: %w", policyErr)
		}
		managedEAV, managedErr := newManagedSourceJudgeRuntime(cfg.EAV, managedEAVPolicy)
		if managedErr != nil {
			return fmt.Errorf("initialise entity attribution: %w", managedErr)
		}
		defer managedEAV.Close()
		sourceJudge = managedEAV
	}
	extractService, err := extractdomain.NewService(scrapeService, requestLLMClient, receiptSigner, extractdomain.Config{
		CompiledRepository: compilerBindings.compiledRepository,
		CompileObserver:    compilerBindings.compileObserver,
		SafeProxyURL:       safeProxyURL,
		SourceJudge:        sourceJudge,
	})
	if err != nil {
		return fmt.Errorf("initialise extract service: %w", err)
	}
	if safeProxyURL != "" {
		slog.Info("multi-source extraction enabled")
	} else {
		slog.Info("multi-source extraction disabled because snapshots are disabled")
	}
	if sourceJudge != nil {
		slog.Info("entity attribution enabled")
	}

	// ── 4f. Initialise request-driven Search ────────────────────────
	// Enrichment reuses the extract service's public-only artifact boundary
	// and follows the snapshot capability like every evidence-bearing path.
	var searchArtifacts searchdomain.ArtifactService
	var searchSigner searchdomain.ReceiptSigner
	if snapshotStore != nil {
		searchArtifacts = extractService
		searchSigner = receiptSigner
	}
	managedSearch, err := newManagedSearchRuntime(cfg, outboundPolicy, searchArtifacts, searchSigner)
	if err != nil {
		return fmt.Errorf("initialise managed search: %w", err)
	}
	switch {
	case managedSearch != nil && managedSearch.enriched:
		defer managedSearch.Close()
		slog.Info("search configured with verified enrichment")
	case managedSearch != nil:
		defer managedSearch.Close()
		slog.Info("search configured without enrichment")
	default:
		slog.Info("search disabled")
	}

	// ── 4g. Initialise the belief-mode Answer service ────────────────
	// Answer composes fresh Search baselines with multi-source consensus
	// extraction, so it requires both capabilities and its own admission burst;
	// lowering Search's baseline gate must not implicitly enable Answer/Watch.
	var answerService handler.AnswerService
	var answerCore *answerdomain.Service
	if managedSearch != nil && safeProxyURL != "" && managedAnswerCapabilityEnabled(cfg) {
		composedAnswer, answerErr := answerdomain.NewService(managedSearch.service, extractService)
		if answerErr != nil {
			return fmt.Errorf("initialise answer service: %w", answerErr)
		}
		answerCore = composedAnswer
		answerService = composedAnswer
		slog.Info("answer enabled")
	} else {
		slog.Info("answer disabled without search and multi-source extraction")
	}

	// ── 4h. Run the durable watch store and scheduler ───────────────
	// Watch follows verification and answer: the binary must never accept a
	// watch it cannot bootstrap or revisit.
	managedWatch, err := newManagedWatchRuntime(context.Background(), ledgerStore, verifyRunner, answerCore)
	if err != nil {
		return fmt.Errorf("initialise watch runtime: %w", err)
	}
	if managedWatch != nil {
		defer managedWatch.Close()
		backgroundLifecycle.watch = managedWatch
		slog.Info("watch enabled")
	} else {
		slog.Info("watch disabled without verification and answer")
	}

	// ── 5. Setup router ─────────────────────────────────────────────
	startTime := time.Now()
	router := api.NewRouterWithOptions(
		sc,
		extractService,
		receiptSigner,
		cfg,
		cc,
		startTime,
		scrapeService,
		batchService,
		crawlService,
		mapService,
		verifyService,
		api.WithSearchService(managedSearchHandlerService(managedSearch)),
		api.WithExtractorHealService(managedHealHandlerService(managedHeal)),
		api.WithAnswerService(answerService),
		api.WithWatchService(managedWatchHandlerService(managedWatch)),
	)

	// ── 6. Start HTTP server ────────────────────────────────────────
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	srv := &http.Server{
		Addr:    addr,
		Handler: router,
	}

	// ── 7. Graceful shutdown ────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(quit)

	if err := listenAndServeUntilShutdown(srv, quit, 5*time.Second); err != nil {
		return err
	}

	if closeErr := backgroundLifecycle.Close(); closeErr != nil {
		slog.Error("background shutdown reported failures", "error", closeErr)
	}

	// sc.Close() runs via defer — drains page pool and kills Chrome.
	slog.Info("purify stopped")
	return nil
}

// listenAndServeUntilShutdown binds synchronously so a listen failure returns
// through run and all already-registered resource defers are honored.
func listenAndServeUntilShutdown(server *http.Server, quit <-chan os.Signal, shutdownTimeout time.Duration) error {
	if server == nil {
		return errors.New("HTTP server is nil")
	}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", server.Addr, err)
	}
	defer listener.Close()

	slog.Info("HTTP server listening", "addr", listener.Addr().String())
	return serveUntilShutdown(server, listener, quit, shutdownTimeout)
}

// serveUntilShutdown returns unexpected Serve failures to its caller. Once a
// shutdown signal arrives, it first drains active requests and then force
// closes the server if the drain deadline expires or Shutdown otherwise fails.
func serveUntilShutdown(server *http.Server, listener net.Listener, quit <-chan os.Signal, shutdownTimeout time.Duration) error {
	if server == nil {
		return errors.New("HTTP server is nil")
	}
	if listener == nil {
		return errors.New("HTTP listener is nil")
	}

	serveErrors := make(chan error, 1)
	go func() {
		serveErrors <- server.Serve(listener)
	}()

	select {
	case err := <-serveErrors:
		return normalizeServeError(err)
	case sig, ok := <-quit:
		if !ok {
			closeErr := server.Close()
			return errors.Join(
				errors.New("shutdown signal channel closed"),
				wrapError("force close HTTP server", closeErr),
			)
		}
		signalName := "unknown"
		if sig != nil {
			signalName = sig.String()
		}
		slog.Info("shutdown signal received", "signal", signalName)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	shutdownErr := server.Shutdown(shutdownCtx)
	cancel()
	if shutdownErr != nil {
		slog.Error("HTTP server graceful shutdown failed; forcing close", "error", shutdownErr)
		closeErr := server.Close()
		serveErr := normalizeServeError(<-serveErrors)
		return errors.Join(
			fmt.Errorf("drain HTTP server: %w", shutdownErr),
			wrapError("force close HTTP server", closeErr),
			serveErr,
		)
	}

	if err := normalizeServeError(<-serveErrors); err != nil {
		return err
	}
	slog.Info("HTTP server drained gracefully")
	return nil
}

func normalizeServeError(err error) error {
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("serve HTTP: %w", err)
}

func wrapError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func newCanonicalScrapeService(sc *scraper.Scraper, cl *cleaner.Cleaner, cc *cache.Cache, cfg *config.Config) (*scrape.Service, error) {
	if sc == nil || cl == nil || cfg == nil {
		return nil, fmt.Errorf("canonical scrape service requires scraper, cleaner, and config")
	}

	// Rod callbacks bypass the legacy dispatcher. The canonical service owns
	// ordered escalation and persists only the quality-selected candidate.
	rodFetch := newRodFetch(sc)

	rodEngine := engine.NewRodEngine(rodFetch, false)
	stealthEngine := engine.NewRodEngine(rodFetch, true)
	backends := []engine.Engine{rodEngine, stealthEngine}
	if cfg.Engine.EnableMultiEngine {
		httpEngine := engine.NewHTTPEngine(cfg.Browser.DefaultProxy)
		backends = []engine.Engine{httpEngine, rodEngine, stealthEngine}
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

type boundedRodScraper interface {
	DoScrapeRodBounded(context.Context, *models.ScrapeRequest, int64) (*scraper.ScrapeResult, error)
}

func newRodFetch(sc boundedRodScraper) engine.RodFetchFunc {
	return func(ctx context.Context, request *engine.FetchRequest) (*engine.FetchResult, error) {
		scrapeRequest := scraper.ScrapeRequestFromFetchRequest(request)
		result, err := sc.DoScrapeRodBounded(ctx, scrapeRequest, request.MaximumBodyBytes)
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
}

func newOutboundPolicy(defaultProxyURL string) (*publicnet.Policy, error) {
	options, err := networkPolicyOptions(defaultProxyURL)
	if err != nil {
		return nil, err
	}
	return publicnet.NewPolicy(options), nil
}

// newManagedEAVPolicy is the only construction boundary that can opt out of
// public-only destination checks. Its result is used only by the process-owned
// entity-attribution runtime and is never shared with request-driven clients.
func newManagedEAVPolicy(defaultProxyURL string, allowPrivate bool) (*publicnet.Policy, error) {
	options, err := networkPolicyOptions(defaultProxyURL)
	if err != nil {
		return nil, err
	}
	options.AllowPrivateNetworks = allowPrivate
	return publicnet.NewPolicy(options), nil
}

func networkPolicyOptions(defaultProxyURL string) (publicnet.Options, error) {
	options := publicnet.Options{}
	if defaultProxyURL != "" {
		dialContext, err := proxy.NewExternalDialContext(defaultProxyURL)
		if err != nil {
			return publicnet.Options{}, err
		}
		options.DialContext = dialContext
	}
	return options, nil
}

func verifyRevisitTimeout(scraperConfig config.ScraperConfig) time.Duration {
	timeout := scraperConfig.DefaultTimeout
	if timeout <= 0 {
		timeout = defaultVerifyRevisitTimeout
	}
	if scraperConfig.MaxTimeout > 0 && timeout > scraperConfig.MaxTimeout {
		timeout = scraperConfig.MaxTimeout
	}
	if timeout > maximumVerifyRevisitTimeout {
		timeout = maximumVerifyRevisitTimeout
	}
	return timeout
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
