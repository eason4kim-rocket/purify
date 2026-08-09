package compiler_test

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/use-agent/purify/compiler"
	"github.com/use-agent/purify/evidence"
)

func TestExecuteExtractsTypedFieldsAndAnchors(t *testing.T) {
	html, err := os.ReadFile("testdata/product.html")
	if err != nil {
		t.Fatal(err)
	}
	ir := compiler.IR{Version: compiler.CurrentIRVersion, Fields: []compiler.FieldRule{
		{Name: "name", Selector: ".title", Transforms: []string{"trim", "collapse_ws", "lower"}, Type: "string", Required: true},
		{Name: "price", Selector: "#price", Transforms: []string{"currency_amount"}, Type: "number", Required: true},
		{Name: "available", Selector: ".stock", Transforms: []string{"trim", "lower"}, Type: "boolean", Required: true},
		{Name: "released_at", Selector: "time.released", Attr: "datetime", Transforms: []string{"parse_date"}, Type: "date", Required: true},
		{Name: "sku", Selector: "a.product", Attr: "href", Regex: `sku=([A-Z]+-\d+)`, Type: "string", Required: true},
		{Name: "first", Selector: ".duplicate", Type: "string"},
		{Name: "optional", Selector: ".does-not-exist", Type: "string"},
	}}

	data, anchors, err := compiler.Execute(ir, string(html))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	want := map[string]any{
		"name":        "acme widget pro",
		"price":       1299.5,
		"available":   true,
		"released_at": "2026-08-09T06:30:00Z",
		"sku":         "ACME-42",
		"first":       "first",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("result mismatch\n got: %#v\nwant: %#v", got, want)
	}
	if _, ok := anchors["optional"]; ok {
		t.Fatal("optional miss unexpectedly produced an anchor")
	}
	for name, anchor := range anchors {
		if anchor.Method != evidence.MethodCompiled {
			t.Errorf("anchor %q method = %q, want compiled", name, anchor.Method)
		}
		if anchor.Selector == "" {
			t.Errorf("anchor %q has no selector", name)
		}
	}
	if got := anchors["name"].Quote; !strings.Contains(got, "ACME") {
		t.Errorf("name anchor quote = %q", got)
	}
	if got := anchors["sku"].Quote; got != "ACME-42" {
		t.Errorf("sku anchor quote = %q, want ACME-42", got)
	}
}

func TestReplayFieldUsesExactRulePipelineAndFirstMatch(t *testing.T) {
	ir := compiler.IR{Version: compiler.CurrentIRVersion, Fields: []compiler.FieldRule{
		{
			Name:       "released_at",
			Selector:   "time.release",
			Attr:       "datetime",
			Regex:      `^date:(.+)$`,
			Transforms: []string{"trim", "parse_date"},
			Type:       compiler.TypeDate,
			Required:   true,
		},
		{
			Name:       "sku",
			Selector:   "a.product",
			Attr:       "href",
			Regex:      `sku=([A-Z]+-\d+)`,
			Transforms: []string{"lower"},
			Type:       compiler.TypeString,
		},
	}}
	html := `<time class="release" datetime="date: August 9, 2026 14:30 +08:00"></time>` +
		`<a class="product" href="/one?sku=FIRST-1">first</a>` +
		`<a class="product" href="/two?sku=SECOND-2">second</a>`

	released, err := compiler.ReplayField(ir, "released_at", html)
	if err != nil {
		t.Fatalf("ReplayField(date): %v", err)
	}
	if !released.Found || string(released.Value) != `"2026-08-09T06:30:00Z"` ||
		released.Anchor.Quote != " August 9, 2026 14:30 +08:00" ||
		released.Anchor.Selector != "time.release" || released.Anchor.Method != evidence.MethodCompiled {
		t.Fatalf("date replay = %#v", released)
	}

	sku, err := compiler.ReplayField(ir, "sku", html)
	if err != nil {
		t.Fatalf("ReplayField(sku): %v", err)
	}
	if !sku.Found || string(sku.Value) != `"first-1"` || sku.Anchor.Quote != "FIRST-1" {
		t.Fatalf("sku replay = %#v", sku)
	}
}

