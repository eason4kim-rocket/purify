package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"reflect"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/publicnet"
	"github.com/use-agent/purify/verify/eav"
)

// SearchService is the transport-neutral Search orchestration boundary.
type SearchService interface {
	Search(context.Context, *models.SearchRequest) (*models.SearchResponse, error)
}

// SearchResponseEncoder lets a production Search service account for final
// JSON encoding in its own shared CPU/resource budget. Services that do not
// implement it use the bounded four-slot fallback below.
type SearchResponseEncoder interface {
	EncodeSearchResponse(context.Context, *models.SearchResponse) ([]byte, error)
}

// SearchRateLimiter consumes one weighted charge for a decoded Search
// request. It is deliberately invoked once: Search must not also pass through
// the fixed-cost middleware used by the other protected routes.
type SearchRateLimiter interface {
	Allow(*gin.Context, int) bool
}

const (
	// MinSearchRequestCost is the one-token baseline capability boundary.
	// Expensive opt-in requests remain subject to their full atomic charge.
	MinSearchRequestCost = 1
	// MaxSearchRequestCost is the trust maximum: two candidate-fanout tokens,
	// two relevance tokens, forty trust tokens, five verify tokens, and ten
	// schema-extraction tokens. Trust content reuses its already-charged fetch.
	MaxSearchRequestCost = 59
)

var fallbackSearchEncodingSlots = make(chan struct{}, 4)

// Search returns the HTTP adapter for POST /api/v1/search. Direct users get
// the same validation and resource boundaries without an implicit limiter;
// the API router supplies its shared per-identity limiter through
// SearchWithRateLimiter.
func Search(service SearchService) gin.HandlerFunc {
	return SearchWithRateLimiter(service, nil)
}

// SearchWithRateLimiter returns a Search handler using the supplied shared
// identity limiter for the endpoint's weighted, single token-bucket charge.
func SearchWithRateLimiter(service SearchService, limiter SearchRateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		if isNilSearchService(service) {
			respondSearchError(c, http.StatusServiceUnavailable, models.ErrCodeSearchUnavailable, "search is unavailable")
			return
		}
		request, err := decodeSearchRequest(c)
		if err != nil {
			if limiter != nil && !limiter.Allow(c, 1) {
				respondSearchError(c, http.StatusTooManyRequests, models.ErrCodeRateLimited, "search rate limited")
				return
			}
			var maximumBytesError *http.MaxBytesError
			if errors.As(err, &maximumBytesError) {
				respondSearchError(c, http.StatusRequestEntityTooLarge, models.ErrCodeInvalidInput, "search request is too large")
				return
			}
			respondSearchError(c, http.StatusBadRequest, models.ErrCodeInvalidInput, "invalid search request")
			return
		}

		if limiter != nil && !limiter.Allow(c, searchRequestCost(request)) {
			respondSearchError(c, http.StatusTooManyRequests, models.ErrCodeRateLimited, "search rate limited")
			return
		}

		resolved := models.ResolveSearchDefaults(request.Ranking, request.Limit, request.Timeout)
		taskContext, cancel := context.WithTimeout(c.Request.Context(), time.Duration(resolved.TimeoutSeconds)*time.Second)
		defer cancel()
		if err := taskContext.Err(); err != nil {
			respondSearchError(c, http.StatusGatewayTimeout, models.ErrCodeTimeout, "search timed out")
			return
		}

		response, err := service.Search(taskContext, request)
		if err != nil {
			if contextErr := taskContext.Err(); contextErr != nil {
				respondSearchError(c, http.StatusGatewayTimeout, models.ErrCodeTimeout, "search timed out")
				return
			}
			status, code, message := mapSearchError(err)
			respondSearchError(c, status, code, message)
			return
		}
		if err := taskContext.Err(); err != nil {
			respondSearchError(c, http.StatusGatewayTimeout, models.ErrCodeTimeout, "search timed out")
			return
		}
		if response == nil {
			respondSearchError(c, http.StatusInternalServerError, models.ErrCodeInternal, "internal search failure")
			return
		}
		if !response.Success || response.Error != nil {
			respondSearchError(c, http.StatusInternalServerError, models.ErrCodeInternal, "internal search failure")
			return
		}

		// Never expose a nullable results collection, including when a service
		// returns the zero value. Clone the envelope so transport normalization
		// does not mutate service-owned state.
		normalized := *response
		if normalized.Results == nil {
			normalized.Results = []models.SearchResult{}
		}
		if err := validateSearchResponseForRequest(request, &normalized); err != nil {
			respondSearchError(c, http.StatusInternalServerError, models.ErrCodeInternal, "internal search failure")
			return
		}
		encoded, err := encodeSearchResponse(taskContext, service, &normalized)
		if err != nil {
			status, code, message := mapSearchError(err)
			respondSearchError(c, status, code, message)
			return
		}
		c.Data(http.StatusOK, "application/json; charset=utf-8", encoded)
	}
}

