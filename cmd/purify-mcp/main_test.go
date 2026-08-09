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

const validExtractSchemaJSON = `{"type":"object","properties":{"name":{"type":"string"}},"required":["name"],"additionalProperties":false}`

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

func TestExtractDataToolContract(t *testing.T) {
	t.Parallel()

	tool := newExtractDataTool()
	if tool.Name != "extract_data" {
		t.Fatalf("tool name = %q, want extract_data", tool.Name)
	}
	if !reflect.DeepEqual(tool.InputSchema.Required, []string{"url", "schema"}) {
		t.Fatalf("required = %#v, want url and schema", tool.InputSchema.Required)
	}
	if got, ok := tool.InputSchema.AdditionalProperties.(bool); !ok || got {
		t.Fatalf("additionalProperties = %#v, want false", tool.InputSchema.AdditionalProperties)
	}
	engine, ok := tool.InputSchema.Properties["engine"].(map[string]any)
	if !ok {
		t.Fatalf("engine property has type %T", tool.InputSchema.Properties["engine"])
	}
	if got := engine["default"]; got != "auto" {
		t.Fatalf("engine default = %#v, want auto", got)
	}
	if got := engine["enum"]; !reflect.DeepEqual(got, []string{"auto", "compiled", "llm"}) {
		t.Fatalf("engine enum = %#v", got)
	}
	if got := tool.InputSchema.Properties["llm_api_key"].(map[string]any)["type"]; got != "string" {
		t.Fatalf("llm_api_key type = %#v, want string", got)
	}
	limits := map[string]int{
		"url":          maxVerifyURLBytes,
		"schema":       maxExtractSchemaJSONBytes,
		"llm_api_key":  maxExtractLLMCredentialBytes,
		"llm_model":    maxExtractLLMModelBytes,
		"llm_base_url": maxExtractLLMBaseURLBytes,
	}
	for name, want := range limits {
		property := tool.InputSchema.Properties[name].(map[string]any)
		if got := property["maxLength"]; got != want {
			t.Fatalf("%s maxLength = %#v, want %d", name, got, want)
		}
	}
	assertHint(t, "readOnlyHint", tool.Annotations.ReadOnlyHint, false)
	assertHint(t, "destructiveHint", tool.Annotations.DestructiveHint, false)
	assertHint(t, "idempotentHint", tool.Annotations.IdempotentHint, false)
	assertHint(t, "openWorldHint", tool.Annotations.OpenWorldHint, true)
}

