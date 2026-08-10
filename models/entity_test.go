package models

import (
	"encoding/json"
	"testing"
)

// The entity attribution literals are wire values consumed by extract and
// answer clients; changing one is a public-contract break.
func TestEntityAttributionContract(t *testing.T) {
	if EntityVerdictMatch != "entity_match" ||
		EntityVerdictMismatch != "entity_mismatch" ||
		EntityVerdictUncertain != "entity_uncertain" {
		t.Fatal("entity verdict literals must stay stable")
	}
	if MultiExtractSourceStatusEntityMismatch != "entity_mismatch" {
		t.Fatal("the entity-mismatch source status literal must stay stable")
	}
	if AnswerUnknownEntityMismatch != "entity_mismatch" {
		t.Fatal("the entity-mismatch unknown reason literal must stay stable")
	}
	if ErrCodeEntityMismatch != "ENTITY_MISMATCH" {
		t.Fatal("the entity-mismatch error code literal must stay stable")
	}
}

// New wire fields must stay additive and optional: an attribution-free
// payload round-trips without them, and a populated one keeps every field.
func TestEntityAttributionEncoding(t *testing.T) {
	bare, err := json.Marshal(MultiExtractSource{URL: "https://example.com/", Status: MultiExtractSourceStatusValid})
	if err != nil {
		t.Fatalf("marshal bare source: %v", err)
	}
	for _, absent := range []string{"entity", "expected_subject", "entity_verdict"} {
		if string(bare) != "" && json.Valid(bare) && containsField(t, bare, absent) {
			t.Fatalf("bare source summary must omit %q, got %s", absent, bare)
		}
	}

	request := ExtractRequest{
		Sources:         []string{"https://example.com/"},
		Schema:          json.RawMessage(`{"price":"string"}`),
		ExpectedSubject: &SubjectSpec{Name: "Apple Inc.", Hint: "stock price"},
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var decoded ExtractRequest
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if decoded.ExpectedSubject == nil || decoded.ExpectedSubject.Name != "Apple Inc." ||
		decoded.ExpectedSubject.Hint != "stock price" {
		t.Fatalf("expected subject must round-trip, got %#v", decoded.ExpectedSubject)
	}

	source := MultiExtractSource{
		URL:    "https://example.com/",
		Status: MultiExtractSourceStatusEntityMismatch,
		Entity: &EntityAttribution{
			Verdict: EntityVerdictMismatch,
			Name:    "Apple Bank",
			Kind:    "organization",
			Quote:   "Apple Bank was founded in 1863.",
			Tier:    "referee",
		},
	}
	encoded, err = json.Marshal(source)
	if err != nil {
		t.Fatalf("marshal attributed source: %v", err)
	}
	var decodedSource MultiExtractSource
	if err := json.Unmarshal(encoded, &decodedSource); err != nil {
		t.Fatalf("unmarshal attributed source: %v", err)
	}
	if decodedSource.Entity == nil || *decodedSource.Entity != *source.Entity {
		t.Fatalf("entity attribution must round-trip, got %#v", decodedSource.Entity)
	}
}

func containsField(t *testing.T, encoded []byte, field string) bool {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	_, ok := payload[field]
	return ok
}
