package compiler

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestValidateCompileSchema(t *testing.T) {
	tests := []struct {
		name    string
		schema  json.RawMessage
		wantErr error
	}{
		{
			name:   "flat scalar object",
			schema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"price":{"type":"number"}},"required":["name"]}`),
		},
		{
			name:    "nested object",
			schema:  json.RawMessage(`{"type":"object","properties":{"details":{"type":"object","properties":{"name":{"type":"string"}}}}}`),
			wantErr: ErrUnsupportedSchema,
		},
		{
			name:    "invalid schema",
			schema:  json.RawMessage(`{"type":`),
			wantErr: ErrUnsupportedSchema,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateCompileSchema(test.schema)
			if test.wantErr == nil && err != nil {
				t.Fatalf("ValidateCompileSchema() error = %v", err)
			}
			if test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("ValidateCompileSchema() error = %v, want %v", err, test.wantErr)
			}
		})
	}
}
