package consensus

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/use-agent/purify/evidence"
)

func TestSelectCoreAnchorsEligibilityAndValidation(t *testing.T) {
	cleaned := "zero alpha beta gamma omega"
	alphaStart := strings.Index(cleaned, "alpha")
	basis := map[string]evidence.Anchor{
		"exact": locatedCoreTestAnchor(cleaned, "alpha", evidence.MethodExact),
		"empty": {
			Quote:     "",
			TextRange: [2]int{len(cleaned) + 10, len(cleaned) + 20},
			Method:    evidence.MethodExact,
		},
		"zero-range": {
			Quote:     "alpha",
			TextRange: [2]int{alphaStart, alphaStart},
			Method:    evidence.MethodExact,
		},
		"unlocated": {
			Quote:     "alpha",
			TextRange: [2]int{len(cleaned) + 10, len(cleaned) + 20},
			Method:    evidence.MethodUnlocated,
		},
	}

	anchors, err := selectCoreAnchors(cleaned, basis)
	if err != nil {
		t.Fatalf("selectCoreAnchors() error = %v", err)
	}
	if len(anchors) != 1 || anchors[0].path != "exact" {
		t.Fatalf("selected anchors = %#v, want only exact", anchors)
	}

	for _, method := range []evidence.Method{
		evidence.MethodExact,
		evidence.MethodNormalized,
		evidence.MethodFuzzy,
		evidence.MethodCompiled,
	} {
		t.Run(string(method), func(t *testing.T) {
			anchors, err := selectCoreAnchors(cleaned, map[string]evidence.Anchor{
				"value": locatedCoreTestAnchor(cleaned, "beta", method),
			})
			if err != nil {
				t.Fatalf("selectCoreAnchors() error = %v", err)
			}
			if len(anchors) != 1 {
				t.Fatalf("selected anchor count = %d, want 1", len(anchors))
			}
		})
	}

	badAnchors := []evidence.Anchor{
		{Quote: "alpha", TextRange: [2]int{-1, alphaStart + len("alpha")}, Method: evidence.MethodExact},
		{Quote: "alpha", TextRange: [2]int{alphaStart, len(cleaned) + 1}, Method: evidence.MethodExact},
		{Quote: "wrong", TextRange: [2]int{alphaStart, alphaStart + len("alpha")}, Method: evidence.MethodExact},
	}
	for index, anchor := range badAnchors {
		t.Run(fmt.Sprintf("invalid-%d", index), func(t *testing.T) {
			_, err := selectCoreAnchors(cleaned, map[string]evidence.Anchor{"value": anchor})
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("selectCoreAnchors() error = %v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestSelectCoreAnchorsDeterministicBottomPaths(t *testing.T) {
	cleaned := "x"
	basis := make(map[string]evidence.Anchor, maxCoreAnchors+44)
	paths := make([]string, 0, maxCoreAnchors+44)
	for index := 0; index < cap(paths); index++ {
		path := fmt.Sprintf("field.%03d", index)
		paths = append(paths, path)
		basis[path] = evidence.Anchor{
			Quote:     cleaned,
			TextRange: [2]int{0, len(cleaned)},
			Method:    evidence.MethodExact,
		}
	}
	sort.Slice(paths, func(i, j int) bool {
		first, second := sha256.Sum256([]byte(paths[i])), sha256.Sum256([]byte(paths[j]))
		if comparison := bytes.Compare(first[:], second[:]); comparison != 0 {
			return comparison < 0
		}
		return paths[i] < paths[j]
	})

	anchors, err := selectCoreAnchors(cleaned, basis)
	if err != nil {
		t.Fatalf("selectCoreAnchors() error = %v", err)
	}
	if len(anchors) != maxCoreAnchors {
		t.Fatalf("selected anchor count = %d, want %d", len(anchors), maxCoreAnchors)
	}
	for index, anchor := range anchors {
		if anchor.path != paths[index] {
			t.Fatalf("selected path %d = %q, want %q", index, anchor.path, paths[index])
		}
	}

	// Every eligible anchor is admitted before the deterministic 256-anchor
	// sample is cut. An invalid anchor outside that sample must still fail closed.
	unsampledPath := paths[len(paths)-1]
	bad := basis[unsampledPath]
	bad.TextRange = [2]int{0, len(cleaned) + 1}
	basis[unsampledPath] = bad
	if _, err := selectCoreAnchors(cleaned, basis); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unsampled invalid anchor error = %v, want ErrInvalidInput", err)
	}
}

func TestCoreWindowBounds(t *testing.T) {
	cleaned := strings.Repeat("x", 2_000)
	tests := []struct {
		name       string
		start, end int
		wantStart  int
		wantEnd    int
	}{
		{name: "centered", start: 995, end: 1_005, wantStart: 488, wantEnd: 1_512},
		{name: "left edge gives budget to right", start: 8, end: 12, wantStart: 0, wantEnd: 1_024},
		{name: "right edge gives budget to left", start: 1_990, end: 1_994, wantStart: 976, wantEnd: 2_000},
		{name: "anchor longer than window uses midpoint", start: 250, end: 1_750, wantStart: 488, wantEnd: 1_512},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			start, end := coreWindowBounds(cleaned, test.start, test.end)
			if start != test.wantStart || end != test.wantEnd {
				t.Fatalf("coreWindowBounds() = [%d,%d), want [%d,%d)", start, end, test.wantStart, test.wantEnd)
			}
		})
	}

	short := "short document"
	start, end := coreWindowBounds(short, 1, 2)
	if start != 0 || end != len(short) {
		t.Fatalf("short document bounds = [%d,%d), want [0,%d)", start, end, len(short))
	}
}

func TestCoreWindowBoundsMovesInwardToUTF8Boundaries(t *testing.T) {
	cleaned := strings.Repeat("你", 400)
	start, end := coreWindowBounds(cleaned, 600, 603)
	if start != 90 || end != 1_113 {
		t.Fatalf("coreWindowBounds() = [%d,%d), want [90,1113)", start, end)
	}
	if end-start > coreWindowBytes || !utf8.ValidString(cleaned[start:end]) {
		t.Fatalf("window length/UTF-8 = %d/%t, want <= %d/valid", end-start, utf8.ValidString(cleaned[start:end]), coreWindowBytes)
	}
	if start > 0 && !utf8.RuneStart(cleaned[start]) {
		t.Fatalf("start %d is not a rune boundary", start)
	}
	if end < len(cleaned) && !utf8.RuneStart(cleaned[end]) {
		t.Fatalf("end %d is not a rune boundary", end)
	}
}

func TestTokenizeCoreWindowNormalizesWidthCaseAndSeparators(t *testing.T) {
	tokens, alnumRunes := tokenizeCoreWindow("Ｆｏｏ—ＢＡＲ baz_甲乙　１２")
	want := []string{"foo", "bar", "baz", "甲乙", "12"}
	if fmt.Sprint(tokens) != fmt.Sprint(want) {
		t.Fatalf("tokens = %q, want %q", tokens, want)
	}
	if alnumRunes != 13 {
		t.Fatalf("alnum rune count = %d, want 13", alnumRunes)
	}
}

func TestCoreShingleEncodingsSeparateFamiliesAndBoundaries(t *testing.T) {
	first := encodeCoreWordShingle("a", "bc", "d")
	second := encodeCoreWordShingle("ab", "c", "d")
	if bytes.Equal(first, second) {
		t.Fatal("length-prefix word encodings collided")
	}
	runeShingle := encodeCoreRuneShingle([]rune("abcdx"))
	wordShingle := encodeCoreWordShingle("a", "b", "cdx")
	if bytes.Equal(runeShingle, wordShingle) {
		t.Fatal("word and rune shingle encodings collided")
	}
	if first[0] == runeShingle[0] {
		t.Fatalf("family tags are equal: %d", first[0])
	}
}

func TestAddCoreWindowShinglesDoesNotCrossWindows(t *testing.T) {
	separate := newCoreShingleSampler(maxCoreShingles)
	firstCount := addCoreWindowShingles(separate, "aa bb")
	secondCount := addCoreWindowShingles(separate, "cc dd")
	if firstCount != 4 || secondCount != 4 {
		t.Fatalf("alnum counts = %d/%d, want 4/4", firstCount, secondCount)
	}
	if got := len(separate.sorted()); got != 0 {
		t.Fatalf("separate windows retained %d shingles, want 0", got)
	}

	combined := newCoreShingleSampler(maxCoreShingles)
	addCoreWindowShingles(combined, "aa bb cc dd")
	if got := len(combined.sorted()); got != 2 {
		t.Fatalf("combined window retained %d shingles, want 2 word shingles", got)
	}

	mixed := newCoreShingleSampler(maxCoreShingles)
	addCoreWindowShingles(mixed, "one two three abcde")
	wordCount, runeCount := 0, 0
	for _, shingle := range mixed.sorted() {
		switch shingle.encoded[0] {
		case coreWordShingleTag:
			wordCount++
		case coreRuneShingleTag:
			runeCount++
		default:
			t.Fatalf("unknown family tag %d", shingle.encoded[0])
		}
	}
	if wordCount != 2 || runeCount != 2 {
		t.Fatalf("word/rune shingles = %d/%d, want 2/2", wordCount, runeCount)
	}
}

func TestCoreShingleSamplerKeepsStableBottomKUnique(t *testing.T) {
	const total = maxCoreShingles + 904
	all := make([]coreShingle, 0, total)
	for index := 0; index < total; index++ {
		encoded := append([]byte{coreRuneShingleTag}, fmt.Appendf(nil, "%05d", index)...)
		all = append(all, newCoreShingle(encoded))
	}
	sort.Slice(all, func(i, j int) bool { return compareCoreShingles(all[i], all[j]) < 0 })
	want := all[:maxCoreShingles]

	forward := newCoreShingleSampler(maxCoreShingles)
	reverse := newCoreShingleSampler(maxCoreShingles)
	for index := 0; index < total; index++ {
		forward.add(all[index].encoded)
		forward.add(all[index].encoded)
		reverse.add(all[total-1-index].encoded)
	}
	if len(forward.heap) != maxCoreShingles || len(forward.retained) != maxCoreShingles {
		t.Fatalf("forward state = heap %d/map %d, want %d/%d", len(forward.heap), len(forward.retained), maxCoreShingles, maxCoreShingles)
	}
	for name, sampler := range map[string]*coreShingleSampler{"forward": forward, "reverse": reverse} {
		got := sampler.sorted()
		if len(got) != len(want) {
			t.Fatalf("%s retained %d shingles, want %d", name, len(got), len(want))
		}
		for index := range want {
			if compareCoreShingles(got[index], want[index]) != 0 {
				t.Fatalf("%s shingle %d = %x/%q, want %x/%q", name, index, got[index].digest, got[index].encoded, want[index].digest, want[index].encoded)
			}
		}
	}
}

func TestDescribeContentCoreValidityBoundary(t *testing.T) {
	shingles := make([]coreShingle, minCoreShingles)
	for index := range shingles {
		shingles[index] = newCoreShingle(append([]byte{coreRuneShingleTag}, fmt.Appendf(nil, "%05d", index)...))
	}

	invalid := describeContentCore(shingles[:minCoreShingles-1], 123)
	if invalid.valid || invalid.retainedShingles != minCoreShingles-1 || invalid.normalizedAlnumRunes != 123 {
		t.Fatalf("invalid descriptor = %#v", invalid)
	}
	valid := describeContentCore(shingles, 456)
	if !valid.valid || valid.retainedShingles != minCoreShingles || valid.normalizedAlnumRunes != 456 {
		t.Fatalf("valid descriptor = %#v", valid)
	}
}

func TestBuildContentCoreDeterministic(t *testing.T) {
	cleaned := "Alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu nu xi omicron pi rho sigma tau upsilon phi chi psi omega alphabetic numeric123"
	basis := map[string]evidence.Anchor{
		"name":   locatedCoreTestAnchor(cleaned, "lambda", evidence.MethodExact),
		"symbol": locatedCoreTestAnchor(cleaned, "omega", evidence.MethodNormalized),
	}

	first, err := buildContentCore(cleaned, basis)
	if err != nil {
		t.Fatalf("buildContentCore() error = %v", err)
	}
	second, err := buildContentCore(cleaned, map[string]evidence.Anchor{
		"symbol": basis["symbol"],
		"name":   basis["name"],
	})
	if err != nil {
		t.Fatalf("buildContentCore(permuted) error = %v", err)
	}
	if first != second {
		t.Fatalf("descriptors differ: %#v vs %#v", first, second)
	}
	if !first.valid || first.retainedShingles < minCoreShingles || first.normalizedAlnumRunes == 0 {
		t.Fatalf("descriptor = %#v, want valid populated core", first)
	}

	empty, err := buildContentCore("", basis)
	if err != nil || empty != (coreDescriptor{}) {
		t.Fatalf("empty cleaned core = %#v, %v; want zero descriptor", empty, err)
	}
}

func BenchmarkBuildContentCore256Anchors(b *testing.B) {
	cleaned, basis := coreBenchmarkFixture()
	b.ReportAllocs()
	b.SetBytes(int64(len(cleaned)))
	for b.Loop() {
		core, err := buildContentCore(cleaned, basis)
		if err != nil || !core.valid {
			b.Fatalf("buildContentCore() = (%#v, %v)", core, err)
		}
	}
}

func BenchmarkIndependentComponentsEightPrecomputedCores(b *testing.B) {
	cleaned, basis := coreBenchmarkFixture()
	core, err := buildContentCore(cleaned, basis)
	if err != nil || !core.valid {
		b.Fatalf("buildContentCore() = (%#v, %v)", core, err)
	}
	sources := make([]preparedSource, MaxSources)
	for index := range sources {
		sources[index] = preparedSource{
			url:  fmt.Sprintf("https://source-%d.example/report", index),
			root: fmt.Sprintf("source-%d.example", index),
			core: core,
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		components := independentComponents(sources)
		if len(components) != MaxSources {
			b.Fatalf("component count = %d", len(components))
		}
	}
}

func coreBenchmarkFixture() (string, map[string]evidence.Anchor) {
	var cleaned strings.Builder
	basis := make(map[string]evidence.Anchor, maxCoreAnchors)
	for index := range maxCoreAnchors {
		for token := range 32 {
			fmt.Fprintf(&cleaned, "context%dtoken%d ", index, token)
		}
		quote := fmt.Sprintf("anchor%d", index)
		start := cleaned.Len()
		cleaned.WriteString(quote)
		basis[fmt.Sprintf("field.%03d", index)] = evidence.Anchor{
			Quote:     quote,
			TextRange: [2]int{start, start + len(quote)},
			Method:    evidence.MethodExact,
		}
		cleaned.WriteByte('\n')
	}
	return cleaned.String(), basis
}

func locatedCoreTestAnchor(cleaned, quote string, method evidence.Method) evidence.Anchor {
	start := strings.Index(cleaned, quote)
	if start < 0 {
		panic("test quote is not in cleaned text")
	}
	return evidence.Anchor{
		Quote:     quote,
		TextRange: [2]int{start, start + len(quote)},
		Method:    method,
	}
}
