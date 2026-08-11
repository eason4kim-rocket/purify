package consensus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode"

	"github.com/use-agent/purify/evidence"
	"github.com/use-agent/purify/simhash"
)

func TestMergeQuoteLineageContainsSymmetricallyAndTransitively(t *testing.T) {
	short, medium, long := lineageMergeNestedFragments()
	data := map[string]string{"claim": "Ada"}
	first := lineageMergeSource(
		t,
		"https://alpha.com/report",
		"claim",
		"Ada",
		data,
		short,
		lineageMergeWords("alphaleft", 70),
		lineageMergeWords("alpharight", 70),
	)
	second := lineageMergeSource(
		t,
		"https://bravo.net/report",
		"claim",
		"Ada",
		data,
		medium,
		lineageMergeWords("bravoleft", 70),
		lineageMergeWords("bravoright", 70),
	)
	third := lineageMergeSource(
		t,
		"https://charlie.org/report",
		"claim",
		"Ada",
		data,
		long,
		lineageMergeWords("charlieleft", 70),
		lineageMergeWords("charlieright", 70),
	)
	lineageMergeAssertIsolatedLineagePair(t, first, second)
	lineageMergeAssertIsolatedLineagePair(t, second, third)
	lineageMergeAssertIsolatedLineagePair(t, first, third)

	t.Run("A contained in B", func(t *testing.T) {
		result, err := Merge([]SourceResult{first, second})
		if err != nil {
			t.Fatalf("Merge() error = %v", err)
		}
		lineageMergeAssertAgreement(t, result, "claim", 2, 1)
	})

	t.Run("containment is symmetric when B is first", func(t *testing.T) {
		result, err := Merge([]SourceResult{second, first})
		if err != nil {
			t.Fatalf("Merge() error = %v", err)
		}
		lineageMergeAssertAgreement(t, result, "claim", 2, 1)
	})

	t.Run("A contained in B contained in C is stable across permutations", func(t *testing.T) {
		var want []byte
		lineageMergeForEachPermutation([]SourceResult{first, second, third}, func(candidate []SourceResult) {
			result, err := Merge(candidate)
			if err != nil {
				t.Fatalf("Merge(permutation) error = %v", err)
			}
			lineageMergeAssertAgreement(t, result, "claim", 3, 1)
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatalf("Marshal(permutation) error = %v", err)
			}
			if want == nil {
				want = append([]byte(nil), encoded...)
			} else if !bytes.Equal(encoded, want) {
				t.Fatalf("permutation changed output:\n got %s\nwant %s", encoded, want)
			}
		})
	})
}

func TestMergeQuoteLineageRequiresSamePathAndCanonicalValue(t *testing.T) {
	short, medium, _ := lineageMergeNestedFragments()

	t.Run("same path with different values stays independent", func(t *testing.T) {
		first := lineageMergeSource(
			t,
			"https://alpha.com/report",
			"claim",
			"Ada",
			map[string]string{"claim": "Ada", "probe": "same"},
			short,
			"",
			"",
		)
		secondFragment := medium + " Grace"
		second := lineageMergeSource(
			t,
			"https://bravo.net/report",
			"claim",
			"Grace",
			map[string]string{"claim": "Grace", "probe": "same"},
			secondFragment,
			"",
			"",
		)
		first.SimText, second.SimText = 0, 0
		if !strings.Contains(secondFragment, short) {
			t.Fatal("fixture does not contain the first fragment")
		}
		lineageMergeAssertNoExistingFold(t, first, second)

		result, err := Merge([]SourceResult{first, second})
		if err != nil {
			t.Fatalf("Merge() error = %v", err)
		}
		lineageMergeAssertAgreement(t, result, "probe", 2, 2)
	})

	t.Run("same value on different evidence paths stays independent", func(t *testing.T) {
		data := map[string]string{"claim_a": "Ada", "claim_b": "Ada", "probe": "same"}
		first := lineageMergeSource(
			t,
			"https://alpha.com/report",
			"claim_a",
			"Ada",
			data,
			short,
			"",
			"",
		)
		second := lineageMergeSource(
			t,
			"https://bravo.net/report",
			"claim_b",
			"Ada",
			data,
			medium,
			"",
			"",
		)
		first.SimText, second.SimText = 0, 0
		lineageMergeAssertNoExistingFold(t, first, second)

		result, err := Merge([]SourceResult{first, second})
		if err != nil {
			t.Fatalf("Merge() error = %v", err)
		}
		lineageMergeAssertAgreement(t, result, "probe", 2, 2)
	})
}

