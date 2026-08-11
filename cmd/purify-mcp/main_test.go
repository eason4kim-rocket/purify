package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/use-agent/purify/evidence"
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
	if !reflect.DeepEqual(tool.InputSchema.Required, []string{"schema"}) {
		t.Fatalf("required = %#v, want schema", tool.InputSchema.Required)
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
	sources, ok := tool.InputSchema.Properties["sources"].(map[string]any)
	if !ok {
		t.Fatalf("sources property has type %T", tool.InputSchema.Properties["sources"])
	}
	if sources["type"] != "array" || sources["minItems"] != 1 || sources["maxItems"] != models.MaxExtractSources {
		t.Fatalf("sources schema = %#v", sources)
	}
	items, ok := sources["items"].(map[string]any)
	if !ok || items["type"] != "string" || items["maxLength"] != models.MaxExtractSourceURLBytes {
		t.Fatalf("sources items schema = %#v", sources["items"])
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

func TestSearchWebToolContract(t *testing.T) {
	t.Parallel()

	tool := newSearchWebTool()
	if tool.Name != "search_web" {
		t.Fatalf("tool name = %q, want search_web", tool.Name)
	}
	if !reflect.DeepEqual(tool.InputSchema.Required, []string{"query"}) {
		t.Fatalf("required = %#v, want query", tool.InputSchema.Required)
	}
	if got, ok := tool.InputSchema.AdditionalProperties.(bool); !ok || got {
		t.Fatalf("additionalProperties = %#v, want false", tool.InputSchema.AdditionalProperties)
	}
	wantTypes := map[string]string{
		"query": "string", "limit": "integer", "domains": "array", "freshness": "string",
		"include_content": "boolean", "verify": "boolean", "deduplicate": "boolean", "schema": "string",
		"engine": "string", "llm_api_key": "string", "llm_model": "string", "llm_base_url": "string",
		"timeout": "integer",
	}
	for name, want := range wantTypes {
		property, ok := tool.InputSchema.Properties[name].(map[string]any)
		if !ok || property["type"] != want {
			t.Fatalf("property %q = %#v, want type %q", name, tool.InputSchema.Properties[name], want)
		}
	}
	if got := tool.InputSchema.Properties["limit"].(map[string]any)["default"]; got != float64(models.DefaultSearchLimit) {
		t.Fatalf("limit default = %#v", got)
	}
	if got := tool.InputSchema.Properties["timeout"].(map[string]any)["default"]; got != float64(models.DefaultSearchTimeoutSeconds) {
		t.Fatalf("timeout default = %#v", got)
	}
	if got := tool.InputSchema.Properties["deduplicate"].(map[string]any)["default"]; got != true {
		t.Fatalf("deduplicate default = %#v", got)
	}
	if got := tool.InputSchema.Properties["freshness"].(map[string]any)["enum"]; !reflect.DeepEqual(got, []string{"day", "1d", "week", "7d", "month", "year"}) {
		t.Fatalf("freshness enum = %#v", got)
	}
	if got := tool.InputSchema.Properties["schema"].(map[string]any)["maxLength"]; got != models.MaxSearchSchemaBytes {
		t.Fatalf("schema maxLength = %#v", got)
	}
	assertHint(t, "readOnlyHint", tool.Annotations.ReadOnlyHint, true)
	assertHint(t, "destructiveHint", tool.Annotations.DestructiveHint, false)
	assertHint(t, "idempotentHint", tool.Annotations.IdempotentHint, false)
	assertHint(t, "openWorldHint", tool.Annotations.OpenWorldHint, true)
}

func TestAnswerFactToolContract(t *testing.T) {
	t.Parallel()

	tool := newAnswerFactTool()
	if tool.Name != "answer_fact" {
		t.Fatalf("tool name = %q, want answer_fact", tool.Name)
	}
	if !reflect.DeepEqual(tool.InputSchema.Required, []string{"subject", "predicate"}) {
		t.Fatalf("required = %#v, want subject and predicate", tool.InputSchema.Required)
	}
	if got, ok := tool.InputSchema.AdditionalProperties.(bool); !ok || got {
		t.Fatalf("additionalProperties = %#v, want false", tool.InputSchema.AdditionalProperties)
	}
	wantTypes := map[string]string{
		"subject": "string", "predicate": "string", "freshness": "string",
		"min_independent_sources": "integer", "on_conflict": "string", "timeout": "integer",
	}
	for name, want := range wantTypes {
		property, ok := tool.InputSchema.Properties[name].(map[string]any)
		if !ok || property["type"] != want {
			t.Fatalf("property %q = %#v, want type %q", name, tool.InputSchema.Properties[name], want)
		}
	}
	if got := tool.InputSchema.Properties["freshness"].(map[string]any)["default"]; got != models.DefaultAnswerFreshness {
		t.Fatalf("freshness default = %#v", got)
	}
	if got := tool.InputSchema.Properties["freshness"].(map[string]any)["enum"]; !reflect.DeepEqual(got, []string{"day", "1d", "week", "7d", "month", "year"}) {
		t.Fatalf("freshness enum = %#v", got)
	}
	if got := tool.InputSchema.Properties["min_independent_sources"].(map[string]any)["default"]; got != float64(models.DefaultAnswerMinIndependentSources) {
		t.Fatalf("minimum default = %#v", got)
	}
	if got := tool.InputSchema.Properties["on_conflict"].(map[string]any)["default"]; got != string(models.FactConflictExpose) {
		t.Fatalf("on_conflict default = %#v", got)
	}
	if got := tool.InputSchema.Properties["timeout"].(map[string]any)["default"]; got != float64(models.DefaultAnswerTimeoutSeconds) {
		t.Fatalf("timeout default = %#v", got)
	}
	if got := tool.InputSchema.Properties["subject"].(map[string]any)["maxLength"]; got != models.MaxAnswerSubjectRunes {
		t.Fatalf("subject maxLength = %#v", got)
	}
	if got := tool.InputSchema.Properties["predicate"].(map[string]any)["maxLength"]; got != models.MaxAnswerPredicateBytes {
		t.Fatalf("predicate maxLength = %#v", got)
	}
	assertHint(t, "readOnlyHint", tool.Annotations.ReadOnlyHint, true)
	assertHint(t, "destructiveHint", tool.Annotations.DestructiveHint, false)
	assertHint(t, "idempotentHint", tool.Annotations.IdempotentHint, false)
	assertHint(t, "openWorldHint", tool.Annotations.OpenWorldHint, true)
}

func TestHandleAnswerFactNestedPayloadAndStructuredResult(t *testing.T) {
	t.Parallel()

	response := validKnownAnswerResponse()
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.String() != "http://purify.test/api/v1/answer" {
			t.Errorf("request = %s %s", request.Method, request.URL)
		}
		if got := request.Header.Get("X-API-Key"); got != "purify-api-secret" {
			t.Errorf("X-API-Key = %q", got)
		}
		requestBody, readErr := io.ReadAll(request.Body)
		if readErr != nil {
			t.Errorf("read request: %v", readErr)
		}
		if bytes.Contains(requestBody, []byte("purify-api-secret")) {
			t.Errorf("process credential leaked into body: %s", requestBody)
		}
		var document map[string]json.RawMessage
		if err := json.Unmarshal(requestBody, &document); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if len(document) != 2 || document["spec"] == nil || document["timeout"] == nil {
			t.Errorf("top-level request = %s", requestBody)
		}
		for _, forbidden := range []string{"schema", "engine", "llm_api_key", "llm_model", "llm_base_url"} {
			if _, present := document[forbidden]; present {
				t.Errorf("request contains forbidden field %q", forbidden)
			}
		}
		var payload answerAPIPayload
		decoder := json.NewDecoder(bytes.NewReader(requestBody))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&payload); err != nil {
			t.Errorf("decode payload: %v", err)
		}
		if payload.Spec.Subject != "anthropic claude" || payload.Spec.Predicate != "price_per_mtok_input" ||
			payload.Spec.Freshness != models.DefaultAnswerFreshness ||
			payload.Spec.MinIndependentSources != models.DefaultAnswerMinIndependentSources ||
			payload.Spec.OnConflict != models.FactConflictExpose || payload.Timeout != models.DefaultAnswerTimeoutSeconds {
			t.Errorf("payload = %#v", payload)
		}
		return httpResponse(http.StatusOK, body), nil
	})}

	result, protocolErr := handleAnswerFactWithClient(client, "http://purify.test", "purify-api-secret")(
		context.Background(), answerRequest(map[string]any{
			"subject": "  anthropic\u00a0 claude  ", "predicate": "price_per_mtok_input",
		}),
	)
	if protocolErr != nil || result == nil || result.IsError {
		t.Fatalf("result = %#v, protocol error = %v", result, protocolErr)
	}
	got, ok := result.StructuredContent.(models.AnswerResponse)
	if !ok || !reflect.DeepEqual(got, response) {
		t.Fatalf("structured response = %#v, want %#v", result.StructuredContent, response)
	}
	fallback := toolResultText(t, result)
	var decodedFallback models.AnswerResponse
	if err := json.Unmarshal([]byte(fallback), &decodedFallback); err != nil || !reflect.DeepEqual(decodedFallback, response) {
		t.Fatalf("fallback = %q, decoded=%#v, err=%v", fallback, decodedFallback, err)
	}
}

func TestAnswerPayloadStrictRuntimeValidation(t *testing.T) {
	t.Parallel()

	invalidUTF8 := string([]byte{0xff})
	threeHundredRunes := strings.Repeat("s", models.MaxAnswerSubjectRunes)
	tests := []struct {
		name string
		args map[string]any
	}{
		{name: "missing subject", args: map[string]any{"predicate": "price"}},
		{name: "missing predicate", args: map[string]any{"subject": "model"}},
		{name: "unknown", args: map[string]any{"subject": "model", "predicate": "price", "schema": `{}`}},
		{name: "subject type", args: map[string]any{"subject": true, "predicate": "price"}},
		{name: "subject empty", args: map[string]any{"subject": "   ", "predicate": "price"}},
		{name: "subject bytes", args: map[string]any{"subject": strings.Repeat("s", models.MaxAnswerSubjectBytes+1), "predicate": "price"}},
		{name: "subject UTF-8", args: map[string]any{"subject": invalidUTF8, "predicate": "price"}},
		{name: "subject control", args: map[string]any{"subject": "bad\tmodel", "predicate": "price"}},
		{name: "subject runes", args: map[string]any{"subject": strings.Repeat("界", models.MaxAnswerSubjectRunes+1), "predicate": "price"}},
		{name: "subject words", args: map[string]any{"subject": strings.Repeat("w ", models.MaxAnswerSubjectWords) + "w", "predicate": "price"}},
		{name: "predicate empty", args: map[string]any{"subject": "model", "predicate": ""}},
		{name: "predicate bytes", args: map[string]any{"subject": "model", "predicate": strings.Repeat("p", models.MaxAnswerPredicateBytes+1)}},
		{name: "predicate whitespace", args: map[string]any{"subject": "model", "predicate": "unit price"}},
		{name: "combined query", args: map[string]any{"subject": threeHundredRunes, "predicate": strings.Repeat("p", 101)}},
		{name: "freshness", args: map[string]any{"subject": "model", "predicate": "price", "freshness": "hour"}},
		{name: "minimum type", args: map[string]any{"subject": "model", "predicate": "price", "min_independent_sources": "2"}},
		{name: "minimum fraction", args: map[string]any{"subject": "model", "predicate": "price", "min_independent_sources": 2.5}},
		{name: "minimum NaN", args: map[string]any{"subject": "model", "predicate": "price", "min_independent_sources": math.NaN()}},
		{name: "minimum infinity", args: map[string]any{"subject": "model", "predicate": "price", "min_independent_sources": math.Inf(1)}},
		{name: "minimum low", args: map[string]any{"subject": "model", "predicate": "price", "min_independent_sources": 0}},
		{name: "minimum high", args: map[string]any{"subject": "model", "predicate": "price", "min_independent_sources": 9}},
		{name: "conflict", args: map[string]any{"subject": "model", "predicate": "price", "on_conflict": "choose"}},
		{name: "timeout fraction", args: map[string]any{"subject": "model", "predicate": "price", "timeout": 1.5}},
		{name: "timeout high", args: map[string]any{"subject": "model", "predicate": "price", "timeout": 121}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := answerPayload(test.args); err == nil {
				t.Fatalf("answerPayload accepted %#v", test.args)
			}
		})
	}

	atLimit, err := answerPayload(map[string]any{
		"subject": strings.Repeat("界", models.MaxAnswerSubjectRunes), "predicate": strings.Repeat("p", 99),
		"min_independent_sources": float64(models.MaxAnswerMinIndependentSources), "timeout": float64(models.MaxAnswerTimeoutSeconds),
	})
	if err != nil || atLimit.Spec.MinIndependentSources != models.MaxAnswerMinIndependentSources || atLimit.Timeout != models.MaxAnswerTimeoutSeconds {
		t.Fatalf("at-limit payload = %#v, err=%v", atLimit, err)
	}
}

