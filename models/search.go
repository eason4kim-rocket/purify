package models

import (
	"encoding/json"
	"time"

	"github.com/use-agent/purify/evidence"
)

const (
	// DefaultSearchLimit is the result count used when limit is omitted.
	DefaultSearchLimit = 10
	// MaxSearchLimit bounds both the public result count and provider fan-out.
	MaxSearchLimit = 20
	// MaxSearchHeavyResults bounds page fetching, verification, and extraction.
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
	// Search LLM settings use the same limits as direct extraction.
	MaxSearchLLMAPIKeyBytes  = 16 << 10
	MaxSearchLLMModelBytes   = 256
	MaxSearchLLMBaseURLBytes = 16 << 10

	DefaultSearchTimeoutSeconds = 30
	MaxSearchTimeoutSeconds     = 120

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
	if request.Limit == 0 {
		request.Limit = DefaultSearchLimit
	}
	if request.Deduplicate == nil {
		value := true
		request.Deduplicate = &value
	}
	if request.Timeout == 0 {
		request.Timeout = DefaultSearchTimeoutSeconds
	}
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
}

// SearchTimingInfo reports end-to-end, provider, and optional enrichment time.
type SearchTimingInfo struct {
	TotalMs      int64 `json:"total_ms"`
	ProviderMs   int64 `json:"provider_ms"`
	EnrichmentMs int64 `json:"enrichment_ms"`
}

// SearchResponse is the stable provider-neutral response envelope. Services
// must initialize Results to a non-nil empty slice when no result is available.
type SearchResponse struct {
	Success      bool             `json:"success"`
	Query        string           `json:"query"`
	Results      []SearchResult   `json:"results"`
	Deduplicated int              `json:"deduplicated"`
	DroppedStale int              `json:"dropped_stale"`
	Partial      bool             `json:"partial"`
	Timing       SearchTimingInfo `json:"timing"`
	Error        *ErrorDetail     `json:"error,omitempty"`
}
