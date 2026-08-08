package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/use-agent/purify/models"
)

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

	params := ExtractParams{APIKey: "test", Model: "test", BaseURL: "https://fallback.test/v1"}
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
	client := NewClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer bad" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		return rawHTTPResponse(http.StatusUnauthorized, `{"error":{"message":"bad key"}}`), nil
	})})

	_, err := client.Extract(context.Background(), "content", json.RawMessage(`{"type":"object"}`), ExtractParams{
		APIKey: "bad", Model: "test", BaseURL: "https://auth.test/v1",
	})
	var scrapeErr *models.ScrapeError
	if !errors.As(err, &scrapeErr) || scrapeErr.Code != models.ErrCodeLLMAuthFailure {
		t.Fatalf("error = %#v, want LLM auth failure", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func chatHTTPResponse(status int, content string) *http.Response {
	body, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
		"usage":   map[string]any{"prompt_tokens": 2, "completion_tokens": 3, "total_tokens": 5},
	})
	return rawHTTPResponse(status, string(body))
}

func rawHTTPResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