func TestDecodeAnswerFactResponseAcceptsKnownAndEveryUnknownReason(t *testing.T) {
	t.Parallel()

	responses := []models.AnswerResponse{
		validKnownAnswerResponse(),
		validUnknownAnswerResponse(models.AnswerUnknownNoSearchResults),
		validUnknownAnswerResponse(models.AnswerUnknownNoValidSources),
		validUnknownAnswerResponse(models.AnswerUnknownMissingValue),
		validUnknownAnswerResponse(models.AnswerUnknownInsufficient),
		validUnknownAnswerResponse(models.AnswerUnknownConflict),
	}
	for _, response := range responses {
		response := response
		t.Run(string(response.Status)+"/"+string(response.Reason), func(t *testing.T) {
			body, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			got, err := decodeAnswerFactResponse(body, models.DefaultAnswerMinIndependentSources)
			if err != nil || !reflect.DeepEqual(got, response) {
				t.Fatalf("decode = %#v, err=%v, want %#v", got, err, response)
			}
		})
	}
}

func TestDecodeAnswerFactResponseAcceptsFoldReasonStates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    []byte
		minimum int
	}{
		{
			name: "belief single reason",
			body: mutateAnswerResponseBody(t, validKnownAnswerResponse(), func(document map[string]any) {
				belief := document["belief"].(map[string]any)
				belief["confidence"] = "low"
				agreement := belief["agreement"].(map[string]any)
				agreement["independent_roots"] = float64(1)
				agreement["fold_reason"] = "near_duplicate"
			}),
			minimum: 1,
		},
		{
			name: "belief mixed reasons omit aggregate reason",
			body: mutateAnswerResponseBody(t, validKnownAnswerResponse(), func(document map[string]any) {
				belief := document["belief"].(map[string]any)
				belief["confidence"] = "low"
				belief["agreement"].(map[string]any)["independent_roots"] = float64(1)
			}),
			minimum: 1,
		},
		{
			name: "candidate single reason",
			body: mutateAnswerResponseBody(t, validKnownAnswerResponse(), func(document map[string]any) {
				document["conflicts"] = []any{map[string]any{
					"value": "$20",
					"agreement": map[string]any{
						"pages":             float64(2),
						"independent_roots": float64(1),
						"fold_reason":       "quote_lineage",
					},
				}}
			}),
			minimum: 2,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := decodeAnswerFactResponse(test.body, test.minimum); err != nil {
				t.Fatalf("decoder rejected valid fold-reason state: %v\n%s", err, test.body)
			}
		})
	}
}

func TestDecodeAnswerFactResponseRejectsInvalidFoldReasons(t *testing.T) {
	t.Parallel()

	foldedBelief := func(reason any) []byte {
		return mutateAnswerResponseBody(t, validKnownAnswerResponse(), func(document map[string]any) {
			belief := document["belief"].(map[string]any)
			belief["confidence"] = "low"
			agreement := belief["agreement"].(map[string]any)
			agreement["independent_roots"] = float64(1)
			agreement["fold_reason"] = reason
		})
	}
	tests := []struct {
		name string
		body []byte
	}{
		{name: "null", body: foldedBelief(nil)},
		{name: "empty", body: foldedBelief("")},
		{name: "unknown", body: foldedBelief("other")},
		{name: "reason without an actual fold", body: mutateAnswerResponseBody(t, validKnownAnswerResponse(), func(document map[string]any) {
			document["belief"].(map[string]any)["agreement"].(map[string]any)["fold_reason"] = "same_root"
		})},
		{name: "case-smuggled field", body: mutateAnswerResponseBody(t, validKnownAnswerResponse(), func(document map[string]any) {
			belief := document["belief"].(map[string]any)
			belief["confidence"] = "low"
			agreement := belief["agreement"].(map[string]any)
			agreement["independent_roots"] = float64(1)
			agreement["Fold_Reason"] = "same_root"
		})},
		{name: "candidate unknown", body: mutateAnswerResponseBody(t, validKnownAnswerResponse(), func(document map[string]any) {
			document["conflicts"] = []any{map[string]any{
				"value": "$20",
				"agreement": map[string]any{
					"pages":             float64(2),
					"independent_roots": float64(1),
					"fold_reason":       "other",
				},
			}}
		})},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := decodeAnswerFactResponse(test.body, 1); err == nil {
				t.Fatalf("decoder accepted invalid fold reason: %s", test.body)
			}
		})
	}
}

func TestDecodeAnswerFactResponseRejectsMalformedOrInconsistentDocuments(t *testing.T) {
	base := validKnownAnswerResponse()
	baseBody, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		body    []byte
		minimum int
	}{
		{name: "null", body: []byte(`null`), minimum: 2},
		{name: "trailing value", body: append(append([]byte(nil), baseBody...), []byte(` {}`)...), minimum: 2},
		{name: "duplicate field", body: bytes.Replace(baseBody, []byte(`{"status":"known"`), []byte(`{"status":"known","status":"known"`), 1), minimum: 2},
		{name: "escaped duplicate field", body: bytes.Replace(baseBody, []byte(`{"status":"known"`), []byte(`{"status":"known","\u0073tatus":"known"`), 1), minimum: 2},
		{name: "case-smuggled top field", body: mutateAnswerResponseBody(t, base, func(document map[string]any) { document["Status"] = "known" }), minimum: 2},
		{name: "case-smuggled nested field", body: mutateAnswerResponseBody(t, base, func(document map[string]any) {
			document["belief"].(map[string]any)["agreement"].(map[string]any)["Pages"] = float64(2)
		}), minimum: 2},
		{name: "200 error envelope", body: []byte(`{"error":{"code":"BAD","message":"bad"}}`), minimum: 2},
		{name: "request minimum not met", body: baseBody, minimum: 3},
		{name: "overstated distinct roots", body: mutateAnswerResponseBody(t, base, func(document map[string]any) {
			evidence := document["belief"].(map[string]any)["evidence"].([]any)
			evidence[1].(map[string]any)["url"] = "https://b.example.com/price"
			evidence[1].(map[string]any)["root"] = "example.com"
		}), minimum: 2},
		{name: "lease is not exact 24h", body: mutateAnswerResponseBody(t, base, func(document map[string]any) {
			document["lease"].(map[string]any)["expires_at"] = "2026-08-11T01:02:04Z"
		}), minimum: 2},
		{name: "known alternative ties winner", body: mutateAnswerResponseBody(t, base, func(document map[string]any) {
			document["conflicts"] = []any{map[string]any{"value": "$20", "agreement": map[string]any{"pages": float64(2), "independent_roots": float64(2)}}}
		}), minimum: 2},
		{name: "known page budget", body: mutateAnswerResponseBody(t, base, func(document map[string]any) {
			document["conflicts"] = []any{map[string]any{"value": "$20", "agreement": map[string]any{"pages": float64(7), "independent_roots": float64(1)}}}
		}), minimum: 2},
		{name: "non-string belief", body: mutateAnswerResponseBody(t, base, func(document map[string]any) {
			document["belief"].(map[string]any)["value"] = float64(19)
		}), minimum: 2},
		{name: "receipt key mismatch", body: mutateAnswerResponseBody(t, base, func(document map[string]any) {
			receipts := document["belief"].(map[string]any)["receipts"].(map[string]any)
			delete(receipts, "https://b.example.org/price")
			receipts["https://unmatched.example.net/"] = "receipt"
		}), minimum: 2},
		{name: "unknown conflict repeats closest", body: mutateAnswerResponseBody(t, validUnknownAnswerResponse(models.AnswerUnknownConflict), func(document map[string]any) {
			document["conflicts"].([]any)[0].(map[string]any)["value"] = "$19"
		}), minimum: 2},
		{name: "unknown wrong need", body: mutateAnswerResponseBody(t, validUnknownAnswerResponse(models.AnswerUnknownInsufficient), func(document map[string]any) {
			document["needs"].(map[string]any)["more_independent_sources"] = float64(2)
		}), minimum: 2},
		{name: "empty optional conflicts", body: mutateAnswerResponseBody(t, base, func(document map[string]any) { document["conflicts"] = []any{} }), minimum: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeAnswerFactResponse(test.body, test.minimum); err == nil {
				t.Fatalf("decoder accepted malformed body: %s", test.body)
			}
		})
	}
}

func TestHandleAnswerFactErrorsSecretScanAndResponseLimit(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        []byte
		wantContain string
		wantAbsent  string
	}{
		{name: "structured error", status: http.StatusTooManyRequests, body: []byte(`{"error":{"code":"RATE_LIMITED","message":"slow down"}}`), wantContain: "[RATE_LIMITED] slow down"},
		{name: "malformed error", status: http.StatusBadGateway, body: []byte(`{"error":{"code":"BAD"}}`), wantContain: "answer failed (HTTP 502)"},
		{name: "escaped secret success", status: http.StatusOK, body: bytes.Replace(mustJSON(t, validKnownAnswerResponse()), []byte(`"$19"`), []byte(`"purify-api-\u0073ecret"`), 1), wantContain: "sensitive data", wantAbsent: "purify-api-secret"},
		{name: "escaped secret error", status: http.StatusBadRequest, body: []byte(`{"error":{"code":"BAD","message":"purify-api-\u0073ecret"}}`), wantContain: "sensitive data", wantAbsent: "purify-api-secret"},
		{name: "exact response limit", status: http.StatusOK, body: answerResponseBodyAtSize(t, int(maxAPIResponseBytes)), wantContain: `"status":"unknown"`},
		{name: "response limit plus one", status: http.StatusOK, body: answerResponseBodyAtSize(t, int(maxAPIResponseBytes)+1), wantContain: "exceeds"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return httpResponse(test.status, test.body), nil
			})}
			result, protocolErr := handleAnswerFactWithClient(client, "http://purify.test", "purify-api-secret")(
				context.Background(), answerRequest(map[string]any{"subject": "model", "predicate": "price"}),
			)
			if protocolErr != nil || result == nil {
				t.Fatalf("result = %#v, protocol error = %v", result, protocolErr)
			}
			text := toolResultText(t, result)
			if !strings.Contains(text, test.wantContain) {
				t.Fatalf("text = %q, want %q", text, test.wantContain)
			}
			if test.wantAbsent != "" && strings.Contains(text, test.wantAbsent) {
				t.Fatalf("text leaks %q: %q", test.wantAbsent, text)
			}
			if test.name == "exact response limit" && result.IsError {
				t.Fatalf("exact limit rejected: %s", text)
			}
			if test.name != "exact response limit" && test.status == http.StatusOK && !strings.Contains(test.name, "secret") && !result.IsError {
				t.Fatalf("malformed/oversized success was accepted: %s", text)
			}
		})
	}
}

func TestHandleAnswerFactContextRedirectAndRequestMinimumAreFrozen(t *testing.T) {
	t.Run("client timeout and redirects", func(t *testing.T) {
		client := newAnswerHTTPClient()
		if client.Timeout != 120*time.Second {
			t.Fatalf("timeout = %s", client.Timeout)
		}
		if err := client.CheckRedirect(&http.Request{}, nil); err != http.ErrUseLastResponse {
			t.Fatalf("redirect error = %v", err)
		}
	})

	t.Run("cancelled before HTTP", func(t *testing.T) {
		var calls atomic.Int32
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return httpResponse(http.StatusOK, mustJSON(t, validKnownAnswerResponse())), nil
		})}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result, protocolErr := handleAnswerFactWithClient(client, "http://purify.test", "purify-api-secret")(
			ctx, answerRequest(map[string]any{"subject": "model", "predicate": "price"}),
		)
		if protocolErr != nil || !result.IsError || calls.Load() != 0 || !strings.Contains(toolResultText(t, result), "canceled") {
			t.Fatalf("result=%#v, protocolErr=%v, calls=%d", result, protocolErr, calls.Load())
		}
	})

	t.Run("request minimum copied before transport", func(t *testing.T) {
		arguments := map[string]any{"subject": "model", "predicate": "price", "min_independent_sources": 3}
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			arguments["min_independent_sources"] = 1
			return httpResponse(http.StatusOK, mustJSON(t, validKnownAnswerResponse())), nil
		})}
		result, protocolErr := handleAnswerFactWithClient(client, "http://purify.test", "purify-api-secret")(
			context.Background(), answerRequest(arguments),
		)
		if protocolErr != nil || !result.IsError || !strings.Contains(toolResultText(t, result), "known answer belief is invalid") {
			t.Fatalf("result=%#v, protocolErr=%v", result, protocolErr)
		}
	})

	t.Run("encoding slot wait observes cancellation", func(t *testing.T) {
		for range cap(answerEncodingSlots) {
			answerEncodingSlots <- struct{}{}
		}
		defer func() {
			for range cap(answerEncodingSlots) {
				<-answerEncodingSlots
			}
		}()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := encodeAnswerToolResponse(ctx, validUnknownAnswerResponse(models.AnswerUnknownNoSearchResults)); err == nil {
			t.Fatal("encoding accepted cancelled context")
		}
	})
}

