package scraper

import (
	"testing"

	"github.com/use-agent/purify/models"
)

// TestBrowserHeadersRefererIsGoogleOrigin locks the fix for the Chromium 151
// navigation failure: the auto-referer must be the bare Google origin, never the
// /search?q=... URL, which both leaked an unrealistic anti-bot tell and tripped
// Chromium into net::ERR_BLOCKED_BY_CLIENT on every browser navigation.
func TestBrowserHeadersRefererIsGoogleOrigin(t *testing.T) {
	headers := browserHeaders(&models.ScrapeRequest{URL: "https://example.com/page"})
	if got := headers["Referer"]; got != "https://www.google.com/" {
		t.Fatalf("auto Referer = %q, want the bare Google origin", got)
	}
}

func TestBrowserHeadersKeepsCallerRefererAndSkipsHTTP(t *testing.T) {
	custom := browserHeaders(&models.ScrapeRequest{
		URL:     "https://example.com/",
		Headers: map[string]string{"Referer": "https://caller.example/"},
	})
	if got := custom["Referer"]; got != "https://caller.example/" {
		t.Fatalf("caller Referer = %q, want it preserved", got)
	}

	plain := browserHeaders(&models.ScrapeRequest{URL: "http://example.com/"})
	if _, ok := plain["Referer"]; ok {
		t.Fatalf("plain http must not get an auto Referer, got %q", plain["Referer"])
	}
}
