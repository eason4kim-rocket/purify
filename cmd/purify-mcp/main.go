package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	urlpkg "net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/use-agent/purify/llm"
	"github.com/use-agent/purify/models"
)

const (
	maxAPIResponseBytes          int64 = 32 << 20
	maxVerifyURLBytes                  = 16 << 10
	maxVerifyClaimsJSONBytes           = 512 << 10
	maxExtractSchemaJSONBytes          = 512 << 10
	maxExtractLLMCredentialBytes       = 16 << 10
	maxExtractLLMModelBytes            = 256
	maxExtractLLMBaseURLBytes          = 16 << 10
)

type apiHTTPResponse struct {
	StatusCode int
	Body       []byte
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
		mcp.WithDescription("Scrape a web page and extract structured data. The default auto engine uses a compiled extractor first and falls back to the caller's LLM only when an LLM API key is supplied."),
		mcp.WithString("url",
			mcp.Required(),
			mcp.Description("The URL of the web page to scrape"),
			mcp.MaxLength(maxVerifyURLBytes),
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

		url, err := requiredExtractString(arguments, "url")
		if err != nil || strings.TrimSpace(url) == "" {
			return mcp.NewToolResultError("url is required and must be a non-empty string"), nil
		}
		if len(url) > maxVerifyURLBytes {
			return mcp.NewToolResultError("url exceeds the 16384-byte limit"), nil
		}
		url = strings.TrimSpace(url)
		parsedURL, err := urlpkg.ParseRequestURI(url)
		if err != nil || parsedURL.Host == "" || parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
			return mcp.NewToolResultError("url must be an absolute http or https URL"), nil
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

		payload := models.ExtractRequest{
			URL:    url,
			Schema: append(json.RawMessage(nil), schemaJSON...),
			Engine: engine,
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
	"url": {}, "schema": {}, "engine": {}, "llm_api_key": {}, "llm_model": {}, "llm_base_url": {},
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
