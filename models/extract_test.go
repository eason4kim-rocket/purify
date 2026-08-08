package models

import (
	"encoding/json"
	"strings"
	"testing"

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
	encoded, err := json.Marshal(ExtractResponse{Success: true, Data: json.RawMessage(`{"count":3}`)})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, field := range []string{`"partial"`, `"violations"`, `"snapshot_id"`, `"unlocated_rate"`, `"basis"`} {
		if strings.Contains(string(encoded), field) {
			t.Fatalf("response %s unexpectedly contains %s", encoded, field)
		}
	}
}

func TestExtractResponseEvidenceIncludesZeroUnlocatedRate(t *testing.T) {
	zero := 0.0
	encoded, err := json.Marshal(ExtractResponse{
		Success:       true,
		Data:          json.RawMessage(`{"name":"Ada"}`),
		SnapshotID:    "sha256:abc",
		UnlocatedRate: &zero,
		Basis: map[string]evidence.Anchor{
			"name": {Quote: "Ada", TextRange: [2]int{0, 3}, Method: evidence.MethodExact, SnapshotID: "sha256:abc"},
		},
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
