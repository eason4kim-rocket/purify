package models

import (
	"encoding/json"
	"time"

	"github.com/use-agent/purify/evidence"
)

const (
	// DefaultSearchLimit is the result count used when limit is omitted.
	DefaultSearchLimit = 10
	// MaxSearchLimit bounds the public result count. Explicit relevance/trust
	// modes use their separately bounded internal candidate fan-out.
	MaxSearchLimit = 20
	// MaxSearchHeavyResults bounds ordinary result enrichment. Trust analysis
	// uses MaxSearchTrustCandidates before final result enrichment.
	MaxSearchHeavyResults = 5
	// MaxSearchDomains bounds caller-supplied post-search domain filters.
	MaxSearchDomains = 20
	// MaxSearchQueryRunes and MaxSearchQueryWords jointly bound natural-language
	// query input. Runes, rather than bytes, keep the public contract Unicode-safe.
	MaxSearchQueryRunes = 400
	MaxSearchQueryWords = 50
	// MaxSearchDomainBytes is the DNS wire-format textual hostname ceiling after
	// IDNA canonicalization.
	MaxSearchDomainBytes = 253
	// MaxSearchURLBytes matches the existing public URL resource boundary.
	MaxSearchURLBytes = 16 << 10
	// MaxSearchSchemaBytes matches structured extraction's schema boundary.
	MaxSearchSchemaBytes = 512 << 10
	// MaxSearchRequestBytes bounds the complete HTTP request body.
	MaxSearchRequestBytes = 1 << 20
	// MaxSearchResponseBytes bounds the complete encoded response.
	MaxSearchResponseBytes = 32 << 20
	// Per-result bounds are transport-owned so the Search service and strict
	// clients reject the same impossible upstream shapes.
	MaxSearchResultTitleBytes   = 16 << 10
	MaxSearchResultSnippetBytes = 64 << 10
	MaxSearchResultContentBytes = 4 << 20
	MaxSearchResultDataBytes    = 4 << 20
	MaxSearchResultReceiptBytes = 1 << 20
	// Search LLM settings use the same limits as direct extraction.
	MaxSearchLLMAPIKeyBytes  = 16 << 10
	MaxSearchLLMModelBytes   = 256
	MaxSearchLLMBaseURLBytes = 16 << 10

	DefaultSearchTimeoutSeconds = 30
	// Trust is an explicit heavy mode with a smaller public result limit and
	// a longer outer deadline. Its execution is added in a later card; these
	// constants keep request resolution and the additive wire stable now.
	DefaultSearchTrustLimit          = 5
	MaxSearchTrustLimit              = 5
	MaxSearchTrustCandidates         = 8
	DefaultSearchTrustTimeoutSeconds = 60
	MaxSearchTimeoutSeconds          = 120

	// ErrCodeSearchUnavailable identifies missing or unsafe process-owned
	// Search capability without exposing a provider or credential detail.
	ErrCodeSearchUnavailable = "SEARCH_UNAVAILABLE"
	// ErrCodeSearchFailed identifies a provider-neutral upstream Search failure.
	ErrCodeSearchFailed = "SEARCH_FAILED"
)

// SearchRequest is the payload for POST /api/v1/search. Provider credentials
// are deliberately absent: only caller-owned structured-extraction settings
// may cross the public request boundary.
type SearchRequest struct {
	Query   string   `json:"query" binding:"required"`
	Limit   int      `json:"limit,omitempty" binding:"omitempty,min=1,max=20"`
	Domains []string `json:"domains,omitempty" binding:"omitempty,max=20"`

	// Freshness accepts the provider-neutral aliases parsed by search.
	Freshness string `json:"freshness,omitempty"`
	// Ranking is permanently provider-ordered when omitted. Relevance is an
	// explicit opt-in; trust additionally requires ExpectedSubject.
	Ranking         SearchRankingMode `json:"ranking,omitempty"`
	ExpectedSubject *SubjectSpec      `json:"expected_subject,omitempty"`

	// Heavy capabilities are opt-in. Deduplicate is a pointer so omission can
	// default to true while an explicit false remains distinguishable.
	IncludeContent bool  `json:"include_content,omitempty"`
	Verify         bool  `json:"verify,omitempty"`
	Deduplicate    *bool `json:"deduplicate,omitempty"`

	// An absent schema disables structured extraction. An explicit null, empty,
	// or invalid schema is rejected during request preparation.
	Schema json.RawMessage `json:"schema,omitempty"`

	Engine     string `json:"engine,omitempty" binding:"omitempty,oneof=auto compiled llm"`
	LLMAPIKey  string `json:"llm_api_key,omitempty"`
	LLMModel   string `json:"llm_model,omitempty"`
	LLMBaseURL string `json:"llm_base_url,omitempty"`

	Timeout int `json:"timeout,omitempty" binding:"omitempty,min=1,max=120"`
}

