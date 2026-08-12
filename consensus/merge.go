// Package consensus merges independently extracted JSON facts while keeping
// every winning and conflicting value attached to its source evidence.
package consensus

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/use-agent/purify/evidence"
	"github.com/use-agent/purify/publicnet"
	"github.com/use-agent/purify/simhash"
	"golang.org/x/net/publicsuffix"
)

const (
	// MaxSources bounds both pairwise similarity work and response fan-out.
	MaxSources = 8
	// MaxURLBytes matches the repository-wide persisted URL bound.
	MaxURLBytes = 16 << 10
	// MaxSourceDataBytes matches the extraction output bound.
	MaxSourceDataBytes = 4 << 20
	// MaxCleanedTextBytes bounds optional non-wire content used to derive
	// anchored independence signals.
	MaxCleanedTextBytes = 4 << 20
	// MaxTotalDataBytes is the aggregate JSON budget across all sources.
	MaxTotalDataBytes = 32 << 20
	// MaxOutputBytes bounds the complete JSON encoding of Result. Merge
	// preflights this budget before allocating any repeated support slices.
	MaxOutputBytes = 32 << 20
	// MaxJSONDepth bounds recursive decoding and evidence path construction.
	MaxJSONDepth = 64
	// MaxJSONNodesPerSource prevents tiny empty containers from bypassing the
	// scalar-leaf limit.
	MaxJSONNodesPerSource = 20_000
	// MaxLeavesPerSource bounds field grouping and evidence maps.
	MaxLeavesPerSource = 10_000
	// MaxPathBytes matches the verification claim path bound.
	MaxPathBytes = 4 << 10
	// MaxNumberBytes and MaxNumberExponent bound exact rational arithmetic.
	MaxNumberBytes    = 1 << 10
	MaxNumberExponent = 4 << 10
	// MaxCanonicalNumberBytes bounds expansion of exponent-form numbers.
	MaxCanonicalNumberBytes = 8 << 10

	MaxEvidenceQuoteBytes      = 8 << 10
	MaxEvidenceSelectorBytes   = 4 << 10
	MaxEvidenceSnapshotIDBytes = 512
	MaxEvidenceMethodBytes     = 128
	MaxReceiptBytes            = 2 << 20
	MaxTotalMetadataBytes      = 32 << 20

	independenceDistance = 3
)

var (
	// ErrInvalidInput marks malformed URLs, JSON, evidence, and source sets.
	ErrInvalidInput = errors.New("consensus: invalid input")
	// ErrResourceLimit marks a documented consensus resource bound.
	ErrResourceLimit = errors.New("consensus: resource limit exceeded")
	// ErrDuplicateSourceConflict marks two non-identical results that claim the
	// same canonical final URL.
	ErrDuplicateSourceConflict = errors.New("consensus: duplicate source conflict")
)

// SourceResult is one successful extraction. URL must be the final response
// URL. Root is deliberately not accepted from callers: Merge derives it from
// the canonical URL. Basis and Receipts use evidence.LeafValues paths.
// CleanedText is optional non-wire input, borrowed read-only for the duration
// of Merge, and is never retained in Result.
type SourceResult struct {
	URL         string
	Data        json.RawMessage
	Basis       map[string]evidence.Anchor
	Receipts    map[string]string
	SimText     uint64
	CleanedText string `json:"-"`
}

// Result contains deterministic field consensus keyed by evidence leaf path.
type Result struct {
	Fields map[string]FieldConsensus `json:"fields"`
}

// FoldReason explains why one source page was folded into another source
// component. It is additive metadata: scoring continues to depend only on
// Pages and IndependentRoots.
type FoldReason string

const (
	FoldReasonSameRoot      FoldReason = "same_root"
	FoldReasonNearDuplicate FoldReason = "near_duplicate"
	FoldReasonQuoteLineage  FoldReason = "quote_lineage"
)

// Agreement reports unique pages and independent evidence components for one
// value. Pages are counted only after canonical-URL deduplication.
type Agreement struct {
	Pages            int        `json:"pages"`
	IndependentRoots int        `json:"independent_roots"`
	FoldReason       FoldReason `json:"fold_reason,omitempty"`
}

// Support keeps a source and its provenance together. Evidence is nil when a
// caller did not supply an anchor for the field; Receipt is empty when absent.
type Support struct {
	URL        string           `json:"url"`
	Root       string           `json:"root"`
	Evidence   *evidence.Anchor `json:"evidence,omitempty"`
	Receipt    string           `json:"receipt,omitempty"`
	FoldReason FoldReason       `json:"fold_reason,omitempty"`
}

// Conflict is a non-winning candidate, or any candidate when the top score is
// tied. Its supports remain explicitly associated with their provenance.
type Conflict struct {
	Value     json.RawMessage `json:"value"`
	Agreement Agreement       `json:"agreement"`
	Supports  []Support       `json:"supports"`
}

// FieldConsensus is the result for one scalar leaf. A tied top score sets
// Ambiguous, leaves Value nil, and exposes every candidate through Conflicts.
type FieldConsensus struct {
	Value     json.RawMessage `json:"value,omitempty"`
	Agreement Agreement       `json:"agreement"`
	Supports  []Support       `json:"supports,omitempty"`
	Conflicts []Conflict      `json:"conflicts,omitempty"`
	Ambiguous bool            `json:"ambiguous,omitempty"`
}

type preparedSource struct {
	url       string
	root      string
	urlJSON   int
	rootJSON  int
	simText   uint64
	signature []byte
	document  any
	fields    map[string]scalarValue
	paths     map[string]string
	basis     map[string]evidence.Anchor
	receipts  map[string]string
	cleaned   string
	core      coreDescriptor
	lineage   lineageIndex
}

type scalarKind uint8

const (
	scalarNull scalarKind = iota
	scalarBoolean
	scalarNumber
	scalarString
)

type scalarValue struct {
	kind     scalarKind
	raw      json.RawMessage
	boolean  bool
	number   *big.Rat
	text     string
	groupKey string
}

type exactNumber struct {
	canonical string
	groupKey  string
	value     *big.Rat
}

// Merge performs field-level consensus over one to eight successful source
// results. It is pure: caller-owned mutable inputs are copied, CleanedText is
// only read during the call, and output ordering is independent of input order.
// It shares the MergeWithMaterialization implementation while preserving the
// original field-only 32 MiB output contract.
func Merge(results []SourceResult) (Result, error) {
	result, _, err := merge(results, false)
	return result, err
}

// MergeWithMaterialization returns both the existing field-level consensus and
// a typed merged document when every topology, presence, length, and scalar
// vote has an unambiguous winner. Ambiguity is a successful outcome represented
// by MaterializationStatusAmbiguous and a nil Data field.
func MergeWithMaterialization(results []SourceResult) (Result, Materialization, error) {
	return merge(results, true)
}

