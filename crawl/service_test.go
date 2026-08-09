package crawl

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/use-agent/purify/jobs"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/scrape"
)

type runnerFunc func(context.Context, *models.ScrapeRequest, scrape.Observer) (*scrape.Result, error)

func (run runnerFunc) Run(ctx context.Context, request *models.ScrapeRequest, observe scrape.Observer) (*scrape.Result, error) {
	return run(ctx, request, observe)
}

func TestOneWorkerCrawlsMultipleBreadthFirstLevelsWithoutDeadlock(t *testing.T) {
	graph := map[string]models.LinksResult{
		"https://example.test/": {
			Internal: []models.Link{{Href: "/b"}, {Href: "/a"}},
		},
		"https://example.test/a": {
			Internal: []models.Link{{Href: "/a/deep"}},
		},
		"https://example.test/b": {
			External: []models.Link{{Href: "https://example.test/b/deep"}},
		},
	}
	runner := graphRunner(graph, nil)
	service, executor := newTestService(t, runner, 1, 16, Config{IDGenerator: fixedID("crawl-one-worker")})
	defer executor.Close()
	defer service.Close()

	accepted, err := service.Submit(models.CrawlRequest{
		URL:      "HTTPS://EXAMPLE.test:443#root",
		MaxDepth: 2,
		MaxPages: 10,
		Scope:    scopeDomain,
	})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	status := awaitTerminal(t, service, accepted.ID)
	assertResultURLs(t, status, []string{
		"https://example.test/",
		"https://example.test/a",
		"https://example.test/b",
		"https://example.test/a/deep",
		"https://example.test/b/deep",
	})
}

func TestCrawlResultOrderIsStableAcrossCompletionOrders(t *testing.T) {
	graph := map[string]models.LinksResult{
		"https://example.test/start": {
			Internal: []models.Link{{Href: "/c"}, {Href: "/a"}},
			External: []models.Link{{Href: "https://EXAMPLE.test:443/b#fragment"}},
		},
	}
	delays := map[string]time.Duration{
		"https://example.test/a": 7 * time.Millisecond,
		"https://example.test/b": 1 * time.Millisecond,
		"https://example.test/c": 4 * time.Millisecond,
	}
	service, executor := newTestService(t, graphRunner(graph, delays), 3, 16, Config{
		Capacity:    32,
		IDGenerator: sequentialIDs("crawl-stable"),
	})
	defer executor.Close()
	defer service.Close()

	want := []string{
		"https://example.test/start",
		"https://example.test/a",
		"https://example.test/b",
		"https://example.test/c",
	}
	for iteration := range 12 {
		accepted, err := service.Submit(models.CrawlRequest{
			URL:      "https://example.test/start",
			MaxDepth: 1,
			MaxPages: 10,
			Scope:    scopeDomain,
		})
		if err != nil {
			t.Fatalf("iteration %d: Submit() error = %v", iteration, err)
		}
		assertResultURLs(t, awaitTerminal(t, service, accepted.ID), want)
	}
}

func TestCrawlEnforcesMaxPagesAndInclusiveMaxDepth(t *testing.T) {
	graph := map[string]models.LinksResult{
		"https://example.test/root": {
			Internal: []models.Link{{Href: "/c"}, {Href: "/b"}, {Href: "/a"}},
		},
		"https://example.test/a": {Internal: []models.Link{{Href: "/a/deep"}}},
		"https://example.test/b": {Internal: []models.Link{{Href: "/b/deep"}}},
		"https://example.test/c": {Internal: []models.Link{{Href: "/c/deep"}}},
	}

	tests := []struct {
		name     string
		request  models.CrawlRequest
		wantURLs []string
	}{
		{
			name: "max pages reserves sorted candidate before scheduling",
			request: models.CrawlRequest{
				URL: "https://example.test/root", MaxDepth: 3, MaxPages: 2, Scope: scopeDomain,
			},
			wantURLs: []string{"https://example.test/root", "https://example.test/a"},
		},
		{
			name: "max depth page is scraped but not expanded",
			request: models.CrawlRequest{
				URL: "https://example.test/root", MaxDepth: 1, MaxPages: 20, Scope: scopeDomain,
			},
			wantURLs: []string{
				"https://example.test/root",
				"https://example.test/a",
				"https://example.test/b",
				"https://example.test/c",
			},
		},
		{
			name: "page scope only scrapes root",
			request: models.CrawlRequest{
				URL: "https://example.test/root", MaxDepth: 3, MaxPages: 20, Scope: scopePage,
			},
			wantURLs: []string{"https://example.test/root"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, executor := newTestService(t, graphRunner(graph, nil), 3, 32, Config{IDGenerator: fixedID("crawl-limits")})
			defer executor.Close()
			defer service.Close()

			accepted, err := service.Submit(test.request)
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			assertResultURLs(t, awaitTerminal(t, service, accepted.ID), test.wantURLs)
		})
	}
}