func TestMergeQuoteLineageRejectsShortAndNonVerbatimFragments(t *testing.T) {
	t.Run("95 alphanumeric runes are below the lineage threshold", func(t *testing.T) {
		short := strings.Repeat("a", 46) + "Ada" + strings.Repeat("b", 46)
		if got := lineageMergeAlnumRunes(short); got != 95 {
			t.Fatalf("fixture alnum runes = %d, want 95", got)
		}
		longer := lineageMergeWords("prefix", 12) + " " + short + " " + lineageMergeWords("suffix", 12)
		data := map[string]string{"claim": "Ada", "probe": "same"}
		first := lineageMergeSource(t, "https://alpha.com/report", "claim", "Ada", data, short, "", "")
		second := lineageMergeSource(t, "https://bravo.net/report", "claim", "Ada", data, longer, "", "")
		first.SimText, second.SimText = 0, 0
		lineageMergeAssertNoExistingFold(t, first, second)

		result, err := Merge([]SourceResult{first, second})
		if err != nil {
			t.Fatalf("Merge() error = %v", err)
		}
		lineageMergeAssertAgreement(t, result, "probe", 2, 2)
	})

	t.Run("similar anchored boilerplate without exact containment stays independent", func(t *testing.T) {
		short, _, _ := lineageMergeNestedFragments()
		mutated := strings.Replace(short, "shared000", "changed000", 1)
		similar := lineageMergeWords("prefix", 18) + " " + mutated + " " + lineageMergeWords("suffix", 18)
		if strings.Contains(short, similar) || strings.Contains(similar, short) {
			t.Fatal("fixture accidentally has verbatim containment")
		}
		data := map[string]string{"claim": "Ada", "probe": "same"}
		first := lineageMergeSource(t, "https://alpha.com/report", "claim", "Ada", data, short, "", "")
		second := lineageMergeSource(t, "https://bravo.net/report", "claim", "Ada", data, similar, "", "")
		first.SimText, second.SimText = 0, 0
		lineageMergeAssertNoExistingFold(t, first, second)

		result, err := Merge([]SourceResult{first, second})
		if err != nil {
			t.Fatalf("Merge() error = %v", err)
		}
		lineageMergeAssertAgreement(t, result, "probe", 2, 2)
	})
}

func TestMergeQuoteLineageOversizedFragmentsRetainLegacyEdge(t *testing.T) {
	firstFragment := strings.Repeat("a", 511) + "Ada" + strings.Repeat("b", 511)
	secondFragment := strings.Repeat("c", 511) + "Ada" + strings.Repeat("d", 511)
	if len(firstFragment) != maxLineageFragmentBytes+1 || len(secondFragment) != maxLineageFragmentBytes+1 {
		t.Fatalf("fixture fragment lengths = %d/%d", len(firstFragment), len(secondFragment))
	}
	first := anchoredCleanedSource("https://alpha.com/report", firstFragment, "Ada")
	second := anchoredCleanedSource("https://bravo.net/report", secondFragment, "Ada")
	first.SimText, second.SimText = 0x1234, 0x1234
	preparedFirst, _, _, err := prepareSource(0, first)
	if err != nil {
		t.Fatalf("prepareSource(first) error = %v", err)
	}
	preparedSecond, _, _, err := prepareSource(1, second)
	if err != nil {
		t.Fatalf("prepareSource(second) error = %v", err)
	}
	if len(preparedFirst.lineage) != 0 || len(preparedSecond.lineage) != 0 {
		t.Fatalf("oversized lineage candidates were retained: %#v / %#v", preparedFirst.lineage, preparedSecond.lineage)
	}
	if contentCoresSimilar(preparedFirst.core, preparedSecond.core) {
		t.Fatalf("fixture unexpectedly has a core edge: %#v / %#v", preparedFirst.core, preparedSecond.core)
	}

	result, err := Merge([]SourceResult{first, second})
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	lineageMergeAssertAgreement(t, result, "name", 2, 1)
}

