package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/use-agent/purify/models"
)

const validClaimsJSON = `[
  {
    "path": "price",
    "value": 19,
    "anchor": {
      "quote": "19",
      "text_range": [0, 2],
      "method": "exact",
      "snapshot_id": "snap-old",
      "fetched_at": "2026-08-08T00:00:00Z"
    }
  }
]`

func TestVerifyFactToolContract(t *testing.T) {
	t.Parallel()

	tool := newVerifyFactTool()
	if tool.Name != "verify_fact" {
		t.Fatalf("tool name = %q, want verify_fact", tool.Name)
	}
	if !reflect.DeepEqual(tool.InputSchema.Required, []string{"url", "claims"}) {
		t.Fatalf("required = %#v, want url and claims", tool.InputSchema.Required)
	}
	if got, ok := tool.InputSchema.AdditionalProperties.(bool); !ok || got {
		t.Fatalf("additionalProperties = %#v, want false", tool.InputSchema.AdditionalProperties)
	}
	for _, name := range []string{"url", "claims"} {
		property, ok := tool.InputSchema.Properties[name].(map[string]any)
		if !ok {
			t.Fatalf("property %q has type %T", name, tool.InputSchema.Properties[name])
		}
		if property["type"] != "string" {
			t.Fatalf("property %q type = %#v, want string", name, property["type"])
		}
	}
	assertHint(t, "readOnlyHint", tool.Annotations.ReadOnlyHint, false)
	assertHint(t, "destructiveHint", tool.Annotations.DestructiveHint, false)
	assertHint(t, "idempotentHint", tool.Annotations.IdempotentHint, false)
	assertHint(t, "openWorldHint", tool.Annotations.OpenWorldHint, true)
}

func TestHandleVerifyFactSuccess(t *testing.T) {
	t.Parallel()

	verifiedAt := time.Date(2026, time.August, 9, 1, 2, 3, 0, time.UTC)
	wantResponse := models.VerifyResponse{
		VerificationID: "verify-1",
		URL:            "https://example.com/product",
		FinalURL:       "https://example.com/product",
		StatusCode:     http.StatusOK,
		Results: []models.ClaimResult{
			{Path: "price", Status: models.VerifyStatusConfirmed},
			{Path: "availability", Status: models.VerifyStatusChanged, NewValue: json.RawMessage(`false`)},
			{Path: "subtitle", Status: models.VerifyStatusGone, GoneScope: models.VerifyGoneScopeField},
		},
		SnapshotID: "snap-new",
		VerifiedAt: verifiedAt,
	}
	responseBody, err := json.Marshal(wantResponse)
	if err != nil {
		t.Fatal(err)
	}

	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", req.Method)
		}
		if req.URL.String() != "http://purify.test/api/v1/verify" {
			t.Errorf("URL = %s", req.URL)
		}
		if got := req.Header.Get("X-API-Key"); got != "secret-key" {
			t.Errorf("X-API-Key = %q, want secret-key", got)
		}
		if got := req.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}

		var payload models.VerifyRequest
		decoder := json.NewDecoder(req.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if payload.URL != wantResponse.URL || len(payload.Claims) != 1 {
			t.Errorf("payload = %#v", payload)
		}

		return httpResponse(http.StatusOK, responseBody), nil
	})}
	handler := handleVerifyFactWithClient(client, "http://purify.test", "secret-key")

	result, protocolErr := handler(context.Background(), verifyRequest(wantResponse.URL, validClaimsJSON))
	if protocolErr != nil {
		t.Fatalf("protocol error: %v", protocolErr)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %s", toolResultText(t, result))
	}
	gotResponse, ok := result.StructuredContent.(models.VerifyResponse)
	if !ok {
		t.Fatalf("structured content type = %T, want models.VerifyResponse", result.StructuredContent)
	}
	if !reflect.DeepEqual(gotResponse, wantResponse) {
		t.Fatalf("structured content = %#v, want %#v", gotResponse, wantResponse)
	}

	pretty := toolResultText(t, result)
	if !strings.Contains(pretty, "\n  \"verification_id\": \"verify-1\"") {
		t.Fatalf("fallback is not pretty JSON: %q", pretty)
	}
	var fallback models.VerifyResponse
	if err := json.Unmarshal([]byte(pretty), &fallback); err != nil {
		t.Fatalf("fallback is not JSON: %v", err)
	}
}