func TestHandleExtractDataRequestMatrixAndCredentialSeparation(t *testing.T) {
	tests := []struct {
		name              string
		arguments         map[string]any
		wantEngine        string
		wantLLMCredential bool
	}{
		{
			name: "auto defaults without LLM fallback credential",
			arguments: map[string]any{
				"url": "https://example.com/product", "schema": validExtractSchemaJSON,
			},
			wantEngine: "auto",
		},
		{
			name: "auto forwards caller fallback settings",
			arguments: map[string]any{
				"url": "https://example.com/product", "schema": validExtractSchemaJSON,
				"engine": "auto", "llm_api_key": "caller-llm-secret",
				"llm_model": "caller-model", "llm_base_url": "https://llm.example/v1",
			},
			wantEngine:        "auto",
			wantLLMCredential: true,
		},
		{
			name: "compiled strips all LLM settings",
			arguments: map[string]any{
				"url": "https://example.com/product", "schema": validExtractSchemaJSON,
				"engine": "compiled", "llm_api_key": "caller-llm-secret",
				"llm_model": "must-not-forward", "llm_base_url": "https://must-not-forward.example/v1",
			},
			wantEngine: "compiled",
		},
		{
			name: "llm keeps API auth separate from caller credential",
			arguments: map[string]any{
				"url": "https://example.com/product", "schema": validExtractSchemaJSON,
				"engine": "llm", "llm_api_key": "caller-llm-secret",
				"llm_model": "caller-model", "llm_base_url": "https://llm.example/v1",
			},
			wantEngine:        "llm",
			wantLLMCredential: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			responseBody := validExtractResponseBody(t)
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.Method != http.MethodPost || request.URL.String() != "http://purify.test/api/v1/extract" {
					t.Errorf("request = %s %s", request.Method, request.URL)
				}
				if got := request.Header.Get("X-API-Key"); got != "purify-api-secret" {
					t.Errorf("X-API-Key = %q, want Purify API credential", got)
				}
				if got := request.Header.Get("Content-Type"); got != "application/json" {
					t.Errorf("Content-Type = %q, want application/json", got)
				}

				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Errorf("read body: %v", err)
				}
				if bytes.Contains(body, []byte("purify-api-secret")) {
					t.Errorf("Purify API credential leaked into body: %s", body)
				}
				var raw map[string]json.RawMessage
				if err := json.Unmarshal(body, &raw); err != nil {
					t.Errorf("decode raw request: %v", err)
				}
				for _, field := range []string{"llm_api_key", "llm_model", "llm_base_url"} {
					_, present := raw[field]
					if present != test.wantLLMCredential {
						t.Errorf("field %q present = %t, want %t; body=%s", field, present, test.wantLLMCredential, body)
					}
				}

				decoder := json.NewDecoder(bytes.NewReader(body))
				decoder.DisallowUnknownFields()
				var payload models.ExtractRequest
				if err := decoder.Decode(&payload); err != nil {
					t.Errorf("decode request: %v", err)
				}
				if err := requireJSONEOF(decoder); err != nil {
					t.Errorf("request trailing data: %v", err)
				}
				if payload.URL != "https://example.com/product" || payload.Engine != test.wantEngine || !json.Valid(payload.Schema) {
					t.Errorf("payload = %#v", payload)
				}
				if test.wantLLMCredential && (payload.LLMAPIKey != "caller-llm-secret" || payload.LLMModel != "caller-model" || payload.LLMBaseURL != "https://llm.example/v1") {
					t.Errorf("LLM settings changed: %#v", payload)
				}
				return httpResponse(http.StatusOK, responseBody), nil
			})}
			handler := handleExtractDataWithClient(client, "http://purify.test", "purify-api-secret")

			result, protocolErr := handler(context.Background(), extractRequest(test.arguments))
			if protocolErr != nil {
				t.Fatalf("protocol error: %v", protocolErr)
			}
			if result.IsError {
				t.Fatalf("unexpected tool error: %s", toolResultText(t, result))
			}
			if _, ok := result.StructuredContent.(models.ExtractResponse); !ok {
				t.Fatalf("structured content type = %T, want models.ExtractResponse", result.StructuredContent)
			}
			fallback := toolResultText(t, result)
			for _, secret := range []string{"purify-api-secret", "caller-llm-secret"} {
				if strings.Contains(fallback, secret) {
					t.Fatalf("fallback leaked %q: %s", secret, fallback)
				}
			}
		})
	}
}

func TestHandleExtractDataRejectsInvalidArgumentsBeforeHTTP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		arguments map[string]any
	}{
		{name: "missing URL", arguments: map[string]any{"schema": validExtractSchemaJSON}},
		{name: "empty URL", arguments: map[string]any{"url": "  ", "schema": validExtractSchemaJSON}},
		{name: "invalid URL", arguments: map[string]any{"url": "file:///private/page", "schema": validExtractSchemaJSON}},
		{name: "missing schema", arguments: map[string]any{"url": "https://example.com"}},
		{name: "malformed schema", arguments: extractArguments("{")},
		{name: "null schema", arguments: extractArguments("null")},
		{name: "trailing schema", arguments: extractArguments(`{"type":"object"} {}`)},
		{name: "unknown schema keyword", arguments: extractArguments(`{"type":"object","unexpected":true}`)},
		{name: "invalid engine", arguments: extractArgumentsWith("engine", "hybrid")},
		{name: "LLM without key", arguments: extractArgumentsWith("engine", "llm")},
		{name: "LLM with blank key", arguments: extractArgumentsWithMany(map[string]any{"engine": "llm", "llm_api_key": "  "})},
		{name: "invalid LLM base URL", arguments: extractArgumentsWith("llm_base_url", "https://user:pass@llm.example/v1?token=secret")},
		{name: "unknown argument", arguments: extractArgumentsWith("unexpected", true)},
		{name: "non-string optional", arguments: extractArgumentsWith("llm_model", 42)},
	}

	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return httpResponse(http.StatusInternalServerError, nil), nil
	})}
	handler := handleExtractDataWithClient(client, "http://purify.test", "purify-api-secret")
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			result, protocolErr := handler(context.Background(), extractRequest(test.arguments))
			if protocolErr != nil {
				t.Fatalf("protocol error: %v", protocolErr)
			}
			if !result.IsError {
				t.Fatalf("expected tool error, got %s", toolResultText(t, result))
			}
		})
	}

	oversized := extractArguments(strings.Repeat(" ", maxExtractSchemaJSONBytes+1))
	result, protocolErr := handler(context.Background(), extractRequest(oversized))
	if protocolErr != nil || !result.IsError || !strings.Contains(toolResultText(t, result), "exceeds") {
		t.Fatalf("oversized schema = (%#v, %v)", result, protocolErr)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("HTTP calls = %d, want 0", got)
	}
}

