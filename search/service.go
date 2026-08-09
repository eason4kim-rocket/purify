package search

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"net/netip"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/use-agent/purify/llm"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/publicnet"
	"github.com/use-agent/purify/simhash"
	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
)

const (
	searchBaselineCacheVersion = "purify-search-baseline/v1"
	maximumRawDomainBytes      = models.MaxSearchDomainBytes*utf8.UTFMax + 1
	simhashDedupDistance       = 3
	providerSearchTimeout      = 8 * time.Second
)

// RunOptions controls internal execution policy without expanding the public
// Search request. BypassProviderCache forces a fresh provider call; a
// successful result still refills the bounded baseline cache.
type RunOptions struct {
	BypassProviderCache bool
}

// ServiceOption reserves an additive construction boundary for optional
// Search capabilities. Implementations live in this package so later concerns
// can add With... constructors without changing NewService's function type.
type ServiceOption interface {
	applySearchService(*Service) error
}

// Service owns provider-neutral baseline search, normalization, post-provider
// domain filtering, deterministic deduplication, and bounded result caching.
// Optional page enrichment is layered onto this type in later concerns.
type Service struct {
	provider          Provider
	providerName      string
	cache             *baselineCache
	now               func() time.Time
	providerTimeout   time.Duration
	artifacts         ArtifactService
	signer            ReceiptSigner
	enrichmentSlots   chan struct{}
	enrichmentTimeout time.Duration
	encodingSlots     chan struct{}
	encodeSearch      func(any) ([]byte, error)
}

// NewService constructs a Search service with the fixed one-minute,
// 256-entry, 16 MiB baseline cache.
func NewService(provider Provider, options ...ServiceOption) (*Service, error) {
	return newService(provider, time.Now, options...)
}

func newService(provider Provider, now func() time.Time, options ...ServiceOption) (*Service, error) {
	if isNilProvider(provider) {
		return nil, errors.New("search: provider is required")
	}
	rawProviderName := provider.Name()
	if len(rawProviderName) > MaxProviderNameBytes || !utf8.ValidString(rawProviderName) || containsControl(rawProviderName) {
		return nil, errors.New("search: provider name is invalid")
	}
	providerName := strings.TrimSpace(rawProviderName)
	if providerName == "" {
		return nil, errors.New("search: provider name is invalid")
	}
	providerName = strings.Clone(providerName)
	if now == nil {
		now = time.Now
	}
	service := &Service{
		provider:     provider,
		providerName: providerName,
		cache: newBaselineCache(
			baselineCacheTTL,
			baselineCacheMaxEntries,
			baselineCacheMaxBytes,
			now,
		),
		now:               now,
		providerTimeout:   providerSearchTimeout,
		enrichmentTimeout: defaultSearchEnrichmentTimeout,
		encodingSlots:     make(chan struct{}, defaultSearchEncodingSlots),
		encodeSearch:      json.Marshal,
	}
	for _, option := range options {
		if isNilServiceOption(option) {
			return nil, errors.New("search: service option is nil")
		}
		if err := option.applySearchService(service); err != nil {
			return nil, err
		}
	}
	return service, nil
}

// Search executes provider-neutral Search with the default cache policy.
func (service *Service) Search(ctx context.Context, request *models.SearchRequest) (*models.SearchResponse, error) {
	return service.SearchWithOptions(ctx, request, RunOptions{})
}