// validateSearchResponseForRequest treats the Search service as a typed trust
// boundary. Additive ranking diagnostics are accepted only when the request
// explicitly selected the matching mode and their presence/status matrix is
// internally self-consistent. Trust execution remains unavailable until R-8,
// so a successful trust envelope is never a valid R-5 service response.
func validateSearchResponseForRequest(request *models.SearchRequest, response *models.SearchResponse) error {
	if request == nil || response == nil || response.Results == nil {
		return errors.New("search response is invalid")
	}
	wantQuery := strings.Join(strings.Fields(request.Query), " ")
	if response.Query != wantQuery {
		return errors.New("search response query does not match the request")
	}
	if response.Deduplicated < 0 || response.DroppedStale < 0 || len(response.Results) > models.MaxSearchLimit {
		return errors.New("search response counters are invalid")
	}
	resolved := models.ResolveSearchDefaults(request.Ranking, request.Limit, request.Timeout)
	if resolved.Limit < 1 || len(response.Results) > resolved.Limit {
		return errors.New("search response exceeds the requested limit")
	}
	hasSchema := len(request.Schema) > 0
	hasHeavyCapability := request.IncludeContent || request.Verify || hasSchema
	hasErrors := false
	seenURLs := make(map[string]struct{}, len(response.Results))
	seenIdentities := make(map[string]struct{}, len(response.Results))
	for index, result := range response.Results {
		if result.Rank != index+1 {
			return errors.New("search result rank is invalid")
		}
		if err := validateSearchResultForRequest(request, result, index, hasSchema, hasHeavyCapability, seenURLs, seenIdentities); err != nil {
			return err
		}
		if !models.ValidSearchResultEnrichmentFlow(models.SearchEnrichmentRequirements{
			IncludeContent: request.IncludeContent,
			Verify:         request.Verify,
			Extract:        hasSchema,
			AttemptRequired: hasHeavyCapability && models.SearchResultEnrichmentAttemptRequired(
				resolved.Limit, index, response.Deduplicated, response.DroppedStale,
			),
		}, result) {
			return errors.New("search result enrichment flow is invalid")
		}
		hasErrors = hasErrors || len(result.Errors) > 0
	}
	if response.Partial != hasErrors {
		return errors.New("search response partial status is invalid")
	}
	if err := validateSearchResponseTiming(response.Timing); err != nil {
		return err
	}

	switch resolved.Ranking {
	case models.SearchRankingProvider:
		if response.Ranking != nil || response.Timing.RerankMs != nil {
			return errors.New("provider response contains ranking diagnostics")
		}
		for _, result := range response.Results {
			if result.Ranking != nil {
				return errors.New("provider result contains ranking diagnostics")
			}
		}
		return nil
	case models.SearchRankingRelevance:
		return validateRelevanceSearchResponse(response)
	case models.SearchRankingTrust:
		return errors.New("trust response capability is unavailable")
	default:
		return errors.New("search response mode is invalid")
	}
}

