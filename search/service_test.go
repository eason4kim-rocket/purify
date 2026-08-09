package search

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/use-agent/purify/llm"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/simhash"
)

type stubSearchProvider struct {
	mu      sync.Mutex
	name    string
	results []ProviderResult
	err     error
	calls   int
	queries []ProviderQuery
	hook    func(context.Context, ProviderQuery, int) ([]ProviderResult, error)
}

func (provider *stubSearchProvider) Name() string {
	if provider == nil {
		return ""
	}
	return provider.name
}

func (provider *stubSearchProvider) Search(ctx context.Context, query ProviderQuery) ([]ProviderResult, error) {
	provider.mu.Lock()
	provider.calls++
	call := provider.calls
	provider.queries = append(provider.queries, query)
	hook := provider.hook
	results := append([]ProviderResult(nil), provider.results...)
	err := provider.err
	provider.mu.Unlock()
	if hook != nil {
		return hook(ctx, query, call)
	}
	return results, err
}

func (provider *stubSearchProvider) snapshot() (int, []ProviderQuery) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.calls, append([]ProviderQuery(nil), provider.queries...)
}

type searchTestClock struct {
	mu  sync.Mutex
	now time.Time
}

type testSearchServiceOption struct {
	applied *bool
}

func (option testSearchServiceOption) applySearchService(*Service) error {
	*option.applied = true
	return nil
}

type pointerSearchServiceOption struct{}

func (*pointerSearchServiceOption) applySearchService(*Service) error { return nil }

func (clock *searchTestClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *searchTestClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(duration)
	clock.mu.Unlock()
}

func TestNewSearchServiceValidatesProvider(t *testing.T) {
	var typedNil *stubSearchProvider
	tests := []struct {
		name     string
		provider Provider
	}{
		{name: "nil", provider: nil},
		{name: "typed nil", provider: typedNil},
		{name: "empty name", provider: &stubSearchProvider{}},
		{name: "oversized name", provider: &stubSearchProvider{name: strings.Repeat("x", MaxProviderNameBytes+1)}},
		{name: "huge raw padded name", provider: &stubSearchProvider{name: strings.Repeat(" ", 1<<20) + "stub"}},
		{name: "control name", provider: &stubSearchProvider{name: "bad\x00name"}},
		{name: "invalid UTF-8 name", provider: &stubSearchProvider{name: string([]byte{0xff})}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if service, err := NewService(test.provider); err == nil || service != nil {
				t.Fatalf("NewService() = (%#v, %v), want error", service, err)
			}
		})
	}
	service, err := NewService(&stubSearchProvider{name: " test "})
	if err != nil || service.providerName != "test" {
		t.Fatalf("NewService(valid) = (%#v, %v)", service, err)
	}
	if service.cache.ttl != time.Minute || service.cache.maxEntries != 256 || service.cache.maxBytes != 16<<20 {
		t.Fatalf("production cache bounds = ttl %s, entries %d, bytes %d", service.cache.ttl, service.cache.maxEntries, service.cache.maxBytes)
	}
	applied := false
	service, err = NewService(&stubSearchProvider{name: "stub"}, testSearchServiceOption{applied: &applied})
	if err != nil || service == nil || !applied {
		t.Fatalf("NewService(option) = (%#v, %v), applied=%v", service, err, applied)
	}
	var nilOption *pointerSearchServiceOption
	if service, err := NewService(&stubSearchProvider{name: "stub"}, nilOption); err == nil || service != nil {
		t.Fatalf("NewService(typed nil option) = (%#v, %v), want error", service, err)
	}
}

func TestSearchDetachesPersistedProviderStringsFromLargeBackings(t *testing.T) {
	providerName := suffixOfLargeBacking("stub")
	title := suffixOfLargeBacking("title")
	snippet := suffixOfLargeBacking("snippet")
	rawURL := suffixOfLargeBacking("https://example.com/path")
	provider := &stubSearchProvider{name: providerName, results: []ProviderResult{{
		Rank: 1, Title: title, URL: rawURL, Snippet: snippet,
	}}}
	service, err := NewService(provider)
	if err != nil {
		t.Fatal(err)
	}
	assertDetachedString(t, "provider name", service.providerName, providerName)

	first, err := service.Search(context.Background(), &models.SearchRequest{Query: "q"})
	if err != nil || len(first.Results) != 1 {
		t.Fatalf("first Search() = (%#v, %v)", first, err)
	}
	assertDetachedString(t, "normalized title", first.Results[0].Title, title)
	assertDetachedString(t, "normalized snippet", first.Results[0].Snippet, snippet)
	assertDetachedString(t, "normalized URL", first.Results[0].URL, rawURL)

	second, err := service.Search(context.Background(), &models.SearchRequest{Query: "q"})
	if err != nil || len(second.Results) != 1 {
		t.Fatalf("cached Search() = (%#v, %v)", second, err)
	}
	assertDetachedString(t, "cached title", second.Results[0].Title, first.Results[0].Title)
	assertDetachedString(t, "cached snippet", second.Results[0].Snippet, first.Results[0].Snippet)
	assertDetachedString(t, "cached URL", second.Results[0].URL, first.Results[0].URL)
}

