package compiler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/cascadia"
	"github.com/use-agent/purify/llm"
	"golang.org/x/net/html"
)

const (
	MinCompileSamples = 3
	MaxCompileSamples = 20

	MaxCompileSchemaBytes    = 512 << 10
	MaxSampleContentBytes    = 4 << 20
	MaxCompileInputBytes     = 32 << 20
	MaxTruthScalarBytes      = MaxAnchorQuoteBytes
	MaxCandidatesPerField    = 256
	MaxCompileCandidates     = 4096
	MaxDOMNodesPerSample     = 100_000
	MaxAttributesPerSample   = 20_000
	MaxDataAttrsPerNode      = 8
	MaxCompileTextIndexBytes = 16 << 20
	MaxAlignmentScanBytes    = 64 << 20
	MaxTextMatchesPerValue   = 256

	ValidationThreshold = 0.9

	maxClassesPerNode        = 8
	maxSelectorDepth         = 4
	alignmentCheckpointBytes = 4 << 10
)

var (
	ErrInvalidInput      = errors.New("compiler: invalid compile input")
	ErrUnsupportedSchema = errors.New("compiler: unsupported schema")
	ErrTruth             = errors.New("compiler: invalid truth extraction")
	ErrNoCandidate       = errors.New("compiler: no extractor candidate")
)

// Sample keeps the cleaned content shown to the truth extractor together with
// the raw snapshot used to learn and validate deterministic selectors.
type Sample struct {
	Content string `json:"-"`
	HTML    string `json:"-"`
}

// TruthExtractor is the transport-neutral compile-time LLM boundary. API keys,
// provider configuration, repair policy, and HTTP all belong in the adapter
// supplied by the caller.
type TruthExtractor interface {
	ExtractTruth(context.Context, string, json.RawMessage) (json.RawMessage, error)
}

// TruthExtractorFunc adapts a function to TruthExtractor.
type TruthExtractorFunc func(context.Context, string, json.RawMessage) (json.RawMessage, error)

func (extract TruthExtractorFunc) ExtractTruth(ctx context.Context, content string, schema json.RawMessage) (json.RawMessage, error) {
	if extract == nil {
		return nil, errors.New("truth extractor function is nil")
	}
	return extract(ctx, content, schema)
}

// FieldValidation reports atomic full-program agreement for one field.
type FieldValidation struct {
	Name    string  `json:"name"`
	Matches int     `json:"matches"`
	Samples int     `json:"samples"`
	Score   float64 `json:"score"`
}

// ValidationReport is safe to publish with an extractor. CanEnable is true
// only when every sample produced schema-valid JSON and overall agreement met
// the public threshold.
type ValidationReport struct {
	PerField     []FieldValidation `json:"per_field"`
	Overall      float64           `json:"overall"`
	Samples      int               `json:"samples"`
	ValidSamples int               `json:"valid_samples"`
	Threshold    float64           `json:"threshold"`
	CanEnable    bool              `json:"can_enable"`
}

type compileField struct {
	name      string
	valueType string
	required  bool
	pipelines [][]string
}

type compileRecord struct {
	sample Sample
	truth  map[string]json.RawMessage
	doc    *goquery.Document
	attrs  []attributeObservation
	text   compileTextIndex
}

type attributeObservation struct {
	node  *html.Node
	name  string
	value string
}

type ruleCandidate struct {
	rule    FieldRule
	matches int
}

type textObservation struct {
	node       *html.Node
	normalized string
	depth      int
}

type indexedTextMatches struct {
	observations []int
	depth        int
	overflow     bool
	initialized  bool
}

type compileTextIndex struct {
	observations []textObservation
	exact        map[string]indexedTextMatches
}

type boundedNodeText struct {
	value    string
	overflow bool
}

type alignmentBudget struct {
	scannedBytes int
}

var nthOfTypePattern = regexp.MustCompile(`:nth-of-type\([0-9]+\)`)

var commonCandidateAttributes = map[string]struct{}{
	"aria-label": {},
	"content":    {},
	"datetime":   {},
	"href":       {},
	"src":        {},
	"title":      {},
	"value":      {},
}

// Compile learns one deterministic IR from at least three snapshots. A weak
// extractor is still returned with CanEnable=false; malformed inputs, invalid
// truth, resource violations, and fields with no candidate fail atomically.
func Compile(
	ctx context.Context,
	samples []Sample,
	schema json.RawMessage,
	extractor TruthExtractor,
) (IR, ValidationReport, error) {
	if err := validateCompileInputs(ctx, samples, schema, extractor); err != nil {
		return IR{}, ValidationReport{}, err
	}

	normalizedSchema, fields, err := parseCompileSchema(schema)
	if err != nil {
		return IR{}, ValidationReport{}, err
	}
	records, err := collectTruth(ctx, samples, normalizedSchema, fields, extractor)
	if err != nil {
		return IR{}, ValidationReport{}, err
	}

	totalCandidates := 0
	alignment := alignmentBudget{}
	rules := make([]FieldRule, 0, len(fields))
	for _, field := range fields {
		if err := ctx.Err(); err != nil {
			return IR{}, ValidationReport{}, err
		}
		candidates, candidateErr := learnFieldCandidates(ctx, records, field, &totalCandidates, &alignment)
		if candidateErr != nil {
			return IR{}, ValidationReport{}, candidateErr
		}
		if len(candidates) == 0 {
			return IR{}, ValidationReport{}, fmt.Errorf("%w: field %q", ErrNoCandidate, field.name)
		}
		best, scoreErr := chooseCandidate(ctx, records, field, candidates)
		if scoreErr != nil {
			return IR{}, ValidationReport{}, scoreErr
		}
		rules = append(rules, best.rule)
	}

	ir := IR{Version: CurrentIRVersion, Fields: rules}
	if err := validateExecutionResources(ir, ""); err != nil {
		return IR{}, ValidationReport{}, err
	}
	if _, err := compileRules(ir.Fields); err != nil {
		return IR{}, ValidationReport{}, fmt.Errorf("%w: generated IR: %v", ErrNoCandidate, err)
	}

	report, err := validateCompiledIR(ctx, ir, records, fields, normalizedSchema)
	if err != nil {
		return IR{}, ValidationReport{}, err
	}
	return ir, report, nil
}

