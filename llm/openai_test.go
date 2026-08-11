package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/use-agent/purify/models"
)

func TestNewClientDefaultTimeoutAndInjectedClientUnchanged(t *testing.T) {
	defaultClient := NewClient(nil)
	if defaultClient.httpClient == nil {
		t.Fatal("NewClient(nil) returned a nil HTTP client")
	}
	if defaultClient.httpClient.Timeout != 120*time.Second {
		t.Fatalf("default timeout = %v, want 120s", defaultClient.httpClient.Timeout)
	}

	injected := &http.Client{Timeout: 7 * time.Second}
	client := NewClient(injected)
	if client.httpClient != injected {
		t.Fatal("NewClient replaced the injected HTTP client")
	}
	if injected.Timeout != 7*time.Second {
		t.Fatalf("injected timeout = %v, want unchanged 7s", injected.Timeout)
	}
}

func TestExtractUsesStrictJSONSchema(t *testing.T) {
	var gotFormat responseFormat
	client := NewClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var request chatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			return nil, err
		}
		gotFormat = *request.ResponseFormat
		return chatHTTPResponse(http.StatusOK, `{"name":"Ada"}`), nil
	})})

	result, err := client.Extract(context.Background(), "Ada", json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}}}`), ExtractParams{
		APIKey: "test", Model: "test", BaseURL: "https://strict.test/v1",
	})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if string(result.Data) != `{"name":"Ada"}` {
		t.Fatalf("Data = %s", result.Data)
	}
	if gotFormat.Type != "json_schema" || gotFormat.JSONSchema == nil || !gotFormat.JSONSchema.Strict {
		t.Fatalf("response format = %#v, want strict json_schema", gotFormat)
	}
}

func TestExtractFallsBackAndCachesUnsupportedResponseFormat(t *testing.T) {
	const baseURL = "https://fallback.test/v1"

	var strictCalls atomic.Int32
	var objectCalls atomic.Int32
	client := NewClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		if strings.Contains(string(body), `"type":"json_schema"`) {
			strictCalls.Add(1)
			return rawHTTPResponse(http.StatusBadRequest, `{"error":{"message":"unsupported response_format json_schema"}}`), nil
		}
		objectCalls.Add(1)
		return chatHTTPResponse(http.StatusOK, `{"ok":true}`), nil
	})})

	params := ExtractParams{APIKey: "test", Model: "test", BaseURL: baseURL}
	schema := json.RawMessage(`{"type":"object"}`)
	for i := 0; i < 2; i++ {
		if _, err := client.Extract(context.Background(), "content", schema, params); err != nil {
			t.Fatalf("Extract() call %d error = %v", i+1, err)
		}
	}
	if strictCalls.Load() != 1 {
		t.Fatalf("strict calls = %d, want 1 capability probe", strictCalls.Load())
	}
	if objectCalls.Load() != 2 {
		t.Fatalf("json_object calls = %d, want 2", objectCalls.Load())
	}
}

func TestResponseFormatCapabilityCacheIsIsolatedPerClient(t *testing.T) {
	const baseURL = "https://client-isolation.test/v1"

	params := ExtractParams{APIKey: "test", Model: "test", BaseURL: baseURL}
	schema := json.RawMessage(`{"type":"object"}`)
	unsupported := NewClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		if strings.Contains(string(body), `"type":"json_schema"`) {
			return rawHTTPResponse(http.StatusBadRequest, `{"error":{"message":"unsupported response_format json_schema"}}`), nil
		}
		return chatHTTPResponse(http.StatusOK, `{"ok":true}`), nil
	})})
	if _, err := unsupported.Extract(context.Background(), "content", schema, params); err != nil {
		t.Fatalf("unsupported client fallback error = %v", err)
	}

	var supportedFormat responseFormat
	supported := NewClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var request chatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			return nil, err
		}
		supportedFormat = *request.ResponseFormat
		return chatHTTPResponse(http.StatusOK, `{"ok":true}`), nil
	})})
	if _, err := supported.Extract(context.Background(), "content", schema, params); err != nil {
		t.Fatalf("supported client extraction error = %v", err)
	}
	if supportedFormat.Type != "json_schema" || supportedFormat.JSONSchema == nil || !supportedFormat.JSONSchema.Strict {
		t.Fatalf("second client response format = %#v, want fresh strict json_schema probe", supportedFormat)
	}
}

func TestResponseFormatCapabilityCacheIsIsolatedPerModel(t *testing.T) {
	const baseURL = "https://model-isolation.test/v1"

	var supportedFormat responseFormat
	client := NewClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var request chatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			return nil, err
		}
		if request.Model == "legacy" && request.ResponseFormat.Type == "json_schema" {
			return rawHTTPResponse(http.StatusBadRequest, `{"error":{"message":"unsupported response_format json_schema"}}`), nil
		}
		if request.Model == "supported" {
			supportedFormat = *request.ResponseFormat
		}
		return chatHTTPResponse(http.StatusOK, `{"ok":true}`), nil
	})})

	schema := json.RawMessage(`{"type":"object"}`)
	if _, err := client.Extract(context.Background(), "content", schema, ExtractParams{
		APIKey: "test", Model: "legacy", BaseURL: baseURL,
	}); err != nil {
		t.Fatalf("legacy model fallback error = %v", err)
	}
	if _, err := client.Extract(context.Background(), "content", schema, ExtractParams{
		APIKey: "test", Model: "supported", BaseURL: baseURL,
	}); err != nil {
		t.Fatalf("supported model extraction error = %v", err)
	}
	if supportedFormat.Type != "json_schema" || supportedFormat.JSONSchema == nil || !supportedFormat.JSONSchema.Strict {
		t.Fatalf("supported model response format = %#v, want fresh strict json_schema probe", supportedFormat)
	}
}

func TestResponseFormatCapabilityCacheIsBounded(t *testing.T) {
	var cache responseFormatCapabilityCache
	for i := 0; i <= maxResponseFormatCapabilityEntries; i++ {
		cache.store(responseFormatCapabilityKey{
			baseURL: "https://bounded.test/v1",
			model:   fmt.Sprintf("model-%03d", i),
		}, true)
	}

	cache.mu.Lock()
	entryCount := len(cache.entries)
	orderCount := len(cache.order)
	cache.mu.Unlock()
	if entryCount != maxResponseFormatCapabilityEntries || orderCount != maxResponseFormatCapabilityEntries {
		t.Fatalf("cache sizes = entries %d, order %d; want both %d", entryCount, orderCount, maxResponseFormatCapabilityEntries)
	}
	if _, ok := cache.load(responseFormatCapabilityKey{baseURL: "https://bounded.test/v1", model: "model-000"}); ok {
		t.Fatal("oldest capability entry survived FIFO eviction")
	}
	if supported, ok := cache.load(responseFormatCapabilityKey{baseURL: "https://bounded.test/v1", model: "model-128"}); !ok || !supported {
		t.Fatalf("newest capability entry = (%v, %v), want (true, true)", supported, ok)
	}
}

func TestExtractWithRepairIncludesPreviousOutputAndViolations(t *testing.T) {
	client := NewClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var request chatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			return nil, err
		}
		joined := request.Messages[0].Content + "\n" + request.Messages[1].Content
		for _, expected := range []string{"/price", "must be number", `{"price":"oops"}`, "Source content"} {
			if !strings.Contains(joined, expected) {
				t.Errorf("repair prompt missing %q: %s", expected, joined)
			}
		}
		return chatHTTPResponse(http.StatusOK, `{"price":29.99}`), nil
	})})

	_, err := client.ExtractWithRepair(
		context.Background(),
		"Price is 29.99",
		json.RawMessage(`{"type":"object","properties":{"price":{"type":"number"}}}`),
		json.RawMessage(`{"price":"oops"}`),
		[]Violation{{Path: "/price", Message: "must be number"}},
		ExtractParams{APIKey: "test", Model: "test", BaseURL: "https://repair.test/v1"},
	)
	if err != nil {
		t.Fatalf("ExtractWithRepair() error = %v", err)
	}
}

func TestFallbackPreservesLLMErrorClassification(t *testing.T) {
	const secret = "provider-auth-secret"
	client := NewClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer bad" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		return rawHTTPResponse(http.StatusUnauthorized, `{"error":{"message":"`+secret+`"}}`), nil
	})})

	_, err := client.Extract(context.Background(), "content", json.RawMessage(`{"type":"object"}`), ExtractParams{
		APIKey: "bad", Model: "test", BaseURL: "https://auth.test/v1",
	})
	var scrapeErr *models.ScrapeError
	if !errors.As(err, &scrapeErr) || scrapeErr.Code != models.ErrCodeLLMAuthFailure ||
		scrapeErr.Message != "LLM authentication failed" {
		t.Fatalf("error = %#v, want LLM auth failure", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked provider body: %v", err)
	}
}

func TestProviderErrorsUseFixedSanitizedMessages(t *testing.T) {
	const secret = "provider-error-secret"
	for _, test := range []struct {
		name     string
		status   int
		wantCode string
		wantMsg  string
	}{
		{name: "forbidden", status: http.StatusForbidden, wantCode: models.ErrCodeLLMAuthFailure, wantMsg: "LLM authentication failed"},
		{name: "rate limited", status: http.StatusTooManyRequests, wantCode: models.ErrCodeLLMRateLimited, wantMsg: "LLM rate limit exceeded"},
		{name: "provider failure", status: http.StatusInternalServerError, wantCode: models.ErrCodeLLMFailure, wantMsg: "LLM API returned HTTP 500"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := NewClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return rawHTTPResponse(test.status, `{"error":{"message":"`+secret+`"}}`), nil
			})})

			_, err := client.Extract(context.Background(), "content", json.RawMessage(`{"type":"object"}`), ExtractParams{
				APIKey: "request-secret", Model: "test", BaseURL: "https://sanitized-" + strings.ReplaceAll(test.name, " ", "-") + ".test/v1",
			})
			var scrapeErr *models.ScrapeError
			if !errors.As(err, &scrapeErr) || scrapeErr.Code != test.wantCode || scrapeErr.Message != test.wantMsg {
				t.Fatalf("error = %#v, want code %q message %q", err, test.wantCode, test.wantMsg)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "request-secret") {
				t.Fatalf("error leaked provider body or request secret: %v", err)
			}
		})
	}
}

func TestProviderResponseBodyLimitExactBoundary(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		body := padResponseBody(t, chatHTTPBody(`{"ok":true}`), MaxLLMResponseBytes)
		client := NewClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return rawHTTPResponse(http.StatusOK, body), nil
		})})

		result, err := client.Extract(context.Background(), "content", json.RawMessage(`{"type":"object"}`), ExtractParams{
			APIKey: "test", Model: "test", BaseURL: "https://response-limit-success.test/v1",
		})
		if err != nil {
			t.Fatalf("Extract() at exact limit error = %v", err)
		}
		if string(result.Data) != `{"ok":true}` {
			t.Fatalf("Data = %s", result.Data)
		}
	})

	t.Run("provider error", func(t *testing.T) {
		const secret = "exact-limit-provider-secret"
		body := padResponseBody(t, `{"error":{"message":"`+secret+`"}}`, MaxLLMResponseBytes)
		client := NewClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return rawHTTPResponse(http.StatusUnauthorized, body), nil
		})})

		_, err := client.Extract(context.Background(), "content", json.RawMessage(`{"type":"object"}`), ExtractParams{
			APIKey: "test", Model: "test", BaseURL: "https://response-limit-error.test/v1",
		})
		var scrapeErr *models.ScrapeError
		if !errors.As(err, &scrapeErr) || scrapeErr.Code != models.ErrCodeLLMAuthFailure ||
			scrapeErr.Message != "LLM authentication failed" {
			t.Fatalf("error at exact limit = %#v, want LLM auth failure", err)
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(scrapeErr.Message, secret) {
			t.Fatalf("exact-limit error leaked provider body: %v", err)
		}
	})
}

func TestProviderResponseBodyLimitRejectsNPlusOneWithoutRetainingBody(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			const secret = "provider-response-secret"
			body := secret + strings.Repeat("x", MaxLLMResponseBytes+1-len(secret))
			var calls atomic.Int32
			client := NewClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return rawHTTPResponse(status, body), nil
			})})

			_, err := client.Extract(context.Background(), "content", json.RawMessage(`{"type":"object"}`), ExtractParams{
				APIKey: "request-secret", Model: "test", BaseURL: "https://response-over-limit-" + strings.ReplaceAll(http.StatusText(status), " ", "-") + ".test/v1",
			})
			var scrapeErr *models.ScrapeError
			if !errors.As(err, &scrapeErr) || scrapeErr.Code != models.ErrCodeLLMFailure ||
				scrapeErr.Message != "LLM response exceeds maximum size" || scrapeErr.Err != nil {
				t.Fatalf("error = %#v, want fixed body-limit LLM failure", err)
			}
			var providerErr *providerResponseError
			if errors.As(err, &providerErr) {
				t.Fatal("oversized response retained a provider response error")
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "request-secret") {
				t.Fatalf("error leaked response body or request secret: %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("provider calls = %d, want no response_format fallback", calls.Load())
			}
		})
	}
}

func TestProviderResponseReadErrorIsSanitizedAndPreservesContext(t *testing.T) {
	t.Run("generic read error", func(t *testing.T) {
		const secret = "provider-read-secret"
		client := NewClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return responseWithBody(http.StatusOK, errorReadCloser{err: errors.New(secret)}), nil
		})})

		_, err := client.Extract(context.Background(), "content", json.RawMessage(`{"type":"object"}`), ExtractParams{
			APIKey: "request-secret", Model: "test", BaseURL: "https://response-read-error.test/v1",
		})
		assertSanitizedReadError(t, err, nil, secret, "request-secret")
	})

	t.Run("context cancellation", func(t *testing.T) {
		const secret = "provider-context-secret"
		ctx, cancel := context.WithCancel(context.Background())
		client := NewClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return responseWithBody(http.StatusOK, errorReadCloser{
				beforeError: cancel,
				err:         errors.New(secret),
			}), nil
		})})

		_, err := client.Extract(ctx, "content", json.RawMessage(`{"type":"object"}`), ExtractParams{
			APIKey: "request-secret", Model: "test", BaseURL: "https://response-read-context.test/v1",
		})
		assertSanitizedReadError(t, err, context.Canceled, secret, "request-secret")
	})
}

func assertSanitizedReadError(t *testing.T, err, wantCause error, forbidden ...string) {
	t.Helper()
	var scrapeErr *models.ScrapeError
	if !errors.As(err, &scrapeErr) || scrapeErr.Code != models.ErrCodeLLMFailure ||
		scrapeErr.Message != "failed to read LLM response" {
		t.Fatalf("error = %#v, want sanitized response read failure", err)
	}
	if wantCause == nil && scrapeErr.Err != nil {
		t.Fatalf("read error retained unsafe cause: %v", scrapeErr.Err)
	}
	if wantCause != nil && !errors.Is(err, wantCause) {
		t.Fatalf("error = %v, want cause %v", err, wantCause)
	}
	for _, value := range forbidden {
		if strings.Contains(err.Error(), value) {
			t.Fatalf("error leaked %q: %v", value, err)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func chatHTTPResponse(status int, content string) *http.Response {
	return rawHTTPResponse(status, chatHTTPBody(content))
}

func chatHTTPBody(content string) string {
	body, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
		"usage":   map[string]any{"prompt_tokens": 2, "completion_tokens": 3, "total_tokens": 5},
	})
	return string(body)
}

func rawHTTPResponse(status int, body string) *http.Response {
	return responseWithBody(status, io.NopCloser(strings.NewReader(body)))
}

func responseWithBody(status int, body io.ReadCloser) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       body,
	}
}

func padResponseBody(t *testing.T, body string, size int) string {
	t.Helper()
	if len(body) > size {
		t.Fatalf("base response is %d bytes, exceeds requested %d", len(body), size)
	}
	return body + strings.Repeat(" ", size-len(body))
}

type errorReadCloser struct {
	beforeError func()
	err         error
}

func (reader errorReadCloser) Read([]byte) (int, error) {
	if reader.beforeError != nil {
		reader.beforeError()
	}
	return 0, reader.err
}

func (errorReadCloser) Close() error { return nil }