func merge(results []SourceResult, withMaterialization bool) (Result, Materialization, error) {
	if len(results) == 0 {
		return Result{}, Materialization{}, fmt.Errorf("%w: at least one source is required", ErrInvalidInput)
	}
	if len(results) > MaxSources {
		return Result{}, Materialization{}, fmt.Errorf("%w: sources exceed %d", ErrResourceLimit, MaxSources)
	}

	prepared := make([]preparedSource, 0, len(results))
	byURL := make(map[string]int, len(results))
	pathIdentities := make(map[string]string)
	totalDataBytes := 0
	totalMetadataBytes := 0
	for index, result := range results {
		source, dataBytes, metadataBytes, err := prepareSource(index, result)
		if err != nil {
			return Result{}, Materialization{}, err
		}
		if dataBytes > MaxTotalDataBytes-totalDataBytes {
			return Result{}, Materialization{}, fmt.Errorf("%w: source data exceeds %d aggregate bytes", ErrResourceLimit, MaxTotalDataBytes)
		}
		totalDataBytes += dataBytes
		if metadataBytes > MaxTotalMetadataBytes-totalMetadataBytes {
			return Result{}, Materialization{}, fmt.Errorf("%w: source metadata exceeds %d aggregate bytes", ErrResourceLimit, MaxTotalMetadataBytes)
		}
		totalMetadataBytes += metadataBytes
		sourcePaths := make([]string, 0, len(source.paths))
		for path := range source.paths {
			sourcePaths = append(sourcePaths, path)
		}
		sort.Strings(sourcePaths)
		for _, path := range sourcePaths {
			identity := source.paths[path]
			if prior, exists := pathIdentities[path]; exists && prior != identity {
				return Result{}, Materialization{}, fmt.Errorf("%w: evidence path %q has inconsistent JSON structure", ErrInvalidInput, path)
			}
			pathIdentities[path] = identity
		}

		if priorIndex, duplicate := byURL[source.url]; duplicate {
			if !sourcesEqual(prepared[priorIndex], source) {
				return Result{}, Materialization{}, fmt.Errorf("%w: canonical URL %q has non-identical results", ErrDuplicateSourceConflict, source.url)
			}
			continue
		}
		byURL[source.url] = len(prepared)
		prepared = append(prepared, source)
	}

	sort.Slice(prepared, func(i, j int) bool { return prepared[i].url < prepared[j].url })
	independence := buildIndependencePlan(prepared) // Merge keeps the no-cancel path.
	paths := collectPaths(prepared)
	if err := preflightOutput(paths, prepared, independence); err != nil {
		return Result{}, Materialization{}, err
	}
	materialization := Materialization{}
	if withMaterialization {
		var materializationErr error
		materialization, materializationErr = materializePrepared(prepared, independence.components)
		if materializationErr != nil {
			return Result{}, Materialization{}, materializationErr
		}
	}
	fields := make(map[string]FieldConsensus, len(paths))
	for _, path := range paths {
		fields[path] = mergeField(path, prepared, independence)
	}
	return Result{Fields: fields}, materialization, nil
}

func prepareSource(index int, result SourceResult) (preparedSource, int, int, error) {
	return prepareSourceContext(context.Background(), index, result)
}

func prepareSourceContext(ctx context.Context, index int, result SourceResult) (preparedSource, int, int, error) {
	if ctx == nil {
		return preparedSource{}, 0, 0, fmt.Errorf("%w: context is required", ErrInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return preparedSource{}, 0, 0, err
	}
	return prepareSourceBody(ctx, index, result)
}

func prepareSourceBody(ctx context.Context, index int, result SourceResult) (preparedSource, int, int, error) {
	if len(result.URL) == 0 {
		return preparedSource{}, 0, 0, fmt.Errorf("%w: source %d URL is empty", ErrInvalidInput, index)
	}
	if len(result.URL) > MaxURLBytes {
		return preparedSource{}, 0, 0, fmt.Errorf("%w: source %d URL exceeds %d bytes", ErrResourceLimit, index, MaxURLBytes)
	}
	if !utf8.ValidString(result.URL) {
		return preparedSource{}, 0, 0, fmt.Errorf("%w: source %d URL is not valid UTF-8", ErrInvalidInput, index)
	}
	if len(result.CleanedText) > MaxCleanedTextBytes {
		return preparedSource{}, 0, 0, fmt.Errorf("%w: source %d cleaned text exceeds %d bytes", ErrResourceLimit, index, MaxCleanedTextBytes)
	}
	if !utf8.ValidString(result.CleanedText) {
		return preparedSource{}, 0, 0, fmt.Errorf("%w: source %d cleaned text is not valid UTF-8", ErrInvalidInput, index)
	}
	canonicalURL, parsedURL, err := publicnet.NormalizeHTTPURL(result.URL, nil, false)
	if err != nil {
		return preparedSource{}, 0, 0, fmt.Errorf("%w: source %d URL is invalid: %v", ErrInvalidInput, index, err)
	}
	hostname := strings.ToLower(parsedURL.Hostname())
	root := ""
	if address, parseErr := netip.ParseAddr(hostname); parseErr == nil {
		root = address.Unmap().String()
	} else {
		root, err = publicsuffix.EffectiveTLDPlusOne(hostname)
		if err != nil {
			return preparedSource{}, 0, 0, fmt.Errorf("%w: source %d URL has no registrable domain", ErrInvalidInput, index)
		}
		root = strings.ToLower(root)
	}

	if len(result.Data) == 0 {
		return preparedSource{}, 0, 0, fmt.Errorf("%w: source %d data is empty", ErrInvalidInput, index)
	}
	if len(result.Data) > MaxSourceDataBytes {
		return preparedSource{}, 0, 0, fmt.Errorf("%w: source %d data exceeds %d bytes", ErrResourceLimit, index, MaxSourceDataBytes)
	}
	if !utf8.Valid(result.Data) {
		return preparedSource{}, 0, 0, fmt.Errorf("%w: source %d data is not valid UTF-8", ErrInvalidInput, index)
	}
	document, leafCount, err := decodeStrictDocument(result.Data)
	if err != nil {
		return preparedSource{}, 0, 0, fmt.Errorf("source %d data: %w", index, err)
	}
	structuralPaths, err := documentStructuralPaths(document)
	if err != nil {
		return preparedSource{}, 0, 0, fmt.Errorf("source %d data: %w", index, err)
	}
	if len(structuralPaths) != leafCount {
		return preparedSource{}, 0, 0, fmt.Errorf("%w: source %d data contains colliding evidence paths", ErrInvalidInput, index)
	}
	leaves, err := evidence.LeafValues(result.Data)
	if err != nil {
		return preparedSource{}, 0, 0, fmt.Errorf("%w: source %d data cannot be flattened", ErrInvalidInput, index)
	}
	if len(leaves) != leafCount {
		return preparedSource{}, 0, 0, fmt.Errorf("%w: source %d data contains colliding evidence paths", ErrInvalidInput, index)
	}
	fields := make(map[string]scalarValue, len(leaves))
	leafPaths := make([]string, 0, len(leaves))
	for path := range leaves {
		leafPaths = append(leafPaths, path)
	}
	sort.Strings(leafPaths)
	for _, path := range leafPaths {
		raw := leaves[path]
		if err := validatePath(path); err != nil {
			return preparedSource{}, 0, 0, fmt.Errorf("source %d field path: %w", index, err)
		}
		value, err := canonicalScalar(raw)
		if err != nil {
			return preparedSource{}, 0, 0, fmt.Errorf("source %d field %q: %w", index, path, err)
		}
		fields[path] = value
	}

	basis, basisBytes, err := prepareBasis(index, result.Basis, fields)
	if err != nil {
		return preparedSource{}, 0, 0, err
	}
	var core coreDescriptor
	var lineage lineageIndex
	if result.CleanedText != "" {
		anchors, anchorErr := selectCoreAnchorsContext(ctx, result.CleanedText, basis)
		if anchorErr != nil {
			return preparedSource{}, 0, 0, fmt.Errorf("source %d enhanced evidence: %w", index, anchorErr)
		}
		if err := ctx.Err(); err != nil {
			return preparedSource{}, 0, 0, err
		}
		core = buildContentCoreFromAnchors(result.CleanedText, anchors)
		lineage = buildLineageIndexFromAnchors(result.CleanedText, anchors)
	}
	receipts, receiptBytes, err := prepareReceipts(index, result.Receipts, fields)
	if err != nil {
		return preparedSource{}, 0, 0, err
	}
	metadataBytes := basisBytes
	if receiptBytes > MaxTotalMetadataBytes-metadataBytes {
		return preparedSource{}, 0, 0, fmt.Errorf("%w: source %d metadata exceeds %d bytes", ErrResourceLimit, index, MaxTotalMetadataBytes)
	}
	metadataBytes += receiptBytes

	return preparedSource{
		url:       canonicalURL,
		root:      root,
		urlJSON:   jsonMarshalStringLen(canonicalURL),
		rootJSON:  jsonMarshalStringLen(root),
		simText:   result.SimText,
		signature: documentSignature(document),
		document:  document,
		fields:    fields,
		paths:     structuralPaths,
		basis:     basis,
		receipts:  receipts,
		cleaned:   result.CleanedText,
		core:      core,
		lineage:   lineage,
	}, len(result.Data), metadataBytes, nil
}

func decodeStrictDocument(data json.RawMessage) (any, int, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	nodes := 0
	leaves := 0
	canonicalBytes := 0
	document, err := decodeStrictValue(decoder, 0, &nodes, &leaves, &canonicalBytes)
	if err != nil {
		return nil, 0, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, 0, fmt.Errorf("%w: data contains trailing JSON", ErrInvalidInput)
		}
		return nil, 0, fmt.Errorf("%w: read trailing JSON", ErrInvalidInput)
	}
	return document, leaves, nil
}