func TestSearchNormalizesRequestAndResultsWithoutMutation(t *testing.T) {
	score := 0.75
	publishedAt := time.Date(2026, 8, 1, 9, 30, 0, 123, time.FixedZone("UTC+8", 8*60*60))
	provider := &stubSearchProvider{name: "stub", results: []ProviderResult{
		{Rank: 1, Score: &score, Title: "  First  ", URL: "HTTPS://Example.COM:443/a#section", Snippet: "  Alpha  ", PublishedAt: &publishedAt},
		{Rank: 2, Title: "bad URL", URL: "javascript:alert(1)", Snippet: "ignored"},
		{Rank: 3, Title: "Evil", URL: "https://evil-example.com/", Snippet: "not a suffix match"},
		{Rank: 4, Title: "  Second  ", URL: "https://news.example.com/story", Snippet: "  Beta  "},
	}}
	clock := &searchTestClock{now: time.Date(2026, 8, 10, 1, 0, 0, 0, time.UTC)}
	provider.hook = func(_ context.Context, _ ProviderQuery, _ int) ([]ProviderResult, error) {
		clock.Advance(25 * time.Millisecond)
		return append([]ProviderResult(nil), provider.results...), nil
	}
	service, err := newService(provider, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	request := &models.SearchRequest{
		Query:     "  alpha   beta  ",
		Domains:   []string{"Example.COM.", "example.com"},
		Freshness: "7d",
	}
	response, err := service.Search(context.Background(), request)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if request.Limit != 0 || request.Timeout != 0 || request.Deduplicate != nil || request.Domains[0] != "Example.COM." {
		t.Fatalf("Search mutated caller request: %#v", request)
	}
	if response.Query != "alpha beta" || !response.Success || response.Partial || response.Results == nil {
		t.Fatalf("response envelope = %#v", response)
	}
	if response.Timing.ProviderMs != 25 || response.Timing.TotalMs != 25 {
		t.Fatalf("timing = %#v, want provider=total=25ms", response.Timing)
	}
	if len(response.Results) != 2 {
		t.Fatalf("results = %#v, want two example.com results", response.Results)
	}
	first := response.Results[0]
	if first.Rank != 1 || first.Title != "First" || first.URL != "https://example.com/a" || first.Snippet != "Alpha" || first.Score == nil || *first.Score != score {
		t.Fatalf("first result = %#v", first)
	}
	if first.PublishedAt == nil || first.PublishedAt.Location() != time.UTC || !first.PublishedAt.Equal(publishedAt) {
		t.Fatalf("published_at = %#v", first.PublishedAt)
	}
	if first.VerificationStatus != models.SearchVerificationNotChecked || first.Verified != nil {
		t.Fatalf("baseline verification fields = %#v / %#v", first.VerificationStatus, first.Verified)
	}
	if response.Results[1].Rank != 2 || response.Results[1].URL != "https://news.example.com/story" {
		t.Fatalf("second result = %#v", response.Results[1])
	}
	calls, queries := provider.snapshot()
	if calls != 1 || len(queries) != 1 || queries[0] != (ProviderQuery{Text: "alpha beta", Limit: models.DefaultSearchLimit, Freshness: FreshnessWeek}) {
		t.Fatalf("provider calls=%d queries=%#v", calls, queries)
	}
}

func TestSearchDomainFilteringUsesHostnameBoundariesAndIDNA(t *testing.T) {
	provider := &stubSearchProvider{name: "stub", results: []ProviderResult{
		{Rank: 1, URL: "https://example.com/a"},
		{Rank: 2, URL: "https://sub.example.com/b"},
		{Rank: 3, URL: "https://evil-example.com/c"},
		{Rank: 4, URL: "https://xn--bcher-kva.de/d"},
	}}
	service, _ := NewService(provider)
	response, err := service.Search(context.Background(), &models.SearchRequest{Query: "q", Domains: []string{"BÜCHER.de."}})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 || response.Results[0].URL != "https://xn--bcher-kva.de/d" {
		t.Fatalf("IDNA-filtered results = %#v", response.Results)
	}

	response, err = service.Search(context.Background(), &models.SearchRequest{Query: "q", Domains: []string{"example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 2 || response.Results[0].URL != "https://example.com/a" || response.Results[1].URL != "https://sub.example.com/b" {
		t.Fatalf("boundary-filtered results = %#v", response.Results)
	}
	calls, _ := provider.snapshot()
	if calls != 1 {
		t.Fatalf("domain post-filtering made %d provider calls, want shared cached baseline", calls)
	}
}

func TestSearchRejectsInvalidDomainFiltersBeforeProvider(t *testing.T) {
	invalid := []string{
		"", "com", "co.uk", "8.8.8.8", "localhost", "x.localhost", "https://example.com",
		"example.com:443", "example.com/path", "*.example.com", ".example.com", "bad_domain.com",
		strings.Repeat("a", 64) + ".com", string([]byte{0xff}), "example.com\x00",
	}
	for _, domain := range invalid {
		t.Run(strings.ReplaceAll(domain, "/", "_"), func(t *testing.T) {
			provider := &stubSearchProvider{name: "stub"}
			service, _ := NewService(provider)
			_, err := service.Search(context.Background(), &models.SearchRequest{Query: "q", Domains: []string{domain}})
			requireSearchErrorCode(t, err, models.ErrCodeInvalidInput)
			if calls, _ := provider.snapshot(); calls != 0 {
				t.Fatalf("provider calls = %d, want zero", calls)
			}
		})
	}
}

func TestSearchDeduplicationPolicyAndStableRanks(t *testing.T) {
	provider := &stubSearchProvider{name: "stub", results: []ProviderResult{
		{Rank: 1, Title: "Breaking News", URL: "https://example.com/story#one", Snippet: "Shared wire copy"},
		{Rank: 2, Title: "duplicate URL", URL: "https://EXAMPLE.com:443/story#two", Snippet: "different text"},
		{Rank: 3, Title: "breaking news", URL: "https://other.example.net/reprint", Snippet: "shared wire copy"},
		{Rank: 4, Title: "", URL: "https://third.example.org/empty", Snippet: ""},
		{Rank: 5, Title: "", URL: "https://fourth.example.edu/empty", Snippet: ""},
	}}
	service, _ := NewService(provider)
	response, err := service.Search(context.Background(), &models.SearchRequest{Query: "q"})
	if err != nil {
		t.Fatal(err)
	}
	if response.Deduplicated != 2 || len(response.Results) != 3 {
		t.Fatalf("deduplicated response = %#v", response)
	}
	for index, result := range response.Results {
		if result.Rank != index+1 {
			t.Fatalf("result[%d].rank = %d", index, result.Rank)
		}
	}
	if response.Results[1].URL != "https://third.example.org/empty" || response.Results[2].URL != "https://fourth.example.edu/empty" {
		t.Fatalf("zero-fingerprint results were collapsed: %#v", response.Results)
	}

	deduplicate := false
	response, err = service.Search(context.Background(), &models.SearchRequest{Query: "q", Deduplicate: &deduplicate})
	if err != nil {
		t.Fatal(err)
	}
	if response.Deduplicated != 1 || len(response.Results) != 4 || response.Results[1].URL != "https://other.example.net/reprint" {
		t.Fatalf("explicit no-simhash response = %#v", response)
	}
	if calls, _ := provider.snapshot(); calls != 1 {
		t.Fatalf("dedup policy should reuse baseline; calls = %d", calls)
	}
}

func TestSearchSimhashDedupUsesTransitiveGlobalComponentsBeforeLimit(t *testing.T) {
	const (
		textA = "variant0 word1 word2 word3 word4 word5 word6 word7 word8 word9 word10 word11 word12 word13 word14 word15 word16 word17 word18 word19 word20 word21 word22 word23 word24 word25 word26 word27 word28 word29 word30"
		textB = "variant1 word1 word2 word3 word4 word5 word6 word7 word8 word9 word10 word11 word12 word13 word14 word15 word16 word17 word18 word19 word20 word21 word22 word23 word24 word25 word26 word27 word28 word29 word30"
		textC = "word0 variant1 word2 word3 word4 word5 word6 word7 word8 word9 word10 word11 word12 word13 word14 word15 word16 word17 word18 word19 word20 word21 word22 word23 word24 word25 word26 word27 word28 word29 word30"
	)
	fingerprintA := searchResultFingerprint(baselineResult{title: textA})
	fingerprintB := searchResultFingerprint(baselineResult{title: textB})
	fingerprintC := searchResultFingerprint(baselineResult{title: textC})
	if distanceAB, distanceBC, distanceAC := simhash.Distance(fingerprintA, fingerprintB), simhash.Distance(fingerprintB, fingerprintC), simhash.Distance(fingerprintA, fingerprintC); distanceAB > 3 || distanceBC > 3 || distanceAC <= 3 {
		t.Fatalf("fixture distances = A-B %d, B-C %d, A-C %d, want <=3, <=3, >3", distanceAB, distanceBC, distanceAC)
	}
	provider := &stubSearchProvider{name: "stub", results: []ProviderResult{
		{Rank: 1, Title: textA, URL: "https://alpha.example.com/"},
		{Rank: 2, Title: textB, URL: "https://bravo.example.net/"},
		{Rank: 3, Title: textC, URL: "https://charlie.example.org/"},
		{Rank: 4, Title: "unrelated result", URL: "https://delta.example.edu/"},
	}}
	service, _ := NewService(provider)
	response, err := service.Search(context.Background(), &models.SearchRequest{Query: "q", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if response.Deduplicated != 2 || len(response.Results) != 1 || response.Results[0].URL != "https://alpha.example.com/" {
		t.Fatalf("transitive component response = %#v", response)
	}
}

func TestSearchProviderValidation(t *testing.T) {
	validURL := "https://example.com/"
	validTime := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	zeroTime := time.Time{}
	yearOutOfRange := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	negative := -0.01
	overOne := 1.01
	nan := math.NaN()
	infinity := math.Inf(1)
	tests := []struct {
		name   string
		result ProviderResult
	}{
		{name: "rank", result: ProviderResult{Rank: 0, URL: validURL}},
		{name: "title bytes", result: ProviderResult{Rank: 1, URL: validURL, Title: strings.Repeat("t", MaxProviderTitleBytes+1)}},
		{name: "snippet bytes", result: ProviderResult{Rank: 1, URL: validURL, Snippet: strings.Repeat("s", MaxProviderSnippetBytes+1)}},
		{name: "title control", result: ProviderResult{Rank: 1, URL: validURL, Title: "bad\ntext"}},
		{name: "snippet control", result: ProviderResult{Rank: 1, URL: validURL, Snippet: "bad\ttext"}},
		{name: "title UTF-8", result: ProviderResult{Rank: 1, URL: validURL, Title: string([]byte{0xff})}},
		{name: "score negative", result: ProviderResult{Rank: 1, URL: validURL, Score: &negative}},
		{name: "score over one", result: ProviderResult{Rank: 1, URL: validURL, Score: &overOne}},
		{name: "score NaN", result: ProviderResult{Rank: 1, URL: validURL, Score: &nan}},
		{name: "score infinity", result: ProviderResult{Rank: 1, URL: validURL, Score: &infinity}},
		{name: "zero time", result: ProviderResult{Rank: 1, URL: validURL, PublishedAt: &zeroTime}},
		{name: "unencodable time", result: ProviderResult{Rank: 1, URL: validURL, PublishedAt: &yearOutOfRange}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := &stubSearchProvider{name: "stub", results: []ProviderResult{test.result}}
			service, _ := NewService(provider)
			_, err := service.Search(context.Background(), &models.SearchRequest{Query: "q"})
			requireSearchErrorCode(t, err, models.ErrCodeSearchFailed)
			var failure *ProviderError
			if !errors.As(err, &failure) || failure.Kind != ProviderErrorInvalidResponse {
				t.Fatalf("error = %v, want invalid provider response", err)
			}
		})
	}

	provider := &stubSearchProvider{name: "stub", results: []ProviderResult{{Rank: 1, URL: validURL, Title: " \nTitle\n "}}}
	service, _ := NewService(provider)
	response, err := service.Search(context.Background(), &models.SearchRequest{Query: "q"})
	if err != nil || len(response.Results) != 1 || response.Results[0].Title != "Title" {
		t.Fatalf("TrimSpace result = (%#v, %v)", response, err)
	}
	_ = validTime
}

func TestSearchDropsBadURLsButRejectsAllBadURLs(t *testing.T) {
	provider := &stubSearchProvider{name: "stub", results: []ProviderResult{
		{Rank: 1, URL: "http://127.0.0.1/private"},
		{Rank: 2, URL: "https://example.com/good"},
	}}
	service, _ := NewService(provider)
	response, err := service.Search(context.Background(), &models.SearchRequest{Query: "q"})
	if err != nil || len(response.Results) != 1 || response.Results[0].URL != "https://example.com/good" {
		t.Fatalf("mixed URL result = (%#v, %v)", response, err)
	}

	provider = &stubSearchProvider{name: "stub", results: []ProviderResult{{Rank: 1, URL: "file:///tmp/secret"}}}
	service, _ = NewService(provider)
	_, err = service.Search(context.Background(), &models.SearchRequest{Query: "q"})
	requireSearchErrorCode(t, err, models.ErrCodeSearchFailed)
}

func TestSearchBoundsProviderCandidatesAndMetadata(t *testing.T) {
	results := make([]ProviderResult, MaxProviderResults+1)
	for index := range results {
		results[index] = ProviderResult{Rank: index + 1, URL: "https://example.com/" + strings.Repeat("a", index+1)}
	}
	results[MaxProviderResults].Rank = 0 // The defensive prefix bound excludes this candidate.
	provider := &stubSearchProvider{name: "stub", results: results}
	service, _ := NewService(provider)
	response, err := service.Search(context.Background(), &models.SearchRequest{Query: "q", Limit: models.MaxSearchLimit})
	if err != nil || len(response.Results) != MaxProviderResults {
		t.Fatalf("bounded candidates = (%d, %v)", len(response.Results), err)
	}

	hugeURL := "https://example.com/" + strings.Repeat("x", 120<<10)
	results = make([]ProviderResult, MaxProviderResults)
	for index := range results {
		results[index] = ProviderResult{Rank: index + 1, URL: hugeURL}
	}
	provider = &stubSearchProvider{name: "stub", results: results}
	service, _ = NewService(provider)
	_, err = service.Search(context.Background(), &models.SearchRequest{Query: "q"})
	requireSearchErrorCode(t, err, models.ErrCodeSearchFailed)
}

func TestSearchEmptyProviderResultIsSuccessfulAndCacheable(t *testing.T) {
	provider := &stubSearchProvider{name: "stub", results: []ProviderResult{}}
	service, _ := NewService(provider)
	for range 2 {
		response, err := service.Search(context.Background(), &models.SearchRequest{Query: "nothing"})
		if err != nil || !response.Success || response.Results == nil || len(response.Results) != 0 {
			t.Fatalf("empty response = (%#v, %v)", response, err)
		}
	}
	if calls, _ := provider.snapshot(); calls != 1 {
		t.Fatalf("provider calls = %d, want one", calls)
	}
}

func TestSearchCacheDeepClonesAndSharesOnlyBaselineDimensions(t *testing.T) {
	score := 0.5
	publishedAt := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	provider := &stubSearchProvider{name: "stub", results: []ProviderResult{
		{Rank: 1, Score: &score, PublishedAt: &publishedAt, Title: "same", URL: "https://one.example.com/a", Snippet: "copy"},
		{Rank: 2, Title: "same", URL: "https://two.example.net/b", Snippet: "copy"},
	}}
	service, _ := NewService(provider)
	first, err := service.Search(context.Background(), &models.SearchRequest{Query: "cache   me", Domains: []string{"example.com"}})
	if err != nil || len(first.Results) != 1 {
		t.Fatalf("first response = (%#v, %v)", first, err)
	}
	*first.Results[0].Score = 0.99
	first.Results[0].PublishedAt = nil
	first.Results[0].Title = "mutated"

	deduplicate := false
	second, err := service.Search(context.Background(), &models.SearchRequest{Query: " cache me ", Domains: []string{"example.net"}, Deduplicate: &deduplicate})
	if err != nil || len(second.Results) != 1 {
		t.Fatalf("second response = (%#v, %v)", second, err)
	}
	if second.Results[0].Score != nil || second.Results[0].Title != "same" {
		t.Fatalf("second result = %#v", second.Results[0])
	}

	third, err := service.Search(context.Background(), &models.SearchRequest{Query: "cache me", Deduplicate: &deduplicate})
	if err != nil || len(third.Results) != 2 || third.Results[0].Score == nil || *third.Results[0].Score != score || third.Results[0].PublishedAt == nil || third.Results[0].Title != "same" {
		t.Fatalf("deep-cloned third response = (%#v, %v)", third, err)
	}
	if calls, _ := provider.snapshot(); calls != 1 {
		t.Fatalf("provider calls = %d, want one baseline call", calls)
	}
}

func TestSearchCacheTTLAndKeyDimensions(t *testing.T) {
	clock := &searchTestClock{now: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)}
	provider := &stubSearchProvider{name: "stub", hook: func(_ context.Context, _ ProviderQuery, call int) ([]ProviderResult, error) {
		return []ProviderResult{{Rank: 1, URL: "https://example.com/", Title: string(rune('0' + call))}}, nil
	}}
	service, _ := newService(provider, clock.Now)
	request := &models.SearchRequest{Query: "q", Limit: 1, Freshness: "day"}
	first, _ := service.Search(context.Background(), request)
	clock.Advance(baselineCacheTTL - time.Nanosecond)
	second, _ := service.Search(context.Background(), request)
	if first.Results[0].Title != second.Results[0].Title {
		t.Fatalf("entry expired before TTL: %q != %q", first.Results[0].Title, second.Results[0].Title)
	}
	clock.Advance(time.Nanosecond)
	third, _ := service.Search(context.Background(), request)
	if third.Results[0].Title == second.Results[0].Title {
		t.Fatalf("entry remained at TTL boundary: %#v", third.Results)
	}

	_, _ = service.Search(context.Background(), &models.SearchRequest{Query: "q2", Limit: 1, Freshness: "day"})
	_, _ = service.Search(context.Background(), &models.SearchRequest{Query: "q2", Limit: 2, Freshness: "day"})
	_, _ = service.Search(context.Background(), &models.SearchRequest{Query: "q2", Limit: 2, Freshness: "week"})
	calls, _ := provider.snapshot()
	if calls != 5 {
		t.Fatalf("provider calls = %d, want five distinct/expired baselines", calls)
	}
}

func TestSearchBypassFetchesAndRefillsWithoutDestroyingOnFailure(t *testing.T) {
	provider := &stubSearchProvider{name: "stub", hook: func(_ context.Context, _ ProviderQuery, call int) ([]ProviderResult, error) {
		switch call {
		case 1:
			return []ProviderResult{{Rank: 1, URL: "https://example.com/v1"}}, nil
		case 2:
			return []ProviderResult{{Rank: 1, URL: "https://example.com/v2"}}, nil
		case 3:
			return nil, NewProviderError(ProviderErrorUpstream, 502, errors.New("secret upstream detail"))
		default:
			return nil, errors.New("unexpected call")
		}
	}}
	service, _ := NewService(provider)
	request := &models.SearchRequest{Query: "q"}
	first, _ := service.Search(context.Background(), request)
	second, err := service.SearchWithOptions(context.Background(), request, RunOptions{BypassProviderCache: true})
	if err != nil || first.Results[0].URL != "https://example.com/v1" || second.Results[0].URL != "https://example.com/v2" {
		t.Fatalf("fresh refill = (%#v, %#v, %v)", first, second, err)
	}
	third, _ := service.Search(context.Background(), request)
	if third.Results[0].URL != "https://example.com/v2" {
		t.Fatalf("normal read did not see refill: %#v", third.Results)
	}
	_, err = service.SearchWithOptions(context.Background(), request, RunOptions{BypassProviderCache: true})
	requireSearchErrorCode(t, err, models.ErrCodeSearchFailed)
	if strings.Contains(err.Error(), "secret upstream detail") {
		t.Fatalf("public error leaked provider cause: %v", err)
	}
	afterFailure, err := service.Search(context.Background(), request)
	if err != nil || afterFailure.Results[0].URL != "https://example.com/v2" {
		t.Fatalf("failed bypass destroyed cache: (%#v, %v)", afterFailure, err)
	}
	if calls, _ := provider.snapshot(); calls != 3 {
		t.Fatalf("provider calls = %d, want three", calls)
	}
}

func TestSearchMapsProviderAndContextErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code string
	}{
		{name: "auth", err: NewProviderError(ProviderErrorAuthentication, 401, errors.New("credential abc")), code: models.ErrCodeSearchUnavailable},
		{name: "rate", err: NewProviderError(ProviderErrorRateLimited, 429, errors.New("provider text")), code: models.ErrCodeRateLimited},
		{name: "timeout", err: NewProviderError(ProviderErrorTimeout, 0, errors.New("provider text")), code: models.ErrCodeTimeout},
		{name: "upstream", err: NewProviderError(ProviderErrorUpstream, 502, errors.New("provider text")), code: models.ErrCodeSearchFailed},
		{name: "invalid", err: NewProviderError(ProviderErrorInvalidResponse, 200, errors.New("provider text")), code: models.ErrCodeSearchFailed},
		{name: "plain", err: errors.New("secret plain failure"), code: models.ErrCodeSearchFailed},
		{name: "context", err: context.DeadlineExceeded, code: models.ErrCodeTimeout},
		{name: "wrapped context", err: errors.Join(errors.New("secret context detail"), context.DeadlineExceeded), code: models.ErrCodeTimeout},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := &stubSearchProvider{name: "stub", err: test.err}
			service, _ := NewService(provider)
			_, err := service.Search(context.Background(), &models.SearchRequest{Query: "q"})
			requireSearchErrorCode(t, err, test.code)
			if strings.Contains(err.Error(), "credential abc") || strings.Contains(err.Error(), "provider text") || strings.Contains(err.Error(), "secret plain failure") || strings.Contains(err.Error(), "secret context detail") {
				t.Fatalf("public error leaked provider details: %v", err)
			}
			if test.name == "wrapped context" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("wrapped context identity was lost: %v", err)
			}
		})
	}

	provider := &stubSearchProvider{name: "stub"}
	service, _ := NewService(provider)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := service.Search(canceled, &models.SearchRequest{Query: "q"})
	requireSearchErrorCode(t, err, models.ErrCodeTimeout)
	if calls, _ := provider.snapshot(); calls != 0 {
		t.Fatalf("canceled request made %d provider calls", calls)
	}

	parent, cancel := context.WithCancel(context.Background())
	provider.hook = func(_ context.Context, _ ProviderQuery, _ int) ([]ProviderResult, error) {
		cancel()
		return []ProviderResult{{Rank: 1, URL: "https://example.com/"}}, nil
	}
	_, err = service.Search(parent, &models.SearchRequest{Query: "fresh"})
	requireSearchErrorCode(t, err, models.ErrCodeTimeout)
}

