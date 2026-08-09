package compiler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

var productCompileSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "title":{"type":"string"},
    "price":{"type":"number"},
    "available":{"type":"boolean"},
    "released_at":{"type":"string","format":"date-time"}
  },
  "required":["title","price","available","released_at"],
  "additionalProperties":false
}`)

func TestCompileLearnsGeneralizedTypedExtractorAndReport(t *testing.T) {
	samples, truths := productCompileFixtures(t)
	extractor := &fixtureTruthExtractor{truths: truths}

	ir, report, err := Compile(context.Background(), samples, productCompileSchema, extractor)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if extractor.calls != len(samples) {
		t.Fatalf("truth calls = %d, want %d", extractor.calls, len(samples))
	}
	if ir.Version != CurrentIRVersion || len(ir.Fields) != 4 {
		t.Fatalf("IR = %#v", ir)
	}

	rules := rulesByName(ir)
	assertStableRule(t, rules["title"], "", TypeString, "collapse_ws")
	assertStableRule(t, rules["price"], "", TypeNumber, "currency_amount")
	assertStableRule(t, rules["available"], "aria-label", TypeBoolean, "lower")
	assertStableRule(t, rules["released_at"], "datetime", TypeDate, "parse_date")
	for name, rule := range rules {
		if strings.Contains(rule.Selector, "#product-") || strings.Contains(rule.Selector, ":nth-of-type(") {
			t.Errorf("rule %q selector = %q, want generalized stable selector", name, rule.Selector)
		}
	}

	for _, sample := range samples {
		got, _, err := Execute(ir, sample.HTML)
		if err != nil {
			t.Fatalf("Execute(compiled) error = %v", err)
		}
		assertJSONEqual(t, got, truths[sample.Content])
	}

	if report.Samples != 3 || report.ValidSamples != 3 || report.Overall != 1 ||
		report.Threshold != ValidationThreshold || !report.CanEnable {
		t.Fatalf("report = %#v", report)
	}
	if got := fieldScores(report); !reflect.DeepEqual(got, map[string]float64{
		"available":   1,
		"price":       1,
		"released_at": 1,
		"title":       1,
	}) {
		t.Fatalf("field scores = %#v", got)
	}
}

func TestCompileIsDeterministicAcrossRunsAndSampleOrder(t *testing.T) {
	samples, truths := productCompileFixtures(t)
	firstIR, firstReport, err := Compile(context.Background(), samples, productCompileSchema, &fixtureTruthExtractor{truths: truths})
	if err != nil {
		t.Fatal(err)
	}

	reversed := append([]Sample(nil), samples...)
	slices.Reverse(reversed)
	secondIR, secondReport, err := Compile(context.Background(), reversed, productCompileSchema, &fixtureTruthExtractor{truths: truths})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(firstIR, secondIR) || !reflect.DeepEqual(firstReport, secondReport) {
		t.Fatalf("non-deterministic compile\nfirst: %#v %#v\nsecond: %#v %#v", firstIR, firstReport, secondIR, secondReport)
	}
}

func TestCompileReusesOneSampleIndexAcrossMaximumFieldsAndLargeContent(t *testing.T) {
	schema, rawHTML, truth, values := indexedFieldFixture(t, MaxFields)
	largeContent := strings.Join(values, " ") + strings.Repeat(" padding", (1<<20)/len(" padding"))
	samples := repeatedSamples(MinCompileSamples, rawHTML, largeContent)
	calls := 0
	extractor := TruthExtractorFunc(func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
		calls++
		return append(json.RawMessage(nil), truth...), nil
	})

	ir, report, err := Compile(context.Background(), samples, schema, extractor)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if calls != MinCompileSamples || len(ir.Fields) != MaxFields || !report.CanEnable || report.ValidSamples != MinCompileSamples {
		t.Fatalf("calls/IR/report = %d/%d/%#v", calls, len(ir.Fields), report)
	}
}

func TestCompileTextIndexExactLookupsAvoidRepeatedScansAndBoundAllocations(t *testing.T) {
	_, rawHTML, _, values := indexedFieldFixture(t, MaxFields)
	_, _, index, err := parseCompileDocument(context.Background(), rawHTML)
	if err != nil {
		t.Fatal(err)
	}
	budget := alignmentBudget{}
	for _, value := range values {
		nodes, err := findCompileTextNodes(context.Background(), index, value, &budget)
		if err != nil || len(nodes) != 1 {
			t.Fatalf("lookup %q = %d nodes, error %v", value, len(nodes), err)
		}
	}
	if budget.scannedBytes != 0 {
		t.Fatalf("exact indexed lookups scanned %d fallback bytes", budget.scannedBytes)
	}

	allocations := testing.AllocsPerRun(10, func() {
		localBudget := alignmentBudget{}
		for _, value := range values {
			_, _ = findCompileTextNodes(context.Background(), index, value, &localBudget)
		}
	})
	if maximum := float64(len(values)*8 + 32); allocations > maximum {
		t.Fatalf("100 indexed lookups allocated %.0f objects, budget %.0f", allocations, maximum)
	}
}

func TestCompilePrefersStrictNumberTransformWhenCurrencyParsingIsUnneeded(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"],"additionalProperties":false}`)
	samples := make([]Sample, 3)
	truths := make(map[string]json.RawMessage)
	for index, count := range []int{12, 24, 36} {
		content := strconv.Itoa(count)
		samples[index] = Sample{Content: content, HTML: `<span class="count">` + content + `</span>`}
		truths[content] = json.RawMessage(fmt.Sprintf(`{"count":%d}`, count))
	}
	ir, report, err := Compile(context.Background(), samples, schema, &fixtureTruthExtractor{truths: truths})
	if err != nil {
		t.Fatal(err)
	}
	rule := rulesByName(ir)["count"]
	if !slices.Contains(rule.Transforms, "parse_number") || slices.Contains(rule.Transforms, "currency_amount") || !report.CanEnable {
		t.Fatalf("rule/report = %#v %#v", rule, report)
	}
}