func TestHandleExtractDataInputLimits(t *testing.T) {
	tests := []struct {
		name      string
		atLimit   map[string]any
		overLimit map[string]any
	}{
		{
			name: "URL",
			atLimit: extractArgumentsWith("url", "https://example.com/"+
				strings.Repeat("p", maxVerifyURLBytes-len("https://example.com/"))),
			overLimit: extractArgumentsWith("url", "https://example.com/"+
				strings.Repeat("p", maxVerifyURLBytes-len("https://example.com/")+1)),
		},
		{
			name: "LLM API key",
			atLimit: extractArgumentsWithMany(map[string]any{
				"engine": "llm", "llm_api_key": strings.Repeat("k", maxExtractLLMCredentialBytes),
			}),
			overLimit: extractArgumentsWithMany(map[string]any{
				"engine": "llm", "llm_api_key": strings.Repeat("k", maxExtractLLMCredentialBytes+1),
			}),
		},
		{
			name:      "LLM model",
			atLimit:   extractArgumentsWith("llm_model", strings.Repeat("m", maxExtractLLMModelBytes)),
			overLimit: extractArgumentsWith("llm_model", strings.Repeat("m", maxExtractLLMModelBytes+1)),
		},
		{
			name: "LLM base URL",
			atLimit: extractArgumentsWith("llm_base_url", "https://llm.example/"+
				strings.Repeat("p", maxExtractLLMBaseURLBytes-len("https://llm.example/"))),
			overLimit: extractArgumentsWith("llm_base_url", "https://llm.example/"+
				strings.Repeat("p", maxExtractLLMBaseURLBytes-len("https://llm.example/")+1)),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			responseBody := validExtractResponseBody(t)
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return httpResponse(http.StatusOK, responseBody), nil
			})}
			handler := handleExtractDataWithClient(client, "http://purify.test", "purify-api-secret")

			result, protocolErr := handler(context.Background(), extractRequest(test.atLimit))
			if protocolErr != nil || result.IsError {
				t.Fatalf("N-byte input = (%#v, %v)", result, protocolErr)
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("N-byte HTTP calls = %d, want 1", got)
			}

			result, protocolErr = handler(context.Background(), extractRequest(test.overLimit))
			if protocolErr != nil || !result.IsError {
				t.Fatalf("N+1-byte input = (%#v, %v)", result, protocolErr)
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("N+1-byte input made HTTP call; total = %d, want 1", got)
			}
		})
	}
}

func TestDecodeExtractSchemaAcceptsExactLimitAndRejectsLimitPlusOne(t *testing.T) {
	t.Parallel()

	atLimit := `{}` + strings.Repeat(" ", maxExtractSchemaJSONBytes-2)
	if len(atLimit) != maxExtractSchemaJSONBytes {
		t.Fatal("test fixture is not at the schema limit")
	}
	if _, err := decodeExtractSchema(atLimit); err != nil {
		t.Fatalf("N-byte schema: %v", err)
	}
	if _, err := decodeExtractSchema(atLimit + " "); err == nil {
		t.Fatal("N+1-byte schema was accepted")
	}
}

func TestHandleExtractDataNon2xxUsesStableStructuredError(t *testing.T) {
	t.Parallel()

	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		body := []byte(`{"success":false,"error":{"code":"EXTRACTOR_UNAVAILABLE","message":"no active compiled extractor matches this page"}}`)
		return httpResponse(http.StatusConflict, body), nil
	})}
	handler := handleExtractDataWithClient(client, "http://purify.test", "purify-api-secret")

	result, protocolErr := handler(context.Background(), extractRequest(extractArgumentsWith("engine", "compiled")))
	if protocolErr != nil {
		t.Fatalf("protocol error: %v", protocolErr)
	}
	if !result.IsError {
		t.Fatal("non-2xx response was not a tool error")
	}
	if got, want := toolResultText(t, result), "[EXTRACTOR_UNAVAILABLE] no active compiled extractor matches this page"; got != want {
		t.Fatalf("tool error = %q, want %q", got, want)
	}
}

