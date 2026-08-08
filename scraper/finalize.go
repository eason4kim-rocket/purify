package scraper

import (
	"github.com/use-agent/purify/models"
)

// FinalizeSelected applies selected-source defaults and persists its snapshot.
// Ordered orchestration must call this exactly once, after quality gating, so
// rejected HTTP/browser candidates never enter the durable snapshot store.
func (s *Scraper) FinalizeSelected(req *models.ScrapeRequest, result *ScrapeResult) (*ScrapeResult, error) {
	if s == nil {
		return nil, models.NewScrapeError(models.ErrCodeInternal, "scraper is not configured", nil)
	}
	if req == nil {
		return nil, models.NewScrapeError(models.ErrCodeInvalidInput, "scrape request is required", nil)
	}
	if result == nil {
		return nil, models.NewScrapeError(models.ErrCodeInternal, "selected scrape result is required", nil)
	}
	return s.finalizeScrape(req, result)
}