func TestCompileLowValidationReturnsIRAndCannotEnable(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}`)
	samples := make([]Sample, 3)
	truths := make(map[string]json.RawMessage)
	for index := range samples {
		content := fmt.Sprintf("Product %d", index)
		className := "title"
		if index == len(samples)-1 {
			className = "alternate-title"
		}
		samples[index] = Sample{Content: content, HTML: fmt.Sprintf(`<h1 class="%s">%s</h1>`, className, content)}
		truths[content] = json.RawMessage(fmt.Sprintf(`{"title":%q}`, content))
	}

	ir, report, err := Compile(context.Background(), samples, schema, &fixtureTruthExtractor{truths: truths})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if len(ir.Fields) != 1 {
		t.Fatalf("IR = %#v", ir)
	}
	if report.ValidSamples != 2 || report.PerField[0].Matches != 2 || report.Overall != 2.0/3.0 || report.CanEnable {
		t.Fatalf("report = %#v", report)
	}
}

func TestCompileRequiresEveryValidationSampleToBeSchemaValid(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}},"required":["title"],"additionalProperties":false}`)
	samples := make([]Sample, 10)
	truths := make(map[string]json.RawMessage)
	for index := range samples {
		content := fmt.Sprintf("Item %02d", index)
		className := "title"
		if index == len(samples)-1 {
			className = "new-title"
		}
		samples[index] = Sample{Content: content, HTML: fmt.Sprintf(`<h1 class="%s">%s</h1>`, className, content)}
		truths[content] = json.RawMessage(fmt.Sprintf(`{"title":%q}`, content))
	}

	_, report, err := Compile(context.Background(), samples, schema, &fixtureTruthExtractor{truths: truths})
	if err != nil {
		t.Fatal(err)
	}
	if report.Overall != ValidationThreshold || report.ValidSamples != 9 || report.CanEnable {
		t.Fatalf("report = %#v, want threshold score but invalid sample gate", report)
	}
}

