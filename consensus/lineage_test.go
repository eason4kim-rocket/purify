package consensus

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/use-agent/purify/evidence"
)

func TestBuildLineageIndexAlnumThreshold(t *testing.T) {
	for _, test := range []struct {
		name string
		text string
		want int
	}{
		{name: "95", text: strings.Repeat("a", 95), want: 0},
		{name: "96", text: strings.Repeat("a", 96), want: 1},
		{name: "97", text: strings.Repeat("a", 97), want: 1},
		{name: "96 CJK", text: strings.Repeat("界", 96), want: 1},
		{name: "mixed", text: strings.Repeat("A界1-", 32), want: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			index, err := buildLineageIndex(test.text, singleLineageBasis(test.text, "value", 0, utf8.RuneLen([]rune(test.text)[0])))
			if err != nil {
				t.Fatalf("buildLineageIndex() error = %v", err)
			}
			if got := len(index["value"]); got != test.want {
				t.Fatalf("fragment count = %d, want %d: %#v", got, test.want, index)
			}
		})
	}
}

func TestBuildLineageIndexExtractsLineSentenceAndParagraph(t *testing.T) {
	priorSentence := strings.Repeat("p", 98) + "."
	targetSentence := strings.Repeat("t", 48) + " CLAIM " + strings.Repeat("u", 48) + "?"
	trailingSentence := strings.Repeat("v", 98) + "!"
	line := priorSentence + "  " + targetSentence + "  " + trailingSentence
	secondLine := strings.Repeat("z", 100)
	cleaned := "discarded paragraph\n\n" + line + "\r\n" + secondLine + "\n\t \r\nnext paragraph"
	claimStart := strings.Index(cleaned, "CLAIM")

	index, err := buildLineageIndex(cleaned, singleLineageBasis(cleaned, "claim", claimStart, claimStart+len("CLAIM")))
	if err != nil {
		t.Fatalf("buildLineageIndex() error = %v", err)
	}
	want := []string{
		line,
		strings.TrimSpace(targetSentence),
		line + "\n" + secondLine,
	}
	if got := index["claim"]; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("fragments = %#v, want %#v", got, want)
	}
}

func TestBuildLineageIndexSkipsCandidateWhenAnchorCrossesItsBoundary(t *testing.T) {
	long := strings.Repeat("a", 100)
	tests := []struct {
		name      string
		cleaned   string
		quote     string
		missing   string
		wantCount int
	}{
		{
			name:      "line",
			cleaned:   long + " left\nright " + long,
			quote:     "left\nright",
			missing:   long + " left",
			wantCount: 1,
		},
		{
			name:      "sentence",
			cleaned:   long + " left. right " + long,
			quote:     "left. right",
			missing:   long + " left.",
			wantCount: 1,
		},
		{
			name:      "paragraph",
			cleaned:   long + " left\n \t\r\nright " + long,
			quote:     "left\n \t\r\nright",
			missing:   long + " left",
			wantCount: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			start := strings.Index(test.cleaned, test.quote)
			index, err := buildLineageIndex(test.cleaned, singleLineageBasis(test.cleaned, "claim", start, start+len(test.quote)))
			if err != nil {
				t.Fatalf("buildLineageIndex() error = %v", err)
			}
			got := index["claim"]
			if len(got) != test.wantCount {
				t.Fatalf("fragment count = %d, want %d: %#v", len(got), test.wantCount, got)
			}
			for _, fragment := range got {
				if fragment == test.missing {
					t.Fatalf("crossed %s candidate was retained: %q", test.name, fragment)
				}
			}
		})
	}
}

func TestNormalizeLineageFragmentOnlyChangesCRLFAndOuterWhitespace(t *testing.T) {
	input := "\u2003MiXeD—甲\r\ninside\t  spacing!?\r\n\u3000"
	want := "MiXeD—甲\ninside\t  spacing!?"
	if got := normalizeLineageFragment(input); got != want {
		t.Fatalf("normalizeLineageFragment() = %q, want %q", got, want)
	}
}

