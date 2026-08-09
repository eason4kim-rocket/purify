package models

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/use-agent/purify/evidence"
)

func TestExtractResponsePartialContract(t *testing.T) {
	encoded, err := json.Marshal(ExtractResponse{
		Success: true,
		Data:    json.RawMessage(`{"count":"three"}`),
		Partial: true,
		Violations: []SchemaViolation{
			{Path: "/count", Message: "expected integer"},
		},
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, fragment := range []string{`"partial":true`, `"violations":[`, `"path":"/count"`} {
		if !strings.Contains(string(encoded), fragment) {
			t.Fatalf("response %s missing %s", encoded, fragment)
		}
	}
}

func TestExtractResponseOmitsNewFieldsWhenUnused(t *testing.T) {
	response := ExtractResponse{
		Success:  true,
		Data:     json.RawMessage(`{"count":3}`),
		Metadata: Metadata{Title: "Example", SourceURL: "https://example.com"},
		Tokens:   TokenInfo{OriginalEstimate: 10, CleanedEstimate: 5, SavingsPercent: 50},
		Timing:   ExtractTimingInfo{TotalMs: 3, NavigationMs: 1, CleaningMs: 1, ExtractionMs: 1},
		LLMUsage: &LLMUsage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3},
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, field := range []string{`"partial"`, `"violations"`, `"snapshot_id"`, `"unlocated_rate"`, `"basis"`, `"receipts"`, `"extractor"`} {
		if strings.Contains(string(encoded), field) {
			t.Fatalf("response %s unexpectedly contains %s", encoded, field)
		}
	}

	// This mirrors the public response before Phase 0 added optional fields.
	legacy, err := json.Marshal(struct {
		Success  bool              `json:"success"`
		Data     json.RawMessage   `json:"data,omitempty"`
		Metadata Metadata          `json:"metadata"`
		Tokens   TokenInfo         `json:"tokens"`
		Timing   ExtractTimingInfo `json:"timing"`
		LLMUsage *LLMUsage         `json:"llm_usage,omitempty"`
		Error    *ErrorDetail      `json:"error,omitempty"`
	}{
		Success:  response.Success,
		Data:     response.Data,
		Metadata: response.Metadata,
		Tokens:   response.Tokens,
		Timing:   response.Timing,
		LLMUsage: response.LLMUsage,
		Error:    response.Error,
	})
	if err != nil {
		t.Fatalf("legacy Marshal() error = %v", err)
	}
	if !bytes.Equal(encoded, legacy) {
		t.Fatalf("response changed with evidence disabled:\nnew:    %s\nlegacy: %s", encoded, legacy)
	}
}

func TestExtractResponseEvidenceIncludesZeroUnlocatedRate(t *testing.T) {
	zero := 0.0
	basis := EvidenceBasis{
		"name": {Quote: "Ada", TextRange: [2]int{0, 3}, Method: evidence.MethodExact, SnapshotID: "sha256:abc"},
	}
	encoded, err := json.Marshal(ExtractResponse{
		Success:       true,
		Data:          json.RawMessage(`{"name":"Ada"}`),
		SnapshotID:    "sha256:abc",
		UnlocatedRate: &zero,
		Basis:         &basis,
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, fragment := range []string{`"snapshot_id":"sha256:abc"`, `"unlocated_rate":0`, `"basis":{"name"`} {
		if !strings.Contains(string(encoded), fragment) {
			t.Fatalf("response %s missing %s", encoded, fragment)
		}
	}
}

func TestExtractResponseEvidenceKeepsEmptyObjects(t *testing.T) {
	zero := 0.0
	basis := EvidenceBasis{}
	receipts := FieldReceipts{}
	encoded, err := json.Marshal(ExtractResponse{
		Success:       true,
		UnlocatedRate: &zero,
		Basis:         &basis,
		Receipts:      &receipts,
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	for _, field := range []string{"basis", "receipts"} {
		if string(document[field]) != `{}` {
			t.Fatalf("%s = %s, want empty object", field, document[field])
		}
	}
}

func TestExtractResponseCompiledExtractorContract(t *testing.T) {
	compiledAt := time.Date(2026, time.August, 9, 8, 0, 0, 0, time.UTC)
	encoded, err := json.Marshal(ExtractResponse{
		Success: true,
		Extractor: &ExtractorMetadata{
			ID:         "123e4567-e89b-12d3-a456-426614174000",
			Version:    3,
			CompiledAt: compiledAt,
			Validation: 0.97,
			Mode:       "compiled",
		},
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	var extractor map[string]any
	if err := json.Unmarshal(document["extractor"], &extractor); err != nil {
		t.Fatalf("extractor decode error = %v", err)
	}
	if extractor["id"] != "123e4567-e89b-12d3-a456-426614174000" || extractor["version"] != float64(3) || extractor["validation"] != 0.97 || extractor["mode"] != "compiled" || extractor["compiled_at"] != compiledAt.Format(time.RFC3339) {
		t.Fatalf("extractor = %#v", extractor)
	}
}

func TestExtractRequestDefaultsEngineToAuto(t *testing.T) {
	request := ExtractRequest{}
	request.Defaults()
	if request.Engine != "auto" {
		t.Fatalf("Engine = %q, want auto", request.Engine)
	}
}