func TestCrawlNormalizesDeduplicatesQueriesAndFinalRedirects(t *testing.T) {
	var calledMu sync.Mutex
	called := make(map[string]int)
	runner := runnerFunc(func(_ context.Context, request *models.ScrapeRequest, _ scrape.Observer) (*scrape.Result, error) {
		calledMu.Lock()
		called[request.URL]++
		calledMu.Unlock()
		response := successResponse(request.URL)
		if request.URL == "https://example.test/start" {
			response.FinalURL = "https://EXAMPLE.test:443/landing#redirect-fragment"
			response.Links.Internal = []models.Link{
				{Href: "/landing#self"},
				{Href: "/child?q=2#first"},
				{Href: "https://example.test:443/child?q=1"},
				{Href: "/child?q=2#second"},
			}
			response.Links.External = []models.Link{{Href: "HTTPS://EXAMPLE.TEST/child?q=1#duplicate"}}
		}
		return &scrape.Result{Response: response}, nil
	})
	service, executor := newTestService(t, runner, 2, 8, Config{IDGenerator: fixedID("crawl-dedup")})
	defer executor.Close()
	defer service.Close()

	accepted, err := service.Submit(models.CrawlRequest{
		URL: "https://EXAMPLE.test:443/start#root", MaxDepth: 1, MaxPages: 20, Scope: scopeDomain,
	})
	if err != nil {
		t.Fatal(err)
	}
	status := awaitTerminal(t, service, accepted.ID)
	assertResultURLs(t, status, []string{
		"https://example.test/start",
		"https://example.test/child?q=1",
		"https://example.test/child?q=2",
	})
	calledMu.Lock()
	defer calledMu.Unlock()
	if called["https://example.test/landing"] != 0 {
		t.Fatalf("redirect destination was scheduled again: calls=%v", called)
	}
	for targetURL, count := range called {
		if count != 1 {
			t.Fatalf("%q calls = %d, want 1", targetURL, count)
		}
	}
}

