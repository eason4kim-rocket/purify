package consensus

import (
	"bytes"
	"container/heap"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/use-agent/purify/evidence"
	"github.com/use-agent/purify/simhash"
)

const (
	maxCoreAnchors  = 256
	coreWindowBytes = 1 << 10
	maxCoreShingles = 4_096
	minCoreShingles = 24
	coreDistance    = 3
	coreLengthRatio = 70

	coreWordShingleTag byte = 1
	coreRuneShingleTag byte = 2
)

// coreDescriptor is the bounded, source-global content-core signal derived
// from located evidence anchors. A zero fingerprint remains meaningful when
// valid is true.
type coreDescriptor struct {
	fingerprint          uint64
	retainedShingles     uint64
	normalizedAlnumRunes uint64
	valid                bool
}

func contentCoresSimilar(first, second coreDescriptor) bool {
	if !first.valid || !second.valid {
		return false
	}
	shorter, longer := first.normalizedAlnumRunes, second.normalizedAlnumRunes
	if shorter > longer {
		shorter, longer = longer, shorter
	}
	// Compare shorter*100 >= longer*70 without overflowing uint64. The
	// right-hand side is ceil(longer*70/100), decomposed before multiplying.
	required := (longer/100)*coreLengthRatio + ((longer%100)*coreLengthRatio+99)/100
	return shorter >= required && simhash.Distance(first.fingerprint, second.fingerprint) <= coreDistance
}

type coreAnchor struct {
	path       string
	start, end int
	pathHash   [sha256.Size]byte
}

// buildContentCore derives one descriptor without retaining cleaned text or
// per-window state. Cleaned-text UTF-8 and size admission belong to the caller;
// this helper validates every eligible anchor before taking the bounded sample.
func buildContentCore(cleaned string, basis map[string]evidence.Anchor) (coreDescriptor, error) {
	if cleaned == "" {
		return coreDescriptor{}, nil
	}
	anchors, err := selectCoreAnchors(cleaned, basis)
	if err != nil {
		return coreDescriptor{}, err
	}
	if len(anchors) == 0 {
		return coreDescriptor{}, nil
	}

	sampler := newCoreShingleSampler(maxCoreShingles)
	var normalizedAlnumRunes uint64
	for _, anchor := range anchors {
		start, end := coreWindowBounds(cleaned, anchor.start, anchor.end)
		normalizedAlnumRunes += addCoreWindowShingles(sampler, cleaned[start:end])
	}
	return describeContentCore(sampler.sorted(), normalizedAlnumRunes), nil
}

// selectCoreAnchors returns the deterministic bottom path sample. Eligibility
// is deliberately narrower than general consensus evidence, but every eligible
// range is checked before truncation so sampling cannot hide malformed input.
func selectCoreAnchors(cleaned string, basis map[string]evidence.Anchor) ([]coreAnchor, error) {
	anchors := make([]coreAnchor, 0, min(len(basis), maxCoreAnchors))
	for path, anchor := range basis {
		if !eligibleCoreAnchor(anchor) {
			continue
		}
		start, end := anchor.TextRange[0], anchor.TextRange[1]
		if start < 0 || start > len(cleaned) || end > len(cleaned) || cleaned[start:end] != anchor.Quote {
			return nil, fmt.Errorf("%w: evidence path %q range does not match cleaned text", ErrInvalidInput, path)
		}
		anchors = append(anchors, coreAnchor{
			path:     path,
			start:    start,
			end:      end,
			pathHash: sha256.Sum256([]byte(path)),
		})
	}

	sort.Slice(anchors, func(i, j int) bool {
		first, second := anchors[i], anchors[j]
		if comparison := bytes.Compare(first.pathHash[:], second.pathHash[:]); comparison != 0 {
			return comparison < 0
		}
		if first.path != second.path {
			return first.path < second.path
		}
		return first.start < second.start
	})
	if len(anchors) > maxCoreAnchors {
		anchors = anchors[:maxCoreAnchors]
	}
	return anchors, nil
}

func eligibleCoreAnchor(anchor evidence.Anchor) bool {
	if anchor.Quote == "" || anchor.TextRange[1] <= anchor.TextRange[0] {
		return false
	}
	switch anchor.Method {
	case evidence.MethodExact, evidence.MethodNormalized, evidence.MethodFuzzy, evidence.MethodCompiled:
		return true
	default:
		return false
	}
}

// coreWindowBounds centers a byte window on the anchor, reallocates unused
// budget at document edges, then moves both ends inward to UTF-8 boundaries.
func coreWindowBounds(cleaned string, anchorStart, anchorEnd int) (int, int) {
	if len(cleaned) <= coreWindowBytes {
		return 0, len(cleaned)
	}

	midpoint := anchorStart + (anchorEnd-anchorStart)/2
	start := midpoint - coreWindowBytes/2
	end := start + coreWindowBytes
	if start < 0 {
		end -= start
		start = 0
	}
	if end > len(cleaned) {
		start -= end - len(cleaned)
		end = len(cleaned)
		if start < 0 {
			start = 0
		}
	}

	for start < end && start > 0 && !utf8.RuneStart(cleaned[start]) {
		start++
	}
	for end > start && end < len(cleaned) && !utf8.RuneStart(cleaned[end]) {
		end--
	}
	return start, end
}

