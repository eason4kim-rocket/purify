package scraper

import (
	"testing"
	"time"

	"github.com/use-agent/purify/config"
	"github.com/use-agent/purify/models"
)

func TestScrapeTimeoutDefaultsAndCapsOneBudget(t *testing.T) {
	scraper := &Scraper{scraperCfg: config.ScraperConfig{
		DefaultTimeout: 15 * time.Second,
		MaxTimeout:     45 * time.Second,
	}}
	for _, test := range []struct {
		name    string
		request *models.ScrapeRequest
		want    time.Duration
	}{
		{name: "nil request", request: nil, want: 15 * time.Second},
		{name: "default", request: &models.ScrapeRequest{}, want: 15 * time.Second},
		{name: "request value", request: &models.ScrapeRequest{Timeout: 20}, want: 20 * time.Second},
		{name: "maximum", request: &models.ScrapeRequest{Timeout: 120}, want: 45 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := scraper.scrapeTimeout(test.request); got != test.want {
				t.Fatalf("scrapeTimeout() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestScrapeTimeoutHasSafeFallback(t *testing.T) {
	scraper := &Scraper{}
	if got := scraper.scrapeTimeout(&models.ScrapeRequest{}); got != 30*time.Second {
		t.Fatalf("scrapeTimeout() = %v, want 30s", got)
	}
}

func TestNetworkIdleRequested(t *testing.T) {
	yes := true
	no := false
	for _, test := range []struct {
		request *models.ScrapeRequest
		want    bool
	}{
		{request: nil, want: false},
		{request: &models.ScrapeRequest{}, want: false},
		{request: &models.ScrapeRequest{WaitForNetworkIdle: &no}, want: false},
		{request: &models.ScrapeRequest{WaitForNetworkIdle: &yes}, want: true},
	} {
		if got := networkIdleRequested(test.request); got != test.want {
			t.Fatalf("networkIdleRequested(%#v) = %v, want %v", test.request, got, test.want)
		}
	}
}
