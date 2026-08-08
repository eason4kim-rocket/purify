// Package evidence aligns extracted JSON leaf values back to source text and
// raw HTML. Anchors are deterministic and explicitly report when a value could
// not be located.
package evidence

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

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

func alignValue(value, cleaned string, findSelector func(string) string) Anchor {
	if value == "" {
		return Anchor{Method: MethodUnlocated}
	}

	if start := strings.Index(cleaned, value); start >= 0 {
		anchor := Anchor{
			Quote:     cleaned[start : start+len(value)],
			TextRange: [2]int{start, start + len(value)},
			Method:    MethodExact,
		}
		anchor.Selector = findSelector(anchor.Quote)
		return anchor
	}

	normalizedCleaned := normalizeWithOffsets(cleaned)
	normalizedValue := normalizeText(value)
	if normalizedValue != "" {
		if start := strings.Index(normalizedCleaned.text, normalizedValue); start >= 0 {
			end := start + len(normalizedValue)
			originalStart, originalEnd, ok := normalizedCleaned.originalRange(start, end)
			if ok {
				anchor := Anchor{
					Quote:     cleaned[originalStart:originalEnd],
					TextRange: [2]int{originalStart, originalEnd},
					Method:    MethodNormalized,
				}
				anchor.Selector = findSelector(anchor.Quote)
				return anchor
			}
		}
	}

	if quote, start, end, ok := fuzzyWindow(value, cleaned); ok {
		anchor := Anchor{
			Quote:     quote,
			TextRange: [2]int{start, end},
			Method:    MethodFuzzy,
		}
		anchor.Selector = findSelector(anchor.Quote)
		return anchor
	}

	return Anchor{Method: MethodUnlocated}
}

// AlignAll walks every scalar JSON leaf and returns dot/index-path anchors plus
// the fraction that could not be located. fetchedAt is variadic to preserve the
// four-argument API from the master plan while allowing handlers to attach the
// real observation time.
func AlignAll(data json.RawMessage, cleaned, rawHTML, snapshotID string, fetchedAt ...time.Time) (map[string]Anchor, float64) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
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

type leaf struct {
	path  string
	value string
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
		*leaves = append(*leaves, leaf{path: rootPath(path), value: typed})
	case json.Number:
		*leaves = append(*leaves, leaf{path: rootPath(path), value: typed.String()})
	case bool:
		*leaves = append(*leaves, leaf{path: rootPath(path), value: strconv.FormatBool(typed)})
	case nil:
		*leaves = append(*leaves, leaf{path: rootPath(path), value: ""})
	default:
		*leaves = append(*leaves, leaf{path: rootPath(path), value: fmt.Sprint(typed)})
	}
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

func normalizeText(input string) string {
	return normalizeWithOffsets(input).text
}

func normalizeWithOffsets(input string) normalizedString {
	var text strings.Builder
	starts := make([]int, 0, len(input))
	ends := make([]int, 0, len(input))
	lastWasSpace := false

	for byteOffset, originalRune := range input {
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
	return normalizedString{text: result, starts: starts, ends: ends}
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
	wanted := tokenize(value)
	available := tokenize(cleaned)
	if len(wanted) == 0 || len(available) == 0 {
		return "", 0, 0, false
	}
	wantedSet := tokenSet(wanted)
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
		for start := 0; start+size <= len(available); start++ {
			window := available[start : start+size]
			score := jaccard(wantedSet, tokenSet(window))
			closer := abs(size-len(wanted)) < abs(bestSize-len(wanted))
			if score > bestScore || (score == bestScore && (bestSize == 0 || closer)) {
				bestScore = score
				bestStart = window[0].start
				bestEnd = window[len(window)-1].end
				bestSize = size
			}
		}
	}
	if bestScore < 0.8 || bestEnd <= bestStart {
		return "", 0, 0, false
	}
	return cleaned[bestStart:bestEnd], bestStart, bestEnd, true
}

func tokenize(input string) []wordToken {
	tokens := make([]wordToken, 0)
	start := -1
	var value strings.Builder
	flush := func(end int) {
		if start < 0 {
			return
		}
		tokens = append(tokens, wordToken{value: value.String(), start: start, end: end})
		start = -1
		value.Reset()
	}
	for offset, originalRune := range input {
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
	return tokens
}

func tokenSet(tokens []wordToken) map[string]struct{} {
	set := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		set[token.value] = struct{}{}
	}
	return set
}

func jaccard(left, right map[string]struct{}) float64 {
	if len(left) == 0 && len(right) == 0 {
		return 1
	}
	intersection := 0
	for item := range left {
		if _, ok := right[item]; ok {
			intersection++
		}
	}
	union := len(left) + len(right) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
