package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/netip"
	urlpkg "net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/use-agent/purify/consensus"
	"github.com/use-agent/purify/evidence"
	"github.com/use-agent/purify/llm"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/publicnet"
	"github.com/use-agent/purify/verify/eav"
	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
)

const (
	maxAPIResponseBytes          int64 = 32 << 20
	maxVerifyURLBytes                  = 16 << 10
	maxVerifyClaimsJSONBytes           = 512 << 10
	maxExtractSchemaJSONBytes          = 512 << 10
	maxExtractLLMCredentialBytes       = 16 << 10
	maxExtractLLMModelBytes            = 256
	maxExtractLLMBaseURLBytes          = 16 << 10
	maxExtractConsensusPathBytes       = 4 << 10
	maxSearchQueryBytes                = models.MaxSearchQueryRunes * utf8.UTFMax
	maxSearchRawDomainBytes            = models.MaxSearchDomainBytes*utf8.UTFMax + 1
	maxAnswerValueBytes                = 64 << 10
	maxAnswerQuoteBytes                = 8 << 10
	maxAnswerSelectorBytes             = 4 << 10
	maxAnswerReceiptBytes              = 2 << 20
	maxAnswerRootBytes                 = 253
)

type apiHTTPResponse struct {
	StatusCode int
	Body       []byte
}

// extractAPIPayload preserves the legacy URL wire shape while allowing the
// MCP adapter to physically omit the mutually exclusive target and BYOK
// fields that are not selected for a call.
type extractAPIPayload struct {
	URL        string          `json:"url,omitempty"`
	Sources    []string        `json:"sources,omitempty"`
	Schema     json.RawMessage `json:"schema"`
	Engine     string          `json:"engine,omitempty"`
	LLMAPIKey  string          `json:"llm_api_key,omitempty"`
	LLMModel   string          `json:"llm_model,omitempty"`
	LLMBaseURL string          `json:"llm_base_url,omitempty"`
}

// searchAPIPayload keeps process authentication in the header and omits
// extraction-only settings unless a schema is selected. Deduplicate remains a
// pointer so an explicit false survives JSON encoding.
type searchAPIPayload struct {
	Query           string                   `json:"query"`
	Limit           int                      `json:"limit,omitempty"`
	Domains         []string                 `json:"domains,omitempty"`
	Freshness       string                   `json:"freshness,omitempty"`
	Ranking         models.SearchRankingMode `json:"ranking,omitempty"`
	ExpectedSubject *models.SubjectSpec      `json:"expected_subject,omitempty"`
	IncludeContent  bool                     `json:"include_content,omitempty"`
	Verify          bool                     `json:"verify,omitempty"`
	Deduplicate     *bool                    `json:"deduplicate,omitempty"`
	Schema          json.RawMessage          `json:"schema,omitempty"`
	Engine          string                   `json:"engine,omitempty"`
	LLMAPIKey       string                   `json:"llm_api_key,omitempty"`
	LLMModel        string                   `json:"llm_model,omitempty"`
	LLMBaseURL      string                   `json:"llm_base_url,omitempty"`
	Timeout         int                      `json:"timeout,omitempty"`
}

// answerAPIPayload keeps the MCP's ergonomic flat arguments out of the HTTP
// contract. The API always receives one explicit, fully-defaulted fact spec.
type answerAPIPayload struct {
	Spec    models.FactSpec `json:"spec"`
	Timeout int             `json:"timeout"`
}

// scrapeRequest mirrors the Purify API request model.
type scrapeRequest struct {
	URL          string `json:"url"`
	OutputFormat string `json:"output_format,omitempty"`
	ExtractMode  string `json:"extract_mode,omitempty"`
}

// scrapeResponse mirrors the Purify API response model.
type scrapeResponse struct {
	Success  bool   `json:"success"`
	Content  string `json:"content"`
	Metadata *struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		SiteName    string `json:"site_name"`
		Author      string `json:"author"`
		Language    string `json:"language"`
		SourceURL   string `json:"source_url"`
	} `json:"metadata"`
	Tokens *struct {
		OriginalEstimate int     `json:"original_estimate"`
		CleanedEstimate  int     `json:"cleaned_estimate"`
		SavingsPercent   float64 `json:"savings_percent"`
	} `json:"tokens"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// batchResponse mirrors the Purify batch API response.
type batchResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Total  int    `json:"total"`
}

// batchStatusResponse mirrors the Purify batch status API response.
type batchStatusResponse struct {
	ID        string            `json:"id"`
	Status    string            `json:"status"`
	Completed int               `json:"completed"`
	Total     int               `json:"total"`
	Results   []json.RawMessage `json:"results"`
}

// crawlResponse mirrors the Purify crawl API response.
type crawlResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// crawlStatusResponse mirrors the Purify crawl status API response.
type crawlStatusResponse struct {
	ID        string            `json:"id"`
	Status    string            `json:"status"`
	Completed int               `json:"completed"`
	Total     int               `json:"total"`
	Results   []json.RawMessage `json:"results"`
}

// mapResponse mirrors the Purify map API response.
type mapResponse struct {
	Success bool     `json:"success"`
	URLs    []string `json:"urls"`
	Total   int      `json:"total"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func newVerifyFactTool() mcp.Tool {
	return mcp.NewTool("verify_fact",
		mcp.WithDescription("Revisit a web page and verify evidence-backed claims, returning confirmed, changed, or gone for each claim."),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description("The source URL containing the facts to verify"),
		),
		mcp.WithString("claims",
			mcp.Required(),
			mcp.Description("A non-empty JSON array of evidence-backed Purify claims"),
		),
		mcp.WithSchemaAdditionalProperties(false),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(true),
	)
}

func newExtractDataTool() mcp.Tool {
	return mcp.NewTool("extract_data",
		mcp.WithDescription("Scrape one web page or merge one to eight source pages into evidence-backed structured data. Exactly one of url and sources is required. The default auto engine uses a compiled extractor first and falls back to the caller's LLM only when an LLM API key is supplied."),
		mcp.WithString("url",
			mcp.Description("One web page to scrape; mutually exclusive with sources"),
			mcp.MaxLength(maxVerifyURLBytes),
		),
		mcp.WithArray("sources",
			mcp.Description("One to eight source URLs for evidence-backed field consensus; mutually exclusive with url. Runtime limits are measured in UTF-8 bytes."),
			mcp.MinItems(1),
			mcp.MaxItems(models.MaxExtractSources),
			mcp.WithStringItems(mcp.MaxLength(models.MaxExtractSourceURLBytes)),
		),
		mcp.WithString("schema",
			mcp.Required(),
			mcp.Description("One JSON value containing the desired output schema"),
			mcp.MaxLength(maxExtractSchemaJSONBytes),
		),
		mcp.WithString("engine",
			mcp.Description("Extraction engine: auto (compiled first, optional LLM fallback), compiled (deterministic only), or llm (direct caller-funded LLM)"),
			mcp.Enum("auto", "compiled", "llm"),
			mcp.DefaultString("auto"),
		),
		mcp.WithString("llm_api_key",
			mcp.Description("Optional caller-owned LLM credential. Required only when engine is llm; auto can use it for fallback."),
			mcp.MaxLength(maxExtractLLMCredentialBytes),
		),
		mcp.WithString("llm_model",
			mcp.Description("LLM model for llm or auto fallback (default: gpt-4o-mini)"),
			mcp.MaxLength(maxExtractLLMModelBytes),
		),
		mcp.WithString("llm_base_url",
			mcp.Description("OpenAI-compatible base URL for llm or auto fallback (default: https://api.openai.com/v1)"),
			mcp.MaxLength(maxExtractLLMBaseURLBytes),
		),
		mcp.WithSchemaAdditionalProperties(false),
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(true),
	)
}

func newSearchWebTool() mcp.Tool {
	tool := mcp.NewTool("search_web",
		mcp.WithDescription("Search the public web and optionally fetch content, verify snippets, deduplicate syndicated copies, or extract schema-shaped data. Provider credentials are process-owned; llm_api_key is only for caller-funded schema extraction."),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("Natural-language query containing one to fifty words"),
			mcp.MaxLength(models.MaxSearchQueryRunes),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum ranked results (provider/relevance default: 10; trust default and maximum: 5)"),
			mcp.Min(1),
			mcp.Max(models.MaxSearchLimit),
			mcp.DefaultNumber(models.DefaultSearchLimit),
		),
		mcp.WithArray("domains",
			mcp.Description("Optional registrable-domain filters, without schemes or paths"),
			mcp.MaxItems(models.MaxSearchDomains),
			mcp.WithStringItems(mcp.MaxLength(maxSearchRawDomainBytes)),
		),
		mcp.WithString("freshness",
			mcp.Description("Provider-neutral recency filter"),
			mcp.Enum("day", "1d", "week", "7d", "month", "year"),
		),
		mcp.WithString("ranking",
			mcp.Description("Ranking mode: provider (default), relevance, or trust; trust requires expected_subject"),
			mcp.Enum("provider", "relevance", "trust"),
		),
		mcp.WithObject("expected_subject",
			mcp.Description("Explicit entity for trust ranking; forbidden for provider and relevance"),
			mcp.Properties(map[string]any{
				"name": map[string]any{
					"type": "string", "maxLength": eav.MaxSubjectBytes,
				},
				"hint": map[string]any{
					"type": "string", "maxLength": eav.MaxHintBytes,
				},
			}),
			mcp.AdditionalProperties(false),
		),
		mcp.WithBoolean("include_content",
			mcp.Description("Fetch and return cleaned content for at most the first five results"),
			mcp.DefaultBool(false),
		),
		mcp.WithBoolean("verify",
			mcp.Description("Fetch at most the first five results and verify each snippet against its page"),
			mcp.DefaultBool(false),
		),
		mcp.WithBoolean("deduplicate",
			mcp.Description("Collapse exact URLs and near-duplicate syndicated results"),
			mcp.DefaultBool(true),
		),
		mcp.WithString("schema",
			mcp.Description("Optional string containing exactly one JSON Schema value for per-result extraction"),
			mcp.MaxLength(models.MaxSearchSchemaBytes),
		),
		mcp.WithString("engine",
			mcp.Description("Schema extraction engine: auto, compiled, or llm"),
			mcp.Enum("auto", "compiled", "llm"),
		),
		mcp.WithString("llm_api_key",
			mcp.Description("Optional caller-owned LLM credential for schema extraction"),
			mcp.MaxLength(models.MaxSearchLLMAPIKeyBytes),
		),
		mcp.WithString("llm_model",
			mcp.Description("Optional LLM model for schema extraction"),
			mcp.MaxLength(models.MaxSearchLLMModelBytes),
		),
		mcp.WithString("llm_base_url",
			mcp.Description("Optional absolute OpenAI-compatible base URL without credentials, query, or fragment"),
			mcp.MaxLength(models.MaxSearchLLMBaseURLBytes),
		),
		mcp.WithNumber("timeout",
			mcp.Description("End-to-end timeout in seconds (provider/relevance default: 30; trust default: 60; max: 120)"),
			mcp.Min(1),
			mcp.Max(models.MaxSearchTimeoutSeconds),
			mcp.DefaultNumber(models.DefaultSearchTimeoutSeconds),
		),
		mcp.WithSchemaAdditionalProperties(false),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(true),
	)
	// mcp-go v0.44 exposes only WithNumber. Search accepts integral counts and
	// seconds, so advertise the narrower JSON Schema type as well as enforcing
	// it at runtime.
	for _, name := range []string{"limit", "timeout"} {
		if property, ok := tool.InputSchema.Properties[name].(map[string]any); ok {
			property["type"] = "integer"
		}
	}
	if subject, ok := tool.InputSchema.Properties["expected_subject"].(map[string]any); ok {
		subject["required"] = []string{"name"}
	}
	return tool
}

func newAnswerFactTool() mcp.Tool {
	tool := mcp.NewTool("answer_fact",
		mcp.WithDescription("Answer one scalar fact from independently rooted web evidence, returning either a supported belief or an honest unknown result."),
		mcp.WithString("subject",
			mcp.Required(),
			mcp.Description("Fact subject, normalized to single spaces before search"),
			mcp.MaxLength(models.MaxAnswerSubjectRunes),
		),
		mcp.WithString("predicate",
			mcp.Required(),
			mcp.Description("Whitespace-free scalar fact predicate"),
			mcp.MaxLength(models.MaxAnswerPredicateBytes),
		),
		mcp.WithString("freshness",
			mcp.Description("Provider-neutral recency requirement"),
			mcp.Enum("day", "1d", "week", "7d", "month", "year"),
			mcp.DefaultString(models.DefaultAnswerFreshness),
		),
		mcp.WithNumber("min_independent_sources",
			mcp.Description("Independent roots required before returning a belief"),
			mcp.Min(1),
			mcp.Max(models.MaxAnswerMinIndependentSources),
			mcp.DefaultNumber(models.DefaultAnswerMinIndependentSources),
		),
		mcp.WithString("on_conflict",
			mcp.Description("Conflict policy; Phase 5 always exposes competing values"),
			mcp.Enum(string(models.FactConflictExpose)),
			mcp.DefaultString(string(models.FactConflictExpose)),
		),
		mcp.WithNumber("timeout",
			mcp.Description("End-to-end timeout in seconds (default: 30, max: 120)"),
			mcp.Min(1),
			mcp.Max(models.MaxAnswerTimeoutSeconds),
			mcp.DefaultNumber(models.DefaultAnswerTimeoutSeconds),
		),
		mcp.WithSchemaAdditionalProperties(false),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(false),
		mcp.WithOpenWorldHintAnnotation(true),
	)
	// mcp-go v0.44 exposes WithNumber only. The runtime also enforces exact
	// integral values, but advertise the narrower schema to clients.
	for _, name := range []string{"min_independent_sources", "timeout"} {
		if property, ok := tool.InputSchema.Properties[name].(map[string]any); ok {
			property["type"] = "integer"
		}
	}
	return tool
}

func main() {
	apiURL := os.Getenv("PURIFY_API_URL")
	if apiURL == "" {
		apiURL = "http://127.0.0.1:8080"
	}
	apiKey := os.Getenv("PURIFY_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "PURIFY_API_KEY is required")
		os.Exit(1)
	}

	s := server.NewMCPServer(
		"purify",
		"1.0.0",
		server.WithToolCapabilities(false),
	)

	scrapeURLTool := mcp.NewTool("scrape_url",
		mcp.WithDescription("Scrape a web page and return cleaned content (markdown/text/html). Uses a headless browser to render JavaScript-heavy pages."),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description("The URL of the web page to scrape"),
		),
		mcp.WithString("extract_mode",
			mcp.Description("Content extraction mode: 'readability' (default, extracts main article), 'raw' (full page HTML), 'pruning' (ML-based pruning), or 'auto' (automatic selection)"),
			mcp.Enum("readability", "raw", "pruning", "auto"),
		),
		mcp.WithString("output_format",
			mcp.Description("Output format: 'markdown' (default), 'text' (plain text), 'html', or 'markdown_citations'"),
			mcp.Enum("markdown", "text", "html", "markdown_citations"),
		),
	)

	s.AddTool(scrapeURLTool, handleScrapeURL(apiURL, apiKey))
	s.AddTool(newVerifyFactTool(), handleVerifyFact(apiURL, apiKey))

	// batch_scrape tool
	batchScrapeTool := mcp.NewTool("batch_scrape",
		mcp.WithDescription("Scrape multiple URLs in parallel and return cleaned content for each. Useful for gathering content from many pages at once."),
		mcp.WithArray("urls",
			mcp.Required(),
			mcp.Description("List of URLs to scrape"),
		),
		mcp.WithString("output_format",
			mcp.Description("Output format: 'markdown' (default), 'text', 'html', or 'markdown_citations'"),
			mcp.Enum("markdown", "text", "html", "markdown_citations"),
		),
		mcp.WithString("extract_mode",
			mcp.Description("Content extraction mode: 'readability' (default), 'raw', 'pruning', or 'auto'"),
			mcp.Enum("readability", "raw", "pruning", "auto"),
		),
	)
	s.AddTool(batchScrapeTool, handleBatchScrape(apiURL, apiKey))

	// crawl_site tool
	crawlSiteTool := mcp.NewTool("crawl_site",
		mcp.WithDescription("Recursively crawl a website starting from a URL, following links up to a specified depth. Returns cleaned content for each discovered page."),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description("The starting URL to crawl from"),
		),
		mcp.WithNumber("max_depth",
			mcp.Description("Maximum crawl depth from the starting URL (default: 3, max: 10)"),
		),
		mcp.WithNumber("max_pages",
			mcp.Description("Maximum number of pages to crawl (default: 100, max: 500)"),
		),
		mcp.WithString("scope",
			mcp.Description("Link following scope: 'subdomain' (default), 'domain' (exact domain only), or 'page' (single page)"),
			mcp.Enum("subdomain", "domain", "page"),
		),
	)
	s.AddTool(crawlSiteTool, handleCrawlSite(apiURL, apiKey))

	// map_site tool
	mapSiteTool := mcp.NewTool("map_site",
		mcp.WithDescription("Discover all URLs on a website by crawling and extracting links. Returns a list of URLs without scraping their content."),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description("The URL of the website to map"),
		),
	)
	s.AddTool(mapSiteTool, handleMapSite(apiURL, apiKey))

	s.AddTool(newExtractDataTool(), handleExtractData(apiURL, apiKey))
	s.AddTool(newSearchWebTool(), handleSearchWeb(apiURL, apiKey))
	s.AddTool(newAnswerFactTool(), handleAnswerFact(apiURL, apiKey))

	if err := server.ServeStdio(s); err != nil {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
}

// apiPostResponse sends a POST request to the Purify API and retains the HTTP
// status while bounding the response body read.
func apiPostResponse(ctx context.Context, client *http.Client, apiURL, apiKey, path string, payload interface{}) (apiHTTPResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return apiHTTPResponse{}, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL+path, bytes.NewReader(body))
	if err != nil {
		return apiHTTPResponse{}, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)

	resp, err := client.Do(req)
	if err != nil {
		return apiHTTPResponse{}, fmt.Errorf("API request failed: %w", err)
	}
	defer resp.Body.Close()

	result := apiHTTPResponse{StatusCode: resp.StatusCode}
	result.Body, err = io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseBytes+1))
	if err != nil {
		return result, fmt.Errorf("read API response: %w", err)
	}
	if int64(len(result.Body)) > maxAPIResponseBytes {
		result.Body = nil
		return result, fmt.Errorf("API response exceeds %d-byte limit", maxAPIResponseBytes)
	}

	return result, nil
}

// apiPost preserves the body-only behavior used by the existing MCP tools.
func apiPost(ctx context.Context, client *http.Client, apiURL, apiKey, path string, payload interface{}) ([]byte, error) {
	resp, err := apiPostResponse(ctx, client, apiURL, apiKey, path, payload)
	return resp.Body, err
}

// pollJobCompletion polls a job endpoint until status is no longer "processing" or context is cancelled.
func pollJobCompletion(ctx context.Context, client *http.Client, apiURL, apiKey, endpoint string) ([]byte, error) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+endpoint, nil)
			if err != nil {
				return nil, fmt.Errorf("create poll request: %w", err)
			}
			req.Header.Set("X-API-Key", apiKey)

			resp, err := client.Do(req)
			if err != nil {
				return nil, fmt.Errorf("poll request failed: %w", err)
			}

			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				return nil, fmt.Errorf("read poll response: %w", err)
			}

			// Quick check if still processing.
			var status struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(body, &status); err != nil {
				return nil, fmt.Errorf("parse poll status: %w", err)
			}

			if status.Status != "processing" {
				return body, nil
			}
		}
	}
}