func validateCompileInputs(ctx context.Context, samples []Sample, schema json.RawMessage, extractor TruthExtractor) error {
	if ctx == nil {
		return fmt.Errorf("%w: context is nil", ErrInvalidInput)
	}
	if isNilTruthExtractor(extractor) {
		return fmt.Errorf("%w: truth extractor is nil", ErrInvalidInput)
	}
	if len(samples) < MinCompileSamples || len(samples) > MaxCompileSamples {
		return fmt.Errorf("%w: sample count must be between %d and %d", ErrInvalidInput, MinCompileSamples, MaxCompileSamples)
	}
	if len(schema) == 0 {
		return fmt.Errorf("%w: schema is empty", ErrInvalidInput)
	}
	if len(schema) > MaxCompileSchemaBytes {
		return fmt.Errorf("%w: schema is %d bytes, maximum is %d", ErrResourceLimit, len(schema), MaxCompileSchemaBytes)
	}

	total := 0
	for index, sample := range samples {
		if strings.TrimSpace(sample.HTML) == "" {
			return fmt.Errorf("%w: sample %d HTML is empty", ErrInvalidInput, index)
		}
		if strings.TrimSpace(sample.Content) == "" {
			return fmt.Errorf("%w: sample %d content is empty", ErrInvalidInput, index)
		}
		if len(sample.HTML) > MaxHTMLBytes {
			return fmt.Errorf("%w: sample %d HTML is %d bytes, maximum is %d", ErrResourceLimit, index, len(sample.HTML), MaxHTMLBytes)
		}
		if len(sample.Content) > MaxSampleContentBytes {
			return fmt.Errorf("%w: sample %d content is %d bytes, maximum is %d", ErrResourceLimit, index, len(sample.Content), MaxSampleContentBytes)
		}
		for _, size := range []int{len(sample.HTML), len(sample.Content)} {
			if size > MaxCompileInputBytes-total {
				return fmt.Errorf("%w: sample input exceeds %d bytes", ErrResourceLimit, MaxCompileInputBytes)
			}
			total += size
		}
	}
	return nil
}

func isNilTruthExtractor(extractor TruthExtractor) bool {
	if extractor == nil {
		return true
	}
	value := reflect.ValueOf(extractor)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func parseCompileSchema(raw json.RawMessage) (json.RawMessage, []compileField, error) {
	normalized, err := llm.NormalizeSchema(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrUnsupportedSchema, err)
	}
	if len(normalized) > MaxCompileSchemaBytes {
		return nil, nil, fmt.Errorf("%w: normalized schema is %d bytes, maximum is %d", ErrResourceLimit, len(normalized), MaxCompileSchemaBytes)
	}
	if err := llm.ValidateSchema(normalized); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrUnsupportedSchema, err)
	}

	var root map[string]json.RawMessage
	if err := decodeOneJSON(normalized, &root); err != nil || root == nil {
		return nil, nil, fmt.Errorf("%w: root must be an object schema", ErrUnsupportedSchema)
	}
	if rawType, ok := root["type"]; ok && !schemaTypeIsOnly(rawType, "object") {
		return nil, nil, fmt.Errorf("%w: root type must be object", ErrUnsupportedSchema)
	}
	for _, unsupported := range unsupportedCompileSchemaKeywords {
		if _, ok := root[unsupported]; ok {
			return nil, nil, fmt.Errorf("%w: root keyword %q is not supported by deterministic compilation", ErrUnsupportedSchema, unsupported)
		}
	}

	var properties map[string]json.RawMessage
	if rawProperties, ok := root["properties"]; !ok || decodeOneJSON(rawProperties, &properties) != nil || len(properties) == 0 {
		return nil, nil, fmt.Errorf("%w: root properties must be a non-empty object", ErrUnsupportedSchema)
	}
	if len(properties) > MaxFields {
		return nil, nil, fmt.Errorf("%w: field count is %d, maximum is %d", ErrResourceLimit, len(properties), MaxFields)
	}

	required := make(map[string]struct{})
	if rawRequired, ok := root["required"]; ok {
		var names []string
		if err := decodeOneJSON(rawRequired, &names); err != nil {
			return nil, nil, fmt.Errorf("%w: root required must be an array of strings", ErrUnsupportedSchema)
		}
		for _, name := range names {
			if _, exists := properties[name]; !exists {
				return nil, nil, fmt.Errorf("%w: required field %q has no property schema", ErrUnsupportedSchema, name)
			}
			required[name] = struct{}{}
		}
	}

	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)
	fields := make([]compileField, 0, len(names))
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			return nil, nil, fmt.Errorf("%w: field name is empty", ErrUnsupportedSchema)
		}
		if len(name) > MaxFieldNameBytes {
			return nil, nil, fmt.Errorf("%w: field name %q exceeds %d bytes", ErrResourceLimit, name, MaxFieldNameBytes)
		}
		valueType, pipelines, fieldErr := parsePropertySchema(properties[name])
		if fieldErr != nil {
			return nil, nil, fmt.Errorf("%w: field %q: %v", ErrUnsupportedSchema, name, fieldErr)
		}
		_, isRequired := required[name]
		fields = append(fields, compileField{name: name, valueType: valueType, required: isRequired, pipelines: pipelines})
	}
	return append(json.RawMessage(nil), normalized...), fields, nil
}