func TestSubmitClonesAndPropagatesCompleteScrapeOptions(t *testing.T) {
	waitForNetwork := false
	onlyMainContent := false
	options := models.ScrapeOptions{
		WaitForNetworkIdle: &waitForNetwork,
		Timeout:            41,
		Stealth:            true,
		ProxyURL:           "https://proxy.example:8443",
		OutputFormat:       "markdown_citations",
		ExtractMode:        "pruning",
		CSSSelector:        "main.content",
		Headers:            map[string]string{"X-First": "one", "X-Second": "two"},
		Cookies: []models.Cookie{
			{Name: "first", Value: "one", Domain: "example.test", Path: "/"},
			{Name: "second", Value: "two", Domain: "example.test", Path: "/docs"},
		},
		Actions: []models.Action{
			{Type: "click", Selector: "#first"},
			{Type: "execute_js", Code: "() => document.title"},
		},
		IncludeTags:     []string{"main", "article"},
		ExcludeTags:     []string{"nav", ".ad"},
		OnlyMainContent: &onlyMainContent,
		RemoveOverlays:  true,
		BlockAds:        true,
		CDPURL:          "wss://browser.example/devtools/browser/id",
		MaxAge:          12_345,
	}
	wantOptions := models.CloneScrapeOptions(options)
	rootStarted := make(chan struct{})
	releaseRoot := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseRoot) }) })
	seen := make(chan models.ScrapeOptions, 2)
	runner := runnerFunc(func(ctx context.Context, request *models.ScrapeRequest, _ scrape.Observer) (*scrape.Result, error) {
		if request.URL == "https://example.test/root" {
			close(rootStarted)
			select {
			case <-releaseRoot:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		seen <- models.ScrapeOptionsFromRequest(request)
		response := successResponse(request.URL)
		if request.URL == "https://example.test/root" {
			response.Links.Internal = []models.Link{{Href: "/child"}}
		}
		return &scrape.Result{Response: response}, nil
	})
	service, executor := newTestService(t, runner, 1, 4, Config{IDGenerator: fixedID("crawl-options")})
	defer executor.Close()
	defer service.Close()

	request := models.CrawlRequest{
		URL: "https://example.test/root", MaxDepth: 1, MaxPages: 2, Scope: scopeDomain, Options: options,
	}
	accepted, err := service.Submit(request)
	if err != nil {
		t.Fatal(err)
	}
	receive(t, rootStarted, "root runner start")
	waitForNetwork = true
	onlyMainContent = true
	request.Options.Headers["X-First"] = "mutated"
	request.Options.Cookies[0].Value = "mutated"
	request.Options.Actions[0].Selector = "#mutated"
	request.Options.IncludeTags[0] = ".mutated"
	request.Options.ExcludeTags[0] = ".mutated"
	releaseOnce.Do(func() { close(releaseRoot) })

	status := awaitTerminal(t, service, accepted.ID)
	if status.Status != statusCompleted || status.Total != 2 {
		t.Fatalf("status = %#v", status)
	}
	for range 2 {
		got := receive(t, seen, "runner options")
		if !reflect.DeepEqual(got, wantOptions) {
			t.Fatalf("runner options = %#v, want %#v", got, wantOptions)
		}
	}
}

func TestNewServiceAndSubmitValidation(t *testing.T) {
	runner := runnerFunc(func(_ context.Context, request *models.ScrapeRequest, _ scrape.Observer) (*scrape.Result, error) {
		return &scrape.Result{Response: successResponse(request.URL)}, nil
	})
	executor, err := jobs.NewExecutor(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer executor.Close()

	tests := []struct {
		name     string
		runner   Runner
		executor *jobs.Executor
		config   Config
		want     error
	}{
		{name: "nil runner", executor: executor},
		{name: "nil executor", runner: runner},
		{name: "negative capacity", runner: runner, executor: executor, config: Config{Capacity: -1}, want: jobs.ErrInvalidManagerConfig},
		{name: "negative ttl", runner: runner, executor: executor, config: Config{TTL: -time.Second}, want: jobs.ErrInvalidManagerConfig},
		{name: "negative sweep", runner: runner, executor: executor, config: Config{SweepInterval: -time.Second}, want: jobs.ErrInvalidManagerConfig},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, err := NewService(test.runner, test.executor, test.config)
			if err == nil {
				service.Close()
				t.Fatal("NewService() error = nil")
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("NewService() error = %v, want %v", err, test.want)
			}
		})
	}

	service, err := NewService(runner, executor, Config{Capacity: 20, IDGenerator: sequentialIDs("crawl-validation")})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	requests := []struct {
		name    string
		request models.CrawlRequest
		want    error
	}{
		{name: "invalid URL", request: models.CrawlRequest{URL: "/relative"}, want: ErrInvalidURL},
		{name: "negative depth", request: models.CrawlRequest{URL: "https://example.test", MaxDepth: -1}, want: ErrInvalidMaxDepth},
		{name: "large depth", request: models.CrawlRequest{URL: "https://example.test", MaxDepth: 11}, want: ErrInvalidMaxDepth},
		{name: "negative pages", request: models.CrawlRequest{URL: "https://example.test", MaxPages: -1}, want: ErrInvalidMaxPages},
		{name: "large pages", request: models.CrawlRequest{URL: "https://example.test", MaxPages: 501}, want: ErrInvalidMaxPages},
		{name: "invalid scope", request: models.CrawlRequest{URL: "https://example.test", Scope: "global"}, want: ErrInvalidScope},
		{name: "invalid exclude", request: models.CrawlRequest{URL: "https://example.test", ExcludePatterns: []string{"["}}, want: ErrInvalidExcludePattern},
		{name: "too many excludes", request: models.CrawlRequest{URL: "https://example.test", ExcludePatterns: make([]string, maximumExcludeCount+1)}, want: ErrInvalidExcludePattern},
		{name: "exclude too long", request: models.CrawlRequest{URL: "https://example.test", ExcludePatterns: []string{strings.Repeat("x", maximumExcludeLength+1)}}, want: ErrInvalidExcludePattern},
	}
	for _, test := range requests {
		t.Run(test.name, func(t *testing.T) {
			response, err := service.Submit(test.request)
			if !errors.Is(err, test.want) || response != nil {
				t.Fatalf("Submit() = (%#v, %v), want (nil, %v)", response, err, test.want)
			}
		})
	}

	t.Run("id generator error", func(t *testing.T) {
		sentinel := errors.New("id source unavailable")
		broken, err := NewService(runner, executor, Config{IDGenerator: func() (string, error) { return "", sentinel }})
		if err != nil {
			t.Fatal(err)
		}
		defer broken.Close()
		if _, err := broken.Submit(models.CrawlRequest{URL: "https://example.test"}); !errors.Is(err, sentinel) {
			t.Fatalf("Submit() error = %v, want %v", err, sentinel)
		}
	})
	t.Run("empty id", func(t *testing.T) {
		broken, err := NewService(runner, executor, Config{IDGenerator: fixedID("  ")})
		if err != nil {
			t.Fatal(err)
		}
		defer broken.Close()
		if _, err := broken.Submit(models.CrawlRequest{URL: "https://example.test"}); !errors.Is(err, ErrEmptyJobID) {
			t.Fatalf("Submit() error = %v, want %v", err, ErrEmptyJobID)
		}
	})
}