func TestBuildLineageIndexDeduplicatesExactCandidates(t *testing.T) {
	cleaned := strings.Repeat("x", 96)
	index, err := buildLineageIndex(cleaned, singleLineageBasis(cleaned, "claim", 0, 1))
	if err != nil {
		t.Fatalf("buildLineageIndex() error = %v", err)
	}
	if got := index["claim"]; len(got) != 1 || got[0] != cleaned {
		t.Fatalf("deduplicated fragments = %#v, want one document fragment", got)
	}
}

func TestBuildLineageIndexFragmentByteLimit(t *testing.T) {
	for _, size := range []int{maxLineageFragmentBytes - 1, maxLineageFragmentBytes, maxLineageFragmentBytes + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			cleaned := strings.Repeat("x", size)
			index, err := buildLineageIndex(cleaned, singleLineageBasis(cleaned, "claim", 0, 1))
			if err != nil {
				t.Fatalf("buildLineageIndex() error = %v", err)
			}
			want := 1
			if size > maxLineageFragmentBytes {
				want = 0
			}
			if got := len(index["claim"]); got != want {
				t.Fatalf("fragment count = %d, want %d", got, want)
			}
		})
	}
}

func TestLineageSentenceTerminators(t *testing.T) {
	for _, r := range []rune{'.', '?', '!', '。', '！', '？'} {
		if !isLineageSentenceTerminator(r) {
			t.Errorf("terminator %q was rejected", r)
		}
	}
	for _, r := range []rune{',', ';', '，', '；', '\n'} {
		if isLineageSentenceTerminator(r) {
			t.Errorf("non-terminator %q was accepted", r)
		}
	}
}

func TestBuildLineageIndexDoesNotTreatUnicodeSpaceAsBlankParagraphLine(t *testing.T) {
	prefix := strings.Repeat("a", 100)
	target := strings.Repeat("b", 48) + "CLAIM" + strings.Repeat("c", 48)
	cleaned := prefix + "\n\u00a0\n" + target
	start := strings.Index(cleaned, "CLAIM")
	index, err := buildLineageIndex(cleaned, singleLineageBasis(cleaned, "claim", start, start+len("CLAIM")))
	if err != nil {
		t.Fatalf("buildLineageIndex() error = %v", err)
	}
	foundWholeParagraph := false
	for _, fragment := range index["claim"] {
		if fragment == cleaned {
			foundWholeParagraph = true
		}
	}
	if !foundWholeParagraph {
		t.Fatalf("NBSP-only line incorrectly split paragraph: %#v", index["claim"])
	}
}

func TestBuildLineageIndexReusesDeterministicCoreAnchorSample(t *testing.T) {
	cleaned := strings.Repeat("x", 96)
	basis := make(map[string]evidence.Anchor, maxCoreAnchors+44)
	for index := 0; index < maxCoreAnchors+44; index++ {
		path := fmt.Sprintf("field.%03d", index)
		basis[path] = evidence.Anchor{Quote: "x", TextRange: [2]int{0, 1}, Method: evidence.MethodExact}
	}
	selected, err := selectCoreAnchors(cleaned, basis)
	if err != nil {
		t.Fatalf("selectCoreAnchors() error = %v", err)
	}
	index, err := buildLineageIndex(cleaned, basis)
	if err != nil {
		t.Fatalf("buildLineageIndex() error = %v", err)
	}
	if len(index) != maxCoreAnchors {
		t.Fatalf("indexed paths = %d, want %d", len(index), maxCoreAnchors)
	}
	for _, anchor := range selected {
		if got := index[anchor.path]; len(got) != 1 || got[0] != cleaned {
			t.Fatalf("selected path %q fragments = %#v", anchor.path, got)
		}
	}
}