func TestSearchBoundsProviderWorkWithIndependentChildContext(t *testing.T) {
	var observedBudget time.Duration
	provider := &stubSearchProvider{name: "stub", hook: func(ctx context.Context, _ ProviderQuery, _ int) ([]ProviderResult, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			return nil, errors.New("provider context has no deadline")
		}
		observedBudget = time.Until(deadline)
		return []ProviderResult{{Rank: 1, URL: "https://example.com/"}}, nil
	}}
	service, _ := NewService(provider)
	response, err := service.Search(context.Background(), &models.SearchRequest{Query: "q", Timeout: models.MaxSearchTimeoutSeconds})
	if err != nil || len(response.Results) != 1 {
		t.Fatalf("Search() = (%#v, %v)", response, err)
	}
	if observedBudget <= 0 || observedBudget > providerSearchTimeout {
		t.Fatalf("provider deadline budget = %s, want (0,%s]", observedBudget, providerSearchTimeout)
	}

	provider = &stubSearchProvider{name: "stub", hook: func(ctx context.Context, _ ProviderQuery, _ int) ([]ProviderResult, error) {
		<-ctx.Done()
		return []ProviderResult{{Rank: 1, URL: "https://example.com/late"}}, nil
	}}
	service, _ = NewService(provider)
	service.providerTimeout = 5 * time.Millisecond
	response, err = service.Search(context.Background(), &models.SearchRequest{Query: "late", Timeout: models.MaxSearchTimeoutSeconds})
	if response != nil {
		t.Fatalf("late provider success returned response: %#v", response)
	}
	requireSearchErrorCode(t, err, models.ErrCodeTimeout)
}