func TestHandleSearchWebRequestShapeAndCredentialSeparation(t *testing.T) {
	tests := []struct {
		name          string
		arguments     map[string]any
		wantFields    []string
		absentFields  []string
		wantLLMAPIKey string
	}{
		{
			name: "baseline omits all extraction settings",
			arguments: map[string]any{
				"query": "purify search", "limit": float64(7), "domains": []any{"example.com"},
				"freshness": "7d", "include_content": true, "verify": true, "deduplicate": false, "timeout": 44,
			},
			wantFields:   []string{"query", "limit", "domains", "freshness", "include_content", "verify", "deduplicate", "timeout"},
			absentFields: []string{"schema", "engine", "llm_api_key", "llm_model", "llm_base_url"},
		},
		{
			name: "compiled physically strips LLM settings",
			arguments: map[string]any{
				"query": "purify search", "schema": validExtractSchemaJSON, "engine": "compiled",
				"llm_api_key": "caller-llm-secret", "llm_model": "ignored-model", "llm_base_url": "https://ignored.example/v1",
			},
			wantFields:   []string{"query", "schema", "engine", "deduplicate", "limit", "timeout"},
			absentFields: []string{"llm_api_key", "llm_model", "llm_base_url"},
		},
		{
			name: "auto without key physically strips fallback options",
			arguments: map[string]any{
				"query": "purify search", "schema": validExtractSchemaJSON,
				"llm_model": "ignored-model", "llm_base_url": "https://ignored.example/v1",
			},
			wantFields:   []string{"query", "schema", "engine", "deduplicate", "limit", "timeout"},
			absentFields: []string{"llm_api_key", "llm_model", "llm_base_url"},
		},
		{
			name: "llm keeps caller credential separate from API auth",
			arguments: map[string]any{
				"query": "purify search", "schema": validExtractSchemaJSON, "engine": "llm",
				"llm_api_key": "caller-llm-secret", "llm_model": "caller-model", "llm_base_url": "https://llm.example/v1",
			},
			wantFields:    []string{"query", "schema", "engine", "llm_api_key", "llm_model", "llm_base_url"},
			wantLLMAPIKey: "caller-llm-secret",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := validSearchResponseBody(t)
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.Method != http.MethodPost || request.URL.String() != "http://purify.test/api/v1/search" {
					t.Errorf("request = %s %s", request.Method, request.URL)
				}
				if got := request.Header.Get("X-API-Key"); got != "purify-api-secret" {
					t.Errorf("X-API-Key = %q", got)
				}
				rawBody, err := io.ReadAll(request.Body)
				if err != nil {
					t.Errorf("read body: %v", err)
				}
				if bytes.Contains(rawBody, []byte("purify-api-secret")) {
					t.Errorf("Purify API credential leaked into body: %s", rawBody)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(rawBody, &fields); err != nil {
					t.Errorf("decode request: %v", err)
				}
				for _, name := range test.wantFields {
					if _, present := fields[name]; !present {
						t.Errorf("request omitted %q: %s", name, rawBody)
					}
				}
				for _, name := range test.absentFields {
					if _, present := fields[name]; present {
						t.Errorf("request physically contains %q: %s", name, rawBody)
					}
				}
				var payload models.SearchRequest
				decoder := json.NewDecoder(bytes.NewReader(rawBody))
				decoder.DisallowUnknownFields()
				if err := decoder.Decode(&payload); err != nil {
					t.Errorf("decode typed request: %v", err)
				}
				if payload.Query != "purify search" || payload.LLMAPIKey != test.wantLLMAPIKey {
					t.Errorf("payload = %#v", payload)
				}
				return httpResponse(http.StatusOK, body), nil
			})}
			handler := handleSearchWebWithClient(client, "http://purify.test", "purify-api-secret")

			result, protocolErr := handler(context.Background(), searchRequest(test.arguments))
			if protocolErr != nil || result.IsError {
				t.Fatalf("result = %#v, protocol error = %v, text = %s", result, protocolErr, toolResultText(t, result))
			}
			got, ok := result.StructuredContent.(models.SearchResponse)
			if !ok || !reflect.DeepEqual(got, validSearchResponse()) {
				t.Fatalf("structured response = %#v", result.StructuredContent)
			}
			var fallback models.SearchResponse
			if err := json.Unmarshal([]byte(toolResultText(t, result)), &fallback); err != nil || !reflect.DeepEqual(fallback, got) {
				t.Fatalf("fallback parity = %#v, error = %v", fallback, err)
			}
		})
	}
}

func TestHandleSearchWebRejectsInvalidArgumentsBeforeHTTP(t *testing.T) {
	tests := []struct {
		name      string
		arguments map[string]any
	}{
		{name: "missing query", arguments: map[string]any{}},
		{name: "non-string query", arguments: map[string]any{"query": 42}},
		{name: "blank query", arguments: map[string]any{"query": " \t "}},
		{name: "too many query words", arguments: map[string]any{"query": strings.Repeat("word ", models.MaxSearchQueryWords+1)}},
		{name: "fractional limit", arguments: map[string]any{"query": "q", "limit": 1.5}},
		{name: "limit too high", arguments: map[string]any{"query": "q", "limit": models.MaxSearchLimit + 1}},
		{name: "fractional timeout", arguments: map[string]any{"query": "q", "timeout": 2.5}},
		{name: "non-array domains", arguments: map[string]any{"query": "q", "domains": "example.com"}},
		{name: "non-string domain", arguments: map[string]any{"query": "q", "domains": []any{42}}},
		{name: "domain with URL", arguments: map[string]any{"query": "q", "domains": []any{"https://example.com"}}},
		{name: "localhost domain", arguments: map[string]any{"query": "q", "domains": []any{"localhost"}}},
		{name: "IP domain", arguments: map[string]any{"query": "q", "domains": []any{"127.0.0.1"}}},
		{name: "unknown freshness", arguments: map[string]any{"query": "q", "freshness": "pw"}},
		{name: "coerced boolean", arguments: map[string]any{"query": "q", "verify": "true"}},
		{name: "engine without schema", arguments: map[string]any{"query": "q", "engine": "compiled"}},
		{name: "key without schema", arguments: map[string]any{"query": "q", "llm_api_key": "key"}},
		{name: "non-string schema", arguments: map[string]any{"query": "q", "schema": map[string]any{"type": "object"}}},
		{name: "invalid schema", arguments: map[string]any{"query": "q", "schema": "null"}},
		{name: "trailing schema", arguments: map[string]any{"query": "q", "schema": `{} {}`}},
		{name: "duplicate schema field", arguments: map[string]any{"query": "q", "schema": `{"type":"object","type":"array"}`}},
		{name: "invalid engine", arguments: map[string]any{"query": "q", "schema": `{}`, "engine": "hybrid"}},
		{name: "llm without key", arguments: map[string]any{"query": "q", "schema": `{}`, "engine": "llm"}},
		{name: "llm blank key", arguments: map[string]any{"query": "q", "schema": `{}`, "engine": "llm", "llm_api_key": "  "}},
		{name: "model with whitespace", arguments: map[string]any{"query": "q", "schema": `{}`, "llm_model": "bad model"}},
		{name: "base URL with credential", arguments: map[string]any{"query": "q", "schema": `{}`, "llm_base_url": "https://user:pass@llm.example/v1"}},
		{name: "unknown argument", arguments: map[string]any{"query": "q", "provider": "brave"}},
	}

	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return httpResponse(http.StatusOK, validSearchResponseBody(t)), nil
	})}
	handler := handleSearchWebWithClient(client, "http://purify.test", "purify-api-secret")
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, protocolErr := handler(context.Background(), searchRequest(test.arguments))
			if protocolErr != nil || !result.IsError {
				t.Fatalf("result = %#v, protocol error = %v", result, protocolErr)
			}
		})
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("HTTP calls = %d, want 0", got)
	}
}

func TestHandleSearchWebInputLimits(t *testing.T) {
	tests := []struct {
		name      string
		atLimit   map[string]any
		overLimit map[string]any
	}{
		{
			name:      "query runes",
			atLimit:   map[string]any{"query": strings.Repeat("界", models.MaxSearchQueryRunes)},
			overLimit: map[string]any{"query": strings.Repeat("界", models.MaxSearchQueryRunes+1)},
		},
		{
			name: "schema bytes",
			atLimit: map[string]any{"query": "q", "schema": `{}` +
				strings.Repeat(" ", models.MaxSearchSchemaBytes-2)},
			overLimit: map[string]any{"query": "q", "schema": `{}` +
				strings.Repeat(" ", models.MaxSearchSchemaBytes-1)},
		},
		{
			name: "LLM key bytes",
			atLimit: map[string]any{"query": "q", "schema": `{}`, "engine": "llm",
				"llm_api_key": strings.Repeat("k", models.MaxSearchLLMAPIKeyBytes)},
			overLimit: map[string]any{"query": "q", "schema": `{}`, "engine": "llm",
				"llm_api_key": strings.Repeat("k", models.MaxSearchLLMAPIKeyBytes+1)},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			response := validSearchResponse()
			response.Query = strings.Join(strings.Fields(test.atLimit["query"].(string)), " ")
			responseBody, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return httpResponse(http.StatusOK, responseBody), nil
			})}
			handler := handleSearchWebWithClient(client, "http://purify.test", "purify-api-secret")
			result, protocolErr := handler(context.Background(), searchRequest(test.atLimit))
			if protocolErr != nil || result.IsError || calls.Load() != 1 {
				t.Fatalf("N input = (%#v, %v), calls=%d", result, protocolErr, calls.Load())
			}
			result, protocolErr = handler(context.Background(), searchRequest(test.overLimit))
			if protocolErr != nil || !result.IsError || calls.Load() != 1 {
				t.Fatalf("N+1 input = (%#v, %v), calls=%d", result, protocolErr, calls.Load())
			}
		})
	}
}

func TestDecodeSearchResponseAcceptsCompleteEnrichmentShape(t *testing.T) {
	t.Parallel()

	response := validSearchResponse()
	fetchedAt := time.Date(2026, time.August, 10, 1, 2, 3, 0, time.UTC)
	verified := true
	unlocatedRate := 0.25
	basis := models.EvidenceBasis{
		"/name": {
			Quote: "Purify", TextRange: [2]int{0, 6}, Method: evidence.MethodExact,
			SnapshotID: "sha256:" + strings.Repeat("a", 64), FetchedAt: fetchedAt,
		},
	}
	receipts := models.FieldReceipts{"/name": "signed-field-receipt"}
	response.Results[0].FinalURL = "https://example.com/final"
	response.Results[0].Content = "Purify content"
	response.Results[0].Verified = &verified
	response.Results[0].VerificationStatus = models.SearchVerificationVerified
	response.Results[0].Evidence = &evidence.Anchor{
		Quote: "Purify", TextRange: [2]int{0, 6}, Method: evidence.MethodExact,
		SnapshotID: "sha256:" + strings.Repeat("a", 64), FetchedAt: fetchedAt,
	}
	response.Results[0].Receipt = "signed-snippet-receipt"
	response.Results[0].Data = json.RawMessage(`{"name":"Purify"}`)
	response.Results[0].Basis = &basis
	response.Results[0].Receipts = &receipts
	response.Results[0].UnlocatedRate = &unlocatedRate
	response.Results[0].Extractor = &models.ExtractorMetadata{
		ID: "extractor-id", Version: 2, CompiledAt: fetchedAt, Validation: 0.98, Mode: "compiled",
	}
	response.Results[0].Violations = []models.SchemaViolation{{Path: "/optional", Message: "optional field missing"}}
	response.Results[0].Errors = []models.SearchResultError{{
		Stage: models.SearchResultStageExtract, Code: models.ErrCodeLLMFailure, Message: "result extraction was partial",
	}}
	response.Partial = true
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeSearchResponse(body)
	if err != nil {
		t.Fatalf("decode complete response: %v", err)
	}
	if !reflect.DeepEqual(got, response) {
		t.Fatalf("decoded response = %#v, want %#v", got, response)
	}
}

