// Package compiler executes deterministic, reusable extraction rules against
// HTML documents.
package compiler

import (
	"errors"
	"fmt"
)

const (
	// CurrentIRVersion is the only extraction program version Execute accepts.
	CurrentIRVersion = 1

	TypeString  = "string"
	TypeNumber  = "number"
	TypeBoolean = "boolean"
	TypeDate    = "date"

	MaxHTMLBytes           = 4 << 20
	MaxFields              = 100
	MaxFieldNameBytes      = 4 << 10
	MaxSelectorBytes       = 4 << 10
	MaxAttributeBytes      = 1 << 10
	MaxRegexBytes          = 4 << 10
	MaxTransformsPerField  = 16
	MaxTransformNameBytes  = 128
	MaxRuleDefinitionBytes = 512 << 10
	MaxAnchorQuoteBytes    = 8 << 10
	MaxOutputBytes         = 4 << 20
)

var (
	// ErrRequiredField is wrapped when a required rule cannot extract a value.
	ErrRequiredField = errors.New("compiler: required field missing")
	// ErrUnsupportedIRVersion is wrapped when Execute cannot safely interpret
	// the supplied extraction program version.
	ErrUnsupportedIRVersion = errors.New("compiler: unsupported IR version")
	// ErrResourceLimit is wrapped by every input, rule, and output size limit.
	ErrResourceLimit = errors.New("compiler: resource limit exceeded")
)

// FieldRule describes how to extract and convert one output field. Selector
// uses CSS syntax. Attr being empty selects element text. Regex, when set,
// contributes capture group 1 rather than the complete match.
type FieldRule struct {
	Name       string   `json:"name"`
	Selector   string   `json:"selector"`
	Attr       string   `json:"attr,omitempty"`
	Regex      string   `json:"regex,omitempty"`
	Transforms []string `json:"transforms,omitempty"`
	Type       string   `json:"type"`
	Required   bool     `json:"required,omitempty"`
}

// IR is a versioned deterministic extraction program.
type IR struct {
	Version int         `json:"version"`
	Fields  []FieldRule `json:"fields"`
}

// FieldError identifies the rule and processing stage that failed.
type FieldError struct {
	Field string
	Stage string
	Err   error
}

func (e *FieldError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Stage == "" {
		return fmt.Sprintf("compiler field %q: %v", e.Field, e.Err)
	}
	return fmt.Sprintf("compiler field %q (%s): %v", e.Field, e.Stage, e.Err)
}

func (e *FieldError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}
