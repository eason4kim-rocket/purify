package api

import (
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/use-agent/purify/api/handler"
	"github.com/use-agent/purify/api/middleware"
	"github.com/use-agent/purify/cache"
	"github.com/use-agent/purify/config"
	"github.com/use-agent/purify/receipts"
	"github.com/use-agent/purify/scraper"
)

type routerOptions struct {
	extractorHealService handler.ExtractorHealService
	searchService        handler.SearchService
	answerService        handler.AnswerService
	watchService         handler.WatchService
}

// RouterOption adds an optional API capability without changing the fixed
// positional router construction surface used by embedders.
type RouterOption func(*routerOptions)

// WithExtractorHealService enables durable manual extractor-heal scheduling.
// A nil service keeps the protected route present but unavailable.
func WithExtractorHealService(service handler.ExtractorHealService) RouterOption {
	return func(options *routerOptions) {
		if options != nil {
			options.extractorHealService = service
		}
	}
}

// WithSearchService enables provider-neutral Search. The protected route is
// registered even when the service is nil so unavailable deployments fail
// closed with a stable authenticated 503 instead of changing route shape.
func WithSearchService(service handler.SearchService) RouterOption {
	return func(options *routerOptions) {
		if options != nil {
			options.searchService = service
		}
	}
}

// WithAnswerService enables the belief-mode Answer API. The route remains
// present and authenticated when this option is absent, but fails closed.
func WithAnswerService(service handler.AnswerService) RouterOption {
	return func(options *routerOptions) {
		if options != nil {
			options.answerService = service
		}
	}
}

// WithWatchService enables durable Watch CRUD and bitemporal Facts lookup.
// Their seven routes remain registered and authenticated when absent, but
// fail closed without calling any partial implementation.
func WithWatchService(service handler.WatchService) RouterOption {
	return func(options *routerOptions) {
		if options != nil {
			options.watchService = service
		}
	}
}

// NewRouter creates a configured Gin engine with all routes and middleware.
//
// Middleware chain:
//
//	Global:  Recovery → Logger
//	API:     Auth (if enabled) → RateLimit
//
// Health and receipt verification endpoints are intentionally outside auth.
func NewRouter(sc *scraper.Scraper, extractService handler.ExtractService, receiptSigner *receipts.Signer, cfg *config.Config, cc *cache.Cache, startTime time.Time, scrapeRunner handler.ScrapeRunner, batchService handler.BatchService, crawlService handler.CrawlService, mapService handler.MapService, verifyService handler.VerifyService) *gin.Engine {
	return NewRouterWithOptions(sc, extractService, receiptSigner, cfg, cc, startTime,
		scrapeRunner, batchService, crawlService, mapService, verifyService)
}