func decodeStrictValue(decoder *json.Decoder, depth int, nodes, leaves, canonicalBytes *int) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("%w: malformed JSON", ErrInvalidInput)
	}
	(*nodes)++
	if *nodes > MaxJSONNodesPerSource {
		return nil, fmt.Errorf("%w: JSON nodes exceed %d", ErrResourceLimit, MaxJSONNodesPerSource)
	}

	delimiter, container := token.(json.Delim)
	if !container {
		(*leaves)++
		if *leaves > MaxLeavesPerSource {
			return nil, fmt.Errorf("%w: JSON leaves exceed %d", ErrResourceLimit, MaxLeavesPerSource)
		}
		switch value := token.(type) {
		case nil:
			if err := reserveCanonicalBytes(canonicalBytes, len("null")); err != nil {
				return nil, err
			}
			return value, nil
		case bool:
			if err := reserveCanonicalBytes(canonicalBytes, len(strconv.FormatBool(value))); err != nil {
				return nil, err
			}
			return value, nil
		case string:
			if err := reserveCanonicalBytes(canonicalBytes, len(canonicalJSONString(value))); err != nil {
				return nil, err
			}
			return value, nil
		case json.Number:
			number, err := normalizeNumber(value.String())
			if err != nil {
				return nil, err
			}
			if err := reserveCanonicalBytes(canonicalBytes, len(number.canonical)); err != nil {
				return nil, err
			}
			return number, nil
		default:
			return nil, fmt.Errorf("%w: unsupported JSON token", ErrInvalidInput)
		}
	}

	if depth >= MaxJSONDepth {
		return nil, fmt.Errorf("%w: JSON depth exceeds %d", ErrResourceLimit, MaxJSONDepth)
	}
	switch delimiter {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, fmt.Errorf("%w: malformed JSON object", ErrInvalidInput)
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("%w: JSON object key is not a string", ErrInvalidInput)
			}
			if _, duplicate := object[key]; duplicate {
				return nil, fmt.Errorf("%w: JSON object contains a duplicate key", ErrInvalidInput)
			}
			child, err := decodeStrictValue(decoder, depth+1, nodes, leaves, canonicalBytes)
			if err != nil {
				return nil, err
			}
			object[key] = child
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return nil, fmt.Errorf("%w: malformed JSON object", ErrInvalidInput)
		}
		return object, nil
	case '[':
		array := make([]any, 0)
		for decoder.More() {
			child, err := decodeStrictValue(decoder, depth+1, nodes, leaves, canonicalBytes)
			if err != nil {
				return nil, err
			}
			array = append(array, child)
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return nil, fmt.Errorf("%w: malformed JSON array", ErrInvalidInput)
		}
		return array, nil
	default:
		return nil, fmt.Errorf("%w: unexpected JSON delimiter", ErrInvalidInput)
	}
}

func canonicalScalar(raw json.RawMessage) (scalarValue, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return scalarValue{}, fmt.Errorf("%w: malformed scalar", ErrInvalidInput)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return scalarValue{}, fmt.Errorf("%w: scalar contains trailing JSON", ErrInvalidInput)
	}
	switch value := decoded.(type) {
	case nil:
		return scalarValue{kind: scalarNull, raw: json.RawMessage("null"), groupKey: "null"}, nil
	case bool:
		encoded := json.RawMessage(strconv.FormatBool(value))
		return scalarValue{kind: scalarBoolean, raw: encoded, boolean: value, groupKey: "bool:" + string(encoded)}, nil
	case json.Number:
		number, err := normalizeNumber(value.String())
		if err != nil {
			return scalarValue{}, err
		}
		return scalarValue{
			kind: scalarNumber, raw: json.RawMessage(number.canonical), number: number.value,
			groupKey: "number:" + number.groupKey,
		}, nil
	case string:
		encoded := canonicalJSONString(value)
		return scalarValue{kind: scalarString, raw: encoded, text: value, groupKey: "string:" + string(encoded)}, nil
	default:
		return scalarValue{}, fmt.Errorf("%w: value is not a JSON scalar", ErrInvalidInput)
	}
}

