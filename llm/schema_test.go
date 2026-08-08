package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateAgainstSchemaViolationMatrix(t *testing.T) {
	schema := json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["name", "status", "items"],
  "properties": {
    "name": {"type": "string"},
    "status": {"enum": ["active", "paused"]},
    "items": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["quantity"],
        "properties": {"quantity": {"type": "number"}},
        "additionalProperties": false
      }
    }
  }
}`)

	tests := []struct {
		name         string
		data         string
		pathContains string
		messagePart  string
	}{
		{name: "wrong type", data: `{"name":12,"status":"active","items":[]}`, pathContains: "/name", messagePart: "string"},
		{name: "missing required", data: `{"status":"active","items":[]}`, pathContains: "$", messagePart: "name"},
		{name: "enum out of range", data: `{"name":"x","status":"deleted","items":[]}`, pathContains: "/status", messagePart: "one of"},
		{name: "nested array error", data: `{"name":"x","status":"active","items":[{"quantity":"many"}]}`, pathContains: "/items/0/quantity", messagePart: "number"},
		{name: "additional property", data: `{"name":"x","status":"active","items":[],"secret":true}`, pathContains: "$", messagePart: "additional"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			violations, err := ValidateAgainstSchema(schema, json.RawMessage(tt.data))
			if err != nil {
				t.Fatalf("ValidateAgainstSchema() error = %v", err)
			}
			if len(violations) == 0 {
				t.Fatal("expected at least one violation")
			}
			var matched bool
			for _, violation := range violations {
				if strings.Contains(violation.Path, tt.pathContains) && strings.Contains(strings.ToLower(violation.Message), strings.ToLower(tt.messagePart)) {
					matched = true
					break
				}
			}
			if !matched {
				t.Fatalf("violations = %#v, want path containing %q and message containing %q", violations, tt.pathContains, tt.messagePart)
			}
		})
	}
}

func TestValidateAgainstSchemaValid(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"],"additionalProperties":false}`)
	violations, err := ValidateAgainstSchema(schema, json.RawMessage(`{"count":3}`))
	if err != nil {
		t.Fatalf("ValidateAgainstSchema() error = %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %#v, want none", violations)
	}
}

func TestValidateSchemaRejectsExternalReference(t *testing.T) {
	err := ValidateSchema(json.RawMessage(`{"$ref":"https://example.com/schema.json"}`))
	if err == nil || !strings.Contains(err.Error(), "external schema reference") {
		t.Fatalf("ValidateSchema() error = %v, want external-reference rejection", err)
	}
}

func TestNormalizeSchemaPreservesLegacyShorthand(t *testing.T) {
	normalized, err := NormalizeSchema(json.RawMessage(`{"name":"string","price":"number","features":["string"]}`))
	if err != nil {
		t.Fatalf("NormalizeSchema() error = %v", err)
	}
	valid := json.RawMessage(`{"name":"Ada","price":29.99,"features":["fast"]}`)
	violations, err := ValidateAgainstSchema(normalized, valid)
	if err != nil || len(violations) != 0 {
		t.Fatalf("legacy normalized validation: violations=%#v err=%v schema=%s", violations, err, normalized)
	}
	nullable := json.RawMessage(`{"name":null,"price":null,"features":null}`)
	violations, err = ValidateAgainstSchema(normalized, nullable)
	if err != nil || len(violations) != 0 {
		t.Fatalf("legacy nullable validation: violations=%#v err=%v schema=%s", violations, err, normalized)
	}
}

func TestNormalizeSchemaDoesNotMistakeFieldNamedTypeForJSONSchema(t *testing.T) {
	normalized, err := NormalizeSchema(json.RawMessage(`{"type":"string","name":"string"}`))
	if err != nil {
		t.Fatalf("NormalizeSchema() error = %v", err)
	}
	violations, err := ValidateAgainstSchema(normalized, json.RawMessage(`{"type":"widget","name":"A"}`))
	if err != nil || len(violations) != 0 {
		t.Fatalf("field named type lost: violations=%#v err=%v schema=%s", violations, err, normalized)
	}
}
