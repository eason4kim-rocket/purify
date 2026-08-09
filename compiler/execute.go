package compiler

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/cascadia"
	"github.com/use-agent/purify/evidence"
)

type compiledRule struct {
	rule       FieldRule
	selector   cascadia.Selector
	pattern    *regexp.Regexp
	transforms []namedTransform
	valueType  string
}

// Execute parses html once and evaluates every field in ir against that one
// goquery document. Optional fields that do not match are omitted. Any invalid
// rule, conversion failure, or missing required field fails the execution
// atomically.
func Execute(ir IR, html string) (json.RawMessage, map[string]evidence.Anchor, error) {
	return executeWithOutputLimit(ir, html, MaxOutputBytes)
}

// ReplayResult is the exact result of evaluating one immutable field rule.
// Found is false only when the selector, attribute, or regular expression did
// not match. Value and Anchor are populated together for a successful replay.
type ReplayResult struct {
	Value  json.RawMessage
	Anchor evidence.Anchor
	Found  bool
}

// ReplayField evaluates one named rule with the same selector, attribute,
// regular-expression, transform, type-conversion, and SingleMatcher semantics
// as Execute. Unlike Execute, a missing required field is reported as
// Found=false so a verifier can distinguish a genuinely gone field from a
// corrupt or otherwise non-executable revision.
func ReplayField(ir IR, fieldName, html string) (ReplayResult, error) {
	if err := validateExecutionResources(ir, html); err != nil {
		return ReplayResult{}, err
	}
	rules, err := compileRules(ir.Fields)
	if err != nil {
		return ReplayResult{}, err
	}

	var selected *compiledRule
	for index := range rules {
		if rules[index].rule.Name == fieldName {
			selected = &rules[index]
			break
		}
	}
	if selected == nil {
		return ReplayResult{}, &FieldError{
			Field: fieldName,
			Stage: "name",
			Err:   errors.New("field is absent from the extraction revision"),
		}
	}

	document, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return ReplayResult{}, fmt.Errorf("compiler: parse HTML: %w", err)
	}
	// Missing is a fact-level state during replay even when the production rule
	// is required. All other rule behavior remains identical.
	replayRule := *selected
	replayRule.rule.Required = false
	value, quote, found, err := executeRule(document, replayRule)
	if err != nil {
		return ReplayResult{}, err
	}
	if !found {
		return ReplayResult{Found: false}, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ReplayResult{}, fmt.Errorf("compiler: encode replayed field %q: %w", fieldName, err)
	}
	if len(encoded) > MaxOutputBytes {
		return ReplayResult{}, fmt.Errorf("%w: replay output is %d bytes, maximum is %d", ErrResourceLimit, len(encoded), MaxOutputBytes)
	}
	return ReplayResult{
		Value: append(json.RawMessage(nil), encoded...),
		Anchor: evidence.Anchor{
			Quote:    quote,
			Selector: selected.rule.Selector,
			Method:   evidence.MethodCompiled,
		},
		Found: true,
	}, nil
}

func executeWithOutputLimit(ir IR, html string, outputLimit int) (json.RawMessage, map[string]evidence.Anchor, error) {
	if err := validateExecutionResources(ir, html); err != nil {
		return nil, nil, err
	}
	output, err := newOutputObjectBudget(outputLimit)
	if err != nil {
		return nil, nil, err
	}

	rules, err := compileRules(ir.Fields)
	if err != nil {
		return nil, nil, err
	}

	document, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, nil, fmt.Errorf("compiler: parse HTML: %w", err)
	}

	data := make(map[string]any, len(rules))
	anchors := make(map[string]evidence.Anchor, len(rules))
	for fieldIndex, rule := range rules {
		value, quote, found, err := executeRule(document, rule)
		if err != nil {
			return nil, nil, err
		}
		if !found {
			continue
		}

		encodedName, err := json.Marshal(rule.rule.Name)
		if err != nil {
			return nil, nil, fmt.Errorf("compiler: encode field %d name: %w", fieldIndex, err)
		}
		encodedValue, err := json.Marshal(value)
		if err != nil {
			return nil, nil, fmt.Errorf("compiler: encode field %d value: %w", fieldIndex, err)
		}
		if err := output.addField(encodedName, encodedValue); err != nil {
			return nil, nil, err
		}

		data[rule.rule.Name] = value
		// Execute has no snapshot observation context. The compiled-extract
		// integration must hydrate TextRange, SnapshotID, and FetchedAt before
		// issuing evidence or receipts. Attr/regex/transform replay also belongs
		// to that integration; this P2-1 anchor records only the rule selector and
		// the post-regex, pre-transform quote without inventing provenance.
		anchors[rule.rule.Name] = evidence.Anchor{
			Quote:    quote,
			Selector: rule.rule.Selector,
			Method:   evidence.MethodCompiled,
		}
	}

	encoded, err := json.Marshal(data)
	if err != nil {
		return nil, nil, fmt.Errorf("compiler: encode result: %w", err)
	}
	if len(encoded) > outputLimit {
		return nil, nil, fmt.Errorf("%w: output JSON is %d bytes, maximum is %d", ErrResourceLimit, len(encoded), outputLimit)
	}
	if len(encoded) != output.used {
		return nil, nil, fmt.Errorf("compiler: output size accounting mismatch: counted %d bytes, encoded %d", output.used, len(encoded))
	}
	return json.RawMessage(encoded), anchors, nil
}