func reserveCanonicalBytes(used *int, size int) error {
	if size < 0 || *used < 0 || *used > MaxSourceDataBytes || size > MaxSourceDataBytes-*used {
		return fmt.Errorf("%w: canonical scalar values exceed %d bytes", ErrResourceLimit, MaxSourceDataBytes)
	}
	*used += size
	return nil
}

func canonicalJSONString(value string) json.RawMessage {
	output := make([]byte, 0, len(value)+2)
	output = append(output, '"')
	for _, character := range value {
		switch character {
		case '"', '\\':
			output = append(output, '\\', byte(character))
		case '\b':
			output = append(output, `\b`...)
		case '\f':
			output = append(output, `\f`...)
		case '\n':
			output = append(output, `\n`...)
		case '\r':
			output = append(output, `\r`...)
		case '\t':
			output = append(output, `\t`...)
		default:
			if character < 0x20 {
				const hexadecimal = "0123456789abcdef"
				output = append(output, '\\', 'u', '0', '0', hexadecimal[character>>4], hexadecimal[character&0xf])
				continue
			}
			output = utf8.AppendRune(output, character)
		}
	}
	return append(output, '"')
}

func normalizeNumber(raw string) (exactNumber, error) {
	if len(raw) == 0 {
		return exactNumber{}, fmt.Errorf("%w: number is empty", ErrInvalidInput)
	}
	if len(raw) > MaxNumberBytes {
		return exactNumber{}, fmt.Errorf("%w: number exceeds %d bytes", ErrResourceLimit, MaxNumberBytes)
	}

	negative := raw[0] == '-'
	unsigned := raw
	if negative {
		unsigned = unsigned[1:]
	}
	exponent := 0
	if exponentIndex := strings.IndexAny(unsigned, "eE"); exponentIndex >= 0 {
		exponentText := unsigned[exponentIndex+1:]
		unsigned = unsigned[:exponentIndex]
		if len(exponentText) == 0 || len(exponentText) > 6 {
			return exactNumber{}, fmt.Errorf("%w: number exponent exceeds %d", ErrResourceLimit, MaxNumberExponent)
		}
		parsed, err := strconv.Atoi(exponentText)
		if err != nil || parsed < -MaxNumberExponent || parsed > MaxNumberExponent {
			return exactNumber{}, fmt.Errorf("%w: number exponent exceeds %d", ErrResourceLimit, MaxNumberExponent)
		}
		exponent = parsed
	}

	fractionDigits := 0
	digits := unsigned
	if dot := strings.IndexByte(unsigned, '.'); dot >= 0 {
		fractionDigits = len(unsigned) - dot - 1
		digits = unsigned[:dot] + unsigned[dot+1:]
	}
	integer := new(big.Int)
	if digits == "" {
		return exactNumber{}, fmt.Errorf("%w: malformed number", ErrInvalidInput)
	}
	if _, ok := integer.SetString(digits, 10); !ok {
		return exactNumber{}, fmt.Errorf("%w: malformed number", ErrInvalidInput)
	}
	if integer.Sign() == 0 {
		return exactNumber{canonical: "0", groupKey: "0", value: new(big.Rat)}, nil
	}
	if negative {
		integer.Neg(integer)
	}
	scale := fractionDigits - exponent
	rational := new(big.Rat).SetInt(integer)
	if scale != 0 {
		power := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(abs(scale))), nil)
		if scale > 0 {
			rational.Quo(rational, new(big.Rat).SetInt(power))
		} else {
			rational.Mul(rational, new(big.Rat).SetInt(power))
		}
	}

	canonical := canonicalDecimal(digits, fractionDigits, exponent, negative)
	if len(canonical) > MaxCanonicalNumberBytes {
		return exactNumber{}, fmt.Errorf("%w: canonical number exceeds %d bytes", ErrResourceLimit, MaxCanonicalNumberBytes)
	}
	return exactNumber{canonical: canonical, groupKey: rational.RatString(), value: rational}, nil
}

func canonicalDecimal(digits string, fractionDigits, exponent int, negative bool) string {
	leading := 0
	for leading < len(digits) && digits[leading] == '0' {
		leading++
	}
	if leading == len(digits) {
		return "0"
	}
	significant := digits[leading:]
	decimalPosition := len(digits) - fractionDigits + exponent - leading
	for len(significant) > 1 && decimalPosition < len(significant) && significant[len(significant)-1] == '0' {
		significant = significant[:len(significant)-1]
	}

	var builder strings.Builder
	if negative {
		builder.WriteByte('-')
	}
	switch {
	case decimalPosition <= 0:
		builder.WriteString("0.")
		builder.WriteString(strings.Repeat("0", -decimalPosition))
		builder.WriteString(significant)
	case decimalPosition >= len(significant):
		builder.WriteString(significant)
		builder.WriteString(strings.Repeat("0", decimalPosition-len(significant)))
	default:
		builder.WriteString(significant[:decimalPosition])
		builder.WriteByte('.')
		builder.WriteString(significant[decimalPosition:])
	}
	return builder.String()
}

func prepareBasis(index int, input map[string]evidence.Anchor, fields map[string]scalarValue) (map[string]evidence.Anchor, int, error) {
	if len(input) > MaxLeavesPerSource {
		return nil, 0, fmt.Errorf("%w: source %d evidence entries exceed %d", ErrResourceLimit, index, MaxLeavesPerSource)
	}
	if input == nil {
		return nil, 0, nil
	}
	output := make(map[string]evidence.Anchor, len(input))
	used := 0
	paths := make([]string, 0, len(input))
	for path := range input {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		anchor := input[path]
		if err := validateMetadataPath(path, fields); err != nil {
			return nil, 0, fmt.Errorf("source %d evidence: %w", index, err)
		}
		if err := validateAnchor(anchor); err != nil {
			return nil, 0, fmt.Errorf("source %d evidence path %q: %w", index, path, err)
		}
		size := len(path) + len(anchor.Quote) + len(anchor.Selector) + len(anchor.Method) + len(anchor.SnapshotID) + 64
		if size > MaxTotalMetadataBytes-used {
			return nil, 0, fmt.Errorf("%w: source %d evidence exceeds %d bytes", ErrResourceLimit, index, MaxTotalMetadataBytes)
		}
		used += size
		anchor.FetchedAt = canonicalTime(anchor.FetchedAt)
		output[path] = anchor
	}
	return output, used, nil
}

