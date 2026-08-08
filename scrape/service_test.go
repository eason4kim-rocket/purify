package scrape

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/use-agent/purify/cleaner"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/scraper"
)

type fakeFetcher struct {
	name      string
	supported bool
	result    *scraper.ScrapeResult
	err       error
	block     bool
	calls     int
	seen      *models.ScrapeRequest
}

func (fetcher *fakeFetcher) Name() string { return fetcher.name }

func (fetcher *fakeFetcher) Supports(*models.ScrapeRequest) bool { return fetcher.supported }

func (fetcher *fakeFetcher) Fetch(ctx context.Context, request *models.ScrapeRequest) (*scraper.ScrapeResult, error) {
	fetcher.calls++
	fetcher.seen = cloneRequest(request)
	if fetcher.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if fetcher.result == nil {
		return nil, fetcher.err
	}
	copy := *fetcher.result
	return &copy, fetcher.err
}

type fakeCleaner struct {
	responses map[string]*models.ScrapeResponse
	calls     []cleanCall
	err       error
}

type cleanCall struct {
	rawHTML   string
	sourceURL string
	format    string
	mode      string
	options   cleaner.CleanOptions
}

func (contentCleaner *fakeCleaner) Clean(rawHTML, sourceURL, format, extractMode string, opts ...cleaner.CleanOptions) (*models.ScrapeResponse, error) {
	var options cleaner.CleanOptions
	if len(opts) > 0 {
		options = opts[0]
	}
	contentCleaner.calls = append(contentCleaner.calls, cleanCall{
		rawHTML: rawHTML, sourceURL: sourceURL, format: format, mode: extractMode, options: options,
	})
	if contentCleaner.err != nil {
		return nil, contentCleaner.err
	}
	response := contentCleaner.responses[rawHTML]
	if response == nil {
		response = &models.ScrapeResponse{Content: strings.Repeat("useful content ", 20)}
	}
	copy := *response
	if response.Quality != nil {
		qualityCopy := *response.Quality
		copy.Quality = &qualityCopy
	}
	return &copy, nil
}

type fakeCache struct {
	mu       sync.Mutex
	value    *models.ScrapeResponse
	getKey   string
	setKey   string
	setValue *models.ScrapeResponse
}

func (responseCache *fakeCache) Get(key string, _ int) (*models.ScrapeResponse, bool) {
	responseCache.mu.Lock()
	defer responseCache.mu.Unlock()
	responseCache.getKey = key
	if responseCache.value == nil {
		return nil, false
	}
	copy := *responseCache.value
	return &copy, true
}

func (responseCache *fakeCache) Set(key string, value *models.ScrapeResponse) {
	responseCache.mu.Lock()
	defer responseCache.mu.Unlock()
	responseCache.setKey = key
	copy := *value
	responseCache.setValue = &copy
}

type fakeFinalizer struct {
	calls     int
	source    *scraper.ScrapeResult
	err       error
	returnNil bool
}

func (finalizer *fakeFinalizer) FinalizeSelected(_ *models.ScrapeRequest, source *scraper.ScrapeResult) (*scraper.ScrapeResult, error) {
	finalizer.calls++
	copy := *source
	finalizer.source = &copy
	if finalizer.returnNil {
		return nil, finalizer.err
	}
	return &copy, finalizer.err
}