func validateSearchResultForRequest(
	request *models.SearchRequest,
	result models.SearchResult,
	index int,
	hasSchema bool,
	hasHeavyCapability bool,
	seenURLs map[string]struct{},
	seenIdentities map[string]struct{},
) error {
	if len(result.Title) > models.MaxSearchResultTitleBytes || !utf8.ValidString(result.Title) ||
		result.Title != strings.TrimSpace(result.Title) || strings.IndexFunc(result.Title, unicode.IsControl) >= 0 {
		return errors.New("search result title is invalid")
	}
	if result.Snippet != "" && (len(result.Snippet) > models.MaxSearchResultSnippetBytes || !utf8.ValidString(result.Snippet) ||
		result.Snippet != strings.TrimSpace(result.Snippet) || strings.IndexFunc(result.Snippet, unicode.IsControl) >= 0) {
		return errors.New("search result snippet is invalid")
	}
	if result.Content != "" && (len(result.Content) > models.MaxSearchResultContentBytes ||
		!utf8.ValidString(result.Content) || strings.TrimSpace(result.Content) == "") {
		return errors.New("search result content is invalid")
	}
	if len(result.Data) > models.MaxSearchResultDataBytes || (len(result.Data) > 0 && !json.Valid(result.Data)) {
		return errors.New("search result data is invalid")
	}
	canonicalURL, _, err := publicnet.NormalizeHTTPURL(result.URL, nil, false)
	if err != nil || canonicalURL != result.URL || len(result.URL) > models.MaxSearchURLBytes {
		return errors.New("search result URL is invalid")
	}
	if _, duplicate := seenURLs[canonicalURL]; duplicate {
		return errors.New("search response contains a duplicate result URL")
	}
	seenURLs[canonicalURL] = struct{}{}
	effectiveIdentity := canonicalURL
	if result.FinalURL != "" {
		canonicalFinalURL, _, normalizeErr := publicnet.NormalizeHTTPURL(result.FinalURL, nil, false)
		if normalizeErr != nil || canonicalFinalURL != result.FinalURL || len(result.FinalURL) > models.MaxSearchURLBytes {
			return errors.New("search result final URL is invalid")
		}
		effectiveIdentity = canonicalFinalURL
	}
	if _, duplicate := seenIdentities[effectiveIdentity]; duplicate {
		return errors.New("search response contains a duplicate effective result identity")
	}
	seenIdentities[effectiveIdentity] = struct{}{}
	if result.Score != nil && (math.IsNaN(*result.Score) || math.IsInf(*result.Score, 0) || *result.Score < 0 || *result.Score > 1) {
		return errors.New("search result provider score is invalid")
	}
	switch result.VerificationStatus {
	case models.SearchVerificationNotChecked, models.SearchVerificationVerified,
		models.SearchVerificationMismatch, models.SearchVerificationUnavailable:
	default:
		return errors.New("search result verification status is invalid")
	}
	if !request.IncludeContent && result.Content != "" {
		return errors.New("search result contains unrequested content")
	}
	if !request.Verify && (result.VerificationStatus != models.SearchVerificationNotChecked || result.Verified != nil ||
		result.Evidence != nil || result.Receipt != "") {
		return errors.New("search result contains unrequested verification")
	}
	if !hasSchema && searchResultHasExtraction(result) {
		return errors.New("search result contains unrequested extraction")
	}
	if !hasHeavyCapability && (result.FinalURL != "" || len(result.Errors) > 0) {
		return errors.New("search result contains unrequested enrichment")
	}
	if index >= models.MaxSearchHeavyResults && searchResultHasEnrichment(result) {
		return errors.New("search result contains enrichment beyond the top-five limit")
	}
	seenStages := make(map[models.SearchResultErrorStage]struct{}, len(result.Errors))
	for _, resultError := range result.Errors {
		if strings.TrimSpace(resultError.Code) == "" || strings.TrimSpace(resultError.Message) == "" {
			return errors.New("search result contains an incomplete error")
		}
		if _, duplicate := seenStages[resultError.Stage]; duplicate {
			return errors.New("search result repeats an error stage")
		}
		seenStages[resultError.Stage] = struct{}{}
		switch resultError.Stage {
		case models.SearchResultStageFetch:
			if !hasHeavyCapability {
				return errors.New("search result contains an unrequested fetch error")
			}
		case models.SearchResultStageVerify:
			if !request.Verify {
				return errors.New("search result contains an unrequested verification error")
			}
		case models.SearchResultStageExtract:
			if !hasSchema {
				return errors.New("search result contains an unrequested extraction error")
			}
		default:
			return errors.New("search result contains an invalid error stage")
		}
	}
	return nil
}

func searchResultHasExtraction(result models.SearchResult) bool {
	return len(result.Data) > 0 || result.Basis != nil || result.Receipts != nil || result.UnlocatedRate != nil ||
		result.Extractor != nil || result.LLMUsage != nil || len(result.Violations) > 0
}