func TestExtractProductionClientRejectsCrossOriginRedirectWithoutLeakingAPIKey(t *testing.T) {
	t.Parallel()

	var sourceCalls atomic.Int32
	var redirectCalls atomic.Int32
	var leakedKey atomic.Bool
	client := newExtractHTTPClient()
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Host {
		case "purify.test":
			sourceCalls.Add(1)
			if got := request.Header.Get("X-API-Key"); got != "purify-api-secret" {
				t.Errorf("source X-API-Key = %q, want Purify API credential", got)
			}
			return &http.Response{
				StatusCode: http.StatusTemporaryRedirect,
				Header:     http.Header{"Location": []string{"https://redirect.test/capture"}},
				Body:       io.NopCloser(strings.NewReader("redirecting")),
				Request:    request,
			}, nil
		case "redirect.test":
			redirectCalls.Add(1)
			if request.Header.Get("X-API-Key") != "" {
				leakedKey.Store(true)
			}
			return httpResponse(http.StatusOK, validExtractResponseBody(t)), nil
		default:
			t.Fatalf("unexpected redirect host %q", request.URL.Host)
			return nil, nil
		}
	})
	handler := handleExtractDataWithClient(client, "http://purify.test", "purify-api-secret")

	result, protocolErr := handler(context.Background(), extractRequest(extractArgumentsWith("engine", "compiled")))
	if protocolErr != nil {
		t.Fatalf("protocol error: %v", protocolErr)
	}
	if !result.IsError {
		t.Fatalf("redirect returned success: %s", toolResultText(t, result))
	}
	if got, want := toolResultText(t, result), "extraction failed (HTTP 307)"; got != want {
		t.Fatalf("redirect tool error = %q, want %q", got, want)
	}
	if got := sourceCalls.Load(); got != 1 {
		t.Fatalf("source calls = %d, want 1", got)
	}
	if got := redirectCalls.Load(); got != 0 {
		t.Fatalf("redirect target calls = %d, want 0", got)
	}
	if leakedKey.Load() {
		t.Fatal("Purify API key leaked to redirect target")
	}
}

func TestHandleExtractDataRedactsKnownCredentialsFromErrors(t *testing.T) {
	responses := []struct {
		name   string
		status int
		body   string
	}{
		{name: "non-2xx detail", status: http.StatusBadGateway, body: `{"error":{"code":"LLM_FAILURE","message":"key api"}}`},
		{name: "2xx unknown field name", status: http.StatusOK, body: `{"success":true,"data":{},"key":true}`},
	}
	for _, response := range responses {
		t.Run(response.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return httpResponse(response.status, []byte(response.body)), nil
			})}
			handler := handleExtractDataWithClient(client, "http://purify.test", "api")
			arguments := extractArgumentsWithMany(map[string]any{"engine": "llm", "llm_api_key": "key"})

			result, protocolErr := handler(context.Background(), extractRequest(arguments))
			if protocolErr != nil || !result.IsError {
				t.Fatalf("result = %#v, protocol error = %v", result, protocolErr)
			}
			message := toolResultText(t, result)
			if strings.Contains(message, "key") || strings.Contains(message, "api") || !strings.Contains(message, "[REDACTED]") {
				t.Fatalf("tool error was not redacted: %q", message)
			}
		})
	}
}

func TestHandleExtractDataPreservesExtractorMetadata(t *testing.T) {
	t.Parallel()

	compiledAt := time.Date(2026, time.August, 9, 1, 2, 3, 0, time.UTC)
	want := models.ExtractResponse{
		Success: true,
		Data:    json.RawMessage(`{"name":"Purify"}`),
		Extractor: &models.ExtractorMetadata{
			ID:         "8c8a6de6-386a-40d7-a22a-abcf3ea2184e",
			Version:    3,
			CompiledAt: compiledAt,
			Validation: 0.97,
			Mode:       "compiled",
		},
		Metadata: models.Metadata{Title: "Product", SourceURL: "https://example.com/product"},
		Tokens:   models.TokenInfo{OriginalEstimate: 100, CleanedEstimate: 20, SavingsPercent: 80},
		Timing:   models.ExtractTimingInfo{TotalMs: 9, NavigationMs: 6, CleaningMs: 2, ExtractionMs: 1},
	}
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return httpResponse(http.StatusOK, body), nil
	})}
	handler := handleExtractDataWithClient(client, "http://purify.test", "purify-api-secret")

	result, protocolErr := handler(context.Background(), extractRequest(extractArgumentsWith("engine", "compiled")))
	if protocolErr != nil || result.IsError {
		t.Fatalf("result = %#v, protocol error = %v", result, protocolErr)
	}
	got, ok := result.StructuredContent.(models.ExtractResponse)
	if !ok {
		t.Fatalf("structured content type = %T", result.StructuredContent)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("structured content = %#v, want %#v", got, want)
	}
	pretty := toolResultText(t, result)
	if !strings.Contains(pretty, "\n  \"extractor\": {") || !strings.Contains(pretty, `"validation": 0.97`) || !strings.Contains(pretty, `"mode": "compiled"`) {
		t.Fatalf("fallback omitted extractor metadata: %s", pretty)
	}
	var fallback models.ExtractResponse
	if err := json.Unmarshal([]byte(pretty), &fallback); err != nil {
		t.Fatalf("fallback decode: %v", err)
	}
	if !fallback.Success || !reflect.DeepEqual(fallback.Extractor, want.Extractor) || fallback.Metadata != want.Metadata {
		t.Fatalf("fallback = %#v", fallback)
	}
}

