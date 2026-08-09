// Package evidence aligns extracted JSON leaf values back to source text and
// raw HTML. Anchors are deterministic and explicitly report when a value could
// not be located.
package evidence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const alignmentContextCheckpointBytes = 4 << 10

// Method identifies how an extracted value was anchored.
type Method string

const (
	MethodExact      Method = "exact"
	MethodNormalized Method = "normalized"
	MethodFuzzy      Method = "fuzzy"
	MethodUnlocated  Method = "unlocated"
	MethodCompiled   Method = "compiled"
)

// Anchor is a field-level evidence locator. TextRange contains UTF-8 byte
// offsets into the cleaned content and uses a half-open [start,end) interval.
type Anchor struct {
	Quote      string    `json:"quote"`
	TextRange  [2]int    `json:"text_range"`
	Selector   string    `json:"selector,omitempty"`
	Method     Method    `json:"method"`
	SnapshotID string    `json:"snapshot_id"`
	FetchedAt  time.Time `json:"fetched_at"`
}

// AlignValue locates one scalar value using exact, normalized, then fuzzy
// matching. If a textual match is found, it also derives a unique CSS selector
// from rawHTML when possible.
func AlignValue(value, cleaned, rawHTML string) Anchor {
	selectors := newSelectorDocument(rawHTML)
	return alignValue(value, cleaned, selectors.find)
}

// AlignValueContext is the context-aware form of AlignValue. It returns the
// context cancellation or deadline error unchanged so callers can classify it
// with errors.Is. A nil context is invalid.
func AlignValueContext(ctx context.Context, value, cleaned, rawHTML string) (Anchor, error) {
	if ctx == nil {
		return Anchor{}, errors.New("evidence: nil context")
	}
	if err := ctx.Err(); err != nil {
		return Anchor{}, err
	}
	return alignValueContext(ctx, value, cleaned, func(ctx context.Context, quote string) (string, error) {
		selectors, err := newSelectorDocumentContext(ctx, rawHTML)
		if err != nil {
			return "", err
		}
		return selectors.findContext(ctx, quote)
	})
}

func alignValue(value, cleaned string, findSelector func(string) string) Anchor {
	anchor, _ := alignValueContext(
		context.Background(),
		value,
		cleaned,
		func(_ context.Context, quote string) (string, error) { return findSelector(quote), nil },
	)
	return anchor
}

func alignValueContext(
	ctx context.Context,
	value, cleaned string,
	findSelector func(context.Context, string) (string, error),
) (Anchor, error) {
	if err := ctx.Err(); err != nil {
		return Anchor{}, err
	}
	if value == "" {
		return Anchor{Method: MethodUnlocated}, nil
	}

	start, err := indexStringContext(ctx, cleaned, value)
	if err != nil {
		return Anchor{}, err
	}
	if start >= 0 {
		anchor := Anchor{
			Quote:     cleaned[start : start+len(value)],
			TextRange: [2]int{start, start + len(value)},
			Method:    MethodExact,
		}
		anchor.Selector, err = findSelector(ctx, anchor.Quote)
		if err != nil {
			return Anchor{}, err
		}
		if err := ctx.Err(); err != nil {
			return Anchor{}, err
		}
		return anchor, nil
	}

	normalizedCleaned, err := normalizeWithOffsetsContext(ctx, cleaned)
	if err != nil {
		return Anchor{}, err
	}
	normalizedValue, err := normalizeTextContext(ctx, value)
	if err != nil {
		return Anchor{}, err
	}
	if normalizedValue != "" {
		start, err := indexStringContext(ctx, normalizedCleaned.text, normalizedValue)
		if err != nil {
			return Anchor{}, err
		}
		if start >= 0 {
			end := start + len(normalizedValue)
			originalStart, originalEnd, ok := normalizedCleaned.originalRange(start, end)
			if ok {
				anchor := Anchor{
					Quote:     cleaned[originalStart:originalEnd],
					TextRange: [2]int{originalStart, originalEnd},
					Method:    MethodNormalized,
				}
				anchor.Selector, err = findSelector(ctx, anchor.Quote)
				if err != nil {
					return Anchor{}, err
				}
				if err := ctx.Err(); err != nil {
					return Anchor{}, err
				}
				return anchor, nil
			}
		}
	}

	quote, start, end, ok, err := fuzzyWindowContext(ctx, value, cleaned)
	if err != nil {
		return Anchor{}, err
	}
	if ok {
		anchor := Anchor{
			Quote:     quote,
			TextRange: [2]int{start, end},
			Method:    MethodFuzzy,
		}
		anchor.Selector, err = findSelector(ctx, anchor.Quote)
		if err != nil {
			return Anchor{}, err
		}
		if err := ctx.Err(); err != nil {
			return Anchor{}, err
		}
		return anchor, nil
	}

	if err := ctx.Err(); err != nil {
		return Anchor{}, err
	}
	return Anchor{Method: MethodUnlocated}, nil
}

