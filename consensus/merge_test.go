package consensus

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/use-agent/purify/evidence"
	"github.com/use-agent/purify/simhash"
)

func TestMergeAllConsistentUsesIndependentRoots(t *testing.T) {
	inputs := []SourceResult{
		testSource("https://alpha.com/report", `{"price":1,"meta":{"name":"pro"},"features":[true,null]}`, 0),
		testSource("https://bravo.net/report", `{"features":[true,null],"meta":{"name":"pro"},"price":1.0}`, 0),
		testSource("https://charlie.org/report", `{"price":1e0,"meta":{"name":"pro"},"features":[true,null]}`, 0),
	}

	result, err := Merge(inputs)
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	if len(result.Fields) != 4 {
		t.Fatalf("field count = %d, want 4: %#v", len(result.Fields), result.Fields)
	}
	for _, path := range []string{"price", "meta.name", "features.0", "features.1"} {
		field := result.Fields[path]
		if field.Ambiguous || field.Agreement != (Agreement{Pages: 3, IndependentRoots: 3}) || len(field.Supports) != 3 || len(field.Conflicts) != 0 {
			t.Fatalf("field %q = %#v", path, field)
		}
	}
	if string(result.Fields["price"].Value) != "1" {
		t.Fatalf("canonical price = %s, want 1", result.Fields["price"].Value)
	}
	if got := supportURLs(result.Fields["price"].Supports); strings.Join(got, ",") != "https://alpha.com/report,https://bravo.net/report,https://charlie.org/report" {
		t.Fatalf("support order = %v", got)
	}
}

func TestMergeCollapsesSameRootAndTransitiveMirrors(t *testing.T) {
	inputs := []SourceResult{
		testSource("https://one.alpha.com/a", `{"claim":"yes"}`, 0x01),
		testSource("https://two.alpha.com/b", `{"claim":"yes"}`, 0xff00),
		testSource("https://bravo.net/c", `{"claim":"yes"}`, 0x07),
		testSource("https://charlie.org/d", `{"claim":"yes"}`, 0x3f),
	}
	result, err := Merge(inputs)
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	field := result.Fields["claim"]
	if field.Agreement != (Agreement{Pages: 4, IndependentRoots: 1}) {
		t.Fatalf("agreement = %#v, want four pages collapsed through same-root + transitive similarity", field.Agreement)
	}
}

func TestMergeCollapsesAnchoredContentCoresAcrossDifferentChrome(t *testing.T) {
	body := numberedWords("wire", 180) + " Ada " + numberedWords("report", 180)
	first := anchoredCleanedSource(
		"https://alpha.com/report",
		numberedWords("alpha-nav", 700)+body+numberedWords("alpha-footer", 700),
		"Ada",
	)
	second := anchoredCleanedSource(
		"https://bravo.net/report",
		numberedWords("bravo-nav", 700)+body+numberedWords("bravo-footer", 700),
		"Ada",
	)
	if distance := simhash.Distance(first.SimText, second.SimText); distance <= independenceDistance {
		t.Fatalf("fixture legacy distance = %d, want > %d", distance, independenceDistance)
	}

	forward, err := Merge([]SourceResult{first, second})
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	reverse, err := Merge([]SourceResult{second, first})
	if err != nil {
		t.Fatalf("reverse Merge() error = %v", err)
	}
	if forward.Fields["name"].Agreement != (Agreement{Pages: 2, IndependentRoots: 1}) {
		t.Fatalf("agreement = %#v, want anchored mirror collapse", forward.Fields["name"].Agreement)
	}
	forwardJSON, _ := json.Marshal(forward)
	reverseJSON, _ := json.Marshal(reverse)
	if !bytes.Equal(forwardJSON, reverseJSON) {
		t.Fatalf("permutation changed output:\nforward=%s\nreverse=%s", forwardJSON, reverseJSON)
	}
}

func TestMergeCollapsesChineseAnchoredContentCoresAcrossDifferentChrome(t *testing.T) {
	body := hanSequence(0, 220) + "阿达" + hanSequence(500, 220)
	first := anchoredCleanedSource(
		"https://alpha.com/report",
		numberedWords("alpha-nav", 700)+body+numberedWords("alpha-footer", 700),
		"阿达",
	)
	second := anchoredCleanedSource(
		"https://bravo.net/report",
		numberedWords("bravo-nav", 700)+body+numberedWords("bravo-footer", 700),
		"阿达",
	)
	if distance := simhash.Distance(first.SimText, second.SimText); distance <= independenceDistance {
		t.Fatalf("fixture legacy distance = %d, want > %d", distance, independenceDistance)
	}

	result, err := Merge([]SourceResult{first, second})
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	if result.Fields["name"].Agreement != (Agreement{Pages: 2, IndependentRoots: 1}) {
		t.Fatalf("agreement = %#v, want CJK rune-shingle mirror collapse", result.Fields["name"].Agreement)
	}
}