func TestMergeQuoteLineageSupportsUTF8CJK(t *testing.T) {
	short := lineageMergeHan(0, 50) + "阿达" + lineageMergeHan(200, 50)
	longer := lineageMergeHan(500, 70) + short + lineageMergeHan(700, 70)
	if lineageMergeAlnumRunes(short) < 96 || len(short) > 1<<10 || len(longer) > 1<<10 {
		t.Fatalf("invalid CJK fragment fixture: short runes=%d, byte lengths=%d/%d", lineageMergeAlnumRunes(short), len(short), len(longer))
	}
	data := map[string]string{"claim": "阿达"}
	first := lineageMergeSource(
		t,
		"https://alpha.com/report",
		"claim",
		"阿达",
		data,
		short,
		lineageMergeHan(1_000, 220),
		lineageMergeHan(1_300, 220),
	)
	second := lineageMergeSource(
		t,
		"https://bravo.net/report",
		"claim",
		"阿达",
		data,
		longer,
		lineageMergeHan(2_000, 220),
		lineageMergeHan(2_300, 220),
	)
	lineageMergeAssertIsolatedLineagePair(t, first, second)

	result, err := Merge([]SourceResult{first, second})
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	lineageMergeAssertAgreement(t, result, "claim", 2, 1)
}

func TestMergeQuoteLineageIsSourceGlobalForFieldsAndMaterialization(t *testing.T) {
	short, medium, _ := lineageMergeNestedFragments()
	first := lineageMergeSource(
		t,
		"https://alpha.com/report",
		"claim",
		"Ada",
		map[string]string{"claim": "Ada", "other": "copied"},
		short,
		lineageMergeWords("alphaleft", 70),
		lineageMergeWords("alpharight", 70),
	)
	second := lineageMergeSource(
		t,
		"https://bravo.net/report",
		"claim",
		"Ada",
		map[string]string{"claim": "Ada", "other": "copied"},
		medium,
		lineageMergeWords("bravoleft", 70),
		lineageMergeWords("bravoright", 70),
	)
	lineageMergeAssertIsolatedLineagePair(t, first, second)
	third := lineageMergeBareSource(
		t,
		"https://charlie.org/report",
		map[string]string{"claim": "Bob", "other": "original"},
	)
	fourth := lineageMergeBareSource(
		t,
		"https://delta.io/report",
		map[string]string{"claim": "Bob", "other": "original"},
	)

	result, materialization, err := MergeWithMaterialization([]SourceResult{first, second, third, fourth})
	if err != nil {
		t.Fatalf("MergeWithMaterialization() error = %v", err)
	}
	lineageMergeAssertAgreement(t, result, "claim", 2, 2)
	lineageMergeAssertAgreement(t, result, "other", 2, 2)
	if string(result.Fields["claim"].Value) != "\"Bob\"" || string(result.Fields["other"].Value) != "\"original\"" {
		t.Fatalf("source-global lineage did not change both field winners: %#v", result.Fields)
	}
	if materialization.Status != MaterializationStatusComplete ||
		string(materialization.Data) != "{\"claim\":\"Bob\",\"other\":\"original\"}" {
		t.Fatalf("materialization did not use source-global lineage component: %#v", materialization)
	}
}