func prepareReceipts(index int, input map[string]string, fields map[string]scalarValue) (map[string]string, int, error) {
	if len(input) > MaxLeavesPerSource {
		return nil, 0, fmt.Errorf("%w: source %d receipt entries exceed %d", ErrResourceLimit, index, MaxLeavesPerSource)
	}
	if input == nil {
		return nil, 0, nil
	}
	output := make(map[string]string, len(input))
	used := 0
	paths := make([]string, 0, len(input))
	for path := range input {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		receipt := input[path]
		if err := validateMetadataPath(path, fields); err != nil {
			return nil, 0, fmt.Errorf("source %d receipt: %w", index, err)
		}
		if !utf8.ValidString(receipt) {
			return nil, 0, fmt.Errorf("%w: source %d receipt path %q is not valid UTF-8", ErrInvalidInput, index, path)
		}
		if len(receipt) > MaxReceiptBytes {
			return nil, 0, fmt.Errorf("%w: source %d receipt path %q exceeds %d bytes", ErrResourceLimit, index, path, MaxReceiptBytes)
		}
		size := len(path) + len(receipt) + 16
		if size > MaxTotalMetadataBytes-used {
			return nil, 0, fmt.Errorf("%w: source %d receipts exceed %d bytes", ErrResourceLimit, index, MaxTotalMetadataBytes)
		}
		used += size
		output[path] = receipt
	}
	return output, used, nil
}

func validatePath(path string) error {
	if path == "" || !utf8.ValidString(path) {
		return fmt.Errorf("%w: path is empty or invalid UTF-8", ErrInvalidInput)
	}
	if len(path) > MaxPathBytes {
		return fmt.Errorf("%w: path exceeds %d bytes", ErrResourceLimit, MaxPathBytes)
	}
	return nil
}

func validateMetadataPath(path string, fields map[string]scalarValue) error {
	if err := validatePath(path); err != nil {
		return err
	}
	if _, exists := fields[path]; !exists {
		return fmt.Errorf("%w: path %q is not present in data", ErrInvalidInput, path)
	}
	return nil
}

func validateAnchor(anchor evidence.Anchor) error {
	for _, field := range []struct {
		label string
		value string
	}{
		{label: "quote", value: anchor.Quote},
		{label: "selector", value: anchor.Selector},
		{label: "method", value: string(anchor.Method)},
		{label: "snapshot_id", value: anchor.SnapshotID},
	} {
		if !utf8.ValidString(field.value) {
			return fmt.Errorf("%w: anchor %s is not valid UTF-8", ErrInvalidInput, field.label)
		}
	}
	if len(anchor.Quote) > MaxEvidenceQuoteBytes {
		return fmt.Errorf("%w: anchor quote exceeds %d bytes", ErrResourceLimit, MaxEvidenceQuoteBytes)
	}
	if len(anchor.Selector) > MaxEvidenceSelectorBytes {
		return fmt.Errorf("%w: anchor selector exceeds %d bytes", ErrResourceLimit, MaxEvidenceSelectorBytes)
	}
	if len(anchor.SnapshotID) > MaxEvidenceSnapshotIDBytes {
		return fmt.Errorf("%w: anchor snapshot ID exceeds %d bytes", ErrResourceLimit, MaxEvidenceSnapshotIDBytes)
	}
	if len(anchor.Method) > MaxEvidenceMethodBytes {
		return fmt.Errorf("%w: anchor method exceeds %d bytes", ErrResourceLimit, MaxEvidenceMethodBytes)
	}
	if anchor.TextRange[0] < 0 || anchor.TextRange[1] < anchor.TextRange[0] {
		return fmt.Errorf("%w: anchor text range is invalid", ErrInvalidInput)
	}
	if _, err := canonicalTime(anchor.FetchedAt).MarshalJSON(); err != nil {
		return fmt.Errorf("%w: anchor fetched_at is invalid", ErrInvalidInput)
	}
	return nil
}

func canonicalTime(value time.Time) time.Time {
	return value.Round(0).UTC()
}

func documentSignature(document any) []byte {
	return appendDocumentSignature(nil, document)
}

func documentStructuralPaths(document any) (map[string]string, error) {
	paths := make(map[string]string)
	used := 0
	if err := appendStructuralPaths(paths, "", nil, document, &used); err != nil {
		return nil, err
	}
	return paths, nil
}

func appendStructuralPaths(paths map[string]string, outward string, identity []byte, document any, used *int) error {
	switch value := document.(type) {
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			childIdentity := append([]byte(nil), identity...)
			childIdentity = append(childIdentity, 'o')
			childIdentity = binary.AppendUvarint(childIdentity, uint64(len(key)))
			childIdentity = append(childIdentity, key...)
			if err := appendStructuralPaths(paths, joinEvidencePath(outward, key), childIdentity, value[key], used); err != nil {
				return err
			}
		}
	case []any:
		for index, child := range value {
			childIdentity := append([]byte(nil), identity...)
			childIdentity = append(childIdentity, 'a')
			childIdentity = binary.AppendUvarint(childIdentity, uint64(index))
			if err := appendStructuralPaths(paths, joinEvidencePath(outward, strconv.Itoa(index)), childIdentity, child, used); err != nil {
				return err
			}
		}
	case exactNumber, string, bool, nil:
		path := outward
		if path == "" {
			path = "$"
			if len(identity) == 0 {
				identity = []byte{'r'}
			}
		}
		if err := validatePath(path); err != nil {
			return err
		}
		size := len(path) + len(identity) + 32
		if size < 0 || *used < 0 || *used > MaxSourceDataBytes || size > MaxSourceDataBytes-*used {
			return fmt.Errorf("%w: structural evidence paths exceed %d bytes", ErrResourceLimit, MaxSourceDataBytes)
		}
		*used += size
		encodedIdentity := string(identity)
		if prior, exists := paths[path]; exists && prior != encodedIdentity {
			return fmt.Errorf("%w: evidence path %q is structurally ambiguous", ErrInvalidInput, path)
		}
		paths[path] = encodedIdentity
	default:
		return fmt.Errorf("%w: unsupported decoded JSON structure", ErrInvalidInput)
	}
	return nil
}

func joinEvidencePath(base, component string) string {
	component = strings.ReplaceAll(component, ".", `\.`)
	if base == "" {
		return component
	}
	return base + "." + component
}

func appendDocumentSignature(output []byte, document any) []byte {
	switch value := document.(type) {
	case map[string]any:
		output = append(output, 'o')
		output = binary.AppendUvarint(output, uint64(len(value)))
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			output = appendLengthPrefixed(output, key)
			output = appendDocumentSignature(output, value[key])
		}
	case []any:
		output = append(output, 'a')
		output = binary.AppendUvarint(output, uint64(len(value)))
		for _, child := range value {
			output = appendDocumentSignature(output, child)
		}
	case exactNumber:
		output = append(output, 'n')
		output = appendLengthPrefixed(output, value.groupKey)
	case string:
		output = append(output, 's')
		output = appendLengthPrefixed(output, value)
	case bool:
		if value {
			output = append(output, 't')
		} else {
			output = append(output, 'f')
		}
	case nil:
		output = append(output, 'z')
	}
	return output
}

func appendLengthPrefixed(output []byte, value string) []byte {
	output = binary.AppendUvarint(output, uint64(len(value)))
	return append(output, value...)
}

