package compiler

import "encoding/json"

// ValidateCompileSchema reports whether schema can be served by the
// deterministic compiler. It performs no extraction and makes no LLM calls.
func ValidateCompileSchema(schema json.RawMessage) error {
	_, _, err := parseCompileSchema(schema)
	return err
}