func searchResultHasEnrichment(result models.SearchResult) bool {
	return result.FinalURL != "" || result.Content != "" || result.Verified != nil ||
		result.VerificationStatus != models.SearchVerificationNotChecked || result.Evidence != nil || result.Receipt != "" ||
		searchResultHasExtraction(result) || len(result.Errors) > 0
}

func validateRelevanceSearchResponse(response *models.SearchResponse) error {
	if response == nil || response.Ranking == nil || response.Ranking.Mode != models.SearchRankingRelevance ||
		response.Timing.RerankMs == nil {
		return errors.New("relevance response envelope is invalid")
	}
	ranking := response.Ranking
	if ranking.CandidateCount < len(response.Results) || ranking.CandidateCount > models.MaxSearchLimit {
		return errors.New("relevance response counters are invalid")
	}
	switch ranking.Status {
	case models.SearchRankingApplied:
		if ranking.DegradedReason != "" {
			return errors.New("applied relevance response contains a degraded reason")
		}
		if ranking.CandidateCount == 0 && (len(response.Results) != 0 || *response.Timing.RerankMs != 0) {
			return errors.New("empty relevance response is invalid")
		}
		return validateAppliedRelevanceResults(response.Results)
	case models.SearchRankingDegraded:
		if ranking.DegradedReason != models.SearchRankingReasonRerankerFailed || ranking.CandidateCount == 0 {
			return errors.New("degraded relevance response reason is invalid")
		}
		for _, result := range response.Results {
			if result.Ranking != nil {
				return errors.New("degraded relevance result contains ranking diagnostics")
			}
		}
		return nil
	default:
		return errors.New("relevance response status is invalid")
	}
}

func validateAppliedRelevanceResults(results []models.SearchResult) error {
	providerRanks := make(map[int]struct{}, len(results))
	for index, result := range results {
		if result.Ranking == nil || result.Ranking.RelevanceScore == nil {
			return errors.New("applied relevance result shape is invalid")
		}
		providerRank := result.Ranking.ProviderRank
		score := *result.Ranking.RelevanceScore
		if providerRank < 1 || providerRank > models.MaxSearchLimit || math.IsNaN(score) || math.IsInf(score, 0) || score < 0 || score > 1 {
			return errors.New("applied relevance result value is invalid")
		}
		if _, duplicate := providerRanks[providerRank]; duplicate {
			return errors.New("applied relevance result provider rank is duplicated")
		}
		providerRanks[providerRank] = struct{}{}
		if index == 0 {
			continue
		}
		previous := results[index-1]
		previousScore := *previous.Ranking.RelevanceScore
		if score > previousScore || (score == previousScore && providerRank < previous.Ranking.ProviderRank) ||
			(score == previousScore && providerRank == previous.Ranking.ProviderRank && result.URL < previous.URL) {
			return errors.New("applied relevance result order is invalid")
		}
	}
	return nil
}

func validateSearchResponseTiming(timing models.SearchTimingInfo) error {
	if timing.TotalMs < 0 || timing.ProviderMs < 0 || timing.EnrichmentMs < 0 {
		return errors.New("search response timing is invalid")
	}
	additional := int64(0)
	for _, value := range []*int64{timing.RerankMs} {
		if value == nil {
			continue
		}
		if *value < 0 || additional > math.MaxInt64-*value {
			return errors.New("search response timing is invalid")
		}
		additional += *value
	}
	if timing.ProviderMs > math.MaxInt64-timing.EnrichmentMs ||
		timing.ProviderMs+timing.EnrichmentMs > math.MaxInt64-additional ||
		timing.TotalMs < timing.ProviderMs+timing.EnrichmentMs+additional {
		return errors.New("search response timing is invalid")
	}
	return nil
}

func decodeSearchRequest(c *gin.Context) (*models.SearchRequest, error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, models.MaxSearchRequestBytes)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(body) {
		return nil, errors.New("search request is not valid UTF-8")
	}
	if err := validateRawSearchRequest(body); err != nil {
		return nil, err
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var request *models.SearchRequest
	if err := decoder.Decode(&request); err != nil {
		return nil, err
	}
	if request == nil {
		return nil, errors.New("search request must be a JSON object")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("search request contains multiple JSON values")
		}
		return nil, err
	}
	if err := binding.Validator.ValidateStruct(request); err != nil {
		return nil, err
	}
	if err := validateSearchExpectedSubject(request.ExpectedSubject); err != nil {
		return nil, err
	}
	resolved := models.ResolveSearchDefaults(request.Ranking, request.Limit, request.Timeout)
	switch resolved.Ranking {
	case models.SearchRankingTrust:
		if request.ExpectedSubject == nil || resolved.Limit > models.MaxSearchTrustLimit {
			return nil, errors.New("trust search request is invalid")
		}
	case models.SearchRankingProvider, models.SearchRankingRelevance:
		if request.ExpectedSubject != nil {
			return nil, errors.New("search expected subject requires trust ranking")
		}
	}
	return request, nil
}