func TestDecodeSearchResponseRejectsMalformedDocuments(t *testing.T) {
	base := validSearchResponse()
	tests := []struct {
		name string
		body []byte
	}{
		{name: "invalid JSON", body: []byte(`{"success":`)},
		{name: "null response", body: []byte(`null`)},
		{name: "trailing value", body: append(validSearchResponseBody(t), []byte(` {}`)...)},
		{name: "duplicate top-level field", body: bytes.Replace(validSearchResponseBody(t), []byte(`{"success":true`), []byte(`{"success":true,"success":true`), 1)},
		{name: "unknown top-level field", body: mutateSearchResponseBody(t, base, func(document map[string]any) {
			document["provider"] = "brave"
		})},
		{name: "missing results", body: mutateSearchResponseBody(t, base, func(document map[string]any) {
			delete(document, "results")
		})},
		{name: "null results", body: mutateSearchResponseBody(t, base, func(document map[string]any) {
			document["results"] = nil
		})},
		{name: "unknown result field", body: mutateSearchResponseBody(t, base, func(document map[string]any) {
			searchResultDocument(t, document, 0)["provider_payload"] = true
		})},
		{name: "missing result rank", body: mutateSearchResponseBody(t, base, func(document map[string]any) {
			delete(searchResultDocument(t, document, 0), "rank")
		})},
		{name: "non-sequential result rank", body: mutateSearchResponseBody(t, base, func(document map[string]any) {
			searchResultDocument(t, document, 0)["rank"] = float64(2)
		})},
		{name: "non-canonical URL", body: mutateSearchResponseBody(t, base, func(document map[string]any) {
			searchResultDocument(t, document, 0)["url"] = "HTTPS://EXAMPLE.COM"
		})},
		{name: "unknown verification status", body: mutateSearchResponseBody(t, base, func(document map[string]any) {
			searchResultDocument(t, document, 0)["verification_status"] = "future"
		})},
		{name: "verified without evidence", body: mutateSearchResponseBody(t, base, func(document map[string]any) {
			result := searchResultDocument(t, document, 0)
			result["verification_status"] = "verified"
			result["verified"] = true
			result["receipt"] = "receipt"
		})},
		{name: "null optional score", body: mutateSearchResponseBody(t, base, func(document map[string]any) {
			searchResultDocument(t, document, 0)["score"] = nil
		})},
		{name: "incomplete extraction shape", body: mutateSearchResponseBody(t, base, func(document map[string]any) {
			searchResultDocument(t, document, 0)["data"] = map[string]any{"name": "Purify"}
		})},
		{name: "null errors", body: mutateSearchResponseBody(t, base, func(document map[string]any) {
			searchResultDocument(t, document, 0)["errors"] = nil
		})},
		{name: "negative timing", body: mutateSearchResponseBody(t, base, func(document map[string]any) {
			document["timing"].(map[string]any)["total_ms"] = float64(-1)
		})},
		{name: "successful response with error", body: mutateSearchResponseBody(t, base, func(document map[string]any) {
			document["error"] = map[string]any{"code": "BAD", "message": "bad"}
		})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeSearchResponse(test.body); err == nil {
				t.Fatalf("decode accepted malformed body: %s", test.body)
			}
		})
	}
}

func TestHandleSearchWebPreservesStructuredErrorsAndRedactsCredentials(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   []byte
	}{
		{
			name:   "non-2xx structured error",
			status: http.StatusTooManyRequests,
			body: validSearchErrorBody(t, models.ErrCodeRateLimited,
				"purify-api-secret and caller-llm-secret were rejected"),
		},
		{
			name:   "successful response echoes secret",
			status: http.StatusOK,
			body: mutateSearchResponseBody(t, validSearchResponse(), func(document map[string]any) {
				searchResultDocument(t, document, 0)["snippet"] = "caller-llm-secret"
			}),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return httpResponse(test.status, test.body), nil
			})}
			handler := handleSearchWebWithClient(client, "http://purify.test", "purify-api-secret")
			result, protocolErr := handler(context.Background(), searchRequest(map[string]any{
				"query": "q", "schema": `{}`, "engine": "llm", "llm_api_key": "caller-llm-secret",
			}))
			if protocolErr != nil || !result.IsError {
				t.Fatalf("result = %#v, protocol error = %v", result, protocolErr)
			}
			message := toolResultText(t, result)
			for _, secret := range []string{"purify-api-secret", "caller-llm-secret"} {
				if strings.Contains(message, secret) {
					t.Fatalf("tool output leaked %q: %q", secret, message)
				}
			}
		})
	}
}

func TestSearchProductionClientRejectsCrossOriginRedirectWithoutLeakingCredentials(t *testing.T) {
	t.Parallel()

	var sourceCalls atomic.Int32
	var redirectCalls atomic.Int32
	var leakedCredential atomic.Bool
	client := newSearchHTTPClient()
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Host {
		case "purify.test":
			sourceCalls.Add(1)
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Errorf("read source body: %v", err)
			}
			if request.Header.Get("X-API-Key") != "purify-api-secret" || bytes.Contains(body, []byte("purify-api-secret")) {
				t.Errorf("Purify credential boundary violated: header=%q body=%s", request.Header.Get("X-API-Key"), body)
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
				leakedCredential.Store(true)
			}
			return httpResponse(http.StatusOK, validSearchResponseBody(t)), nil
		default:
			t.Fatalf("unexpected host %q", request.URL.Host)
			return nil, nil
		}
	})
	handler := handleSearchWebWithClient(client, "http://purify.test", "purify-api-secret")
	result, protocolErr := handler(context.Background(), searchRequest(map[string]any{
		"query": "q", "schema": `{}`, "engine": "llm", "llm_api_key": "caller-llm-secret",
	}))
	if protocolErr != nil || !result.IsError {
		t.Fatalf("result = %#v, protocol error = %v", result, protocolErr)
	}
	if got, want := toolResultText(t, result), "search failed (HTTP 307)"; got != want {
		t.Fatalf("tool error = %q, want %q", got, want)
	}
	if sourceCalls.Load() != 1 || redirectCalls.Load() != 0 || leakedCredential.Load() {
		t.Fatalf("source/redirect/leak = %d/%d/%t", sourceCalls.Load(), redirectCalls.Load(), leakedCredential.Load())
	}
}

func TestHandleSearchWebResponseBodyLimit(t *testing.T) {
	tests := []struct {
		name      string
		bodyBytes int
		wantError bool
	}{
		{name: "N bytes", bodyBytes: int(maxAPIResponseBytes)},
		{name: "N plus one bytes", bodyBytes: int(maxAPIResponseBytes) + 1, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := searchResponseBodyAtSize(t, test.bodyBytes)
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return httpResponse(http.StatusOK, body), nil
			})}
			handler := handleSearchWebWithClient(client, "http://purify.test", "purify-api-secret")
			result, protocolErr := handler(context.Background(), searchRequest(map[string]any{
				"query": "purify search", "include_content": true,
			}))
			if protocolErr != nil || result.IsError != test.wantError {
				t.Fatalf("result = %#v, protocol error = %v", result, protocolErr)
			}
			if !test.wantError && len(toolResultText(t, result)) != test.bodyBytes {
				t.Fatalf("fallback bytes = %d, want %d", len(toolResultText(t, result)), test.bodyBytes)
			}
		})
	}
}

func TestHandleSearchWebLateCancellationIsToolError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		cancel()
		return httpResponse(http.StatusOK, validSearchResponseBody(t)), nil
	})}
	handler := handleSearchWebWithClient(client, "http://purify.test", "purify-api-secret")
	result, protocolErr := handler(ctx, searchRequest(map[string]any{"query": "q"}))
	if protocolErr != nil || !result.IsError || !strings.Contains(toolResultText(t, result), context.Canceled.Error()) {
		t.Fatalf("result = %#v, protocol error = %v", result, protocolErr)
	}
}

func TestSearchJSONContainsSecretAcrossEscapedNestedValuesAndKeys(t *testing.T) {
	t.Parallel()

	tests := []string{
		`{"data":{"safe":"caller-llm-\u0073ecret"}}`,
		`{"data":{"caller-llm-\u0073ecret":"safe"}}`,
	}
	for _, body := range tests {
		contains, err := searchJSONContainsSecret([]byte(body), "caller-llm-secret")
		if err != nil || !contains {
			t.Fatalf("scan(%s) = %t, %v", body, contains, err)
		}
	}
}

func TestHandleSearchWebEscapedSecretUsesFixedError(t *testing.T) {
	t.Parallel()

	body := bytes.Replace(validSearchResponseBody(t), []byte("Provider-neutral verified search"), []byte(`caller-llm-\u0073ecret`), 1)
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return httpResponse(http.StatusOK, body), nil
	})}
	handler := handleSearchWebWithClient(client, "http://purify.test", "purify-api-secret")
	result, protocolErr := handler(context.Background(), searchRequest(map[string]any{
		"query": "purify search", "schema": `{}`, "engine": "llm", "llm_api_key": "caller-llm-secret",
	}))
	if protocolErr != nil || !result.IsError {
		t.Fatalf("result = %#v, protocol error = %v", result, protocolErr)
	}
	if got, want := toolResultText(t, result), "failed to parse search response: response contains sensitive data"; got != want {
		t.Fatalf("tool error = %q, want %q", got, want)
	}
}

func TestHandleSearchWebUsesRequestTimeoutAndParentDeadline(t *testing.T) {
	tests := []struct {
		name       string
		ctx        func() (context.Context, context.CancelFunc)
		timeout    int
		maximumRun time.Duration
	}{
		{
			name: "request timeout",
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
			timeout:    1,
			maximumRun: 2 * time.Second,
		},
		{
			name: "earlier parent deadline",
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 20*time.Millisecond)
			},
			timeout:    models.MaxSearchTimeoutSeconds,
			maximumRun: time.Second,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				<-request.Context().Done()
				return nil, request.Context().Err()
			})}
			handler := handleSearchWebWithClient(client, "http://purify.test", "purify-api-secret")
			ctx, cancel := test.ctx()
			defer cancel()
			started := time.Now()
			result, protocolErr := handler(ctx, searchRequest(map[string]any{"query": "q", "timeout": test.timeout}))
			elapsed := time.Since(started)
			if protocolErr != nil || !result.IsError || !strings.Contains(toolResultText(t, result), context.DeadlineExceeded.Error()) {
				t.Fatalf("result = %#v, protocol error = %v", result, protocolErr)
			}
			if elapsed > test.maximumRun {
				t.Fatalf("elapsed = %s, want <= %s", elapsed, test.maximumRun)
			}
		})
	}
}

func TestHandleSearchWebRejectsResponseOutsideRequestCapabilities(t *testing.T) {
	tests := []struct {
		name      string
		arguments map[string]any
		response  models.SearchResponse
	}{
		{
			name:      "query mismatch",
			arguments: map[string]any{"query": "different query"},
			response:  validSearchResponse(),
		},
		{
			name:      "content not requested",
			arguments: map[string]any{"query": "purify search"},
			response: searchResponseWith(t, func(response *models.SearchResponse) {
				response.Results[0].Content = "forged content"
			}),
		},
		{
			name:      "result count exceeds request limit",
			arguments: map[string]any{"query": "purify search", "limit": 1},
			response: searchResponseWith(t, func(response *models.SearchResponse) {
				second := response.Results[0]
				second.Rank = 2
				second.URL = "https://example.net/article"
				response.Results = append(response.Results, second)
			}),
		},
		{
			name:      "verification not requested",
			arguments: map[string]any{"query": "purify search"},
			response: searchResponseWith(t, func(response *models.SearchResponse) {
				verified := false
				response.Results[0].Verified = &verified
				response.Results[0].VerificationStatus = models.SearchVerificationMismatch
			}),
		},
		{
			name:      "extraction not requested",
			arguments: map[string]any{"query": "purify search"},
			response:  searchResponseWithNullData(t),
		},
		{
			name:      "enrichment beyond top five",
			arguments: map[string]any{"query": "purify search", "limit": 6, "include_content": true},
			response: searchResponseWith(t, func(response *models.SearchResponse) {
				for len(response.Results) < 6 {
					index := len(response.Results)
					result := response.Results[0]
					result.Rank = index + 1
					result.URL = fmt.Sprintf("https://example%d.com/article", index)
					result.Score = nil
					result.PublishedAt = nil
					response.Results = append(response.Results, result)
				}
				response.Results[5].FinalURL = "https://example5.com/final"
			}),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body, err := json.Marshal(test.response)
			if err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return httpResponse(http.StatusOK, body), nil
			})}
			handler := handleSearchWebWithClient(client, "http://purify.test", "purify-api-secret")
			result, protocolErr := handler(context.Background(), searchRequest(test.arguments))
			if protocolErr != nil || !result.IsError || !strings.Contains(toolResultText(t, result), "failed to parse search response") {
				t.Fatalf("result = %#v, protocol error = %v", result, protocolErr)
			}
		})
	}
}

func TestHandleSearchWebAcceptsSchemaDataNull(t *testing.T) {
	t.Parallel()

	response := searchResponseWithNullData(t)
	response.Query = "q"
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return httpResponse(http.StatusOK, body), nil
	})}
	handler := handleSearchWebWithClient(client, "http://purify.test", "purify-api-secret")
	result, protocolErr := handler(context.Background(), searchRequest(map[string]any{
		"query": "q", "schema": `{}`, "engine": "compiled",
	}))
	if protocolErr != nil || result.IsError {
		t.Fatalf("result = %#v, protocol error = %v, text=%s", result, protocolErr, toolResultText(t, result))
	}
	got := result.StructuredContent.(models.SearchResponse)
	if string(got.Results[0].Data) != "null" {
		t.Fatalf("data = %s, want null", got.Results[0].Data)
	}
}