func TestMergeQuoteLineageConnectsMixedSameRootCoreAndLineageGraph(t *testing.T) {
	short, medium, _ := lineageMergeNestedFragments()
	data := map[string]string{"claim": "Ada"}
	first := lineageMergeSource(
		t,
		"https://one.alpha.com/report",
		"claim",
		"Ada",
		data,
		lineageMergeWords("unrelated", 18)+" Ada "+lineageMergeWords("source", 18),
		lineageMergeWords("firstleft", 70),
		lineageMergeWords("firstright", 70),
	)
	secondFragment := strings.Replace(short, "shared000", "changed000", 1)
	second := lineageMergeSource(
		t,
		"https://two.alpha.com/report",
		"claim",
		"Ada",
		data,
		secondFragment,
		lineageMergeWords("coreleft", 70),
		lineageMergeWords("coreright", 70),
	)
	third := lineageMergeSource(
		t,
		"https://bravo.net/report",
		"claim",
		"Ada",
		data,
		short,
		lineageMergeWords("coreleft", 70),
		lineageMergeWords("coreright", 70),
	)
	fourth := lineageMergeSource(
		t,
		"https://charlie.org/report",
		"claim",
		"Ada",
		data,
		medium,
		lineageMergeWords("fourthleft", 70),
		lineageMergeWords("fourthright", 70),
	)
	for _, source := range []*SourceResult{&first, &second, &third, &fourth} {
		source.SimText = 0
	}
	prepared := make([]preparedSource, 4)
	for index, source := range []SourceResult{first, second, third, fourth} {
		var err error
		prepared[index], _, _, err = prepareSource(index, source)
		if err != nil {
			t.Fatalf("prepareSource(%d) error = %v", index, err)
		}
	}
	if prepared[0].root != prepared[1].root {
		t.Fatalf("same-root fixture roots = %q/%q", prepared[0].root, prepared[1].root)
	}
	if !contentCoresSimilar(prepared[1].core, prepared[2].core) ||
		quoteLineageSimilar(prepared[1].lineage, prepared[1].fields, prepared[2].lineage, prepared[2].fields) {
		t.Fatalf("B-C fixture is not core-only: cores=%#v/%#v lineage=%#v/%#v", prepared[1].core, prepared[2].core, prepared[1].lineage, prepared[2].lineage)
	}
	if !quoteLineageSimilar(prepared[2].lineage, prepared[2].fields, prepared[3].lineage, prepared[3].fields) ||
		contentCoresSimilar(prepared[2].core, prepared[3].core) {
		t.Fatalf("C-D fixture is not lineage-only: cores=%#v/%#v lineage=%#v/%#v", prepared[2].core, prepared[3].core, prepared[2].lineage, prepared[3].lineage)
	}

	var want []byte
	lineageMergeForEachPermutation([]SourceResult{first, second, third, fourth}, func(candidate []SourceResult) {
		result, err := Merge(candidate)
		if err != nil {
			t.Fatalf("Merge(permutation) error = %v", err)
		}
		lineageMergeAssertAgreement(t, result, "claim", 4, 1)
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("Marshal(permutation) error = %v", err)
		}
		if want == nil {
			want = append([]byte(nil), encoded...)
		} else if !bytes.Equal(encoded, want) {
			t.Fatalf("permutation changed mixed graph output:\n got %s\nwant %s", encoded, want)
		}
	})
}

func lineageMergeNestedFragments() (string, string, string) {
	short := lineageMergeWords("shared", 8) + " Ada " + lineageMergeWords("quoted", 8)
	medium := lineageMergeWords("derivedleft", 10) + " " + short + " " + lineageMergeWords("derivedright", 10)
	long := lineageMergeWords("extendedleft", 7) + " " + medium + " " + lineageMergeWords("extendedright", 7)
	return short, medium, long
}

func lineageMergeSource(
	t *testing.T,
	url string,
	path string,
	quote string,
	data map[string]string,
	fragment string,
	leftContext string,
	rightContext string,
) SourceResult {
	t.Helper()
	if strings.Count(fragment, quote) != 1 {
		t.Fatalf("fragment must contain quote %q exactly once", quote)
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("Marshal(data) error = %v", err)
	}
	cleaned := leftContext + "\n" + fragment + "\n" + rightContext
	start := len(leftContext) + 1 + strings.Index(fragment, quote)
	return SourceResult{
		URL:         url,
		Data:        encoded,
		Basis:       map[string]evidence.Anchor{path: {Quote: quote, Method: evidence.MethodExact, TextRange: [2]int{start, start + len(quote)}}},
		SimText:     simhash.Fingerprint(cleaned),
		CleanedText: cleaned,
	}
}

