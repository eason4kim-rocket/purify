package scraper

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"
	"github.com/use-agent/purify/config"
	"github.com/use-agent/purify/engine"
	"github.com/use-agent/purify/internal/testsite"
	"github.com/use-agent/purify/models"
)

type deadlineEngine struct{}

func (deadlineEngine) Name() string { return "deadline-fixture" }

func (deadlineEngine) Supports(*engine.FetchRequest) bool { return true }

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

func TestBrowserHonorsPerRequestProxyHeadersAndCookies(t *testing.T) {
	if os.Getenv("PURIFY_BROWSER_TEST") != "1" {
		t.Skip("set PURIFY_BROWSER_TEST=1 with PURIFY_BROWSER_BIN for the fixed Chromium regression")
	}
	browserBin := os.Getenv("PURIFY_BROWSER_BIN")
	if browserBin == "" {
		t.Fatal("PURIFY_BROWSER_BIN is required for deterministic browser tests")
	}

	var proxyHits atomic.Int32
	proxyServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Host != "purify-browser-proxy.invalid" {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		proxyHits.Add(1)
		cookie, err := request.Cookie("session")
		if err != nil {
			t.Errorf("proxied request cookie: %v", err)
		}
		writer.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(writer, "<html><body>browser-proxy-marker|%s|%s</body></html>", request.Header.Get("X-Purify-Test"), cookie.Value)
	}))
	t.Cleanup(proxyServer.Close)

	scraper, err := NewScraper(config.BrowserConfig{
		Headless:   true,
		MaxPages:   1,
		BrowserBin: browserBin,
		NoSandbox:  os.Getenv("CI") == "true",
	}, config.ScraperConfig{
		DefaultTimeout: 8 * time.Second,
		MaxTimeout:     8 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewScraper() error = %v", err)
	}
	t.Cleanup(scraper.Close)
	wait := false
	result, err := scraper.DoScrapeRod(context.Background(), &models.ScrapeRequest{
		URL:                "http://purify-browser-proxy.invalid/page",
		Timeout:            8,
		ProxyURL:           proxyServer.URL,
		Headers:            map[string]string{"X-Purify-Test": "header-value"},
		Cookies:            []models.Cookie{{Name: "session", Value: "cookie-value"}},
		WaitForNetworkIdle: &wait,
	})
	if err != nil {
		t.Fatalf("DoScrapeRod() error = %v", err)
	}
	if proxyHits.Load() == 0 {
		t.Fatal("request-scoped browser proxy did not receive the target request")
	}
	if !strings.Contains(result.RawHTML, "browser-proxy-marker|header-value|cookie-value") {
		t.Fatalf("proxied browser HTML = %q", result.RawHTML)
	}
	if active := scraper.Stats().ActivePages; active != 0 {
		t.Fatalf("active pages after isolated proxy request = %d, want 0", active)
	}
}

func TestIsolatedBrowserTimeoutDoesNotExtendDeadlineOrLeakContext(t *testing.T) {
	if os.Getenv("PURIFY_BROWSER_TEST") != "1" {
		t.Skip("set PURIFY_BROWSER_TEST=1 with PURIFY_BROWSER_BIN for the fixed Chromium regression")
	}
	browserBin := os.Getenv("PURIFY_BROWSER_BIN")
	if browserBin == "" {
		t.Fatal("PURIFY_BROWSER_BIN is required for deterministic browser tests")
	}

	proxyServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Host != "purify-timeout-proxy.invalid" {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		time.Sleep(2 * time.Second)
		writer.Header().Set("Content-Type", "text/html")
		_, _ = writer.Write([]byte("<html><body>too late</body></html>"))
	}))
	t.Cleanup(proxyServer.Close)

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
	baseline, err := (proto.TargetGetBrowserContexts{}).Call(scraper.browser)
	if err != nil {
		t.Fatalf("TargetGetBrowserContexts() baseline error = %v", err)
	}

	wait := false
	started := time.Now()
	_, err = scraper.DoScrapeRod(context.Background(), &models.ScrapeRequest{
		URL:                "http://purify-timeout-proxy.invalid/page",
		Timeout:            1,
		ProxyURL:           proxyServer.URL,
		WaitForNetworkIdle: &wait,
	})
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("DoScrapeRod() error = nil, want timeout")
	}
	if elapsed > 1350*time.Millisecond {
		t.Fatalf("isolated proxy request elapsed = %v, want one 1s request budget without cleanup extension", elapsed)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		contexts, contextsErr := (proto.TargetGetBrowserContexts{}).Call(scraper.browser)
		if contextsErr == nil && len(contexts.BrowserContextIDs) == len(baseline.BrowserContextIDs) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("isolated browser context was not disposed; contexts=%v error=%v", contexts, contextsErr)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestBrowserContextCleanupIsIdempotentWithAuthenticatedRelay(t *testing.T) {
	if os.Getenv("PURIFY_BROWSER_TEST") != "1" {
		t.Skip("set PURIFY_BROWSER_TEST=1 with PURIFY_BROWSER_BIN for the fixed Chromium regression")
	}
	browserBin := os.Getenv("PURIFY_BROWSER_BIN")
	if browserBin == "" {
		t.Fatal("PURIFY_BROWSER_BIN is required for deterministic browser tests")
	}
	scraper, err := NewScraper(config.BrowserConfig{
		Headless:   true,
		MaxPages:   1,
		BrowserBin: browserBin,
		NoSandbox:  os.Getenv("CI") == "true",
	}, config.ScraperConfig{DefaultTimeout: 5 * time.Second, MaxTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("NewScraper() error = %v", err)
	}
	t.Cleanup(scraper.Close)

	_, cleanup, err := newIsolatedBrowserContext(context.Background(), scraper.browser, "http://user:pass@127.0.0.1:9")
	if err != nil {
		t.Fatalf("newIsolatedBrowserContext() error = %v", err)
	}
	cleanup()
	cleanup() // must not close the relay channel twice or panic
}
