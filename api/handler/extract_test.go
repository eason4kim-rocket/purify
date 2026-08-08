package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/use-agent/purify/evidence"
	"github.com/use-agent/purify/llm"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/receipts"
)

type fakeStructuredExtractor struct {
	initial     json.RawMessage
	repaired    json.RawMessage
	extracts    int
	repairs     int
	initialUse  *models.LLMUsage
	repairedUse *models.LLMUsage
}

func (f *fakeStructuredExtractor) Extract(context.Context, string, json.RawMessage, llm.ExtractParams) (*llm.ExtractResult, error) {
	f.extracts++
	return &llm.ExtractResult{Data: f.initial, Usage: f.initialUse}, nil
}

func (f *fakeStructuredExtractor) ExtractWithRepair(context.Context, string, json.RawMessage, json.RawMessage, []llm.Violation, llm.ExtractParams) (*llm.ExtractResult, error) {
	f.repairs++
	return &llm.ExtractResult{Data: f.repaired, Usage: f.repairedUse}, nil
}

func TestExtractWithValidationDoesNotRepairValidOutput(t *testing.T) {
	client := &fakeStructuredExtractor{initial: json.RawMessage(`{"count":3}`)}
	result, violations, err := extractWithValidation(context.Background(), client, "content", numberSchema(), llm.ExtractParams{})
	if err != nil {
		t.Fatalf("extractWithValidation() error = %v", err)
	}
	if string(result.Data) != `{"count":3}` || len(violations) != 0 {
		t.Fatalf("result = %s, violations = %#v", result.Data, violations)
	}
	if client.extracts != 1 || client.repairs != 0 {
		t.Fatalf("calls = extract:%d repair:%d, want 1/0", client.extracts, client.repairs)
	}
}

func TestExtractWithValidationRepairsAtMostOnce(t *testing.T) {
	client := &fakeStructuredExtractor{
		initial:     json.RawMessage(`{"count":"three"}`),
		repaired:    json.RawMessage(`{"count":3}`),
		initialUse:  &models.LLMUsage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3},
		repairedUse: &models.LLMUsage{PromptTokens: 4, CompletionTokens: 2, TotalTokens: 6},
	}
	result, violations, err := extractWithValidation(context.Background(), client, "content", numberSchema(), llm.ExtractParams{})
	if err != nil {
		t.Fatalf("extractWithValidation() error = %v", err)
	}
	if len(violations) != 0 || string(result.Data) != `{"count":3}` {
		t.Fatalf("result = %s, violations = %#v", result.Data, violations)
	}
	if client.extracts != 1 || client.repairs != 1 {
		t.Fatalf("calls = extract:%d repair:%d, want 1/1", client.extracts, client.repairs)
	}
	if result.Usage == nil || result.Usage.TotalTokens != 9 {
		t.Fatalf("usage = %#v, want accumulated total 9", result.Usage)
	}
}

func TestExtractWithValidationReturnsPartialAfterFailedRepair(t *testing.T) {
	client := &fakeStructuredExtractor{
		initial:  json.RawMessage(`{"count":"three"}`),
		repaired: json.RawMessage(`{"count":"still three"}`),
	}
	result, violations, err := extractWithValidation(context.Background(), client, "content", numberSchema(), llm.ExtractParams{})
	if err != nil {
		t.Fatalf("extractWithValidation() error = %v", err)
	}
	if result == nil || len(violations) == 0 {
		t.Fatalf("result = %#v, violations = %#v, want partial data and violations", result, violations)
	}
	if client.extracts != 1 || client.repairs != 1 {
		t.Fatalf("calls = extract:%d repair:%d, want bounded 1/1", client.extracts, client.repairs)
	}
}

func numberSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"],"additionalProperties":false}`)
}

type recordingReceiptSigner struct {
	payloads []receipts.Payload
}

func (signer *recordingReceiptSigner) Sign(payload receipts.Payload) (string, error) {
	signer.payloads = append(signer.payloads, payload)
	return "signed:" + payload.Path, nil
}

func TestSignFieldReceiptsPreservesLeafClaims(t *testing.T) {
	fetchedAt := time.Date(2026, time.August, 9, 8, 0, 0, 0, time.UTC)
	issuedAt := fetchedAt.Add(time.Minute)
	basis := models.EvidenceBasis{
		"active": {Quote: "true", Method: evidence.MethodExact, SnapshotID: "sha256:abc", FetchedAt: fetchedAt},
		"name":   {Quote: "Ada", Method: evidence.MethodExact, SnapshotID: "sha256:abc", FetchedAt: fetchedAt},
		"price":  {Quote: "$29.99", Method: evidence.MethodNormalized, SnapshotID: "sha256:abc", FetchedAt: fetchedAt},
	}
	signer := &recordingReceiptSigner{}
	tokens, err := signFieldReceipts(
		json.RawMessage(`{"price":29.99,"active":true,"name":"Ada"}`),
		basis,
		"https://example.com/final",
		issuedAt,
		signer,
	)
	if err != nil {
		t.Fatalf("signFieldReceipts() error = %v", err)
	}
	for _, path := range []string{"active", "name", "price"} {
		if tokens[path] != "signed:"+path {
			t.Fatalf("token %q = %q", path, tokens[path])
		}
	}
	if len(signer.payloads) != 3 {
		t.Fatalf("signed payloads = %d, want 3", len(signer.payloads))
	}
	wantValues := map[string]string{"active": "true", "name": `"Ada"`, "price": "29.99"}
	for index, payload := range signer.payloads {
		if index > 0 && signer.payloads[index-1].Path > payload.Path {
			t.Fatalf("payload order is not deterministic: %#v", signer.payloads)
		}
		if payload.URL != "https://example.com/final" || !payload.IssuedAt.Equal(issuedAt) || string(payload.Value) != wantValues[payload.Path] || payload.Anchor != basis[payload.Path] {
			t.Fatalf("payload %q = %#v", payload.Path, payload)
		}
	}
}

func TestSignFieldReceiptsRejectsPathMismatch(t *testing.T) {
	_, err := signFieldReceipts(
		json.RawMessage(`{"name":"Ada"}`),
		models.EvidenceBasis{"other": {Method: evidence.MethodUnlocated}},
		"https://example.com",
		time.Now(),
		&recordingReceiptSigner{},
	)
	if err == nil {
		t.Fatal("signFieldReceipts() accepted mismatched evidence paths")
	}
	if got := fmt.Sprint(err); got == "" {
		t.Fatal("signFieldReceipts() returned an empty error")
	}
}

func TestEvidenceUnavailableMapsToServiceUnavailable(t *testing.T) {
	errorValue := models.NewScrapeError(models.ErrCodeEvidenceUnavailable, "snapshot storage disabled", nil)
	if got := mapExtractErrorToStatus(errorValue); got != http.StatusServiceUnavailable {
		t.Fatalf("mapExtractErrorToStatus() = %d, want %d", got, http.StatusServiceUnavailable)
	}
}