func TestQuoteLineageSimilarRequiresSamePathAndCanonicalValue(t *testing.T) {
	a := strings.Repeat("alpha", 20)
	b := "prefix " + a + " suffix"
	c := "outer " + b + " tail"
	value, err := canonicalScalar([]byte(`"Ada"`))
	if err != nil {
		t.Fatalf("canonicalScalar() error = %v", err)
	}
	otherValue, err := canonicalScalar([]byte(`"Bob"`))
	if err != nil {
		t.Fatalf("canonicalScalar() error = %v", err)
	}
	fields := map[string]scalarValue{"claim": value}

	if !quoteLineageSimilar(
		lineageIndex{"claim": {a}}, fields,
		lineageIndex{"claim": {b}}, fields,
	) {
		t.Fatal("A subset B did not match")
	}
	if !quoteLineageSimilar(
		lineageIndex{"claim": {b}}, fields,
		lineageIndex{"claim": {a}}, fields,
	) {
		t.Fatal("B superset A did not match symmetrically")
	}
	if !quoteLineageSimilar(
		lineageIndex{"claim": {a}}, fields,
		lineageIndex{"claim": {c}}, fields,
	) {
		t.Fatal("A subset B subset C chain endpoint did not match")
	}
	if quoteLineageSimilar(
		lineageIndex{"claim": {a}}, fields,
		lineageIndex{"claim": {b}}, map[string]scalarValue{"claim": otherValue},
	) {
		t.Fatal("same path with different canonical value matched")
	}
	if quoteLineageSimilar(
		lineageIndex{"claim": {a}}, fields,
		lineageIndex{"other": {b}}, map[string]scalarValue{"other": value},
	) {
		t.Fatal("different evidence paths matched")
	}

	canonicalOne, err := canonicalScalar([]byte(`1`))
	if err != nil {
		t.Fatalf("canonicalScalar(1) error = %v", err)
	}
	canonicalOnePointZero, err := canonicalScalar([]byte(`1.0`))
	if err != nil {
		t.Fatalf("canonicalScalar(1.0) error = %v", err)
	}
	if !quoteLineageSimilar(
		lineageIndex{"claim": {a}}, map[string]scalarValue{"claim": canonicalOne},
		lineageIndex{"claim": {b}}, map[string]scalarValue{"claim": canonicalOnePointZero},
	) {
		t.Fatal("canonically equal numeric values did not share a comparison bucket")
	}
}

func TestQuoteLineageSimilarUsesCanonicalScalarGroupKey(t *testing.T) {
	fragment := strings.Repeat("canonical wording ", 8)
	index := lineageIndex{"claim": {fragment}}
	integer, err := canonicalScalar([]byte(`2`))
	if err != nil {
		t.Fatalf("canonicalScalar(integer) error = %v", err)
	}
	decimal, err := canonicalScalar([]byte(`2.0`))
	if err != nil {
		t.Fatalf("canonicalScalar(decimal) error = %v", err)
	}
	text, err := canonicalScalar([]byte(`"2"`))
	if err != nil {
		t.Fatalf("canonicalScalar(text) error = %v", err)
	}
	if !quoteLineageSimilar(index, map[string]scalarValue{"claim": integer}, index, map[string]scalarValue{"claim": decimal}) {
		t.Fatal("canonically equal numeric values did not share a lineage bucket")
	}
	if quoteLineageSimilar(index, map[string]scalarValue{"claim": integer}, index, map[string]scalarValue{"claim": text}) {
		t.Fatal("numeric and string values shared a lineage bucket")
	}
}

func TestQuoteLineageSimilarUsesExactWholeFragmentContainment(t *testing.T) {
	value, err := canonicalScalar([]byte(`true`))
	if err != nil {
		t.Fatalf("canonicalScalar() error = %v", err)
	}
	fields := map[string]scalarValue{"claim": value}
	commonValue := "Ada"
	first := lineageIndex{"claim": {strings.Repeat("orchard ", 15) + commonValue + " copper"}}
	second := lineageIndex{"claim": {strings.Repeat("tundra ", 15) + commonValue + " quartz"}}
	if quoteLineageSimilar(first, fields, second, fields) {
		t.Fatal("common short scalar matched unrelated fragments")
	}

	almost := strings.Repeat("identical ", 11) + "ending-a"
	different := strings.Repeat("identical ", 11) + "ending-b"
	if quoteLineageSimilar(lineageIndex{"claim": {almost}}, fields, lineageIndex{"claim": {different}}, fields) {
		t.Fatal("similar but non-contained wording matched")
	}
	if !quoteLineageSimilar(lineageIndex{"claim": {almost}}, fields, lineageIndex{"claim": {almost}}, fields) {
		t.Fatal("identical long boilerplate did not match its documented positive limitation")
	}
}