func parsePropertySchema(raw json.RawMessage) (string, [][]string, error) {
	var property map[string]json.RawMessage
	if err := decodeOneJSON(raw, &property); err != nil || property == nil {
		return "", nil, errors.New("property must be an object schema")
	}
	for _, unsupported := range unsupportedCompileSchemaKeywords {
		if _, ok := property[unsupported]; ok {
			return "", nil, fmt.Errorf("keyword %q is not supported by deterministic compilation", unsupported)
		}
	}
	rawType, ok := property["type"]
	if !ok {
		return "", nil, errors.New("scalar type is required")
	}
	typeName, err := oneNonNullSchemaType(rawType)
	if err != nil {
		return "", nil, err
	}

	switch typeName {
	case "string":
		var format string
		if rawFormat, ok := property["format"]; ok {
			if err := decodeOneJSON(rawFormat, &format); err != nil {
				return "", nil, errors.New("format must be a string")
			}
		}
		if format == "date-time" {
			return TypeDate, [][]string{{"trim", "parse_date"}}, nil
		}
		return TypeString, [][]string{{"trim", "collapse_ws"}}, nil
	case "number", "integer":
		return TypeNumber, [][]string{{"trim", "parse_number"}, {"trim", "currency_amount"}}, nil
	case "boolean":
		return TypeBoolean, [][]string{{"trim", "lower"}}, nil
	default:
		return "", nil, fmt.Errorf("type %q is not a supported scalar", typeName)
	}
}

func schemaTypeIsOnly(raw json.RawMessage, wanted string) bool {
	typeName, err := oneNonNullSchemaType(raw)
	return err == nil && typeName == wanted
}

func oneNonNullSchemaType(raw json.RawMessage) (string, error) {
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		if single == "null" {
			return "", errors.New("null-only fields cannot be compiled")
		}
		return single, nil
	}
	var union []string
	if err := json.Unmarshal(raw, &union); err != nil || len(union) == 0 {
		return "", errors.New("type must be a string or nullable scalar union")
	}
	nonNull := ""
	for _, item := range union {
		if item == "null" {
			continue
		}
		if nonNull != "" && item != nonNull {
			return "", errors.New("type union must contain exactly one non-null scalar type")
		}
		nonNull = item
	}
	if nonNull == "" {
		return "", errors.New("null-only fields cannot be compiled")
	}
	return nonNull, nil
}

var unsupportedCompileSchemaKeywords = []string{"$ref", "$dynamicRef", "allOf", "anyOf", "oneOf", "not", "if", "then", "else"}

func collectTruth(
	ctx context.Context,
	samples []Sample,
	schema json.RawMessage,
	fields []compileField,
	extractor TruthExtractor,
) ([]compileRecord, error) {
	records := make([]compileRecord, 0, len(samples))
	for index, sample := range samples {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := callTruthExtractor(ctx, extractor, sample.Content, schema)
		if err != nil {
			return nil, fmt.Errorf("%w: sample %d: %w", ErrTruth, index, err)
		}
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("%w: sample %d: %w", ErrTruth, index, err)
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("%w: sample %d returned empty JSON", ErrTruth, index)
		}
		if len(data) > MaxOutputBytes {
			return nil, fmt.Errorf("%w: sample %d truth exceeds %d bytes: %w", ErrTruth, index, MaxOutputBytes, ErrResourceLimit)
		}
		violations, err := llm.ValidateAgainstSchema(schema, data)
		if err != nil {
			return nil, fmt.Errorf("%w: sample %d JSON: %v", ErrTruth, index, err)
		}
		if len(violations) != 0 {
			return nil, fmt.Errorf("%w: sample %d has %d schema violations", ErrTruth, index, len(violations))
		}
		var truth map[string]json.RawMessage
		if err := decodeOneJSON(data, &truth); err != nil || truth == nil {
			return nil, fmt.Errorf("%w: sample %d truth must be an object", ErrTruth, index)
		}
		if err := validateTruthScalars(index, truth, fields); err != nil {
			return nil, err
		}

		doc, attrs, textIndex, err := parseCompileDocument(ctx, sample.HTML)
		if err != nil {
			return nil, err
		}
		records = append(records, compileRecord{sample: sample, truth: truth, doc: doc, attrs: attrs, text: textIndex})
	}
	return records, nil
}