func sourcesEqual(first, second preparedSource) bool {
	if first.simText != second.simText || first.cleaned != second.cleaned || !bytes.Equal(first.signature, second.signature) ||
		len(first.basis) != len(second.basis) || len(first.receipts) != len(second.receipts) {
		return false
	}
	for path, anchor := range first.basis {
		if other, exists := second.basis[path]; !exists || anchor != other {
			return false
		}
	}
	for path, receipt := range first.receipts {
		if other, exists := second.receipts[path]; !exists || receipt != other {
			return false
		}
	}
	return true
}

type disjointSet struct {
	parent []int
	rank   []uint8
}

func newDisjointSet(size int) *disjointSet {
	parent := make([]int, size)
	for index := range parent {
		parent[index] = index
	}
	return &disjointSet{parent: parent, rank: make([]uint8, size)}
}

func (set *disjointSet) find(value int) int {
	for set.parent[value] != value {
		set.parent[value] = set.parent[set.parent[value]]
		value = set.parent[value]
	}
	return value
}

func (set *disjointSet) union(first, second int) {
	first = set.find(first)
	second = set.find(second)
	if first == second {
		return
	}
	if set.rank[first] < set.rank[second] {
		first, second = second, first
	}
	set.parent[second] = first
	if set.rank[first] == set.rank[second] {
		set.rank[first]++
	}
}

type independenceEdge struct {
	first  int
	second int
	reason FoldReason
}

type independencePlan struct {
	components   []int
	forest       []independenceEdge
	parent       []int
	parentReason []FoldReason
}

type independenceNeighbor struct {
	sourceID int
	reason   FoldReason
}

func buildIndependencePlan(sources []preparedSource) independencePlan {
	plan, _ := buildIndependencePlanContext(context.Background(), sources)
	return plan
}

func buildIndependencePlanContext(ctx context.Context, sources []preparedSource) (independencePlan, error) {
	if ctx == nil {
		return independencePlan{}, fmt.Errorf("%w: context is required", ErrInvalidInput)
	}
	if err := ctx.Err(); err != nil {
		return independencePlan{}, err
	}
	candidates := make([]independenceEdge, 0, len(sources)*(len(sources)-1)/2)
	for first := range sources {
		if err := ctx.Err(); err != nil {
			return independencePlan{}, err
		}
		for second := first + 1; second < len(sources); second++ {
			if err := ctx.Err(); err != nil {
				return independencePlan{}, err
			}
			reason := sourcePairFoldReason(sources[first], sources[second])
			if reason == "" {
				continue
			}
			edgeFirst, edgeSecond := first, second
			if sources[edgeSecond].url < sources[edgeFirst].url {
				edgeFirst, edgeSecond = edgeSecond, edgeFirst
			}
			candidates = append(candidates, independenceEdge{first: edgeFirst, second: edgeSecond, reason: reason})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		first, second := candidates[i], candidates[j]
		if foldReasonPriority(first.reason) != foldReasonPriority(second.reason) {
			return foldReasonPriority(first.reason) < foldReasonPriority(second.reason)
		}
		if sources[first.first].url != sources[second.first].url {
			return sources[first.first].url < sources[second.first].url
		}
		return sources[first.second].url < sources[second.second].url
	})

	set := newDisjointSet(len(sources))
	forest := make([]independenceEdge, 0, len(sources))
	for _, edge := range candidates {
		if set.find(edge.first) == set.find(edge.second) {
			continue
		}
		set.union(edge.first, edge.second)
		forest = append(forest, edge)
	}

	adjacency := make([][]independenceNeighbor, len(sources))
	for _, edge := range forest {
		adjacency[edge.first] = append(adjacency[edge.first], independenceNeighbor{sourceID: edge.second, reason: edge.reason})
		adjacency[edge.second] = append(adjacency[edge.second], independenceNeighbor{sourceID: edge.first, reason: edge.reason})
	}
	for sourceID := range adjacency {
		sort.Slice(adjacency[sourceID], func(i, j int) bool {
			return sources[adjacency[sourceID][i].sourceID].url < sources[adjacency[sourceID][j].sourceID].url
		})
	}

	order := make([]int, len(sources))
	for sourceID := range sources {
		order[sourceID] = sourceID
	}
	sort.Slice(order, func(i, j int) bool { return sources[order[i]].url < sources[order[j]].url })
	components := make([]int, len(sources))
	parent := make([]int, len(sources))
	parentReason := make([]FoldReason, len(sources))
	for sourceID := range sources {
		components[sourceID] = -1
		parent[sourceID] = -1
	}
	for _, representative := range order {
		if components[representative] >= 0 {
			continue
		}
		components[representative] = representative
		queue := []int{representative}
		for len(queue) > 0 {
			current := queue[0]
			queue = queue[1:]
			for _, neighbor := range adjacency[current] {
				if components[neighbor.sourceID] >= 0 {
					continue
				}
				components[neighbor.sourceID] = representative
				parent[neighbor.sourceID] = current
				parentReason[neighbor.sourceID] = neighbor.reason
				queue = append(queue, neighbor.sourceID)
			}
		}
	}
	return independencePlan{
		components:   components,
		forest:       forest,
		parent:       parent,
		parentReason: parentReason,
	}, nil
}

func sourcePairFoldReason(first, second preparedSource) FoldReason {
	if first.root == second.root {
		return FoldReasonSameRoot
	}
	if quoteLineageSimilar(first.lineage, first.fields, second.lineage, second.fields) {
		return FoldReasonQuoteLineage
	}
	legacySimilar := first.simText != 0 && second.simText != 0 &&
		simhash.Distance(first.simText, second.simText) <= independenceDistance
	if legacySimilar || contentCoresSimilar(first.core, second.core) {
		return FoldReasonNearDuplicate
	}
	return ""
}

func foldReasonPriority(reason FoldReason) int {
	switch reason {
	case FoldReasonSameRoot:
		return 0
	case FoldReasonQuoteLineage:
		return 1
	case FoldReasonNearDuplicate:
		return 2
	default:
		return 3
	}
}

func (plan independencePlan) agreementFoldReason(sourceIDs []int) FoldReason {
	if len(sourceIDs) < 2 {
		return ""
	}
	byComponent := make(map[int][]int, len(sourceIDs))
	for _, sourceID := range sourceIDs {
		byComponent[plan.components[sourceID]] = append(byComponent[plan.components[sourceID]], sourceID)
	}
	reasons := make(map[FoldReason]struct{}, 2)
	for _, members := range byComponent {
		if len(members) < 2 {
			continue
		}
		for _, member := range members[1:] {
			if !plan.collectPathReasons(members[0], member, reasons) || len(reasons) > 1 {
				return ""
			}
		}
	}
	if len(reasons) != 1 {
		return ""
	}
	for reason := range reasons {
		return reason
	}
	return ""
}