func TestCompileNullableTruthIsNotLearnedOrEqualToOmission(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"title":{"type":["string","null"]}},"additionalProperties":false}`)
	samples := []Sample{
		{Content: "Alpha", HTML: `<h1 class="title">Alpha</h1>`},
		{Content: "Beta", HTML: `<h1 class="title">Beta</h1>`},
		{Content: "No title", HTML: `<main class="empty">No title</main>`},
	}
	truths := map[string]json.RawMessage{
		"Alpha":    json.RawMessage(`{"title":"Alpha"}`),
		"Beta":     json.RawMessage(`{"title":"Beta"}`),
		"No title": json.RawMessage(`{"title":null}`),
	}

	_, report, err := Compile(context.Background(), samples, schema, &fixtureTruthExtractor{truths: truths})
	if err != nil {
		t.Fatal(err)
	}
	if report.ValidSamples != 3 || report.PerField[0].Matches != 2 || report.Overall != 2.0/3.0 || report.CanEnable {
		t.Fatalf("report = %#v", report)
	}
}

func TestCompileAllNullOrUnlocatedFieldReturnsNoCandidateAtomically(t *testing.T) {
	tests := []struct {
		name    string
		schema  json.RawMessage
		samples []Sample
		truths  map[string]json.RawMessage
	}{
		{
			name:   "all null",
			schema: json.RawMessage(`{"type":"object","properties":{"value":{"type":["string","null"]}},"required":["value"]}`),
			samples: []Sample{
				{Content: "one", HTML: `<p>one</p>`},
				{Content: "two", HTML: `<p>two</p>`},
				{Content: "three", HTML: `<p>three</p>`},
			},
			truths: map[string]json.RawMessage{
				"one": json.RawMessage(`{"value":null}`), "two": json.RawMessage(`{"value":null}`), "three": json.RawMessage(`{"value":null}`),
			},
		},
		{
			name:   "unlocated",
			schema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}`),
			samples: []Sample{
				{Content: "truth one", HTML: `<p>source one</p>`},
				{Content: "truth two", HTML: `<p>source two</p>`},
				{Content: "truth three", HTML: `<p>source three</p>`},
			},
			truths: map[string]json.RawMessage{
				"truth one": json.RawMessage(`{"value":"absent one"}`), "truth two": json.RawMessage(`{"value":"absent two"}`), "truth three": json.RawMessage(`{"value":"absent three"}`),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertCompileErrorWithoutOutput(t, test.samples, test.schema, &fixtureTruthExtractor{truths: test.truths}, ErrNoCandidate)
		})
	}
}