func lineageMergeBareSource(t *testing.T, url string, data map[string]string) SourceResult {
	t.Helper()
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("Marshal(data) error = %v", err)
	}
	return SourceResult{URL: url, Data: encoded}
}

func lineageMergeAssertIsolatedLineagePair(t *testing.T, first, second SourceResult) {
	t.Helper()
	preparedFirst, _, _, err := prepareSource(0, first)
	if err != nil {
		t.Fatalf("prepareSource(first) error = %v", err)
	}
	preparedSecond, _, _, err := prepareSource(1, second)
	if err != nil {
		t.Fatalf("prepareSource(second) error = %v", err)
	}
	if preparedFirst.root == preparedSecond.root {
		t.Fatalf("fixture roots are equal: %q", preparedFirst.root)
	}
	if first.SimText == 0 || second.SimText == 0 {
		t.Fatal("fixture legacy fingerprints must be nonzero")
	}
	if distance := simhash.Distance(first.SimText, second.SimText); distance <= independenceDistance {
		t.Fatalf("fixture legacy distance = %d, want > %d", distance, independenceDistance)
	}
	if !preparedFirst.core.valid || !preparedSecond.core.valid {
		t.Fatalf("fixture cores are invalid: %#v / %#v", preparedFirst.core, preparedSecond.core)
	}
	if distance := simhash.Distance(preparedFirst.core.fingerprint, preparedSecond.core.fingerprint); distance <= coreDistance {
		t.Fatalf("fixture core distance = %d, want > %d", distance, coreDistance)
	}
}

func lineageMergeAssertNoExistingFold(t *testing.T, first, second SourceResult) {
	t.Helper()
	preparedFirst, _, _, err := prepareSource(0, first)
	if err != nil {
		t.Fatalf("prepareSource(first) error = %v", err)
	}
	preparedSecond, _, _, err := prepareSource(1, second)
	if err != nil {
		t.Fatalf("prepareSource(second) error = %v", err)
	}
	if preparedFirst.root == preparedSecond.root {
		t.Fatalf("fixture roots are equal: %q", preparedFirst.root)
	}
	if first.SimText != 0 && second.SimText != 0 &&
		simhash.Distance(first.SimText, second.SimText) <= independenceDistance {
		t.Fatal("fixture has a legacy similarity edge")
	}
	if contentCoresSimilar(preparedFirst.core, preparedSecond.core) {
		t.Fatalf("fixture has a content-core edge: %#v / %#v", preparedFirst.core, preparedSecond.core)
	}
}

func lineageMergeAssertAgreement(t *testing.T, result Result, path string, pages, roots int) {
	t.Helper()
	field, exists := result.Fields[path]
	if !exists {
		t.Fatalf("field %q is absent: %#v", path, result.Fields)
	}
	if field.Agreement != (Agreement{Pages: pages, IndependentRoots: roots}) {
		t.Fatalf("field %q agreement = %#v, want pages=%d roots=%d", path, field.Agreement, pages, roots)
	}
}

func lineageMergeWords(prefix string, count int) string {
	words := make([]string, count)
	for index := range words {
		words[index] = fmt.Sprintf("%s%03d", prefix, index)
	}
	return strings.Join(words, " ")
}

func lineageMergeHan(offset, count int) string {
	var builder strings.Builder
	for index := range count {
		builder.WriteRune(rune(0x3400 + (offset+index)%6_000))
	}
	return builder.String()
}

func lineageMergeAlnumRunes(value string) int {
	count := 0
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			count++
		}
	}
	return count
}

func lineageMergeForEachPermutation(values []SourceResult, visit func([]SourceResult)) {
	candidate := append([]SourceResult(nil), values...)
	var generate func(int)
	generate = func(index int) {
		if index == len(candidate) {
			visit(append([]SourceResult(nil), candidate...))
			return
		}
		for swap := index; swap < len(candidate); swap++ {
			candidate[index], candidate[swap] = candidate[swap], candidate[index]
			generate(index + 1)
			candidate[index], candidate[swap] = candidate[swap], candidate[index]
		}
	}
	generate(0)
}