func TestServiceEscalatesInOrderAndFinalizesOnlySelectedCandidate(t *testing.T) {
	httpFetcher := &fakeFetcher{name: "http", supported: true, result: &scraper.ScrapeResult{
		RawHTML: `<html><head><title>head only</title></head></html>`,
	}}
	rodFetcher := &fakeFetcher{name: "rod", supported: true, result: &scraper.ScrapeResult{
		RawHTML: `<html><body><main id="cf-chl-widget">Checking your browser</main></body></html>`,
	}}
	stealthFetcher := &fakeFetcher{name: "rod-stealth", supported: true, result: &scraper.ScrapeResult{
		RawHTML:     `<html><body><article>` + strings.Repeat("verified article content ", 20) + `</article></body></html>`,
		Title:       "Browser title",
		StatusCode:  200,
		FinalURL:    "https://final.example/article",
		FetchMethod: "browser",
	}}
	contentCleaner := &fakeCleaner{responses: map[string]*models.ScrapeResponse{
		stealthFetcher.result.RawHTML: {Content: strings.Repeat("verified article content ", 20)},
	}}
	finalizer := &fakeFinalizer{}
	service := mustService(t, []Fetcher{httpFetcher, rodFetcher, stealthFetcher}, contentCleaner, nil, finalizer)

	var events []Event
	result, err := service.Run(context.Background(), &models.ScrapeRequest{
		URL:         "https://requested.example/start",
		IncludeTags: []string{"article"},
	}, func(event Event) { events = append(events, event) })
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if httpFetcher.calls != 1 || rodFetcher.calls != 1 || stealthFetcher.calls != 1 {
		t.Fatalf("fetch calls = http:%d rod:%d stealth:%d", httpFetcher.calls, rodFetcher.calls, stealthFetcher.calls)
	}
	if len(contentCleaner.calls) != 1 {
		t.Fatalf("clean calls = %d, want only complete candidate cleaned", len(contentCleaner.calls))
	}
	if contentCleaner.calls[0].sourceURL != stealthFetcher.result.FinalURL {
		t.Fatalf("clean source URL = %q, want final URL", contentCleaner.calls[0].sourceURL)
	}
	if !reflect.DeepEqual(contentCleaner.calls[0].options.IncludeTags, []string{"article"}) {
		t.Fatalf("clean options = %#v", contentCleaner.calls[0].options)
	}
	if finalizer.calls != 1 || finalizer.source.RawHTML != stealthFetcher.result.RawHTML {
		t.Fatalf("finalizer calls/source = %d/%#v", finalizer.calls, finalizer.source)
	}
	if result.Source == nil || result.Response == nil || result.CacheHit {
		t.Fatalf("result = %#v", result)
	}
	response := result.Response
	if response.FinalURL != stealthFetcher.result.FinalURL || response.Metadata.SourceURL != stealthFetcher.result.FinalURL ||
		response.Metadata.Title != "Browser title" || response.Metadata.FetchMethod != "browser" ||
		response.EngineUsed != "rod-stealth" || !response.Success {
		t.Fatalf("assembled response = %#v", response)
	}
	if response.Quality == nil || response.Quality.Status != models.QualityStatusGood || len(response.Quality.FetchAttempts) != 3 {
		t.Fatalf("quality = %#v", response.Quality)
	}
	if got := []models.FetchAttemptOutcome{
		response.Quality.FetchAttempts[0].Outcome,
		response.Quality.FetchAttempts[1].Outcome,
		response.Quality.FetchAttempts[2].Outcome,
	}; !reflect.DeepEqual(got, []models.FetchAttemptOutcome{models.FetchAttemptRejected, models.FetchAttemptRejected, models.FetchAttemptSelected}) {
		t.Fatalf("attempt outcomes = %v", got)
	}
	if response.Quality.FetchAttempts[0].Reason != models.QualityReasonMissingBody ||
		response.Quality.FetchAttempts[1].Reason != models.QualityReasonChallengePage {
		t.Fatalf("attempt reasons = %#v", response.Quality.FetchAttempts)
	}
	assertEventTypes(t, events, []EventType{EventStarted, EventAttempt, EventAttempt, EventAttempt, EventNavigated, EventCompleted})
}

