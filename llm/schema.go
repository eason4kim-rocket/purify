package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/use-agent/purify/models"
)

// Violation describes one JSON Schema validation failure. It is an alias so
// API models can expose violations without introducing a models -> llm import
// cycle.
type Violation = models.SchemaViolation

var schemaKeywords = map[string]struct{}{
	"$anchor": {}, "$comment": {}, "$defs": {}, "$dynamicAnchor": {}, "$dynamicRef": {}, "$id": {}, "$ref": {}, "$schema": {}, "$vocabulary": {},
	"additionalItems": {}, "additionalProperties": {}, "allOf": {}, "anyOf": {}, "const": {}, "contains": {}, "contentEncoding": {}, "contentMediaType": {}, "contentSchema": {},
	"default": {}, "dependentRequired": {}, "dependentSchemas": {}, "deprecated": {}, "description": {}, "else": {}, "enum": {}, "examples": {}, "exclusiveMaximum": {}, "exclusiveMinimum": {},
	"format": {}, "if": {}, "items": {}, "maxContains": {}, "maximum": {}, "maxItems": {}, "maxLength": {}, "maxProperties": {}, "minContains": {}, "minimum": {}, "minItems": {},
	"minLength": {}, "minProperties": {}, "multipleOf": {}, "not": {}, "oneOf": {}, "pattern": {}, "patternProperties": {}, "prefixItems": {}, "properties": {}, "propertyNames": {},
	"readOnly": {}, "required": {}, "then": {}, "title": {}, "type": {}, "unevaluatedItems": {}, "unevaluatedProperties": {}, "uniqueItems": {}, "writeOnly": {},
}

// NormalizeSchema preserves standards-compliant JSON Schemas and converts the
// legacy Purify shorthand used by the v0.1 API (for example
// {"name":"string","features":["string"]}) into an equivalent strict
// object schema. This keeps existing clients working while giving providers
// and the server a real schema to enforce.
func NormalizeSchema(raw json.RawMessage) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("parse schema: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("parse schema: multiple JSON values")
		}
		return nil, fmt.Errorf("parse schema: %w", err)
	}

	if isJSONSchema(value) {
		compact := new(bytes.Buffer)
		if err := json.Compact(compact, raw); err != nil {
			return nil, fmt.Errorf("compact schema: %w", err)
		}
		return json.RawMessage(compact.Bytes()), nil
	}

	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("legacy schema shorthand must be a JSON object")
	}
	normalized, err := shorthandObject(object, false)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("encode normalized schema: %w", err)
	}
	return json.RawMessage(encoded), nil
}

func isJSONSchema(value any) bool {
	switch schema := value.(type) {
	case bool:
		return true
	case map[string]any:
		if len(schema) == 0 {
			return true
		}
		allKeywords := true
		strongSignal := false
		for key := range schema {
			if _, ok := schemaKeywords[key]; !ok {
				allKeywords = false
				continue
			}
			if key != "type" && key != "title" && key != "description" && key != "default" && key != "examples" {
				strongSignal = true
			}
		}
		if strongSignal && allKeywords {
			return true
		}
		if !allKeywords {
			return false
		}
		if rawType, ok := schema["type"]; ok {
			return validSchemaType(rawType)
		}
		return false
	default:
		return false
	}
}