type outputObjectBudget struct {
	maximum int
	used    int
	fields  int
}

func newOutputObjectBudget(maximum int) (outputObjectBudget, error) {
	const emptyObjectBytes = 2
	if maximum < emptyObjectBytes {
		return outputObjectBudget{}, fmt.Errorf("%w: output JSON maximum %d cannot hold an empty object", ErrResourceLimit, maximum)
	}
	return outputObjectBudget{maximum: maximum, used: emptyObjectBytes}, nil
}

func (budget *outputObjectBudget) addField(encodedName, encodedValue []byte) error {
	if budget == nil || budget.maximum < 0 || budget.used < 0 || budget.used > budget.maximum {
		return fmt.Errorf("%w: invalid output JSON budget", ErrResourceLimit)
	}

	// Braces are included in used at initialization. Every entry adds a colon,
	// and every entry after the first also adds one comma. Subtraction-based
	// accounting avoids overflowing int even if this helper is misused later.
	fixedBytes := 1
	if budget.fields > 0 {
		fixedBytes++
	}
	remaining := budget.maximum - budget.used
	if len(encodedName) > remaining {
		return fmt.Errorf("%w: output JSON exceeds %d bytes", ErrResourceLimit, budget.maximum)
	}
	remaining -= len(encodedName)
	if fixedBytes > remaining {
		return fmt.Errorf("%w: output JSON exceeds %d bytes", ErrResourceLimit, budget.maximum)
	}
	remaining -= fixedBytes
	if len(encodedValue) > remaining {
		return fmt.Errorf("%w: output JSON exceeds %d bytes", ErrResourceLimit, budget.maximum)
	}
	remaining -= len(encodedValue)

	budget.used = budget.maximum - remaining
	budget.fields++
	return nil
}

func validateExecutionResources(ir IR, html string) error {
	if ir.Version != CurrentIRVersion {
		return fmt.Errorf("%w: got %d, want %d", ErrUnsupportedIRVersion, ir.Version, CurrentIRVersion)
	}
	if len(html) > MaxHTMLBytes {
		return fmt.Errorf("%w: HTML is %d bytes, maximum is %d", ErrResourceLimit, len(html), MaxHTMLBytes)
	}
	if len(ir.Fields) > MaxFields {
		return fmt.Errorf("%w: field count is %d, maximum is %d", ErrResourceLimit, len(ir.Fields), MaxFields)
	}

	definitionBytes := 0
	addDefinitionBytes := func(size int) error {
		if size > MaxRuleDefinitionBytes-definitionBytes {
			return fmt.Errorf("%w: rule definitions exceed %d bytes", ErrResourceLimit, MaxRuleDefinitionBytes)
		}
		definitionBytes += size
		return nil
	}

	for fieldIndex, rule := range ir.Fields {
		if err := checkFieldBytes(fieldIndex, "name", len(rule.Name), MaxFieldNameBytes); err != nil {
			return err
		}
		if err := checkFieldBytes(fieldIndex, "selector", len(rule.Selector), MaxSelectorBytes); err != nil {
			return err
		}
		if err := checkFieldBytes(fieldIndex, "attribute", len(rule.Attr), MaxAttributeBytes); err != nil {
			return err
		}
		if err := checkFieldBytes(fieldIndex, "regular expression", len(rule.Regex), MaxRegexBytes); err != nil {
			return err
		}
		if len(rule.Transforms) > MaxTransformsPerField {
			return fmt.Errorf("%w: field %d has %d transforms, maximum is %d", ErrResourceLimit, fieldIndex, len(rule.Transforms), MaxTransformsPerField)
		}

		for transformIndex, name := range rule.Transforms {
			if len(name) > MaxTransformNameBytes {
				return fmt.Errorf("%w: field %d transform %d name is %d bytes, maximum is %d", ErrResourceLimit, fieldIndex, transformIndex, len(name), MaxTransformNameBytes)
			}
		}

		if err := addDefinitionBytes(len(rule.Name)); err != nil {
			return err
		}
		if err := addDefinitionBytes(len(rule.Selector)); err != nil {
			return err
		}
		if err := addDefinitionBytes(len(rule.Attr)); err != nil {
			return err
		}
		if err := addDefinitionBytes(len(rule.Regex)); err != nil {
			return err
		}
		if err := addDefinitionBytes(len(rule.Type)); err != nil {
			return err
		}
		for _, name := range rule.Transforms {
			if err := addDefinitionBytes(len(name)); err != nil {
				return err
			}
		}
	}

	encodedRules, err := json.Marshal(ir.Fields)
	if err != nil {
		return fmt.Errorf("compiler: encode rule definitions: %w", err)
	}
	if len(encodedRules) > MaxRuleDefinitionBytes {
		return fmt.Errorf("%w: encoded rule definitions are %d bytes, maximum is %d", ErrResourceLimit, len(encodedRules), MaxRuleDefinitionBytes)
	}
	return nil
}