// SearchWithOptions executes Search while allowing trusted internal callers to
// bypass a cached baseline. The caller-owned request is never mutated.
func (service *Service) SearchWithOptions(ctx context.Context, request *models.SearchRequest, options RunOptions) (*models.SearchResponse, error) {
	if ctx == nil {
		return nil, invalidSearchInput("search context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, searchTimeout(err)
	}
	if service == nil || isNilProvider(service.provider) || service.cache == nil {
		return nil, searchUnavailable("search is unavailable", nil)
	}
	startedAt := service.now()
	prepared, err := prepareSearchRequest(request)
	if err != nil {
		return nil, err
	}
	if prepared.requiresEnrichment && !service.enrichmentAvailable() {
		return nil, searchUnavailable("requested search capability is unavailable", nil)
	}

	runCtx, cancel := context.WithTimeout(ctx, prepared.timeout)
	defer cancel()
	if err := runCtx.Err(); err != nil {
		return nil, searchTimeout(err)
	}

	providerQuery := ProviderQuery{Text: prepared.query, Limit: prepared.limit, Freshness: prepared.freshness}
	cacheKey := baselineKey(service.providerName, providerQuery)
	providerMilliseconds := int64(0)
	var baseline []baselineResult
	found := false
	if !options.BypassProviderCache {
		baseline, found = service.cache.get(cacheKey)
	}
	if !found {
		providerStartedAt := service.now()
		providerCtx, providerCancel := context.WithTimeout(runCtx, service.providerTimeout)
		providerResults, providerErr := service.provider.Search(providerCtx, providerQuery)
		providerContextErr := providerCtx.Err()
		providerCancel()
		providerMilliseconds = elapsedMilliseconds(providerStartedAt, service.now())
		if providerContextErr != nil {
			return nil, searchTimeout(providerContextErr)
		}
		if providerErr != nil {
			return nil, publicProviderError(providerErr)
		}
		baseline, providerErr = normalizeProviderResults(providerResults)
		if providerErr != nil {
			return nil, publicProviderError(providerErr)
		}
		if err := runCtx.Err(); err != nil {
			return nil, searchTimeout(err)
		}
		service.cache.set(cacheKey, baseline)
	}

	results, deduplicated := projectBaseline(baseline, prepared.domains, prepared.deduplicate, prepared.limit)
	if err := runCtx.Err(); err != nil {
		return nil, searchTimeout(err)
	}
	droppedStale := 0
	partial := false
	enrichmentMilliseconds := int64(0)
	if prepared.requiresEnrichment && len(results) > 0 {
		enrichmentStartedAt := service.now()
		var additionalDeduplicated int
		results, droppedStale, additionalDeduplicated, partial = service.enrichResults(runCtx, results, prepared)
		enrichmentMilliseconds = elapsedMilliseconds(enrichmentStartedAt, service.now())
		deduplicated += additionalDeduplicated
		if err := runCtx.Err(); err != nil {
			return nil, searchTimeout(err)
		}
	}
	return &models.SearchResponse{
		Success:      true,
		Query:        prepared.query,
		Results:      results,
		Deduplicated: deduplicated,
		DroppedStale: droppedStale,
		Partial:      partial,
		Timing: models.SearchTimingInfo{
			TotalMs:      elapsedMilliseconds(startedAt, service.now()),
			ProviderMs:   providerMilliseconds,
			EnrichmentMs: enrichmentMilliseconds,
		},
	}, nil
}

type preparedSearchRequest struct {
	query              string
	limit              int
	domains            []string
	freshness          Freshness
	deduplicate        bool
	timeout            time.Duration
	requiresEnrichment bool
	includeContent     bool
	verify             bool
	schema             json.RawMessage
	engine             string
	llmAPIKey          string
	llmModel           string
	llmBaseURL         string
}

func prepareSearchRequest(source *models.SearchRequest) (preparedSearchRequest, error) {
	if source == nil {
		return preparedSearchRequest{}, invalidSearchInput("search request is required")
	}
	// Bound mutable collections before cloning so trusted in-process callers do
	// not get an unbounded allocation path that the HTTP body limit would hide.
	if len(source.Domains) > models.MaxSearchDomains {
		return preparedSearchRequest{}, invalidSearchInput("search domains cannot contain more than 20 entries")
	}
	if len(source.Schema) > models.MaxSearchSchemaBytes {
		return preparedSearchRequest{}, invalidSearchInput("search schema exceeds 524288 bytes")
	}
	request := cloneSearchRequest(source)
	request.Defaults()

	query, err := normalizeSearchQuery(request.Query)
	if err != nil {
		return preparedSearchRequest{}, err
	}
	if request.Limit < 1 || request.Limit > models.MaxSearchLimit {
		return preparedSearchRequest{}, invalidSearchInput("search limit must be between 1 and 20")
	}
	domains, err := normalizeSearchDomains(request.Domains)
	if err != nil {
		return preparedSearchRequest{}, err
	}
	freshness, err := ParseFreshness(request.Freshness)
	if err != nil {
		return preparedSearchRequest{}, invalidSearchInput("search freshness must be day, week, month, or year")
	}
	if request.Deduplicate == nil {
		return preparedSearchRequest{}, invalidSearchInput("search deduplication policy is invalid")
	}
	if request.Timeout < 1 || request.Timeout > models.MaxSearchTimeoutSeconds {
		return preparedSearchRequest{}, invalidSearchInput("search timeout must be between 1 and 120 seconds")
	}

	schemaPresent, err := validateSearchExtractionOptions(&request)
	if err != nil {
		return preparedSearchRequest{}, err
	}
	return preparedSearchRequest{
		query:              query,
		limit:              request.Limit,
		domains:            domains,
		freshness:          freshness,
		deduplicate:        *request.Deduplicate,
		timeout:            time.Duration(request.Timeout) * time.Second,
		requiresEnrichment: request.IncludeContent || request.Verify || schemaPresent,
		includeContent:     request.IncludeContent,
		verify:             request.Verify,
		schema:             bytes.Clone(request.Schema),
		engine:             strings.Clone(request.Engine),
		llmAPIKey:          strings.Clone(request.LLMAPIKey),
		llmModel:           strings.Clone(request.LLMModel),
		llmBaseURL:         strings.Clone(request.LLMBaseURL),
	}, nil
}

func cloneSearchRequest(source *models.SearchRequest) models.SearchRequest {
	cloned := *source
	cloned.Domains = append([]string(nil), source.Domains...)
	if source.Schema != nil {
		cloned.Schema = append(make(json.RawMessage, 0, len(source.Schema)), source.Schema...)
	}
	if source.Deduplicate != nil {
		value := *source.Deduplicate
		cloned.Deduplicate = &value
	}
	return cloned
}

func normalizeSearchQuery(raw string) (string, error) {
	if !utf8.ValidString(raw) || utf8.RuneCountInString(raw) > models.MaxSearchQueryRunes || containsControl(raw) {
		return "", invalidSearchInput("search query is invalid")
	}
	words := strings.Fields(raw)
	if len(words) == 0 || len(words) > models.MaxSearchQueryWords {
		return "", invalidSearchInput("search query must contain between 1 and 50 words")
	}
	return strings.Clone(strings.Join(words, " ")), nil
}

func normalizeSearchDomains(rawDomains []string) ([]string, error) {
	if len(rawDomains) > models.MaxSearchDomains {
		return nil, invalidSearchInput("search domains cannot contain more than 20 entries")
	}
	unique := make(map[string]struct{}, len(rawDomains))
	for _, raw := range rawDomains {
		domain, err := normalizeSearchDomain(raw)
		if err != nil {
			return nil, invalidSearchInput("search domains must be registrable hostnames")
		}
		unique[domain] = struct{}{}
	}
	domains := make([]string, 0, len(unique))
	for domain := range unique {
		domains = append(domains, domain)
	}
	sort.Strings(domains)
	return domains, nil
}

func normalizeSearchDomain(raw string) (string, error) {
	if !utf8.ValidString(raw) || len(raw) > maximumRawDomainBytes || containsControl(raw) {
		return "", errors.New("invalid domain")
	}
	domain := strings.TrimSuffix(strings.TrimSpace(raw), ".")
	if domain == "" || strings.ContainsAny(domain, "/\\:@?#*[]") {
		return "", errors.New("invalid domain")
	}
	canonical, err := idna.Lookup.ToASCII(domain)
	if err != nil {
		return "", err
	}
	canonical = strings.ToLower(canonical)
	if canonical == "" || len(canonical) > models.MaxSearchDomainBytes || canonical == "localhost" || strings.HasSuffix(canonical, ".localhost") {
		return "", errors.New("invalid domain")
	}
	if _, err := netip.ParseAddr(canonical); err == nil {
		return "", errors.New("IP domain filters are not supported")
	}
	for _, label := range strings.Split(canonical, ".") {
		if !validDNSLabel(label) {
			return "", errors.New("invalid domain label")
		}
	}
	if _, err := publicsuffix.EffectiveTLDPlusOne(canonical); err != nil {
		return "", err
	}
	return canonical, nil
}

func validDNSLabel(label string) bool {
	if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for index := range len(label) {
		character := label[index]
		if character != '-' && (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func validateSearchExtractionOptions(request *models.SearchRequest) (bool, error) {
	schemaPresent := request.Schema != nil
	if !schemaPresent {
		if request.Engine != "" || request.LLMAPIKey != "" || request.LLMModel != "" || request.LLMBaseURL != "" {
			return false, invalidSearchInput("search extraction settings require a schema")
		}
		return false, nil
	}
	if len(request.Schema) > models.MaxSearchSchemaBytes {
		return false, invalidSearchInput("search schema exceeds 524288 bytes")
	}
	if len(request.Schema) == 0 || !utf8.Valid(request.Schema) {
		return false, invalidSearchInput("search schema is invalid")
	}
	normalizedSchema, err := llm.NormalizeSchema(request.Schema)
	if err != nil {
		return false, invalidSearchInput("search schema is invalid")
	}
	if len(normalizedSchema) > models.MaxSearchSchemaBytes {
		return false, invalidSearchInput("normalized search schema exceeds 524288 bytes")
	}
	if err := llm.ValidateSchema(normalizedSchema); err != nil {
		return false, invalidSearchInput("search schema is invalid")
	}
	request.Schema = bytes.Clone(normalizedSchema)
	switch request.Engine {
	case "auto", "compiled", "llm":
	default:
		return false, invalidSearchInput("search extraction engine is invalid")
	}
	if err := validateBoundedSearchText(request.LLMAPIKey, models.MaxSearchLLMAPIKeyBytes, true, false); err != nil {
		return false, invalidSearchInput("search extraction API key is invalid")
	}
	if request.Engine == "llm" && strings.TrimSpace(request.LLMAPIKey) == "" {
		return false, invalidSearchInput("llm search extraction requires an API key")
	}
	if err := validateBoundedSearchText(request.LLMModel, models.MaxSearchLLMModelBytes, false, true); err != nil {
		return false, invalidSearchInput("search extraction model is invalid")
	}
	if err := validateBoundedSearchText(request.LLMBaseURL, models.MaxSearchLLMBaseURLBytes, false, false); err != nil {
		return false, invalidSearchInput("search extraction base URL is invalid")
	}
	baseURL, err := normalizeSearchLLMBaseURL(request.LLMBaseURL)
	if err != nil {
		return false, invalidSearchInput("search extraction base URL is invalid")
	}
	request.LLMBaseURL = baseURL
	return true, nil
}

func validateBoundedSearchText(value string, maximum int, allowEmpty, rejectWhitespace bool) error {
	if (!allowEmpty && strings.TrimSpace(value) == "") || len(value) > maximum || !utf8.ValidString(value) || containsControl(value) {
		return errors.New("invalid text")
	}
	if rejectWhitespace {
		for _, character := range value {
			if unicode.IsSpace(character) {
				return errors.New("invalid text")
			}
		}
	}
	return nil
}

func normalizeSearchLLMBaseURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Opaque != "" || parsed.Host == "" || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		strings.Contains(raw, "#") || strings.HasSuffix(parsed.Host, ":") {
		return "", errors.New("invalid base URL")
	}
	canonical, _, err := publicnet.NormalizeHTTPURL(raw, nil, false)
	if err != nil {
		return "", errors.New("invalid base URL")
	}
	return strings.TrimRight(canonical, "/"), nil
}

func normalizeProviderResults(source []ProviderResult) ([]baselineResult, error) {
	count := min(len(source), MaxProviderResults)
	results := make([]baselineResult, 0, count)
	totalBytes := 0
	for index := range count {
		candidate := source[index]
		if candidate.Rank != index+1 {
			return nil, invalidProviderResponse(errors.New("provider rank is invalid"))
		}
		if err := reserveProviderMetadata(&totalBytes, candidate.URL, candidate.Title, candidate.Snippet); err != nil {
			return nil, invalidProviderResponse(err)
		}
		title, err := normalizeProviderText(candidate.Title, MaxProviderTitleBytes)
		if err != nil {
			return nil, invalidProviderResponse(errors.New("provider title is invalid"))
		}
		snippet, err := normalizeProviderText(candidate.Snippet, MaxProviderSnippetBytes)
		if err != nil {
			return nil, invalidProviderResponse(errors.New("provider snippet is invalid"))
		}
		if candidate.Score != nil && (math.IsNaN(*candidate.Score) || math.IsInf(*candidate.Score, 0) || *candidate.Score < 0 || *candidate.Score > 1) {
			return nil, invalidProviderResponse(errors.New("provider score is invalid"))
		}
		publishedAt, err := normalizeProviderTime(candidate.PublishedAt)
		if err != nil {
			return nil, invalidProviderResponse(err)
		}
		canonicalURL, err := normalizeProviderURL(candidate.URL)
		if err != nil {
			continue
		}
		result := baselineResult{title: title, url: canonicalURL, snippet: snippet, publishedAt: publishedAt}
		if candidate.Score != nil {
			value := *candidate.Score
			result.score = &value
		}
		results = append(results, result)
	}
	if count > 0 && len(results) == 0 {
		return nil, invalidProviderResponse(errors.New("provider returned no usable public URL"))
	}
	return results, nil
}

func reserveProviderMetadata(total *int, values ...string) error {
	for _, value := range values {
		if len(value) > MaxProviderMetadataBytes-*total {
			return errors.New("provider metadata exceeds its resource limit")
		}
		*total += len(value)
	}
	return nil
}

func normalizeProviderText(raw string, maximum int) (string, error) {
	if len(raw) > maximum || !utf8.ValidString(raw) {
		return "", errors.New("invalid provider text")
	}
	normalized := strings.TrimSpace(raw)
	if containsControl(normalized) {
		return "", errors.New("invalid provider text")
	}
	return strings.Clone(normalized), nil
}

func normalizeProviderURL(raw string) (string, error) {
	if len(raw) == 0 || len(raw) > models.MaxSearchURLBytes || !utf8.ValidString(raw) || containsControl(raw) {
		return "", errors.New("invalid provider URL")
	}
	canonical, _, err := publicnet.NormalizeHTTPURL(raw, nil, false)
	if err != nil || len(canonical) > models.MaxSearchURLBytes {
		return "", errors.New("invalid provider URL")
	}
	return strings.Clone(canonical), nil
}

func normalizeProviderTime(source *time.Time) (*time.Time, error) {
	if source == nil {
		return nil, nil
	}
	if source.IsZero() {
		return nil, errors.New("provider published time is invalid")
	}
	canonical := source.UTC().Round(0)
	if _, err := canonical.MarshalJSON(); err != nil {
		return nil, errors.New("provider published time is invalid")
	}
	return &canonical, nil
}

func projectBaseline(source []baselineResult, domains []string, deduplicate bool, limit int) ([]models.SearchResult, int) {
	seenURLs := make(map[string]struct{}, len(source))
	candidates := make([]baselineResult, 0, len(source))
	deduplicated := 0
	for _, candidate := range source {
		if !matchesSearchDomains(candidate.url, domains) {
			continue
		}
		if _, duplicate := seenURLs[candidate.url]; duplicate {
			deduplicated++
			continue
		}
		seenURLs[candidate.url] = struct{}{}
		candidates = append(candidates, candidate)
	}
	if deduplicate {
		var removed int
		candidates, removed = collapseSimhashComponents(candidates)
		deduplicated += removed
	}
	results := make([]models.SearchResult, 0, min(len(candidates), limit))
	for _, candidate := range candidates {
		if len(results) >= limit {
			break
		}
		result := models.SearchResult{
			Rank:               len(results) + 1,
			Title:              candidate.title,
			URL:                candidate.url,
			Snippet:            candidate.snippet,
			VerificationStatus: models.SearchVerificationNotChecked,
		}
		if candidate.score != nil {
			value := *candidate.score
			result.Score = &value
		}
		if candidate.publishedAt != nil {
			value := *candidate.publishedAt
			result.PublishedAt = &value
		}
		results = append(results, result)
	}
	return results, deduplicated
}

// collapseSimhashComponents computes the complete near-duplicate graph before
// applying the public limit. Connected components make syndicated-copy
// folding transitive; the earliest provider-ranked member is the winner.
func collapseSimhashComponents(source []baselineResult) ([]baselineResult, int) {
	if len(source) < 2 {
		return source, 0
	}
	fingerprints := make([]uint64, len(source))
	parents := make([]int, len(source))
	for index := range source {
		fingerprints[index] = searchResultFingerprint(source[index])
		parents[index] = index
	}
	var find func(int) int
	find = func(index int) int {
		if parents[index] != index {
			parents[index] = find(parents[index])
		}
		return parents[index]
	}
	union := func(left, right int) {
		leftRoot := find(left)
		rightRoot := find(right)
		if leftRoot == rightRoot {
			return
		}
		if leftRoot < rightRoot {
			parents[rightRoot] = leftRoot
		} else {
			parents[leftRoot] = rightRoot
		}
	}
	for left := 0; left < len(source); left++ {
		if fingerprints[left] == 0 {
			continue
		}
		for right := left + 1; right < len(source); right++ {
			if fingerprints[right] != 0 && simhash.Distance(fingerprints[left], fingerprints[right]) <= simhashDedupDistance {
				union(left, right)
			}
		}
	}
	winners := make([]baselineResult, 0, len(source))
	for index := range source {
		if find(index) == index {
			winners = append(winners, source[index])
		}
	}
	return winners, len(source) - len(winners)
}

func matchesSearchDomains(rawURL string, domains []string) bool {
	if len(domains) == 0 {
		return true
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	for _, domain := range domains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

func searchResultFingerprint(result baselineResult) uint64 {
	text := strings.TrimSpace(result.title + " " + result.snippet)
	if text == "" {
		return 0
	}
	return simhash.Fingerprint(strings.ToLower(text))
}

func baselineKey(providerName string, query ProviderQuery) [32]byte {
	digest := sha256.New()
	_, _ = digest.Write([]byte(searchBaselineCacheVersion))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(providerName))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(query.Text))
	var options [9]byte
	binary.BigEndian.PutUint64(options[:8], uint64(query.Limit))
	options[8] = byte(query.Freshness)
	_, _ = digest.Write(options[:])
	var key [32]byte
	copy(key[:], digest.Sum(nil))
	return key
}

func invalidProviderResponse(cause error) error {
	return NewProviderError(ProviderErrorInvalidResponse, 0, cause)
}

func publicProviderError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return searchTimeout(NewProviderError(ProviderErrorTimeout, 0, err))
	}
	var failure *ProviderError
	if !errors.As(err, &failure) {
		failure = NewProviderError(ProviderErrorUpstream, 0, err)
	}
	switch failure.Kind {
	case ProviderErrorAuthentication:
		return searchUnavailable("search is unavailable", failure)
	case ProviderErrorRateLimited:
		return models.NewScrapeError(models.ErrCodeRateLimited, "search rate limited", failure)
	case ProviderErrorTimeout:
		return searchTimeout(failure)
	case ProviderErrorUpstream, ProviderErrorInvalidResponse:
		return models.NewScrapeError(models.ErrCodeSearchFailed, "search failed", failure)
	default:
		return models.NewScrapeError(models.ErrCodeSearchFailed, "search failed", NewProviderError(ProviderErrorUpstream, 0, failure))
	}
}

func invalidSearchInput(message string) error {
	return models.NewScrapeError(models.ErrCodeInvalidInput, message, nil)
}

func searchUnavailable(message string, cause error) error {
	return models.NewScrapeError(models.ErrCodeSearchUnavailable, message, cause)
}

func searchTimeout(cause error) error {
	return models.NewScrapeError(models.ErrCodeTimeout, "search timed out", cause)
}

func elapsedMilliseconds(startedAt, finishedAt time.Time) int64 {
	if finishedAt.Before(startedAt) {
		return 0
	}
	return finishedAt.Sub(startedAt).Milliseconds()
}

func containsControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}

func isNilProvider(provider Provider) bool {
	if provider == nil {
		return true
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func isNilServiceOption(option ServiceOption) bool {
	if option == nil {
		return true
	}
	value := reflect.ValueOf(option)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