func TestReplayFieldDistinguishesMissingFromExecutionFailure(t *testing.T) {
	ir := compiler.IR{Version: compiler.CurrentIRVersion, Fields: []compiler.FieldRule{{
		Name: "price", Selector: ".price", Attr: "data-value", Type: compiler.TypeNumber, Required: true,
	}}}

	missing, err := compiler.ReplayField(ir, "price", `<span class="price"></span>`)
	if err != nil || missing.Found || missing.Value != nil {
		t.Fatalf("ReplayField(missing) = (%#v, %v), want clean miss", missing, err)
	}
	if _, err := compiler.ReplayField(ir, "price", `<span class="price" data-value="not-a-number"></span>`); err == nil {
		t.Fatal("ReplayField(conversion failure) succeeded")
	}
	if _, err := compiler.ReplayField(ir, "absent", `<span class="price" data-value="1"></span>`); err == nil {
		t.Fatal("ReplayField(absent revision field) succeeded")
	}
	badIR := ir
	badIR.Fields = append(badIR.Fields, compiler.FieldRule{Name: "bad", Selector: `div:not(`, Type: compiler.TypeString})
	if _, err := compiler.ReplayField(badIR, "price", `<span class="price" data-value="1"></span>`); err == nil {
		t.Fatal("ReplayField(corrupt sibling rule) succeeded")
	}
}

func TestExecuteRequiredMissesReturnFieldError(t *testing.T) {
	tests := []struct {
		name string
		rule compiler.FieldRule
	}{
		{name: "selector", rule: compiler.FieldRule{Name: "title", Selector: ".missing", Type: "string", Required: true}},
		{name: "attribute", rule: compiler.FieldRule{Name: "title", Selector: "h1", Attr: "data-missing", Type: "string", Required: true}},
		{name: "regex", rule: compiler.FieldRule{Name: "title", Selector: "h1", Regex: `(price:\s+\d+)`, Type: "string", Required: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data, anchors, err := compiler.Execute(compiler.IR{Version: compiler.CurrentIRVersion, Fields: []compiler.FieldRule{test.rule}}, `<h1>Hello</h1>`)
			if err == nil {
				t.Fatal("Execute succeeded, want required-field error")
			}
			if data != nil || anchors != nil {
				t.Fatalf("failure returned partial output: data=%s anchors=%v", data, anchors)
			}
			if !errors.Is(err, compiler.ErrRequiredField) {
				t.Fatalf("error = %v, want ErrRequiredField", err)
			}
			var fieldErr *compiler.FieldError
			if !errors.As(err, &fieldErr) || fieldErr.Field != "title" {
				t.Fatalf("error = %#v, want FieldError for title", err)
			}
		})
	}
}

func TestExecuteRejectsMalformedRulesWithoutPanicking(t *testing.T) {
	tests := []struct {
		name string
		rule compiler.FieldRule
	}{
		{name: "malformed selector", rule: compiler.FieldRule{Name: "x", Selector: `div:not(`, Type: "string"}},
		{name: "malformed regex", rule: compiler.FieldRule{Name: "x", Selector: "div", Regex: `([`, Type: "string"}},
		{name: "regex without group one", rule: compiler.FieldRule{Name: "x", Selector: "div", Regex: `\d+`, Type: "string"}},
		{name: "unknown transform", rule: compiler.FieldRule{Name: "x", Selector: "div", Transforms: []string{"panic(now)"}, Type: "string"}},
		{name: "unknown type", rule: compiler.FieldRule{Name: "x", Selector: "div", Type: "interface{}"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf("Execute panicked: %v", recovered)
				}
			}()
			if _, _, err := compiler.Execute(compiler.IR{Version: compiler.CurrentIRVersion, Fields: []compiler.FieldRule{test.rule}}, `<div>42</div>`); err == nil {
				t.Fatal("Execute succeeded, want rule error")
			}
		})
	}
}

func TestExecuteTypeConversionErrorsAreScoped(t *testing.T) {
	tests := []struct {
		name      string
		valueType string
		value     string
	}{
		{name: "number", valueType: "number", value: "not-a-number"},
		{name: "boolean", valueType: "boolean", value: "perhaps"},
		{name: "date", valueType: "date", value: "not-a-date"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ir := compiler.IR{Version: compiler.CurrentIRVersion, Fields: []compiler.FieldRule{{Name: "value", Selector: "p", Type: test.valueType}}}
			_, _, err := compiler.Execute(ir, `<p>`+test.value+`</p>`)
			var fieldErr *compiler.FieldError
			if !errors.As(err, &fieldErr) || fieldErr.Field != "value" {
				t.Fatalf("error = %v, want field-scoped conversion error", err)
			}
		})
	}
}