func TestDecodeSearchResponseAcceptsCompiledQuoteWithoutTextRange(t *testing.T) {
	t.Parallel()

	response := searchResponseWithNullData(t)
	fetchedAt := time.Date(2026, time.August, 10, 1, 2, 3, 0, time.UTC)
	basis := models.EvidenceBasis{"/value": {
		Quote: "compiled quote", Method: evidence.MethodCompiled,
		SnapshotID: "sha256:" + strings.Repeat("a", 64), FetchedAt: fetchedAt,
	}}
	receipts := models.FieldReceipts{"/value": "signed-receipt"}
	rate := 1.0
	response.Results[0].Basis = &basis
	response.Results[0].Receipts = &receipts
	response.Results[0].UnlocatedRate = &rate
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeSearchResponse(body); err != nil {
		t.Fatalf("compiled zero-range quote rejected: %v", err)
	}
}

func TestDecodeSearchResponseRejectsDuplicateEffectiveFinalURL(t *testing.T) {
	t.Parallel()

	response := validSearchResponse()
	response.Results[0].FinalURL = "https://canonical.example.com/article"
	second := response.Results[0]
	second.Rank = 2
	second.URL = "https://alias.example.net/article"
	response.Results = append(response.Results, second)
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeSearchResponse(body); err == nil {
		t.Fatal("duplicate effective final URL was accepted")
	}
}

func TestDecodeSearchResponseRejectsOrphanExtractionPresence(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value any
	}{
		{name: "null extractor", field: "extractor", value: nil},
		{name: "null llm usage", field: "llm_usage", value: nil},
		{name: "empty violations", field: "violations", value: []any{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := mutateSearchResponseBody(t, validSearchResponse(), func(document map[string]any) {
				searchResultDocument(t, document, 0)[test.field] = test.value
			})
			if _, err := decodeSearchResponse(body); err == nil {
				t.Fatalf("orphan %s was accepted", test.field)
			}
		})
	}
}

func TestHandleSearchWebRequiresHTTP200(t *testing.T) {
	t.Parallel()

	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return httpResponse(http.StatusCreated, validSearchResponseBody(t)), nil
	})}
	handler := handleSearchWebWithClient(client, "http://purify.test", "purify-api-secret")
	result, protocolErr := handler(context.Background(), searchRequest(map[string]any{"query": "purify search"}))
	if protocolErr != nil || !result.IsError || toolResultText(t, result) != "search failed (HTTP 201)" {
		t.Fatalf("result = %#v, protocol error = %v", result, protocolErr)
	}
}

func TestDecodeSearchResponseRejectsTimingAndPartialInconsistency(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "timing components exceed total", mutate: func(document map[string]any) {
			timing := document["timing"].(map[string]any)
			timing["total_ms"] = float64(3)
			timing["provider_ms"] = float64(2)
			timing["enrichment_ms"] = float64(2)
		}},
		{name: "partial without errors", mutate: func(document map[string]any) {
			document["partial"] = true
		}},
		{name: "errors without partial", mutate: func(document map[string]any) {
			searchResultDocument(t, document, 0)["errors"] = []any{map[string]any{
				"stage": "fetch", "code": models.ErrCodeNavigation, "message": "result fetch failed",
			}}
		}},
		{name: "verification unavailable without error", mutate: func(document map[string]any) {
			result := searchResultDocument(t, document, 0)
			result["verification_status"] = "unavailable"
			result["verified"] = false
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := mutateSearchResponseBody(t, validSearchResponse(), test.mutate)
			if _, err := decodeSearchResponse(body); err == nil {
				t.Fatal("inconsistent response was accepted")
			}
		})
	}
	t.Run("timing addition overflow", func(t *testing.T) {
		response := validSearchResponse()
		response.Timing = models.SearchTimingInfo{
			TotalMs: math.MaxInt64, ProviderMs: math.MaxInt64, EnrichmentMs: 1,
		}
		body, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeSearchResponse(body); err == nil {
			t.Fatal("overflowing timing response was accepted")
		}
	})
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
				if _, present := raw["url"]; !present {
					t.Errorf("single request physically omitted url: %s", body)
				}
				if _, present := raw["sources"]; present {
					t.Errorf("single request physically contains sources: %s", body)
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

func TestHandleExtractDataMultiRequestMatrixAndCredentialSeparation(t *testing.T) {
	tests := []struct {
		name              string
		arguments         map[string]any
		wantEngine        string
		wantLLMCredential bool
	}{
		{
			name: "auto defaults without LLM fallback credential",
			arguments: multiExtractArguments(
				"https://one.example.com/product",
				"https://two.example.net/product",
			),
			wantEngine: "auto",
		},
		{
			name: "auto forwards caller fallback settings",
			arguments: multiExtractArgumentsWithMany(map[string]any{
				"engine": "auto", "llm_api_key": "caller-llm-secret",
				"llm_model": "caller-model", "llm_base_url": "https://llm.example/v1",
			}, "https://one.example.com/product", "https://two.example.net/product"),
			wantEngine:        "auto",
			wantLLMCredential: true,
		},
		{
			name: "compiled physically strips all LLM settings",
			arguments: multiExtractArgumentsWithMany(map[string]any{
				"engine": "compiled", "llm_api_key": "caller-llm-secret",
				"llm_model": "must-not-forward", "llm_base_url": "https://must-not-forward.example/v1",
			}, "https://one.example.com/product", "https://two.example.net/product"),
			wantEngine: "compiled",
		},
		{
			name: "llm keeps API auth separate from caller credential",
			arguments: multiExtractArgumentsWithMany(map[string]any{
				"engine": "llm", "llm_api_key": "caller-llm-secret",
				"llm_model": "caller-model", "llm_base_url": "https://llm.example/v1",
			}, "https://one.example.com/product", "https://two.example.net/product"),
			wantEngine:        "llm",
			wantLLMCredential: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			responseBody := validMultiExtractResponseBody(t, models.MultiExtractStatusComplete)
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.Method != http.MethodPost || request.URL.String() != "http://purify.test/api/v1/extract" {
					t.Errorf("request = %s %s", request.Method, request.URL)
				}
				if got := request.Header.Get("X-API-Key"); got != "purify-api-secret" {
					t.Errorf("X-API-Key = %q, want Purify API credential", got)
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
				if _, present := raw["url"]; present {
					t.Errorf("multi request physically contains url: %s", body)
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
				if payload.URL != "" || !reflect.DeepEqual(payload.Sources, []string{
					"https://one.example.com/product", "https://two.example.net/product",
				}) || payload.Engine != test.wantEngine || !json.Valid(payload.Schema) {
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
			if _, ok := result.StructuredContent.(models.MultiExtractResponse); !ok {
				t.Fatalf("structured content type = %T, want models.MultiExtractResponse", result.StructuredContent)
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

func TestHandleExtractDataKnownMultiStatusesAreStructuredSuccess(t *testing.T) {
	nullComplete := validMultiExtractResponse(models.MultiExtractStatusComplete)
	nullComplete.Data = json.RawMessage(`null`)
	nullField := nullComplete.Consensus.Fields["/name"]
	nullField.Value = json.RawMessage(`null`)
	nullComplete.Consensus.Fields["/name"] = nullField
	mixedComplete := validMultiExtractResponse(models.MultiExtractStatusComplete)
	mixedComplete.Sources[0].LLMUsage = &models.LLMUsage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}
	duplicate := mixedComplete.Sources[0]
	duplicate.URL = "https://alias.example.org/product"
	duplicate.Success = false
	duplicate.Status = models.MultiExtractSourceStatusDuplicate
	duplicate.DuplicateOf = mixedComplete.Sources[0].URL
	timedOut := models.MultiExtractSource{
		URL:     "https://timeout.example.net/product",
		Success: false,
		Status:  models.MultiExtractSourceStatusTimeout,
		Timing:  models.ExtractTimingInfo{TotalMs: 8},
		Error:   &models.ErrorDetail{Code: models.ErrCodeTimeout, Message: "source timed out"},
	}
	mixedComplete.Sources = append(mixedComplete.Sources, duplicate, timedOut)
	mixedComplete.Tokens = models.TokenInfo{OriginalEstimate: 200, CleanedEstimate: 40, SavingsPercent: 80}
	mixedComplete.Timing = models.ExtractTimingInfo{TotalMs: 10, NavigationMs: 12, CleaningMs: 4, ExtractionMs: 2}
	mixedComplete.LLMUsage = &models.LLMUsage{PromptTokens: 6, CompletionTokens: 4, TotalTokens: 10}
	whitespacePath := validMultiExtractResponse(models.MultiExtractStatusComplete)
	whitespaceField := whitespacePath.Consensus.Fields["/name"]
	delete(whitespacePath.Consensus.Fields, "/name")
	whitespacePath.Consensus.Fields["   "] = whitespaceField
	whitespacePath.Data = json.RawMessage(`{"   ":"Purify"}`)
	tests := []struct {
		name     string
		response models.MultiExtractResponse
	}{
		{name: "complete", response: validMultiExtractResponse(models.MultiExtractStatusComplete)},
		{name: "complete JSON null", response: nullComplete},
		{name: "complete with duplicate and failed source", response: mixedComplete},
		{name: "complete with whitespace-only JSON property path", response: whitespacePath},
		{name: "complete with lower-scored conflict", response: validNonAmbiguousConflictMultiExtractResponse()},
		{name: "object presence ambiguity without ambiguous scalar", response: validMultiExtractResponse(models.MultiExtractStatusAmbiguous)},
		{name: "node kind ambiguity without ambiguous scalar", response: validMultiExtractResponse(models.MultiExtractStatusAmbiguous)},
		{name: "array length ambiguity without ambiguous scalar", response: validMultiExtractResponse(models.MultiExtractStatusAmbiguous)},
		{name: "field ambiguity", response: validFieldAmbiguousMultiExtractResponse()},
		{name: "schema invalid", response: validMultiExtractResponse(models.MultiExtractStatusSchemaInvalid)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body, err := json.Marshal(test.response)
			if err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return httpResponse(http.StatusOK, body), nil
			})}
			handler := handleExtractDataWithClient(client, "http://purify.test", "purify-api-secret")

			result, protocolErr := handler(context.Background(), extractRequest(multiExtractArguments("https://one.example.com/product")))
			if protocolErr != nil || result.IsError {
				t.Fatalf("result = %#v, protocol error = %v, text = %s", result, protocolErr, toolResultText(t, result))
			}
			got, ok := result.StructuredContent.(models.MultiExtractResponse)
			if !ok {
				t.Fatalf("structured content type = %T", result.StructuredContent)
			}
			if !reflect.DeepEqual(got, test.response) {
				t.Fatalf("structured response = %#v, want %#v", got, test.response)
			}
			var fallback models.MultiExtractResponse
			if err := json.Unmarshal([]byte(toolResultText(t, result)), &fallback); err != nil {
				t.Fatalf("fallback is not JSON: %v", err)
			}
			fallbackJSON, err := json.Marshal(fallback)
			if err != nil {
				t.Fatal(err)
			}
			wantJSON, err := json.Marshal(test.response)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(fallbackJSON, wantJSON) {
				t.Fatalf("fallback JSON = %s, want %s", fallbackJSON, wantJSON)
			}
		})
	}
}

func TestDecodeMultiExtractResponseAcceptsEveryJSONScalarKind(t *testing.T) {
	tests := []struct {
		name  string
		value json.RawMessage
	}{
		{name: "null", value: json.RawMessage(`null`)},
		{name: "boolean", value: json.RawMessage(`true`)},
		{name: "number", value: json.RawMessage(`12.5`)},
		{name: "string", value: json.RawMessage(`"Purify"`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := validMultiExtractResponse(models.MultiExtractStatusComplete)
			field := response.Consensus.Fields["/name"]
			field.Value = append(json.RawMessage(nil), test.value...)
			response.Consensus.Fields["/name"] = field
			response.Data = append(json.RawMessage(nil), test.value...)
			body, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeMultiExtractResponse(body); err != nil {
				t.Fatalf("decode scalar %s: %v", test.value, err)
			}
		})
	}
}

func TestDecodeMultiExtractResponseAcceptsFoldReasonStates(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{
			name: "agreement single reason",
			body: mutateMultiExtractResponseBody(t, validNonAmbiguousConflictMultiExtractResponse(), func(document map[string]any) {
				agreement := multiConsensusFieldDocument(t, document, "/name")["agreement"].(map[string]any)
				agreement["independent_roots"] = float64(1)
				agreement["fold_reason"] = "same_root"
			}),
		},
		{
			name: "agreement mixed reasons omitted",
			body: mutateMultiExtractResponseBody(t, validNonAmbiguousConflictMultiExtractResponse(), func(document map[string]any) {
				field := multiConsensusFieldDocument(t, document, "/name")
				field["agreement"].(map[string]any)["independent_roots"] = float64(1)
				supports := field["supports"].([]any)
				supports[0].(map[string]any)["fold_reason"] = "same_root"
				supports[1].(map[string]any)["fold_reason"] = "quote_lineage"
			}),
		},
		{
			name: "support reason is source-global",
			body: mutateMultiExtractResponseBody(t, validMultiExtractResponse(models.MultiExtractStatusComplete), func(document map[string]any) {
				support := multiConsensusFieldDocument(t, document, "/name")["supports"].([]any)[0].(map[string]any)
				support["fold_reason"] = "near_duplicate"
			}),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeMultiExtractResponse(test.body); err != nil {
				t.Fatalf("decoder rejected valid fold-reason state: %v\n%s", err, test.body)
			}
		})
	}
}

func TestDecodeMultiExtractResponseRejectsInvalidFoldReasons(t *testing.T) {
	agreementBody := func(reasonField string, reason any, folded bool) []byte {
		return mutateMultiExtractResponseBody(t, validNonAmbiguousConflictMultiExtractResponse(), func(document map[string]any) {
			field := multiConsensusFieldDocument(t, document, "/name")
			if folded {
				field["agreement"].(map[string]any)["independent_roots"] = float64(1)
			}
			field["agreement"].(map[string]any)[reasonField] = reason
		})
	}
	supportBody := func(reasonField string, reason any) []byte {
		return mutateMultiExtractResponseBody(t, validMultiExtractResponse(models.MultiExtractStatusComplete), func(document map[string]any) {
			support := multiConsensusFieldDocument(t, document, "/name")["supports"].([]any)[0].(map[string]any)
			support[reasonField] = reason
		})
	}
	tests := []struct {
		name string
		body []byte
	}{
		{name: "agreement null", body: agreementBody("fold_reason", nil, true)},
		{name: "agreement empty", body: agreementBody("fold_reason", "", true)},
		{name: "agreement unknown", body: agreementBody("fold_reason", "other", true)},
		{name: "agreement reason without an actual fold", body: agreementBody("fold_reason", "same_root", false)},
		{name: "agreement case-smuggled field", body: agreementBody("Fold_Reason", "same_root", true)},
		{name: "support null", body: supportBody("fold_reason", nil)},
		{name: "support empty", body: supportBody("fold_reason", "")},
		{name: "support unknown", body: supportBody("fold_reason", "other")},
		{name: "support case-smuggled field", body: supportBody("Fold_Reason", "same_root")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeMultiExtractResponse(test.body); err == nil {
				t.Fatalf("decoder accepted invalid fold reason: %s", test.body)
			}
		})
	}
}