func TestBaselineCacheKeyIncludesProviderIdentity(t *testing.T) {
	query := ProviderQuery{Text: "same", Limit: 10, Freshness: FreshnessMonth}
	if baselineKey("provider-one", query) == baselineKey("provider-two", query) {
		t.Fatal("baseline cache key omitted provider identity")
	}
}

func TestSearchRejectsUnsupportedHeavyCapabilitiesBeforeProvider(t *testing.T) {
	validSchema := jsonRaw(`{"type":"object"}`)
	tests := []models.SearchRequest{
		{Query: "q", IncludeContent: true},
		{Query: "q", Verify: true},
		{Query: "q", Schema: validSchema},
	}
	for index := range tests {
		provider := &stubSearchProvider{name: "stub"}
		service, _ := NewService(provider)
		_, err := service.Search(context.Background(), &tests[index])
		requireSearchErrorCode(t, err, models.ErrCodeSearchUnavailable)
		if calls, _ := provider.snapshot(); calls != 0 {
			t.Fatalf("test %d made %d provider calls", index, calls)
		}
	}

	invalid := []models.SearchRequest{
		{Query: "q", Schema: make([]byte, 0)},
		{Query: "q", Schema: jsonRaw(`null`)},
		{Query: "q", Schema: jsonRaw(`[]`)},
		{Query: "q", Schema: jsonRaw(`{`)},
		{Query: "q", Engine: "auto"},
		{Query: "q", Schema: validSchema, LLMModel: "bad model"},
		{Query: "q", Schema: validSchema, LLMBaseURL: "https://api.openai.com/v1?secret=x"},
		{Query: "q", Schema: validSchema, LLMBaseURL: "https://api.openai.com/v1#fragment"},
		{Query: "q", Schema: validSchema, Engine: "llm"},
		{Query: "q", Schema: validSchema, Engine: "llm", LLMAPIKey: "   "},
		{Query: "q", Schema: jsonRaw(`{"$ref":"https://schemas.example.com/external.json"}`)},
	}
	for index := range invalid {
		provider := &stubSearchProvider{name: "stub"}
		service, _ := NewService(provider)
		_, err := service.Search(context.Background(), &invalid[index])
		requireSearchErrorCode(t, err, models.ErrCodeInvalidInput)
	}
}