func (plan independencePlan) collectPathReasons(first, second int, reasons map[FoldReason]struct{}) bool {
	ancestors := make(map[int]struct{}, len(plan.parent))
	for current := first; current >= 0; current = plan.parent[current] {
		ancestors[current] = struct{}{}
	}
	common := second
	for {
		if _, exists := ancestors[common]; exists {
			break
		}
		reason := plan.parentReason[common]
		if reason == "" {
			return false
		}
		reasons[reason] = struct{}{}
		common = plan.parent[common]
		if common < 0 {
			return false
		}
	}
	for current := first; current != common; current = plan.parent[current] {
		reason := plan.parentReason[current]
		if reason == "" {
			return false
		}
		reasons[reason] = struct{}{}
	}
	return true
}

// independentComponents remains the narrow numerical view used by
// materialization benchmarks and compatibility tests.
func independentComponents(sources []preparedSource) []int {
	return buildIndependencePlan(sources).components
}

func collectPaths(sources []preparedSource) []string {
	set := make(map[string]struct{})
	for _, source := range sources {
		for path := range source.fields {
			set[path] = struct{}{}
		}
	}
	paths := make([]string, 0, len(set))
	for path := range set {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

type valueGroup struct {
	value     scalarValue
	sourceIDs []int
	agreement Agreement
}

type fieldPlan struct {
	groups    []*valueGroup
	ambiguous bool
}

func planField(path string, sources []preparedSource, independence independencePlan) fieldPlan {
	byValue := make(map[string]*valueGroup)
	for sourceID, source := range sources {
		value, exists := source.fields[path]
		if !exists {
			continue
		}
		group := byValue[value.groupKey]
		if group == nil {
			group = &valueGroup{value: value}
			byValue[value.groupKey] = group
		}
		group.sourceIDs = append(group.sourceIDs, sourceID)
	}
	groups := make([]*valueGroup, 0, len(byValue))
	for _, group := range byValue {
		independent := make(map[int]struct{}, len(group.sourceIDs))
		for _, sourceID := range group.sourceIDs {
			independent[independence.components[sourceID]] = struct{}{}
		}
		group.agreement = Agreement{
			Pages:            len(group.sourceIDs),
			IndependentRoots: len(independent),
			FoldReason:       independence.agreementFoldReason(group.sourceIDs),
		}
		groups = append(groups, group)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].agreement.IndependentRoots != groups[j].agreement.IndependentRoots {
			return groups[i].agreement.IndependentRoots > groups[j].agreement.IndependentRoots
		}
		if groups[i].agreement.Pages != groups[j].agreement.Pages {
			return groups[i].agreement.Pages > groups[j].agreement.Pages
		}
		return scalarLess(groups[i].value, groups[j].value)
	})

	return fieldPlan{
		groups:    groups,
		ambiguous: len(groups) > 1 && sameScore(groups[0].agreement, groups[1].agreement),
	}
}

func mergeField(path string, sources []preparedSource, independence independencePlan) FieldConsensus {
	fieldPlan := planField(path, sources, independence)
	if fieldPlan.ambiguous {
		groups := fieldPlan.groups
		conflicts := make([]Conflict, 0, len(groups))
		for _, group := range groups {
			conflicts = append(conflicts, makeConflict(path, group, sources, independence))
		}
		return FieldConsensus{Conflicts: conflicts, Ambiguous: true}
	}

	groups := fieldPlan.groups
	winner := groups[0]
	field := FieldConsensus{
		Value:     append(json.RawMessage(nil), winner.value.raw...),
		Agreement: winner.agreement,
		Supports:  makeSupports(path, winner.sourceIDs, sources, independence),
	}
	if len(groups) > 1 {
		field.Conflicts = make([]Conflict, 0, len(groups)-1)
		for _, group := range groups[1:] {
			field.Conflicts = append(field.Conflicts, makeConflict(path, group, sources, independence))
		}
	}
	return field
}

type outputBudget struct {
	used int
}

func (budget *outputBudget) reserve(size int) error {
	if budget == nil || size < 0 || budget.used < 0 || budget.used > MaxOutputBytes || size > MaxOutputBytes-budget.used {
		return fmt.Errorf("%w: consensus output exceeds %d bytes", ErrResourceLimit, MaxOutputBytes)
	}
	budget.used += size
	return nil
}

func preflightOutput(paths []string, sources []preparedSource, independence independencePlan) error {
	_, err := measuredOutputSize(paths, sources, independence)
	return err
}

func measuredOutputSize(paths []string, sources []preparedSource, independence independencePlan) (int, error) {
	budget := &outputBudget{}
	if err := budget.reserve(len(`{"fields":{`)); err != nil {
		return 0, err
	}
	for index, path := range paths {
		if index > 0 {
			if err := budget.reserve(1); err != nil { // map-entry comma
				return 0, err
			}
		}
		if err := budget.reserve(jsonMarshalStringLen(path) + 1); err != nil { // key + colon
			return 0, err
		}
		if err := reserveFieldOutput(budget, path, planField(path, sources, independence), sources, independence); err != nil {
			return 0, err
		}
	}
	if err := budget.reserve(len(`}}`)); err != nil {
		return 0, err
	}
	return budget.used, nil
}

func reserveFieldOutput(budget *outputBudget, path string, plan fieldPlan, sources []preparedSource, independence independencePlan) error {
	if err := budget.reserve(1); err != nil { // {
		return err
	}
	properties := 0
	property := func(prefix string) error {
		if properties > 0 {
			if err := budget.reserve(1); err != nil { // property comma
				return err
			}
		}
		properties++
		return budget.reserve(len(prefix))
	}

	if !plan.ambiguous {
		winner := plan.groups[0]
		if err := property(`"value":`); err != nil {
			return err
		}
		if err := budget.reserve(scalarJSONLen(winner.value)); err != nil {
			return err
		}
		if err := property(`"agreement":`); err != nil {
			return err
		}
		if err := reserveAgreementOutput(budget, winner.agreement); err != nil {
			return err
		}
		if err := property(`"supports":`); err != nil {
			return err
		}
		if err := reserveSupportsOutput(budget, path, winner.sourceIDs, sources, independence); err != nil {
			return err
		}
	}

	// Agreement is never omitted. In the ambiguous case it is the zero value.
	if plan.ambiguous {
		if err := property(`"agreement":`); err != nil {
			return err
		}
		if err := reserveAgreementOutput(budget, Agreement{}); err != nil {
			return err
		}
	}
	conflictStart := 1
	if plan.ambiguous {
		conflictStart = 0
	}
	if len(plan.groups) > conflictStart {
		if err := property(`"conflicts":`); err != nil {
			return err
		}
		if err := budget.reserve(1); err != nil { // [
			return err
		}
		for index, group := range plan.groups[conflictStart:] {
			if index > 0 {
				if err := budget.reserve(1); err != nil {
					return err
				}
			}
			if err := reserveConflictOutput(budget, path, group, sources, independence); err != nil {
				return err
			}
		}
		if err := budget.reserve(1); err != nil { // ]
			return err
		}
	}
	if plan.ambiguous {
		if err := property(`"ambiguous":`); err != nil {
			return err
		}
		if err := budget.reserve(len("true")); err != nil {
			return err
		}
	}
	return budget.reserve(1) // }
}

