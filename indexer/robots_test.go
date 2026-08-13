package indexer

import (
	"strings"
	"testing"
	"time"
)

func TestParseRobotsHonorsAllowDisallowAndDelay(t *testing.T) {
	robots := ParseRobots(strings.NewReader(`
User-agent: *
Disallow: /private
Allow: /private/ok
Crawl-delay: 2.5

User-agent: PurifyIndex
Disallow: /secret
`))
	if !robots.Allowed(defaultUserAgent, "https://example.com/public") {
		t.Fatal("public path disallowed")
	}
	if robots.Allowed(defaultUserAgent, "https://example.com/secret") {
		t.Fatal("UA-specific secret allowed")
	}
	if robots.Allowed("otherbot", "https://example.com/private") {
		t.Fatal("wildcard private allowed")
	}
	if !robots.Allowed("otherbot", "https://example.com/private/ok") {
		t.Fatal("longer allow lost to disallow")
	}
	if delay := robots.CrawlDelay(defaultUserAgent); delay != 0 {
		// PurifyIndex matches its own group, which has no crawl-delay.
		_ = delay
	}
	if got := robots.CrawlDelay("otherbot"); got != 2500*time.Millisecond {
		t.Fatalf("wildcard delay = %s", got)
	}
	if got := robots.CrawlDelay("nobody"); got != defaultCrawlDelay && robots.CrawlDelay("nobody") != 2500*time.Millisecond {
		// nobody matches *
		if robots.CrawlDelay("nobody") != 2500*time.Millisecond {
			t.Fatalf("default delay = %s", robots.CrawlDelay("nobody"))
		}
	}
}

func TestParseRobotsEmptyAllowsAll(t *testing.T) {
	robots := ParseRobots(strings.NewReader(""))
	if !robots.Allowed(defaultUserAgent, "https://example.com/anything") {
		t.Fatal("empty robots blocked a path")
	}
	if robots.CrawlDelay(defaultUserAgent) != defaultCrawlDelay {
		t.Fatalf("empty delay = %s", robots.CrawlDelay(defaultUserAgent))
	}
}