func handleScrapeURL(apiURL, apiKey string) server.ToolHandlerFunc {
	client := &http.Client{Timeout: 120 * time.Second}

	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		url, err := request.RequireString("url")
		if err != nil {
			return mcp.NewToolResultError("url is required"), nil
		}

		extractMode := request.GetString("extract_mode", "")
		outputFormat := request.GetString("output_format", "")

		reqBody := scrapeRequest{
			URL:          url,
			ExtractMode:  extractMode,
			OutputFormat: outputFormat,
		}

		body, err := json.Marshal(reqBody)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to marshal request: %v", err)), nil
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL+"/api/v1/scrape", bytes.NewReader(body))
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to create request: %v", err)), nil
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("X-API-Key", apiKey)

		resp, err := client.Do(httpReq)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("API request failed: %v", err)), nil
		}
		defer resp.Body.Close()

		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to read response: %v", err)), nil
		}

		var scrapeResp scrapeResponse
		if err := json.Unmarshal(respBody, &scrapeResp); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to parse response: %v", err)), nil
		}

		if !scrapeResp.Success {
			errMsg := "scrape failed"
			if scrapeResp.Error != nil {
				errMsg = fmt.Sprintf("[%s] %s", scrapeResp.Error.Code, scrapeResp.Error.Message)
			}
			return mcp.NewToolResultError(errMsg), nil
		}

		// Build result with metadata header
		var result string
		if scrapeResp.Metadata != nil {
			m := scrapeResp.Metadata
			result = fmt.Sprintf("Title: %s\nSource: %s\n\n", m.Title, m.SourceURL)
		}
		result += scrapeResp.Content

		if scrapeResp.Tokens != nil {
			t := scrapeResp.Tokens
			result += fmt.Sprintf("\n\n---\nTokens: %d (saved %.0f%% from original %d)",
				t.CleanedEstimate, t.SavingsPercent, t.OriginalEstimate)
		}

		return mcp.NewToolResultText(result), nil
	}
}

func handleVerifyFact(apiURL, apiKey string) server.ToolHandlerFunc {
	return handleVerifyFactWithClient(&http.Client{Timeout: 120 * time.Second}, apiURL, apiKey)
}

func handleVerifyFactWithClient(client *http.Client, apiURL, apiKey string) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		url, err := request.RequireString("url")
		if err != nil || strings.TrimSpace(url) == "" || len(url) > maxVerifyURLBytes {
			return mcp.NewToolResultError("url is required and must be a non-empty string"), nil
		}

		claimsJSON, err := request.RequireString("claims")
		if err != nil {
			return mcp.NewToolResultError("claims is required and must be a JSON array string"), nil
		}
		claims, err := decodeVerifyClaims(claimsJSON)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("claims must be one non-empty JSON array: %v", err)), nil
		}

		apiResp, err := apiPostResponse(ctx, client, apiURL, apiKey, "/api/v1/verify", models.VerifyRequest{
			URL:    url,
			Claims: claims,
		})
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("verify request failed: %v", err)), nil
		}

		if apiResp.StatusCode < http.StatusOK || apiResp.StatusCode >= http.StatusMultipleChoices {
			return mcp.NewToolResultError(verifyAPIError(apiResp.StatusCode, apiResp.Body)), nil
		}

		verifyResp, err := decodeVerifyResponse(apiResp.Body)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to parse verify response: %v", err)), nil
		}
		pretty, err := json.MarshalIndent(verifyResp, "", "  ")
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to format verify response: %v", err)), nil
		}

		return mcp.NewToolResultStructured(verifyResp, string(pretty)), nil
	}
}

func decodeVerifyClaims(raw string) ([]models.Claim, error) {
	if len(raw) > maxVerifyClaimsJSONBytes {
		return nil, fmt.Errorf("array exceeds %d-byte limit", maxVerifyClaimsJSONBytes)
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()

	var claims []models.Claim
	if err := decoder.Decode(&claims); err != nil {
		return nil, err
	}
	if len(claims) == 0 {
		return nil, fmt.Errorf("array must contain at least one claim")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}

	return claims, nil
}

func decodeVerifyResponse(body []byte) (models.VerifyResponse, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()

	var response *models.VerifyResponse
	if err := decoder.Decode(&response); err != nil {
		return models.VerifyResponse{}, err
	}
	if response == nil {
		return models.VerifyResponse{}, fmt.Errorf("response must be a JSON object")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return models.VerifyResponse{}, err
	}
	if err := validateVerifyResponse(*response); err != nil {
		return models.VerifyResponse{}, err
	}

	return *response, nil
}

func validateVerifyResponse(response models.VerifyResponse) error {
	if response.VerificationID == "" || response.URL == "" || response.FinalURL == "" ||
		response.StatusCode < 100 || response.StatusCode > 599 || len(response.Results) == 0 ||
		response.VerifiedAt.IsZero() {
		return fmt.Errorf("response is missing required verification fields")
	}
	for _, result := range response.Results {
		if result.Path == "" {
			return fmt.Errorf("response contains a result without a path")
		}
		switch result.Status {
		case models.VerifyStatusConfirmed, models.VerifyStatusChanged, models.VerifyStatusGone:
		default:
			return fmt.Errorf("response contains an invalid claim status")
		}
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return fmt.Errorf("invalid trailing data: %w", err)
	}
	return nil
}

func verifyAPIError(statusCode int, body []byte) string {
	var response models.VerifyErrorResponse
	if err := json.Unmarshal(body, &response); err == nil && response.Error != nil {
		code := strings.TrimSpace(response.Error.Code)
		message := strings.TrimSpace(response.Error.Message)
		switch {
		case code != "" && message != "":
			return fmt.Sprintf("[%s] %s", code, message)
		case message != "":
			return message
		case code != "":
			return fmt.Sprintf("[%s] verification failed (HTTP %d)", code, statusCode)
		}
	}
	return fmt.Sprintf("verification failed (HTTP %d)", statusCode)
}

func handleBatchScrape(apiURL, apiKey string) server.ToolHandlerFunc {
	client := &http.Client{Timeout: 600 * time.Second}

	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		urls, err := request.RequireStringSlice("urls")
		if err != nil {
			return mcp.NewToolResultError("urls is required and must be an array of strings"), nil
		}

		outputFormat := request.GetString("output_format", "")
		extractMode := request.GetString("extract_mode", "")

		payload := map[string]interface{}{
			"urls": urls,
			"options": map[string]interface{}{
				"output_format": outputFormat,
				"extract_mode":  extractMode,
			},
		}

		// POST to create batch job.
		respBody, err := apiPost(ctx, client, apiURL, apiKey, "/api/v1/batch/scrape", payload)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("batch request failed: %v", err)), nil
		}

		var batchResp batchResponse
		if err := json.Unmarshal(respBody, &batchResp); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to parse batch response: %v", err)), nil
		}

		if batchResp.ID == "" {
			return mcp.NewToolResultError("batch job creation failed"), nil
		}

		// Poll for completion.
		resultBody, err := pollJobCompletion(ctx, client, apiURL, apiKey, "/api/v1/batch/"+batchResp.ID)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("polling batch job failed: %v", err)), nil
		}

		var statusResp batchStatusResponse
		if err := json.Unmarshal(resultBody, &statusResp); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to parse batch status: %v", err)), nil
		}

		// Format results.
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Batch %s: %s (%d/%d completed)\n\n", statusResp.ID, statusResp.Status, statusResp.Completed, statusResp.Total))

		for i, raw := range statusResp.Results {
			var sr scrapeResponse
			if err := json.Unmarshal(raw, &sr); err != nil {
				sb.WriteString(fmt.Sprintf("--- Result %d: parse error ---\n\n", i+1))
				continue
			}
			if sr.Success {
				title := ""
				if sr.Metadata != nil {
					title = sr.Metadata.Title
				}
				sb.WriteString(fmt.Sprintf("--- [%d] %s ---\n%s\n\n", i+1, title, sr.Content))
			} else {
				errMsg := "unknown error"
				if sr.Error != nil {
					errMsg = sr.Error.Message
				}
				sb.WriteString(fmt.Sprintf("--- [%d] FAILED: %s ---\n\n", i+1, errMsg))
			}
		}

		return mcp.NewToolResultText(sb.String()), nil
	}
}

func handleCrawlSite(apiURL, apiKey string) server.ToolHandlerFunc {
	client := &http.Client{Timeout: 600 * time.Second}

	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		url, err := request.RequireString("url")
		if err != nil {
			return mcp.NewToolResultError("url is required"), nil
		}

		payload := map[string]interface{}{
			"url": url,
		}

		args := request.GetArguments()
		if maxDepth, ok := args["max_depth"]; ok {
			payload["max_depth"] = maxDepth
		}
		if maxPages, ok := args["max_pages"]; ok {
			payload["max_pages"] = maxPages
		}
		if scope := request.GetString("scope", ""); scope != "" {
			payload["scope"] = scope
		}

		// POST to create crawl job.
		respBody, err := apiPost(ctx, client, apiURL, apiKey, "/api/v1/crawl", payload)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("crawl request failed: %v", err)), nil
		}

		var crawlResp crawlResponse
		if err := json.Unmarshal(respBody, &crawlResp); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to parse crawl response: %v", err)), nil
		}

		if crawlResp.ID == "" {
			return mcp.NewToolResultError("crawl job creation failed"), nil
		}

		// Poll for completion.
		resultBody, err := pollJobCompletion(ctx, client, apiURL, apiKey, "/api/v1/crawl/"+crawlResp.ID)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("polling crawl job failed: %v", err)), nil
		}

		var statusResp crawlStatusResponse
		if err := json.Unmarshal(resultBody, &statusResp); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to parse crawl status: %v", err)), nil
		}

		// Format results.
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Crawl %s: %s (%d/%d pages)\n\n", statusResp.ID, statusResp.Status, statusResp.Completed, statusResp.Total))

		for i, raw := range statusResp.Results {
			var sr scrapeResponse
			if err := json.Unmarshal(raw, &sr); err != nil {
				sb.WriteString(fmt.Sprintf("--- Page %d: parse error ---\n\n", i+1))
				continue
			}
			if sr.Success {
				title := ""
				source := ""
				if sr.Metadata != nil {
					title = sr.Metadata.Title
					source = sr.Metadata.SourceURL
				}
				sb.WriteString(fmt.Sprintf("--- Page %d: %s (%s) ---\n%s\n\n", i+1, title, source, sr.Content))
			} else {
				errMsg := "unknown error"
				if sr.Error != nil {
					errMsg = sr.Error.Message
				}
				sb.WriteString(fmt.Sprintf("--- Page %d: FAILED: %s ---\n\n", i+1, errMsg))
			}
		}

		return mcp.NewToolResultText(sb.String()), nil
	}
}

func handleMapSite(apiURL, apiKey string) server.ToolHandlerFunc {
	client := &http.Client{Timeout: 120 * time.Second}

	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		url, err := request.RequireString("url")
		if err != nil {
			return mcp.NewToolResultError("url is required"), nil
		}

		respBody, err := apiPost(ctx, client, apiURL, apiKey, "/api/v1/map", map[string]string{"url": url})
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("map request failed: %v", err)), nil
		}

		var mapResp mapResponse
		if err := json.Unmarshal(respBody, &mapResp); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to parse map response: %v", err)), nil
		}

		if !mapResp.Success {
			errMsg := "map failed"
			if mapResp.Error != nil {
				errMsg = fmt.Sprintf("[%s] %s", mapResp.Error.Code, mapResp.Error.Message)
			}
			return mcp.NewToolResultError(errMsg), nil
		}

		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Found %d URLs:\n\n", mapResp.Total))
		for _, u := range mapResp.URLs {
			sb.WriteString(u + "\n")
		}

		return mcp.NewToolResultText(sb.String()), nil
	}
}

func handleExtractData(apiURL, apiKey string) server.ToolHandlerFunc {
	return handleExtractDataWithClient(newExtractHTTPClient(), apiURL, apiKey)
}

func handleSearchWeb(apiURL, apiKey string) server.ToolHandlerFunc {
	return handleSearchWebWithClient(newSearchHTTPClient(), apiURL, apiKey)
}

func handleAnswerFact(apiURL, apiKey string) server.ToolHandlerFunc {
	return handleAnswerFactWithClient(newAnswerHTTPClient(), apiURL, apiKey)
}

func newAnswerHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 120 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func handleAnswerFactWithClient(client *http.Client, apiURL, apiKey string) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		payload, err := answerPayload(request.GetArguments())
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if ctx == nil {
			return mcp.NewToolResultError("answer request failed: context is required"), nil
		}
		if err := ctx.Err(); err != nil {
			return answerContextToolError(err, apiKey), nil
		}
		taskContext, cancel := context.WithTimeout(ctx, time.Duration(payload.Timeout)*time.Second)
		defer cancel()
		if err := taskContext.Err(); err != nil {
			return answerContextToolError(err, apiKey), nil
		}

		apiResponse, err := apiPostResponse(taskContext, client, apiURL, apiKey, "/api/v1/answer", payload)
		if err != nil {
			if contextErr := taskContext.Err(); contextErr != nil {
				return answerContextToolError(contextErr, apiKey), nil
			}
			return mcp.NewToolResultError(redactAnswerSecrets(fmt.Sprintf("answer request failed: %v", err), apiKey)), nil
		}
		if err := taskContext.Err(); err != nil {
			return answerContextToolError(err, apiKey), nil
		}

		containsSecret, scanErr := searchJSONContainsSecret(apiResponse.Body, apiKey)
		if err := taskContext.Err(); err != nil {
			return answerContextToolError(err, apiKey), nil
		}
		if scanErr != nil {
			if apiResponse.StatusCode == http.StatusOK {
				return mcp.NewToolResultError("failed to parse answer response"), nil
			}
			return mcp.NewToolResultError(fmt.Sprintf("answer failed (HTTP %d)", apiResponse.StatusCode)), nil
		}
		if containsSecret {
			return mcp.NewToolResultError("failed to parse answer response: response contains sensitive data"), nil
		}

		if apiResponse.StatusCode != http.StatusOK {
			message := answerAPIError(apiResponse.StatusCode, apiResponse.Body)
			if err := taskContext.Err(); err != nil {
				return answerContextToolError(err, apiKey), nil
			}
			return mcp.NewToolResultError(redactAnswerSecrets(message, apiKey)), nil
		}

		response, err := decodeAnswerFactResponse(apiResponse.Body, payload.Spec.MinIndependentSources)
		if contextErr := taskContext.Err(); contextErr != nil {
			return answerContextToolError(contextErr, apiKey), nil
		}
		if err != nil {
			return mcp.NewToolResultError(redactAnswerSecrets(fmt.Sprintf("failed to parse answer response: %v", err), apiKey)), nil
		}
		encoded, err := encodeAnswerToolResponse(taskContext, response)
		if contextErr := taskContext.Err(); contextErr != nil {
			return answerContextToolError(contextErr, apiKey), nil
		}
		if err != nil {
			return mcp.NewToolResultError("failed to format answer response"), nil
		}
		return mcp.NewToolResultStructured(response, string(encoded)), nil
	}
}

func answerContextToolError(err error, secrets ...string) *mcp.CallToolResult {
	return mcp.NewToolResultError(redactAnswerSecrets(fmt.Sprintf("answer request failed: %v", err), secrets...))
}

var answerEncodingSlots = make(chan struct{}, 4)

