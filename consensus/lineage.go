package consensus

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/use-agent/purify/evidence"
)

const (
	maxLineageFragmentBytes = 1 << 10
	minLineageAlnumRunes    = 96

	lineageFragmentKinds = 3
)

const (
	lineageFragmentLine = iota
	lineageFragmentSentence
	lineageFragmentParagraph
)

// lineageIndex keeps the at-most-three normalized fragments for each sampled
// evidence path. Values borrow CleanedText when normalization can return a
// substring; callers must retain them only for the enclosing Merge call.
type lineageIndex map[string][]string

type lineageAnchorBounds struct {
	anchor coreAnchor
	start  [lineageFragmentKinds]int
	end    [lineageFragmentKinds]int
}

// buildLineageIndex derives bounded verbatim candidates from the same
// deterministic anchor sample as the content core. Cleaned text admission is a
// caller concern; an empty string means the enhanced input was not supplied.
func buildLineageIndex(cleaned string, basis map[string]evidence.Anchor) (lineageIndex, error) {
	index := make(lineageIndex)
	if cleaned == "" {
		return index, nil
	}

	anchors, err := selectCoreAnchors(cleaned, basis)
	if err != nil {
		return nil, err
	}
	return buildLineageIndexFromAnchors(cleaned, anchors), nil
}

func buildLineageIndexFromAnchors(cleaned string, anchors []coreAnchor) lineageIndex {
	index := make(lineageIndex)
	if len(anchors) == 0 {
		return index
	}

	bounds := make([]lineageAnchorBounds, len(anchors))
	for position, anchor := range anchors {
		bounds[position].anchor = anchor
		for kind := range lineageFragmentKinds {
			bounds[position].end[kind] = len(cleaned)
		}
	}
	sort.Slice(bounds, func(first, second int) bool {
		if bounds[first].anchor.start != bounds[second].anchor.start {
			return bounds[first].anchor.start < bounds[second].anchor.start
		}
		if bounds[first].anchor.end != bounds[second].anchor.end {
			return bounds[first].anchor.end < bounds[second].anchor.end
		}
		return bounds[first].anchor.path < bounds[second].anchor.path
	})
	locateLineageBounds(cleaned, bounds)

	for _, bounded := range bounds {
		fragments := make([]string, 0, lineageFragmentKinds)
		seen := make(map[string]struct{}, lineageFragmentKinds)
		for kind := range lineageFragmentKinds {
			start, end := bounded.start[kind], bounded.end[kind]
			if start < 0 || start > bounded.anchor.start || end < bounded.anchor.end || end > len(cleaned) || end < start {
				continue
			}
			candidate := cleaned[start:end]
			if len(candidate) > maxLineageFragmentBytes {
				continue
			}
			candidate = normalizeLineageFragment(candidate)
			if lineageAlnumRunes(candidate) < minLineageAlnumRunes {
				continue
			}
			if _, duplicate := seen[candidate]; duplicate {
				continue
			}
			seen[candidate] = struct{}{}
			fragments = append(fragments, candidate)
		}
		if len(fragments) > 0 {
			index[bounded.anchor.path] = fragments
		}
	}
	return index
}

// locateLineageBounds makes one streaming pass over cleaned text. Its retained
// state is proportional to the selected anchors, not to the number of lines or
// sentences in a document.
func locateLineageBounds(cleaned string, bounds []lineageAnchorBounds) {
	if len(bounds) == 0 {
		return
	}

	nextStart := 0
	nextLineEnd := 0
	nextSentenceEnd := 0
	nextParagraphEnd := 0
	lastLineStart := 0
	lastSentenceStart := 0
	lastParagraphStart := 0
	lineStart := 0
	lineIsBlank := true

	assignStarts := func(offset int) {
		for nextStart < len(bounds) && bounds[nextStart].anchor.start <= offset {
			bounds[nextStart].start[lineageFragmentLine] = lastLineStart
			bounds[nextStart].start[lineageFragmentSentence] = lastSentenceStart
			bounds[nextStart].start[lineageFragmentParagraph] = lastParagraphStart
			nextStart++
		}
	}

	for offset := 0; offset < len(cleaned); {
		assignStarts(offset)
		r, size := utf8.DecodeRuneInString(cleaned[offset:])
		boundaryEnd := offset + size

		if r == '\n' {
			for nextLineEnd < len(bounds) && bounds[nextLineEnd].anchor.start < boundaryEnd {
				bounds[nextLineEnd].end[lineageFragmentLine] = offset
				nextLineEnd++
			}
			lastLineStart = boundaryEnd

			if lineIsBlank {
				for nextParagraphEnd < len(bounds) && bounds[nextParagraphEnd].anchor.start < boundaryEnd {
					bounds[nextParagraphEnd].end[lineageFragmentParagraph] = lineStart
					nextParagraphEnd++
				}
				lastParagraphStart = boundaryEnd
			}
			lineStart = boundaryEnd
			lineIsBlank = true
		} else if r != ' ' && r != '\t' && r != '\r' {
			lineIsBlank = false
		}

		if isLineageSentenceTerminator(r) {
			for nextSentenceEnd < len(bounds) && bounds[nextSentenceEnd].anchor.start < boundaryEnd {
				bounds[nextSentenceEnd].end[lineageFragmentSentence] = boundaryEnd
				nextSentenceEnd++
			}
			lastSentenceStart = boundaryEnd
		}
		offset = boundaryEnd
	}
	assignStarts(len(cleaned))
}

func isLineageSentenceTerminator(r rune) bool {
	switch r {
	case '.', '?', '!', '。', '！', '？':
		return true
	default:
		return false
	}
}

func normalizeLineageFragment(fragment string) string {
	fragment = strings.ReplaceAll(fragment, "\r\n", "\n")
	return strings.TrimFunc(fragment, unicode.IsSpace)
}

func lineageAlnumRunes(fragment string) int {
	count := 0
	for _, r := range fragment {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			count++
		}
	}
	return count
}

// quoteLineageSimilar compares only buckets with the same evidence path and
// canonical scalar value. Each constructed bucket has at most three fragments,
// bounding the inner comparison to 3x3 exact substring checks.
func quoteLineageSimilar(
	first lineageIndex,
	firstFields map[string]scalarValue,
	second lineageIndex,
	secondFields map[string]scalarValue,
) bool {
	for path, firstFragments := range first {
		secondFragments, exists := second[path]
		if !exists {
			continue
		}
		firstValue, firstExists := firstFields[path]
		secondValue, secondExists := secondFields[path]
		if !firstExists || !secondExists || firstValue.groupKey != secondValue.groupKey {
			continue
		}
		for _, firstFragment := range firstFragments {
			for _, secondFragment := range secondFragments {
				needle, haystack := firstFragment, secondFragment
				if len(needle) > len(haystack) {
					needle, haystack = haystack, needle
				}
				if strings.Contains(haystack, needle) {
					return true
				}
			}
		}
	}
	return false
}
