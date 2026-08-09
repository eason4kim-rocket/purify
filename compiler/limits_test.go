package compiler

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/use-agent/purify/evidence"
)

func TestExecuteRejectsUnsupportedIRVersionsAtomically(t *testing.T) {
	for _, version := range []int{0, -1, CurrentIRVersion + 1} {
		t.Run(fmt.Sprintf("version_%d", version), func(t *testing.T) {
			data, anchors, err := executeWithoutPanic(t, IR{Version: version}, "")
			if !errors.Is(err, ErrUnsupportedIRVersion) {
				t.Fatalf("error = %v, want ErrUnsupportedIRVersion", err)
			}
			if data != nil || anchors != nil {
				t.Fatalf("failure returned partial output: data=%s anchors=%v", data, anchors)
			}
		})
	}
}

func TestExecutionResourceBoundaries(t *testing.T) {
	t.Run("HTML", func(t *testing.T) {
		ir := IR{Version: CurrentIRVersion}
		if err := validateExecutionResources(ir, strings.Repeat("x", MaxHTMLBytes)); err != nil {
			t.Fatalf("limit-sized HTML: %v", err)
		}
		if err := validateExecutionResources(ir, strings.Repeat("x", MaxHTMLBytes+1)); !errors.Is(err, ErrResourceLimit) {
			t.Fatalf("over-limit HTML error = %v, want ErrResourceLimit", err)
		}
	})

	t.Run("fields", func(t *testing.T) {
		atLimit := IR{Version: CurrentIRVersion, Fields: make([]FieldRule, MaxFields)}
		assertValidationBoundary(t, atLimit, IR{Version: CurrentIRVersion, Fields: make([]FieldRule, MaxFields+1)})
	})

	t.Run("field name", func(t *testing.T) {
		assertFieldStringBoundary(t, func(rule *FieldRule, size int) {
			rule.Name = strings.Repeat("n", size)
		}, MaxFieldNameBytes)
	})

	t.Run("selector", func(t *testing.T) {
		assertFieldStringBoundary(t, func(rule *FieldRule, size int) {
			rule.Selector = strings.Repeat("s", size)
		}, MaxSelectorBytes)
	})

	t.Run("attribute", func(t *testing.T) {
		assertFieldStringBoundary(t, func(rule *FieldRule, size int) {
			rule.Attr = strings.Repeat("a", size)
		}, MaxAttributeBytes)
	})

	t.Run("regular expression", func(t *testing.T) {
		assertFieldStringBoundary(t, func(rule *FieldRule, size int) {
			rule.Regex = strings.Repeat("r", size)
		}, MaxRegexBytes)
	})

	t.Run("transform count", func(t *testing.T) {
		atLimit := oneRuleIR()
		atLimit.Fields[0].Transforms = make([]string, MaxTransformsPerField)
		overLimit := oneRuleIR()
		overLimit.Fields[0].Transforms = make([]string, MaxTransformsPerField+1)
		assertValidationBoundary(t, atLimit, overLimit)
	})

	t.Run("transform name", func(t *testing.T) {
		atLimit := oneRuleIR()
		atLimit.Fields[0].Transforms = []string{strings.Repeat("t", MaxTransformNameBytes)}
		overLimit := oneRuleIR()
		overLimit.Fields[0].Transforms = []string{strings.Repeat("t", MaxTransformNameBytes+1)}
		assertValidationBoundary(t, atLimit, overLimit)
	})

	t.Run("total rule definition", func(t *testing.T) {
		fields := fieldsWithEncodedSize(t, MaxRuleDefinitionBytes)
		atLimit := IR{Version: CurrentIRVersion, Fields: fields}
		if err := validateExecutionResources(atLimit, ""); err != nil {
			t.Fatalf("limit-sized rule definition: %v", err)
		}

		overLimit := cloneIR(atLimit)
		grew := false
		for index := range overLimit.Fields {
			if len(overLimit.Fields[index].Name) < MaxFieldNameBytes {
				overLimit.Fields[index].Name += "n"
				grew = true
				break
			}
		}
		if !grew {
			t.Fatal("could not grow exact-sized rule definition")
		}
		if err := validateExecutionResources(overLimit, ""); !errors.Is(err, ErrResourceLimit) {
			t.Fatalf("over-limit rule definition error = %v, want ErrResourceLimit", err)
		}
	})
}