func callTruthExtractor(
	ctx context.Context,
	extractor TruthExtractor,
	content string,
	schema json.RawMessage,
) (data json.RawMessage, err error) {
	defer func() {
		if recover() != nil {
			data = nil
			err = errors.New("truth extractor panicked")
		}
	}()
	return extractor.ExtractTruth(ctx, content, schema)
}

func validateTruthScalars(sampleIndex int, truth map[string]json.RawMessage, fields []compileField) error {
	for _, field := range fields {
		value, ok := truth[field.name]
		if !ok || isJSONNull(value) {
			continue
		}
		if len(value) > MaxTruthScalarBytes {
			return fmt.Errorf(
				"%w: sample %d field %q truth is %d bytes, maximum is %d: %w",
				ErrTruth,
				sampleIndex,
				field.name,
				len(value),
				MaxTruthScalarBytes,
				ErrResourceLimit,
			)
		}
	}
	return nil
}

func parseCompileDocument(ctx context.Context, rawHTML string) (*goquery.Document, []attributeObservation, compileTextIndex, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, compileTextIndex{}, err
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(rawHTML))
	if err != nil {
		return nil, nil, compileTextIndex{}, fmt.Errorf("%w: parse sample HTML: %v", ErrInvalidInput, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, compileTextIndex{}, err
	}
	attrs := make([]attributeObservation, 0)
	depths := make(map[*html.Node]int)
	nodes := 0
	resourceErr := error(nil)
	doc.Find("*").EachWithBreak(func(_ int, selection *goquery.Selection) bool {
		nodes++
		if err := ctx.Err(); err != nil {
			resourceErr = err
			return false
		}
		if nodes > MaxDOMNodesPerSample {
			resourceErr = fmt.Errorf("%w: DOM exceeds %d elements", ErrResourceLimit, MaxDOMNodesPerSample)
			return false
		}
		if len(selection.Nodes) == 0 {
			return true
		}
		node := selection.Nodes[0]
		depths[node] = depths[node.Parent] + 1
		nodeAttrs := candidateAttributes(node)
		if len(nodeAttrs) > MaxAttributesPerSample-len(attrs) {
			resourceErr = fmt.Errorf("%w: candidate attributes exceed %d per sample", ErrResourceLimit, MaxAttributesPerSample)
			return false
		}
		for _, attr := range nodeAttrs {
			attrs = append(attrs, attributeObservation{node: node, name: attr.Key, value: attr.Val})
		}
		return true
	})
	if resourceErr != nil {
		return nil, nil, compileTextIndex{}, resourceErr
	}
	textIndex, err := buildCompileTextIndex(ctx, doc.Selection.Nodes, depths)
	if err != nil {
		return nil, nil, compileTextIndex{}, err
	}
	return doc, attrs, textIndex, nil
}

func buildCompileTextIndex(ctx context.Context, roots []*html.Node, depths map[*html.Node]int) (compileTextIndex, error) {
	type frame struct {
		node    *html.Node
		visited bool
	}
	stack := make([]frame, 0, len(roots)*2)
	for index := len(roots) - 1; index >= 0; index-- {
		stack = append(stack, frame{node: roots[index]})
	}
	nodeText := make(map[*html.Node]boundedNodeText)
	index := compileTextIndex{exact: make(map[string]indexedTextMatches)}
	indexedBytes := 0
	processed := 0

	for len(stack) > 0 {
		last := len(stack) - 1
		current := stack[last]
		stack = stack[:last]
		if current.node == nil {
			continue
		}
		if !current.visited {
			if current.node.Type == html.ElementNode && ignoredCompileTextElement(current.node.Data) {
				nodeText[current.node] = boundedNodeText{}
				continue
			}
			stack = append(stack, frame{node: current.node, visited: true})
			children := make([]*html.Node, 0)
			childCount := 0
			for child := current.node.FirstChild; child != nil; child = child.NextSibling {
				children = append(children, child)
				childCount++
				if childCount%256 == 0 {
					if err := ctx.Err(); err != nil {
						return compileTextIndex{}, err
					}
				}
			}
			for childIndex := len(children) - 1; childIndex >= 0; childIndex-- {
				stack = append(stack, frame{node: children[childIndex]})
			}
			continue
		}

		processed++
		if processed%256 == 0 {
			if err := ctx.Err(); err != nil {
				return compileTextIndex{}, err
			}
		}
		switch current.node.Type {
		case html.TextNode:
			if len(current.node.Data) > MaxTruthScalarBytes {
				nodeText[current.node] = boundedNodeText{overflow: true}
			} else {
				nodeText[current.node] = boundedNodeText{value: current.node.Data}
			}
		case html.ElementNode, html.DocumentNode:
			if current.node.Type == html.ElementNode && ignoredCompileTextElement(current.node.Data) {
				nodeText[current.node] = boundedNodeText{}
				continue
			}
			combined := combineCompileChildText(current.node, nodeText)
			nodeText[current.node] = combined
			if current.node.Type != html.ElementNode || combined.overflow || combined.value == "" {
				continue
			}
			normalized, err := normalizeCompileText(ctx, combined.value)
			if err != nil {
				return compileTextIndex{}, err
			}
			if normalized == "" {
				continue
			}
			var reserveErr error
			indexedBytes, reserveErr = reserveCompileTextIndex(indexedBytes, len(normalized))
			if reserveErr != nil {
				return compileTextIndex{}, reserveErr
			}
			observationIndex := len(index.observations)
			observation := textObservation{node: current.node, normalized: normalized, depth: depths[current.node]}
			index.observations = append(index.observations, observation)
			matches := index.exact[normalized]
			switch {
			case !matches.initialized || observation.depth > matches.depth:
				matches = indexedTextMatches{observations: []int{observationIndex}, depth: observation.depth, initialized: true}
			case observation.depth == matches.depth && !matches.overflow:
				if len(matches.observations) >= MaxTextMatchesPerValue {
					matches.overflow = true
					matches.observations = nil
				} else {
					matches.observations = append(matches.observations, observationIndex)
				}
			}
			index.exact[normalized] = matches
		}
	}
	if err := ctx.Err(); err != nil {
		return compileTextIndex{}, err
	}
	return index, nil
}