func validateSearchExpectedSubject(subject *models.SubjectSpec) error {
	if subject == nil {
		return nil
	}
	if len(subject.Name) > eav.MaxSubjectBytes || len(subject.Hint) > eav.MaxHintBytes ||
		!utf8.ValidString(subject.Name) || !utf8.ValidString(subject.Hint) ||
		strings.IndexFunc(subject.Name, unicode.IsControl) >= 0 || strings.IndexFunc(subject.Hint, unicode.IsControl) >= 0 ||
		strings.TrimSpace(subject.Name) == "" || eav.Normalize(subject.Name) == "" {
		return errors.New("search expected subject is invalid")
	}
	return nil
}

var searchRequestFieldNames = map[string]struct{}{
	"query": {}, "limit": {}, "domains": {}, "freshness": {}, "ranking": {}, "expected_subject": {},
	"include_content": {}, "verify": {}, "deduplicate": {}, "schema": {}, "engine": {}, "llm_api_key": {},
	"llm_model": {}, "llm_base_url": {}, "timeout": {},
}

var searchSubjectFieldNames = map[string]struct{}{"name": {}, "hint": {}}

// validateRawSearchRequest closes encoding/json's duplicate-key, last-wins,
// and case-insensitive field matching before ranking can affect cost or
// capability selection. Schema contents remain caller-defined, but duplicate
// keys are rejected recursively throughout the complete request.
func validateRawSearchRequest(body []byte) error {
	if err := rejectDuplicateSearchJSONFields(body); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	var fields map[string]json.RawMessage
	if err := decoder.Decode(&fields); err != nil || fields == nil {
		return errors.New("search request must be a JSON object")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("search request must contain exactly one JSON object")
	}
	for name := range fields {
		if _, allowed := searchRequestFieldNames[name]; !allowed {
			return errors.New("search request contains an invalid field")
		}
	}
	if raw, present := fields["ranking"]; present {
		var ranking string
		if err := json.Unmarshal(raw, &ranking); err != nil {
			return errors.New("search ranking is invalid")
		}
		switch models.SearchRankingMode(ranking) {
		case models.SearchRankingProvider, models.SearchRankingRelevance, models.SearchRankingTrust:
		default:
			return errors.New("search ranking is invalid")
		}
	}
	if raw, present := fields["expected_subject"]; present {
		var subject map[string]json.RawMessage
		if err := json.Unmarshal(raw, &subject); err != nil || subject == nil {
			return errors.New("search expected subject is invalid")
		}
		for name := range subject {
			if _, allowed := searchSubjectFieldNames[name]; !allowed {
				return errors.New("search expected subject contains an invalid field")
			}
		}
		name, present := subject["name"]
		if !present || !rawJSONString(name) {
			return errors.New("search expected subject name is invalid")
		}
		if hint, present := subject["hint"]; present && !rawJSONString(hint) {
			return errors.New("search expected subject hint is invalid")
		}
	}
	for _, name := range []string{"limit", "timeout"} {
		raw, present := fields[name]
		if !present {
			continue
		}
		var value int
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil || value < 1 {
			return errors.New("search numeric option is invalid")
		}
	}
	return nil
}

func rawJSONString(raw json.RawMessage) bool {
	var value string
	return json.Unmarshal(raw, &value) == nil && len(bytes.TrimSpace(raw)) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func rejectDuplicateSearchJSONFields(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := consumeUniqueSearchJSONValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("search request must contain exactly one JSON value")
	}
	return nil
}

func consumeUniqueSearchJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 256 {
		return errors.New("search request JSON nesting is too deep")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("search request object key is invalid")
			}
			if _, duplicate := seen[key]; duplicate {
				return errors.New("search request contains a duplicate field")
			}
			seen[key] = struct{}{}
			if err := consumeUniqueSearchJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("search request contains an invalid object")
		}
	case '[':
		for decoder.More() {
			if err := consumeUniqueSearchJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("search request contains an invalid array")
		}
	default:
		return errors.New("search request contains an invalid delimiter")
	}
	return nil
}

func searchRequestCost(request *models.SearchRequest) int {
	if request == nil {
		return MinSearchRequestCost
	}
	resolved := models.ResolveSearchDefaults(request.Ranking, request.Limit, request.Timeout)
	limit := resolved.Limit
	// decodeSearchRequest validates public ranges. Keep this helper bounded for
	// direct package callers as well.
	if limit < 1 {
		limit = 1
	}
	if limit > models.MaxSearchLimit {
		limit = models.MaxSearchLimit
	}
	candidateLimit := limit
	if resolved.Ranking == models.SearchRankingRelevance || resolved.Ranking == models.SearchRankingTrust {
		candidateLimit = models.MaxSearchLimit
	}
	heavyResults := min(limit, models.MaxSearchHeavyResults)
	cost := (candidateLimit + 9) / 10
	if resolved.Ranking == models.SearchRankingRelevance || resolved.Ranking == models.SearchRankingTrust {
		cost += 2
	}
	if resolved.Ranking == models.SearchRankingTrust {
		cost += 40
	}
	if request.IncludeContent && resolved.Ranking != models.SearchRankingTrust {
		cost += heavyResults
	}
	if request.Verify {
		cost += heavyResults
	}
	if len(request.Schema) > 0 {
		cost += 2 * heavyResults
	}
	if cost < MinSearchRequestCost {
		return MinSearchRequestCost
	}
	if cost > MaxSearchRequestCost {
		return MaxSearchRequestCost
	}
	return cost
}

func encodeSearchResponse(ctx context.Context, service SearchService, response *models.SearchResponse) ([]byte, error) {
	if encoder, ok := service.(SearchResponseEncoder); ok {
		select {
		case fallbackSearchEncodingSlots <- struct{}{}:
			defer func() { <-fallbackSearchEncodingSlots }()
		case <-ctx.Done():
			return nil, searchEncodingTimeout(ctx.Err())
		}
		if err := ctx.Err(); err != nil {
			return nil, searchEncodingTimeout(err)
		}
		preflightOK, err := preflightSearchResponseSize(ctx, response)
		if err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return nil, searchEncodingTimeout(contextErr)
			}
			return nil, err
		}
		if !preflightOK {
			return nil, errors.New("search response exceeds its output budget")
		}
		// Freeze the validated semantics before invoking the optional encoder.
		// The encoder receives a detached copy, while its returned bytes must
		// match the pre-encode canonical snapshot exactly. The shared slot bounds
		// the retained snapshot/copy/output to four concurrent responses.
		canonical, err := json.Marshal(response)
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, searchEncodingTimeout(contextErr)
		}
		if err != nil || len(canonical) == 0 || len(canonical) > models.MaxSearchResponseBytes {
			return nil, errors.New("search response encoding failed")
		}
		var detached models.SearchResponse
		if err := json.Unmarshal(canonical, &detached); err != nil {
			return nil, errors.New("search response encoding failed")
		}
		encoded, err := encoder.EncodeSearchResponse(ctx, &detached)
		if err := ctx.Err(); err != nil {
			return nil, searchEncodingTimeout(err)
		}
		if err != nil {
			return nil, err
		}
		if len(encoded) == 0 || len(encoded) > models.MaxSearchResponseBytes {
			return nil, errors.New("search response exceeds its output budget")
		}
		if !json.Valid(encoded) {
			return nil, errors.New("search response encoder returned invalid JSON")
		}
		if !bytes.Equal(encoded, canonical) {
			return nil, errors.New("search response encoder changed the canonical response")
		}
		if err := ctx.Err(); err != nil {
			return nil, searchEncodingTimeout(err)
		}
		return encoded, nil
	}
	return encodeSearchResponseFallback(ctx, response)
}