func TestQuoteLineageSimilarNormalizesCRLFToLF(t *testing.T) {
	firstLine := strings.Repeat("a", 24) + " Ada " + strings.Repeat("b", 23)
	secondLine := strings.Repeat("c", 50)
	if lineageAlnumRunes(firstLine) >= minLineageAlnumRunes || lineageAlnumRunes(secondLine) >= minLineageAlnumRunes {
		t.Fatal("fixture line unexpectedly qualifies on its own")
	}
	firstCleaned := firstLine + "\r\n" + secondLine
	secondCleaned := firstLine + "\n" + secondLine
	firstStart := strings.Index(firstCleaned, "Ada")
	secondStart := strings.Index(secondCleaned, "Ada")
	first, err := buildLineageIndex(firstCleaned, singleLineageBasis(firstCleaned, "claim", firstStart, firstStart+3))
	if err != nil {
		t.Fatalf("first buildLineageIndex() error = %v", err)
	}
	second, err := buildLineageIndex(secondCleaned, singleLineageBasis(secondCleaned, "claim", secondStart, secondStart+3))
	if err != nil {
		t.Fatalf("second buildLineageIndex() error = %v", err)
	}
	value, err := canonicalScalar([]byte(`"Ada"`))
	if err != nil {
		t.Fatalf("canonicalScalar() error = %v", err)
	}
	fields := map[string]scalarValue{"claim": value}
	if len(first["claim"]) != 1 || len(second["claim"]) != 1 ||
		!strings.Contains(first["claim"][0], "\n") || !strings.Contains(second["claim"][0], "\n") {
		t.Fatalf("fixture did not isolate a multiline candidate: %#v / %#v", first, second)
	}
	if !quoteLineageSimilar(first, fields, second, fields) {
		t.Fatalf("CRLF/LF fragments did not match: %#v / %#v", first, second)
	}
}

func TestBuildLineageIndexDoesNotUseNavigationOutsideAnchorFragment(t *testing.T) {
	navigation := strings.Repeat("shared navigation ", 7)
	firstBody := strings.Repeat("orchard copper ", 8) + "Ada."
	secondBody := strings.Repeat("tundra quartz ", 8) + "Ada."
	firstCleaned := navigation + "\n\n" + firstBody
	secondCleaned := navigation + "\n\n" + secondBody
	firstStart := strings.LastIndex(firstCleaned, "Ada")
	secondStart := strings.LastIndex(secondCleaned, "Ada")
	first, err := buildLineageIndex(firstCleaned, singleLineageBasis(firstCleaned, "claim", firstStart, firstStart+3))
	if err != nil {
		t.Fatalf("first buildLineageIndex() error = %v", err)
	}
	second, err := buildLineageIndex(secondCleaned, singleLineageBasis(secondCleaned, "claim", secondStart, secondStart+3))
	if err != nil {
		t.Fatalf("second buildLineageIndex() error = %v", err)
	}
	value, err := canonicalScalar([]byte(`"Ada"`))
	if err != nil {
		t.Fatalf("canonicalScalar() error = %v", err)
	}
	fields := map[string]scalarValue{"claim": value}
	if quoteLineageSimilar(first, fields, second, fields) {
		t.Fatalf("shared navigation outside anchored fragments matched:\nfirst=%#v\nsecond=%#v", first, second)
	}
}

func TestBuildLineageIndexMaximumStructureIsThreeFragmentsPerSelectedAnchor(t *testing.T) {
	cleaned, basis := lineageMaximumFixture()
	index, err := buildLineageIndex(cleaned, basis)
	if err != nil {
		t.Fatalf("buildLineageIndex() error = %v", err)
	}
	total := 0
	for path, fragments := range index {
		if len(fragments) != 3 {
			t.Fatalf("path %q fragment count = %d, want 3: %#v", path, len(fragments), fragments)
		}
		total += len(fragments)
	}
	if len(index) != maxCoreAnchors || total != 3*maxCoreAnchors {
		t.Fatalf("index paths/fragments = %d/%d, want %d/%d", len(index), total, maxCoreAnchors, 3*maxCoreAnchors)
	}
}

func TestIndependentComponentsBoundsEightSourcesWithMaximumLineageIndex(t *testing.T) {
	sources := maximumPreparedLineageSources(t)
	components := independentComponents(sources)
	unique := make(map[int]struct{}, len(components))
	for _, component := range components {
		unique[component] = struct{}{}
	}
	if len(unique) != MaxSources {
		t.Fatalf("independent components = %d, want %d", len(unique), MaxSources)
	}
}

