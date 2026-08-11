package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/use-agent/purify/models"
)

const (
	// MaxLLMResponseBytes caps the complete provider response envelope. The
	// 32 MiB budget leaves room for a 4 MiB structured JSON value after it is
	// nested and escaped inside choices[].message.content.
	MaxLLMResponseBytes = 32 << 20

	defaultHTTPTimeout = 120 * time.Second

	maxResponseFormatCapabilityEntries = 128
)

// Client is a lightweight OpenAI-compatible API client for structured extraction.
// It uses net/http directly — no third-party SDK needed.
type Client struct {
	httpClient *http.Client

	// rfSupport is per Client so separately configured transports cannot
	// influence each other's capability probes.
	rfSupport *responseFormatCapabilityCache
}

type responseFormatCapabilityKey struct {
	baseURL string
	model   string
}

// responseFormatCapabilityCache remembers whether an OpenAI-compatible
// endpoint/model pair accepts strict response_format=json_schema. It uses
// deterministic FIFO eviction to bound attacker-controlled BYOK entries.
type responseFormatCapabilityCache struct {
	mu      sync.Mutex
	entries map[responseFormatCapabilityKey]bool
	order   []responseFormatCapabilityKey
}

func (c *responseFormatCapabilityCache) load(key responseFormatCapabilityKey) (bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	supported, ok := c.entries[key]
	return supported, ok
}

func (c *responseFormatCapabilityCache) store(key responseFormatCapabilityKey, supported bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.entries == nil {
		c.entries = make(map[responseFormatCapabilityKey]bool, maxResponseFormatCapabilityEntries)
	}
	if _, ok := c.entries[key]; ok {
		c.entries[key] = supported
		return
	}
	if len(c.entries) == maxResponseFormatCapabilityEntries {
		delete(c.entries, c.order[0])
		c.order = c.order[1:]
	}
	c.entries[key] = supported
	c.order = append(c.order, key)
}

// NewClient creates a new LLM client with the given http.Client. A supplied
// client is used unchanged; nil selects a client with a bounded timeout.
func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &Client{
		httpClient: httpClient,
		rfSupport:  &responseFormatCapabilityCache{},
	}
}

// ExtractParams holds per-request LLM configuration (BYOK).
type ExtractParams struct {
	APIKey  string
	Model   string
	BaseURL string // e.g. "https://api.openai.com/v1"
}

// ExtractResult holds the LLM extraction output.
type ExtractResult struct {
	Data  json.RawMessage
	Usage *models.LLMUsage
}

