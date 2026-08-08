package scrape

import (
	"context"
	"fmt"
	"time"

	"github.com/use-agent/purify/engine"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/scraper"
)

// EngineFetcher adapts one fetch-layer engine to the canonical ordered
// service. It performs no persistence; Service finalizes only the candidate
// that survives the quality gate.
type EngineFetcher struct {
	backend engine.Engine
}

// NewEngineFetcher wraps one configured fetch engine.
func NewEngineFetcher(backend engine.Engine) (*EngineFetcher, error) {
	if backend == nil {
		return nil, fmt.Errorf("scrape: fetch engine is required")
	}
	return &EngineFetcher{backend: backend}, nil
}

// Name returns the stable engine name exposed in fetch attempts.
func (fetcher *EngineFetcher) Name() string {
	if fetcher == nil || fetcher.backend == nil {
		return ""
	}
	return fetcher.backend.Name()
}

// Supports delegates capability selection after mapping every fetch option.
func (fetcher *EngineFetcher) Supports(request *models.ScrapeRequest) bool {
	if fetcher == nil || fetcher.backend == nil || request == nil {
		return false
	}
	return fetcher.backend.Supports(fetchRequest(request))
}

// Fetch invokes the engine and maps its private result to the raw source used
// by the quality gate. The request remains caller-owned and unmodified.
func (fetcher *EngineFetcher) Fetch(ctx context.Context, request *models.ScrapeRequest) (*scraper.ScrapeResult, error) {
	if fetcher == nil || fetcher.backend == nil {
		return nil, fmt.Errorf("scrape: fetch engine is required")
	}
	if request == nil {
		return nil, models.NewScrapeError(models.ErrCodeInvalidInput, "scrape request is required", nil)
	}

	result, err := fetcher.backend.Fetch(ctx, fetchRequest(request))
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("scrape: engine %s returned a nil result", fetcher.backend.Name())
	}

	engineName := result.EngineName
	if engineName == "" {
		engineName = fetcher.backend.Name()
	}
	return &scraper.ScrapeResult{
		RawHTML:     result.HTML,
		Title:       result.Title,
		StatusCode:  result.StatusCode,
		FinalURL:    result.FinalURL,
		EngineUsed:  engineName,
		FetchMethod: fetchMethod(engineName),
		ContentType: result.ContentType,
	}, nil
}

func fetchRequest(request *models.ScrapeRequest) *engine.FetchRequest {
	timeout := time.Duration(request.Timeout) * time.Second
	return scraper.FetchRequestFromScrapeRequest(request, timeout)
}

func fetchMethod(engineName string) string {
	if engineName == "http" {
		return "http"
	}
	return "browser"
}