func reserveCompileTextIndex(used, size int) (int, error) {
	if used < 0 || size < 0 || used > MaxCompileTextIndexBytes || size > MaxCompileTextIndexBytes-used {
		return 0, fmt.Errorf("%w: compile text index exceeds %d bytes", ErrResourceLimit, MaxCompileTextIndexBytes)
	}
	return used + size, nil
}

func ignoredCompileTextElement(tag string) bool {
	switch strings.ToLower(tag) {
	case "script", "style", "noscript":
		return true
	default:
		return false
	}
}

func combineCompileChildText(node *html.Node, values map[*html.Node]boundedNodeText) boundedNodeText {
	var combined strings.Builder
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		value := values[child]
		if value.overflow {
			return boundedNodeText{overflow: true}
		}
		if value.value == "" {
			continue
		}
		if len(value.value) > MaxTruthScalarBytes-combined.Len() {
			return boundedNodeText{overflow: true}
		}
		combined.WriteString(value.value)
	}
	return boundedNodeText{value: combined.String()}
}

func normalizeCompileText(ctx context.Context, input string) (string, error) {
	var normalized strings.Builder
	normalized.Grow(len(input))
	lastSpace := false
	lastCheckpoint := 0
	for byteOffset, original := range input {
		if byteOffset-lastCheckpoint >= alignmentCheckpointBytes {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			lastCheckpoint = byteOffset
		}
		value := foldCompileWidth(unicode.ToLower(original))
		if dropCompileComparisonRune(value) {
			continue
		}
		if unicode.IsSpace(value) {
			if normalized.Len() == 0 || lastSpace {
				continue
			}
			value = ' '
			lastSpace = true
		} else {
			lastSpace = false
		}
		normalized.WriteRune(value)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return strings.TrimSpace(normalized.String()), nil
}

func foldCompileWidth(value rune) rune {
	switch {
	case value == '\u3000':
		return ' '
	case value >= '\uff01' && value <= '\uff5e':
		return value - 0xfee0
	default:
		return value
	}
}

func dropCompileComparisonRune(value rune) bool {
	switch value {
	case ',', '$', '¥', '￥', '€', '£':
		return true
	default:
		return false
	}
}

func candidateAttributes(node *html.Node) []html.Attribute {
	attrs := make([]html.Attribute, 0)
	dataAttrs := make([]html.Attribute, 0)
	for _, attr := range node.Attr {
		name := strings.ToLower(strings.TrimSpace(attr.Key))
		if _, ok := commonCandidateAttributes[name]; ok {
			attr.Key = name
			attrs = append(attrs, attr)
			continue
		}
		if strings.HasPrefix(name, "data-") && len(name) > len("data-") && len(name) <= MaxAttributeBytes {
			attr.Key = name
			dataAttrs = append(dataAttrs, attr)
		}
	}
	sort.Slice(attrs, func(i, j int) bool {
		if attrs[i].Key == attrs[j].Key {
			return attrs[i].Val < attrs[j].Val
		}
		return attrs[i].Key < attrs[j].Key
	})
	sort.Slice(dataAttrs, func(i, j int) bool {
		if dataAttrs[i].Key == dataAttrs[j].Key {
			return dataAttrs[i].Val < dataAttrs[j].Val
		}
		return dataAttrs[i].Key < dataAttrs[j].Key
	})
	if len(dataAttrs) > MaxDataAttrsPerNode {
		dataAttrs = dataAttrs[:MaxDataAttrsPerNode]
	}
	return append(attrs, dataAttrs...)
}

func learnFieldCandidates(
	ctx context.Context,
	records []compileRecord,
	field compileField,
	totalCandidates *int,
	alignment *alignmentBudget,
) (map[string]ruleCandidate, error) {
	candidates := make(map[string]ruleCandidate)
	addForNode := func(node *html.Node, exactSelector, attr string) error {
		selectors := selectorCandidates(node)
		if exactSelector != "" {
			selectors = append(selectors, exactSelector, nthOfTypePattern.ReplaceAllString(exactSelector, ""))
		}
		sort.Strings(selectors)
		selectors = compactStrings(selectors)
		for _, selector := range selectors {
			if len(selector) == 0 || len(selector) > MaxSelectorBytes || len(attr) > MaxAttributeBytes {
				continue
			}
			if _, err := cascadia.Compile(selector); err != nil {
				continue
			}
			for _, pipeline := range field.pipelines {
				rule := FieldRule{
					Name:       field.name,
					Selector:   selector,
					Attr:       attr,
					Transforms: append([]string(nil), pipeline...),
					Type:       field.valueType,
					Required:   field.required,
				}
				key := candidateKey(rule)
				if _, exists := candidates[key]; exists {
					continue
				}
				if err := reserveCandidate(len(candidates), *totalCandidates); err != nil {
					return err
				}
				candidates[key] = ruleCandidate{rule: rule}
				(*totalCandidates)++
			}
		}
		return nil
	}

	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		expected, present := record.truth[field.name]
		if !present || isJSONNull(expected) {
			continue
		}
		text, ok := truthText(expected)
		if !ok {
			continue
		}
		textNodes, err := findCompileTextNodes(ctx, record.text, text, alignment)
		if err != nil {
			return nil, err
		}
		for _, node := range textNodes {
			if err := addForNode(node, "", ""); err != nil {
				return nil, err
			}
		}

		for attributeIndex, observation := range record.attrs {
			if attributeIndex%256 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			if len(observation.value) > MaxAnchorQuoteBytes || !rawCanMatchTruth(observation.value, expected, field) {
				continue
			}
			if err := addForNode(observation.node, "", observation.name); err != nil {
				return nil, err
			}
		}
	}
	return candidates, nil
}