// Defaults applies only semantic defaults and does not validate or normalize
// caller input. IncludeContent and Verify intentionally remain false.
func (request *SearchRequest) Defaults() {
	if request == nil {
		return
	}
	resolved := ResolveSearchDefaults(request.Ranking, request.Limit, request.Timeout)
	request.Limit = resolved.Limit
	if request.Deduplicate == nil {
		value := true
		request.Deduplicate = &value
	}
	request.Timeout = resolved.TimeoutSeconds
	if len(request.Schema) == 0 {
		return
	}
	if request.Engine == "" {
		request.Engine = "auto"
	}
	if request.LLMModel == "" {
		request.LLMModel = "gpt-4o-mini"
	}
	if request.LLMBaseURL == "" {
		request.LLMBaseURL = "https://api.openai.com/v1"
	}
}

// SearchRankingMode selects the provider-ordered baseline or one explicit
// ranking capability. The zero value is intentionally provider-compatible.
type SearchRankingMode string

const (
	SearchRankingProvider  SearchRankingMode = "provider"
	SearchRankingRelevance SearchRankingMode = "relevance"
	SearchRankingTrust     SearchRankingMode = "trust"
)

// SearchResolvedDefaults is the single mode-aware default resolution used by
// models, HTTP, MCP, and the transport-neutral service.
type SearchResolvedDefaults struct {
	Ranking        SearchRankingMode
	Limit          int
	TimeoutSeconds int
}

// ResolveSearchDefaults resolves only omitted limit/timeout values. It never
// validates an enum or mutates the caller's Ranking field.
func ResolveSearchDefaults(ranking SearchRankingMode, limit, timeoutSeconds int) SearchResolvedDefaults {
	resolvedRanking := ranking
	if resolvedRanking == "" {
		resolvedRanking = SearchRankingProvider
	}
	if limit == 0 {
		limit = DefaultSearchLimit
		if resolvedRanking == SearchRankingTrust {
			limit = DefaultSearchTrustLimit
		}
	}
	if timeoutSeconds == 0 {
		timeoutSeconds = DefaultSearchTimeoutSeconds
		if resolvedRanking == SearchRankingTrust {
			timeoutSeconds = DefaultSearchTrustTimeoutSeconds
		}
	}
	return SearchResolvedDefaults{Ranking: resolvedRanking, Limit: limit, TimeoutSeconds: timeoutSeconds}
}

// SearchVerificationStatus distinguishes a negative verification verdict from
// work that was not attempted or could not be completed.
type SearchVerificationStatus string

const (
	SearchVerificationNotChecked  SearchVerificationStatus = "not_checked"
	SearchVerificationVerified    SearchVerificationStatus = "verified"
	SearchVerificationMismatch    SearchVerificationStatus = "mismatch"
	SearchVerificationUnavailable SearchVerificationStatus = "unavailable"
)

// SearchResultErrorStage identifies which optional enrichment stage failed.
type SearchResultErrorStage string

const (
	SearchResultStageFetch   SearchResultErrorStage = "fetch"
	SearchResultStageVerify  SearchResultErrorStage = "verify"
	SearchResultStageExtract SearchResultErrorStage = "extract"
)

// SearchResultError is a stable, sanitized per-result enrichment error.
type SearchResultError struct {
	Stage   SearchResultErrorStage `json:"stage"`
	Code    string                 `json:"code"`
	Message string                 `json:"message"`
}