func TestCompileLearnsBoundedDataAttributeAndEscapesHostileSelectors(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"code":{"type":"string"}},"required":["code"],"additionalProperties":false}`)
	samples := make([]Sample, 3)
	truths := make(map[string]json.RawMessage)
	for index := range samples {
		content := fmt.Sprintf("label-%d", index)
		code := fmt.Sprintf("C-%d", index)
		samples[index] = Sample{
			Content: content,
			HTML:    fmt.Sprintf(`<div id="hostile:%d[*]" class="stable-code odd]name" data-code="%s">%s</div>`, index, code, content),
		}
		truths[content] = json.RawMessage(fmt.Sprintf(`{"code":%q}`, code))
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("Compile panicked on hostile selector material: %v", recovered)
		}
	}()
	ir, report, err := Compile(context.Background(), samples, schema, &fixtureTruthExtractor{truths: truths})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	rule := rulesByName(ir)["code"]
	if rule.Attr != "data-code" || strings.Contains(rule.Selector, "hostile") || !report.CanEnable {
		t.Fatalf("rule/report = %#v %#v", rule, report)
	}
	for _, sample := range samples {
		if _, _, err := Execute(ir, sample.HTML); err != nil {
			t.Fatalf("hostile material produced invalid IR: %v", err)
		}
	}
}

func TestCompileRejectsUnsupportedSchemasAtomically(t *testing.T) {
	samples := repeatedSamples(3, `<p class="value">x</p>`, "x")
	extractor := TruthExtractorFunc(func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"value":"x"}`), nil
	})
	tests := []json.RawMessage{
		json.RawMessage(`{"type":"array","items":{"type":"string"}}`),
		json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"$ref":"#/$defs/root","$defs":{"root":{"type":"object"}}}`),
		json.RawMessage(`{"type":"object","properties":{"value":{"type":"array","items":{"type":"string"}}}}`),
		json.RawMessage(`{"type":"object","properties":{"value":{"$ref":"#/$defs/value"}},"$defs":{"value":{"type":"string"}}}`),
		json.RawMessage(`{"type":"object","properties":{"value":{"type":["string","number","null"]}}}`),
		json.RawMessage(`{"type":"object","properties":{"value":{"oneOf":[{"type":"string"},{"type":"number"}]}}}`),
	}
	for index, schema := range tests {
		t.Run(fmt.Sprintf("schema_%d", index), func(t *testing.T) {
			assertCompileErrorWithoutOutput(t, samples, schema, extractor, ErrUnsupportedSchema)
		})
	}
}

func TestFieldValuesEqualUsesExactDecimalComparison(t *testing.T) {
	field := compileField{name: "value", valueType: TypeNumber}
	left := json.RawMessage(`123456789012345678901234567890123456789012345678901234567890123456789012345678901`)
	right := json.RawMessage(`123456789012345678901234567890123456789012345678901234567890123456789012345678900`)
	if fieldValuesEqual(field, left, true, right, true) {
		t.Fatal("adjacent large decimals compared equal")
	}
	if !fieldValuesEqual(field, json.RawMessage(`1.2300e3`), true, json.RawMessage(`1230`), true) {
		t.Fatal("mathematically equal decimal encodings compared different")
	}
}

func TestTruthScalarResourceBoundary(t *testing.T) {
	fields := []compileField{{name: "value", valueType: TypeString}}
	atLimit := json.RawMessage(`"` + strings.Repeat("x", MaxTruthScalarBytes-2) + `"`)
	if err := validateTruthScalars(0, map[string]json.RawMessage{"value": atLimit}, fields); err != nil {
		t.Fatalf("N truth scalar: %v", err)
	}
	overLimit := json.RawMessage(`"` + strings.Repeat("x", MaxTruthScalarBytes-1) + `"`)
	err := validateTruthScalars(0, map[string]json.RawMessage{"value": overLimit}, fields)
	if !errors.Is(err, ErrTruth) || !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("N+1 truth scalar error = %v", err)
	}
}

func TestCandidateResourceBoundaries(t *testing.T) {
	if err := reserveCandidate(MaxCandidatesPerField-1, MaxCompileCandidates-1); err != nil {
		t.Fatalf("N candidate budget: %v", err)
	}
	for _, test := range []struct {
		field int
		total int
	}{
		{field: MaxCandidatesPerField, total: 0},
		{field: 0, total: MaxCompileCandidates},
		{field: -1, total: 0},
	} {
		if err := reserveCandidate(test.field, test.total); !errors.Is(err, ErrResourceLimit) {
			t.Fatalf("N+1 candidate budget (%d,%d) error = %v", test.field, test.total, err)
		}
	}
}

func TestCompileAlignmentAndTextIndexResourceBoundaries(t *testing.T) {
	budget := alignmentBudget{}
	if err := budget.reserve(MaxAlignmentScanBytes); err != nil {
		t.Fatalf("N alignment bytes: %v", err)
	}
	if err := budget.reserve(1); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("N+1 alignment bytes error = %v", err)
	}
	if used, err := reserveCompileTextIndex(0, MaxCompileTextIndexBytes); err != nil || used != MaxCompileTextIndexBytes {
		t.Fatalf("N text index bytes = %d, error %v", used, err)
	}
	if _, err := reserveCompileTextIndex(MaxCompileTextIndexBytes, 1); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("N+1 text index bytes error = %v", err)
	}
}

func TestCompileAlignmentCancellationHasBoundedCheckpoints(t *testing.T) {
	t.Run("normalization", func(t *testing.T) {
		ctx := &cancelAfterChecksContext{Context: context.Background(), cancelAt: 2}
		_, err := normalizeCompileText(ctx, strings.Repeat("x", MaxSampleContentBytes))
		if !errors.Is(err, context.Canceled) || ctx.checks != 2 {
			t.Fatalf("normalization error/checks = %v/%d", err, ctx.checks)
		}
	})

	t.Run("fallback scan", func(t *testing.T) {
		observation := strings.Repeat("a", MaxTruthScalarBytes)
		index := compileTextIndex{observations: make([]textObservation, 10), exact: map[string]indexedTextMatches{}}
		for observationIndex := range index.observations {
			index.observations[observationIndex] = textObservation{normalized: observation, depth: 1}
		}
		ctx := &cancelAfterChecksContext{Context: context.Background(), cancelAt: 4}
		budget := alignmentBudget{}
		_, err := findCompileTextNodes(ctx, index, "needle", &budget)
		if !errors.Is(err, context.Canceled) || ctx.checks != 4 {
			t.Fatalf("scan error/checks = %v/%d", err, ctx.checks)
		}
		if budget.scannedBytes > 2*MaxTruthScalarBytes {
			t.Fatalf("cancellation scanned %d bytes, want at most two observations", budget.scannedBytes)
		}
	})
}

func TestCompileTruthFailuresAreStableAndAtomic(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`)
	samples := []Sample{
		{Content: "one", HTML: `<p class="value">one</p>`},
		{Content: "two", HTML: `<p class="value">two</p>`},
		{Content: "three", HTML: `<p class="value">three</p>`},
	}

	t.Run("extractor error stops immediately", func(t *testing.T) {
		cause := errors.New("provider unavailable")
		extractor := &fixtureTruthExtractor{truths: map[string]json.RawMessage{
			"one": json.RawMessage(`{"value":"one"}`), "two": json.RawMessage(`{"value":"two"}`), "three": json.RawMessage(`{"value":"three"}`),
		}, failAt: 2, err: cause}
		ir, report, err := Compile(context.Background(), samples, schema, extractor)
		if !errors.Is(err, ErrTruth) || !errors.Is(err, cause) || extractor.calls != 2 {
			t.Fatalf("error/calls = %v/%d", err, extractor.calls)
		}
		assertZeroCompileOutput(t, ir, report)
	})

	tests := []struct {
		name string
		data json.RawMessage
		want error
	}{
		{name: "invalid JSON", data: json.RawMessage(`{"value":`), want: ErrTruth},
		{name: "schema violation", data: json.RawMessage(`{"value":123}`), want: ErrTruth},
		{name: "oversized scalar", data: oversizedScalarTruth(), want: ErrResourceLimit},
		{name: "oversized", data: oversizedTruth(), want: ErrResourceLimit},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			extractor := TruthExtractorFunc(func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
				return test.data, nil
			})
			ir, report, err := Compile(context.Background(), samples, schema, extractor)
			if !errors.Is(err, ErrTruth) || !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want ErrTruth and %v", err, test.want)
			}
			assertZeroCompileOutput(t, ir, report)
		})
	}
}