// NewRouterWithOptions creates a configured Gin engine with optional
// capabilities while preserving NewRouter's exact historical function type.
func NewRouterWithOptions(sc *scraper.Scraper, extractService handler.ExtractService, receiptSigner *receipts.Signer, cfg *config.Config, cc *cache.Cache, startTime time.Time, scrapeRunner handler.ScrapeRunner, batchService handler.BatchService, crawlService handler.CrawlService, mapService handler.MapService, verifyService handler.VerifyService, configured ...RouterOption) *gin.Engine {
	gin.SetMode(cfg.Server.Mode)
	options := routerOptions{}
	for _, option := range configured {
		if option != nil {
			option(&options)
		}
	}

	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(gin.Logger())

	v1 := r.Group("/api/v1")

	// Health — no auth required.
	v1.GET("/health", handler.Health(sc, startTime))

	// Portable receipt verification and public key — no auth required.
	v1.POST("/receipts/verify", handler.VerifyReceipt(receiptSigner))
	v1.GET("/receipts/pubkey", handler.ReceiptPublicKey(receiptSigner))

	// Weighted routes use separate groups so Search and Answer retain their own
	// stable response envelopes. Every protected group consumes from the same
	// identity limiter.
	limiter := middleware.NewLimiter(cfg.RateLimit)
	searchProtected := v1.Group("")
	if cfg.Auth.Enabled {
		searchProtected.Use(middleware.SearchAuth(cfg.Auth.APIKeys))
	}
	searchService := options.searchService
	if !searchCapabilityEnabled(cfg) {
		searchService = nil
	}
	searchProtected.POST("/search", handler.SearchWithRateLimiter(searchService, limiter))

	answerProtected := v1.Group("")
	if cfg.Auth.Enabled {
		answerProtected.Use(middleware.AnswerAuth(cfg.Auth.APIKeys))
	}
	answerService := options.answerService
	if !answerCapabilityEnabled(cfg) {
		answerService = nil
	}
	answerProtected.POST("/answer", handler.AnswerWithRateLimiter(answerService, limiter))

	watchProtected := v1.Group("")
	if cfg.Auth.Enabled {
		watchProtected.Use(middleware.WatchAuth(cfg.Auth.APIKeys))
	}
	watchService := options.watchService
	if !watchCapabilityEnabled(cfg) {
		watchService = nil
	}
	watchProtected.POST("/watches", handler.CreateWatchWithRateLimiter(watchService, limiter))
	watchProtected.GET("/watches", handler.ListWatchesWithRateLimiter(watchService, limiter))
	watchProtected.GET("/watches/:id", handler.GetWatchWithRateLimiter(watchService, limiter))
	watchProtected.POST("/watches/:id/pause", handler.PauseWatchWithRateLimiter(watchService, limiter))
	watchProtected.POST("/watches/:id/resume", handler.ResumeWatchWithRateLimiter(watchService, limiter))
	watchProtected.DELETE("/watches/:id", handler.DeleteWatchWithRateLimiter(watchService, limiter))
	watchProtected.GET("/facts", handler.GetFactAtWithRateLimiter(watchService, limiter))

	standardProtected := v1.Group("")
	if cfg.Auth.Enabled {
		standardProtected.Use(middleware.Auth(cfg.Auth.APIKeys))
	}
	standardProtected.Use(limiter.MiddlewareFixed(1))

	// Scrape
	standardProtected.POST("/scrape", handler.Scrape(scrapeRunner))

	// Extract (structured extraction via LLM)
	standardProtected.POST("/extract", handler.Extract(extractService))

	// Batch
	standardProtected.POST("/batch/scrape", handler.PostBatch(batchService))
	standardProtected.GET("/batch/:id", handler.GetBatch(batchService))

	// Crawl
	standardProtected.POST("/crawl", handler.PostCrawl(crawlService))
	standardProtected.GET("/crawl/:id", handler.GetCrawl(crawlService))

	// Map
	standardProtected.POST("/map", handler.PostMap(mapService))

	// Re-verify evidence-backed facts against a durable current observation.
	standardProtected.POST("/verify", handler.Verify(verifyService))

	// Manually wake one exact durable extractor-heal run.
	standardProtected.POST("/extractors/:id/heal", handler.PostExtractorHeal(options.extractorHealService))

	return r
}

func searchCapabilityEnabled(cfg *config.Config) bool {
	if cfg == nil || !cfg.Auth.Enabled || cfg.RateLimit.Burst < handler.MinSearchRequestCost {
		return false
	}
	for _, key := range cfg.Auth.APIKeys {
		if strings.TrimSpace(key) != "" {
			return true
		}
	}
	return false
}

func answerCapabilityEnabled(cfg *config.Config) bool {
	if cfg == nil || !cfg.Auth.Enabled || cfg.RateLimit.Burst < handler.MaxAnswerRequestCost {
		return false
	}
	for _, key := range cfg.Auth.APIKeys {
		if strings.TrimSpace(key) != "" {
			return true
		}
	}
	return false
}

func watchCapabilityEnabled(cfg *config.Config) bool {
	if cfg == nil || !cfg.Auth.Enabled || cfg.RateLimit.Burst < handler.MaxWatchRequestCost {
		return false
	}
	for _, key := range cfg.Auth.APIKeys {
		if strings.TrimSpace(key) != "" {
			return true
		}
	}
	return false
}