func TestHandleExtractDataRejectsMalformedMultiSuccessResponse(t *testing.T) {
	complete := validMultiExtractResponse(models.MultiExtractStatusComplete)
	fieldAmbiguous := validFieldAmbiguousMultiExtractResponse()
	nonAmbiguousConflict := validNonAmbiguousConflictMultiExtractResponse()
	duplicateResponse := validMultiExtractResponse(models.MultiExtractStatusComplete)
	duplicateSource := duplicateResponse.Sources[0]
	duplicateSource.URL = "https://alias.example.org/product"
	duplicateSource.Success = false
	duplicateSource.Status = models.MultiExtractSourceStatusDuplicate
	duplicateSource.DuplicateOf = duplicateResponse.Sources[0].URL
	duplicateResponse.Sources = append(duplicateResponse.Sources, duplicateSource)
	invalidUTF8Path := bytes.Replace(
		validMultiExtractResponseBody(t, models.MultiExtractStatusComplete),
		[]byte(`"/name"`),
		[]byte{'"', 0xff, '"'},
		1,
	)
	tests := []struct {
		name string
		body []byte
	}{
		{name: "invalid JSON", body: []byte(`{"success":`)},
		{name: "null", body: []byte(`null`)},
		{name: "trailing value", body: append(validMultiExtractResponseBody(t, models.MultiExtractStatusComplete), []byte(` {}`)...)},
		{name: "unknown top-level field", body: mutateMultiExtractResponseBody(t, complete, func(document map[string]any) {
			document["unexpected"] = true
		})},
		{name: "missing status", body: mutateMultiExtractResponseBody(t, complete, func(document map[string]any) {
			delete(document, "status")
		})},
		{name: "unknown aggregate status", body: mutateMultiExtractResponseBody(t, complete, func(document map[string]any) {
			document["status"] = "future"
		})},
		{name: "complete missing data", body: mutateMultiExtractResponseBody(t, complete, func(document map[string]any) {
			delete(document, "data")
		})},
		{name: "ambiguous with data", body: mutateMultiExtractResponseBody(t, validMultiExtractResponse(models.MultiExtractStatusAmbiguous), func(document map[string]any) {
			document["data"] = map[string]any{"name": "Purify"}
		})},
		{name: "schema invalid without violations", body: mutateMultiExtractResponseBody(t, validMultiExtractResponse(models.MultiExtractStatusSchemaInvalid), func(document map[string]any) {
			delete(document, "violations")
		})},
		{name: "successful response with error", body: mutateMultiExtractResponseBody(t, complete, func(document map[string]any) {
			document["error"] = map[string]any{"code": "BAD", "message": "bad"}
		})},
		{name: "unknown source status", body: mutateMultiExtractResponseBody(t, complete, func(document map[string]any) {
			multiSourceDocument(t, document, 0)["status"] = "future"
		})},
		{name: "valid source marked unsuccessful", body: mutateMultiExtractResponseBody(t, complete, func(document map[string]any) {
			multiSourceDocument(t, document, 0)["success"] = false
		})},
		{name: "duplicate source summary URL", body: mutateMultiExtractResponseBody(t, complete, func(document map[string]any) {
			sources := document["sources"].([]any)
			document["sources"] = append(sources, cloneJSONMap(t, sources[0].(map[string]any)))
		})},
		{name: "duplicate points to missing winner", body: mutateMultiExtractResponseBody(t, duplicateResponse, func(document map[string]any) {
			multiSourceDocument(t, document, 1)["duplicate_of"] = "https://missing.example.net/product"
		})},
		{name: "duplicate points to failed source", body: mutateMultiExtractResponseBody(t, duplicateResponse, func(document map[string]any) {
			sources := document["sources"].([]any)
			failed := map[string]any{
				"url": "https://failed.example.net/product", "success": false, "status": "timeout",
				"tokens": map[string]any{"original_estimate": 0, "cleaned_estimate": 0, "savings_percent": 0},
				"timing": map[string]any{"total_ms": 1, "navigation_ms": 0, "cleaning_ms": 0, "extraction_ms": 0},
				"error":  map[string]any{"code": models.ErrCodeTimeout, "message": "source timed out"},
			}
			document["sources"] = append(sources, failed)
			multiSourceDocument(t, document, 1)["duplicate_of"] = "https://failed.example.net/product"
		})},
		{name: "duplicate final URL differs from winner", body: mutateMultiExtractResponseBody(t, duplicateResponse, func(document map[string]any) {
			multiSourceDocument(t, document, 1)["final_url"] = "https://other.example.net/product"
		})},
		{name: "negative source metric", body: mutateMultiExtractResponseBody(t, complete, func(document map[string]any) {
			multiSourceDocument(t, document, 0)["tokens"].(map[string]any)["cleaned_estimate"] = -1
		})},
		{name: "missing agreement component", body: mutateMultiExtractResponseBody(t, complete, func(document map[string]any) {
			agreement := multiConsensusFieldDocument(t, document, "/name")["agreement"].(map[string]any)
			delete(agreement, "independent_roots")
		})},
		{name: "agreement exceeds page support", body: mutateMultiExtractResponseBody(t, complete, func(document map[string]any) {
			multiConsensusFieldDocument(t, document, "/name")["agreement"].(map[string]any)["pages"] = 2
		})},
		{name: "support root does not match URL", body: mutateMultiExtractResponseBody(t, complete, func(document map[string]any) {
			support := multiConsensusFieldDocument(t, document, "/name")["supports"].([]any)[0].(map[string]any)
			support["root"] = "example.net"
		})},
		{name: "repeated support URL inflates agreement", body: mutateMultiExtractResponseBody(t, complete, func(document map[string]any) {
			field := multiConsensusFieldDocument(t, document, "/name")
			supports := field["supports"].([]any)
			field["supports"] = append(supports, cloneJSONMap(t, supports[0].(map[string]any)))
			field["agreement"].(map[string]any)["pages"] = 2
			field["agreement"].(map[string]any)["independent_roots"] = 2
		})},
		{name: "independent roots exceed distinct derived roots", body: mutateMultiExtractResponseBody(t, complete, func(document map[string]any) {
			sources := document["sources"].([]any)
			secondSource := cloneJSONMap(t, sources[0].(map[string]any))
			secondSource["url"] = "https://two.example.com/product"
			secondSource["final_url"] = "https://two.example.com/product"
			secondSource["snapshot_id"] = "snap-two"
			document["sources"] = append(sources, secondSource)
			field := multiConsensusFieldDocument(t, document, "/name")
			supports := field["supports"].([]any)
			secondSupport := cloneJSONMap(t, supports[0].(map[string]any))
			secondSupport["url"] = "https://two.example.com/product"
			secondSupport["root"] = "example.com"
			secondSupport["receipt"] = "receipt-two"
			secondSupport["evidence"].(map[string]any)["snapshot_id"] = "snap-two"
			field["supports"] = append(supports, secondSupport)
			field["agreement"].(map[string]any)["pages"] = 2
			field["agreement"].(map[string]any)["independent_roots"] = 2
		})},
		{name: "missing support receipt", body: mutateMultiExtractResponseBody(t, complete, func(document map[string]any) {
			support := multiConsensusFieldDocument(t, document, "/name")["supports"].([]any)[0].(map[string]any)
			delete(support, "receipt")
		})},
		{name: "unlocated support evidence", body: mutateMultiExtractResponseBody(t, complete, func(document map[string]any) {
			support := multiConsensusFieldDocument(t, document, "/name")["supports"].([]any)[0].(map[string]any)
			support["evidence"].(map[string]any)["method"] = "unlocated"
		})},
		{name: "support references another snapshot", body: mutateMultiExtractResponseBody(t, complete, func(document map[string]any) {
			support := multiConsensusFieldDocument(t, document, "/name")["supports"].([]any)[0].(map[string]any)
			support["evidence"].(map[string]any)["snapshot_id"] = "snap-other"
		})},
		{name: "evidence range has extra offset", body: mutateMultiExtractResponseBody(t, complete, func(document map[string]any) {
			support := multiConsensusFieldDocument(t, document, "/name")["supports"].([]any)[0].(map[string]any)
			support["evidence"].(map[string]any)["text_range"] = []any{0, 6, 7}
		})},
		{name: "ambiguous field missing conflict receipt", body: mutateMultiExtractResponseBody(t, fieldAmbiguous, func(document map[string]any) {
			conflict := multiConsensusFieldDocument(t, document, "/name")["conflicts"].([]any)[0].(map[string]any)
			delete(conflict["supports"].([]any)[0].(map[string]any), "receipt")
		})},
		{name: "winner and conflict reuse one source", body: mutateMultiExtractResponseBody(t, nonAmbiguousConflict, func(document map[string]any) {
			field := multiConsensusFieldDocument(t, document, "/name")
			winnerSupport := cloneJSONMap(t, field["supports"].([]any)[0].(map[string]any))
			field["conflicts"].([]any)[0].(map[string]any)["supports"] = []any{winnerSupport}
		})},
		{name: "ambiguous conflicts reuse one source", body: mutateMultiExtractResponseBody(t, fieldAmbiguous, func(document map[string]any) {
			field := multiConsensusFieldDocument(t, document, "/name")
			conflicts := field["conflicts"].([]any)
			firstSupport := cloneJSONMap(t, conflicts[0].(map[string]any)["supports"].([]any)[0].(map[string]any))
			conflicts[1].(map[string]any)["supports"] = []any{firstSupport}
		})},
		{name: "winner value is an object", body: mutateMultiExtractResponseBody(t, complete, func(document map[string]any) {
			multiConsensusFieldDocument(t, document, "/name")["value"] = map[string]any{}
			document["data"] = map[string]any{"name": map[string]any{}}
		})},
		{name: "conflict value is an array", body: mutateMultiExtractResponseBody(t, fieldAmbiguous, func(document map[string]any) {
			field := multiConsensusFieldDocument(t, document, "/name")
			field["conflicts"].([]any)[0].(map[string]any)["value"] = []any{}
		})},
		{name: "non-ambiguous winner does not outrank conflict", body: mutateMultiExtractResponseBody(t, fieldAmbiguous, func(document map[string]any) {
			field := multiConsensusFieldDocument(t, document, "/name")
			conflicts := field["conflicts"].([]any)
			winner := conflicts[0].(map[string]any)
			field["value"] = winner["value"]
			field["agreement"] = winner["agreement"]
			field["supports"] = winner["supports"]
			field["conflicts"] = conflicts[1:]
			delete(field, "ambiguous")
		})},
		{name: "ambiguous leading conflicts are not tied", body: mutateMultiExtractResponseBody(t, fieldAmbiguous, func(document map[string]any) {
			field := multiConsensusFieldDocument(t, document, "/name")
			conflicts := field["conflicts"].([]any)
			first := conflicts[0].(map[string]any)
			secondSupport := cloneJSONMap(t, conflicts[1].(map[string]any)["supports"].([]any)[0].(map[string]any))
			first["supports"] = append(first["supports"].([]any), secondSupport)
			first["agreement"].(map[string]any)["pages"] = 2
			first["agreement"].(map[string]any)["independent_roots"] = 2
		})},
		{name: "conflict scores increase", body: mutateMultiExtractResponseBody(t, fieldAmbiguous, func(document map[string]any) {
			field := multiConsensusFieldDocument(t, document, "/name")
			conflicts := field["conflicts"].([]any)
			third := cloneJSONMap(t, conflicts[0].(map[string]any))
			secondSupport := cloneJSONMap(t, conflicts[1].(map[string]any)["supports"].([]any)[0].(map[string]any))
			third["supports"] = append(third["supports"].([]any), secondSupport)
			third["agreement"].(map[string]any)["pages"] = 2
			third["agreement"].(map[string]any)["independent_roots"] = 2
			field["conflicts"] = append(conflicts, third)
		})},
		{name: "empty consensus path", body: mutateMultiExtractResponseBody(t, complete, func(document map[string]any) {
			fields := document["consensus"].(map[string]any)["fields"].(map[string]any)
			fields[""] = fields["/name"]
			delete(fields, "/name")
		})},
		{name: "oversized consensus path", body: mutateMultiExtractResponseBody(t, complete, func(document map[string]any) {
			fields := document["consensus"].(map[string]any)["fields"].(map[string]any)
			fields[strings.Repeat("p", maxExtractConsensusPathBytes+1)] = fields["/name"]
			delete(fields, "/name")
		})},
		{name: "invalid UTF-8 consensus path", body: invalidUTF8Path},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return httpResponse(http.StatusOK, test.body), nil
			})}
			handler := handleExtractDataWithClient(client, "http://purify.test", "purify-api-secret")

			result, protocolErr := handler(context.Background(), extractRequest(multiExtractArguments("https://one.example.com/product")))
			if protocolErr != nil {
				t.Fatalf("protocol error: %v", protocolErr)
			}
			if !result.IsError || !strings.Contains(toolResultText(t, result), "failed to parse multi-source extract response") {
				t.Fatalf("result = %#v", result)
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
		{name: "missing target", arguments: map[string]any{"schema": validExtractSchemaJSON}},
		{name: "both targets", arguments: map[string]any{
			"url": "https://example.com", "sources": []any{"https://source.example.com"}, "schema": validExtractSchemaJSON,
		}},
		{name: "empty URL", arguments: map[string]any{"url": "  ", "schema": validExtractSchemaJSON}},
		{name: "invalid URL", arguments: map[string]any{"url": "file:///private/page", "schema": validExtractSchemaJSON}},
		{name: "null sources", arguments: map[string]any{"sources": nil, "schema": validExtractSchemaJSON}},
		{name: "non-array sources", arguments: map[string]any{"sources": "https://example.com", "schema": validExtractSchemaJSON}},
		{name: "empty sources", arguments: map[string]any{"sources": []any{}, "schema": validExtractSchemaJSON}},
		{name: "too many sources", arguments: map[string]any{"sources": []any{
			"https://1.example.com", "https://2.example.com", "https://3.example.com",
			"https://4.example.com", "https://5.example.com", "https://6.example.com",
			"https://7.example.com", "https://8.example.com", "https://9.example.com",
		}, "schema": validExtractSchemaJSON}},
		{name: "non-string source", arguments: map[string]any{"sources": []any{42}, "schema": validExtractSchemaJSON}},
		{name: "empty source", arguments: map[string]any{"sources": []any{""}, "schema": validExtractSchemaJSON}},
		{name: "invalid source URL", arguments: map[string]any{"sources": []any{"file:///private/page"}, "schema": validExtractSchemaJSON}},
		{name: "invalid UTF-8 source", arguments: map[string]any{"sources": []any{"https://example.com/\xff"}, "schema": validExtractSchemaJSON}},
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

func TestHandleExtractDataSourceCountAndRawByteBudgets(t *testing.T) {
	exactSources := make([]string, models.MaxExtractSources)
	for index := range exactSources {
		prefix := fmt.Sprintf("https://s%d.example/", index)
		exactSources[index] = prefix + strings.Repeat("p", models.MaxExtractSourceURLBytes-len(prefix))
		if len(exactSources[index]) != models.MaxExtractSourceURLBytes {
			t.Fatal("source fixture is not at the per-source limit")
		}
	}
	used := 0
	for _, source := range exactSources {
		used += len(source)
	}
	if used != models.MaxExtractSourcesURLBytes {
		t.Fatalf("source fixture bytes = %d, want %d", used, models.MaxExtractSourcesURLBytes)
	}

	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return httpResponse(http.StatusOK, validMultiExtractResponseBody(t, models.MultiExtractStatusComplete)), nil
	})}
	handler := handleExtractDataWithClient(client, "http://purify.test", "purify-api-secret")

	result, protocolErr := handler(context.Background(), extractRequest(multiExtractArguments(exactSources...)))
	if protocolErr != nil || result.IsError {
		t.Fatalf("exact source budget = (%#v, %v)", result, protocolErr)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("exact source budget HTTP calls = %d, want 1", got)
	}

	overSources := append([]string(nil), exactSources...)
	overSources[len(overSources)-1] += "x"
	result, protocolErr = handler(context.Background(), extractRequest(multiExtractArguments(overSources...)))
	if protocolErr != nil || !result.IsError || !strings.Contains(toolResultText(t, result), "budget") {
		t.Fatalf("source budget plus one = (%#v, %v)", result, protocolErr)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("source budget plus one made HTTP call; total = %d", got)
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

func TestHandleExtractDataMultiNon2xxUsesStableRedactedError(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "aggregate error envelope with source summaries",
			body: `{"success":false,"sources":[{"url":"https://one.example.com","success":false,"status":"fetch_failed","tokens":{"original_estimate":0,"cleaned_estimate":0,"savings_percent":0},"timing":{"total_ms":1,"navigation_ms":0,"cleaning_ms":0,"extraction_ms":0},"error":{"code":"NAVIGATION_FAILED","message":"source fetch failed"}}],"tokens":{"original_estimate":0,"cleaned_estimate":0,"savings_percent":0},"timing":{"total_ms":1,"navigation_ms":0,"cleaning_ms":0,"extraction_ms":0},"usage_complete":true,"error":{"code":"NO_VALID_SOURCE","message":"no valid extraction source"}}`,
			want: "[NO_VALID_SOURCE] no valid extraction source",
		},
		{
			name: "malformed envelope falls back to status",
			body: `{"error":`,
			want: "extraction failed (HTTP 502)",
		},
		{
			name: "known credentials are redacted",
			body: `{"error":{"code":"LLM_FAILURE","message":"Purify purify-api-secret and BYOK caller-llm-secret failed"}}`,
			want: "[LLM_FAILURE] Purify [REDACTED] and BYOK [REDACTED] failed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return httpResponse(http.StatusBadGateway, []byte(test.body)), nil
			})}
			handler := handleExtractDataWithClient(client, "http://purify.test", "purify-api-secret")
			arguments := multiExtractArgumentsWithMany(map[string]any{
				"engine": "llm", "llm_api_key": "caller-llm-secret",
			}, "https://one.example.com")

			result, protocolErr := handler(context.Background(), extractRequest(arguments))
			if protocolErr != nil || !result.IsError {
				t.Fatalf("result = %#v, protocol error = %v", result, protocolErr)
			}
			if got := toolResultText(t, result); got != test.want {
				t.Fatalf("tool error = %q, want %q", got, test.want)
			}
		})
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