func TestMergeDoesNotFoldIndependentAnchoredContextsWithSharedScalar(t *testing.T) {
	first := anchoredCleanedSource(
		"https://alpha.com/report",
		numberedWords("orchard", 180)+" Ada "+numberedWords("copper", 180),
		"Ada",
	)
	second := anchoredCleanedSource(
		"https://bravo.net/report",
		numberedWords("tundra", 180)+" Ada "+numberedWords("quartz", 180),
		"Ada",
	)
	if distance := simhash.Distance(first.SimText, second.SimText); distance <= independenceDistance {
		t.Fatalf("fixture legacy distance = %d, want > %d", distance, independenceDistance)
	}

	result, err := Merge([]SourceResult{first, second})
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	if result.Fields["name"].Agreement != (Agreement{Pages: 2, IndependentRoots: 2}) {
		t.Fatalf("agreement = %#v, shared scalar/short quote caused false fold", result.Fields["name"].Agreement)
	}
}

func TestContentCoresSimilarUsesExplicitValidityDistanceAndUnsaturatedLength(t *testing.T) {
	base := coreDescriptor{fingerprint: 0, retainedShingles: 4_096, normalizedAlnumRunes: 100, valid: true}
	if !contentCoresSimilar(base, coreDescriptor{fingerprint: 0b111, retainedShingles: 4_096, normalizedAlnumRunes: 142, valid: true}) {
		t.Fatal("valid zero fingerprint at distance 3 and 70% length boundary did not match")
	}
	if contentCoresSimilar(base, coreDescriptor{fingerprint: 0b1111, retainedShingles: 24, normalizedAlnumRunes: 100, valid: true}) {
		t.Fatal("distance 4 matched")
	}
	if contentCoresSimilar(base, coreDescriptor{fingerprint: 0b111, retainedShingles: 4_096, normalizedAlnumRunes: 143, valid: true}) {
		t.Fatal("retained-count saturation bypassed the normalized length ratio")
	}
	invalid := base
	invalid.valid = false
	if contentCoresSimilar(base, invalid) {
		t.Fatal("invalid core matched")
	}
}

func TestMergeIndependentEvidenceBeatsSixMirrors(t *testing.T) {
	inputs := []SourceResult{
		testSource("https://truth-one.com/a", `{"available":true}`, 0),
		testSource("https://truth-two.net/b", `{"available":true}`, 0),
	}
	for _, domain := range []string{"mirror-a.com", "mirror-b.net", "mirror-c.org", "mirror-d.io", "mirror-e.dev", "mirror-f.app"} {
		inputs = append(inputs, testSource("https://"+domain+"/wire", `{"available":false}`, 0x1234))
	}

	result, err := Merge(inputs)
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	field := result.Fields["available"]
	if string(field.Value) != "true" || field.Agreement != (Agreement{Pages: 2, IndependentRoots: 2}) {
		t.Fatalf("winner = %#v", field)
	}
	if len(field.Conflicts) != 1 || string(field.Conflicts[0].Value) != "false" ||
		field.Conflicts[0].Agreement != (Agreement{Pages: 6, IndependentRoots: 1}) {
		t.Fatalf("mirror conflict = %#v", field.Conflicts)
	}
}

func TestMergeConflictsSortByIndependentRootsBeforePages(t *testing.T) {
	inputs := []SourceResult{
		testSource("https://winner-a.com/a", `{"status":"winner"}`, 0),
		testSource("https://winner-b.net/b", `{"status":"winner"}`, 0),
		testSource("https://winner-c.org/c", `{"status":"winner"}`, 0),
		testSource("https://conflict-a.com/a", `{"status":"two-roots"}`, 0),
		testSource("https://conflict-b.net/b", `{"status":"two-roots"}`, 0),
		testSource("https://wire-a.com/a", `{"status":"three-pages"}`, 0x55),
		testSource("https://wire-b.net/b", `{"status":"three-pages"}`, 0x55),
		testSource("https://wire-c.org/c", `{"status":"three-pages"}`, 0x55),
	}
	result, err := Merge(inputs)
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	field := result.Fields["status"]
	if string(field.Value) != `"winner"` || field.Agreement != (Agreement{Pages: 3, IndependentRoots: 3}) {
		t.Fatalf("winner = %#v", field)
	}
	if len(field.Conflicts) != 2 || string(field.Conflicts[0].Value) != `"two-roots"` ||
		field.Conflicts[0].Agreement != (Agreement{Pages: 2, IndependentRoots: 2}) ||
		string(field.Conflicts[1].Value) != `"three-pages"` ||
		field.Conflicts[1].Agreement != (Agreement{Pages: 3, IndependentRoots: 1}) {
		t.Fatalf("conflicts = %#v", field.Conflicts)
	}
}

