package api

import (
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

	// Protected group — auth + rate limit.
	protected := v1.Group("")
	if cfg.Auth.Enabled {
		protected.Use(middleware.Auth(cfg.Auth.APIKeys))
	}
	protected.Use(middleware.RateLimit(cfg.RateLimit))

	// Scrape
	protected.POST("/scrape", handler.Scrape(scrapeRunner))

	// Extract (structured extraction via LLM)
	protected.POST("/extract", handler.Extract(extractService))

	// Batch
	protected.POST("/batch/scrape", handler.PostBatch(batchService))
	protected.GET("/batch/:id", handler.GetBatch(batchService))

	// Crawl
	protected.POST("/crawl", handler.PostCrawl(crawlService))
	protected.GET("/crawl/:id", handler.GetCrawl(crawlService))

	// Map
	protected.POST("/map", handler.PostMap(mapService))

	// Re-verify evidence-backed facts against a durable current observation.
	protected.POST("/verify", handler.Verify(verifyService))

	// Manually wake one exact durable extractor-heal run.
	protected.POST("/extractors/:id/heal", handler.PostExtractorHeal(options.extractorHealService))

	return r
}