func TestExtractProductionClientRejectsMultiSourceRedirectWithoutLeakingCredentials(t *testing.T) {
	t.Parallel()

	var sourceCalls atomic.Int32
	var redirectCalls atomic.Int32
	var leakedCredential atomic.Bool
	client := newExtractHTTPClient()
	client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Host {
		case "purify.test":
			sourceCalls.Add(1)
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Errorf("read source body: %v", err)
			}
			if request.Header.Get("X-API-Key") != "purify-api-secret" || bytes.Contains(body, []byte("purify-api-secret")) {
				t.Errorf("Purify credential boundary violated: header=%q body=%s", request.Header.Get("X-API-Key"), body)
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
				leakedCredential.Store(true)
			}
			return httpResponse(http.StatusOK, validMultiExtractResponseBody(t, models.MultiExtractStatusComplete)), nil
		default:
			t.Fatalf("unexpected redirect host %q", request.URL.Host)
			return nil, nil
		}
	})
	handler := handleExtractDataWithClient(client, "http://purify.test", "purify-api-secret")

	result, protocolErr := handler(context.Background(), extractRequest(multiExtractArgumentsWithMany(map[string]any{
		"engine": "llm", "llm_api_key": "caller-llm-secret",
	}, "https://one.example.com")))
	if protocolErr != nil || !result.IsError {
		t.Fatalf("result = %#v, protocol error = %v", result, protocolErr)
	}
	if got, want := toolResultText(t, result), "extraction failed (HTTP 307)"; got != want {
		t.Fatalf("tool error = %q, want %q", got, want)
	}
	if sourceCalls.Load() != 1 || redirectCalls.Load() != 0 || leakedCredential.Load() {
		t.Fatalf("redirect calls/source calls/leak = %d/%d/%t", redirectCalls.Load(), sourceCalls.Load(), leakedCredential.Load())
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

func TestAPIPostResponseAcceptsExactResponseBodyLimit(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		body := io.NopCloser(io.LimitReader(zeroReader{}, maxAPIResponseBytes))
		return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header)}, nil
	})}

	response, err := apiPostResponse(context.Background(), client, "http://purify.test", "secret-key", "/api/v1/extract", map[string]string{"url": "https://example.com"})
	if err != nil {
		t.Fatalf("exact response body limit: %v", err)
	}
	if response.StatusCode != http.StatusOK || int64(len(response.Body)) != maxAPIResponseBytes {
		t.Fatalf("response status/bytes = %d/%d, want %d/%d", response.StatusCode, len(response.Body), http.StatusOK, maxAPIResponseBytes)
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

func searchRequest(arguments map[string]any) mcp.CallToolRequest {
	return mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: arguments}}
}

func answerRequest(arguments map[string]any) mcp.CallToolRequest {
	return mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: arguments}}
}

func validKnownAnswerResponse() models.AnswerResponse {
	asOf := time.Date(2026, time.August, 10, 1, 2, 3, 0, time.UTC)
	firstURL := "https://a.example.com/price"
	secondURL := "https://b.example.org/price"
	return models.AnswerResponse{
		Status: models.AnswerStatusKnown,
		Belief: &models.AnswerBelief{
			Value:      json.RawMessage(`"$19"`),
			Confidence: models.AnswerConfidenceMedium,
			Agreement:  models.MultiExtractAgreement{Pages: 2, IndependentRoots: 2},
			AsOf:       asOf,
			Evidence: []models.AnswerEvidence{
				{
					URL: firstURL, Root: "example.com", Quote: "$19", TextRange: [2]int{0, 3},
					Method: evidence.MethodExact, SnapshotID: "sha256:" + strings.Repeat("a", 64), FetchedAt: asOf.Add(-time.Minute),
				},
				{
					URL: secondURL, Root: "example.org", Quote: "$19", TextRange: [2]int{4, 7},
					Selector: ".price", Method: evidence.MethodCompiled, SnapshotID: "sha256:" + strings.Repeat("b", 64), FetchedAt: asOf,
				},
			},
			Receipts: map[string]string{firstURL: "receipt-one", secondURL: "receipt-two"},
		},
		Lease: &models.AnswerLease{
			ExpiresAt:          asOf.Add(time.Duration(models.DefaultAnswerLeaseSeconds) * time.Second),
			RenewURL:           models.DefaultAnswerRenewURL,
			ConfidenceHalflife: models.DefaultAnswerConfidenceHalflifeSeconds,
		},
	}
}