// SearchResult is one provider-neutral ranked result with optional Purify
// enrichment. Evidence and Receipt refer to snippet verification; Basis and
// Receipts refer to schema-shaped extraction from the same fetched artifact.
type SearchResult struct {
	Rank        int        `json:"rank"`
	Score       *float64   `json:"score,omitempty"`
	Title       string     `json:"title"`
	URL         string     `json:"url"`
	FinalURL    string     `json:"final_url,omitempty"`
	Snippet     string     `json:"snippet,omitempty"`
	PublishedAt *time.Time `json:"published_at,omitempty"`

	Content string `json:"content,omitempty"`

	Verified           *bool                    `json:"verified,omitempty"`
	VerificationStatus SearchVerificationStatus `json:"verification_status"`
	Evidence           *evidence.Anchor         `json:"evidence,omitempty"`
	Receipt            string                   `json:"receipt,omitempty"`

	Data          json.RawMessage    `json:"data,omitempty"`
	Basis         *EvidenceBasis     `json:"basis,omitempty"`
	Receipts      *FieldReceipts     `json:"receipts,omitempty"`
	UnlocatedRate *float64           `json:"unlocated_rate,omitempty"`
	Extractor     *ExtractorMetadata `json:"extractor,omitempty"`
	LLMUsage      *LLMUsage          `json:"llm_usage,omitempty"`
	Violations    []SchemaViolation  `json:"violations,omitempty"`

	Errors []SearchResultError `json:"errors,omitempty"`

	// Ranking appears only for an explicit relevance request that produced a
	// valid relevance score. Provider Score remains independent.
	Ranking *SearchResultRanking `json:"ranking,omitempty"`
}

// SearchEnrichmentRequirements describes the ordered heavy stages selected by
// one request. AttemptRequired is true when every retained result is known to
// belong to the top-five worker pool; larger result sets may contain shifted,
// unattempted tail results after stale pages are removed.
type SearchEnrichmentRequirements struct {
	IncludeContent  bool
	Verify          bool
	Extract         bool
	AttemptRequired bool
}

// SearchResultEnrichmentAttemptRequired reports when response counters prove
// that a retained result originated inside the top-five enrichment pool. The
// duplicate count may include pre-enrichment collapse, so it is used only as a
// conservative upper bound on removals and can never force a tail result into
// the attempted set.
func SearchResultEnrichmentAttemptRequired(limit, resultIndex, deduplicated, droppedStale int) bool {
	if limit < 1 || resultIndex < 0 || deduplicated < 0 || droppedStale < 0 {
		return false
	}
	if limit <= MaxSearchHeavyResults {
		return true
	}
	if resultIndex >= MaxSearchHeavyResults {
		return false
	}
	maximumRemovals := MaxSearchHeavyResults - 1 - resultIndex
	return droppedStale <= maximumRemovals && deduplicated <= maximumRemovals-droppedStale
}

// ValidSearchResultEnrichmentFlow verifies the observable stage order emitted
// by Search's fetch -> verify -> content -> extract worker. It deliberately
// checks only cross-stage shape; transports retain their stricter field and
// request-correlation validation around this shared invariant.
func ValidSearchResultEnrichmentFlow(requirements SearchEnrichmentRequirements, result SearchResult) bool {
	var fetchError, verifyError, extractError *SearchResultError
	for index := range result.Errors {
		resultError := &result.Errors[index]
		slot := &fetchError
		switch resultError.Stage {
		case SearchResultStageFetch:
		case SearchResultStageVerify:
			slot = &verifyError
		case SearchResultStageExtract:
			slot = &extractError
		default:
			return false
		}
		if *slot != nil {
			return false
		}
		*slot = resultError
	}

	verificationTouched := result.VerificationStatus != SearchVerificationNotChecked || result.Verified != nil ||
		result.Evidence != nil || result.Receipt != ""
	extractionTouched := len(result.Data) > 0 || result.Basis != nil || result.Receipts != nil || result.UnlocatedRate != nil ||
		result.Extractor != nil || result.LLMUsage != nil || len(result.Violations) > 0
	if (result.Content != "" && !requirements.IncludeContent) ||
		((verificationTouched || verifyError != nil) && !requirements.Verify) ||
		((extractionTouched || extractError != nil) && !requirements.Extract) {
		return false
	}
	if (verificationTouched || verifyError != nil) && !validSearchVerificationFlow(result, verifyError) {
		return false
	}

	attempted := result.FinalURL != "" || verificationTouched || result.Content != "" || extractionTouched || len(result.Errors) > 0
	if !attempted {
		return !requirements.AttemptRequired
	}
	if result.FinalURL == "" {
		return fetchError != nil && verifyError == nil && extractError == nil &&
			!verificationTouched && result.Content == "" && !extractionTouched
	}
	// A validated final URL is published before every later stage. It can never
	// be the worker's only observable outcome.
	if !verificationTouched && result.Content == "" && !extractionTouched && len(result.Errors) == 0 {
		return false
	}

	if fetchError != nil {
		if result.Content != "" || extractionTouched || extractError != nil {
			return false
		}
		if verificationTouched || verifyError != nil {
			// Verification can precede only a content-stage fetch timeout.
			if !requirements.IncludeContent || verifyError != nil && isSearchVerificationTimeout(*verifyError) {
				return false
			}
		}
		return true
	}

	if requirements.Verify {
		if !verificationTouched || result.Verified == nil || result.VerificationStatus == SearchVerificationNotChecked {
			return false
		}
		if verifyError != nil {
			if result.VerificationStatus != SearchVerificationUnavailable || *result.Verified {
				return false
			}
			if isSearchVerificationTimeout(*verifyError) {
				return result.Content == "" && !extractionTouched && extractError == nil
			}
			if verifyError.Code != ErrCodeEvidenceUnavailable || verifyError.Message != "result verification is unavailable" {
				return false
			}
		} else if result.VerificationStatus == SearchVerificationUnavailable {
			return false
		}
	}

	if requirements.IncludeContent && result.Content == "" {
		return false
	}
	if requirements.Extract {
		if !extractionTouched && extractError == nil {
			return false
		}
		if extractionTouched && extractError != nil &&
			(len(result.Data) == 0 || len(result.Violations) == 0 || extractError.Code != ErrCodeLLMFailure ||
				extractError.Message != "result extraction was partial") {
			return false
		}
	}
	return true
}