// AlignAll walks every scalar JSON leaf and returns dot/index-path anchors plus
// the fraction that could not be located. fetchedAt is variadic to preserve the
// four-argument API from the master plan while allowing handlers to attach the
// real observation time.
func AlignAll(data json.RawMessage, cleaned, rawHTML, snapshotID string, fetchedAt ...time.Time) (map[string]Anchor, float64) {
	document, err := decodeDocument(data)
	if err != nil {
		return map[string]Anchor{}, 1
	}

	leaves := make([]leaf, 0)
	flattenLeaves("", document, &leaves)
	anchors := make(map[string]Anchor, len(leaves))
	if len(leaves) == 0 {
		return anchors, 0
	}
	var observedAt time.Time
	if len(fetchedAt) > 0 {
		observedAt = fetchedAt[0]
	}
	unlocated := 0
	selectors := newSelectorDocument(rawHTML)
	for _, item := range leaves {
		anchor := alignValue(item.value, cleaned, selectors.find)
		anchor.SnapshotID = snapshotID
		anchor.FetchedAt = observedAt
		if anchor.Method == MethodUnlocated {
			unlocated++
		}
		anchors[item.path] = anchor
	}
	return anchors, float64(unlocated) / float64(len(leaves))
}

// LeafValues returns every scalar JSON leaf using the same deterministic
// dot/index paths as AlignAll. Values retain their JSON types and bytes are
// copied so callers may safely keep them in signed receipts.
func LeafValues(data json.RawMessage) (map[string]json.RawMessage, error) {
	document, err := decodeDocument(data)
	if err != nil {
		return nil, err
	}
	leaves := make([]leaf, 0)
	flattenLeaves("", document, &leaves)
	values := make(map[string]json.RawMessage, len(leaves))
	for _, item := range leaves {
		values[item.path] = append(json.RawMessage(nil), item.raw...)
	}
	return values, nil
}

func decodeDocument(data json.RawMessage) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	return document, nil
}

type leaf struct {
	path  string
	value string
	raw   json.RawMessage
}

func flattenLeaves(path string, value any, leaves *[]leaf) {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			flattenLeaves(joinPath(path, key), typed[key], leaves)
		}
	case []any:
		for index, child := range typed {
			flattenLeaves(joinPath(path, strconv.Itoa(index)), child, leaves)
		}
	case string:
		appendLeaf(leaves, path, typed, typed)
	case json.Number:
		appendLeaf(leaves, path, typed, typed.String())
	case bool:
		appendLeaf(leaves, path, typed, strconv.FormatBool(typed))
	case nil:
		appendLeaf(leaves, path, nil, "")
	default:
		appendLeaf(leaves, path, typed, fmt.Sprint(typed))
	}
}

func appendLeaf(leaves *[]leaf, path string, value any, rendered string) {
	raw, err := json.Marshal(value)
	if err != nil {
		raw = json.RawMessage("null")
	}
	*leaves = append(*leaves, leaf{path: rootPath(path), value: rendered, raw: raw})
}

func joinPath(base, component string) string {
	component = strings.ReplaceAll(component, ".", `\.`)
	if base == "" {
		return component
	}
	return base + "." + component
}

func rootPath(path string) string {
	if path == "" {
		return "$"
	}
	return path
}

type normalizedString struct {
	text   string
	starts []int
	ends   []int
}

type contextScanTicker struct {
	remaining int
}

func newContextScanTicker() contextScanTicker {
	return contextScanTicker{remaining: alignmentContextCheckpointBytes}
}

func (ticker *contextScanTicker) step(ctx context.Context) error {
	return ticker.advance(ctx, 1)
}

func (ticker *contextScanTicker) advance(ctx context.Context, amount int) error {
	if amount < ticker.remaining {
		ticker.remaining -= amount
		return nil
	}
	overflow := amount - ticker.remaining
	ticker.remaining = alignmentContextCheckpointBytes - overflow%alignmentContextCheckpointBytes
	return ctx.Err()
}