// chatRequest is the OpenAI chat completion request body.
type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	Temperature    float64         `json:"temperature"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseFormat struct {
	Type       string            `json:"type"`
	JSONSchema *jsonSchemaFormat `json:"json_schema,omitempty"`
}

type jsonSchemaFormat struct {
	Name   string          `json:"name"`
	Strict bool            `json:"strict"`
	Schema json.RawMessage `json:"schema"`
}

// chatResponse is the minimal OpenAI chat completion response we need.
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

type providerResponseError struct {
	status int
	body   []byte
}

func (e *providerResponseError) Error() string {
	return fmt.Sprintf("LLM API returned HTTP %d", e.status)
}

// Extract sends the cleaned content + schema to the LLM and returns structured JSON.
func (c *Client) Extract(ctx context.Context, content string, schema json.RawMessage, params ExtractParams) (*ExtractResult, error) {
	return c.extract(ctx, schema, params, []chatMessage{
		{Role: "system", Content: buildSystemPrompt(schema)},
		{Role: "user", Content: content},
	})
}

// ExtractWithRepair performs the one allowed schema-repair attempt. The
// previous output and concrete violations are supplied so the model can make
// the smallest necessary correction without rewriting valid fields.
func (c *Client) ExtractWithRepair(
	ctx context.Context,
	content string,
	schema, previous json.RawMessage,
	violations []Violation,
	params ExtractParams,
) (*ExtractResult, error) {
	return c.extract(ctx, schema, params, []chatMessage{
		{Role: "system", Content: buildRepairPrompt(schema, violations)},
		{Role: "user", Content: fmt.Sprintf("Source content:\n%s\n\nPrevious JSON output:\n%s", content, previous)},
	})
}

func (c *Client) extract(ctx context.Context, schema json.RawMessage, params ExtractParams, messages []chatMessage) (*ExtractResult, error) {
	baseURL := strings.TrimRight(params.BaseURL, "/")
	capabilityKey := responseFormatCapabilityKey{baseURL: baseURL, model: params.Model}
	strict := true
	if supported, ok := c.rfSupport.load(capabilityKey); ok {
		strict = supported
	}

	result, err := c.sendChat(ctx, schema, params, messages, strict)
	if err == nil {
		if strict {
			c.rfSupport.store(capabilityKey, true)
		}
		return result, nil
	}

	var providerErr *providerResponseError
	if strict && errors.As(err, &providerErr) && providerErr.status == http.StatusBadRequest &&
		strings.Contains(strings.ToLower(string(providerErr.body)), "response_format") {
		c.rfSupport.store(capabilityKey, false)
		result, fallbackErr := c.sendChat(ctx, schema, params, messages, false)
		if fallbackErr != nil {
			return nil, normalizeProviderError(fallbackErr)
		}
		return result, nil
	}
	return nil, normalizeProviderError(err)
}

func normalizeProviderError(err error) error {
	var providerErr *providerResponseError
	if errors.As(err, &providerErr) {
		return classifyLLMError(providerErr.status)
	}
	return err
}

func (c *Client) sendChat(
	ctx context.Context,
	schema json.RawMessage,
	params ExtractParams,
	messages []chatMessage,
	strict bool,
) (*ExtractResult, error) {
	format := &responseFormat{Type: "json_object"}
	if strict {
		format = &responseFormat{
			Type: "json_schema",
			JSONSchema: &jsonSchemaFormat{
				Name:   "purify_extract",
				Strict: true,
				Schema: schema,
			},
		}
	}

	reqBody := chatRequest{
		Model:          params.Model,
		Messages:       messages,
		Temperature:    0,
		ResponseFormat: format,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	// Build URL: baseURL + /chat/completions
	endpoint := strings.TrimRight(params.BaseURL, "/") + "/chat/completions"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+params.APIKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, models.NewScrapeError(models.ErrCodeLLMFailure, "LLM request failed", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, MaxLLMResponseBytes+1))
	if err != nil {
		var safeCause error
		switch {
		case ctx.Err() != nil:
			safeCause = ctx.Err()
		case errors.Is(err, context.Canceled):
			safeCause = context.Canceled
		case errors.Is(err, context.DeadlineExceeded):
			safeCause = context.DeadlineExceeded
		}
		return nil, models.NewScrapeError(models.ErrCodeLLMFailure, "failed to read LLM response", safeCause)
	}
	if len(respBody) > MaxLLMResponseBytes {
		return nil, models.NewScrapeError(models.ErrCodeLLMFailure, "LLM response exceeds maximum size", nil)
	}

	// Handle error status codes.
	if resp.StatusCode != http.StatusOK {
		return nil, &providerResponseError{status: resp.StatusCode, body: respBody}
	}

	var chatResp chatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return nil, models.NewScrapeError(models.ErrCodeLLMFailure, "failed to parse LLM response", err)
	}

	if len(chatResp.Choices) == 0 {
		return nil, models.NewScrapeError(models.ErrCodeLLMFailure, "LLM returned no choices", nil)
	}

	raw := chatResp.Choices[0].Message.Content

	// Validate that the response is valid JSON.
	if !json.Valid([]byte(raw)) {
		return nil, models.NewScrapeError(models.ErrCodeLLMFailure, "LLM returned invalid JSON", nil)
	}

	return &ExtractResult{
		Data: json.RawMessage(raw),
		Usage: &models.LLMUsage{
			PromptTokens:     chatResp.Usage.PromptTokens,
			CompletionTokens: chatResp.Usage.CompletionTokens,
			TotalTokens:      chatResp.Usage.TotalTokens,
		},
	}, nil
}

// buildSystemPrompt creates the system prompt for structured extraction.
func buildSystemPrompt(schema json.RawMessage) string {
	return fmt.Sprintf(`You are a structured data extraction assistant. Extract information from the provided content and return it as JSON matching the following schema.

Schema:
%s

Rules:
- Return ONLY valid JSON, no markdown fences or explanation.
- If a field cannot be found in the content, use null.
- Extract exactly the fields specified in the schema.`, string(schema))
}

func buildRepairPrompt(schema json.RawMessage, violations []Violation) string {
	violationJSON, _ := json.Marshal(violations)
	return fmt.Sprintf(`You repair structured extraction JSON. Return a corrected JSON value that strictly matches the schema.

Schema:
%s

Validation violations from the previous output:
%s

Rules:
- Return ONLY valid JSON, with no markdown fences or explanation.
- Preserve every field from the previous output that is already correct.
- Change only fields needed to resolve the listed violations.
- Use the source content as the sole factual basis.
- Include exactly the fields allowed by the schema.`, string(schema), string(violationJSON))
}

// classifyLLMError maps HTTP status codes to fixed public errors. Provider
// response bodies are deliberately excluded because they may echo secrets or
// user content.
func classifyLLMError(statusCode int) *models.ScrapeError {
	switch {
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		return models.NewScrapeError(models.ErrCodeLLMAuthFailure, "LLM authentication failed", nil)
	case statusCode == http.StatusTooManyRequests:
		return models.NewScrapeError(models.ErrCodeLLMRateLimited, "LLM rate limit exceeded", nil)
	default:
		return models.NewScrapeError(models.ErrCodeLLMFailure, fmt.Sprintf("LLM API returned HTTP %d", statusCode), nil)
	}
}