func encodeSearchResponseFallback(ctx context.Context, response *models.SearchResponse) ([]byte, error) {
	select {
	case fallbackSearchEncodingSlots <- struct{}{}:
		defer func() { <-fallbackSearchEncodingSlots }()
	case <-ctx.Done():
		return nil, searchEncodingTimeout(ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return nil, searchEncodingTimeout(err)
	}
	preflightOK, err := preflightSearchResponseSize(ctx, response)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, searchEncodingTimeout(contextErr)
		}
		return nil, err
	}
	if !preflightOK {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, searchEncodingTimeout(contextErr)
		}
		return nil, errors.New("search response exceeds its output budget")
	}
	if err := ctx.Err(); err != nil {
		return nil, searchEncodingTimeout(err)
	}
	encoded, err := json.Marshal(response)
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, searchEncodingTimeout(contextErr)
	}
	if err != nil {
		return nil, err
	}
	if len(encoded) > models.MaxSearchResponseBytes {
		return nil, errors.New("search response exceeds its output budget")
	}
	return encoded, nil
}

func preflightSearchResponseSize(ctx context.Context, response *models.SearchResponse) (bool, error) {
	if response == nil {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	remaining := models.MaxSearchResponseBytes
	if !reserveSearchResponseBytes(&remaining, 2) {
		return false, nil
	}
	fields := 0
	var preflightErr error
	addField := func(name string, value any) bool {
		if err := ctx.Err(); err != nil {
			preflightErr = err
			return false
		}
		encodedName, err := json.Marshal(name)
		if err != nil {
			preflightErr = err
			return false
		}
		encodedValue, err := json.Marshal(value)
		if err != nil {
			preflightErr = err
			return false
		}
		if err := ctx.Err(); err != nil {
			preflightErr = err
			return false
		}
		if fields > 0 && !reserveSearchResponseBytes(&remaining, 1) {
			return false
		}
		if !reserveSearchResponseBytes(&remaining, len(encodedName)) ||
			!reserveSearchResponseBytes(&remaining, 1) ||
			!reserveSearchResponseBytes(&remaining, len(encodedValue)) {
			return false
		}
		fields++
		return true
	}

	valid := addField("success", response.Success) &&
		addField("query", response.Query) &&
		addField("results", response.Results) &&
		addField("deduplicated", response.Deduplicated) &&
		addField("dropped_stale", response.DroppedStale) &&
		addField("partial", response.Partial) &&
		addField("timing", response.Timing)
	if response.Ranking != nil {
		valid = valid && addField("ranking", response.Ranking)
	}
	if response.Error != nil {
		valid = valid && addField("error", response.Error)
	}
	if preflightErr != nil {
		return false, preflightErr
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return valid, nil
}

func reserveSearchResponseBytes(remaining *int, requested int) bool {
	if remaining == nil || requested < 0 || requested > *remaining {
		return false
	}
	*remaining -= requested
	return true
}

func searchEncodingTimeout(cause error) error {
	return models.NewScrapeError(models.ErrCodeTimeout, "search timed out", cause)
}

func respondSearchError(c *gin.Context, status int, code, message string) {
	c.JSON(status, models.SearchResponse{
		Success: false,
		Results: []models.SearchResult{},
		Error:   &models.ErrorDetail{Code: code, Message: message},
	})
}

func mapSearchError(err error) (int, string, string) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, models.ErrCodeTimeout, "search timed out"
	}
	var scrapeError *models.ScrapeError
	if errors.As(err, &scrapeError) {
		switch scrapeError.Code {
		case models.ErrCodeInvalidInput:
			return http.StatusBadRequest, models.ErrCodeInvalidInput, "invalid search request"
		case models.ErrCodeRateLimited:
			return http.StatusTooManyRequests, models.ErrCodeRateLimited, "search rate limited"
		case models.ErrCodeSearchFailed:
			return http.StatusBadGateway, models.ErrCodeSearchFailed, "search failed"
		case models.ErrCodeSearchUnavailable:
			return http.StatusServiceUnavailable, models.ErrCodeSearchUnavailable, "search is unavailable"
		case models.ErrCodeTimeout:
			return http.StatusGatewayTimeout, models.ErrCodeTimeout, "search timed out"
		case models.ErrCodeInternal:
			return http.StatusInternalServerError, models.ErrCodeInternal, "internal search failure"
		}
	}
	return http.StatusInternalServerError, models.ErrCodeInternal, "internal search failure"
}

func isNilSearchService(service SearchService) bool {
	if service == nil {
		return true
	}
	value := reflect.ValueOf(service)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
