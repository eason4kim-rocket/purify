package models

import (
	"encoding/json"
	"time"

	"github.com/use-agent/purify/evidence"
)

// ExtractRequest is the payload for POST /api/v1/extract.
// It wraps a scrape operation with LLM-based structured data extraction.
type ExtractRequest struct {
	// URL is the target page to scrape. Required.
	URL string `json:"url" binding:"required,url"`

	// Schema is the JSON schema describing the desired output structure. Required.
	Schema json.RawMessage `json:"schema" binding:"required"`

	// Engine selects deterministic extraction, direct LLM extraction, or the
	// automatic compiled-first fallback chain. Default: "auto".
	Engine string `json:"engine,omitempty" binding:"omitempty,oneof=auto compiled llm"`

	// LLMAPIKey is the user's own LLM API key (BYOK). It is required only when
	// Engine is "llm" or when the auto engine must fall back to the LLM path.
	LLMAPIKey string `json:"llm_api_key,omitempty"`

	// LLMModel is the model to use for extraction. Default: "gpt-4o-mini".
	LLMModel string `json:"llm_model,omitempty"`

	// LLMBaseURL is the base URL for the LLM API. Default: "https://api.openai.com/v1".
	// Supports any OpenAI-compatible API (DeepSeek, Groq, Azure, etc.).
	LLMBaseURL string `json:"llm_base_url,omitempty"`

	// CSSSelector is an optional CSS selector to filter HTML before cleaning.
	CSSSelector string `json:"css_selector,omitempty"`

	// OutputFormat controls the intermediate format before LLM extraction.
	// Default: "markdown".
	OutputFormat string `json:"output_format,omitempty" binding:"omitempty,oneof=markdown html text"`

	// ExtractMode controls the content extraction strategy.
	// Default: "readability".
	ExtractMode string `json:"extract_mode,omitempty" binding:"omitempty,oneof=readability raw pruning auto"`

	// WaitForNetworkIdle instructs the scraper to wait for network idle.
	// Default: true.
	WaitForNetworkIdle *bool `json:"wait_for_network_idle,omitempty"`

	// Timeout is the max duration in seconds for the scrape operation.
	// Default: 30. Max: 120.
	Timeout int `json:"timeout,omitempty" binding:"omitempty,min=1,max=120"`

	// Stealth enables anti-bot-detection evasions.
	Stealth bool `json:"stealth,omitempty"`

	// ProxyURL overrides the default proxy for this request.
	ProxyURL string `json:"proxy_url,omitempty" binding:"omitempty,url"`

	// Evidence asks Purify to align every extracted leaf value to source text
	// and raw HTML and include field-level basis in the response.
	Evidence bool `json:"evidence,omitempty"`
}

// Defaults applies default values to unset fields.
func (r *ExtractRequest) Defaults() {
	if r.Engine == "" {
		r.Engine = "auto"
	}
	if r.LLMModel == "" {
		r.LLMModel = "gpt-4o-mini"
	}
	if r.LLMBaseURL == "" {
		r.LLMBaseURL = "https://api.openai.com/v1"
	}
	if r.OutputFormat == "" {
		r.OutputFormat = "markdown"
	}
	if r.ExtractMode == "" {
		r.ExtractMode = "readability"
	}
	if r.WaitForNetworkIdle == nil {
		t := true
		r.WaitForNetworkIdle = &t
	}
	if r.Timeout == 0 {
		r.Timeout = 30
	}
}

// ToScrapeRequest converts an ExtractRequest into a ScrapeRequest for reuse.
func (r *ExtractRequest) ToScrapeRequest() *ScrapeRequest {
	return &ScrapeRequest{
		URL:                r.URL,
		WaitForNetworkIdle: r.WaitForNetworkIdle,
		Timeout:            r.Timeout,
		Stealth:            r.Stealth,
		ProxyURL:           r.ProxyURL,
		OutputFormat:       r.OutputFormat,
		ExtractMode:        r.ExtractMode,
		CSSSelector:        r.CSSSelector,
	}
}

// ExtractResponse is the response for POST /api/v1/extract.
type ExtractResponse struct {
	// Success indicates whether the extraction completed without errors.
	Success bool `json:"success"`

	// Data is the structured JSON extracted by the LLM.
	Data json.RawMessage `json:"data,omitempty"`

	// Partial is true when the best available LLM output still violates the
	// requested JSON Schema after one bounded repair attempt.
	Partial bool `json:"partial,omitempty"`

	// Violations explains why a partial response does not satisfy the schema.
	Violations []SchemaViolation `json:"violations,omitempty"`

	// SnapshotID identifies the raw HTML used for evidence alignment.
	SnapshotID string `json:"snapshot_id,omitempty"`

	// UnlocatedRate is present in evidence mode, including when the rate is 0.
	UnlocatedRate *float64 `json:"unlocated_rate,omitempty"`

	// Basis maps JSON leaf paths to source anchors. The pointer distinguishes
	// evidence mode with zero leaves ({}) from evidence mode being disabled.
	Basis *EvidenceBasis `json:"basis,omitempty"`

	// Receipts maps the same JSON leaf paths to portable signed receipts. Like
	// Basis, an empty object remains visible when evidence mode is enabled.
	Receipts *FieldReceipts `json:"receipts,omitempty"`

	// Extractor identifies the deterministic extractor that served this
	// response. It is omitted while the LLM path is used.
	Extractor *ExtractorMetadata `json:"extractor,omitempty"`

	// Metadata contains extracted page metadata.
	Metadata Metadata `json:"metadata"`

	// Tokens provides token estimates for the scrape pipeline.
	Tokens TokenInfo `json:"tokens"`

	// Timing provides duration breakdowns for the operation.
	Timing ExtractTimingInfo `json:"timing"`

	// LLMUsage reports the LLM token consumption.
	LLMUsage *LLMUsage `json:"llm_usage,omitempty"`

	// Error is populated only when Success is false.
	Error *ErrorDetail `json:"error,omitempty"`
}

// EvidenceBasis is the public path-to-anchor map returned in evidence mode.
type EvidenceBasis map[string]evidence.Anchor

// FieldReceipts is the public path-to-token map returned in evidence mode.
type FieldReceipts map[string]string

// ExtractorMetadata describes a compiled extractor selected by the Phase 2
// auto engine. Nil means no deterministic extractor served the response.
type ExtractorMetadata struct {
	ID         string    `json:"id"`
	Version    int       `json:"version"`
	CompiledAt time.Time `json:"compiled_at"`
	Validation float64   `json:"validation"`
	Mode       string    `json:"mode"`
}

// SchemaViolation identifies one location where extracted data does not
// satisfy the caller-provided JSON Schema. Path uses JSON Pointer syntax and
// "$" denotes the document root.
type SchemaViolation struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// ExtractTimingInfo extends TimingInfo with extraction timing.
type ExtractTimingInfo struct {
	TotalMs      int64 `json:"total_ms"`
	NavigationMs int64 `json:"navigation_ms"`
	CleaningMs   int64 `json:"cleaning_ms"`
	ExtractionMs int64 `json:"extraction_ms"`
}

// LLMUsage reports token consumption from the LLM call.
type LLMUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