func TestSearchRejectsNormalizedSchemaExpansionBeforeProvider(t *testing.T) {
	var schema strings.Builder
	schema.WriteByte('{')
	for index := range 12_000 {
		if index > 0 {
			schema.WriteByte(',')
		}
		schema.WriteString(`"field`)
		schema.WriteString(strconv.Itoa(index))
		schema.WriteString(`":"string"`)
	}
	schema.WriteByte('}')
	raw := jsonRaw(schema.String())
	if len(raw) > models.MaxSearchSchemaBytes {
		t.Fatalf("raw fixture = %d bytes, must remain within %d", len(raw), models.MaxSearchSchemaBytes)
	}
	normalized, err := llm.NormalizeSchema(raw)
	if err != nil {
		t.Fatalf("NormalizeSchema(fixture) error = %v", err)
	}
	if len(normalized) <= models.MaxSearchSchemaBytes {
		t.Fatalf("normalized fixture = %d bytes, want > %d", len(normalized), models.MaxSearchSchemaBytes)
	}
	provider := &stubSearchProvider{name: "stub"}
	service, _ := NewService(provider)
	_, err = service.Search(context.Background(), &models.SearchRequest{Query: "q", Schema: raw})
	requireSearchErrorCode(t, err, models.ErrCodeInvalidInput)
	if calls, _ := provider.snapshot(); calls != 0 {
		t.Fatalf("provider calls = %d, want zero", calls)
	}
}