func TestHandleExtractDataRejectsMalformedSuccessResponse(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "invalid JSON", body: `{"success":`},
		{name: "null", body: `null`},
		{name: "trailing value", body: `{"success":true,"data":{}} {}`},
		{name: "unknown top-level field", body: `{"success":true,"data":{},"unexpected":true}`},
		{name: "unknown nested field", body: `{"success":true,"data":{},"metadata":{"unexpected":true}}`},
		{name: "missing data", body: `{"success":true}`},
		{name: "null data", body: `{"success":true,"data":null,"metadata":{"title":"","source_url":""},"tokens":{"original_estimate":0,"cleaned_estimate":0,"savings_percent":0},"timing":{"total_ms":0,"navigation_ms":0,"cleaning_ms":0,"extraction_ms":0}}`},
		{name: "null metadata", body: `{"success":true,"data":{},"metadata":null,"tokens":{"original_estimate":0,"cleaned_estimate":0,"savings_percent":0},"timing":{"total_ms":0,"navigation_ms":0,"cleaning_ms":0,"extraction_ms":0}}`},
		{name: "null tokens", body: `{"success":true,"data":{},"metadata":{"title":"","source_url":""},"tokens":null,"timing":{"total_ms":0,"navigation_ms":0,"cleaning_ms":0,"extraction_ms":0}}`},
		{name: "null timing", body: `{"success":true,"data":{},"metadata":{"title":"","source_url":""},"tokens":{"original_estimate":0,"cleaned_estimate":0,"savings_percent":0},"timing":null}`},
		{name: "success with error", body: `{"success":true,"data":{},"error":{"code":"BAD","message":"bad"}}`},
		{name: "invalid extractor metadata", body: `{"success":true,"data":{},"extractor":{"id":"","version":0,"compiled_at":"0001-01-01T00:00:00Z","validation":2,"mode":"llm"}}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return httpResponse(http.StatusOK, []byte(test.body)), nil
			})}
			handler := handleExtractDataWithClient(client, "http://purify.test", "purify-api-secret")

			result, protocolErr := handler(context.Background(), extractRequest(extractArgumentsWith("engine", "compiled")))
			if protocolErr != nil {
				t.Fatalf("protocol error: %v", protocolErr)
			}
			if !result.IsError || !strings.Contains(toolResultText(t, result), "failed to parse extract response") {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestHandleExtractDataRejectsOversizedResponse(t *testing.T) {
	t.Parallel()

	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		body := io.NopCloser(io.LimitReader(zeroReader{}, maxAPIResponseBytes+1))
		return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header)}, nil
	})}
	handler := handleExtractDataWithClient(client, "http://purify.test", "purify-api-secret")

	result, protocolErr := handler(context.Background(), extractRequest(extractArgumentsWith("engine", "compiled")))
	if protocolErr != nil || !result.IsError || !strings.Contains(toolResultText(t, result), "exceeds") {
		t.Fatalf("result = %#v, protocol error = %v", result, protocolErr)
	}
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

func extractRequest(arguments map[string]any) mcp.CallToolRequest {
	return mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: arguments}}
}

func extractArguments(schema string) map[string]any {
	return map[string]any{
		"url":    "https://example.com/product",
		"schema": schema,
	}
}

func extractArgumentsWith(name string, value any) map[string]any {
	return extractArgumentsWithMany(map[string]any{name: value})
}

func extractArgumentsWithMany(values map[string]any) map[string]any {
	arguments := extractArguments(validExtractSchemaJSON)
	for name, value := range values {
		arguments[name] = value
	}
	return arguments
}

func validExtractResponseBody(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(models.ExtractResponse{
		Success:  true,
		Data:     json.RawMessage(`{"name":"Purify"}`),
		Metadata: models.Metadata{Title: "Product", SourceURL: "https://example.com/product"},
		Tokens:   models.TokenInfo{OriginalEstimate: 100, CleanedEstimate: 20, SavingsPercent: 80},
		Timing:   models.ExtractTimingInfo{TotalMs: 9, NavigationMs: 6, CleaningMs: 2, ExtractionMs: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
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