func TestCompileInputResourceBoundaries(t *testing.T) {
	extractor := TruthExtractorFunc(func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"value":"x"}`), nil
	})
	schema := json.RawMessage(`{"value":"string"}`)

	t.Run("sample count", func(t *testing.T) {
		if err := validateCompileInputs(context.Background(), repeatedSamples(MaxCompileSamples, "<p>x</p>", "x"), schema, extractor); err != nil {
			t.Fatalf("N samples: %v", err)
		}
		if err := validateCompileInputs(context.Background(), repeatedSamples(MaxCompileSamples+1, "<p>x</p>", "x"), schema, extractor); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("N+1 samples error = %v", err)
		}
	})

	t.Run("schema bytes", func(t *testing.T) {
		atLimit := json.RawMessage(`{}` + strings.Repeat(" ", MaxCompileSchemaBytes-2))
		if err := validateCompileInputs(context.Background(), repeatedSamples(3, "<p>x</p>", "x"), atLimit, extractor); err != nil {
			t.Fatalf("N schema: %v", err)
		}
		overLimit := append(append(json.RawMessage(nil), atLimit...), ' ')
		if err := validateCompileInputs(context.Background(), repeatedSamples(3, "<p>x</p>", "x"), overLimit, extractor); !errors.Is(err, ErrResourceLimit) {
			t.Fatalf("N+1 schema error = %v", err)
		}
	})

	t.Run("HTML bytes", func(t *testing.T) {
		atLimit := strings.Repeat("h", MaxHTMLBytes)
		if err := validateCompileInputs(context.Background(), repeatedSamples(3, atLimit, "x"), schema, extractor); err != nil {
			t.Fatalf("N HTML: %v", err)
		}
		if err := validateCompileInputs(context.Background(), repeatedSamples(3, atLimit+"h", "x"), schema, extractor); !errors.Is(err, ErrResourceLimit) {
			t.Fatalf("N+1 HTML error = %v", err)
		}
	})

	t.Run("content bytes", func(t *testing.T) {
		atLimit := strings.Repeat("c", MaxSampleContentBytes)
		if err := validateCompileInputs(context.Background(), repeatedSamples(3, "<p>x</p>", atLimit), schema, extractor); err != nil {
			t.Fatalf("N content: %v", err)
		}
		if err := validateCompileInputs(context.Background(), repeatedSamples(3, "<p>x</p>", atLimit+"c"), schema, extractor); !errors.Is(err, ErrResourceLimit) {
			t.Fatalf("N+1 content error = %v", err)
		}
	})

	t.Run("aggregate bytes", func(t *testing.T) {
		htmlAtLimit := strings.Repeat("h", MaxHTMLBytes-1)
		atLimit := repeatedSamples(MaxCompileInputBytes/MaxHTMLBytes, htmlAtLimit, "c")
		if err := validateCompileInputs(context.Background(), atLimit, schema, extractor); err != nil {
			t.Fatalf("N aggregate: %v", err)
		}
		overLimit := append([]Sample(nil), atLimit...)
		overLimit[0].Content = "cc"
		if err := validateCompileInputs(context.Background(), overLimit, schema, extractor); !errors.Is(err, ErrResourceLimit) {
			t.Fatalf("N+1 aggregate error = %v", err)
		}
	})
}

func TestCompileSchemaFieldBoundary(t *testing.T) {
	buildSchema := func(count int) json.RawMessage {
		properties := make(map[string]any, count)
		for index := 0; index < count; index++ {
			properties[fmt.Sprintf("field_%03d", index)] = map[string]any{"type": "string"}
		}
		encoded, err := json.Marshal(map[string]any{"type": "object", "properties": properties})
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	if _, fields, err := parseCompileSchema(buildSchema(MaxFields)); err != nil || len(fields) != MaxFields {
		t.Fatalf("N fields = %d, error %v", len(fields), err)
	}
	if _, _, err := parseCompileSchema(buildSchema(MaxFields + 1)); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("N+1 fields error = %v", err)
	}
}

func TestCompileDocumentAttributeBoundary(t *testing.T) {
	build := func(count int) string {
		var html strings.Builder
		html.Grow(count * 20)
		for index := 0; index < count; index++ {
			fmt.Fprintf(&html, `<i title="%d"></i>`, index)
		}
		return html.String()
	}
	if _, attrs, _, err := parseCompileDocument(context.Background(), build(MaxAttributesPerSample)); err != nil || len(attrs) != MaxAttributesPerSample {
		t.Fatalf("N attrs = %d, error %v", len(attrs), err)
	}
	if _, _, _, err := parseCompileDocument(context.Background(), build(MaxAttributesPerSample+1)); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("N+1 attrs error = %v", err)
	}
}

func TestCompileDocumentDOMNodeBoundary(t *testing.T) {
	// The HTML parser materializes html/head/body in addition to the explicit
	// siblings below, so N explicit nodes is the public limit minus three.
	const implicitDocumentNodes = 3
	build := func(explicit int) string {
		return strings.Repeat("<i></i>", explicit)
	}
	if _, _, _, err := parseCompileDocument(context.Background(), build(MaxDOMNodesPerSample-implicitDocumentNodes)); err != nil {
		t.Fatalf("N DOM nodes: %v", err)
	}
	if _, _, _, err := parseCompileDocument(context.Background(), build(MaxDOMNodesPerSample-implicitDocumentNodes+1)); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("N+1 DOM nodes error = %v", err)
	}
}

func TestCandidateDataAttributesAreDeterministicallyCapped(t *testing.T) {
	node := &html.Node{Type: html.ElementNode, Data: "div"}
	for _, suffix := range []string{"z", "i", "h", "g", "f", "e", "d", "c", "b", "a"} {
		node.Attr = append(node.Attr, html.Attribute{Key: "data-" + suffix, Val: suffix})
	}
	attrs := candidateAttributes(node)
	if len(attrs) != MaxDataAttrsPerNode || attrs[0].Key != "data-a" || attrs[len(attrs)-1].Key != "data-h" {
		t.Fatalf("candidate attrs = %#v", attrs)
	}
}

func TestCompileRejectsCanceledContextWithoutPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	samples := repeatedSamples(3, `<p class="value">x</p>`, "x")
	extractor := TruthExtractorFunc(func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
		t.Fatal("truth extractor called after cancellation")
		return nil, nil
	})
	ir, report, err := Compile(ctx, samples, json.RawMessage(`{"value":"string"}`), extractor)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	assertZeroCompileOutput(t, ir, report)
}

func TestCompileContainsNilAndPanickingTruthExtractors(t *testing.T) {
	samples := repeatedSamples(3, `<p class="value">x</p>`, "x")
	schema := json.RawMessage(`{"value":"string"}`)

	t.Run("typed nil", func(t *testing.T) {
		var extractor *nilableTruthExtractor
		assertCompileErrorWithoutOutput(t, samples, schema, extractor, ErrInvalidInput)
	})
	t.Run("panic", func(t *testing.T) {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("Compile leaked truth extractor panic: %v", recovered)
			}
		}()
		extractor := TruthExtractorFunc(func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
			panic("provider bug")
		})
		assertCompileErrorWithoutOutput(t, samples, schema, extractor, ErrTruth)
	})
}

type fixtureTruthExtractor struct {
	truths map[string]json.RawMessage
	failAt int
	err    error
	calls  int
}

type nilableTruthExtractor struct{}

func (*nilableTruthExtractor) ExtractTruth(context.Context, string, json.RawMessage) (json.RawMessage, error) {
	return nil, errors.New("should not be called")
}

type cancelAfterChecksContext struct {
	context.Context
	checks   int
	cancelAt int
}

func (ctx *cancelAfterChecksContext) Err() error {
	ctx.checks++
	if ctx.checks >= ctx.cancelAt {
		return context.Canceled
	}
	return nil
}

func (extractor *fixtureTruthExtractor) ExtractTruth(_ context.Context, content string, _ json.RawMessage) (json.RawMessage, error) {
	extractor.calls++
	if extractor.failAt > 0 && extractor.calls == extractor.failAt {
		return nil, extractor.err
	}
	data, ok := extractor.truths[content]
	if !ok {
		return nil, fmt.Errorf("missing fixture truth for %q", content)
	}
	return append(json.RawMessage(nil), data...), nil
}

func productCompileFixtures(t *testing.T) ([]Sample, map[string]json.RawMessage) {
	t.Helper()
	contents := []string{
		"Aurora Lamp costs $1,299.50. In stock. Released August 1, 2026.",
		"Borealis Lamp costs USD 899.00. Sold out. Released August 5, 2026.",
		"Comet Lamp costs EUR 749,25. Available now. Released August 8, 2026.",
	}
	truth := []json.RawMessage{
		json.RawMessage(`{"title":"Aurora Lamp","price":1299.5,"available":true,"released_at":"2026-08-01T00:00:00Z"}`),
		json.RawMessage(`{"title":"Borealis Lamp","price":899,"available":false,"released_at":"2026-08-05T09:30:00Z"}`),
		json.RawMessage(`{"title":"Comet Lamp","price":749.25,"available":true,"released_at":"2026-08-08T12:00:00Z"}`),
	}
	samples := make([]Sample, len(contents))
	truths := make(map[string]json.RawMessage, len(contents))
	for index, content := range contents {
		rawHTML, err := os.ReadFile(fmt.Sprintf("testdata/compile-product-%d.html", index+1))
		if err != nil {
			t.Fatal(err)
		}
		samples[index] = Sample{Content: content, HTML: string(rawHTML)}
		truths[content] = truth[index]
	}
	return samples, truths
}

func indexedFieldFixture(t *testing.T, count int) (json.RawMessage, string, json.RawMessage, []string) {
	t.Helper()
	properties := make(map[string]any, count)
	required := make([]string, 0, count)
	truth := make(map[string]any, count)
	values := make([]string, 0, count)
	var rawHTML strings.Builder
	rawHTML.WriteString("<main>")
	for index := 0; index < count; index++ {
		name := fmt.Sprintf("field_%03d", index)
		value := fmt.Sprintf("value-%03d", index)
		properties[name] = map[string]any{"type": "string"}
		required = append(required, name)
		truth[name] = value
		values = append(values, value)
		fmt.Fprintf(&rawHTML, `<span class="%s">%s</span>`, name, value)
	}
	rawHTML.WriteString("</main>")
	schema, err := json.Marshal(map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             required,
		"additionalProperties": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	truthJSON, err := json.Marshal(truth)
	if err != nil {
		t.Fatal(err)
	}
	return schema, rawHTML.String(), truthJSON, values
}

func repeatedSamples(count int, rawHTML, content string) []Sample {
	samples := make([]Sample, count)
	for index := range samples {
		samples[index] = Sample{HTML: rawHTML, Content: content}
	}
	return samples
}

func rulesByName(ir IR) map[string]FieldRule {
	rules := make(map[string]FieldRule, len(ir.Fields))
	for _, rule := range ir.Fields {
		rules[rule.Name] = rule
	}
	return rules
}

func assertStableRule(t *testing.T, rule FieldRule, attr, valueType, transform string) {
	t.Helper()
	if rule.Name == "" || rule.Selector == "" || rule.Attr != attr || rule.Type != valueType || !slices.Contains(rule.Transforms, transform) || !rule.Required {
		t.Fatalf("rule = %#v, want attr=%q type=%q transform=%q required", rule, attr, valueType, transform)
	}
}

func fieldScores(report ValidationReport) map[string]float64 {
	scores := make(map[string]float64, len(report.PerField))
	for _, field := range report.PerField {
		scores[field.Name] = field.Score
	}
	return scores
}

func assertJSONEqual(t *testing.T, got, want json.RawMessage) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON mismatch\n got: %s\nwant: %s", got, want)
	}
}

func assertCompileErrorWithoutOutput(t *testing.T, samples []Sample, schema json.RawMessage, extractor TruthExtractor, target error) {
	t.Helper()
	ir, report, err := Compile(context.Background(), samples, schema, extractor)
	if !errors.Is(err, target) {
		t.Fatalf("Compile() error = %v, want %v", err, target)
	}
	assertZeroCompileOutput(t, ir, report)
}

func assertZeroCompileOutput(t *testing.T, ir IR, report ValidationReport) {
	t.Helper()
	if !reflect.DeepEqual(ir, IR{}) || !reflect.DeepEqual(report, ValidationReport{}) {
		t.Fatalf("error returned partial output: IR=%#v report=%#v", ir, report)
	}
}

func oversizedTruth() json.RawMessage {
	const prefix = `{"value":"`
	const suffix = `"}`
	return json.RawMessage(prefix + strings.Repeat("x", MaxOutputBytes-len(prefix)-len(suffix)+1) + suffix)
}

func oversizedScalarTruth() json.RawMessage {
	return json.RawMessage(`{"value":"` + strings.Repeat("x", MaxTruthScalarBytes-1) + `"}`)
}