func TestExecuteRejectsResourceViolationsAtomicallyWithoutPanicking(t *testing.T) {
	totalOver := IR{Version: CurrentIRVersion, Fields: fieldsWithEncodedSize(t, MaxRuleDefinitionBytes)}
	for index := range totalOver.Fields {
		if len(totalOver.Fields[index].Name) < MaxFieldNameBytes {
			totalOver.Fields[index].Name += "n"
			break
		}
	}

	tests := []struct {
		name string
		ir   IR
		html string
	}{
		{name: "HTML", ir: IR{Version: CurrentIRVersion, Fields: []FieldRule{{Name: "x", Selector: "div:not("}}}, html: strings.Repeat("x", MaxHTMLBytes+1)},
		{name: "fields", ir: IR{Version: CurrentIRVersion, Fields: make([]FieldRule, MaxFields+1)}},
		{name: "field name", ir: IR{Version: CurrentIRVersion, Fields: []FieldRule{{Name: strings.Repeat("n", MaxFieldNameBytes+1), Selector: ".x"}}}},
		{name: "selector", ir: IR{Version: CurrentIRVersion, Fields: []FieldRule{{Name: "x", Selector: strings.Repeat("s", MaxSelectorBytes+1)}}}},
		{name: "attribute", ir: IR{Version: CurrentIRVersion, Fields: []FieldRule{{Name: "x", Selector: ".x", Attr: strings.Repeat("a", MaxAttributeBytes+1)}}}},
		{name: "regular expression", ir: IR{Version: CurrentIRVersion, Fields: []FieldRule{{Name: "x", Selector: ".x", Regex: strings.Repeat("(", MaxRegexBytes+1)}}}},
		{name: "transform count", ir: IR{Version: CurrentIRVersion, Fields: []FieldRule{{Name: "x", Selector: ".x", Transforms: make([]string, MaxTransformsPerField+1)}}}},
		{name: "transform name", ir: IR{Version: CurrentIRVersion, Fields: []FieldRule{{Name: "x", Selector: ".x", Transforms: []string{strings.Repeat("t", MaxTransformNameBytes+1)}}}}},
		{name: "total rule definition", ir: totalOver},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data, anchors, err := executeWithoutPanic(t, test.ir, test.html)
			if !errors.Is(err, ErrResourceLimit) {
				t.Fatalf("error = %v, want ErrResourceLimit", err)
			}
			if data != nil || anchors != nil {
				t.Fatalf("failure returned partial output: data=%s anchors=%v", data, anchors)
			}
		})
	}
}

func TestExecuteAnchorQuoteBoundary(t *testing.T) {
	ir := IR{Version: CurrentIRVersion, Fields: []FieldRule{{
		Name:     "value",
		Selector: "p",
		Type:     TypeString,
		Required: true,
	}}}

	atLimitHTML := "<p>" + strings.Repeat("x", MaxAnchorQuoteBytes) + "</p>"
	data, anchors, err := executeWithoutPanic(t, ir, atLimitHTML)
	if err != nil {
		t.Fatalf("limit-sized quote: %v", err)
	}
	if len(data) == 0 || len(anchors) != 1 || len(anchors["value"].Quote) != MaxAnchorQuoteBytes {
		t.Fatalf("quote boundary result: data=%d bytes anchors=%#v", len(data), anchors)
	}

	overLimitHTML := "<p>" + strings.Repeat("x", MaxAnchorQuoteBytes+1) + "</p>"
	data, anchors, err = executeWithoutPanic(t, ir, overLimitHTML)
	if !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("over-limit quote error = %v, want ErrResourceLimit", err)
	}
	if data != nil || anchors != nil {
		t.Fatalf("quote failure returned partial output: data=%d bytes anchors=%v", len(data), anchors)
	}
}

func TestExecuteRejectsLargeCollapsibleQuoteOnFirstField(t *testing.T) {
	const prefix = "<body>"
	const suffix = "</body>"
	html := prefix + strings.Repeat(" ", MaxHTMLBytes-len(prefix)-len(suffix)) + suffix
	fields := make([]FieldRule, MaxFields)
	for index := range fields {
		fields[index] = FieldRule{
			Name:       fmt.Sprintf("field_%03d", index),
			Selector:   "body",
			Transforms: []string{"collapse_ws"},
			Type:       TypeString,
		}
	}

	data, anchors, err := executeWithoutPanic(t, IR{Version: CurrentIRVersion, Fields: fields}, html)
	if !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("error = %v, want ErrResourceLimit", err)
	}
	var fieldErr *FieldError
	if !errors.As(err, &fieldErr) || fieldErr.Field != "field_000" || fieldErr.Stage != "anchor quote" {
		t.Fatalf("error = %#v, want first-field anchor quote error", err)
	}
	if data != nil || anchors != nil {
		t.Fatalf("quote failure returned partial output: data=%d bytes anchors=%v", len(data), anchors)
	}
}

func TestExecuteStopsAtCumulativeOutputLimit(t *testing.T) {
	const countingTransform = "test_count_output_budget"
	oldTransform, existed := transformRegistry[countingTransform]
	defer func() {
		if existed {
			transformRegistry[countingTransform] = oldTransform
		} else {
			delete(transformRegistry, countingTransform)
		}
	}()

	calls := 0
	transformRegistry[countingTransform] = func(value string) (string, error) {
		calls++
		return value, nil
	}
	value := strings.Repeat("m", 1024)
	fields := make([]FieldRule, 4)
	var html strings.Builder
	for index := range fields {
		name := fmt.Sprintf("field_%d", index)
		selector := fmt.Sprintf("#field-%d", index)
		fields[index] = FieldRule{
			Name:       name,
			Selector:   selector,
			Transforms: []string{countingTransform},
			Type:       TypeString,
		}
		fmt.Fprintf(&html, `<p id="field-%d">%s</p>`, index, value)
	}

	// Set the test seam to the exact encoded size of the first two fields. The
	// third field is evaluated to learn its value, then rejected before map
	// insertion; the fourth field must never run.
	outputLimit := encodedObjectSize(t, fields[:2], value)
	data, anchors, err := executeWithOutputLimit(IR{Version: CurrentIRVersion, Fields: fields}, html.String(), outputLimit)
	if !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("error = %v, want ErrResourceLimit", err)
	}
	if calls != 3 {
		t.Fatalf("transform calls = %d, want 3 (stop at overflowing field)", calls)
	}
	if data != nil || anchors != nil {
		t.Fatalf("output failure returned partial output: data=%d bytes anchors=%v", len(data), anchors)
	}
}