func validUnknownAnswerResponse(reason models.AnswerUnknownReason) models.AnswerResponse {
	response := models.AnswerResponse{
		Status: models.AnswerStatusUnknown,
		Belief: nil,
		Reason: reason,
		Needs:  &models.AnswerNeeds{MoreIndependentSources: models.DefaultAnswerMinIndependentSources},
	}
	switch reason {
	case models.AnswerUnknownInsufficient:
		response.Closest = &models.AnswerClosest{
			Value: json.RawMessage(`"$19"`), IndependentRoots: 1, Note: "insufficient independent roots",
		}
		response.Needs.MoreIndependentSources = 1
	case models.AnswerUnknownConflict:
		response.Closest = &models.AnswerClosest{
			Value: json.RawMessage(`"$19"`), IndependentRoots: 2, Note: "independent-root tie",
		}
		response.Needs.MoreIndependentSources = 1
		response.Conflicts = []models.AnswerCandidate{{
			Value: json.RawMessage(`"$20"`), Agreement: models.MultiExtractAgreement{Pages: 2, IndependentRoots: 2},
		}}
	}
	return response
}

func mutateAnswerResponseBody(t *testing.T, response models.AnswerResponse, mutate func(map[string]any)) []byte {
	t.Helper()
	body := mustJSON(t, response)
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	mutate(document)
	return mustJSON(t, document)
}

func answerResponseBodyAtSize(t *testing.T, size int) []byte {
	t.Helper()
	body := mustJSON(t, validUnknownAnswerResponse(models.AnswerUnknownNoSearchResults))
	if size < len(body) {
		t.Fatalf("response size %d is smaller than fixture %d", size, len(body))
	}
	body = append(body, bytes.Repeat([]byte(" "), size-len(body))...)
	if len(body) != size {
		t.Fatalf("answer response bytes = %d, want %d", len(body), size)
	}
	return body
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func validSearchResponse() models.SearchResponse {
	score := 0.91
	publishedAt := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	return models.SearchResponse{
		Success: true,
		Query:   "purify search",
		Results: []models.SearchResult{{
			Rank:               1,
			Score:              &score,
			Title:              "Purify Search",
			URL:                "https://example.com/article",
			Snippet:            "Provider-neutral verified search",
			PublishedAt:        &publishedAt,
			VerificationStatus: models.SearchVerificationNotChecked,
		}},
		Timing: models.SearchTimingInfo{TotalMs: 12, ProviderMs: 8, EnrichmentMs: 0},
	}
}

func validSearchResponseBody(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(validSearchResponse())
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func searchResponseWith(t *testing.T, mutate func(*models.SearchResponse)) models.SearchResponse {
	t.Helper()
	response := validSearchResponse()
	mutate(&response)
	return response
}

func searchResponseWithNullData(t *testing.T) models.SearchResponse {
	t.Helper()
	response := validSearchResponse()
	basis := models.EvidenceBasis{}
	receipts := models.FieldReceipts{}
	unlocatedRate := 0.0
	response.Results[0].Data = json.RawMessage(`null`)
	response.Results[0].Basis = &basis
	response.Results[0].Receipts = &receipts
	response.Results[0].UnlocatedRate = &unlocatedRate
	return response
}

func validSearchErrorBody(t *testing.T, code, message string) []byte {
	t.Helper()
	body, err := json.Marshal(models.SearchResponse{
		Success: false,
		Results: []models.SearchResult{},
		Error:   &models.ErrorDetail{Code: code, Message: message},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func mutateSearchResponseBody(t *testing.T, response models.SearchResponse, mutate func(map[string]any)) []byte {
	t.Helper()
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	mutate(document)
	body, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func searchResultDocument(t *testing.T, document map[string]any, index int) map[string]any {
	t.Helper()
	results, ok := document["results"].([]any)
	if !ok || index < 0 || index >= len(results) {
		t.Fatalf("invalid results fixture: %#v", document["results"])
	}
	result, ok := results[index].(map[string]any)
	if !ok {
		t.Fatalf("invalid result fixture: %#v", results[index])
	}
	return result
}

func searchResponseBodyAtSize(t *testing.T, size int) []byte {
	t.Helper()
	if size < 1 {
		t.Fatalf("invalid response size %d", size)
	}
	response := validSearchResponse()
	response.Results[0].Content = "x"
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	padding := size - len(body)
	if padding < 0 {
		t.Fatalf("response size %d is smaller than fixture %d", size, len(body))
	}
	response.Results[0].Content += strings.Repeat("x", padding)
	body, err = json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != size {
		t.Fatalf("response bytes = %d, want %d", len(body), size)
	}
	return body
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

func multiExtractArguments(sources ...string) map[string]any {
	values := make([]any, len(sources))
	for index := range sources {
		values[index] = sources[index]
	}
	return map[string]any{
		"sources": values,
		"schema":  validExtractSchemaJSON,
	}
}

func multiExtractArgumentsWithMany(values map[string]any, sources ...string) map[string]any {
	arguments := multiExtractArguments(sources...)
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

func validMultiExtractResponseBody(t *testing.T, status models.MultiExtractStatus) []byte {
	t.Helper()
	body, err := json.Marshal(validMultiExtractResponse(status))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func validMultiExtractResponse(status models.MultiExtractStatus) models.MultiExtractResponse {
	fetchedAt := time.Date(2026, time.August, 10, 1, 2, 3, 0, time.UTC)
	support := models.MultiExtractSupport{
		URL:  "https://one.example.com/product",
		Root: "example.com",
		Evidence: &evidence.Anchor{
			Quote:      "Purify",
			TextRange:  [2]int{0, len("Purify")},
			Method:     evidence.MethodExact,
			SnapshotID: "snap-one",
			FetchedAt:  fetchedAt,
		},
		Receipt: "receipt-one",
	}
	response := models.MultiExtractResponse{
		Success: true,
		Status:  status,
		Consensus: &models.MultiExtractConsensus{Fields: map[string]models.MultiExtractFieldConsensus{
			"/name": {
				Value:     json.RawMessage(`"Purify"`),
				Agreement: models.MultiExtractAgreement{Pages: 1, IndependentRoots: 1},
				Supports:  []models.MultiExtractSupport{support},
			},
		}},
		Sources: []models.MultiExtractSource{{
			URL:        "https://one.example.com/product",
			FinalURL:   "https://one.example.com/product",
			Success:    true,
			Status:     models.MultiExtractSourceStatusValid,
			SnapshotID: "snap-one",
			Tokens:     models.TokenInfo{OriginalEstimate: 100, CleanedEstimate: 20, SavingsPercent: 80},
			Timing:     models.ExtractTimingInfo{TotalMs: 9, NavigationMs: 6, CleaningMs: 2, ExtractionMs: 1},
		}},
		Tokens:        models.TokenInfo{OriginalEstimate: 100, CleanedEstimate: 20, SavingsPercent: 80},
		Timing:        models.ExtractTimingInfo{TotalMs: 9, NavigationMs: 6, CleaningMs: 2, ExtractionMs: 1},
		UsageComplete: true,
	}
	switch status {
	case models.MultiExtractStatusComplete:
		response.Data = json.RawMessage(`{"name":"Purify"}`)
	case models.MultiExtractStatusSchemaInvalid:
		response.Violations = []models.SchemaViolation{{Path: "/name", Message: "must be a number"}}
	case models.MultiExtractStatusAmbiguous:
		// Aggregate materialization may be ambiguous because of object presence,
		// node kind, or array length even when no scalar field is ambiguous.
	}
	return response
}

func validFieldAmbiguousMultiExtractResponse() models.MultiExtractResponse {
	response := validMultiExtractResponse(models.MultiExtractStatusAmbiguous)
	fetchedAt := time.Date(2026, time.August, 10, 1, 2, 4, 0, time.UTC)
	secondSupport := models.MultiExtractSupport{
		URL:  "https://two.example.net/product",
		Root: "example.net",
		Evidence: &evidence.Anchor{
			Quote:      "Other",
			TextRange:  [2]int{0, len("Other")},
			Method:     evidence.MethodNormalized,
			SnapshotID: "snap-two",
			FetchedAt:  fetchedAt,
		},
		Receipt: "receipt-two",
	}
	firstSupport := response.Consensus.Fields["/name"].Supports[0]
	response.Consensus.Fields["/name"] = models.MultiExtractFieldConsensus{
		Ambiguous: true,
		Conflicts: []models.MultiExtractConflict{
			{Value: json.RawMessage(`"Purify"`), Agreement: models.MultiExtractAgreement{Pages: 1, IndependentRoots: 1}, Supports: []models.MultiExtractSupport{firstSupport}},
			{Value: json.RawMessage(`"Other"`), Agreement: models.MultiExtractAgreement{Pages: 1, IndependentRoots: 1}, Supports: []models.MultiExtractSupport{secondSupport}},
		},
	}
	response.Sources = append(response.Sources, models.MultiExtractSource{
		URL:        "https://two.example.net/product",
		FinalURL:   "https://two.example.net/product",
		Success:    true,
		Status:     models.MultiExtractSourceStatusValid,
		SnapshotID: "snap-two",
		Tokens:     models.TokenInfo{OriginalEstimate: 90, CleanedEstimate: 18, SavingsPercent: 80},
		Timing:     models.ExtractTimingInfo{TotalMs: 8, NavigationMs: 5, CleaningMs: 2, ExtractionMs: 1},
	})
	response.Tokens = models.TokenInfo{OriginalEstimate: 190, CleanedEstimate: 38, SavingsPercent: 80}
	return response
}

func validNonAmbiguousConflictMultiExtractResponse() models.MultiExtractResponse {
	response := validFieldAmbiguousMultiExtractResponse()
	field := response.Consensus.Fields["/name"]
	firstSupport := field.Conflicts[0].Supports[0]
	secondSupport := field.Conflicts[1].Supports[0]
	fetchedAt := time.Date(2026, time.August, 10, 1, 2, 5, 0, time.UTC)
	thirdSupport := models.MultiExtractSupport{
		URL:  "https://three.example.org/product",
		Root: "example.org",
		Evidence: &evidence.Anchor{
			Quote:      "Other",
			TextRange:  [2]int{0, len("Other")},
			Method:     evidence.MethodFuzzy,
			SnapshotID: "snap-three",
			FetchedAt:  fetchedAt,
		},
		Receipt: "receipt-three",
	}
	response.Consensus.Fields["/name"] = models.MultiExtractFieldConsensus{
		Value:     json.RawMessage(`"Purify"`),
		Agreement: models.MultiExtractAgreement{Pages: 2, IndependentRoots: 2},
		Supports:  []models.MultiExtractSupport{firstSupport, secondSupport},
		Conflicts: []models.MultiExtractConflict{{
			Value:     json.RawMessage(`"Other"`),
			Agreement: models.MultiExtractAgreement{Pages: 1, IndependentRoots: 1},
			Supports:  []models.MultiExtractSupport{thirdSupport},
		}},
	}
	response.Status = models.MultiExtractStatusComplete
	response.Data = json.RawMessage(`{"name":"Purify"}`)
	response.Sources = append(response.Sources, models.MultiExtractSource{
		URL:        "https://three.example.org/product",
		FinalURL:   "https://three.example.org/product",
		Success:    true,
		Status:     models.MultiExtractSourceStatusValid,
		SnapshotID: "snap-three",
		Tokens:     models.TokenInfo{OriginalEstimate: 80, CleanedEstimate: 16, SavingsPercent: 80},
		Timing:     models.ExtractTimingInfo{TotalMs: 7, NavigationMs: 4, CleaningMs: 2, ExtractionMs: 1},
	})
	response.Tokens = models.TokenInfo{OriginalEstimate: 270, CleanedEstimate: 54, SavingsPercent: 80}
	return response
}

func mutateMultiExtractResponseBody(t *testing.T, response models.MultiExtractResponse, mutate func(map[string]any)) []byte {
	t.Helper()
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	mutate(document)
	body, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func multiSourceDocument(t *testing.T, document map[string]any, index int) map[string]any {
	t.Helper()
	sources, ok := document["sources"].([]any)
	if !ok || index < 0 || index >= len(sources) {
		t.Fatalf("invalid sources fixture: %#v", document["sources"])
	}
	source, ok := sources[index].(map[string]any)
	if !ok {
		t.Fatalf("invalid source fixture: %#v", sources[index])
	}
	return source
}

func multiConsensusFieldDocument(t *testing.T, document map[string]any, path string) map[string]any {
	t.Helper()
	consensus, ok := document["consensus"].(map[string]any)
	if !ok {
		t.Fatalf("invalid consensus fixture: %#v", document["consensus"])
	}
	fields, ok := consensus["fields"].(map[string]any)
	if !ok {
		t.Fatalf("invalid consensus fields fixture: %#v", consensus["fields"])
	}
	field, ok := fields[path].(map[string]any)
	if !ok {
		t.Fatalf("invalid consensus field fixture: %#v", fields[path])
	}
	return field
}

func cloneJSONMap(t *testing.T, input map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var output map[string]any
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatal(err)
	}
	return output
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