func TestHandleVerifyFactRejectsInvalidClaimsBeforeHTTP(t *testing.T) {
	t.Parallel()

	invalid := []string{
		"",
		"{}",
		"[]",
		"null",
		`[{"path":"price"}] {}`,
		`[{"path":"price","unknown":true}]`,
		validClaimsJSON + validClaimsJSON,
	}
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return httpResponse(http.StatusInternalServerError, nil), nil
	})}
	handler := handleVerifyFactWithClient(client, "http://purify.test", "secret-key")

	for _, claims := range invalid {
		claims := claims
		t.Run(claims, func(t *testing.T) {
			result, protocolErr := handler(context.Background(), verifyRequest("https://example.com", claims))
			if protocolErr != nil {
				t.Fatalf("protocol error: %v", protocolErr)
			}
			if !result.IsError {
				t.Fatalf("expected tool error, got %s", toolResultText(t, result))
			}
		})
	}
	result, protocolErr := handler(context.Background(), verifyRequest(
		"https://example.com",
		strings.Repeat(" ", maxVerifyClaimsJSONBytes+1),
	))
	if protocolErr != nil || !result.IsError {
		t.Fatalf("oversized claims = (%#v, %v), want tool error", result, protocolErr)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("HTTP calls = %d, want 0", got)
	}
}

func TestHandleVerifyFactHTTPErrorIsToolError(t *testing.T) {
	t.Parallel()

	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return httpResponse(http.StatusUnprocessableEntity, []byte(`{"error":{"code":"EVIDENCE_UNAVAILABLE","message":"old snapshot is missing"}}`)), nil
	})}
	handler := handleVerifyFactWithClient(client, "http://purify.test", "secret-key")

	result, protocolErr := handler(context.Background(), verifyRequest("https://example.com", validClaimsJSON))
	if protocolErr != nil {
		t.Fatalf("non-2xx returned a protocol error: %v", protocolErr)
	}
	if !result.IsError {
		t.Fatal("non-2xx response was not a tool error")
	}
	if got, want := toolResultText(t, result), "[EVIDENCE_UNAVAILABLE] old snapshot is missing"; got != want {
		t.Fatalf("tool error = %q, want %q", got, want)
	}
}

func TestHandleVerifyFactRejectsMalformedSuccessResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{name: "invalid JSON", body: `{"verification_id":`},
		{name: "null", body: `null`},
		{name: "trailing value", body: `{"verification_id":"one","verified_at":"2026-08-09T00:00:00Z"} {}`},
		{name: "unknown field", body: `{"verification_id":"one","verified_at":"2026-08-09T00:00:00Z","unexpected":true}`},
		{name: "missing required fields", body: `{}`},
		{name: "invalid result status", body: `{"verification_id":"one","url":"https://example.com","final_url":"https://example.com","status_code":200,"results":[{"path":"price","status":"maybe"}],"verified_at":"2026-08-09T00:00:00Z"}`},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return httpResponse(http.StatusOK, []byte(test.body)), nil
			})}
			handler := handleVerifyFactWithClient(client, "http://purify.test", "secret-key")

			result, protocolErr := handler(context.Background(), verifyRequest("https://example.com", validClaimsJSON))
			if protocolErr != nil {
				t.Fatalf("protocol error: %v", protocolErr)
			}
			if !result.IsError {
				t.Fatalf("expected tool error, got %s", toolResultText(t, result))
			}
			if !strings.Contains(toolResultText(t, result), "failed to parse verify response") {
				t.Fatalf("unexpected tool error: %s", toolResultText(t, result))
			}
		})
	}
}

func TestAPIPostResponseRejectsOversizeBodyAndRetainsStatus(t *testing.T) {
	t.Parallel()

	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		body := io.NopCloser(io.LimitReader(zeroReader{}, maxAPIResponseBytes+1))
		return &http.Response{StatusCode: http.StatusBadGateway, Body: body, Header: make(http.Header)}, nil
	})}

	response, err := apiPostResponse(context.Background(), client, "http://purify.test", "secret-key", "/api/v1/verify", map[string]string{"url": "https://example.com"})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error = %v, want oversize error", err)
	}
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusBadGateway)
	}
	if response.Body != nil {
		t.Fatalf("oversize body retained %d bytes", len(response.Body))
	}
}

func TestHandleVerifyFactContextCancellationIsToolError(t *testing.T) {
	t.Parallel()

	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	handler := handleVerifyFactWithClient(client, "http://purify.test", "secret-key")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, protocolErr := handler(ctx, verifyRequest("https://example.com", validClaimsJSON))
	if protocolErr != nil {
		t.Fatalf("context cancellation returned a protocol error: %v", protocolErr)
	}
	if !result.IsError {
		t.Fatal("context cancellation was not a tool error")
	}
	if !strings.Contains(toolResultText(t, result), context.Canceled.Error()) {
		t.Fatalf("tool error = %q, want context cancellation", toolResultText(t, result))
	}
}

func assertHint(t *testing.T, name string, got *bool, want bool) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s = %v, want %t", name, got, want)
	}
}

func verifyRequest(url, claims string) mcp.CallToolRequest {
	return mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: map[string]any{
		"url":    url,
		"claims": claims,
	}}}
}

func toolResultText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if result == nil || len(result.Content) != 1 {
		t.Fatalf("unexpected result content: %#v", result)
	}
	content, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("content type = %T, want mcp.TextContent", result.Content[0])
	}
	return content.Text
}

func httpResponse(status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     make(http.Header),
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
