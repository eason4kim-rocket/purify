package scraper

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/use-agent/purify/config"
	"github.com/use-agent/purify/engine"
	"github.com/use-agent/purify/internal/testsite"
	"github.com/use-agent/purify/models"
)

type deadlineEngine struct{}

func (deadlineEngine) Name() string { return "deadline-fixture" }

func (deadlineEngine) Fetch(ctx context.Context, _ *engine.FetchRequest) (*engine.FetchResult, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestBrowserWaitsForCompleteDynamicDocuments(t *testing.T) {
	if os.Getenv("PURIFY_BROWSER_TEST") != "1" {
		t.Skip("set PURIFY_BROWSER_TEST=1 with PURIFY_BROWSER_BIN for the fixed Chromium regression")
	}
	browserBin := os.Getenv("PURIFY_BROWSER_BIN")
	if browserBin == "" {
		t.Fatal("PURIFY_BROWSER_BIN is required for deterministic browser tests")
	}

	site := testsite.New()
	t.Cleanup(site.Close)
	scraper, err := NewScraper(config.BrowserConfig{
		Headless:   true,
		MaxPages:   1,
		BrowserBin: browserBin,
		NoSandbox:  os.Getenv("CI") == "true",
	}, config.ScraperConfig{
		DefaultTimeout:    5 * time.Second,
		MaxTimeout:        10 * time.Second,
		NavigationTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewScraper() error = %v", err)
	}
	t.Cleanup(scraper.Close)
	waitForNetworkIdle := true

	for _, test := range []struct {
		name   string
		path   string
		marker string
	}{
		{name: "streamed delayed body", path: "/delayed-body", marker: testsite.DelayedBodyMarker},
		{name: "delayed SPA content", path: "/spa", marker: testsite.SPAMarker},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := scraper.DoScrapeRod(context.Background(), &models.ScrapeRequest{
				URL:                site.URL(test.path),
				Timeout:            5,
				WaitForNetworkIdle: &waitForNetworkIdle,
			})
			if err != nil {
				t.Fatalf("DoScrapeRod() error = %v", err)
			}
			if !strings.Contains(result.RawHTML, test.marker) {
				t.Fatalf("rendered HTML missing %q: %s", test.marker, result.RawHTML)
			}
		})
	}
}

func TestDispatcherFallbackSharesRequestDeadline(t *testing.T) {
	if os.Getenv("PURIFY_BROWSER_TEST") != "1" {
		t.Skip("set PURIFY_BROWSER_TEST=1 with PURIFY_BROWSER_BIN for the fixed Chromium regression")
	}
	browserBin := os.Getenv("PURIFY_BROWSER_BIN")
	if browserBin == "" {
		t.Fatal("PURIFY_BROWSER_BIN is required for deterministic browser tests")
	}

	site := testsite.New()
	t.Cleanup(site.Close)
	scraper, err := NewScraper(config.BrowserConfig{
		Headless:   true,
		MaxPages:   1,
		BrowserBin: browserBin,
		NoSandbox:  os.Getenv("CI") == "true",
	}, config.ScraperConfig{
		DefaultTimeout: time.Second,
		MaxTimeout:     time.Second,
	})
	if err != nil {
		t.Fatalf("NewScraper() error = %v", err)
	}
	t.Cleanup(scraper.Close)
	scraper.SetDispatcher(engine.NewDispatcher(
		[]engine.Engine{deadlineEngine{}},
		[]time.Duration{0},
		engine.NewDomainMemory(time.Minute),
	))

	started := time.Now()
	_, err = scraper.DoScrape(context.Background(), &models.ScrapeRequest{
		URL:     site.URL("/timeout?delay_ms=2000"),
		Timeout: 1,
	})
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("DoScrape() error = nil, want request deadline failure")
	}
	if elapsed > 1600*time.Millisecond {
		t.Fatalf("DoScrape() elapsed = %v, want one 1s request budget", elapsed)
	}
}

func TestBrowserRetiresUsedPageAndReplenishesPool(t *testing.T) {
	if os.Getenv("PURIFY_BROWSER_TEST") != "1" {
		t.Skip("set PURIFY_BROWSER_TEST=1 with PURIFY_BROWSER_BIN for the fixed Chromium regression")
	}
	browserBin := os.Getenv("PURIFY_BROWSER_BIN")
	if browserBin == "" {
		t.Fatal("PURIFY_BROWSER_BIN is required for deterministic browser tests")
	}

	site := testsite.New()
	t.Cleanup(site.Close)
	scraper, err := NewScraper(config.BrowserConfig{
		Headless:   true,
		MaxPages:   1,
		BrowserBin: browserBin,
		NoSandbox:  os.Getenv("CI") == "true",
	}, config.ScraperConfig{
		DefaultTimeout: 5 * time.Second,
		MaxTimeout:     5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewScraper() error = %v", err)
	}
	t.Cleanup(scraper.Close)
	scraper.pagePolicy = pageRetirementPolicy{
		maxUses:                1,
		maxConsecutiveFailures: 3,
		maxAge:                 time.Hour,
	}

	for attempt := 0; attempt < 2; attempt++ {
		result, scrapeErr := scraper.DoScrapeRod(context.Background(), &models.ScrapeRequest{
			URL:     site.URL("/static"),
			Timeout: 5,
		})
		if scrapeErr != nil {
			t.Fatalf("attempt %d DoScrapeRod() error = %v", attempt+1, scrapeErr)
		}
		if !strings.Contains(result.RawHTML, testsite.StaticMarker) {
			t.Fatalf("attempt %d HTML missing fixture marker", attempt+1)
		}
		if active := scraper.Stats().ActivePages; active != 0 {
			t.Fatalf("attempt %d active pages = %d, want 0", attempt+1, active)
		}
	}
	if retired := scraper.retiredPages.Load(); retired != 2 {
		t.Fatalf("retired pages = %d, want 2", retired)
	}
}