func TestBuildLineageIndexEmptyCleanedTextPreservesCompatibility(t *testing.T) {
	basis := map[string]evidence.Anchor{"claim": {
		Quote:     "not present",
		TextRange: [2]int{50, 61},
		Method:    evidence.MethodExact,
	}}
	index, err := buildLineageIndex("", basis)
	if err != nil || len(index) != 0 {
		t.Fatalf("empty cleaned index/error = %#v/%v, want empty/nil", index, err)
	}
}

func BenchmarkBuildLineageIndex256Anchors(b *testing.B) {
	cleaned, basis := lineageMaximumFixture()
	b.ReportAllocs()
	b.SetBytes(int64(len(cleaned)))
	for b.Loop() {
		index, err := buildLineageIndex(cleaned, basis)
		if err != nil || len(index) != maxCoreAnchors {
			b.Fatalf("buildLineageIndex() = (%d paths, %v)", len(index), err)
		}
	}
}

func BenchmarkIndependentComponentsEightMaximumLineageIndexes(b *testing.B) {
	sources := maximumPreparedLineageSources(b)
	b.ReportAllocs()
	for b.Loop() {
		components := independentComponents(sources)
		if len(components) != MaxSources {
			b.Fatalf("component count = %d", len(components))
		}
	}
}

type lineageTestHelper interface {
	Helper()
	Fatalf(string, ...any)
}

func maximumPreparedLineageSources(t lineageTestHelper) []preparedSource {
	t.Helper()
	value, err := canonicalScalar([]byte(`"same"`))
	if err != nil {
		t.Fatalf("canonicalScalar() error = %v", err)
	}
	sources := make([]preparedSource, MaxSources)
	for sourceID := range sources {
		index := make(lineageIndex, maxCoreAnchors)
		fields := make(map[string]scalarValue, maxCoreAnchors)
		for pathID := range maxCoreAnchors {
			path := fmt.Sprintf("field.%03d", pathID)
			fragments := make([]string, lineageFragmentKinds)
			for kind := range lineageFragmentKinds {
				prefix := fmt.Sprintf("source%02d-path%03d-kind%d-", sourceID, pathID, kind)
				fragments[kind] = prefix + strings.Repeat(string(rune('a'+sourceID)), minLineageAlnumRunes)
			}
			index[path] = fragments
			fields[path] = value
		}
		sources[sourceID] = preparedSource{
			url:     fmt.Sprintf("https://source-%d.example/report", sourceID),
			root:    fmt.Sprintf("source-%d.example", sourceID),
			fields:  fields,
			lineage: index,
		}
	}
	return sources
}

func singleLineageBasis(cleaned, path string, start, end int) map[string]evidence.Anchor {
	return map[string]evidence.Anchor{path: {
		Quote:     cleaned[start:end],
		TextRange: [2]int{start, end},
		Method:    evidence.MethodExact,
	}}
}

func lineageMaximumFixture() (string, map[string]evidence.Anchor) {
	var cleaned strings.Builder
	basis := make(map[string]evidence.Anchor, maxCoreAnchors)
	for index := 0; index < maxCoreAnchors; index++ {
		prefix := fmt.Sprintf("p%03d", index)
		cleaned.WriteString(strings.Repeat(prefix, 24))
		cleaned.WriteString(". ")
		cleaned.WriteString(strings.Repeat("s", 48))
		start := cleaned.Len()
		cleaned.WriteString("CLAIM")
		end := cleaned.Len()
		cleaned.WriteString(strings.Repeat("t", 48))
		cleaned.WriteString("? ")
		cleaned.WriteString(strings.Repeat("u", 100))
		cleaned.WriteString("!\n")
		cleaned.WriteString(strings.Repeat("v", 100))
		cleaned.WriteString("\n\n")
		basis[fmt.Sprintf("field.%03d", index)] = evidence.Anchor{
			Quote:     "CLAIM",
			TextRange: [2]int{start, end},
			Method:    evidence.MethodExact,
		}
	}
	return cleaned.String(), basis
}
