package indexer

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/use-agent/purify/searchindex"
)

// TestRunFetchesDistinctHostsConcurrently locks the worker fan-out. The loop
// used to be sequential with Workers ignored, so one host's crawl-delay stalled
// the whole crawl and throughput collapsed to one page per delay.
func TestRunFetchesDistinctHostsConcurrently(t *testing.T) {
	var mu sync.Mutex
	inFlight, peak := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/robots.txt" {
			_, _ = io.WriteString(writer, "User-agent: *\nCrawl-delay: 5\n")
			return
		}
		mu.Lock()
		inFlight++
		peak = max(peak, inFlight)
		mu.Unlock()
		time.Sleep(150 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		_, _ = io.WriteString(writer, "<html><title>Page</title><body>"+strings.Repeat("indexable words ", 40)+"</body></html>")
	}))
	t.Cleanup(server.Close)

	store, err := searchindex.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// All URLs point at one test server, but each carries its own registrable
	// domain so the frontier hands them to different workers. A five-second
	// crawl-delay makes any cross-host serialization impossible to miss.
	items := make([]searchindex.FrontierItem, 0, 4)
	for index := range 4 {
		items = append(items, searchindex.FrontierItem{
			URL:  fmt.Sprintf("%s/page-%d", server.URL, index),
			Root: fmt.Sprintf("host-%d.example", index),
		})
	}
	if _, err := store.Enqueue(context.Background(), items); err != nil {
		t.Fatal(err)
	}

	fetcher, err := NewFetcher(FetcherConfig{AllowPrivateNetworks: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stats, err := Run(ctx, store, fetcher, stubDiscoverer{}, nil, RunConfig{
		Workers: 4, BatchSize: 4, MaxPages: 10, AllowPrivate: true,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if stats.Indexed != 4 {
		t.Fatalf("indexed = %#v", stats)
	}
	mu.Lock()
	observed := peak
	mu.Unlock()
	if observed < 2 {
		t.Fatalf("peak concurrent fetches = %d, want the workers to overlap across hosts", observed)
	}
}

// TestRunGrowsFrontierFromFetchedPages locks in-crawl link discovery: startup
// discovery used to be the only frontier source, so a site without a sitemap
// was frozen at its front page's one-hop neighborhood no matter how large the
// crawl budget was.
func TestRunGrowsFrontierFromFetchedPages(t *testing.T) {
	filler := strings.Repeat("indexable words ", 40)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/":
			_, _ = io.WriteString(writer, `<html><title>root</title><body>`+filler+
				`<a href="/a">a</a><a href="/b">b</a>`+
				`<a href="/logo.png">asset</a><a href="/search?q=x">query</a>`+
				`<a href="https://other.example/x">offsite</a></body></html>`)
		case "/a":
			_, _ = io.WriteString(writer, `<html><title>a</title><body>`+filler+`<a href="/c">c</a></body></html>`)
		default:
			_, _ = io.WriteString(writer, `<html><title>leaf</title><body>`+filler+`</body></html>`)
		}
	}))
	t.Cleanup(server.Close)

	store, err := searchindex.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	fetcher, err := NewFetcher(FetcherConfig{AllowPrivateNetworks: true})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := Run(context.Background(), store, fetcher, stubDiscoverer{}, []string{server.URL + "/"}, RunConfig{
		Workers: 2, BatchSize: 2, MaxPages: 10, AllowPrivate: true,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	// Seed plus the growth chain: / -> {a, b}, a -> c. The asset, query, and
	// offsite links must not be fetched or enqueued.
	if stats.Indexed != 4 || stats.Discovered != 4 {
		t.Fatalf("stats = %#v, want 4 indexed and 4 discovered", stats)
	}
	if active, err := store.ActiveFrontier(context.Background()); err != nil || active != 0 {
		t.Fatalf("ActiveFrontier() after run = %d, %v, want drained", active, err)
	}
	hits, err := store.Query(context.Background(), "indexable words", 10)
	if err != nil || len(hits) != 4 {
		t.Fatalf("Query() = %d hits, %v, want the whole chain indexed", len(hits), err)
	}
}

// TestRunRefetchesSeedsOnDrainedFrontier locks the discovery bootstrap: after
// earlier runs close every row, a crawl used to end instantly with nothing to
// lease, so link discovery never saw a page. Seeds must reopen each run.
func TestRunRefetchesSeedsOnDrainedFrontier(t *testing.T) {
	filler := strings.Repeat("indexable words ", 40)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/" {
			_, _ = io.WriteString(writer, `<html><title>root</title><body>`+filler+`<a href="/n1">n</a></body></html>`)
			return
		}
		_, _ = io.WriteString(writer, `<html><title>leaf</title><body>`+filler+`</body></html>`)
	}))
	t.Cleanup(server.Close)

	store, err := searchindex.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// An earlier run left the seed row closed.
	seedURL := server.URL + "/"
	if _, err := store.Enqueue(context.Background(), []searchindex.FrontierItem{{URL: seedURL, Root: "127.0.0.1"}}); err != nil {
		t.Fatal(err)
	}
	leased, ok, err := store.Lease(context.Background(), time.Now())
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	if err := store.Complete(context.Background(), leased.URL); err != nil {
		t.Fatal(err)
	}

	fetcher, err := NewFetcher(FetcherConfig{AllowPrivateNetworks: true})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := Run(context.Background(), store, fetcher, stubDiscoverer{}, []string{seedURL}, RunConfig{
		Workers: 1, BatchSize: 2, MaxPages: 10, AllowPrivate: true,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if stats.Indexed != 2 {
		t.Fatalf("stats = %#v, want the reopened seed and its discovered link indexed", stats)
	}
}

// TestRunStopsGrowingAtTheFrontierBudget locks the guardrail end to end: once
// a root's frontier rows reach MaxFrontierPerHost, fetched pages stop adding
// links for it.
func TestRunStopsGrowingAtTheFrontierBudget(t *testing.T) {
	filler := strings.Repeat("indexable words ", 40)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/" {
			_, _ = io.WriteString(writer, `<html><title>root</title><body>`+filler+
				`<a href="/a">a</a><a href="/b">b</a></body></html>`)
			return
		}
		_, _ = io.WriteString(writer, `<html><title>leaf</title><body>`+filler+`<a href="/c">c</a></body></html>`)
	}))
	t.Cleanup(server.Close)

	store, err := searchindex.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	fetcher, err := NewFetcher(FetcherConfig{AllowPrivateNetworks: true})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := Run(context.Background(), store, fetcher, stubDiscoverer{}, []string{server.URL + "/"}, RunConfig{
		Workers: 1, BatchSize: 2, MaxPages: 10, MaxFrontierPerHost: 2, AllowPrivate: true,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	// The seed spends one budget slot, one link fits the second slot, and the
	// crawl ends there instead of following the whole chain.
	if stats.Indexed != 2 {
		t.Fatalf("stats = %#v, want exactly 2 indexed pages", stats)
	}
}

func TestRunIndexesDiscoveredPagesAndHonorsRobots(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/robots.txt":
			_, _ = io.WriteString(writer, "User-agent: *\nDisallow: /hidden\n")
		case "/hidden":
			_, _ = io.WriteString(writer, "<html><title>secret</title><body>should not index</body></html>")
		default:
			_, _ = io.WriteString(writer, "<html><title>Hello</title><body>"+strings.Repeat("visible content ", 40)+"</body></html>")
		}
	}))
	t.Cleanup(server.Close)

	store, err := searchindex.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	fetcher, err := NewFetcher(FetcherConfig{AllowPrivateNetworks: true})
	if err != nil {
		t.Fatal(err)
	}
	discoverer := stubDiscoverer{urls: []string{server.URL + "/", server.URL + "/hidden"}}
	stats, err := Run(context.Background(), store, fetcher, discoverer, []string{server.URL + "/"}, RunConfig{
		Workers: 1, BatchSize: 2, MaxPages: 10, AllowPrivate: true,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if stats.Indexed < 1 {
		t.Fatalf("indexed = %#v", stats)
	}
	if stats.RobotsDeny < 1 {
		t.Fatalf("robots deny = %#v", stats)
	}
	hits, err := store.Query(context.Background(), "visible content", 5)
	if err != nil || len(hits) == 0 {
		t.Fatalf("Query() = %#v, %v", hits, err)
	}
	hidden, err := store.Query(context.Background(), "should not index", 5)
	if err != nil {
		t.Fatalf("hidden Query() error = %v", err)
	}
	for _, hit := range hidden {
		if strings.Contains(hit.URL, "/hidden") {
			t.Fatalf("robots-denied URL indexed: %#v", hit)
		}
	}
}