func TestSearchRequestValidationBoundaries(t *testing.T) {
	deduplicate := true
	tests := []struct {
		name    string
		request *models.SearchRequest
	}{
		{name: "nil", request: nil},
		{name: "empty query", request: &models.SearchRequest{}},
		{name: "query control", request: &models.SearchRequest{Query: "a\nb"}},
		{name: "query UTF-8", request: &models.SearchRequest{Query: string([]byte{0xff})}},
		{name: "query runes", request: &models.SearchRequest{Query: strings.Repeat("界", models.MaxSearchQueryRunes+1)}},
		{name: "query words", request: &models.SearchRequest{Query: strings.Repeat("x ", models.MaxSearchQueryWords) + "x"}},
		{name: "negative limit", request: &models.SearchRequest{Query: "q", Limit: -1}},
		{name: "large limit", request: &models.SearchRequest{Query: "q", Limit: models.MaxSearchLimit + 1}},
		{name: "freshness wire code", request: &models.SearchRequest{Query: "q", Freshness: "pw"}},
		{name: "negative timeout", request: &models.SearchRequest{Query: "q", Timeout: -1}},
		{name: "large timeout", request: &models.SearchRequest{Query: "q", Timeout: models.MaxSearchTimeoutSeconds + 1}},
		{name: "too many domains", request: &models.SearchRequest{Query: "q", Domains: make([]string, models.MaxSearchDomains+1)}},
		{name: "oversized schema", request: &models.SearchRequest{Query: "q", Schema: jsonRaw(strings.Repeat(" ", models.MaxSearchSchemaBytes+1))}},
		{name: "invalid engine", request: &models.SearchRequest{Query: "q", Schema: jsonRaw(`{}`), Engine: "other"}},
		{name: "orphan settings", request: &models.SearchRequest{Query: "q", LLMModel: "model"}},
		{name: "explicit dedup valid control", request: &models.SearchRequest{Query: "q", Deduplicate: &deduplicate, IncludeContent: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := &stubSearchProvider{name: "stub"}
			service, _ := NewService(provider)
			_, err := service.Search(context.Background(), test.request)
			if test.name == "explicit dedup valid control" {
				requireSearchErrorCode(t, err, models.ErrCodeSearchUnavailable)
			} else {
				requireSearchErrorCode(t, err, models.ErrCodeInvalidInput)
			}
			if calls, _ := provider.snapshot(); calls != 0 {
				t.Fatalf("provider calls = %d, want zero", calls)
			}
		})
	}
	service, _ := NewService(&stubSearchProvider{name: "stub"})
	_, err := service.Search(nil, &models.SearchRequest{Query: "q"})
	requireSearchErrorCode(t, err, models.ErrCodeInvalidInput)
}

func requireSearchErrorCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want %s", code)
	}
	var scrapeError *models.ScrapeError
	if !errors.As(err, &scrapeError) {
		t.Fatalf("error = %T %v, want *models.ScrapeError", err, err)
	}
	if scrapeError.Code != code {
		t.Fatalf("error code = %q, want %q (%v)", scrapeError.Code, code, err)
	}
}

func jsonRaw(value string) []byte {
	return []byte(value)
}

func suffixOfLargeBacking(suffix string) string {
	backing := strings.Repeat("x", 2<<20) + suffix
	return backing[len(backing)-len(suffix):]
}

func assertDetachedString(t *testing.T, label, detached, source string) {
	t.Helper()
	if detached != source {
		t.Fatalf("%s = %q, want %q", label, detached, source)
	}
	if detached != "" && unsafe.StringData(detached) == unsafe.StringData(source) {
		t.Fatalf("%s retained caller string backing", label)
	}
}