func reserveAgreementOutput(budget *outputBudget, agreement Agreement) error {
	if err := budget.reserve(len(`{"pages":`) + decimalIntLen(agreement.Pages)); err != nil {
		return err
	}
	if err := budget.reserve(len(`,"independent_roots":`) + decimalIntLen(agreement.IndependentRoots)); err != nil {
		return err
	}
	if agreement.FoldReason != "" {
		if err := budget.reserve(len(`,"fold_reason":`) + jsonMarshalStringLen(string(agreement.FoldReason))); err != nil {
			return err
		}
	}
	return budget.reserve(1)
}

func reserveConflictOutput(budget *outputBudget, path string, group *valueGroup, sources []preparedSource, independence independencePlan) error {
	if err := budget.reserve(len(`{"value":`) + scalarJSONLen(group.value)); err != nil {
		return err
	}
	if err := budget.reserve(len(`,"agreement":`)); err != nil {
		return err
	}
	if err := reserveAgreementOutput(budget, group.agreement); err != nil {
		return err
	}
	if err := budget.reserve(len(`,"supports":`)); err != nil {
		return err
	}
	if err := reserveSupportsOutput(budget, path, group.sourceIDs, sources, independence); err != nil {
		return err
	}
	return budget.reserve(1)
}

func reserveSupportsOutput(budget *outputBudget, path string, sourceIDs []int, sources []preparedSource, independence independencePlan) error {
	if err := budget.reserve(1); err != nil { // [
		return err
	}
	for index, sourceID := range sourceIDs {
		if index > 0 {
			if err := budget.reserve(1); err != nil {
				return err
			}
		}
		if err := reserveSupportOutput(budget, path, sources[sourceID], independence.parentReason[sourceID]); err != nil {
			return err
		}
	}
	return budget.reserve(1)
}

func reserveSupportOutput(budget *outputBudget, path string, source preparedSource, reason FoldReason) error {
	if err := budget.reserve(len(`{"url":`) + source.urlJSON); err != nil {
		return err
	}
	if err := budget.reserve(len(`,"root":`) + source.rootJSON); err != nil {
		return err
	}
	if anchor, exists := source.basis[path]; exists {
		if err := budget.reserve(len(`,"evidence":`)); err != nil {
			return err
		}
		if err := reserveAnchorOutput(budget, anchor); err != nil {
			return err
		}
	}
	if receipt := source.receipts[path]; receipt != "" {
		if err := budget.reserve(len(`,"receipt":`) + jsonMarshalStringLen(receipt)); err != nil {
			return err
		}
	}
	if reason != "" {
		if err := budget.reserve(len(`,"fold_reason":`) + jsonMarshalStringLen(string(reason))); err != nil {
			return err
		}
	}
	return budget.reserve(1)
}

func reserveAnchorOutput(budget *outputBudget, anchor evidence.Anchor) error {
	if err := budget.reserve(len(`{"quote":`) + jsonMarshalStringLen(anchor.Quote)); err != nil {
		return err
	}
	if err := budget.reserve(len(`,"text_range":[`) + decimalIntLen(anchor.TextRange[0]) + 1 + decimalIntLen(anchor.TextRange[1]) + 1); err != nil {
		return err
	}
	if anchor.Selector != "" {
		if err := budget.reserve(len(`,"selector":`) + jsonMarshalStringLen(anchor.Selector)); err != nil {
			return err
		}
	}
	if err := budget.reserve(len(`,"method":`) + jsonMarshalStringLen(string(anchor.Method))); err != nil {
		return err
	}
	if err := budget.reserve(len(`,"snapshot_id":`) + jsonMarshalStringLen(anchor.SnapshotID)); err != nil {
		return err
	}
	if err := budget.reserve(len(`,"fetched_at":`) + jsonTimeLen(anchor.FetchedAt)); err != nil {
		return err
	}
	return budget.reserve(1)
}

func scalarJSONLen(value scalarValue) int {
	if value.kind == scalarString {
		return jsonMarshalStringLen(value.text)
	}
	return len(value.raw)
}

func jsonMarshalStringLen(value string) int {
	size := 2 // surrounding quotes
	for _, character := range value {
		switch character {
		case '"', '\\', '\b', '\f', '\n', '\r', '\t':
			size += 2
		case '<', '>', '&':
			size += 6
		default:
			switch {
			case character < 0x20, character == '\u2028', character == '\u2029':
				size += 6
			default:
				size += utf8.RuneLen(character)
			}
		}
	}
	return size
}

func jsonTimeLen(value time.Time) int {
	nanoseconds := canonicalTime(value).Nanosecond()
	if nanoseconds == 0 {
		return len(`"0001-01-01T00:00:00Z"`)
	}
	digits := 9
	for nanoseconds%10 == 0 {
		nanoseconds /= 10
		digits--
	}
	return len(`"0001-01-01T00:00:00Z"`) + 1 + digits
}

func decimalIntLen(value int) int {
	if value == 0 {
		return 1
	}
	size := 0
	if value < 0 {
		size++
		value = -value
	}
	for value > 0 {
		size++
		value /= 10
	}
	return size
}

func sameScore(first, second Agreement) bool {
	return first.IndependentRoots == second.IndependentRoots && first.Pages == second.Pages
}

func scalarLess(first, second scalarValue) bool {
	if first.kind != second.kind {
		return first.kind < second.kind
	}
	switch first.kind {
	case scalarNull:
		return false
	case scalarBoolean:
		return !first.boolean && second.boolean
	case scalarNumber:
		return first.number.Cmp(second.number) < 0
	case scalarString:
		return first.text < second.text
	default:
		return first.groupKey < second.groupKey
	}
}

func makeConflict(path string, group *valueGroup, sources []preparedSource, independence independencePlan) Conflict {
	return Conflict{
		Value:     append(json.RawMessage(nil), group.value.raw...),
		Agreement: group.agreement,
		Supports:  makeSupports(path, group.sourceIDs, sources, independence),
	}
}

func makeSupports(path string, sourceIDs []int, sources []preparedSource, independence independencePlan) []Support {
	supports := make([]Support, 0, len(sourceIDs))
	for _, sourceID := range sourceIDs {
		source := sources[sourceID]
		support := Support{
			URL:        source.url,
			Root:       source.root,
			Receipt:    source.receipts[path],
			FoldReason: independence.parentReason[sourceID],
		}
		if anchor, exists := source.basis[path]; exists {
			anchorCopy := anchor
			support.Evidence = &anchorCopy
		}
		supports = append(supports, support)
	}
	return supports
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
