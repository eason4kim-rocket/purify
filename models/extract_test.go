package models

import (
	"encoding/json"
	"strings"
	"testing"
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
	encoded, err := json.Marshal(ExtractResponse{Success: true, Data: json.RawMessage(`{"count":3}`)})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, field := range []string{`"partial"`, `"violations"`} {
		if strings.Contains(string(encoded), field) {
			t.Fatalf("response %s unexpectedly contains %s", encoded, field)
		}
	}
}