func isSearchVerificationTimeout(resultError SearchResultError) bool {
	return resultError.Code == ErrCodeTimeout && resultError.Message == "result verification timed out"
}

func validSearchVerificationFlow(result SearchResult, verifyError *SearchResultError) bool {
	switch result.VerificationStatus {
	case SearchVerificationVerified:
		return result.Verified != nil && *result.Verified && result.Evidence != nil && result.Receipt != "" && verifyError == nil
	case SearchVerificationMismatch:
		return result.Verified != nil && !*result.Verified && result.Evidence == nil && result.Receipt == "" && verifyError == nil
	case SearchVerificationUnavailable:
		return result.Verified != nil && !*result.Verified && result.Evidence == nil && result.Receipt == "" && verifyError != nil &&
			(isSearchVerificationTimeout(*verifyError) ||
				verifyError.Code == ErrCodeEvidenceUnavailable && verifyError.Message == "result verification is unavailable")
	default:
		return result.Verified == nil && result.Evidence == nil && result.Receipt == "" && verifyError == nil
	}
}

// SearchResultRanking is per-result opt-in ranking diagnostics.
type SearchResultRanking struct {
	ProviderRank   int      `json:"provider_rank"`
	RelevanceScore *float64 `json:"relevance_score,omitempty"`
}

// SearchRankingStatus and SearchRankingDegradedReason are strict response
// enums for explicit ranking modes.
type SearchRankingStatus string
type SearchRankingDegradedReason string

const (
	SearchRankingApplied  SearchRankingStatus = "applied"
	SearchRankingDegraded SearchRankingStatus = "degraded"

	SearchRankingReasonRerankerFailed SearchRankingDegradedReason = "reranker_failed"
)

// SearchResponseRanking reports request-global relevance ranking status.
type SearchResponseRanking struct {
	Mode           SearchRankingMode           `json:"mode"`
	Status         SearchRankingStatus         `json:"status"`
	DegradedReason SearchRankingDegradedReason `json:"degraded_reason,omitempty"`
	CandidateCount int                         `json:"candidate_count"`
}

// SearchTimingInfo reports end-to-end, provider, and optional enrichment time.
type SearchTimingInfo struct {
	TotalMs      int64  `json:"total_ms"`
	ProviderMs   int64  `json:"provider_ms"`
	EnrichmentMs int64  `json:"enrichment_ms"`
	RerankMs     *int64 `json:"rerank_ms,omitempty"`
}

// SearchResponse is the stable provider-neutral response envelope. Services
// must initialize Results to a non-nil empty slice when no result is available.
type SearchResponse struct {
	Success      bool                   `json:"success"`
	Query        string                 `json:"query"`
	Results      []SearchResult         `json:"results"`
	Deduplicated int                    `json:"deduplicated"`
	DroppedStale int                    `json:"dropped_stale"`
	Partial      bool                   `json:"partial"`
	Timing       SearchTimingInfo       `json:"timing"`
	Ranking      *SearchResponseRanking `json:"ranking,omitempty"`
	Error        *ErrorDetail           `json:"error,omitempty"`
}