func findCompileTextNodes(
	ctx context.Context,
	index compileTextIndex,
	value string,
	budget *alignmentBudget,
) ([]*html.Node, error) {
	normalized, err := normalizeCompileText(ctx, value)
	if err != nil {
		return nil, err
	}
	if normalized == "" {
		return nil, nil
	}
	if exact, ok := index.exact[normalized]; ok {
		if exact.overflow {
			return nil, fmt.Errorf("%w: more than %d exact text matches", ErrResourceLimit, MaxTextMatchesPerValue)
		}
		nodes := make([]*html.Node, 0, len(exact.observations))
		for _, observationIndex := range exact.observations {
			if observationIndex < 0 || observationIndex >= len(index.observations) {
				return nil, fmt.Errorf("%w: corrupt compile text index", ErrInvalidInput)
			}
			nodes = append(nodes, index.observations[observationIndex].node)
		}
		return nodes, nil
	}

	deepest := -1
	matches := make([]*html.Node, 0)
	for _, observation := range index.observations {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := budget.reserve(len(observation.normalized)); err != nil {
			return nil, err
		}
		if len(normalized) > len(observation.normalized) || !strings.Contains(observation.normalized, normalized) {
			continue
		}
		switch {
		case observation.depth > deepest:
			deepest = observation.depth
			matches = append(matches[:0], observation.node)
		case observation.depth == deepest:
			if len(matches) >= MaxTextMatchesPerValue {
				return nil, fmt.Errorf("%w: more than %d normalized text matches", ErrResourceLimit, MaxTextMatchesPerValue)
			}
			matches = append(matches, observation.node)
		}
	}
	return matches, nil
}

func (budget *alignmentBudget) reserve(size int) error {
	if budget == nil || size < 0 || budget.scannedBytes < 0 || budget.scannedBytes > MaxAlignmentScanBytes ||
		size > MaxAlignmentScanBytes-budget.scannedBytes {
		return fmt.Errorf("%w: compile alignment scan exceeds %d bytes", ErrResourceLimit, MaxAlignmentScanBytes)
	}
	budget.scannedBytes += size
	return nil
}

func reserveCandidate(fieldCandidates, totalCandidates int) error {
	if fieldCandidates < 0 || totalCandidates < 0 ||
		fieldCandidates >= MaxCandidatesPerField || totalCandidates >= MaxCompileCandidates {
		return fmt.Errorf("%w: extractor candidates exceed configured maximum", ErrResourceLimit)
	}
	return nil
}

func selectorCandidates(node *html.Node) []string {
	if node == nil || node.Type != html.ElementNode {
		return nil
	}
	tag := strings.ToLower(node.Data)
	candidates := make([]string, 0, 24)
	if id := nodeAttribute(node, "id"); id != "" {
		candidates = append(candidates, "#"+escapeCSSIdentifier(id))
	}
	classes := strings.Fields(nodeAttribute(node, "class"))
	sort.Strings(classes)
	if len(classes) > maxClassesPerNode {
		classes = classes[:maxClassesPerNode]
	}
	for _, className := range classes {
		escaped := escapeCSSIdentifier(className)
		candidates = append(candidates, "."+escaped, tag+"."+escaped)
	}
	if len(classes) > 1 {
		combined := tag
		for _, className := range classes {
			combined += "." + escapeCSSIdentifier(className)
		}
		candidates = append(candidates, combined)
	}

	segments := make([]string, 0, maxSelectorDepth)
	for current := node; current != nil && current.Type == html.ElementNode && len(segments) < maxSelectorDepth; current = current.Parent {
		segment := strings.ToLower(current.Data)
		currentClasses := strings.Fields(nodeAttribute(current, "class"))
		sort.Strings(currentClasses)
		if len(currentClasses) > 0 {
			segment += "." + escapeCSSIdentifier(currentClasses[0])
		}
		segments = append([]string{segment}, segments...)
		candidates = append(candidates, strings.Join(segments, " > "))
	}
	return candidates
}