func checkFieldBytes(fieldIndex int, kind string, size, maximum int) error {
	if size <= maximum {
		return nil
	}
	return fmt.Errorf("%w: field %d %s is %d bytes, maximum is %d", ErrResourceLimit, fieldIndex, kind, size, maximum)
}

func compileRules(fields []FieldRule) ([]compiledRule, error) {
	compiled := make([]compiledRule, 0, len(fields))
	names := make(map[string]struct{}, len(fields))
	for _, rule := range fields {
		if strings.TrimSpace(rule.Name) == "" {
			return nil, &FieldError{Field: rule.Name, Stage: "name", Err: errors.New("name is required")}
		}
		if _, exists := names[rule.Name]; exists {
			return nil, &FieldError{Field: rule.Name, Stage: "name", Err: errors.New("duplicate field name")}
		}
		names[rule.Name] = struct{}{}

		if strings.TrimSpace(rule.Selector) == "" {
			return nil, &FieldError{Field: rule.Name, Stage: "selector", Err: errors.New("selector is required")}
		}
		selector, err := cascadia.Compile(rule.Selector)
		if err != nil {
			return nil, &FieldError{Field: rule.Name, Stage: "selector", Err: err}
		}

		var pattern *regexp.Regexp
		if rule.Regex != "" {
			pattern, err = regexp.Compile(rule.Regex)
			if err != nil {
				return nil, &FieldError{Field: rule.Name, Stage: "regex", Err: err}
			}
			if pattern.NumSubexp() < 1 {
				return nil, &FieldError{Field: rule.Name, Stage: "regex", Err: errors.New("capture group 1 is required")}
			}
		}

		transforms := make([]namedTransform, 0, len(rule.Transforms))
		for _, name := range rule.Transforms {
			transform, ok := transformRegistry[name]
			if !ok {
				return nil, &FieldError{Field: rule.Name, Stage: "transform", Err: fmt.Errorf("unknown transform %q", name)}
			}
			transforms = append(transforms, namedTransform{name: name, apply: transform})
		}

		valueType := strings.ToLower(strings.TrimSpace(rule.Type))
		if valueType == "" {
			valueType = TypeString
		}
		switch valueType {
		case TypeString, TypeNumber, TypeBoolean, TypeDate:
		default:
			return nil, &FieldError{Field: rule.Name, Stage: "type", Err: fmt.Errorf("unsupported type %q", rule.Type)}
		}

		compiled = append(compiled, compiledRule{
			rule:       rule,
			selector:   selector,
			pattern:    pattern,
			transforms: transforms,
			valueType:  valueType,
		})
	}
	return compiled, nil
}

func executeRule(document *goquery.Document, rule compiledRule) (any, string, bool, error) {
	selection := document.FindMatcher(goquery.SingleMatcher(rule.selector))
	if selection.Length() == 0 {
		return missingRule(rule, fmt.Sprintf("selector %q did not match", rule.rule.Selector))
	}

	raw := selection.Text()
	if rule.rule.Attr != "" {
		var ok bool
		raw, ok = selection.Attr(rule.rule.Attr)
		if !ok {
			return missingRule(rule, fmt.Sprintf("attribute %q is missing", rule.rule.Attr))
		}
	}

	if rule.pattern != nil {
		matches := rule.pattern.FindStringSubmatch(raw)
		if matches == nil {
			return missingRule(rule, "regular expression did not match")
		}
		raw = matches[1]
	}
	quote := raw
	if len(quote) > MaxAnchorQuoteBytes {
		return nil, "", false, &FieldError{
			Field: rule.rule.Name,
			Stage: "anchor quote",
			Err:   fmt.Errorf("%w: quote is %d bytes, maximum is %d", ErrResourceLimit, len(quote), MaxAnchorQuoteBytes),
		}
	}
	// Regex captures and trimmed attribute/text values can be short slices of a
	// multi-megabyte backing string. Clone the bounded quote so retaining it in
	// data or anchors cannot retain one full selection allocation per field.
	quote = strings.Clone(quote)

	transformed := quote
	for _, transform := range rule.transforms {
		var err error
		transformed, err = transform.apply(transformed)
		if err != nil {
			return nil, "", false, &FieldError{
				Field: rule.rule.Name,
				Stage: "transform " + transform.name,
				Err:   err,
			}
		}
	}

	value, err := convertValue(transformed, rule.valueType)
	if err != nil {
		return nil, "", false, &FieldError{Field: rule.rule.Name, Stage: "type " + rule.valueType, Err: err}
	}
	return value, quote, true, nil
}

func missingRule(rule compiledRule, reason string) (any, string, bool, error) {
	if !rule.rule.Required {
		return nil, "", false, nil
	}
	return nil, "", false, &FieldError{
		Field: rule.rule.Name,
		Stage: "required",
		Err:   fmt.Errorf("%w: %s", ErrRequiredField, reason),
	}
}