func encodeAnswerToolResponse(ctx context.Context, response models.AnswerResponse) ([]byte, error) {
	if ctx == nil {
		return nil, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case answerEncodingSlots <- struct{}{}:
		defer func() { <-answerEncodingSlots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(response)
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if err != nil {
		return nil, err
	}
	if len(encoded) == 0 || len(encoded) > models.MaxAnswerResponseBytes || !json.Valid(encoded) {
		return nil, fmt.Errorf("encoded answer response exceeds its output budget")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return encoded, nil
}

var answerArgumentNames = map[string]struct{}{
	"subject": {}, "predicate": {}, "freshness": {}, "min_independent_sources": {},
	"on_conflict": {}, "timeout": {},
}

func answerPayload(arguments map[string]any) (answerAPIPayload, error) {
	unknown := make([]string, 0)
	for name := range arguments {
		if _, ok := answerArgumentNames[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return answerAPIPayload{}, fmt.Errorf("unsupported answer_fact argument %q", unknown[0])
	}

	subject, present, err := strictSearchString(arguments, "subject")
	if err != nil || !present {
		return answerAPIPayload{}, fmt.Errorf("subject is required and must be a string")
	}
	if len(subject) == 0 || len(subject) > models.MaxAnswerSubjectBytes || !utf8.ValidString(subject) ||
		utf8.RuneCountInString(subject) > models.MaxAnswerSubjectRunes || strings.IndexFunc(subject, unicode.IsControl) >= 0 {
		return answerAPIPayload{}, fmt.Errorf("subject is invalid or exceeds its public limit")
	}
	normalizedSubject := strings.Join(strings.Fields(subject), " ")
	if normalizedSubject == "" || len(strings.Fields(normalizedSubject)) > models.MaxAnswerSubjectWords {
		return answerAPIPayload{}, fmt.Errorf("subject must contain between 1 and %d words", models.MaxAnswerSubjectWords)
	}

	predicate, present, err := strictSearchString(arguments, "predicate")
	if err != nil || !present {
		return answerAPIPayload{}, fmt.Errorf("predicate is required and must be a string")
	}
	if len(predicate) == 0 || len(predicate) > models.MaxAnswerPredicateBytes || !utf8.ValidString(predicate) ||
		strings.IndexFunc(predicate, func(character rune) bool { return unicode.IsSpace(character) || unicode.IsControl(character) }) >= 0 {
		return answerAPIPayload{}, fmt.Errorf("predicate must be a non-empty whitespace-free UTF-8 string of at most %d bytes", models.MaxAnswerPredicateBytes)
	}

	query := normalizedSubject + " " + predicate
	if utf8.RuneCountInString(query) > models.MaxSearchQueryRunes || len(strings.Fields(query)) > models.MaxSearchQueryWords {
		return answerAPIPayload{}, fmt.Errorf("subject and predicate exceed the combined search query limit")
	}

	freshness, freshnessPresent, err := strictSearchString(arguments, "freshness")
	if err != nil {
		return answerAPIPayload{}, err
	}
	if !freshnessPresent {
		freshness = models.DefaultAnswerFreshness
	}
	switch freshness {
	case "day", "1d", "week", "7d", "month", "year":
	default:
		return answerAPIPayload{}, fmt.Errorf("freshness must be day, 1d, week, 7d, month, or year")
	}

	minimum, err := optionalSearchInteger(arguments, "min_independent_sources", models.DefaultAnswerMinIndependentSources, 1, models.MaxAnswerMinIndependentSources)
	if err != nil {
		return answerAPIPayload{}, err
	}
	onConflict, conflictPresent, err := strictSearchString(arguments, "on_conflict")
	if err != nil {
		return answerAPIPayload{}, err
	}
	if !conflictPresent {
		onConflict = string(models.FactConflictExpose)
	}
	if onConflict != string(models.FactConflictExpose) {
		return answerAPIPayload{}, fmt.Errorf("on_conflict must be expose")
	}
	timeout, err := optionalSearchInteger(arguments, "timeout", models.DefaultAnswerTimeoutSeconds, 1, models.MaxAnswerTimeoutSeconds)
	if err != nil {
		return answerAPIPayload{}, err
	}

	return answerAPIPayload{
		Spec: models.FactSpec{
			Subject:               normalizedSubject,
			Predicate:             predicate,
			Freshness:             freshness,
			MinIndependentSources: minimum,
			OnConflict:            models.FactConflictPolicy(onConflict),
		},
		Timeout: timeout,
	}, nil
}

func newSearchHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 120 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func handleSearchWebWithClient(client *http.Client, apiURL, apiKey string) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		payload, llmAPIKey, err := searchPayload(request.GetArguments())
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if ctx == nil {
			return mcp.NewToolResultError("search request failed: context is required"), nil
		}
		if err := ctx.Err(); err != nil {
			return mcp.NewToolResultError(redactSearchSecrets(fmt.Sprintf("search request failed: %v", err), apiKey, llmAPIKey)), nil
		}
		resolved := models.ResolveSearchDefaults(payload.Ranking, payload.Limit, payload.Timeout)
		taskContext, cancel := context.WithTimeout(ctx, time.Duration(resolved.TimeoutSeconds)*time.Second)
		defer cancel()
		if err := taskContext.Err(); err != nil {
			return searchContextToolError(err, apiKey, llmAPIKey), nil
		}

		apiResponse, err := apiPostResponse(taskContext, client, apiURL, apiKey, "/api/v1/search", payload)
		if err != nil {
			if contextErr := taskContext.Err(); contextErr != nil {
				return searchContextToolError(contextErr, apiKey, llmAPIKey), nil
			}
			message := redactSearchSecrets(fmt.Sprintf("search request failed: %v", err), apiKey, llmAPIKey)
			return mcp.NewToolResultError(message), nil
		}
		if err := taskContext.Err(); err != nil {
			return searchContextToolError(err, apiKey, llmAPIKey), nil
		}

		if apiResponse.StatusCode != http.StatusOK {
			message := searchAPIError(apiResponse.StatusCode, apiResponse.Body)
			if err := taskContext.Err(); err != nil {
				return searchContextToolError(err, apiKey, llmAPIKey), nil
			}
			return mcp.NewToolResultError(redactSearchSecrets(message, apiKey, llmAPIKey)), nil
		}
		containsSecret, scanErr := searchJSONContainsSecret(apiResponse.Body, apiKey, llmAPIKey)
		if err := taskContext.Err(); err != nil {
			return searchContextToolError(err, apiKey, llmAPIKey), nil
		}
		if scanErr != nil {
			return mcp.NewToolResultError("failed to parse search response"), nil
		}
		if containsSecret {
			return mcp.NewToolResultError("failed to parse search response: response contains sensitive data"), nil
		}

		response, err := decodeSearchResponse(apiResponse.Body)
		if contextErr := taskContext.Err(); contextErr != nil {
			return searchContextToolError(contextErr, apiKey, llmAPIKey), nil
		}
		if err != nil {
			message := redactSearchSecrets(fmt.Sprintf("failed to parse search response: %v", err), apiKey, llmAPIKey)
			return mcp.NewToolResultError(message), nil
		}
		if !response.Success {
			message := formatSearchError(response.Error, apiResponse.StatusCode)
			return mcp.NewToolResultError(redactSearchSecrets(message, apiKey, llmAPIKey)), nil
		}
		if err := validateSearchResponseForRequest(response, payload); err != nil {
			if contextErr := taskContext.Err(); contextErr != nil {
				return searchContextToolError(contextErr, apiKey, llmAPIKey), nil
			}
			return mcp.NewToolResultError(redactSearchSecrets(fmt.Sprintf("failed to parse search response: %v", err), apiKey, llmAPIKey)), nil
		}
		if err := taskContext.Err(); err != nil {
			return searchContextToolError(err, apiKey, llmAPIKey), nil
		}
		encoded, err := encodeSearchToolResponse(taskContext, response)
		if contextErr := taskContext.Err(); contextErr != nil {
			return searchContextToolError(contextErr, apiKey, llmAPIKey), nil
		}
		if err != nil {
			return mcp.NewToolResultError("failed to format search response"), nil
		}

		return mcp.NewToolResultStructured(response, string(encoded)), nil
	}
}

func searchContextToolError(err error, secrets ...string) *mcp.CallToolResult {
	message := redactSearchSecrets(fmt.Sprintf("search request failed: %v", err), secrets...)
	return mcp.NewToolResultError(message)
}

func encodeSearchToolResponse(ctx context.Context, response models.SearchResponse) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(response)
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if err != nil {
		return nil, err
	}
	if len(encoded) == 0 || len(encoded) > models.MaxSearchResponseBytes || !json.Valid(encoded) {
		return nil, fmt.Errorf("encoded search response exceeds its output budget")
	}
	return encoded, nil
}

var searchArgumentNames = map[string]struct{}{
	"query": {}, "limit": {}, "domains": {}, "freshness": {}, "include_content": {}, "verify": {},
	"deduplicate": {}, "schema": {}, "engine": {}, "llm_api_key": {}, "llm_model": {}, "llm_base_url": {},
	"ranking": {}, "expected_subject": {}, "timeout": {},
}

func searchPayload(arguments map[string]any) (searchAPIPayload, string, error) {
	unknown := make([]string, 0)
	for name := range arguments {
		if _, ok := searchArgumentNames[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return searchAPIPayload{}, "", fmt.Errorf("unsupported search_web argument %q", unknown[0])
	}

	query, present, err := strictSearchString(arguments, "query")
	if err != nil || !present {
		return searchAPIPayload{}, "", fmt.Errorf("query is required and must be a string")
	}
	if err := validateSearchQuery(query); err != nil {
		return searchAPIPayload{}, "", err
	}

	rankingText, rankingPresent, err := strictSearchString(arguments, "ranking")
	if err != nil {
		return searchAPIPayload{}, "", err
	}
	ranking := models.SearchRankingMode(rankingText)
	if !rankingPresent {
		ranking = ""
	} else {
		switch ranking {
		case models.SearchRankingProvider, models.SearchRankingRelevance, models.SearchRankingTrust:
		default:
			return searchAPIPayload{}, "", fmt.Errorf("ranking must be provider, relevance, or trust")
		}
	}
	expectedSubject, err := optionalSearchExpectedSubject(arguments, ranking)
	if err != nil {
		return searchAPIPayload{}, "", err
	}
	maximumLimit := models.MaxSearchLimit
	if ranking == models.SearchRankingTrust {
		maximumLimit = models.MaxSearchTrustLimit
	}
	limit, err := optionalSearchInteger(arguments, "limit", 0, 1, maximumLimit)
	if err != nil {
		return searchAPIPayload{}, "", err
	}
	timeout, err := optionalSearchInteger(arguments, "timeout", 0, 1, models.MaxSearchTimeoutSeconds)
	if err != nil {
		return searchAPIPayload{}, "", err
	}
	domains, err := optionalSearchDomains(arguments)
	if err != nil {
		return searchAPIPayload{}, "", err
	}
	freshness, freshnessPresent, err := strictSearchString(arguments, "freshness")
	if err != nil {
		return searchAPIPayload{}, "", err
	}
	if freshnessPresent {
		switch freshness {
		case "day", "1d", "week", "7d", "month", "year":
		default:
			return searchAPIPayload{}, "", fmt.Errorf("freshness must be day, 1d, week, 7d, month, or year")
		}
	}
	includeContent, err := optionalSearchBool(arguments, "include_content", false)
	if err != nil {
		return searchAPIPayload{}, "", err
	}
	verify, err := optionalSearchBool(arguments, "verify", false)
	if err != nil {
		return searchAPIPayload{}, "", err
	}
	deduplicate, err := optionalSearchBool(arguments, "deduplicate", true)
	if err != nil {
		return searchAPIPayload{}, "", err
	}

	payload := searchAPIPayload{
		Query:           query,
		Limit:           limit,
		Domains:         domains,
		Freshness:       freshness,
		Ranking:         ranking,
		ExpectedSubject: expectedSubject,
		IncludeContent:  includeContent,
		Verify:          verify,
		Deduplicate:     &deduplicate,
		Timeout:         timeout,
	}

	schemaString, schemaPresent, err := strictSearchString(arguments, "schema")
	if err != nil {
		return searchAPIPayload{}, "", err
	}
	extractionNames := []string{"engine", "llm_api_key", "llm_model", "llm_base_url"}
	if !schemaPresent {
		for _, name := range extractionNames {
			if _, present := arguments[name]; present {
				return searchAPIPayload{}, "", fmt.Errorf("%s requires schema", name)
			}
		}
		return payload, "", nil
	}
	schema, err := decodeSearchSchema(schemaString)
	if err != nil {
		return searchAPIPayload{}, "", fmt.Errorf("schema must be one valid JSON schema: %v", err)
	}
	engine, enginePresent, err := strictSearchString(arguments, "engine")
	if err != nil {
		return searchAPIPayload{}, "", err
	}
	if !enginePresent {
		engine = "auto"
	}
	switch engine {
	case "auto", "compiled", "llm":
	default:
		return searchAPIPayload{}, "", fmt.Errorf("engine must be auto, compiled, or llm")
	}
	llmAPIKey, _, err := strictSearchString(arguments, "llm_api_key")
	if err != nil {
		return searchAPIPayload{}, "", err
	}
	llmModel, _, err := strictSearchString(arguments, "llm_model")
	if err != nil {
		return searchAPIPayload{}, "", err
	}
	llmBaseURL, _, err := strictSearchString(arguments, "llm_base_url")
	if err != nil {
		return searchAPIPayload{}, "", err
	}
	if err := validateSearchTextOption("llm_api_key", llmAPIKey, models.MaxSearchLLMAPIKeyBytes, true); err != nil {
		return searchAPIPayload{}, "", err
	}
	if err := validateSearchTextOption("llm_model", llmModel, models.MaxSearchLLMModelBytes, true); err != nil {
		return searchAPIPayload{}, "", err
	}
	if llmModel != "" && strings.IndexFunc(llmModel, unicode.IsSpace) >= 0 {
		return searchAPIPayload{}, "", fmt.Errorf("llm_model must not contain whitespace")
	}
	if err := validateSearchTextOption("llm_base_url", llmBaseURL, models.MaxSearchLLMBaseURLBytes, true); err != nil {
		return searchAPIPayload{}, "", err
	}
	if llmBaseURL != "" {
		llmBaseURL, err = normalizeExtractLLMBaseURL(llmBaseURL)
		if err != nil {
			return searchAPIPayload{}, "", fmt.Errorf("llm_base_url must be an absolute http or https URL without credentials, query, or fragment")
		}
	}
	if engine == "llm" && strings.TrimSpace(llmAPIKey) == "" {
		return searchAPIPayload{}, "", fmt.Errorf("llm_api_key is required when engine is llm")
	}

	payload.Schema = append(json.RawMessage(nil), schema...)
	payload.Engine = engine
	// Compiled extraction never receives caller LLM settings. Auto without a
	// credential is compiled-only, so model/base options are irrelevant there
	// as well and are physically omitted from the wire request.
	if engine == "llm" || engine == "auto" && strings.TrimSpace(llmAPIKey) != "" {
		payload.LLMAPIKey = llmAPIKey
		payload.LLMModel = llmModel
		payload.LLMBaseURL = llmBaseURL
		return payload, llmAPIKey, nil
	}
	return payload, "", nil
}

func strictSearchString(arguments map[string]any, name string) (string, bool, error) {
	value, present := arguments[name]
	if !present {
		return "", false, nil
	}
	result, ok := value.(string)
	if !ok {
		return "", true, fmt.Errorf("%s must be a string", name)
	}
	return result, true, nil
}

func optionalSearchExpectedSubject(arguments map[string]any, ranking models.SearchRankingMode) (*models.SubjectSpec, error) {
	value, present := arguments["expected_subject"]
	if ranking != models.SearchRankingTrust {
		if present {
			return nil, fmt.Errorf("expected_subject is only supported when ranking is trust")
		}
		return nil, nil
	}
	if !present {
		return nil, fmt.Errorf("expected_subject is required when ranking is trust")
	}
	object, ok := value.(map[string]any)
	if !ok || object == nil {
		return nil, fmt.Errorf("expected_subject must be an object")
	}
	for name := range object {
		if name != "name" && name != "hint" {
			return nil, fmt.Errorf("expected_subject contains unsupported field %q", name)
		}
	}
	name, namePresent, err := strictSearchString(object, "name")
	if err != nil || !namePresent {
		return nil, fmt.Errorf("expected_subject.name is required and must be a string")
	}
	hint, _, err := strictSearchString(object, "hint")
	if err != nil {
		return nil, fmt.Errorf("expected_subject.hint must be a string")
	}
	if err := validateSearchTextOption("expected_subject.name", name, eav.MaxSubjectBytes, false); err != nil {
		return nil, err
	}
	if err := validateSearchTextOption("expected_subject.hint", hint, eav.MaxHintBytes, true); err != nil {
		return nil, err
	}
	if eav.Normalize(name) == "" {
		return nil, fmt.Errorf("expected_subject.name must identify an entity")
	}
	name = strings.TrimSpace(name)
	hint = strings.TrimSpace(hint)
	if name == "" {
		return nil, fmt.Errorf("expected_subject.name must not be empty")
	}
	return &models.SubjectSpec{Name: name, Hint: hint}, nil
}

func optionalSearchBool(arguments map[string]any, name string, fallback bool) (bool, error) {
	value, present := arguments[name]
	if !present {
		return fallback, nil
	}
	result, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("%s must be a boolean", name)
	}
	return result, nil
}

func optionalSearchInteger(arguments map[string]any, name string, fallback, minimum, maximum int) (int, error) {
	value, present := arguments[name]
	if !present {
		return fallback, nil
	}
	result, ok := exactSearchInteger(value)
	if !ok {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	if result < minimum || result > maximum {
		return 0, fmt.Errorf("%s must be between %d and %d", name, minimum, maximum)
	}
	return result, nil
}

func exactSearchInteger(value any) (int, bool) {
	maximumInt := int64(^uint(0) >> 1)
	minimumInt := -maximumInt - 1
	var result int64
	switch typed := value.(type) {
	case int:
		return typed, true
	case int8:
		result = int64(typed)
	case int16:
		result = int64(typed)
	case int32:
		result = int64(typed)
	case int64:
		result = typed
	case uint:
		if uint64(typed) > uint64(maximumInt) {
			return 0, false
		}
		result = int64(typed)
	case uint8:
		result = int64(typed)
	case uint16:
		result = int64(typed)
	case uint32:
		result = int64(typed)
	case uint64:
		if typed > uint64(maximumInt) {
			return 0, false
		}
		result = int64(typed)
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || math.Trunc(typed) != typed || typed < float64(minimumInt) || typed > float64(maximumInt) {
			return 0, false
		}
		result = int64(typed)
	case float32:
		converted := float64(typed)
		if math.IsNaN(converted) || math.IsInf(converted, 0) || math.Trunc(converted) != converted || converted < float64(minimumInt) || converted > float64(maximumInt) {
			return 0, false
		}
		result = int64(typed)
	case json.Number:
		parsed, err := strconv.ParseInt(string(typed), 10, 64)
		if err != nil {
			return 0, false
		}
		result = parsed
	default:
		return 0, false
	}
	if result < minimumInt || result > maximumInt {
		return 0, false
	}
	return int(result), true
}

func validateSearchQuery(query string) error {
	if len(query) == 0 || len(query) > maxSearchQueryBytes || !utf8.ValidString(query) ||
		utf8.RuneCountInString(query) > models.MaxSearchQueryRunes || strings.IndexFunc(query, unicode.IsControl) >= 0 {
		return fmt.Errorf("query must be valid UTF-8 and contain at most %d characters", models.MaxSearchQueryRunes)
	}
	words := strings.Fields(query)
	if len(words) < 1 || len(words) > models.MaxSearchQueryWords {
		return fmt.Errorf("query must contain between 1 and %d words", models.MaxSearchQueryWords)
	}
	return nil
}

func optionalSearchDomains(arguments map[string]any) ([]string, error) {
	value, present := arguments["domains"]
	if !present {
		return nil, nil
	}
	var values []any
	switch typed := value.(type) {
	case []any:
		values = typed
	case []string:
		values = make([]any, len(typed))
		for index := range typed {
			values[index] = typed[index]
		}
	default:
		return nil, fmt.Errorf("domains must be an array of strings")
	}
	if len(values) > models.MaxSearchDomains {
		return nil, fmt.Errorf("domains must contain at most %d entries", models.MaxSearchDomains)
	}
	result := make([]string, len(values))
	for index, value := range values {
		domain, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("domains[%d] must be a string", index)
		}
		trimmed := strings.TrimSuffix(strings.TrimSpace(domain), ".")
		if trimmed == "" || len(domain) > maxSearchRawDomainBytes || !utf8.ValidString(domain) ||
			strings.IndexFunc(domain, unicode.IsControl) >= 0 || strings.ContainsAny(trimmed, "/\\:@?#*[]") {
			return nil, fmt.Errorf("domains[%d] must be a registrable hostname", index)
		}
		canonical, err := idna.Lookup.ToASCII(trimmed)
		if err != nil {
			return nil, fmt.Errorf("domains[%d] must be a registrable hostname", index)
		}
		canonical = strings.ToLower(canonical)
		if canonical == "" || len(canonical) > models.MaxSearchDomainBytes || canonical == "localhost" ||
			strings.HasSuffix(canonical, ".localhost") {
			return nil, fmt.Errorf("domains[%d] must be a registrable hostname", index)
		}
		if _, err := netip.ParseAddr(canonical); err == nil {
			return nil, fmt.Errorf("domains[%d] must be a registrable hostname", index)
		}
		for _, label := range strings.Split(canonical, ".") {
			if !validSearchDomainLabel(label) {
				return nil, fmt.Errorf("domains[%d] must be a registrable hostname", index)
			}
		}
		if _, err := publicsuffix.EffectiveTLDPlusOne(canonical); err != nil {
			return nil, fmt.Errorf("domains[%d] must be a registrable hostname", index)
		}
		result[index] = domain
	}
	return result, nil
}

func validSearchDomainLabel(label string) bool {
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

func decodeSearchSchema(raw string) (json.RawMessage, error) {
	if len(raw) > models.MaxSearchSchemaBytes {
		return nil, fmt.Errorf("schema exceeds %d-byte limit", models.MaxSearchSchemaBytes)
	}
	if !utf8.ValidString(raw) {
		return nil, fmt.Errorf("schema must be valid UTF-8")
	}
	if err := rejectDuplicateJSONFields([]byte(raw)); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var schema json.RawMessage
	if err := decoder.Decode(&schema); err != nil {
		return nil, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	normalized, err := llm.NormalizeSchema(schema)
	if err != nil {
		return nil, err
	}
	if len(normalized) > models.MaxSearchSchemaBytes {
		return nil, fmt.Errorf("normalized schema exceeds %d-byte limit", models.MaxSearchSchemaBytes)
	}
	if err := llm.ValidateSchema(normalized); err != nil {
		return nil, err
	}
	return append(json.RawMessage(nil), normalized...), nil
}

func validateSearchTextOption(name, value string, maximum int, allowEmpty bool) error {
	if value == "" && allowEmpty {
		return nil
	}
	if (!allowEmpty && strings.TrimSpace(value) == "") || len(value) > maximum || !utf8.ValidString(value) ||
		strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("%s is invalid or exceeds the %d-byte limit", name, maximum)
	}
	return nil
}

func decodeSearchResponse(body []byte) (models.SearchResponse, error) {
	if !utf8.Valid(body) {
		return models.SearchResponse{}, fmt.Errorf("response must be valid UTF-8")
	}
	if err := rejectDuplicateJSONFields(body); err != nil {
		return models.SearchResponse{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var response *models.SearchResponse
	if err := decoder.Decode(&response); err != nil {
		return models.SearchResponse{}, err
	}
	if response == nil {
		return models.SearchResponse{}, fmt.Errorf("response must be a JSON object")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return models.SearchResponse{}, err
	}
	fields, err := decodeExtractJSONObject(body, "response")
	if err != nil {
		return models.SearchResponse{}, err
	}
	if err := rejectUnsupportedJSONFields(fields, "response",
		"success", "query", "results", "deduplicated", "dropped_stale", "partial", "timing", "ranking", "error"); err != nil {
		return models.SearchResponse{}, err
	}
	if err := requirePresentJSONFields(fields, "response", "success", "query", "results", "deduplicated", "dropped_stale", "partial", "timing"); err != nil {
		return models.SearchResponse{}, err
	}
	if response.Ranking == nil {
		if _, present := fields["ranking"]; present {
			return models.SearchResponse{}, fmt.Errorf("response ranking must not be null")
		}
	} else {
		rawRanking, present := fields["ranking"]
		if !present {
			return models.SearchResponse{}, fmt.Errorf("response is missing ranking")
		}
		if err := validateSearchResponseRanking(*response.Ranking, rawRanking); err != nil {
			return models.SearchResponse{}, err
		}
	}
	if response.Results == nil {
		return models.SearchResponse{}, fmt.Errorf("response results must be a non-null JSON array")
	}
	if response.Deduplicated < 0 || response.DroppedStale < 0 {
		return models.SearchResponse{}, fmt.Errorf("response counters must be non-negative")
	}
	if err := validateSearchTiming(response.Timing, fields["timing"]); err != nil {
		return models.SearchResponse{}, err
	}
	if response.Success {
		if _, present := fields["error"]; present || response.Error != nil {
			return models.SearchResponse{}, fmt.Errorf("successful response must not contain an error")
		}
		if err := validateSearchQuery(response.Query); err != nil {
			return models.SearchResponse{}, fmt.Errorf("successful response contains an invalid query")
		}
		if err := validateSearchResults(response.Results, fields["results"]); err != nil {
			return models.SearchResponse{}, err
		}
		hasErrors := false
		for _, result := range response.Results {
			hasErrors = hasErrors || len(result.Errors) > 0
		}
		if response.Partial != hasErrors {
			return models.SearchResponse{}, fmt.Errorf("response partial status is inconsistent with result errors")
		}
		return *response, nil
	}
	if len(response.Results) != 0 || response.Partial {
		return models.SearchResponse{}, fmt.Errorf("unsuccessful response must not contain results")
	}
	if response.Ranking != nil || response.Timing.RerankMs != nil {
		return models.SearchResponse{}, fmt.Errorf("unsuccessful response must not contain ranking diagnostics")
	}
	rawError, present := fields["error"]
	if !present {
		return models.SearchResponse{}, fmt.Errorf("unsuccessful response is missing an error")
	}
	errorFields, err := decodeExtractJSONObject(rawError, "response error")
	if err != nil {
		return models.SearchResponse{}, err
	}
	if err := rejectUnsupportedJSONFields(errorFields, "response error", "code", "message"); err != nil {
		return models.SearchResponse{}, err
	}
	if err := validateExtractErrorDetail(response.Error, rawError, "response error"); err != nil {
		return models.SearchResponse{}, err
	}
	return *response, nil
}

func validateSearchResponseForRequest(response models.SearchResponse, request searchAPIPayload) error {
	wantQuery := strings.Join(strings.Fields(request.Query), " ")
	if response.Query != wantQuery {
		return fmt.Errorf("response query does not match the request")
	}
	resolved := models.ResolveSearchDefaults(request.Ranking, request.Limit, request.Timeout)
	if resolved.Limit < 1 || len(response.Results) > resolved.Limit {
		return fmt.Errorf("response exceeds the requested result limit")
	}
	if err := validateSearchRankingForRequest(response, resolved.Ranking); err != nil {
		return err
	}
	hasSchema := len(request.Schema) > 0
	hasHeavyCapability := request.IncludeContent || request.Verify || hasSchema
	for index, result := range response.Results {
		name := fmt.Sprintf("result %d", index)
		if !request.IncludeContent && result.Content != "" {
			return fmt.Errorf("%s contains unrequested content", name)
		}
		if !request.Verify && (result.VerificationStatus != models.SearchVerificationNotChecked || result.Verified != nil ||
			result.Evidence != nil || result.Receipt != "") {
			return fmt.Errorf("%s contains unrequested verification", name)
		}
		if !hasSchema && searchResultHasExtraction(result) {
			return fmt.Errorf("%s contains unrequested extraction", name)
		}
		if hasSchema && len(result.Data) > 0 {
			authoritative, err := llm.ValidateAgainstSchema(request.Schema, result.Data)
			if err != nil || !equalSearchSchemaViolations(result.Violations, authoritative) {
				return fmt.Errorf("%s extraction violations do not match the requested schema", name)
			}
			if len(authoritative) > 0 && !hasSearchExtractionPartialMarker(result.Errors) {
				return fmt.Errorf("%s is missing the extraction partial marker", name)
			}
		}
		if !hasHeavyCapability && (result.FinalURL != "" || len(result.Errors) > 0) {
			return fmt.Errorf("%s contains unrequested enrichment", name)
		}
		if index >= models.MaxSearchHeavyResults && searchResultHasEnrichment(result) {
			return fmt.Errorf("%s contains enrichment beyond the top-five limit", name)
		}
		if !models.ValidSearchResultEnrichmentFlow(models.SearchEnrichmentRequirements{
			IncludeContent: request.IncludeContent,
			Verify:         request.Verify,
			Extract:        hasSchema,
			AttemptRequired: hasHeavyCapability && models.SearchResultEnrichmentAttemptRequired(
				resolved.Limit, index, response.Deduplicated, response.DroppedStale,
			),
		}, result) {
			return fmt.Errorf("%s contains an invalid enrichment flow", name)
		}
		for _, resultError := range result.Errors {
			switch resultError.Stage {
			case models.SearchResultStageFetch:
				if !hasHeavyCapability {
					return fmt.Errorf("%s contains an unrequested fetch error", name)
				}
			case models.SearchResultStageVerify:
				if !request.Verify {
					return fmt.Errorf("%s contains an unrequested verification error", name)
				}
			case models.SearchResultStageExtract:
				if !hasSchema {
					return fmt.Errorf("%s contains an unrequested extraction error", name)
				}
			}
		}
	}
	return nil
}

func hasSearchExtractionPartialMarker(errorsList []models.SearchResultError) bool {
	for _, resultError := range errorsList {
		if resultError.Stage == models.SearchResultStageExtract {
			return resultError.Code == models.ErrCodeLLMFailure && resultError.Message == "result extraction was partial"
		}
	}
	return false
}

func equalSearchSchemaViolations(left, right []models.SchemaViolation) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func validateSearchRankingForRequest(response models.SearchResponse, ranking models.SearchRankingMode) error {
	if response.Ranking != nil && response.Ranking.CandidateCount < len(response.Results) {
		return fmt.Errorf("response ranking candidate_count is smaller than the result set")
	}
	switch ranking {
	case models.SearchRankingProvider:
		if response.Ranking != nil || response.Timing.RerankMs != nil {
			return fmt.Errorf("provider response contains unrequested ranking diagnostics")
		}
		for _, result := range response.Results {
			if result.Ranking != nil {
				return fmt.Errorf("provider response contains unrequested result ranking")
			}
		}
		return nil
	case models.SearchRankingRelevance:
		if response.Ranking == nil || response.Ranking.Mode != models.SearchRankingRelevance || response.Timing.RerankMs == nil {
			return fmt.Errorf("relevance response has an invalid ranking envelope")
		}
		switch response.Ranking.Status {
		case models.SearchRankingApplied:
			if response.Ranking.CandidateCount == 0 && (len(response.Results) != 0 || *response.Timing.RerankMs != 0) {
				return fmt.Errorf("relevance applied empty pool has invalid results or timing")
			}
			seenProviderRanks := make(map[int]struct{}, len(response.Results))
			for index, result := range response.Results {
				if result.Ranking == nil || result.Ranking.RelevanceScore == nil {
					return fmt.Errorf("relevance applied response has an invalid result ranking")
				}
				if _, duplicate := seenProviderRanks[result.Ranking.ProviderRank]; duplicate {
					return fmt.Errorf("relevance applied response contains a duplicate provider rank")
				}
				seenProviderRanks[result.Ranking.ProviderRank] = struct{}{}
				if index > 0 && !searchRelevanceResultBefore(response.Results[index-1], result) {
					return fmt.Errorf("relevance applied response results are not in strict ranking order")
				}
			}
		case models.SearchRankingDegraded:
			if response.Ranking.DegradedReason != models.SearchRankingReasonRerankerFailed || response.Ranking.CandidateCount == 0 {
				return fmt.Errorf("relevance degraded response has an invalid reason")
			}
			for _, result := range response.Results {
				if result.Ranking != nil {
					return fmt.Errorf("relevance degraded response contains result ranking")
				}
			}
		default:
			return fmt.Errorf("relevance response has an invalid status")
		}
		return nil
	case models.SearchRankingTrust:
		return fmt.Errorf("trust ranking is unavailable")
	default:
		return fmt.Errorf("response request ranking is invalid")
	}
}

func searchRelevanceResultBefore(left, right models.SearchResult) bool {
	leftScore := *left.Ranking.RelevanceScore
	rightScore := *right.Ranking.RelevanceScore
	if leftScore != rightScore {
		return leftScore > rightScore
	}
	if left.Ranking.ProviderRank != right.Ranking.ProviderRank {
		return left.Ranking.ProviderRank < right.Ranking.ProviderRank
	}
	return left.URL < right.URL
}

func validateSearchResponseRanking(ranking models.SearchResponseRanking, raw json.RawMessage) error {
	fields, err := decodeExtractJSONObject(raw, "response ranking")
	if err != nil {
		return err
	}
	if err := rejectUnsupportedJSONFields(fields, "response ranking", "mode", "status", "degraded_reason", "candidate_count"); err != nil {
		return err
	}
	if err := requirePresentJSONFields(fields, "response ranking", "mode", "status", "candidate_count"); err != nil {
		return err
	}
	if ranking.CandidateCount < 0 || ranking.CandidateCount > models.MaxSearchLimit {
		return fmt.Errorf("response ranking candidate_count is invalid")
	}
	if ranking.Status == models.SearchRankingDegraded {
		if _, present := fields["degraded_reason"]; !present || ranking.DegradedReason == "" {
			return fmt.Errorf("degraded response ranking is missing degraded_reason")
		}
	} else if _, present := fields["degraded_reason"]; present || ranking.DegradedReason != "" {
		return fmt.Errorf("non-degraded response ranking contains degraded_reason")
	}
	if ranking.Mode != models.SearchRankingRelevance {
		return fmt.Errorf("response ranking has an invalid mode")
	}
	if ranking.Status != models.SearchRankingApplied && ranking.Status != models.SearchRankingDegraded {
		return fmt.Errorf("relevance response ranking has an invalid status")
	}
	if ranking.Status == models.SearchRankingDegraded && ranking.DegradedReason != models.SearchRankingReasonRerankerFailed {
		return fmt.Errorf("relevance response ranking has an invalid degraded reason")
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

func searchResultRequiresFinalURL(result models.SearchResult) bool {
	if result.Content != "" || result.Verified != nil || result.VerificationStatus != models.SearchVerificationNotChecked ||
		result.Evidence != nil || result.Receipt != "" || searchResultHasExtraction(result) {
		return true
	}
	for _, resultError := range result.Errors {
		if resultError.Stage == models.SearchResultStageVerify || resultError.Stage == models.SearchResultStageExtract {
			return true
		}
	}
	return false
}

func validateSearchTiming(timing models.SearchTimingInfo, raw json.RawMessage) error {
	fields, err := decodeExtractJSONObject(raw, "response timing")
	if err != nil {
		return err
	}
	if err := rejectUnsupportedJSONFields(fields, "response timing", "total_ms", "provider_ms", "enrichment_ms", "rerank_ms"); err != nil {
		return err
	}
	if err := requirePresentJSONFields(fields, "response timing", "total_ms", "provider_ms", "enrichment_ms"); err != nil {
		return err
	}
	additional := int64(0)
	rawRerank, rerankPresent := fields["rerank_ms"]
	if timing.RerankMs == nil {
		if rerankPresent {
			return fmt.Errorf("response timing rerank_ms must not be null")
		}
	} else {
		if !rerankPresent || bytes.Equal(bytes.TrimSpace(rawRerank), []byte("null")) || *timing.RerankMs < 0 {
			return fmt.Errorf("response timing rerank_ms is invalid")
		}
		additional = *timing.RerankMs
	}
	if timing.TotalMs < 0 || timing.ProviderMs < 0 || timing.EnrichmentMs < 0 ||
		timing.ProviderMs > math.MaxInt64-timing.EnrichmentMs || timing.ProviderMs+timing.EnrichmentMs > math.MaxInt64-additional ||
		timing.TotalMs < timing.ProviderMs+timing.EnrichmentMs+additional {
		return fmt.Errorf("response timing must be non-negative")
	}
	return nil
}

func validateSearchResults(results []models.SearchResult, raw json.RawMessage) error {
	var rawResults []json.RawMessage
	if err := json.Unmarshal(raw, &rawResults); err != nil || rawResults == nil || len(rawResults) != len(results) {
		return fmt.Errorf("response results must be a matching JSON array")
	}
	if len(results) > models.MaxSearchLimit {
		return fmt.Errorf("response contains too many results")
	}
	seenURLs := make(map[string]struct{}, len(results))
	seenIdentities := make(map[string]struct{}, len(results))
	for index, result := range results {
		name := fmt.Sprintf("result %d", index)
		fields, err := decodeExtractJSONObject(rawResults[index], name)
		if err != nil {
			return err
		}
		if err := rejectUnsupportedJSONFields(fields, name,
			"rank", "score", "title", "url", "final_url", "snippet", "published_at", "content", "verified",
			"verification_status", "evidence", "receipt", "data", "basis", "receipts", "unlocated_rate",
			"extractor", "llm_usage", "violations", "errors", "ranking"); err != nil {
			return err
		}
		if err := requirePresentJSONFields(fields, name, "rank", "title", "url", "verification_status"); err != nil {
			return err
		}
		if result.Rank != index+1 {
			return fmt.Errorf("%s has an invalid rank", name)
		}
		if len(result.Title) > models.MaxSearchResultTitleBytes || !utf8.ValidString(result.Title) ||
			result.Title != strings.TrimSpace(result.Title) || strings.IndexFunc(result.Title, unicode.IsControl) >= 0 {
			return fmt.Errorf("%s title is invalid", name)
		}
		if result.Snippet != "" && (len(result.Snippet) > models.MaxSearchResultSnippetBytes || !utf8.ValidString(result.Snippet) ||
			result.Snippet != strings.TrimSpace(result.Snippet) || strings.IndexFunc(result.Snippet, unicode.IsControl) >= 0) {
			return fmt.Errorf("%s snippet is invalid", name)
		}
		if result.Content != "" && (len(result.Content) > models.MaxSearchResultContentBytes || !utf8.ValidString(result.Content) ||
			strings.TrimSpace(result.Content) == "") {
			return fmt.Errorf("%s content is invalid", name)
		}
		canonicalURL, _, err := publicnet.NormalizeHTTPURL(result.URL, nil, false)
		if err != nil || canonicalURL != result.URL || len(result.URL) > models.MaxSearchURLBytes {
			return fmt.Errorf("%s url is not canonical", name)
		}
		if _, duplicate := seenURLs[canonicalURL]; duplicate {
			return fmt.Errorf("response contains a duplicate result URL")
		}
		seenURLs[canonicalURL] = struct{}{}
		effectiveIdentity := canonicalURL
		if result.FinalURL != "" {
			if _, ok := fields["final_url"]; !ok {
				return fmt.Errorf("%s is missing final_url", name)
			}
			canonicalFinalURL, _, normalizeErr := publicnet.NormalizeHTTPURL(result.FinalURL, nil, false)
			if normalizeErr != nil || canonicalFinalURL != result.FinalURL || len(result.FinalURL) > models.MaxSearchURLBytes {
				return fmt.Errorf("%s final_url is not canonical", name)
			}
			effectiveIdentity = canonicalFinalURL
		} else if _, present := fields["final_url"]; present {
			return fmt.Errorf("%s final_url must not be empty or null", name)
		}
		if _, duplicate := seenIdentities[effectiveIdentity]; duplicate {
			return fmt.Errorf("response contains a duplicate effective result identity")
		}
		seenIdentities[effectiveIdentity] = struct{}{}
		if result.Score != nil {
			if _, present := fields["score"]; !present || math.IsNaN(*result.Score) || math.IsInf(*result.Score, 0) || *result.Score < 0 || *result.Score > 1 {
				return fmt.Errorf("%s score is invalid", name)
			}
		} else if _, present := fields["score"]; present {
			return fmt.Errorf("%s score must not be null", name)
		}
		if result.PublishedAt != nil {
			if _, present := fields["published_at"]; !present || result.PublishedAt.IsZero() {
				return fmt.Errorf("%s published_at is invalid", name)
			}
		} else if _, present := fields["published_at"]; present {
			return fmt.Errorf("%s published_at must not be null", name)
		}
		for field, value := range map[string]string{"snippet": result.Snippet, "content": result.Content} {
			_, present := fields[field]
			if value != "" && !present {
				return fmt.Errorf("%s is missing %s", name, field)
			}
			if value == "" && present {
				return fmt.Errorf("%s %s must not be empty or null", name, field)
			}
		}
		if err := validateSearchVerification(result, fields, name); err != nil {
			return err
		}
		if err := validateSearchExtractionResult(result, fields, name); err != nil {
			return err
		}
		if err := validateSearchResultErrors(result, fields, name); err != nil {
			return err
		}
		if result.FinalURL == "" && searchResultRequiresFinalURL(result) {
			return fmt.Errorf("%s contains completed enrichment without final_url", name)
		}
		if err := validateSearchResultRanking(result.Ranking, fields["ranking"], name+" ranking"); err != nil {
			return err
		}
	}
	return nil
}

func validateSearchResultRanking(ranking *models.SearchResultRanking, raw json.RawMessage, name string) error {
	if ranking == nil {
		if raw != nil {
			return fmt.Errorf("%s must not be null", name)
		}
		return nil
	}
	if raw == nil {
		return fmt.Errorf("%s is missing", name)
	}
	fields, err := decodeExtractJSONObject(raw, name)
	if err != nil {
		return err
	}
	if err := rejectUnsupportedJSONFields(fields, name, "provider_rank", "relevance_score"); err != nil {
		return err
	}
	if err := requirePresentJSONFields(fields, name, "provider_rank"); err != nil {
		return err
	}
	if ranking.ProviderRank < 1 || ranking.ProviderRank > models.MaxSearchLimit {
		return fmt.Errorf("%s provider_rank is invalid", name)
	}
	if ranking.RelevanceScore == nil {
		if _, present := fields["relevance_score"]; present {
			return fmt.Errorf("%s relevance_score must not be null", name)
		}
	} else if rawScore, present := fields["relevance_score"]; !present || bytes.Equal(bytes.TrimSpace(rawScore), []byte("null")) ||
		math.IsNaN(*ranking.RelevanceScore) || math.IsInf(*ranking.RelevanceScore, 0) || *ranking.RelevanceScore < 0 || *ranking.RelevanceScore > 1 {
		return fmt.Errorf("%s relevance_score is invalid", name)
	}
	return nil
}

func validateSearchVerification(result models.SearchResult, fields map[string]json.RawMessage, name string) error {
	if result.Verified == nil {
		if _, present := fields["verified"]; present {
			return fmt.Errorf("%s verified must not be null", name)
		}
	} else if _, present := fields["verified"]; !present {
		return fmt.Errorf("%s is missing verified", name)
	}
	if result.Evidence == nil {
		if _, present := fields["evidence"]; present {
			return fmt.Errorf("%s evidence must not be null", name)
		}
	} else {
		rawEvidence, present := fields["evidence"]
		if !present {
			return fmt.Errorf("%s is missing evidence", name)
		}
		if err := validateSearchResponseAnchor(*result.Evidence, rawEvidence, name+" evidence", false); err != nil {
			return err
		}
	}
	switch result.VerificationStatus {
	case models.SearchVerificationNotChecked:
		if result.Verified != nil || result.Evidence != nil || result.Receipt != "" {
			return fmt.Errorf("%s has an invalid not-checked verification shape", name)
		}
	case models.SearchVerificationVerified:
		if result.Verified == nil || !*result.Verified || result.Evidence == nil || strings.TrimSpace(result.Receipt) == "" {
			return fmt.Errorf("%s has an invalid verified shape", name)
		}
		if result.Evidence.Method != evidence.MethodExact && result.Evidence.Method != evidence.MethodNormalized && result.Evidence.Method != evidence.MethodFuzzy {
			return fmt.Errorf("%s has an invalid verification evidence method", name)
		}
	case models.SearchVerificationMismatch, models.SearchVerificationUnavailable:
		if result.Verified == nil || *result.Verified || result.Evidence != nil || result.Receipt != "" {
			return fmt.Errorf("%s has an invalid negative verification shape", name)
		}
	default:
		return fmt.Errorf("%s has an invalid verification status", name)
	}
	if result.Receipt != "" {
		if _, present := fields["receipt"]; !present || len(result.Receipt) > models.MaxSearchResultReceiptBytes ||
			!utf8.ValidString(result.Receipt) || strings.IndexFunc(result.Receipt, unicode.IsControl) >= 0 {
			return fmt.Errorf("%s receipt is invalid", name)
		}
	} else if _, present := fields["receipt"]; present {
		return fmt.Errorf("%s receipt must not be empty or null", name)
	}
	return nil
}

func validateSearchExtractionResult(result models.SearchResult, fields map[string]json.RawMessage, name string) error {
	_, hasData := fields["data"]
	_, hasBasis := fields["basis"]
	_, hasReceipts := fields["receipts"]
	_, hasUnlocatedRate := fields["unlocated_rate"]
	_, hasExtractor := fields["extractor"]
	_, hasLLMUsage := fields["llm_usage"]
	_, hasViolations := fields["violations"]
	hasExtraction := hasData || hasBasis || hasReceipts || hasUnlocatedRate
	if hasExtraction && (!hasData || !hasBasis || !hasReceipts || !hasUnlocatedRate) {
		return fmt.Errorf("%s contains an incomplete extraction result", name)
	}
	if !hasExtraction {
		if hasExtractor || hasLLMUsage || hasViolations || len(result.Data) != 0 || result.Basis != nil || result.Receipts != nil || result.UnlocatedRate != nil ||
			result.Extractor != nil || result.LLMUsage != nil || len(result.Violations) != 0 {
			return fmt.Errorf("%s contains extraction metadata without data", name)
		}
		return nil
	}
	if len(result.Data) == 0 || len(result.Data) > models.MaxSearchResultDataBytes || !json.Valid(result.Data) ||
		result.Basis == nil || result.Receipts == nil || result.UnlocatedRate == nil ||
		math.IsNaN(*result.UnlocatedRate) || math.IsInf(*result.UnlocatedRate, 0) || *result.UnlocatedRate < 0 || *result.UnlocatedRate > 1 {
		return fmt.Errorf("%s extraction result is invalid", name)
	}
	if _, err := consensus.Merge([]consensus.SourceResult{{URL: result.URL, Data: result.Data}}); err != nil {
		return fmt.Errorf("%s extraction data exceeds its production bounds", name)
	}
	leaves, err := evidence.LeafValues(result.Data)
	if err != nil || len(leaves) > consensus.MaxLeavesPerSource || len(*result.Basis) != len(leaves) ||
		len(*result.Receipts) != len(leaves) || len(result.Violations) > consensus.MaxLeavesPerSource {
		return fmt.Errorf("%s extraction evidence does not match its data", name)
	}
	if bytes.Equal(bytes.TrimSpace(fields["basis"]), []byte("null")) || bytes.Equal(bytes.TrimSpace(fields["receipts"]), []byte("null")) ||
		bytes.Equal(bytes.TrimSpace(fields["unlocated_rate"]), []byte("null")) {
		return fmt.Errorf("%s extraction metadata must not be null", name)
	}
	if len(*result.Basis) != len(*result.Receipts) {
		return fmt.Errorf("%s extraction evidence and receipts differ", name)
	}
	var rawBasis map[string]json.RawMessage
	if err := json.Unmarshal(fields["basis"], &rawBasis); err != nil || rawBasis == nil || len(rawBasis) != len(*result.Basis) {
		return fmt.Errorf("%s basis must be a matching JSON object", name)
	}
	for path, anchor := range *result.Basis {
		rawAnchor, present := rawBasis[path]
		if !present || strings.TrimSpace(path) == "" || len(path) > consensus.MaxPathBytes || !utf8.ValidString(path) {
			return fmt.Errorf("%s basis contains an invalid path", name)
		}
		if _, present := leaves[path]; !present {
			return fmt.Errorf("%s basis path is absent from extraction data", name)
		}
		if err := validateSearchResponseAnchor(anchor, rawAnchor, name+" basis", true); err != nil {
			return err
		}
		if result.Evidence != nil && (anchor.SnapshotID != result.Evidence.SnapshotID || !anchor.FetchedAt.Equal(result.Evidence.FetchedAt)) {
			return fmt.Errorf("%s basis observation differs from snippet evidence", name)
		}
		receipt, present := (*result.Receipts)[path]
		if !present || strings.TrimSpace(receipt) == "" || len(receipt) > consensus.MaxReceiptBytes ||
			!utf8.ValidString(receipt) || strings.IndexFunc(receipt, unicode.IsControl) >= 0 {
			return fmt.Errorf("%s receipts contain an invalid token", name)
		}
	}
	var observationSnapshot string
	var observationTime time.Time
	for _, anchor := range *result.Basis {
		if observationSnapshot == "" {
			observationSnapshot = anchor.SnapshotID
			observationTime = anchor.FetchedAt
			continue
		}
		if anchor.SnapshotID != observationSnapshot || !anchor.FetchedAt.Equal(observationTime) {
			return fmt.Errorf("%s basis contains mixed artifact observations", name)
		}
	}
	unlocated := 0
	for _, anchor := range *result.Basis {
		if anchor.Method == evidence.MethodUnlocated || anchor.Method == evidence.MethodCompiled && anchor.TextRange == [2]int{} {
			unlocated++
		}
	}
	wantUnlocatedRate := 0.0
	if len(leaves) > 0 {
		wantUnlocatedRate = float64(unlocated) / float64(len(leaves))
	}
	if *result.UnlocatedRate != wantUnlocatedRate {
		return fmt.Errorf("%s unlocated_rate is inconsistent with its evidence", name)
	}
	if result.Extractor != nil {
		rawExtractor, present := fields["extractor"]
		if !present {
			return fmt.Errorf("%s is missing extractor", name)
		}
		if err := requireNestedJSONFields(rawExtractor, name+" extractor", "id", "version", "compiled_at", "validation", "mode"); err != nil {
			return err
		}
		extractorFields, err := decodeExtractJSONObject(rawExtractor, name+" extractor")
		if err != nil {
			return err
		}
		if err := rejectUnsupportedJSONFields(extractorFields, name+" extractor", "id", "version", "compiled_at", "validation", "mode"); err != nil {
			return err
		}
		if strings.TrimSpace(result.Extractor.ID) == "" || len(result.Extractor.ID) > consensus.MaxPathBytes ||
			len(result.Extractor.Mode) > 64 || result.Extractor.Version <= 0 || result.Extractor.CompiledAt.IsZero() ||
			math.IsNaN(result.Extractor.Validation) || math.IsInf(result.Extractor.Validation, 0) ||
			result.Extractor.Validation < 0 || result.Extractor.Validation > 1 || result.Extractor.Mode != "compiled" {
			return fmt.Errorf("%s extractor is invalid", name)
		}
	} else if _, present := fields["extractor"]; present {
		return fmt.Errorf("%s extractor must not be null", name)
	}
	if result.LLMUsage != nil {
		rawUsage, present := fields["llm_usage"]
		if !present {
			return fmt.Errorf("%s is missing llm_usage", name)
		}
		usageFields, err := decodeExtractJSONObject(rawUsage, name+" llm_usage")
		if err != nil {
			return err
		}
		if err := rejectUnsupportedJSONFields(usageFields, name+" llm_usage", "prompt_tokens", "completion_tokens", "total_tokens"); err != nil {
			return err
		}
		if err := validateExtractLLMUsage(result.LLMUsage, rawUsage, name+" llm_usage"); err != nil {
			return err
		}
	} else if _, present := fields["llm_usage"]; present {
		return fmt.Errorf("%s llm_usage must not be null", name)
	}
	if rawViolations, present := fields["violations"]; present {
		var violations []json.RawMessage
		if err := json.Unmarshal(rawViolations, &violations); err != nil || violations == nil || len(violations) != len(result.Violations) || len(violations) == 0 {
			return fmt.Errorf("%s violations must be a matching non-empty JSON array", name)
		}
		for index, rawViolation := range violations {
			violationName := fmt.Sprintf("%s violation %d", name, index)
			violationFields, err := decodeExtractJSONObject(rawViolation, violationName)
			if err != nil {
				return err
			}
			if err := rejectUnsupportedJSONFields(violationFields, violationName, "path", "message"); err != nil {
				return err
			}
			if err := requirePresentJSONFields(violationFields, violationName, "path", "message"); err != nil {
				return err
			}
			violation := result.Violations[index]
			if len(violation.Path) == 0 || len(violation.Path) > consensus.MaxPathBytes || !utf8.ValidString(violation.Path) ||
				len(violation.Message) > models.MaxSearchResultSnippetBytes || !utf8.ValidString(violation.Message) {
				return fmt.Errorf("%s is outside production bounds", violationName)
			}
		}
	} else if len(result.Violations) != 0 {
		return fmt.Errorf("%s is missing violations", name)
	}
	return nil
}

func validateSearchResponseAnchor(anchor evidence.Anchor, raw json.RawMessage, name string, allowUnlocated bool) error {
	fields, err := decodeExtractJSONObject(raw, name)
	if err != nil {
		return err
	}
	if err := requirePresentJSONFields(fields, name, "quote", "text_range", "method", "snapshot_id", "fetched_at"); err != nil {
		return err
	}
	if err := rejectUnsupportedJSONFields(fields, name, "quote", "text_range", "method", "selector", "snapshot_id", "fetched_at"); err != nil {
		return err
	}
	var textRange []json.RawMessage
	if err := json.Unmarshal(fields["text_range"], &textRange); err != nil || len(textRange) != 2 {
		return fmt.Errorf("%s text_range must contain two offsets", name)
	}
	for index, rawOffset := range textRange {
		if bytes.Equal(bytes.TrimSpace(rawOffset), []byte("null")) {
			return fmt.Errorf("%s text_range must contain integer offsets", name)
		}
		var offset int
		if err := json.Unmarshal(rawOffset, &offset); err != nil || offset != anchor.TextRange[index] {
			return fmt.Errorf("%s text_range must contain integer offsets", name)
		}
	}
	if !validSearchResponseSnapshotID(anchor.SnapshotID) || anchor.FetchedAt.IsZero() ||
		len(anchor.Quote) > consensus.MaxEvidenceQuoteBytes || len(anchor.Selector) > consensus.MaxEvidenceSelectorBytes ||
		!utf8.ValidString(anchor.Quote) || !utf8.ValidString(anchor.Selector) || strings.IndexFunc(anchor.Selector, unicode.IsControl) >= 0 {
		return fmt.Errorf("%s is incomplete", name)
	}
	if anchor.Selector == "" {
		if _, present := fields["selector"]; present {
			return fmt.Errorf("%s selector must not be empty or null", name)
		}
	} else if _, present := fields["selector"]; !present {
		return fmt.Errorf("%s is missing selector", name)
	}
	switch anchor.Method {
	case evidence.MethodExact, evidence.MethodNormalized, evidence.MethodFuzzy:
		if anchor.Quote == "" || anchor.TextRange[0] < 0 || anchor.TextRange[1] <= anchor.TextRange[0] ||
			anchor.TextRange[1]-anchor.TextRange[0] != len(anchor.Quote) {
			return fmt.Errorf("%s is not a located anchor", name)
		}
	case evidence.MethodCompiled:
		if anchor.TextRange != [2]int{} && (anchor.Quote == "" || anchor.TextRange[0] < 0 ||
			anchor.TextRange[1] <= anchor.TextRange[0] || anchor.TextRange[1]-anchor.TextRange[0] != len(anchor.Quote)) {
			return fmt.Errorf("%s contains invalid compiled evidence", name)
		}
	case evidence.MethodUnlocated:
		if !allowUnlocated || anchor.Quote != "" || anchor.Selector != "" || anchor.TextRange != [2]int{} {
			return fmt.Errorf("%s contains an invalid unlocated anchor", name)
		}
	default:
		return fmt.Errorf("%s contains an invalid method", name)
	}
	return nil
}

func validSearchResponseSnapshotID(snapshotID string) bool {
	if len(snapshotID) != len("sha256:")+64 || !strings.HasPrefix(snapshotID, "sha256:") {
		return false
	}
	for _, character := range snapshotID[len("sha256:"):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func rejectDuplicateJSONFields(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := consumeUniqueJSONValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func consumeUniqueJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 256 {
		return fmt.Errorf("JSON nesting exceeds the response limit")
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
				return fmt.Errorf("object field name must be a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("JSON contains a duplicate object field")
			}
			seen[key] = struct{}{}
			if err := consumeUniqueJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return fmt.Errorf("invalid JSON object")
		}
	case '[':
		for decoder.More() {
			if err := consumeUniqueJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return fmt.Errorf("invalid JSON array")
		}
	default:
		return fmt.Errorf("invalid JSON delimiter")
	}
	return nil
}

func validateSearchResultErrors(result models.SearchResult, fields map[string]json.RawMessage, name string) error {
	errorsList := result.Errors
	raw, present := fields["errors"]
	if !present {
		if len(errorsList) != 0 {
			return fmt.Errorf("%s is missing errors", name)
		}
		if result.VerificationStatus == models.SearchVerificationUnavailable || len(result.Violations) > 0 {
			return fmt.Errorf("%s is missing an enrichment error", name)
		}
		return nil
	}
	var rawErrors []json.RawMessage
	if err := json.Unmarshal(raw, &rawErrors); err != nil || rawErrors == nil || len(rawErrors) != len(errorsList) || len(rawErrors) == 0 {
		return fmt.Errorf("%s errors must be a matching non-empty JSON array", name)
	}
	seenStages := make(map[models.SearchResultErrorStage]struct{}, len(errorsList))
	hasVerifyError := false
	hasExtractError := false
	for index, resultError := range errorsList {
		errorName := fmt.Sprintf("%s error %d", name, index)
		errorFields, err := decodeExtractJSONObject(rawErrors[index], errorName)
		if err != nil {
			return err
		}
		if err := rejectUnsupportedJSONFields(errorFields, errorName, "stage", "code", "message"); err != nil {
			return err
		}
		if err := requirePresentJSONFields(errorFields, errorName, "stage", "code", "message"); err != nil {
			return err
		}
		switch resultError.Stage {
		case models.SearchResultStageFetch, models.SearchResultStageVerify, models.SearchResultStageExtract:
		default:
			return fmt.Errorf("%s contains an invalid stage", errorName)
		}
		if _, duplicate := seenStages[resultError.Stage]; duplicate {
			return fmt.Errorf("%s repeats an enrichment error stage", name)
		}
		seenStages[resultError.Stage] = struct{}{}
		hasVerifyError = hasVerifyError || resultError.Stage == models.SearchResultStageVerify
		hasExtractError = hasExtractError || resultError.Stage == models.SearchResultStageExtract
		if resultError.Stage == models.SearchResultStageExtract && len(result.Data) > 0 && len(result.Violations) > 0 &&
			(resultError.Code != models.ErrCodeLLMFailure || resultError.Message != "result extraction was partial") {
			return fmt.Errorf("%s contains an invalid extraction partial marker", name)
		}
		if strings.TrimSpace(resultError.Code) == "" || strings.TrimSpace(resultError.Message) == "" {
			return fmt.Errorf("%s is incomplete", errorName)
		}
	}
	if hasVerifyError != (result.VerificationStatus == models.SearchVerificationUnavailable) {
		return fmt.Errorf("%s verification error is inconsistent with its status", name)
	}
	if len(result.Data) > 0 && hasExtractError != (len(result.Violations) > 0) {
		return fmt.Errorf("%s extraction error is inconsistent with its violations", name)
	}
	return nil
}

func searchAPIError(statusCode int, body []byte) string {
	response, err := decodeSearchResponse(body)
	if err == nil && !response.Success {
		return formatSearchError(response.Error, statusCode)
	}
	return fmt.Sprintf("search failed (HTTP %d)", statusCode)
}

func formatSearchError(detail *models.ErrorDetail, statusCode int) string {
	if detail != nil {
		code := strings.TrimSpace(detail.Code)
		message := strings.TrimSpace(detail.Message)
		switch {
		case code != "" && message != "":
			return fmt.Sprintf("[%s] %s", code, message)
		case message != "":
			return message
		case code != "":
			return fmt.Sprintf("[%s] search failed (HTTP %d)", code, statusCode)
		}
	}
	return fmt.Sprintf("search failed (HTTP %d)", statusCode)
}

func redactSearchSecrets(message string, secrets ...string) string {
	for _, secret := range secrets {
		trimmed := strings.TrimSpace(secret)
		for _, candidate := range []string{secret, trimmed} {
			if candidate != "" {
				message = strings.ReplaceAll(message, candidate, "[REDACTED]")
			}
		}
	}
	return message
}

func searchJSONContainsSecret(body []byte, secrets ...string) (bool, error) {
	candidates := make([]string, 0, len(secrets)*2)
	for _, secret := range secrets {
		trimmed := strings.TrimSpace(secret)
		for _, candidate := range []string{secret, trimmed} {
			if candidate != "" {
				candidates = append(candidates, candidate)
			}
		}
	}
	if len(candidates) == 0 {
		return false, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		value, ok := token.(string)
		if !ok {
			continue
		}
		for _, candidate := range candidates {
			if strings.Contains(value, candidate) {
				return true, nil
			}
		}
	}
}

func decodeAnswerFactResponse(body []byte, minimum int) (models.AnswerResponse, error) {
	if minimum < 1 || minimum > models.MaxAnswerMinIndependentSources {
		return models.AnswerResponse{}, fmt.Errorf("requested independent-source minimum is invalid")
	}
	if !utf8.Valid(body) {
		return models.AnswerResponse{}, fmt.Errorf("response must be valid UTF-8")
	}
	if err := rejectDuplicateJSONFields(body); err != nil {
		return models.AnswerResponse{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var response *models.AnswerResponse
	if err := decoder.Decode(&response); err != nil {
		return models.AnswerResponse{}, err
	}
	if response == nil {
		return models.AnswerResponse{}, fmt.Errorf("response must be a JSON object")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return models.AnswerResponse{}, err
	}

	fields, err := answerObjectFields(body, "answer response", "status", "belief", "reason", "needs", "closest", "conflicts", "lease")
	if err != nil {
		return models.AnswerResponse{}, err
	}
	if err := requirePresentAnswerFields(fields, "answer response", "status"); err != nil {
		return models.AnswerResponse{}, err
	}
	if _, present := fields["belief"]; !present {
		return models.AnswerResponse{}, fmt.Errorf("answer response is missing required field %q", "belief")
	}
	if err := validateAnswerFactRawShape(*response, fields); err != nil {
		return models.AnswerResponse{}, err
	}
	if err := validateAnswerFactResponseForRequest(*response, minimum); err != nil {
		return models.AnswerResponse{}, err
	}
	return *response, nil
}

func validateAnswerFactRawShape(response models.AnswerResponse, fields map[string]json.RawMessage) error {
	if err := validateAnswerCandidateRawArray(fields["conflicts"], response.Conflicts, "answer conflicts", false); err != nil {
		return err
	}
	switch response.Status {
	case models.AnswerStatusKnown:
		if err := rejectPresentAnswerFields(fields, "known answer", "reason", "needs", "closest"); err != nil {
			return err
		}
		beliefRaw, present := fields["belief"]
		if !present || answerJSONNull(beliefRaw) || response.Belief == nil {
			return fmt.Errorf("known answer is missing belief")
		}
		leaseRaw, present := fields["lease"]
		if !present || answerJSONNull(leaseRaw) || response.Lease == nil {
			return fmt.Errorf("known answer is missing lease")
		}
		if err := validateAnswerBeliefRaw(*response.Belief, beliefRaw); err != nil {
			return err
		}
		return validateAnswerLeaseRaw(leaseRaw)
	case models.AnswerStatusUnknown:
		if !answerJSONNull(fields["belief"]) || response.Belief != nil {
			return fmt.Errorf("unknown answer belief must be null")
		}
		if err := rejectPresentAnswerFields(fields, "unknown answer", "lease"); err != nil {
			return err
		}
		if err := requirePresentAnswerFields(fields, "unknown answer", "reason", "needs"); err != nil {
			return err
		}
		if err := validateAnswerNeedsRaw(fields["needs"]); err != nil {
			return err
		}
		if rawClosest, present := fields["closest"]; present {
			if answerJSONNull(rawClosest) || response.Closest == nil {
				return fmt.Errorf("answer closest must not be null")
			}
			if err := validateAnswerClosestRaw(rawClosest); err != nil {
				return err
			}
		} else if response.Closest != nil {
			return fmt.Errorf("unknown answer is missing closest")
		}
		return nil
	default:
		return fmt.Errorf("answer response contains an invalid status")
	}
}

func validateAnswerBeliefRaw(belief models.AnswerBelief, raw json.RawMessage) error {
	fields, err := answerObjectFields(raw, "answer belief", "value", "confidence", "agreement", "as_of", "evidence", "receipts")
	if err != nil {
		return err
	}
	if err := requirePresentAnswerFields(fields, "answer belief", "value", "confidence", "agreement", "as_of", "evidence", "receipts"); err != nil {
		return err
	}
	if err := validateAnswerAgreementRaw(belief.Agreement, fields["agreement"], "answer belief agreement"); err != nil {
		return err
	}
	var rawEvidence []json.RawMessage
	if err := json.Unmarshal(fields["evidence"], &rawEvidence); err != nil || rawEvidence == nil || len(rawEvidence) != len(belief.Evidence) {
		return fmt.Errorf("answer belief evidence must be a matching JSON array")
	}
	for index := range rawEvidence {
		if err := validateAnswerEvidenceRaw(belief.Evidence[index], rawEvidence[index], fmt.Sprintf("answer evidence %d", index)); err != nil {
			return err
		}
	}
	var rawReceipts map[string]json.RawMessage
	if err := json.Unmarshal(fields["receipts"], &rawReceipts); err != nil || rawReceipts == nil || len(rawReceipts) != len(belief.Receipts) {
		return fmt.Errorf("answer belief receipts must be a matching JSON object")
	}
	return nil
}

func validateAnswerEvidenceRaw(item models.AnswerEvidence, raw json.RawMessage, name string) error {
	fields, err := answerObjectFields(raw, name, "url", "root", "quote", "text_range", "selector", "method", "snapshot_id", "fetched_at")
	if err != nil {
		return err
	}
	if err := requirePresentAnswerFields(fields, name, "url", "root", "quote", "text_range", "method", "snapshot_id", "fetched_at"); err != nil {
		return err
	}
	_, selectorPresent := fields["selector"]
	if selectorPresent != (item.Selector != "") {
		return fmt.Errorf("%s selector presence is invalid", name)
	}
	return nil
}

func validateAnswerLeaseRaw(raw json.RawMessage) error {
	fields, err := answerObjectFields(raw, "answer lease", "expires_at", "renew_url", "confidence_halflife_s")
	if err != nil {
		return err
	}
	return requirePresentAnswerFields(fields, "answer lease", "expires_at", "renew_url", "confidence_halflife_s")
}

func validateAnswerNeedsRaw(raw json.RawMessage) error {
	fields, err := answerObjectFields(raw, "answer needs", "more_independent_sources")
	if err != nil {
		return err
	}
	return requirePresentAnswerFields(fields, "answer needs", "more_independent_sources")
}

func validateAnswerClosestRaw(raw json.RawMessage) error {
	fields, err := answerObjectFields(raw, "answer closest", "value", "independent_roots", "note")
	if err != nil {
		return err
	}
	return requirePresentAnswerFields(fields, "answer closest", "value", "independent_roots", "note")
}

func validateAnswerAgreementRaw(agreement models.MultiExtractAgreement, raw json.RawMessage, name string) error {
	fields, err := answerObjectFields(raw, name, "pages", "independent_roots", "fold_reason")
	if err != nil {
		return err
	}
	if err := requirePresentAnswerFields(fields, name, "pages", "independent_roots"); err != nil {
		return err
	}
	return validateMultiExtractAgreementFoldReason(agreement, fields, name)
}

func validateAnswerCandidateRawArray(raw json.RawMessage, candidates []models.AnswerCandidate, name string, required bool) error {
	if len(raw) == 0 {
		if required || len(candidates) != 0 {
			return fmt.Errorf("%s is missing", name)
		}
		return nil
	}
	var rawCandidates []json.RawMessage
	if err := json.Unmarshal(raw, &rawCandidates); err != nil || rawCandidates == nil || len(rawCandidates) == 0 || len(rawCandidates) != len(candidates) {
		return fmt.Errorf("%s must be a matching non-empty JSON array", name)
	}
	for index, rawCandidate := range rawCandidates {
		candidateName := fmt.Sprintf("%s candidate %d", name, index)
		fields, err := answerObjectFields(rawCandidate, candidateName, "value", "agreement")
		if err != nil {
			return err
		}
		if _, present := fields["value"]; !present {
			return fmt.Errorf("%s is missing required field %q", candidateName, "value")
		}
		if err := requirePresentAnswerFields(fields, candidateName, "agreement"); err != nil {
			return err
		}
		if err := validateAnswerAgreementRaw(candidates[index].Agreement, fields["agreement"], candidateName+" agreement"); err != nil {
			return err
		}
	}
	return nil
}

func validateAnswerFactResponseForRequest(response models.AnswerResponse, minimum int) error {
	if len(response.Conflicts) > models.MaxExtractSources || !orderedAnswerFactCandidates(response.Conflicts) {
		return fmt.Errorf("answer conflicts exceed their ordering or count limit")
	}
	conflictValues := make(map[string]struct{}, len(response.Conflicts))
	for _, conflict := range response.Conflicts {
		if !validAnswerFactAgreement(conflict.Agreement) {
			return fmt.Errorf("answer contains an invalid conflict agreement")
		}
		canonical, ok := canonicalAnswerFactValue(conflict.Value, true)
		if !ok {
			return fmt.Errorf("answer contains an invalid conflict value")
		}
		if _, duplicate := conflictValues[canonical]; duplicate {
			return fmt.Errorf("answer repeats a conflict value")
		}
		conflictValues[canonical] = struct{}{}
	}

	switch response.Status {
	case models.AnswerStatusKnown:
		if response.Belief == nil || response.Reason != "" || response.Needs != nil || response.Closest != nil || response.Lease == nil {
			return fmt.Errorf("known answer has an invalid shape")
		}
		if err := validateKnownAnswerFact(*response.Belief, *response.Lease, minimum); err != nil {
			return err
		}
		if len(response.Conflicts)+1 > models.MaxExtractSources || !answerFactPageBudgetFits(response.Belief.Agreement.Pages, response.Conflicts) {
			return fmt.Errorf("known answer exceeds its candidate budget")
		}
		winnerValue, _ := canonicalAnswerFactValue(response.Belief.Value, false)
		for _, conflict := range response.Conflicts {
			candidateValue, _ := canonicalAnswerFactValue(conflict.Value, true)
			if candidateValue == winnerValue || conflict.Agreement.IndependentRoots >= response.Belief.Agreement.IndependentRoots {
				return fmt.Errorf("known answer contains an invalid alternative")
			}
		}
		return nil
	case models.AnswerStatusUnknown:
		return validateUnknownAnswerFact(response, minimum)
	default:
		return fmt.Errorf("answer response contains an invalid status")
	}
}

func validateKnownAnswerFact(belief models.AnswerBelief, lease models.AnswerLease, minimum int) error {
	if _, ok := canonicalAnswerFactValue(belief.Value, false); !ok || !validAnswerFactAgreement(belief.Agreement) ||
		belief.Agreement.IndependentRoots < minimum || !validAnswerFactTime(belief.AsOf) ||
		len(belief.Evidence) != belief.Agreement.Pages || len(belief.Receipts) != len(belief.Evidence) {
		return fmt.Errorf("known answer belief is invalid")
	}
	if belief.Confidence != answerFactConfidence(belief.Agreement.IndependentRoots) {
		return fmt.Errorf("known answer confidence is inconsistent")
	}
	seenURLs := make(map[string]struct{}, len(belief.Evidence))
	distinctRoots := make(map[string]struct{}, len(belief.Evidence))
	for _, item := range belief.Evidence {
		if !validAnswerFactEvidence(item, belief.AsOf) {
			return fmt.Errorf("known answer evidence is invalid")
		}
		if _, duplicate := seenURLs[item.URL]; duplicate {
			return fmt.Errorf("known answer repeats an evidence URL")
		}
		seenURLs[item.URL] = struct{}{}
		distinctRoots[item.Root] = struct{}{}
		receipt, present := belief.Receipts[item.URL]
		if !present || !validAnswerFactReceipt(receipt) {
			return fmt.Errorf("known answer receipt set is invalid")
		}
	}
	if belief.Agreement.IndependentRoots > len(distinctRoots) {
		return fmt.Errorf("known answer overstates independent roots")
	}
	for rawURL := range belief.Receipts {
		if _, present := seenURLs[rawURL]; !present {
			return fmt.Errorf("known answer contains an unmatched receipt")
		}
	}
	if !validAnswerFactTime(lease.ExpiresAt) ||
		!lease.ExpiresAt.Equal(belief.AsOf.Add(time.Duration(models.DefaultAnswerLeaseSeconds)*time.Second)) ||
		lease.RenewURL != models.DefaultAnswerRenewURL ||
		lease.ConfidenceHalflife != models.DefaultAnswerConfidenceHalflifeSeconds {
		return fmt.Errorf("known answer lease is invalid")
	}
	return nil
}

func validateUnknownAnswerFact(response models.AnswerResponse, minimum int) error {
	if response.Belief != nil || response.Lease != nil || !validAnswerFactUnknownReason(response.Reason) ||
		response.Needs == nil || response.Needs.MoreIndependentSources < 1 ||
		response.Needs.MoreIndependentSources > models.MaxAnswerMinIndependentSources {
		return fmt.Errorf("unknown answer has an invalid shape")
	}
	closestRoots := 0
	var closestValue string
	if response.Closest != nil {
		var ok bool
		closestValue, ok = canonicalAnswerFactValue(response.Closest.Value, false)
		if !ok || response.Closest.IndependentRoots < 1 || response.Closest.IndependentRoots > models.MaxExtractSources {
			return fmt.Errorf("unknown answer closest candidate is invalid")
		}
		closestRoots = response.Closest.IndependentRoots
	}
	expectedNeed := minimum - closestRoots
	if expectedNeed < 1 {
		expectedNeed = 1
	}
	if response.Needs.MoreIndependentSources != expectedNeed {
		return fmt.Errorf("unknown answer need is inconsistent")
	}

	switch response.Reason {
	case models.AnswerUnknownNoSearchResults, models.AnswerUnknownNoValidSources, models.AnswerUnknownMissingValue:
		if response.Closest != nil || len(response.Conflicts) != 0 || response.Needs.MoreIndependentSources != minimum {
			return fmt.Errorf("source-empty unknown answer contains stray candidates")
		}
	case models.AnswerUnknownInsufficient:
		if response.Closest == nil || response.Closest.Note != "insufficient independent roots" ||
			response.Closest.IndependentRoots >= minimum || len(response.Conflicts)+1 > models.MaxExtractSources ||
			!answerFactPageBudgetFits(response.Closest.IndependentRoots, response.Conflicts) {
			return fmt.Errorf("insufficient answer is invalid")
		}
		for _, conflict := range response.Conflicts {
			candidateValue, _ := canonicalAnswerFactValue(conflict.Value, true)
			if candidateValue == closestValue || conflict.Agreement.IndependentRoots >= response.Closest.IndependentRoots {
				return fmt.Errorf("insufficient answer contains an invalid alternative")
			}
		}
	case models.AnswerUnknownConflict:
		if response.Closest == nil || response.Closest.Note != "independent-root tie" ||
			len(response.Conflicts) < 1 || len(response.Conflicts)+1 > models.MaxExtractSources ||
			response.Conflicts[0].Agreement.IndependentRoots != response.Closest.IndependentRoots ||
			!answerFactPageBudgetFits(response.Closest.IndependentRoots, response.Conflicts) {
			return fmt.Errorf("conflicting answer is invalid")
		}
		for _, conflict := range response.Conflicts {
			candidateValue, _ := canonicalAnswerFactValue(conflict.Value, true)
			if candidateValue == closestValue {
				return fmt.Errorf("conflicting answer repeats its closest value")
			}
		}
	}
	return nil
}

func validAnswerFactEvidence(item models.AnswerEvidence, asOf time.Time) bool {
	if len(item.URL) == 0 || len(item.URL) > models.MaxExtractSourceURLBytes {
		return false
	}
	canonicalURL, parsed, err := publicnet.NormalizeHTTPURL(item.URL, nil, false)
	if err != nil || parsed == nil || parsed.User != nil || canonicalURL != item.URL {
		return false
	}
	root, err := authoritativeAnswerFactRoot(parsed.Hostname())
	if err != nil || item.Root != root || len(item.Root) > maxAnswerRootBytes ||
		item.Quote == "" || len(item.Quote) > maxAnswerQuoteBytes || !utf8.ValidString(item.Quote) ||
		strings.IndexFunc(item.Quote, unicode.IsControl) >= 0 || len(item.Selector) > maxAnswerSelectorBytes ||
		!utf8.ValidString(item.Selector) || strings.IndexFunc(item.Selector, unicode.IsControl) >= 0 ||
		!validSearchResponseSnapshotID(item.SnapshotID) || !validAnswerFactTime(item.FetchedAt) || item.FetchedAt.After(asOf) {
		return false
	}
	if item.TextRange[0] < 0 || item.TextRange[1] <= item.TextRange[0] || item.TextRange[1]-item.TextRange[0] != len(item.Quote) {
		return false
	}
	switch item.Method {
	case evidence.MethodExact, evidence.MethodNormalized, evidence.MethodFuzzy, evidence.MethodCompiled:
		return true
	default:
		return false
	}
}

func authoritativeAnswerFactRoot(hostname string) (string, error) {
	hostname = strings.ToLower(hostname)
	if address, err := netip.ParseAddr(hostname); err == nil {
		return address.Unmap().String(), nil
	}
	root, err := publicsuffix.EffectiveTLDPlusOne(hostname)
	if err != nil {
		return "", err
	}
	return strings.ToLower(root), nil
}

func canonicalAnswerFactValue(raw json.RawMessage, allowNull bool) (string, bool) {
	if len(raw) == 0 || len(raw) > maxAnswerValueBytes || !utf8.Valid(raw) {
		return "", false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var value any
	if err := decoder.Decode(&value); err != nil || requireJSONEOF(decoder) != nil {
		return "", false
	}
	switch value.(type) {
	case string:
		encoded, err := json.Marshal(value)
		if err != nil || len(encoded) > maxAnswerValueBytes {
			return "", false
		}
		return string(encoded), true
	case nil:
		return "null", allowNull
	default:
		return "", false
	}
}

func validAnswerFactAgreement(agreement models.MultiExtractAgreement) bool {
	return agreement.Pages >= 1 && agreement.Pages <= models.MaxExtractSources &&
		agreement.IndependentRoots >= 1 && agreement.IndependentRoots <= agreement.Pages
}

func orderedAnswerFactCandidates(candidates []models.AnswerCandidate) bool {
	previousRoots := models.MaxExtractSources + 1
	previousPages := models.MaxExtractSources + 1
	for _, candidate := range candidates {
		if candidate.Agreement.IndependentRoots > previousRoots ||
			candidate.Agreement.IndependentRoots == previousRoots && candidate.Agreement.Pages > previousPages {
			return false
		}
		previousRoots = candidate.Agreement.IndependentRoots
		previousPages = candidate.Agreement.Pages
	}
	return true
}

func answerFactPageBudgetFits(initial int, candidates []models.AnswerCandidate) bool {
	if initial < 0 || initial > models.MaxExtractSources {
		return false
	}
	used := initial
	for _, candidate := range candidates {
		if candidate.Agreement.Pages < 0 || candidate.Agreement.Pages > models.MaxExtractSources-used {
			return false
		}
		used += candidate.Agreement.Pages
	}
	return true
}

func answerFactConfidence(roots int) models.AnswerConfidence {
	switch {
	case roots >= 3:
		return models.AnswerConfidenceHigh
	case roots == 2:
		return models.AnswerConfidenceMedium
	default:
		return models.AnswerConfidenceLow
	}
}

func validAnswerFactReceipt(receipt string) bool {
	if receipt == "" || len(receipt) > maxAnswerReceiptBytes || !utf8.ValidString(receipt) {
		return false
	}
	for index := range len(receipt) {
		character := receipt[index]
		if character == '.' || character == '-' || character == '_' ||
			character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return true
}

func validAnswerFactTime(value time.Time) bool {
	if value.IsZero() || value.Location() != time.UTC {
		return false
	}
	_, err := value.MarshalJSON()
	return err == nil
}

func validAnswerFactUnknownReason(reason models.AnswerUnknownReason) bool {
	switch reason {
	case models.AnswerUnknownNoSearchResults, models.AnswerUnknownNoValidSources, models.AnswerUnknownMissingValue,
		models.AnswerUnknownInsufficient, models.AnswerUnknownConflict:
		return true
	default:
		return false
	}
}

func answerObjectFields(raw []byte, name string, allowed ...string) (map[string]json.RawMessage, error) {
	fields, err := decodeExtractJSONObject(raw, name)
	if err != nil {
		return nil, err
	}
	if err := rejectUnsupportedJSONFields(fields, name, allowed...); err != nil {
		return nil, err
	}
	return fields, nil
}

func requirePresentAnswerFields(fields map[string]json.RawMessage, name string, required ...string) error {
	for _, field := range required {
		raw, present := fields[field]
		if !present || answerJSONNull(raw) {
			return fmt.Errorf("%s is missing required field %q", name, field)
		}
	}
	return nil
}

func rejectPresentAnswerFields(fields map[string]json.RawMessage, name string, rejected ...string) error {
	for _, field := range rejected {
		if _, present := fields[field]; present {
			return fmt.Errorf("%s must not contain field %q", name, field)
		}
	}
	return nil
}

func answerJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func answerAPIError(statusCode int, body []byte) string {
	detail, err := decodeAnswerError(body)
	if err != nil {
		return fmt.Sprintf("answer failed (HTTP %d)", statusCode)
	}
	return fmt.Sprintf("[%s] %s", strings.TrimSpace(detail.Code), strings.TrimSpace(detail.Message))
}

func decodeAnswerError(body []byte) (*models.ErrorDetail, error) {
	if !utf8.Valid(body) {
		return nil, fmt.Errorf("error response must be valid UTF-8")
	}
	if err := rejectDuplicateJSONFields(body); err != nil {
		return nil, err
	}
	var envelope *models.AnswerErrorResponse
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil || envelope == nil {
		return nil, fmt.Errorf("error response must be a JSON object")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	fields, err := answerObjectFields(body, "answer error response", "error")
	if err != nil {
		return nil, err
	}
	if err := requirePresentAnswerFields(fields, "answer error response", "error"); err != nil {
		return nil, err
	}
	detailFields, err := answerObjectFields(fields["error"], "answer error", "code", "message")
	if err != nil {
		return nil, err
	}
	if err := requirePresentAnswerFields(detailFields, "answer error", "code", "message"); err != nil {
		return nil, err
	}
	if envelope.Error == nil || strings.TrimSpace(envelope.Error.Code) == "" || strings.TrimSpace(envelope.Error.Message) == "" {
		return nil, fmt.Errorf("answer error is incomplete")
	}
	return envelope.Error, nil
}

func redactAnswerSecrets(message string, secrets ...string) string {
	return redactSearchSecrets(message, secrets...)
}

func newExtractHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 120 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func handleExtractDataWithClient(client *http.Client, apiURL, apiKey string) server.ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		arguments := request.GetArguments()
		if err := validateExtractArguments(arguments); err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		urlValue, hasURL := arguments["url"]
		sourcesValue, hasSources := arguments["sources"]
		if hasURL == hasSources {
			return mcp.NewToolResultError("exactly one of url and sources is required"), nil
		}

		var targetURL string
		var sourceURLs []string
		var err error
		if hasURL {
			targetURL, err = extractTargetURL(urlValue, "url", maxVerifyURLBytes)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
		} else {
			sourceURLs, err = extractSourceURLs(sourcesValue)
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
		}

		schemaString, err := requiredExtractString(arguments, "schema")
		if err != nil {
			return mcp.NewToolResultError("schema is required and must be a JSON string"), nil
		}
		schemaJSON, err := decodeExtractSchema(schemaString)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("schema must be one valid JSON schema: %v", err)), nil
		}

		engine, err := optionalExtractString(arguments, "engine")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if engine == "" {
			engine = "auto"
		}
		switch engine {
		case "auto", "compiled", "llm":
		default:
			return mcp.NewToolResultError("engine must be auto, compiled, or llm"), nil
		}

		llmAPIKey, err := optionalExtractString(arguments, "llm_api_key")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		llmModel, err := optionalExtractString(arguments, "llm_model")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		llmBaseURL, err := optionalExtractString(arguments, "llm_base_url")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if len(llmAPIKey) > maxExtractLLMCredentialBytes {
			return mcp.NewToolResultError("llm_api_key exceeds the 16384-byte limit"), nil
		}
		if len(llmModel) > maxExtractLLMModelBytes {
			return mcp.NewToolResultError("llm_model exceeds the 256-byte limit"), nil
		}
		if len(llmBaseURL) > maxExtractLLMBaseURLBytes {
			return mcp.NewToolResultError("llm_base_url exceeds the 16384-byte limit"), nil
		}
		if engine == "llm" && strings.TrimSpace(llmAPIKey) == "" {
			return mcp.NewToolResultError("llm_api_key is required when engine is llm"), nil
		}
		if engine != "compiled" && llmBaseURL != "" {
			llmBaseURL, err = normalizeExtractLLMBaseURL(llmBaseURL)
			if err != nil {
				return mcp.NewToolResultError("llm_base_url must be an absolute http or https URL without credentials, query, or fragment"), nil
			}
		}

		payload := extractAPIPayload{
			Schema: append(json.RawMessage(nil), schemaJSON...),
			Engine: engine,
		}
		if hasSources {
			payload.Sources = append([]string(nil), sourceURLs...)
		} else {
			payload.URL = targetURL
		}
		if engine != "compiled" {
			payload.LLMAPIKey = llmAPIKey
			payload.LLMModel = llmModel
			payload.LLMBaseURL = llmBaseURL
		}

		apiResponse, err := apiPostResponse(ctx, client, apiURL, apiKey, "/api/v1/extract", payload)
		if err != nil {
			message := redactExtractSecrets(fmt.Sprintf("extract request failed: %v", err), apiKey, llmAPIKey)
			return mcp.NewToolResultError(message), nil
		}

		if apiResponse.StatusCode < http.StatusOK || apiResponse.StatusCode >= http.StatusMultipleChoices {
			message := extractAPIError(apiResponse.StatusCode, apiResponse.Body)
			return mcp.NewToolResultError(redactExtractSecrets(message, apiKey, llmAPIKey)), nil
		}

		if hasSources {
			multiResponse, err := decodeMultiExtractResponse(apiResponse.Body)
			if err != nil {
				message := redactExtractSecrets(fmt.Sprintf("failed to parse multi-source extract response: %v", err), apiKey, llmAPIKey)
				return mcp.NewToolResultError(message), nil
			}
			if !multiResponse.Success {
				message := formatExtractError(multiResponse.Error, apiResponse.StatusCode)
				return mcp.NewToolResultError(redactExtractSecrets(message, apiKey, llmAPIKey)), nil
			}
			pretty, err := json.MarshalIndent(multiResponse, "", "  ")
			if err != nil {
				message := redactExtractSecrets(fmt.Sprintf("failed to format multi-source extract response: %v", err), apiKey, llmAPIKey)
				return mcp.NewToolResultError(message), nil
			}
			return mcp.NewToolResultStructured(multiResponse, string(pretty)), nil
		}

		extractResponse, err := decodeExtractResponse(apiResponse.Body)
		if err != nil {
			message := redactExtractSecrets(fmt.Sprintf("failed to parse extract response: %v", err), apiKey, llmAPIKey)
			return mcp.NewToolResultError(message), nil
		}
		if !extractResponse.Success {
			message := extractResponseError(extractResponse, apiResponse.StatusCode)
			return mcp.NewToolResultError(redactExtractSecrets(message, apiKey, llmAPIKey)), nil
		}

		pretty, err := json.MarshalIndent(extractResponse, "", "  ")
		if err != nil {
			message := redactExtractSecrets(fmt.Sprintf("failed to format extract response: %v", err), apiKey, llmAPIKey)
			return mcp.NewToolResultError(message), nil
		}

		return mcp.NewToolResultStructured(extractResponse, string(pretty)), nil
	}
}

var extractArgumentNames = map[string]struct{}{
	"url": {}, "sources": {}, "schema": {}, "engine": {}, "llm_api_key": {}, "llm_model": {}, "llm_base_url": {},
}

func validateExtractArguments(arguments map[string]any) error {
	unknown := make([]string, 0)
	for name := range arguments {
		if _, ok := extractArgumentNames[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("unsupported extract_data argument %q", unknown[0])
}

func extractTargetURL(value any, name string, maximumBytes int) (string, error) {
	raw, ok := value.(string)
	if !ok || strings.TrimSpace(raw) == "" {
		if name == "url" {
			return "", fmt.Errorf("url is required and must be a non-empty string")
		}
		return "", fmt.Errorf("%s must be a non-empty string", name)
	}
	if len(raw) > maximumBytes {
		return "", fmt.Errorf("%s exceeds the %d-byte limit", name, maximumBytes)
	}
	if !utf8.ValidString(raw) {
		return "", fmt.Errorf("%s must be valid UTF-8", name)
	}
	trimmed := strings.TrimSpace(raw)
	parsed, err := urlpkg.ParseRequestURI(trimmed)
	if err != nil || parsed.Host == "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("%s must be an absolute http or https URL", name)
	}
	return trimmed, nil
}

func extractSourceURLs(value any) ([]string, error) {
	var rawSources []any
	switch sources := value.(type) {
	case []any:
		rawSources = sources
	case []string:
		rawSources = make([]any, len(sources))
		for index := range sources {
			rawSources[index] = sources[index]
		}
	default:
		return nil, fmt.Errorf("sources must be an array of URL strings")
	}
	if len(rawSources) < 1 || len(rawSources) > models.MaxExtractSources {
		return nil, fmt.Errorf("sources must contain one to eight URLs")
	}

	used := 0
	result := make([]string, len(rawSources))
	for index, value := range rawSources {
		raw, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("sources[%d] must be a URL string", index)
		}
		if len(raw) == 0 || len(raw) > models.MaxExtractSourceURLBytes ||
			len(raw) > models.MaxExtractSourcesURLBytes-used {
			return nil, fmt.Errorf("source URLs exceed the request budget")
		}
		used += len(raw)
		normalized, err := extractTargetURL(raw, fmt.Sprintf("sources[%d]", index), models.MaxExtractSourceURLBytes)
		if err != nil {
			return nil, err
		}
		result[index] = normalized
	}
	return result, nil
}

func requiredExtractString(arguments map[string]any, name string) (string, error) {
	value, ok := arguments[name]
	if !ok {
		return "", fmt.Errorf("%s is required", name)
	}
	result, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", name)
	}
	return result, nil
}

func optionalExtractString(arguments map[string]any, name string) (string, error) {
	value, ok := arguments[name]
	if !ok {
		return "", nil
	}
	result, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", name)
	}
	return result, nil
}

func normalizeExtractLLMBaseURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	parsed, err := urlpkg.Parse(trimmed)
	validScheme := err == nil && (strings.EqualFold(parsed.Scheme, "http") || strings.EqualFold(parsed.Scheme, "https"))
	if err != nil || !parsed.IsAbs() || parsed.Opaque != "" || parsed.Host == "" || parsed.Hostname() == "" ||
		!validScheme || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		strings.HasSuffix(parsed.Host, ":") {
		return "", fmt.Errorf("invalid LLM base URL")
	}
	if port := parsed.Port(); port != "" {
		numericPort, err := strconv.Atoi(port)
		if err != nil || numericPort < 1 || numericPort > 65535 {
			return "", fmt.Errorf("invalid LLM base URL")
		}
	}
	return trimmed, nil
}

func decodeExtractSchema(raw string) (json.RawMessage, error) {
	if len(raw) > maxExtractSchemaJSONBytes {
		return nil, fmt.Errorf("schema exceeds %d-byte limit", maxExtractSchemaJSONBytes)
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()

	var schema json.RawMessage
	if err := decoder.Decode(&schema); err != nil {
		return nil, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	normalized, err := llm.NormalizeSchema(schema)
	if err != nil {
		return nil, err
	}
	if err := llm.ValidateSchema(normalized); err != nil {
		return nil, err
	}
	return append(json.RawMessage(nil), schema...), nil
}

func decodeExtractResponse(body []byte) (models.ExtractResponse, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()

	var response *models.ExtractResponse
	if err := decoder.Decode(&response); err != nil {
		return models.ExtractResponse{}, err
	}
	if response == nil {
		return models.ExtractResponse{}, fmt.Errorf("response must be a JSON object")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return models.ExtractResponse{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		return models.ExtractResponse{}, fmt.Errorf("response must be a JSON object")
	}
	if err := requirePresentJSONFields(fields, "response", "success", "metadata", "tokens", "timing"); err != nil {
		return models.ExtractResponse{}, err
	}
	if err := requireNestedJSONFields(fields["metadata"], "metadata", "title", "source_url"); err != nil {
		return models.ExtractResponse{}, err
	}
	if err := requireNestedJSONFields(fields["tokens"], "tokens", "original_estimate", "cleaned_estimate", "savings_percent"); err != nil {
		return models.ExtractResponse{}, err
	}
	if err := requireNestedJSONFields(fields["timing"], "timing", "total_ms", "navigation_ms", "cleaning_ms", "extraction_ms"); err != nil {
		return models.ExtractResponse{}, err
	}
	if response.Success {
		if err := requirePresentJSONFields(fields, "response", "data"); err != nil {
			return models.ExtractResponse{}, err
		}
		if len(response.Data) == 0 || !json.Valid(response.Data) {
			return models.ExtractResponse{}, fmt.Errorf("successful response is missing valid data")
		}
		if response.Error != nil {
			return models.ExtractResponse{}, fmt.Errorf("successful response must not contain an error")
		}
	} else if response.Error == nil {
		return models.ExtractResponse{}, fmt.Errorf("unsuccessful response is missing an error")
	}
	if response.Error != nil {
		if err := validateExtractErrorDetail(response.Error, fields["error"], "error"); err != nil {
			return models.ExtractResponse{}, err
		}
	}
	if response.Extractor != nil {
		if err := requireNestedJSONFields(fields["extractor"], "extractor", "id", "version", "compiled_at", "validation", "mode"); err != nil {
			return models.ExtractResponse{}, err
		}
		metadata := response.Extractor
		if strings.TrimSpace(metadata.ID) == "" || metadata.Version <= 0 || metadata.CompiledAt.IsZero() ||
			metadata.Validation < 0 || metadata.Validation > 1 || metadata.Mode != "compiled" {
			return models.ExtractResponse{}, fmt.Errorf("response contains invalid extractor metadata")
		}
	}
	return *response, nil
}

func decodeMultiExtractResponse(body []byte) (models.MultiExtractResponse, error) {
	if !utf8.Valid(body) {
		return models.MultiExtractResponse{}, fmt.Errorf("response must be valid UTF-8")
	}
	if err := rejectDuplicateJSONFields(body); err != nil {
		return models.MultiExtractResponse{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()

	var response *models.MultiExtractResponse
	if err := decoder.Decode(&response); err != nil {
		return models.MultiExtractResponse{}, err
	}
	if response == nil {
		return models.MultiExtractResponse{}, fmt.Errorf("response must be a JSON object")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return models.MultiExtractResponse{}, err
	}
	fields, err := decodeExtractJSONObject(body, "response")
	if err != nil {
		return models.MultiExtractResponse{}, err
	}
	if err := requirePresentJSONFields(fields, "response", "success", "sources", "tokens", "timing", "usage_complete"); err != nil {
		return models.MultiExtractResponse{}, err
	}
	if err := validateMultiExtractMetrics(*response, fields); err != nil {
		return models.MultiExtractResponse{}, err
	}
	if err := validateMultiExtractSources(response.Sources, fields["sources"]); err != nil {
		return models.MultiExtractResponse{}, err
	}

	if !response.Success {
		if err := requirePresentJSONFields(fields, "response", "error"); err != nil {
			return models.MultiExtractResponse{}, err
		}
		if err := rejectPresentJSONFields(fields, "unsuccessful response", "status", "data", "consensus", "violations"); err != nil {
			return models.MultiExtractResponse{}, err
		}
		if err := validateExtractErrorDetail(response.Error, fields["error"], "response error"); err != nil {
			return models.MultiExtractResponse{}, err
		}
		return *response, nil
	}

	if len(response.Sources) < 1 || len(response.Sources) > models.MaxExtractSources {
		return models.MultiExtractResponse{}, fmt.Errorf("successful response must contain one to eight sources")
	}
	if err := requirePresentJSONFields(fields, "response", "status", "consensus"); err != nil {
		return models.MultiExtractResponse{}, err
	}
	if err := rejectPresentJSONFields(fields, "successful response", "error"); err != nil {
		return models.MultiExtractResponse{}, err
	}
	if response.Error != nil {
		return models.MultiExtractResponse{}, fmt.Errorf("successful response must not contain an error")
	}
	if !hasValidMultiExtractSource(response.Sources) {
		return models.MultiExtractResponse{}, fmt.Errorf("successful response contains no valid source")
	}

	hasAmbiguousField, err := validateMultiExtractConsensus(response.Consensus, fields["consensus"], response.Sources)
	if err != nil {
		return models.MultiExtractResponse{}, err
	}
	switch response.Status {
	case models.MultiExtractStatusComplete:
		if _, present := fields["data"]; !present || len(response.Data) == 0 || !json.Valid(response.Data) {
			return models.MultiExtractResponse{}, fmt.Errorf("complete response is missing valid data")
		}
		if err := rejectPresentJSONFields(fields, "complete response", "violations"); err != nil {
			return models.MultiExtractResponse{}, err
		}
		if hasAmbiguousField {
			return models.MultiExtractResponse{}, fmt.Errorf("complete response contains an ambiguous field")
		}
	case models.MultiExtractStatusAmbiguous:
		if err := rejectPresentJSONFields(fields, "ambiguous response", "data", "violations"); err != nil {
			return models.MultiExtractResponse{}, err
		}
	case models.MultiExtractStatusSchemaInvalid:
		if err := rejectPresentJSONFields(fields, "schema-invalid response", "data"); err != nil {
			return models.MultiExtractResponse{}, err
		}
		if err := requirePresentJSONFields(fields, "schema-invalid response", "violations"); err != nil {
			return models.MultiExtractResponse{}, err
		}
		if len(response.Violations) == 0 {
			return models.MultiExtractResponse{}, fmt.Errorf("schema-invalid response contains no violations")
		}
		for index, violation := range response.Violations {
			if strings.TrimSpace(violation.Path) == "" || strings.TrimSpace(violation.Message) == "" {
				return models.MultiExtractResponse{}, fmt.Errorf("violation %d is incomplete", index)
			}
		}
		if hasAmbiguousField {
			return models.MultiExtractResponse{}, fmt.Errorf("schema-invalid response contains an ambiguous field")
		}
	default:
		return models.MultiExtractResponse{}, fmt.Errorf("response contains an invalid multi-source status")
	}
	return *response, nil
}

func validateMultiExtractMetrics(response models.MultiExtractResponse, fields map[string]json.RawMessage) error {
	if err := validateExtractTokenInfo(response.Tokens, fields["tokens"], "response tokens"); err != nil {
		return err
	}
	if err := validateExtractTiming(response.Timing, fields["timing"], "response timing"); err != nil {
		return err
	}
	if response.LLMUsage != nil {
		raw, ok := fields["llm_usage"]
		if !ok {
			return fmt.Errorf("response is missing llm_usage")
		}
		if err := validateExtractLLMUsage(response.LLMUsage, raw, "response llm_usage"); err != nil {
			return err
		}
	} else if _, present := fields["llm_usage"]; present {
		return fmt.Errorf("response llm_usage must not be null")
	}
	return nil
}

func validateMultiExtractSources(sources []models.MultiExtractSource, raw json.RawMessage) error {
	var rawSources []json.RawMessage
	if err := json.Unmarshal(raw, &rawSources); err != nil || rawSources == nil {
		return fmt.Errorf("sources must be a JSON array")
	}
	if len(rawSources) != len(sources) || len(sources) > models.MaxExtractSources {
		return fmt.Errorf("response contains an invalid source count")
	}
	seenURLs := make(map[string]struct{}, len(sources))
	validSources := make(map[string]models.MultiExtractSource, len(sources))
	validFinalURLs := make(map[string]struct{}, len(sources))
	duplicates := make([]models.MultiExtractSource, 0, len(sources))
	for index, source := range sources {
		name := fmt.Sprintf("source %d", index)
		fields, err := decodeExtractJSONObject(rawSources[index], name)
		if err != nil {
			return err
		}
		if err := requirePresentJSONFields(fields, name, "url", "success", "status", "tokens", "timing"); err != nil {
			return err
		}
		canonicalURL, _, err := publicnet.NormalizeHTTPURL(source.URL, nil, false)
		if err != nil || canonicalURL != source.URL || len(source.URL) > models.MaxExtractSourceURLBytes {
			return fmt.Errorf("%s url is not canonical", name)
		}
		if _, duplicate := seenURLs[canonicalURL]; duplicate {
			return fmt.Errorf("response contains duplicate source URL %q", canonicalURL)
		}
		seenURLs[canonicalURL] = struct{}{}
		if err := validateExtractTokenInfo(source.Tokens, fields["tokens"], name+" tokens"); err != nil {
			return err
		}
		if err := validateExtractTiming(source.Timing, fields["timing"], name+" timing"); err != nil {
			return err
		}
		if source.LLMUsage != nil {
			rawUsage, ok := fields["llm_usage"]
			if !ok {
				return fmt.Errorf("%s is missing llm_usage", name)
			}
			if err := validateExtractLLMUsage(source.LLMUsage, rawUsage, name+" llm_usage"); err != nil {
				return err
			}
		} else if _, present := fields["llm_usage"]; present {
			return fmt.Errorf("%s llm_usage must not be null", name)
		}

		switch source.Status {
		case models.MultiExtractSourceStatusValid:
			if !source.Success || source.Error != nil || source.DuplicateOf != "" || source.FinalURL == "" || source.SnapshotID == "" {
				return fmt.Errorf("%s has an invalid valid-source shape", name)
			}
			if err := rejectPresentJSONFields(fields, name, "error", "duplicate_of"); err != nil {
				return err
			}
			canonicalFinalURL, _, err := publicnet.NormalizeHTTPURL(source.FinalURL, nil, false)
			if err != nil || canonicalFinalURL != source.FinalURL || len(source.FinalURL) > models.MaxExtractSourceURLBytes {
				return fmt.Errorf("%s final_url is not canonical", name)
			}
			if _, duplicate := validFinalURLs[canonicalFinalURL]; duplicate {
				return fmt.Errorf("response contains duplicate valid final URL %q", canonicalFinalURL)
			}
			validFinalURLs[canonicalFinalURL] = struct{}{}
			validSources[source.URL] = source
		case models.MultiExtractSourceStatusDuplicate:
			if source.Success || source.Error != nil || source.DuplicateOf == "" || source.FinalURL == "" || source.SnapshotID == "" {
				return fmt.Errorf("%s has an invalid duplicate-source shape", name)
			}
			if err := rejectPresentJSONFields(fields, name, "error"); err != nil {
				return err
			}
			canonicalFinalURL, _, err := publicnet.NormalizeHTTPURL(source.FinalURL, nil, false)
			if err != nil || canonicalFinalURL != source.FinalURL || len(source.FinalURL) > models.MaxExtractSourceURLBytes {
				return fmt.Errorf("%s final_url is not canonical", name)
			}
			canonicalDuplicateOf, _, err := publicnet.NormalizeHTTPURL(source.DuplicateOf, nil, false)
			if err != nil || canonicalDuplicateOf != source.DuplicateOf || len(source.DuplicateOf) > models.MaxExtractSourceURLBytes {
				return fmt.Errorf("%s duplicate_of is not canonical", name)
			}
			duplicates = append(duplicates, source)
		case models.MultiExtractSourceStatusTimeout,
			models.MultiExtractSourceStatusFetchFailed,
			models.MultiExtractSourceStatusExtractionFailed,
			models.MultiExtractSourceStatusPartial,
			models.MultiExtractSourceStatusSchemaInvalid,
			models.MultiExtractSourceStatusEvidenceUnavailable:
			if source.Success || source.Error == nil || source.FinalURL != "" || source.SnapshotID != "" || source.DuplicateOf != "" {
				return fmt.Errorf("%s has an invalid failed-source shape", name)
			}
			if err := rejectPresentJSONFields(fields, name, "final_url", "snapshot_id", "duplicate_of"); err != nil {
				return err
			}
			rawError, ok := fields["error"]
			if !ok {
				return fmt.Errorf("%s is missing error", name)
			}
			if err := validateExtractErrorDetail(source.Error, rawError, name+" error"); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s contains an invalid status", name)
		}
	}
	for _, duplicate := range duplicates {
		winner, ok := validSources[duplicate.DuplicateOf]
		if !ok || winner.FinalURL != duplicate.FinalURL {
			return fmt.Errorf("duplicate source %q does not reference its valid final-URL winner", duplicate.URL)
		}
	}
	return nil
}

func hasValidMultiExtractSource(sources []models.MultiExtractSource) bool {
	for _, source := range sources {
		if source.Status == models.MultiExtractSourceStatusValid && source.Success {
			return true
		}
	}
	return false
}

func validateMultiExtractConsensus(
	consensus *models.MultiExtractConsensus,
	raw json.RawMessage,
	sources []models.MultiExtractSource,
) (bool, error) {
	if consensus == nil {
		return false, fmt.Errorf("successful response is missing consensus")
	}
	fields, err := decodeExtractJSONObject(raw, "consensus")
	if err != nil {
		return false, err
	}
	if err := requirePresentJSONFields(fields, "consensus", "fields"); err != nil {
		return false, err
	}
	var rawFields map[string]json.RawMessage
	if err := json.Unmarshal(fields["fields"], &rawFields); err != nil || rawFields == nil {
		return false, fmt.Errorf("consensus fields must be a JSON object")
	}
	if len(rawFields) != len(consensus.Fields) {
		return false, fmt.Errorf("consensus fields are inconsistent")
	}
	validSnapshots := make(map[string]string, len(sources))
	for _, source := range sources {
		if source.Status == models.MultiExtractSourceStatusValid && source.Success {
			validSnapshots[source.FinalURL] = source.SnapshotID
		}
	}

	hasAmbiguous := false
	for path, field := range consensus.Fields {
		if path == "" || !utf8.ValidString(path) || len(path) > maxExtractConsensusPathBytes {
			return false, fmt.Errorf("consensus contains an invalid field path")
		}
		rawField, ok := rawFields[path]
		if !ok {
			return false, fmt.Errorf("consensus is missing field %q", path)
		}
		fieldFields, err := decodeExtractJSONObject(rawField, "consensus field "+path)
		if err != nil {
			return false, err
		}
		if err := requirePresentJSONFields(fieldFields, "consensus field "+path, "agreement"); err != nil {
			return false, err
		}
		if field.Ambiguous {
			hasAmbiguous = true
			if _, present := fieldFields["ambiguous"]; !present || field.Agreement.Pages != 0 || field.Agreement.IndependentRoots != 0 ||
				len(field.Value) != 0 || len(field.Supports) != 0 || len(field.Conflicts) < 2 {
				return false, fmt.Errorf("consensus field %q has an invalid ambiguous shape", path)
			}
			if err := validateMultiExtractAgreementFields(field.Agreement, fieldFields["agreement"], "consensus field "+path+" agreement"); err != nil {
				return false, err
			}
			if err := rejectPresentJSONFields(fieldFields, "ambiguous consensus field "+path, "value", "supports"); err != nil {
				return false, err
			}
		} else {
			if _, present := fieldFields["ambiguous"]; present {
				return false, fmt.Errorf("consensus field %q has a redundant ambiguous flag", path)
			}
			if _, present := fieldFields["value"]; !present || len(field.Value) == 0 {
				return false, fmt.Errorf("consensus field %q is missing a valid value", path)
			}
			if err := validateMultiExtractScalar(field.Value, "consensus field "+path+" value"); err != nil {
				return false, err
			}
			rawSupports, ok := fieldFields["supports"]
			if !ok || len(field.Supports) == 0 {
				return false, fmt.Errorf("consensus field %q is missing supports", path)
			}
			uniqueRoots, err := validateMultiExtractSupports(field.Supports, rawSupports, validSnapshots, "consensus field "+path+" supports")
			if err != nil {
				return false, err
			}
			if err := validateMultiExtractAgreement(field.Agreement, len(field.Supports), uniqueRoots, fieldFields["agreement"], "consensus field "+path); err != nil {
				return false, err
			}
		}
		if len(field.Conflicts) > 0 {
			rawConflicts, ok := fieldFields["conflicts"]
			if !ok {
				return false, fmt.Errorf("consensus field %q is missing conflicts", path)
			}
			if err := validateMultiExtractConflicts(field.Conflicts, rawConflicts, validSnapshots, "consensus field "+path+" conflicts"); err != nil {
				return false, err
			}
		} else if _, present := fieldFields["conflicts"]; present {
			return false, fmt.Errorf("consensus field %q contains empty conflicts", path)
		}
		if err := validateMultiExtractConflictScores(field, "consensus field "+path); err != nil {
			return false, err
		}
		if err := validateMultiExtractFieldSupportPartition(field, "consensus field "+path); err != nil {
			return false, err
		}
	}
	return hasAmbiguous, nil
}

func validateMultiExtractConflicts(
	conflicts []models.MultiExtractConflict,
	raw json.RawMessage,
	validSnapshots map[string]string,
	name string,
) error {
	var rawConflicts []json.RawMessage
	if err := json.Unmarshal(raw, &rawConflicts); err != nil || len(rawConflicts) != len(conflicts) {
		return fmt.Errorf("%s must be a matching JSON array", name)
	}
	for index, conflict := range conflicts {
		conflictName := fmt.Sprintf("%s %d", name, index)
		fields, err := decodeExtractJSONObject(rawConflicts[index], conflictName)
		if err != nil {
			return err
		}
		if err := requirePresentJSONFields(fields, conflictName, "agreement", "supports"); err != nil {
			return err
		}
		if _, present := fields["value"]; !present || len(conflict.Value) == 0 {
			return fmt.Errorf("%s is missing a valid value", conflictName)
		}
		if err := validateMultiExtractScalar(conflict.Value, conflictName+" value"); err != nil {
			return err
		}
		if len(conflict.Supports) == 0 {
			return fmt.Errorf("%s contains no supports", conflictName)
		}
		uniqueRoots, err := validateMultiExtractSupports(conflict.Supports, fields["supports"], validSnapshots, conflictName+" supports")
		if err != nil {
			return err
		}
		if err := validateMultiExtractAgreement(conflict.Agreement, len(conflict.Supports), uniqueRoots, fields["agreement"], conflictName); err != nil {
			return err
		}
	}
	return nil
}

func validateMultiExtractAgreement(
	agreement models.MultiExtractAgreement,
	supports int,
	uniqueRoots int,
	raw json.RawMessage,
	name string,
) error {
	if err := validateMultiExtractAgreementFields(agreement, raw, name+" agreement"); err != nil {
		return err
	}
	if agreement.Pages < 1 || agreement.IndependentRoots < 1 || agreement.IndependentRoots > agreement.Pages ||
		agreement.IndependentRoots > uniqueRoots ||
		agreement.Pages != supports {
		return fmt.Errorf("%s contains invalid agreement", name)
	}
	return nil
}

func validateMultiExtractAgreementFields(agreement models.MultiExtractAgreement, raw json.RawMessage, name string) error {
	fields, err := decodeExtractJSONObject(raw, name)
	if err != nil {
		return err
	}
	if err := rejectUnsupportedJSONFields(fields, name, "pages", "independent_roots", "fold_reason"); err != nil {
		return err
	}
	if err := requirePresentJSONFields(fields, name, "pages", "independent_roots"); err != nil {
		return err
	}
	return validateMultiExtractAgreementFoldReason(agreement, fields, name)
}

func validateMultiExtractSupports(
	supports []models.MultiExtractSupport,
	raw json.RawMessage,
	validSnapshots map[string]string,
	name string,
) (int, error) {
	var rawSupports []json.RawMessage
	if err := json.Unmarshal(raw, &rawSupports); err != nil || len(rawSupports) != len(supports) {
		return 0, fmt.Errorf("%s must be a matching JSON array", name)
	}
	seenURLs := make(map[string]struct{}, len(supports))
	uniqueRoots := make(map[string]struct{}, len(supports))
	for index, support := range supports {
		supportName := fmt.Sprintf("%s %d", name, index)
		fields, err := decodeExtractJSONObject(rawSupports[index], supportName)
		if err != nil {
			return 0, err
		}
		if err := rejectUnsupportedJSONFields(fields, supportName, "url", "root", "evidence", "receipt", "fold_reason"); err != nil {
			return 0, err
		}
		if err := requirePresentJSONFields(fields, supportName, "url", "root", "evidence", "receipt"); err != nil {
			return 0, err
		}
		if _, err := validateMultiExtractFoldReasonField(support.FoldReason, fields, supportName); err != nil {
			return 0, err
		}
		canonicalURL, root, err := canonicalMultiExtractSupport(support.URL)
		if err != nil || canonicalURL != support.URL || root != support.Root || len(support.URL) > models.MaxExtractSourceURLBytes {
			return 0, fmt.Errorf("%s contains an invalid URL root", supportName)
		}
		if _, duplicate := seenURLs[canonicalURL]; duplicate {
			return 0, fmt.Errorf("%s repeats support URL %q", name, canonicalURL)
		}
		seenURLs[canonicalURL] = struct{}{}
		uniqueRoots[root] = struct{}{}
		if strings.TrimSpace(support.Root) == "" || strings.TrimSpace(support.Receipt) == "" || support.Evidence == nil {
			return 0, fmt.Errorf("%s is incomplete", supportName)
		}
		snapshotID, admitted := validSnapshots[support.URL]
		if !admitted || support.Evidence.SnapshotID != snapshotID {
			return 0, fmt.Errorf("%s does not reference an admitted source snapshot", supportName)
		}
		if err := validateMultiExtractEvidence(*support.Evidence, fields["evidence"], supportName+" evidence"); err != nil {
			return 0, err
		}
	}
	return len(uniqueRoots), nil
}

func validateMultiExtractAgreementFoldReason(
	agreement models.MultiExtractAgreement,
	fields map[string]json.RawMessage,
	name string,
) error {
	present, err := validateMultiExtractFoldReasonField(agreement.FoldReason, fields, name)
	if err != nil {
		return err
	}
	if present && agreement.Pages <= agreement.IndependentRoots {
		return fmt.Errorf("%s fold_reason requires fewer independent roots than pages", name)
	}
	return nil
}

func validateMultiExtractFoldReasonField(
	reason models.MultiExtractFoldReason,
	fields map[string]json.RawMessage,
	name string,
) (bool, error) {
	raw, present := fields["fold_reason"]
	if !present {
		if reason != "" {
			return false, fmt.Errorf("%s is missing fold_reason", name)
		}
		return false, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || !validMultiExtractFoldReason(reason) {
		return false, fmt.Errorf("%s contains an invalid fold_reason", name)
	}
	return true, nil
}

func validMultiExtractFoldReason(reason models.MultiExtractFoldReason) bool {
	switch reason {
	case models.MultiExtractFoldReasonSameRoot,
		models.MultiExtractFoldReasonNearDuplicate,
		models.MultiExtractFoldReasonQuoteLineage:
		return true
	default:
		return false
	}
}

func canonicalMultiExtractSupport(rawURL string) (string, string, error) {
	canonicalURL, parsedURL, err := publicnet.NormalizeHTTPURL(rawURL, nil, false)
	if err != nil {
		return "", "", err
	}
	hostname := strings.ToLower(parsedURL.Hostname())
	if address, parseErr := netip.ParseAddr(hostname); parseErr == nil {
		return canonicalURL, address.Unmap().String(), nil
	}
	root, err := publicsuffix.EffectiveTLDPlusOne(hostname)
	if err != nil {
		return "", "", err
	}
	return canonicalURL, strings.ToLower(root), nil
}

func validateMultiExtractConflictScores(field models.MultiExtractFieldConsensus, name string) error {
	if len(field.Conflicts) == 0 {
		return nil
	}
	if field.Ambiguous {
		if len(field.Conflicts) < 2 || compareMultiExtractAgreement(
			field.Conflicts[0].Agreement,
			field.Conflicts[1].Agreement,
		) != 0 {
			return fmt.Errorf("%s does not expose a tied leading conflict", name)
		}
	} else if compareMultiExtractAgreement(field.Agreement, field.Conflicts[0].Agreement) <= 0 {
		return fmt.Errorf("%s winner does not outrank its conflicts", name)
	}
	for index := 1; index < len(field.Conflicts); index++ {
		if compareMultiExtractAgreement(field.Conflicts[index-1].Agreement, field.Conflicts[index].Agreement) < 0 {
			return fmt.Errorf("%s conflicts are not score-sorted", name)
		}
	}
	return nil
}

func compareMultiExtractAgreement(first, second models.MultiExtractAgreement) int {
	if first.IndependentRoots != second.IndependentRoots {
		if first.IndependentRoots > second.IndependentRoots {
			return 1
		}
		return -1
	}
	if first.Pages != second.Pages {
		if first.Pages > second.Pages {
			return 1
		}
		return -1
	}
	return 0
}

func validateMultiExtractFieldSupportPartition(field models.MultiExtractFieldConsensus, name string) error {
	seen := make(map[string]struct{})
	consume := func(supports []models.MultiExtractSupport) error {
		for _, support := range supports {
			if _, duplicate := seen[support.URL]; duplicate {
				return fmt.Errorf("%s repeats one source across value groups", name)
			}
			seen[support.URL] = struct{}{}
		}
		return nil
	}
	if err := consume(field.Supports); err != nil {
		return err
	}
	for _, conflict := range field.Conflicts {
		if err := consume(conflict.Supports); err != nil {
			return err
		}
	}
	return nil
}

func validateMultiExtractScalar(raw json.RawMessage, name string) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("%s is invalid: %w", name, err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return fmt.Errorf("%s is invalid: %w", name, err)
	}
	switch value.(type) {
	case nil, bool, json.Number, string:
		return nil
	default:
		return fmt.Errorf("%s must be a JSON scalar", name)
	}
}

func validateMultiExtractEvidence(anchor evidence.Anchor, raw json.RawMessage, name string) error {
	fields, err := decodeExtractJSONObject(raw, name)
	if err != nil {
		return err
	}
	if err := requirePresentJSONFields(fields, name, "quote", "text_range", "method", "snapshot_id", "fetched_at"); err != nil {
		return err
	}
	var textRange []json.RawMessage
	if err := json.Unmarshal(fields["text_range"], &textRange); err != nil || len(textRange) != 2 {
		return fmt.Errorf("%s text_range must contain two offsets", name)
	}
	switch anchor.Method {
	case evidence.MethodExact, evidence.MethodNormalized, evidence.MethodFuzzy, evidence.MethodCompiled:
	default:
		return fmt.Errorf("%s contains an invalid method", name)
	}
	if anchor.Quote == "" || anchor.TextRange[0] < 0 || anchor.TextRange[1] <= anchor.TextRange[0] ||
		anchor.TextRange[1]-anchor.TextRange[0] != len(anchor.Quote) ||
		anchor.SnapshotID == "" || anchor.FetchedAt.IsZero() {
		return fmt.Errorf("%s is not a located anchor", name)
	}
	return nil
}

func validateExtractTokenInfo(info models.TokenInfo, raw json.RawMessage, name string) error {
	fields, err := decodeExtractJSONObject(raw, name)
	if err != nil {
		return err
	}
	if err := requirePresentJSONFields(fields, name, "original_estimate", "cleaned_estimate", "savings_percent"); err != nil {
		return err
	}
	if info.OriginalEstimate < 0 || info.CleanedEstimate < 0 || info.CleanedEstimate > info.OriginalEstimate ||
		math.IsNaN(info.SavingsPercent) || math.IsInf(info.SavingsPercent, 0) || info.SavingsPercent < 0 || info.SavingsPercent > 100 {
		return fmt.Errorf("%s is invalid", name)
	}
	return nil
}

func validateExtractTiming(timing models.ExtractTimingInfo, raw json.RawMessage, name string) error {
	fields, err := decodeExtractJSONObject(raw, name)
	if err != nil {
		return err
	}
	if err := requirePresentJSONFields(fields, name, "total_ms", "navigation_ms", "cleaning_ms", "extraction_ms"); err != nil {
		return err
	}
	if timing.TotalMs < 0 || timing.NavigationMs < 0 || timing.CleaningMs < 0 || timing.ExtractionMs < 0 {
		return fmt.Errorf("%s is invalid", name)
	}
	return nil
}

func validateExtractLLMUsage(usage *models.LLMUsage, raw json.RawMessage, name string) error {
	if usage == nil {
		return fmt.Errorf("%s must be a JSON object", name)
	}
	fields, err := decodeExtractJSONObject(raw, name)
	if err != nil {
		return err
	}
	if err := requirePresentJSONFields(fields, name, "prompt_tokens", "completion_tokens", "total_tokens"); err != nil {
		return err
	}
	maximumInt := int(^uint(0) >> 1)
	if usage.PromptTokens < 0 || usage.CompletionTokens < 0 || usage.TotalTokens < 0 ||
		usage.CompletionTokens > maximumInt-usage.PromptTokens || usage.TotalTokens != usage.PromptTokens+usage.CompletionTokens {
		return fmt.Errorf("%s is invalid", name)
	}
	return nil
}

func validateExtractErrorDetail(detail *models.ErrorDetail, raw json.RawMessage, name string) error {
	if detail == nil {
		return fmt.Errorf("%s must be a JSON object", name)
	}
	fields, err := decodeExtractJSONObject(raw, name)
	if err != nil {
		return err
	}
	if err := rejectUnsupportedJSONFields(fields, name, "code", "message"); err != nil {
		return err
	}
	if err := requirePresentJSONFields(fields, name, "code", "message"); err != nil {
		return err
	}
	if strings.TrimSpace(detail.Code) == "" || strings.TrimSpace(detail.Message) == "" {
		return fmt.Errorf("%s is incomplete", name)
	}
	return nil
}

func decodeExtractJSONObject(raw []byte, name string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("%s must be a JSON object", name)
	}
	return fields, nil
}

func rejectUnsupportedJSONFields(fields map[string]json.RawMessage, objectName string, allowed ...string) error {
	allowlist := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allowlist[name] = struct{}{}
	}
	for name := range fields {
		if _, ok := allowlist[name]; !ok {
			return fmt.Errorf("%s contains unsupported field %q", objectName, name)
		}
	}
	return nil
}

func rejectPresentJSONFields(fields map[string]json.RawMessage, objectName string, names ...string) error {
	for _, name := range names {
		if _, present := fields[name]; present {
			return fmt.Errorf("%s must not contain field %q", objectName, name)
		}
	}
	return nil
}

func requirePresentJSONFields(fields map[string]json.RawMessage, objectName string, names ...string) error {
	for _, name := range names {
		value, ok := fields[name]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("%s is missing required field %q", objectName, name)
		}
	}
	return nil
}

func requireNestedJSONFields(raw json.RawMessage, objectName string, names ...string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return fmt.Errorf("%s must be a JSON object", objectName)
	}
	return requirePresentJSONFields(fields, objectName, names...)
}

func extractAPIError(statusCode int, body []byte) string {
	decoder := json.NewDecoder(bytes.NewReader(body))
	var response *struct {
		Error *models.ErrorDetail `json:"error"`
	}
	if err := decoder.Decode(&response); err == nil && response != nil && requireJSONEOF(decoder) == nil {
		return formatExtractError(response.Error, statusCode)
	}
	return fmt.Sprintf("extraction failed (HTTP %d)", statusCode)
}

func extractResponseError(response models.ExtractResponse, statusCode int) string {
	return formatExtractError(response.Error, statusCode)
}

func formatExtractError(detail *models.ErrorDetail, statusCode int) string {
	if detail != nil {
		code := strings.TrimSpace(detail.Code)
		message := strings.TrimSpace(detail.Message)
		switch {
		case code != "" && message != "":
			return fmt.Sprintf("[%s] %s", code, message)
		case message != "":
			return message
		case code != "":
			return fmt.Sprintf("[%s] extraction failed (HTTP %d)", code, statusCode)
		}
	}
	return fmt.Sprintf("extraction failed (HTTP %d)", statusCode)
}

func redactExtractSecrets(message string, secrets ...string) string {
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	return message
}
