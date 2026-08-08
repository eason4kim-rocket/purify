package handler

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/use-agent/purify/llm"
	"github.com/use-agent/purify/models"
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