func TestMergeTieIsExplicitlyAmbiguous(t *testing.T) {
	result, err := Merge([]SourceResult{
		testSource("https://alpha.com/a", `{"count":2}`, 0),
		testSource("https://bravo.net/b", `{"count":1}`, 0),
	})
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	field := result.Fields["count"]
	if !field.Ambiguous || field.Value != nil || field.Supports != nil || field.Agreement != (Agreement{}) {
		t.Fatalf("ambiguous field silently selected a winner: %#v", field)
	}
	if len(field.Conflicts) != 2 || string(field.Conflicts[0].Value) != "1" || string(field.Conflicts[1].Value) != "2" {
		t.Fatalf("stable candidates = %#v", field.Conflicts)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if strings.Contains(string(encoded), `"value":null`) || strings.Contains(string(encoded), `"supports":null`) {
		t.Fatalf("ambiguous result emitted a selected value/supports: %s", encoded)
	}
}

func TestMergeTypedExactCanonicalValues(t *testing.T) {
	result, err := Merge([]SourceResult{
		testSource("https://alpha.com/a", `{"value":1}`, 0),
		testSource("https://bravo.net/b", `{"value":1.0}`, 0),
		testSource("https://charlie.org/c", `{"value":1e0}`, 0),
		testSource("https://delta.io/d", `{"value":"1"}`, 0),
	})
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	field := result.Fields["value"]
	if string(field.Value) != "1" || field.Agreement != (Agreement{Pages: 3, IndependentRoots: 3}) {
		t.Fatalf("exact-number winner = %#v", field)
	}
	if len(field.Conflicts) != 1 || string(field.Conflicts[0].Value) != `"1"` {
		t.Fatalf("typed string conflict = %#v", field.Conflicts)
	}

	numbers := []struct {
		raw  string
		want string
	}{
		{`-0`, `0`},
		{`0.0100`, `0.01`},
		{`1.2300e2`, `123`},
		{`12300e-1`, `1230`},
		{`1e-3`, `0.001`},
		{`-1.20e2`, `-120`},
	}
	for _, test := range numbers {
		t.Run(test.raw, func(t *testing.T) {
			result, err := Merge([]SourceResult{testSource("https://alpha.com/number", test.raw, 0)})
			if err != nil {
				t.Fatalf("Merge(%s) error = %v", test.raw, err)
			}
			if got := string(result.Fields["$"].Value); got != test.want {
				t.Fatalf("canonical value = %s, want %s", got, test.want)
			}
		})
	}
}

func TestMergeNullBooleanAndStringRemainDistinctTypes(t *testing.T) {
	result, err := Merge([]SourceResult{
		testSource("https://alpha.com/a", `{"value":null}`, 0),
		testSource("https://bravo.net/b", `{"value":null}`, 0),
		testSource("https://charlie.org/c", `{"value":"null"}`, 0),
		testSource("https://delta.io/d", `{"value":false}`, 0),
	})
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	field := result.Fields["value"]
	if string(field.Value) != "null" || field.Agreement != (Agreement{Pages: 2, IndependentRoots: 2}) {
		t.Fatalf("typed null winner = %#v", field)
	}
	if len(field.Conflicts) != 2 || string(field.Conflicts[0].Value) != "false" || string(field.Conflicts[1].Value) != `"null"` {
		t.Fatalf("typed conflicts = %#v", field.Conflicts)
	}
}

func TestMergeCanonicalStringIsValidJSONWithoutHTMLEnvelopeExpansion(t *testing.T) {
	result, err := Merge([]SourceResult{testSource("https://alpha.com/a", `{"value":"<>&\u2028"}`, 0)})
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	raw := result.Fields["value"].Value
	if !json.Valid(raw) || strings.Contains(string(raw), `\u003c`) || strings.Contains(string(raw), `\u003e`) || strings.Contains(string(raw), `\u0026`) {
		t.Fatalf("canonical string = %q", raw)
	}
	var decoded string
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded != "<>&\u2028" {
		t.Fatalf("decoded canonical string = %q, %v", decoded, err)
	}
}

func TestMergeMissingNestedAndEscapedPaths(t *testing.T) {
	result, err := Merge([]SourceResult{
		testSource("https://alpha.com/a", `{"profile":{"name":"Ada"},"a.b":7}`, 0),
		testSource("https://bravo.net/b", `{"profile":{"name":"Ada"}}`, 0),
	})
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	if result.Fields["profile.name"].Agreement != (Agreement{Pages: 2, IndependentRoots: 2}) {
		t.Fatalf("nested agreement = %#v", result.Fields["profile.name"])
	}
	if result.Fields[`a\.b`].Agreement != (Agreement{Pages: 1, IndependentRoots: 1}) || string(result.Fields[`a\.b`].Value) != "7" {
		t.Fatalf("escaped dotted path = %#v", result.Fields[`a\.b`])
	}
}

func TestMergeZeroSimHashDoesNotCollapseDifferentRoots(t *testing.T) {
	result, err := Merge([]SourceResult{
		testSource("https://alpha.com/a", `{"x":true}`, 0),
		testSource("https://bravo.net/b", `{"x":true}`, 0),
	})
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	if got := result.Fields["x"].Agreement.IndependentRoots; got != 2 {
		t.Fatalf("zero simhash roots = %d, want 2", got)
	}
}

func TestMergeLiteralPublicIPRoots(t *testing.T) {
	t.Run("same IPv4", func(t *testing.T) {
		result, err := Merge([]SourceResult{
			testSource("https://8.8.8.8/a", `{"x":true}`, 0),
			testSource("http://8.8.8.8/b", `{"x":true}`, 0),
			testSource("https://[::ffff:8.8.8.8]/c", `{"x":true}`, 0),
		})
		if err != nil {
			t.Fatalf("Merge() error = %v", err)
		}
		field := result.Fields["x"]
		if field.Agreement != (Agreement{Pages: 3, IndependentRoots: 1}) ||
			field.Supports[0].Root != "8.8.8.8" || field.Supports[1].Root != "8.8.8.8" || field.Supports[2].Root != "8.8.8.8" {
			t.Fatalf("same-IP field = %#v", field)
		}
	})

	t.Run("different IPv4 and IPv6", func(t *testing.T) {
		result, err := Merge([]SourceResult{
			testSource("https://8.8.8.8/a", `{"x":true}`, 0),
			testSource("https://1.1.1.1/b", `{"x":true}`, 0),
			testSource("https://[2606:4700:4700::1111]/c", `{"x":true}`, 0),
		})
		if err != nil {
			t.Fatalf("Merge() error = %v", err)
		}
		field := result.Fields["x"]
		if field.Agreement != (Agreement{Pages: 3, IndependentRoots: 3}) {
			t.Fatalf("different-IP agreement = %#v", field.Agreement)
		}
		roots := []string{field.Supports[0].Root, field.Supports[1].Root, field.Supports[2].Root}
		sort.Strings(roots)
		if strings.Join(roots, ",") != "1.1.1.1,2606:4700:4700::1111,8.8.8.8" {
			t.Fatalf("literal roots = %v", roots)
		}
	})
}

func TestMergeSupportKeepsEvidenceAndReceiptAssociated(t *testing.T) {
	fetchedAt := time.Date(2026, 8, 9, 12, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	inputs := []SourceResult{
		{
			URL:  "https://bravo.net/b",
			Data: json.RawMessage(`{"price":29}`),
			Basis: map[string]evidence.Anchor{"price": {
				Quote: "bravo-29", Method: evidence.MethodExact, SnapshotID: "snapshot-b", FetchedAt: fetchedAt,
			}},
			Receipts: map[string]string{"price": "receipt-b"},
		},
		{
			URL:  "https://alpha.com/a",
			Data: json.RawMessage(`{"price":29}`),
			Basis: map[string]evidence.Anchor{"price": {
				Quote: "alpha-29", Method: evidence.MethodExact, SnapshotID: "snapshot-a", FetchedAt: fetchedAt.UTC(),
			}},
			Receipts: map[string]string{"price": "receipt-a"},
		},
	}
	result, err := Merge(inputs)
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	supports := result.Fields["price"].Supports
	if len(supports) != 2 || supports[0].URL != "https://alpha.com/a" || supports[0].Root != "alpha.com" ||
		supports[0].Receipt != "receipt-a" || supports[0].Evidence == nil || supports[0].Evidence.Quote != "alpha-29" ||
		supports[1].URL != "https://bravo.net/b" || supports[1].Receipt != "receipt-b" ||
		supports[1].Evidence == nil || supports[1].Evidence.Quote != "bravo-29" {
		t.Fatalf("supports lost provenance association: %#v", supports)
	}
	if supports[0].Evidence.FetchedAt.Location() != time.UTC || supports[1].Evidence.FetchedAt.Location() != time.UTC {
		t.Fatalf("support times were not canonicalized: %#v", supports)
	}
}

func TestMergeDuplicateCanonicalURL(t *testing.T) {
	first := testSource("HTTPS://Example.com:443/report#first", `{"x":1,"empty":[]}`, 7)
	second := testSource("https://example.com/report#second", `{"empty":[],"x":1.0}`, 7)
	result, err := Merge([]SourceResult{first, second})
	if err != nil {
		t.Fatalf("identical duplicate Merge() error = %v", err)
	}
	if result.Fields["x"].Agreement != (Agreement{Pages: 1, IndependentRoots: 1}) || len(result.Fields["x"].Supports) != 1 {
		t.Fatalf("duplicate was not deduplicated: %#v", result.Fields["x"])
	}

	second.Data = json.RawMessage(`{"x":2,"empty":[]}`)
	if _, err := Merge([]SourceResult{first, second}); !errors.Is(err, ErrDuplicateSourceConflict) {
		t.Fatalf("conflicting data error = %v, want ErrDuplicateSourceConflict", err)
	}
	second.Data = first.Data
	second.SimText = first.SimText + 1
	if _, err := Merge([]SourceResult{first, second}); !errors.Is(err, ErrDuplicateSourceConflict) {
		t.Fatalf("conflicting simhash error = %v, want ErrDuplicateSourceConflict", err)
	}
	second.SimText = first.SimText
	first.CleanedText = "first cleaned snapshot"
	second.CleanedText = "second cleaned snapshot"
	if _, err := Merge([]SourceResult{first, second}); !errors.Is(err, ErrDuplicateSourceConflict) {
		t.Fatalf("conflicting cleaned text error = %v, want ErrDuplicateSourceConflict", err)
	}
}

func TestMergeRejectsCrossSourcePathStructuralAmbiguity(t *testing.T) {
	tests := []struct {
		name   string
		first  string
		second string
	}{
		{name: "dotted key versus backslash parent", first: `{"a.b":1}`, second: `{"a\\":{"b":1}}`},
		{name: "object numeric key versus array index", first: `{"a":{"0":1}}`, second: `{"a":[1]}`},
		{name: "root versus dollar key", first: `1`, second: `{"$":1}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			orders := [][]SourceResult{
				{testSource("https://alpha.com/a", test.first, 0), testSource("https://bravo.net/b", test.second, 0)},
				{testSource("https://bravo.net/b", test.second, 0), testSource("https://alpha.com/a", test.first, 0)},
			}
			for _, inputs := range orders {
				if _, err := Merge(inputs); !errors.Is(err, ErrInvalidInput) {
					t.Fatalf("Merge() error = %v, want ErrInvalidInput", err)
				}
			}
		})
	}
}

func TestMergeEmptyContainersProduceExplicitEmptyFields(t *testing.T) {
	result, err := Merge([]SourceResult{
		testSource("https://alpha.com/a", `{}`, 0),
		testSource("https://bravo.net/b", `[]`, 0),
	})
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	if result.Fields == nil || len(result.Fields) != 0 {
		t.Fatalf("fields = %#v, want non-nil empty map", result.Fields)
	}
	encoded, err := json.Marshal(result)
	if err != nil || string(encoded) != `{"fields":{}}` {
		t.Fatalf("encoded empty result = %s, %v", encoded, err)
	}
}

func TestMergeOutputIsByteStableAcrossPermutations(t *testing.T) {
	fetchedAt := time.Date(2026, 8, 9, 8, 0, 0, 0, time.UTC)
	inputs := []SourceResult{
		provenanceSource("https://delta.io/d", `{"stock":"no","meta":{"count":2}}`, 0, "delta", fetchedAt),
		provenanceSource("https://alpha.com/a", `{"stock":"yes","meta":{"count":2.0}}`, 0, "alpha", fetchedAt),
		provenanceSource("https://charlie.org/c", `{"stock":"yes","meta":{"count":2e0}}`, 0, "charlie", fetchedAt),
		provenanceSource("https://bravo.net/b", `{"stock":"no","meta":{"count":2}}`, 0, "bravo", fetchedAt),
	}

	var want []byte
	permutations(inputs, func(candidate []SourceResult) {
		result, err := Merge(candidate)
		if err != nil {
			t.Fatalf("Merge(permutation) error = %v", err)
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("Marshal(permutation) error = %v", err)
		}
		if want == nil {
			want = append([]byte(nil), encoded...)
			return
		}
		if string(encoded) != string(want) {
			t.Fatalf("output is order-dependent:\n got %s\nwant %s", encoded, want)
		}
	})
}

func TestOutputBudgetBoundaryAndExactMeasurement(t *testing.T) {
	budget := &outputBudget{}
	if err := budget.reserve(MaxOutputBytes - 1); err != nil {
		t.Fatalf("reserve(N-1) error = %v", err)
	}
	if err := budget.reserve(1); err != nil || budget.used != MaxOutputBytes {
		t.Fatalf("reserve(N) = (%d, %v)", budget.used, err)
	}
	if err := budget.reserve(1); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("reserve(N+1) error = %v, want ErrResourceLimit", err)
	}

	richAlpha := provenanceSource("https://alpha.com/a", `{"stock":"<yes>","count":2,"<&>":"\u2028"}`, 0, "alpha<&>", time.Date(2026, 8, 9, 1, 2, 3, 120_000_000, time.UTC))
	richAnchor := richAlpha.Basis["stock"]
	richAnchor.Selector = `a[data-x="<>&"]`
	richAlpha.Basis["stock"] = richAnchor
	richAlpha.Receipts["stock"] = "receipt:<>&\u2028"
	cases := map[string][]SourceResult{
		"winner and conflict with provenance": {
			richAlpha,
			provenanceSource("https://bravo.net/b", `{"stock":"no","count":2.0,"<&>":"\u2028"}`, 0, "bravo", time.Date(2026, 8, 9, 1, 2, 3, 0, time.UTC)),
			provenanceSource("https://charlie.org/c", `{"stock":"<yes>","count":2e0,"<&>":"\u2028"}`, 0, "charlie", time.Date(2026, 8, 9, 1, 2, 3, 1, time.UTC)),
		},
		"ambiguous": {
			testSource("https://alpha.com/a", `{"x":"a"}`, 0),
			testSource("https://bravo.net/b", `{"x":"b"}`, 0),
		},
		"empty": {
			testSource("https://alpha.com/a", `{}`, 0),
		},
	}
	for name, inputs := range cases {
		t.Run(name, func(t *testing.T) {
			prepared := prepareSourcesForTest(t, inputs)
			components := independentComponents(prepared)
			paths := collectPaths(prepared)
			predicted, err := measuredOutputSize(paths, prepared, components)
			if err != nil {
				t.Fatalf("measuredOutputSize() error = %v", err)
			}
			result, err := Merge(inputs)
			if err != nil {
				t.Fatalf("Merge() error = %v", err)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			if predicted != len(encoded) {
				t.Fatalf("predicted output = %d bytes, encoded = %d", predicted, len(encoded))
			}
		})
	}
}

func TestMergeRejectsTenThousandLeafLongURLAmplification(t *testing.T) {
	data := tenThousandLeafObject()
	inputs := make([]SourceResult, MaxSources)
	for index := range inputs {
		prefix := fmt.Sprintf("https://example.com/%d/", index)
		inputs[index] = SourceResult{
			URL:  prefix + strings.Repeat("u", MaxURLBytes-len(prefix)),
			Data: data,
		}
	}
	result, err := Merge(inputs)
	if !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("Merge() = (%#v, %v), want output ErrResourceLimit", result, err)
	}
	if !strings.Contains(err.Error(), "consensus output exceeds") {
		t.Fatalf("Merge() failed before output preflight: %v", err)
	}
	if result.Fields != nil {
		t.Fatalf("oversized output was partially constructed: %#v", result)
	}
}

func TestMergeRejectsInvalidAndOversizedInputs(t *testing.T) {
	valid := testSource("https://alpha.com/a", `{"x":1}`, 0)
	tooMany := make([]SourceResult, MaxSources+1)
	for index := range tooMany {
		tooMany[index] = testSource(fmt.Sprintf("https://source-%d.com/", index), `{"x":1}`, 0)
	}

	invalidUTF8 := append(json.RawMessage(`{"x":"`), 0xff)
	invalidUTF8 = append(invalidUTF8, []byte(`"}`)...)
	invalidCleanedUTF8 := string([]byte{'o', 'k', 0xff})
	deep := strings.Repeat(`[`, MaxJSONDepth+1) + `0` + strings.Repeat(`]`, MaxJSONDepth+1)
	tooManyLeaves := `[` + strings.Repeat(`0,`, MaxLeavesPerSource) + `0]`
	tooManyNodes := `[` + strings.Repeat(`{},`, MaxJSONNodesPerSource) + `{}]`
	longNumber := `1` + strings.Repeat(`0`, MaxNumberBytes)
	expandedNumbers := `[` + strings.Repeat(`1e4096,`, 1023) + `1e4096]`

	tests := []struct {
		name string
		in   []SourceResult
		want error
	}{
		{name: "empty", want: ErrInvalidInput},
		{name: "too many sources", in: tooMany, want: ErrResourceLimit},
		{name: "malformed", in: []SourceResult{testSource("https://alpha.com/a", `{`, 0)}, want: ErrInvalidInput},
		{name: "trailing", in: []SourceResult{testSource("https://alpha.com/a", `{} {}`, 0)}, want: ErrInvalidInput},
		{name: "duplicate key", in: []SourceResult{testSource("https://alpha.com/a", `{"x":1,"x":1}`, 0)}, want: ErrInvalidInput},
		{name: "invalid UTF-8", in: []SourceResult{{URL: valid.URL, Data: invalidUTF8}}, want: ErrInvalidInput},
		{name: "invalid cleaned UTF-8", in: []SourceResult{{URL: valid.URL, Data: valid.Data, CleanedText: invalidCleanedUTF8}}, want: ErrInvalidInput},
		{name: "oversized cleaned text", in: []SourceResult{{URL: valid.URL, Data: valid.Data, CleanedText: strings.Repeat("x", MaxCleanedTextBytes+1)}}, want: ErrResourceLimit},
		{name: "cleaned quote mismatch", in: []SourceResult{{URL: valid.URL, Data: valid.Data, CleanedText: "one", Basis: map[string]evidence.Anchor{"x": {Quote: "two", Method: evidence.MethodExact, TextRange: [2]int{0, 3}}}}}, want: ErrInvalidInput},
		{name: "cleaned range out of bounds", in: []SourceResult{{URL: valid.URL, Data: valid.Data, CleanedText: "one", Basis: map[string]evidence.Anchor{"x": {Quote: "one", Method: evidence.MethodExact, TextRange: [2]int{0, 4}}}}}, want: ErrInvalidInput},
		{name: "oversized data", in: []SourceResult{testSource("https://alpha.com/a", `"`+strings.Repeat("x", MaxSourceDataBytes)+`"`, 0)}, want: ErrResourceLimit},
		{name: "depth", in: []SourceResult{testSource("https://alpha.com/a", deep, 0)}, want: ErrResourceLimit},
		{name: "leaves", in: []SourceResult{testSource("https://alpha.com/a", tooManyLeaves, 0)}, want: ErrResourceLimit},
		{name: "nodes", in: []SourceResult{testSource("https://alpha.com/a", tooManyNodes, 0)}, want: ErrResourceLimit},
		{name: "number bytes", in: []SourceResult{testSource("https://alpha.com/a", longNumber, 0)}, want: ErrResourceLimit},
		{name: "number exponent", in: []SourceResult{testSource("https://alpha.com/a", `1e4097`, 0)}, want: ErrResourceLimit},
		{name: "canonical number aggregate", in: []SourceResult{testSource("https://alpha.com/a", expandedNumbers, 0)}, want: ErrResourceLimit},
		{name: "unknown evidence path", in: []SourceResult{{URL: valid.URL, Data: valid.Data, Basis: map[string]evidence.Anchor{"y": {}}}}, want: ErrInvalidInput},
		{name: "unknown receipt path", in: []SourceResult{{URL: valid.URL, Data: valid.Data, Receipts: map[string]string{"y": "token"}}}, want: ErrInvalidInput},
		{name: "path collision", in: []SourceResult{testSource("https://alpha.com/a", `{"a.b":1,"a\\":{"b":2}}`, 0)}, want: ErrInvalidInput},
		{name: "no registrable URL", in: []SourceResult{testSource("https://localhost/a", `{"x":1}`, 0)}, want: ErrInvalidInput},
		{name: "invalid anchor range", in: []SourceResult{{URL: valid.URL, Data: valid.Data, Basis: map[string]evidence.Anchor{"x": {TextRange: [2]int{2, 1}}}}}, want: ErrInvalidInput},
		{name: "oversized receipt", in: []SourceResult{{URL: valid.URL, Data: valid.Data, Receipts: map[string]string{"x": strings.Repeat("r", MaxReceiptBytes+1)}}}, want: ErrResourceLimit},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Merge(test.in)
			if !errors.Is(err, test.want) {
				t.Fatalf("Merge() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestMergeMissingCoreSignalPreservesV0AndNeverUsesPageHead(t *testing.T) {
	unlocated := func(url, cleaned string, simText uint64) SourceResult {
		return SourceResult{
			URL:         url,
			Data:        json.RawMessage(`{"x":1}`),
			SimText:     simText,
			CleanedText: cleaned,
			Basis: map[string]evidence.Anchor{"x": {
				Method:    evidence.MethodUnlocated,
				TextRange: [2]int{},
			}},
		}
	}

	t.Run("zero eligible anchors do not fingerprint shared page head", func(t *testing.T) {
		sharedHead := numberedWords("shared-head", 180)
		result, err := Merge([]SourceResult{
			unlocated("https://alpha.com/a", sharedHead+numberedWords("alpha-tail", 180), 0),
			unlocated("https://bravo.net/b", sharedHead+numberedWords("bravo-tail", 180), 0),
		})
		if err != nil {
			t.Fatalf("Merge() error = %v", err)
		}
		if result.Fields["x"].Agreement != (Agreement{Pages: 2, IndependentRoots: 2}) {
			t.Fatalf("agreement = %#v, ineligible anchors created a page-head core", result.Fields["x"].Agreement)
		}
	})

	t.Run("zero eligible anchors retain same-root edge", func(t *testing.T) {
		result, err := Merge([]SourceResult{
			unlocated("https://one.alpha.com/a", "first page", 0),
			unlocated("https://two.alpha.com/b", "second page", 0),
		})
		if err != nil {
			t.Fatalf("Merge() error = %v", err)
		}
		if result.Fields["x"].Agreement != (Agreement{Pages: 2, IndependentRoots: 1}) {
			t.Fatalf("agreement = %#v, same-root edge was lost", result.Fields["x"].Agreement)
		}
	})

	t.Run("invalid short cores retain legacy simhash edge", func(t *testing.T) {
		first := anchoredCleanedSource("https://alpha.com/a", "short one context", "one")
		second := anchoredCleanedSource("https://bravo.net/b", "different one text", "one")
		first.Data, second.Data = json.RawMessage(`{"name":"one"}`), json.RawMessage(`{"name":"one"}`)
		first.SimText, second.SimText = 0x1234, 0x1234
		result, err := Merge([]SourceResult{first, second})
		if err != nil {
			t.Fatalf("Merge() error = %v", err)
		}
		if result.Fields["name"].Agreement != (Agreement{Pages: 2, IndependentRoots: 1}) {
			t.Fatalf("agreement = %#v, legacy edge was lost", result.Fields["name"].Agreement)
		}
	})
}

func TestSourceResultCleanedTextIsNonWire(t *testing.T) {
	encoded, err := json.Marshal(SourceResult{
		URL:         "https://alpha.com/a",
		Data:        json.RawMessage(`{"x":1}`),
		CleanedText: "private cleaned content",
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if bytes.Contains(encoded, []byte("CleanedText")) || bytes.Contains(encoded, []byte("private cleaned content")) {
		t.Fatalf("CleanedText leaked onto wire: %s", encoded)
	}
}

func TestMergeCopiesCallerOwnedData(t *testing.T) {
	input := SourceResult{
		URL:      "https://alpha.com/a",
		Data:     json.RawMessage(`{"x":"original"}`),
		Basis:    map[string]evidence.Anchor{"x": {Quote: "original", Method: evidence.MethodExact}},
		Receipts: map[string]string{"x": "receipt"},
	}
	result, err := Merge([]SourceResult{input})
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	input.Data[0] = '['
	anchor := input.Basis["x"]
	anchor.Quote = "mutated"
	input.Basis["x"] = anchor
	input.Receipts["x"] = "mutated"
	field := result.Fields["x"]
	if string(field.Value) != `"original"` || field.Supports[0].Evidence.Quote != "original" || field.Supports[0].Receipt != "receipt" {
		t.Fatalf("result aliases caller data: %#v", field)
	}
}

func testSource(url, data string, simText uint64) SourceResult {
	return SourceResult{URL: url, Data: json.RawMessage(data), SimText: simText}
}

func anchoredCleanedSource(url, cleaned, quote string) SourceResult {
	start := strings.Index(cleaned, quote)
	if start < 0 {
		panic("quote is absent from cleaned fixture")
	}
	return SourceResult{
		URL:         url,
		Data:        json.RawMessage(`{"name":"` + quote + `"}`),
		Basis:       map[string]evidence.Anchor{"name": {Quote: quote, Method: evidence.MethodExact, TextRange: [2]int{start, start + len(quote)}}},
		SimText:     simhash.Fingerprint(cleaned),
		CleanedText: cleaned,
	}
}

func numberedWords(prefix string, count int) string {
	var builder strings.Builder
	for index := range count {
		fmt.Fprintf(&builder, "%s-%04d ", prefix, index)
	}
	return builder.String()
}

func hanSequence(offset, count int) string {
	var builder strings.Builder
	for index := range count {
		builder.WriteRune(rune(0x4e00 + (offset+index)%2_000))
	}
	return builder.String()
}

func provenanceSource(url, data string, simText uint64, marker string, fetchedAt time.Time) SourceResult {
	values, err := evidence.LeafValues(json.RawMessage(data))
	if err != nil {
		panic(err)
	}
	basis := make(map[string]evidence.Anchor, len(values))
	receipts := make(map[string]string, len(values))
	for path := range values {
		basis[path] = evidence.Anchor{Quote: marker + ":" + path, Method: evidence.MethodExact, SnapshotID: marker, FetchedAt: fetchedAt}
		receipts[path] = "receipt:" + marker + ":" + path
	}
	return SourceResult{URL: url, Data: json.RawMessage(data), Basis: basis, Receipts: receipts, SimText: simText}
}

func prepareSourcesForTest(t *testing.T, inputs []SourceResult) []preparedSource {
	t.Helper()
	prepared := make([]preparedSource, 0, len(inputs))
	for index, input := range inputs {
		source, _, _, err := prepareSource(index, input)
		if err != nil {
			t.Fatalf("prepareSource(%d) error = %v", index, err)
		}
		prepared = append(prepared, source)
	}
	sort.Slice(prepared, func(i, j int) bool { return prepared[i].url < prepared[j].url })
	return prepared
}

func tenThousandLeafObject() json.RawMessage {
	var builder strings.Builder
	builder.Grow(MaxLeavesPerSource * 11)
	builder.WriteByte('{')
	for index := 0; index < MaxLeavesPerSource; index++ {
		if index > 0 {
			builder.WriteByte(',')
		}
		fmt.Fprintf(&builder, `"f%05d":0`, index)
	}
	builder.WriteByte('}')
	return json.RawMessage(builder.String())
}

func supportURLs(supports []Support) []string {
	urls := make([]string, len(supports))
	for index, support := range supports {
		urls[index] = support.URL
	}
	return urls
}

func permutations(values []SourceResult, visit func([]SourceResult)) {
	working := append([]SourceResult(nil), values...)
	var generate func(int)
	generate = func(index int) {
		if index == len(working) {
			visit(append([]SourceResult(nil), working...))
			return
		}
		for candidate := index; candidate < len(working); candidate++ {
			working[index], working[candidate] = working[candidate], working[index]
			generate(index + 1)
			working[index], working[candidate] = working[candidate], working[index]
		}
	}
	generate(0)
}

func FuzzMergeNeverPanics(f *testing.F) {
	f.Add([]byte(`{"x":1}`))
	f.Add([]byte(`{"a":[true,null,"x"]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		if !utf8.Valid(data) || len(data) > MaxSourceDataBytes {
			return
		}
		result, err := Merge([]SourceResult{{URL: "https://example.com/", Data: append(json.RawMessage(nil), data...)}})
		if err != nil {
			return
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("Marshal(successful Merge) error = %v", err)
		}
		if len(encoded) > MaxOutputBytes {
			t.Fatalf("successful Merge encoded %d bytes, maximum is %d", len(encoded), MaxOutputBytes)
		}
	})
}