func tokenizeCoreWindow(window string) ([]string, uint64) {
	tokens := make([]string, 0, 32)
	var token strings.Builder
	var normalizedAlnumRunes uint64
	flush := func() {
		if token.Len() == 0 {
			return
		}
		tokens = append(tokens, token.String())
		token.Reset()
	}

	for _, original := range window {
		r := foldCoreWidth(original)
		r = unicode.ToLower(r)
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			token.WriteRune(r)
			normalizedAlnumRunes++
			continue
		}
		flush()
	}
	flush()
	return tokens, normalizedAlnumRunes
}

func foldCoreWidth(r rune) rune {
	switch {
	case r == '\u3000':
		return ' '
	case r >= '\uff01' && r <= '\uff5e':
		return r - 0xfee0
	default:
		return r
	}
}

func addCoreWindowShingles(sampler *coreShingleSampler, window string) uint64 {
	tokens, normalizedAlnumRunes := tokenizeCoreWindow(window)
	for index := 0; index+2 < len(tokens); index++ {
		sampler.add(encodeCoreWordShingle(tokens[index], tokens[index+1], tokens[index+2]))
	}
	for _, token := range tokens {
		runes := []rune(token)
		for index := 0; index+5 <= len(runes); index++ {
			sampler.add(encodeCoreRuneShingle(runes[index : index+5]))
		}
	}
	return normalizedAlnumRunes
}

func encodeCoreWordShingle(first, second, third string) []byte {
	encoded := make([]byte, 0, 1+len(first)+len(second)+len(third)+3*binary.MaxVarintLen64)
	encoded = append(encoded, coreWordShingleTag)
	for _, token := range []string{first, second, third} {
		encoded = binary.AppendUvarint(encoded, uint64(len(token)))
		encoded = append(encoded, token...)
	}
	return encoded
}

func encodeCoreRuneShingle(runes []rune) []byte {
	encoded := make([]byte, 1, 1+len(runes)*utf8.UTFMax)
	encoded[0] = coreRuneShingleTag
	for _, r := range runes {
		encoded = utf8.AppendRune(encoded, r)
	}
	return encoded
}

type coreShingle struct {
	digest  [sha256.Size]byte
	encoded []byte
}

func newCoreShingle(encoded []byte) coreShingle {
	owned := append([]byte(nil), encoded...)
	return coreShingle{digest: sha256.Sum256(owned), encoded: owned}
}

func compareCoreShingles(first, second coreShingle) int {
	if comparison := bytes.Compare(first.digest[:], second.digest[:]); comparison != 0 {
		return comparison
	}
	return bytes.Compare(first.encoded, second.encoded)
}

// coreShingleMaxHeap puts the greatest retained key at index zero so a smaller
// key can replace it in O(log k) time.
type coreShingleMaxHeap []coreShingle

func (values coreShingleMaxHeap) Len() int { return len(values) }

func (values coreShingleMaxHeap) Less(first, second int) bool {
	return compareCoreShingles(values[first], values[second]) > 0
}

func (values coreShingleMaxHeap) Swap(first, second int) {
	values[first], values[second] = values[second], values[first]
}

func (values *coreShingleMaxHeap) Push(value any) {
	*values = append(*values, value.(coreShingle))
}

func (values *coreShingleMaxHeap) Pop() any {
	old := *values
	last := len(old) - 1
	value := old[last]
	old[last] = coreShingle{}
	*values = old[:last]
	return value
}

type coreShingleSampler struct {
	limit    int
	heap     coreShingleMaxHeap
	retained map[string]struct{}
}

func newCoreShingleSampler(limit int) *coreShingleSampler {
	if limit < 0 {
		limit = 0
	}
	return &coreShingleSampler{
		limit:    limit,
		heap:     make(coreShingleMaxHeap, 0, limit),
		retained: make(map[string]struct{}, limit),
	}
}

func (sampler *coreShingleSampler) add(encoded []byte) {
	if sampler == nil || sampler.limit == 0 {
		return
	}
	key := string(encoded)
	if _, duplicate := sampler.retained[key]; duplicate {
		return
	}
	candidate := newCoreShingle(encoded)
	if len(sampler.heap) < sampler.limit {
		heap.Push(&sampler.heap, candidate)
		sampler.retained[string(candidate.encoded)] = struct{}{}
		return
	}
	if compareCoreShingles(candidate, sampler.heap[0]) >= 0 {
		return
	}

	removed := heap.Pop(&sampler.heap).(coreShingle)
	delete(sampler.retained, string(removed.encoded))
	heap.Push(&sampler.heap, candidate)
	sampler.retained[string(candidate.encoded)] = struct{}{}
}

func (sampler *coreShingleSampler) sorted() []coreShingle {
	if sampler == nil || len(sampler.heap) == 0 {
		return nil
	}
	retained := append([]coreShingle(nil), sampler.heap...)
	sort.Slice(retained, func(i, j int) bool {
		return compareCoreShingles(retained[i], retained[j]) < 0
	})
	return retained
}

func describeContentCore(shingles []coreShingle, normalizedAlnumRunes uint64) coreDescriptor {
	encoded := make([]string, len(shingles))
	for index, shingle := range shingles {
		encoded[index] = string(shingle.encoded)
	}
	fingerprint := simhash.FingerprintShingles(encoded, normalizedAlnumRunes)
	return coreDescriptor{
		fingerprint:          fingerprint.Fingerprint,
		retainedShingles:     fingerprint.RetainedShingles,
		normalizedAlnumRunes: fingerprint.NormalizedAlnumRunes,
		valid:                fingerprint.Valid,
	}
}