func validSchemaType(value any) bool {
	valid := func(s string) bool {
		switch s {
		case "null", "boolean", "object", "array", "number", "string", "integer":
			return true
		default:
			return false
		}
	}
	switch typed := value.(type) {
	case string:
		return valid(typed)
	case []any:
		if len(typed) == 0 {
			return false
		}
		for _, item := range typed {
			s, ok := item.(string)
			if !ok || !valid(s) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func shorthandObject(fields map[string]any, nullable bool) (map[string]any, error) {
	properties := make(map[string]any, len(fields))
	required := make([]string, 0, len(fields))
	for name, descriptor := range fields {
		fieldSchema, err := shorthandDescriptor(descriptor)
		if err != nil {
			return nil, fmt.Errorf("legacy schema field %q: %w", name, err)
		}
		properties[name] = fieldSchema
		required = append(required, name)
	}
	sort.Strings(required)
	typeValue := any("object")
	if nullable {
		typeValue = []string{"object", "null"}
	}
	return map[string]any{
		"type":                 typeValue,
		"properties":           properties,
		"required":             required,
		"additionalProperties": false,
	}, nil
}

func shorthandDescriptor(value any) (any, error) {
	switch descriptor := value.(type) {
	case string:
		if !validSchemaType(descriptor) || descriptor == "object" || descriptor == "array" || descriptor == "null" {
			return nil, fmt.Errorf("unsupported type descriptor %q", descriptor)
		}
		return map[string]any{"type": []string{descriptor, "null"}}, nil
	case map[string]any:
		return shorthandObject(descriptor, true)
	case []any:
		if len(descriptor) != 1 {
			return nil, fmt.Errorf("array descriptor must contain exactly one item schema")
		}
		items, err := shorthandDescriptor(descriptor[0])
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": []string{"array", "null"}, "items": items}, nil
	default:
		return nil, fmt.Errorf("unsupported descriptor type %T", value)
	}
}

// ValidateAgainstSchema validates data against a caller-provided JSON Schema.
// Schema references are intentionally restricted to the in-memory document so
// validating an API request cannot trigger filesystem or network access.
func ValidateAgainstSchema(schema, data json.RawMessage) ([]Violation, error) {
	compiled, err := compileSchema(schema)
	if err != nil {
		return nil, err
	}

	instance, err := jsonschema.UnmarshalJSON(strings.NewReader(string(data)))
	if err != nil {
		return nil, fmt.Errorf("parse JSON data: %w", err)
	}
	if err := compiled.Validate(instance); err != nil {
		var validationErr *jsonschema.ValidationError
		if !errors.As(err, &validationErr) {
			return nil, fmt.Errorf("validate JSON data: %w", err)
		}
		return flattenViolations(validationErr), nil
	}

	return nil, nil
}

// ValidateSchema compiles a caller-provided schema without validating an
// instance. Handlers use it before spending an LLM request on an invalid
// schema.
func ValidateSchema(schema json.RawMessage) error {
	_, err := compileSchema(schema)
	return err
}

func compileSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	normalized, err := NormalizeSchema(raw)
	if err != nil {
		return nil, err
	}
	schemaDoc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(normalized)))
	if err != nil {
		return nil, fmt.Errorf("parse JSON schema: %w", err)
	}

	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(rejectExternalSchemaLoader{})
	const schemaURL = "purify://request/schema.json"
	if err := compiler.AddResource(schemaURL, schemaDoc); err != nil {
		return nil, fmt.Errorf("register JSON schema: %w", err)
	}
	compiled, err := compiler.Compile(schemaURL)
	if err != nil {
		return nil, fmt.Errorf("compile JSON schema: %w", err)
	}
	return compiled, nil
}

type rejectExternalSchemaLoader struct{}

func (rejectExternalSchemaLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external schema reference is not allowed: %s", url)
}

func flattenViolations(root *jsonschema.ValidationError) []Violation {
	violations := make([]Violation, 0)
	seen := make(map[string]struct{})

	var visit func(*jsonschema.ValidationError)
	visit = func(current *jsonschema.ValidationError) {
		if len(current.Causes) > 0 {
			for _, cause := range current.Causes {
				visit(cause)
			}
			return
		}

		path := instancePath(current.InstanceLocation)
		message := strings.TrimSpace(current.Error())
		key := path + "\x00" + message
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		violations = append(violations, Violation{Path: path, Message: message})
	}
	visit(root)

	sort.Slice(violations, func(i, j int) bool {
		if violations[i].Path == violations[j].Path {
			return violations[i].Message < violations[j].Message
		}
		return violations[i].Path < violations[j].Path
	})
	return violations
}

func instancePath(tokens []string) string {
	if len(tokens) == 0 {
		return "$"
	}
	var b strings.Builder
	for _, token := range tokens {
		b.WriteByte('/')
		b.WriteString(strings.ReplaceAll(strings.ReplaceAll(token, "~", "~0"), "/", "~1"))
	}
	return b.String()
}