func TestOutputObjectBudgetBoundary(t *testing.T) {
	encodedName, err := json.Marshal("value")
	if err != nil {
		t.Fatal(err)
	}
	encodedValueBytes := MaxOutputBytes - 2 - 1 - len(encodedName)
	encodedValue := []byte(`"` + strings.Repeat("x", encodedValueBytes-2) + `"`)

	atLimit, err := newOutputObjectBudget(MaxOutputBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := atLimit.addField(encodedName, encodedValue); err != nil {
		t.Fatalf("limit-sized output entry: %v", err)
	}
	if atLimit.used != MaxOutputBytes {
		t.Fatalf("counted output = %d bytes, want %d", atLimit.used, MaxOutputBytes)
	}

	overLimit, err := newOutputObjectBudget(MaxOutputBytes)
	if err != nil {
		t.Fatal(err)
	}
	encodedValue = []byte(`"` + strings.Repeat("x", encodedValueBytes-1) + `"`)
	if err := overLimit.addField(encodedName, encodedValue); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("over-limit output entry error = %v, want ErrResourceLimit", err)
	}
	if overLimit.used != 2 || overLimit.fields != 0 {
		t.Fatalf("failed reservation mutated budget: %#v", overLimit)
	}
}

func encodedObjectSize(t *testing.T, fields []FieldRule, value string) int {
	t.Helper()
	size := 2
	for index, rule := range fields {
		encodedName, err := json.Marshal(rule.Name)
		if err != nil {
			t.Fatal(err)
		}
		encodedValue, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if index > 0 {
			size++
		}
		size += len(encodedName) + 1 + len(encodedValue)
	}
	return size
}

func assertFieldStringBoundary(t *testing.T, set func(*FieldRule, int), maximum int) {
	t.Helper()
	atLimit := oneRuleIR()
	set(&atLimit.Fields[0], maximum)
	overLimit := oneRuleIR()
	set(&overLimit.Fields[0], maximum+1)
	assertValidationBoundary(t, atLimit, overLimit)
}

func assertValidationBoundary(t *testing.T, atLimit, overLimit IR) {
	t.Helper()
	if err := validateExecutionResources(atLimit, ""); err != nil {
		t.Fatalf("limit-sized input: %v", err)
	}
	if err := validateExecutionResources(overLimit, ""); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("over-limit error = %v, want ErrResourceLimit", err)
	}
}

func oneRuleIR() IR {
	return IR{Version: CurrentIRVersion, Fields: []FieldRule{{Name: "value", Selector: ".missing", Type: TypeString}}}
}

func fieldsWithEncodedSize(t *testing.T, target int) []FieldRule {
	t.Helper()
	fields := make([]FieldRule, MaxFields)
	for index := range fields {
		fields[index] = FieldRule{
			Name:     fmt.Sprintf("field_%03d", index),
			Selector: "x",
			Type:     TypeString,
		}
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	remaining := target - len(encoded)
	if remaining < 0 {
		t.Fatalf("base rule definition is %d bytes, target is %d", len(encoded), target)
	}

	for index := range fields {
		add := min(remaining, MaxSelectorBytes-len(fields[index].Selector))
		fields[index].Selector += strings.Repeat("s", add)
		remaining -= add
	}
	for index := range fields {
		add := min(remaining, MaxFieldNameBytes-len(fields[index].Name))
		fields[index].Name += strings.Repeat("n", add)
		remaining -= add
	}
	if remaining != 0 {
		t.Fatalf("could not construct %d-byte rule definition; %d bytes remain", target, remaining)
	}
	encoded, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != target {
		t.Fatalf("encoded rule definition = %d bytes, want %d", len(encoded), target)
	}
	return fields
}

func cloneIR(ir IR) IR {
	clone := IR{Version: ir.Version, Fields: append([]FieldRule(nil), ir.Fields...)}
	for index := range clone.Fields {
		clone.Fields[index].Transforms = append([]string(nil), clone.Fields[index].Transforms...)
	}
	return clone
}

func executeWithoutPanic(t *testing.T, ir IR, html string) (data json.RawMessage, anchors map[string]evidence.Anchor, err error) {
	t.Helper()
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("Execute panicked: %v", recovered)
		}
	}()
	return Execute(ir, html)
}