func graphRunner(graph map[string]models.LinksResult, delays map[string]time.Duration) Runner {
	return runnerFunc(func(ctx context.Context, request *models.ScrapeRequest, _ scrape.Observer) (*scrape.Result, error) {
		if delay := delays[request.URL]; delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		response := successResponse(request.URL)
		response.Links = graph[request.URL]
		return &scrape.Result{Response: response}, nil
	})
}

func successResponse(targetURL string) *models.ScrapeResponse {
	return &models.ScrapeResponse{
		Success:    true,
		StatusCode: 200,
		FinalURL:   targetURL,
		Content:    targetURL,
		Metadata:   models.Metadata{Title: targetURL, SourceURL: targetURL},
	}
}

func newTestService(t *testing.T, runner Runner, workers, queue int, config Config) (*Service, *jobs.Executor) {
	t.Helper()
	executor, err := jobs.NewExecutor(workers, queue)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	service, err := NewService(runner, executor, config)
	if err != nil {
		executor.Close()
		t.Fatalf("NewService() error = %v", err)
	}
	return service, executor
}

func fixedID(id string) IDGenerator {
	return func() (string, error) { return id, nil }
}

func sequentialIDs(prefix string) IDGenerator {
	var sequence atomic.Int64
	return func() (string, error) {
		return fmt.Sprintf("%s-%d", prefix, sequence.Add(1)), nil
	}
}

func awaitTerminal(t *testing.T, service *Service, id string) *models.CrawlStatusResponse {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		status, ok := service.Get(id)
		if !ok {
			t.Fatalf("Get(%q) did not find accepted job", id)
		}
		if status.Status != statusProcessing {
			if status.Completed != status.Total || len(status.Results) != status.Total {
				t.Fatalf("terminal state is incomplete: %#v", status)
			}
			for index, response := range status.Results {
				if response == nil {
					t.Fatalf("terminal result[%d] is nil: %#v", index, status)
				}
			}
			return status
		}
		select {
		case <-deadline.C:
			t.Fatalf("job %q did not reach terminal state; last=%#v", id, status)
		case <-ticker.C:
		}
	}
}

func assertResultURLs(t *testing.T, status *models.CrawlStatusResponse, want []string) {
	t.Helper()
	if status.Status != statusCompleted || status.Completed != len(want) || status.Total != len(want) {
		t.Fatalf("status = %#v, want completed %d/%d", status, len(want), len(want))
	}
	got := make([]string, len(status.Results))
	for index, response := range status.Results {
		if response == nil || !response.Success {
			t.Fatalf("result[%d] = %#v", index, response)
		}
		got[index] = response.Content
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("result URLs = %v, want %v", got, want)
	}
}

func receive[T any](t *testing.T, channel <-chan T, description string) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(5 * time.Second):
		var zero T
		t.Fatalf("timed out waiting for %s", description)
		return zero
	}
}

func containsMessage(response *models.ScrapeResponse, fragment string) bool {
	return response != nil && response.Error != nil && strings.Contains(response.Error.Message, fragment)
}