func nodeAttribute(node *html.Node, wanted string) string {
	for _, attr := range node.Attr {
		if strings.EqualFold(attr.Key, wanted) {
			return attr.Val
		}
	}
	return ""
}

func escapeCSSIdentifier(value string) string {
	var escaped strings.Builder
	for index, r := range value {
		if (unicode.IsLetter(r) || r == '_' || r == '-' || index > 0 && unicode.IsDigit(r)) && r < unicode.MaxASCII {
			escaped.WriteRune(r)
			continue
		}
		fmt.Fprintf(&escaped, `\%x `, r)
	}
	return escaped.String()
}

func compactStrings(values []string) []string {
	if len(values) == 0 {
		return values
	}
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func chooseCandidate(
	ctx context.Context,
	records []compileRecord,
	field compileField,
	candidateMap map[string]ruleCandidate,
) (ruleCandidate, error) {
	keys := make([]string, 0, len(candidateMap))
	for key := range candidateMap {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	candidates := make([]ruleCandidate, 0, len(keys))
	for _, key := range keys {
		candidate := candidateMap[key]
		compiled, err := compileRules([]FieldRule{candidate.rule})
		if err != nil {
			continue
		}
		for _, record := range records {
			if err := ctx.Err(); err != nil {
				return ruleCandidate{}, err
			}
			selection := record.doc.FindMatcher(compiled[0].selector)
			if selection.Length() != 1 {
				continue
			}
			value, _, found, err := executeRule(record.doc, compiled[0])
			if err != nil {
				if errors.Is(err, ErrResourceLimit) {
					return ruleCandidate{}, err
				}
				continue
			}
			actual, present := encodedValue(value, found)
			expected, expectedPresent := record.truth[field.name]
			if fieldValuesEqual(field, expected, expectedPresent, actual, present) {
				candidate.matches++
			}
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		return ruleCandidate{}, fmt.Errorf("%w: field %q has no valid rule", ErrNoCandidate, field.name)
	}
	sort.Slice(candidates, func(i, j int) bool { return betterCandidate(candidates[i], candidates[j]) })
	return candidates[0], nil
}

func betterCandidate(left, right ruleCandidate) bool {
	if left.matches != right.matches {
		return left.matches > right.matches
	}
	leftRank, rightRank := selectorRank(left.rule.Selector), selectorRank(right.rule.Selector)
	if leftRank != rightRank {
		return leftRank < rightRank
	}
	if len(left.rule.Selector) != len(right.rule.Selector) {
		return len(left.rule.Selector) < len(right.rule.Selector)
	}
	if (left.rule.Attr == "") != (right.rule.Attr == "") {
		return left.rule.Attr == ""
	}
	if len(left.rule.Transforms) != len(right.rule.Transforms) {
		return len(left.rule.Transforms) < len(right.rule.Transforms)
	}
	leftTransformRank, rightTransformRank := transformPipelineRank(left.rule.Transforms), transformPipelineRank(right.rule.Transforms)
	if leftTransformRank != rightTransformRank {
		return leftTransformRank < rightTransformRank
	}
	return candidateKey(left.rule) < candidateKey(right.rule)
}

func transformPipelineRank(transforms []string) int {
	for _, transform := range transforms {
		if transform == "currency_amount" {
			return 1
		}
	}
	return 0
}

func selectorRank(selector string) int {
	switch {
	case strings.Contains(selector, ":nth-of-type("):
		return 5
	case strings.HasPrefix(selector, "#") && !strings.ContainsAny(selector, " >"):
		return 0
	case !strings.ContainsAny(selector, " >") && strings.Contains(selector, "."):
		return 1
	case strings.Contains(selector, " > "):
		return 3
	default:
		return 4
	}
}

func candidateKey(rule FieldRule) string {
	return rule.Selector + "\x00" + rule.Attr + "\x00" + strings.Join(rule.Transforms, "\x00") + "\x00" + rule.Type
}

func rawCanMatchTruth(raw string, expected json.RawMessage, field compileField) bool {
	for _, pipeline := range field.pipelines {
		value := raw
		valid := true
		for _, name := range pipeline {
			transform, ok := transformRegistry[name]
			if !ok {
				valid = false
				break
			}
			var err error
			value, err = transform(value)
			if err != nil {
				valid = false
				break
			}
		}
		if !valid {
			continue
		}
		converted, err := convertValue(value, field.valueType)
		if err != nil {
			continue
		}
		actual, _ := json.Marshal(converted)
		if fieldValuesEqual(field, expected, true, actual, true) {
			return true
		}
	}
	return false
}

func validateCompiledIR(
	ctx context.Context,
	ir IR,
	records []compileRecord,
	fields []compileField,
	schema json.RawMessage,
) (ValidationReport, error) {
	report := ValidationReport{
		PerField:  make([]FieldValidation, len(fields)),
		Samples:   len(records),
		Threshold: ValidationThreshold,
	}
	for index, field := range fields {
		report.PerField[index] = FieldValidation{Name: field.name, Samples: len(records)}
	}

	totalMatches := 0
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return ValidationReport{}, err
		}
		data, _, executeErr := Execute(ir, record.sample.HTML)
		if executeErr != nil {
			continue
		}
		violations, err := llm.ValidateAgainstSchema(schema, data)
		if err != nil {
			return ValidationReport{}, fmt.Errorf("%w: validate compiled output: %v", ErrUnsupportedSchema, err)
		}
		if len(violations) == 0 {
			report.ValidSamples++
		}
		var actual map[string]json.RawMessage
		if err := decodeOneJSON(data, &actual); err != nil || actual == nil {
			return ValidationReport{}, fmt.Errorf("%w: generated output is not an object", ErrNoCandidate)
		}
		for fieldIndex, field := range fields {
			expected, expectedPresent := record.truth[field.name]
			got, gotPresent := actual[field.name]
			if fieldValuesEqual(field, expected, expectedPresent, got, gotPresent) {
				report.PerField[fieldIndex].Matches++
				totalMatches++
			}
		}
	}

	for index := range report.PerField {
		report.PerField[index].Score = float64(report.PerField[index].Matches) / float64(report.PerField[index].Samples)
	}
	denominator := len(fields) * len(records)
	report.Overall = float64(totalMatches) / float64(denominator)
	report.CanEnable = report.ValidSamples == report.Samples && report.Overall >= ValidationThreshold
	return report, nil
}

func encodedValue(value any, present bool) (json.RawMessage, bool) {
	if !present {
		return nil, false
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

func fieldValuesEqual(
	field compileField,
	expected json.RawMessage,
	expectedPresent bool,
	actual json.RawMessage,
	actualPresent bool,
) bool {
	if !expectedPresent {
		return !actualPresent
	}
	if isJSONNull(expected) {
		return actualPresent && isJSONNull(actual)
	}
	if !actualPresent || isJSONNull(actual) {
		return false
	}

	switch field.valueType {
	case TypeString:
		var left, right string
		if json.Unmarshal(expected, &left) != nil || json.Unmarshal(actual, &right) != nil {
			return false
		}
		return normalizeComparableString(left) == normalizeComparableString(right)
	case TypeDate:
		var left, right string
		if json.Unmarshal(expected, &left) != nil || json.Unmarshal(actual, &right) != nil {
			return false
		}
		leftDate, leftErr := canonicalDate(left)
		rightDate, rightErr := canonicalDate(right)
		return leftErr == nil && rightErr == nil && leftDate == rightDate
	case TypeBoolean:
		var left, right bool
		return json.Unmarshal(expected, &left) == nil && json.Unmarshal(actual, &right) == nil && left == right
	case TypeNumber:
		left, leftOK := parseComparableNumber(expected)
		right, rightOK := parseComparableNumber(actual)
		return leftOK && rightOK && left.Cmp(right) == 0
	default:
		return false
	}
}

func normalizeComparableString(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func parseComparableNumber(raw json.RawMessage) (*big.Rat, bool) {
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err != nil {
		return nil, false
	}
	return exactDecimalRat(number.String())
}

func exactDecimalRat(value string) (*big.Rat, bool) {
	negative := strings.HasPrefix(value, "-")
	if negative {
		value = value[1:]
	}
	exponent := 0
	if index := strings.IndexAny(value, "eE"); index >= 0 {
		rawExponent := value[index+1:]
		value = value[:index]
		// Execute converts through a finite float, so exponents beyond this
		// bounded exact-comparison range can never become a valid IR value.
		if len(rawExponent) == 0 || len(rawExponent) > 6 {
			return nil, false
		}
		parsed, err := strconv.Atoi(rawExponent)
		if err != nil || parsed < -MaxTruthScalarBytes || parsed > MaxTruthScalarBytes {
			return nil, false
		}
		exponent = parsed
	}
	fractionDigits := 0
	if dot := strings.IndexByte(value, '.'); dot >= 0 {
		fractionDigits = len(value) - dot - 1
		value = value[:dot] + value[dot+1:]
	}
	if value == "" || len(value) > MaxTruthScalarBytes {
		return nil, false
	}
	integer := new(big.Int)
	if _, ok := integer.SetString(value, 10); !ok {
		return nil, false
	}
	if negative {
		integer.Neg(integer)
	}
	result := new(big.Rat).SetInt(integer)
	scale := fractionDigits - exponent
	if scale == 0 {
		return result, true
	}
	power := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(absInt(scale))), nil)
	if scale > 0 {
		result.Quo(result, new(big.Rat).SetInt(power))
	} else {
		result.Mul(result, new(big.Rat).SetInt(power))
	}
	return result, true
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func truthText(raw json.RawMessage) (string, bool) {
	if isJSONNull(raw) {
		return "", false
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", false
	}
	switch typed := value.(type) {
	case string:
		return typed, typed != ""
	case json.Number:
		return typed.String(), true
	case bool:
		if typed {
			return "true", true
		}
		return "false", true
	default:
		return "", false
	}
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func decodeOneJSON(raw json.RawMessage, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