// indexStringContext is a cancellable byte-exact substring search. Horspool's
// fixed-size shift table keeps auxiliary memory bounded independently of the
// caller-controlled value length.
func indexStringContext(ctx context.Context, text, value string) (int, error) {
	if err := ctx.Err(); err != nil {
		return -1, err
	}
	if value == "" {
		return 0, nil
	}
	if len(value) > len(text) {
		if err := ctx.Err(); err != nil {
			return -1, err
		}
		return -1, nil
	}

	ticker := newContextScanTicker()
	var shifts [256]int
	for index := range shifts {
		shifts[index] = len(value)
		if err := ticker.step(ctx); err != nil {
			return -1, err
		}
	}
	for index := 0; index < len(value)-1; index++ {
		shifts[value[index]] = len(value) - 1 - index
		if err := ticker.step(ctx); err != nil {
			return -1, err
		}
	}

	for end := len(value) - 1; end < len(text); {
		valueIndex := len(value) - 1
		textIndex := end
		for valueIndex >= 0 && text[textIndex] == value[valueIndex] {
			if err := ticker.step(ctx); err != nil {
				return -1, err
			}
			valueIndex--
			textIndex--
		}
		if valueIndex < 0 {
			if err := ctx.Err(); err != nil {
				return -1, err
			}
			return end - len(value) + 1, nil
		}
		if err := ticker.step(ctx); err != nil {
			return -1, err
		}
		shift := shifts[text[end]]
		if err := ticker.advance(ctx, shift); err != nil {
			return -1, err
		}
		end += shift
	}
	if err := ctx.Err(); err != nil {
		return -1, err
	}
	return -1, nil
}

func normalizeText(input string) string {
	normalized, _ := normalizeTextContext(context.Background(), input)
	return normalized
}

func normalizeWithOffsets(input string) normalizedString {
	normalized, _ := normalizeWithOffsetsContext(context.Background(), input)
	return normalized
}

func normalizeTextContext(ctx context.Context, input string) (string, error) {
	normalized, err := normalizeWithOffsetsContext(ctx, input)
	return normalized.text, err
}

func normalizeWithOffsetsContext(ctx context.Context, input string) (normalizedString, error) {
	if err := ctx.Err(); err != nil {
		return normalizedString{}, err
	}
	var text strings.Builder
	starts := make([]int, 0, len(input))
	ends := make([]int, 0, len(input))
	lastWasSpace := false
	nextCheckpoint := alignmentContextCheckpointBytes

	for byteOffset, originalRune := range input {
		if byteOffset >= nextCheckpoint {
			if err := ctx.Err(); err != nil {
				return normalizedString{}, err
			}
			for nextCheckpoint <= byteOffset {
				nextCheckpoint += alignmentContextCheckpointBytes
			}
		}
		originalWidth := utf8.RuneLen(originalRune)
		if originalWidth < 0 {
			originalWidth = 1
		}
		originalEnd := byteOffset + originalWidth
		r := foldWidth(originalRune)
		r = unicode.ToLower(r)
		if shouldDropForComparison(r) {
			continue
		}

		if unicode.IsSpace(r) {
			if text.Len() == 0 {
				continue
			}
			if lastWasSpace {
				ends[len(ends)-1] = originalEnd
				continue
			}
			r = ' '
			lastWasSpace = true
		} else {
			lastWasSpace = false
		}

		encoded := string(r)
		text.WriteString(encoded)
		for range len(encoded) {
			starts = append(starts, byteOffset)
			ends = append(ends, originalEnd)
		}
	}

	result := text.String()
	if strings.HasSuffix(result, " ") {
		result = strings.TrimSuffix(result, " ")
		starts = starts[:len(starts)-1]
		ends = ends[:len(ends)-1]
	}
	if err := ctx.Err(); err != nil {
		return normalizedString{}, err
	}
	return normalizedString{text: result, starts: starts, ends: ends}, nil
}

func (n normalizedString) originalRange(start, end int) (int, int, bool) {
	if start < 0 || end <= start || end > len(n.text) || start >= len(n.starts) || end-1 >= len(n.ends) {
		return 0, 0, false
	}
	return n.starts[start], n.ends[end-1], true
}

func foldWidth(r rune) rune {
	switch {
	case r == '\u3000':
		return ' '
	case r >= '\uff01' && r <= '\uff5e':
		return r - 0xfee0
	default:
		return r
	}
}

