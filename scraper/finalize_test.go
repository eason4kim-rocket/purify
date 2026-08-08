package scraper

import (
	"errors"
	"testing"

	"github.com/use-agent/purify/models"
)

func TestFinalizeSelectedValidatesInputs(t *testing.T) {
	scraper := &Scraper{}
	for _, test := range []struct {
		name   string
		target *Scraper
		req    *models.ScrapeRequest
		result *ScrapeResult
		code   string
	}{
		{name: "nil scraper", req: &models.ScrapeRequest{}, result: &ScrapeResult{}, code: models.ErrCodeInternal},
		{name: "nil request", target: scraper, result: &ScrapeResult{}, code: models.ErrCodeInvalidInput},
		{name: "nil result", target: scraper, req: &models.ScrapeRequest{}, code: models.ErrCodeInternal},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := test.target.FinalizeSelected(test.req, test.result)
			var scrapeErr *models.ScrapeError
			if !errors.As(err, &scrapeErr) || scrapeErr.Code != test.code {
				t.Fatalf("FinalizeSelected() error = %#v, want %s", err, test.code)
			}
		})
	}
}

func TestFinalizeSelectedAppliesResultDefaults(t *testing.T) {
	scraper := &Scraper{}
	result, err := scraper.FinalizeSelected(
		&models.ScrapeRequest{URL: "https://requested.example/page"},
		&ScrapeResult{RawHTML: "<html><body>selected</body></html>", FetchMethod: "browser"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalURL != "https://requested.example/page" || result.EngineUsed != "browser" || result.FetchedAt.IsZero() {
		t.Fatalf("finalized result = %#v", result)
	}
}
