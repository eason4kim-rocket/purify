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
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/use-agent/purify/evidence"
	"github.com/use-agent/purify/llm"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/publicnet"
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
		if err := requireNestedJSONFields(fields["error"], "error", "code", "message"); err != nil {
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
			if err := validateMultiExtractAgreementFields(fieldFields["agreement"], "consensus field "+path+" agreement"); err != nil {
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
	if err := validateMultiExtractAgreementFields(raw, name+" agreement"); err != nil {
		return err
	}
	if agreement.Pages < 1 || agreement.IndependentRoots < 1 || agreement.IndependentRoots > agreement.Pages ||
		agreement.IndependentRoots > uniqueRoots ||
		agreement.Pages != supports {
		return fmt.Errorf("%s contains invalid agreement", name)
	}
	return nil
}

func validateMultiExtractAgreementFields(raw json.RawMessage, name string) error {
	fields, err := decodeExtractJSONObject(raw, name)
	if err != nil {
		return err
	}
	return requirePresentJSONFields(fields, name, "pages", "independent_roots")
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
		if err := requirePresentJSONFields(fields, supportName, "url", "root", "evidence", "receipt"); err != nil {
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