func TestServiceAcceptsDegradedCandidateWithoutStartingHeavierFetcher(t *testing.T) {
	first := &fakeFetcher{name: "http", supported: true, result: &scraper.ScrapeResult{
		RawHTML: `<html><body><p>short but meaningful response text for a useful page</p></body></html>`,
	}}
	second := &fakeFetcher{name: "rod", supported: true, result: &scraper.ScrapeResult{RawHTML: `<html><body>unused</body></html>`}}
	contentCleaner := &fakeCleaner{responses: map[string]*models.ScrapeResponse{
		first.result.RawHTML: {Content: "short but meaningful response text for a useful page"},
	}}
	service := mustService(t, []Fetcher{first, second}, contentCleaner, nil, nil)
	result, err := service.Run(context.Background(), &models.ScrapeRequest{URL: "https://example.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.calls != 0 {
		t.Fatalf("heavier fetcher calls = %d, want 0", second.calls)
	}
	if result.Response.Quality == nil || result.Response.Quality.Status != models.QualityStatusDegraded {
		t.Fatalf("quality = %#v, want degraded", result.Response.Quality)
	}
}

func TestServiceReturnsContentUnusableAfterAllCandidatesRejected(t *testing.T) {
	fetchers := []Fetcher{
		&fakeFetcher{name: "http", supported: true, result: &scraper.ScrapeResult{RawHTML: `<html><body>Loading</body></html>`}},
		&fakeFetcher{name: "rod", supported: true, result: &scraper.ScrapeResult{RawHTML: `<html><body><div id="cf-chl-widget">Checking your browser</div></body></html>`}},
	}
	service := mustService(t, fetchers, &fakeCleaner{}, nil, &fakeFinalizer{})
	var events []Event
	_, err := service.Run(context.Background(), &models.ScrapeRequest{URL: "https://example.com"}, func(event Event) { events = append(events, event) })
	var scrapeErr *models.ScrapeError
	if !errors.As(err, &scrapeErr) || scrapeErr.Code != models.ErrCodeContentUnusable {
		t.Fatalf("Run() error = %#v, want CONTENT_UNUSABLE", err)
	}
	assertEventTypes(t, events, []EventType{EventStarted, EventAttempt, EventAttempt, EventError})
	if events[len(events)-1].Response == nil || events[len(events)-1].Response.Error.Code != models.ErrCodeContentUnusable {
		t.Fatalf("error event = %#v", events[len(events)-1])
	}
}

func TestServiceCacheHitSkipsFetchAndSource(t *testing.T) {
	responseCache := &fakeCache{value: &models.ScrapeResponse{
		Success: true, Content: "cached", CacheStatus: "miss", Timing: models.TimingInfo{NavigationMs: 99, CleaningMs: 88},
	}}
	fetcher := &fakeFetcher{name: "http", supported: true, result: &scraper.ScrapeResult{RawHTML: `<html><body>unused</body></html>`}}
	service := mustService(t, []Fetcher{fetcher}, &fakeCleaner{}, responseCache, nil)
	result, err := service.Run(context.Background(), &models.ScrapeRequest{URL: "https://example.com", MaxAge: 1_000}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fetcher.calls != 0 || result.Source != nil || !result.CacheHit {
		t.Fatalf("cache result/fetch calls = %#v/%d", result, fetcher.calls)
	}
	if result.Response.CacheStatus != "hit" || result.Response.Timing.NavigationMs != 0 || result.Response.Timing.CleaningMs != 0 {
		t.Fatalf("cached response status/timing = %#v", result.Response)
	}
	if responseCache.value.CacheStatus != "miss" || responseCache.value.Timing.NavigationMs != 99 {
		t.Fatal("service mutated cache-owned value")
	}
}

func TestServiceCacheMissStoresCompletedResponse(t *testing.T) {
	responseCache := &fakeCache{}
	fetcher := &fakeFetcher{name: "http", supported: true, result: &scraper.ScrapeResult{
		RawHTML: `<html><body>` + strings.Repeat("cacheable content ", 20) + `</body></html>`,
	}}
	service := mustService(t, []Fetcher{fetcher}, &fakeCleaner{}, responseCache, nil)
	result, err := service.Run(context.Background(), &models.ScrapeRequest{URL: "https://example.com", MaxAge: 1_000}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if responseCache.getKey == "" || responseCache.setKey != responseCache.getKey || responseCache.setValue == nil {
		t.Fatalf("cache keys/value = %q/%q/%#v", responseCache.getKey, responseCache.setKey, responseCache.setValue)
	}
	if result.Response.CacheStatus != "miss" || responseCache.setValue.Quality == nil {
		t.Fatalf("stored response = %#v", responseCache.setValue)
	}
}

func TestServiceUsesOneParentDeadline(t *testing.T) {
	blocking := &fakeFetcher{name: "http", supported: true, block: true}
	heavier := &fakeFetcher{name: "rod", supported: true, result: &scraper.ScrapeResult{
		RawHTML: `<html><body>` + strings.Repeat("late content ", 20) + `</body></html>`,
	}}
	service := mustService(t, []Fetcher{blocking, heavier}, &fakeCleaner{}, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := service.Run(ctx, &models.ScrapeRequest{URL: "https://example.com", Timeout: 10}, nil)
	if time.Since(started) > 300*time.Millisecond {
		t.Fatalf("Run exceeded parent deadline: %v", time.Since(started))
	}
	var scrapeErr *models.ScrapeError
	if !errors.As(err, &scrapeErr) || scrapeErr.Code != models.ErrCodeTimeout {
		t.Fatalf("Run() error = %#v, want timeout", err)
	}
	if heavier.calls != 0 {
		t.Fatalf("heavier fetcher calls = %d after deadline", heavier.calls)
	}
}

func TestServiceRejectsNilFinalizedSource(t *testing.T) {
	fetcher := &fakeFetcher{name: "http", supported: true, result: &scraper.ScrapeResult{
		RawHTML: `<html><body>` + strings.Repeat("finalizer content ", 20) + `</body></html>`,
	}}
	service := mustService(t, []Fetcher{fetcher}, &fakeCleaner{}, nil, &fakeFinalizer{returnNil: true})
	_, err := service.Run(context.Background(), &models.ScrapeRequest{URL: "https://example.com"}, nil)
	var scrapeErr *models.ScrapeError
	if !errors.As(err, &scrapeErr) || scrapeErr.Code != models.ErrCodeInternal {
		t.Fatalf("Run() error = %#v, want INTERNAL_ERROR", err)
	}
}

func TestServiceDefaultsClonedRequestAndSkipsUnsupportedFetcher(t *testing.T) {
	unsupported := &fakeFetcher{name: "http", supported: false}
	supported := &fakeFetcher{name: "rod", supported: true, result: &scraper.ScrapeResult{
		RawHTML: `<html><body>` + strings.Repeat("default content ", 20) + `</body></html>`,
	}}
	service := mustService(t, []Fetcher{unsupported, supported}, &fakeCleaner{}, nil, nil)
	original := &models.ScrapeRequest{URL: "https://example.com", Headers: map[string]string{"X-Test": "one"}}
	_, err := service.Run(context.Background(), original, nil)
	if err != nil {
		t.Fatal(err)
	}
	if unsupported.calls != 0 || supported.seen == nil || supported.seen.Timeout != 30 || supported.seen.OutputFormat != "markdown" || supported.seen.ExtractMode != "readability" {
		t.Fatalf("fetchers/defaults = unsupported:%d seen:%#v", unsupported.calls, supported.seen)
	}
	supported.seen.Headers["X-Test"] = "mutated"
	if original.Headers["X-Test"] != "one" || original.Timeout != 0 || original.WaitForNetworkIdle != nil {
		t.Fatalf("service mutated caller request: %#v", original)
	}
}

func TestServiceValidationAndConstruction(t *testing.T) {
	if _, err := NewService(nil, &fakeCleaner{}, nil, nil, Config{}); err == nil {
		t.Fatal("NewService accepted no fetchers")
	}
	if _, err := NewService([]Fetcher{&fakeFetcher{name: "http"}}, nil, nil, nil, Config{}); err == nil {
		t.Fatal("NewService accepted nil cleaner")
	}
	service := mustService(t, []Fetcher{&fakeFetcher{name: "http", supported: true}}, &fakeCleaner{}, nil, nil)
	for _, request := range []*models.ScrapeRequest{
		nil,
		{URL: "file:///tmp/page"},
		{URL: "https://example.com", OutputFormat: "pdf"},
		{URL: "https://example.com", ExtractMode: "magic"},
		{URL: "https://example.com", Actions: make([]models.Action, 51)},
		{URL: "https://example.com", Actions: []models.Action{{Type: "unknown"}}},
		{URL: "https://example.com", Cookies: []models.Cookie{{Name: "session"}}},
		{URL: "https://example.com", ProxyURL: "ftp://proxy.example"},
		{URL: "https://example.com", CDPURL: "file:///tmp/socket"},
		{URL: "https://example.com", MaxAge: -1},
	} {
		_, err := service.Run(context.Background(), request, nil)
		var scrapeErr *models.ScrapeError
		if !errors.As(err, &scrapeErr) || scrapeErr.Code != models.ErrCodeInvalidInput {
			t.Fatalf("request %#v error = %#v, want INVALID_INPUT", request, err)
		}
	}
}

func mustService(t *testing.T, fetchers []Fetcher, contentCleaner ContentCleaner, responseCache ResponseCache, finalizer SourceFinalizer) *Service {
	t.Helper()
	service, err := NewService(fetchers, contentCleaner, responseCache, finalizer, Config{})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func assertEventTypes(t *testing.T, events []Event, want []EventType) {
	t.Helper()
	got := make([]EventType, len(events))
	for index := range events {
		got[index] = events[index].Type
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("event types = %v, want %v", got, want)
	}
}