func shouldDropForComparison(r rune) bool {
	switch r {
	case ',', '$', '¥', '￥', '€', '£':
		return true
	default:
		return false
	}
}

type wordToken struct {
	value      string
	start, end int
}

func fuzzyWindow(value, cleaned string) (string, int, int, bool) {
	quote, start, end, ok, _ := fuzzyWindowContext(context.Background(), value, cleaned)
	return quote, start, end, ok
}

func fuzzyWindowContext(ctx context.Context, value, cleaned string) (string, int, int, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", 0, 0, false, err
	}
	wanted, err := tokenizeContext(ctx, value)
	if err != nil {
		return "", 0, 0, false, err
	}
	available, err := tokenizeContext(ctx, cleaned)
	if err != nil {
		return "", 0, 0, false, err
	}
	if len(wanted) == 0 || len(available) == 0 {
		return "", 0, 0, false, nil
	}
	wantedSet, err := tokenSetContext(ctx, wanted)
	if err != nil {
		return "", 0, 0, false, err
	}
	minWindow := len(wanted) - 2
	if minWindow < 1 {
		minWindow = 1
	}
	maxWindow := len(wanted) + 2
	if maxWindow > len(available) {
		maxWindow = len(available)
	}

	bestScore := 0.0
	bestStart, bestEnd, bestSize := 0, 0, 0
	for size := minWindow; size <= maxWindow; size++ {
		if err := ctx.Err(); err != nil {
			return "", 0, 0, false, err
		}
		for start := 0; start+size <= len(available); start++ {
			if start%256 == 0 {
				if err := ctx.Err(); err != nil {
					return "", 0, 0, false, err
				}
			}
			window := available[start : start+size]
			windowSet, err := tokenSetContext(ctx, window)
			if err != nil {
				return "", 0, 0, false, err
			}
			score, err := jaccardContext(ctx, wantedSet, windowSet)
			if err != nil {
				return "", 0, 0, false, err
			}
			closer := abs(size-len(wanted)) < abs(bestSize-len(wanted))
			if score > bestScore || (score == bestScore && (bestSize == 0 || closer)) {
				bestScore = score
				bestStart = window[0].start
				bestEnd = window[len(window)-1].end
				bestSize = size
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return "", 0, 0, false, err
	}
	if bestScore < 0.8 || bestEnd <= bestStart {
		return "", 0, 0, false, nil
	}
	return cleaned[bestStart:bestEnd], bestStart, bestEnd, true, nil
}

func tokenize(input string) []wordToken {
	tokens, _ := tokenizeContext(context.Background(), input)
	return tokens
}

func tokenizeContext(ctx context.Context, input string) ([]wordToken, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tokens := make([]wordToken, 0)
	start := -1
	var value strings.Builder
	nextCheckpoint := alignmentContextCheckpointBytes
	flush := func(end int) {
		if start < 0 {
			return
		}
		tokens = append(tokens, wordToken{value: value.String(), start: start, end: end})
		start = -1
		value.Reset()
	}
	for offset, originalRune := range input {
		if offset >= nextCheckpoint {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			for nextCheckpoint <= offset {
				nextCheckpoint += alignmentContextCheckpointBytes
			}
		}
		r := unicode.ToLower(foldWidth(originalRune))
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if start < 0 {
				start = offset
			}
			value.WriteRune(r)
			continue
		}
		flush(offset)
	}
	flush(len(input))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return tokens, nil
}

func tokenSet(tokens []wordToken) map[string]struct{} {
	set, _ := tokenSetContext(context.Background(), tokens)
	return set
}

func tokenSetContext(ctx context.Context, tokens []wordToken) (map[string]struct{}, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	set := make(map[string]struct{}, len(tokens))
	for index, token := range tokens {
		if index%256 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		set[token.value] = struct{}{}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return set, nil
}

func jaccard(left, right map[string]struct{}) float64 {
	score, _ := jaccardContext(context.Background(), left, right)
	return score
}

func jaccardContext(ctx context.Context, left, right map[string]struct{}) (float64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(left) == 0 && len(right) == 0 {
		return 1, nil
	}
	intersection := 0
	processed := 0
	for item := range left {
		processed++
		if processed%256 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		if _, ok := right[item]; ok {
			intersection++
		}
	}
	union := len(left) + len(right) - intersection
	if union == 0 {
		return 0, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return float64(intersection) / float64(union), nil
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
