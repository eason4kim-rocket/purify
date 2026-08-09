package consensus

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMergeWithMaterializationPreservesFieldConsensusContract(t *testing.T) {
	inputs := []SourceResult{
		testSource("https://alpha.com/a", `{"profile":{"name":"Ada"},"active":true}`, 0),
		testSource("https://bravo.net/b", `{"active":true,"profile":{"name":"Ada"}}`, 0),
	}

	wantResult, err := Merge(inputs)
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	gotResult, materialization, err := MergeWithMaterialization(inputs)
	if err != nil {
		t.Fatalf("MergeWithMaterialization() error = %v", err)
	}
	wantJSON, err := json.Marshal(wantResult)
	if err != nil {
		t.Fatalf("Marshal(Merge result) error = %v", err)
	}
	gotJSON, err := json.Marshal(gotResult)
	if err != nil {
		t.Fatalf("Marshal(new result) error = %v", err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("field result changed:\n got %s\nwant %s", gotJSON, wantJSON)
	}
	if strings.Contains(string(gotJSON), "material") || strings.Contains(string(gotJSON), `"status"`) || strings.Contains(string(gotJSON), `"data"`) {
		t.Fatalf("materialization leaked into Result JSON: %s", gotJSON)
	}
	if materialization.Status != MaterializationStatusComplete || string(materialization.Data) != `{"active":true,"profile":{"name":"Ada"}}` {
		t.Fatalf("materialization = %#v", materialization)
	}
}

func TestMergeWithMaterializationKeepsTypedNestedTopology(t *testing.T) {
	inputs := []SourceResult{
		testSource("https://alpha.com/a", `{"emptyObject":{},"a.b":{"c.d":[{},[],{"x\\y":1}]},"emptyArray":[]}`, 0),
		testSource("https://bravo.net/b", `{"a.b":{"c.d":[{},[],{"x\\y":1.0}]},"emptyArray":[],"emptyObject":{}}`, 0),
	}
	result, materialization, err := MergeWithMaterialization(inputs)
	if err != nil {
		t.Fatalf("MergeWithMaterialization() error = %v", err)
	}
	want := `{"a.b":{"c.d":[{},[],{"x\\y":1}]},"emptyArray":[],"emptyObject":{}}`
	if materialization.Status != MaterializationStatusComplete || string(materialization.Data) != want {
		t.Fatalf("materialization = (%q, %s), want complete %s", materialization.Status, materialization.Data, want)
	}
	if string(result.Fields[`a\.b.c\.d.2.x\y`].Value) != "1" {
		t.Fatalf("escaped-key field consensus was lost: %#v", result.Fields)
	}
}

func TestMergeWithMaterializationRootEmptyContainers(t *testing.T) {
	for _, document := range []string{`{}`, `[]`} {
		t.Run(document, func(t *testing.T) {
			result, materialization, err := MergeWithMaterialization([]SourceResult{
				testSource("https://alpha.com/a", document, 0),
			})
			if err != nil {
				t.Fatalf("MergeWithMaterialization() error = %v", err)
			}
			if result.Fields == nil || len(result.Fields) != 0 {
				t.Fatalf("empty document fields = %#v", result.Fields)
			}
			if materialization.Status != MaterializationStatusComplete || string(materialization.Data) != document {
				t.Fatalf("empty materialization = %#v", materialization)
			}
		})
	}
}

func TestMergeWithMaterializationObjectPresenceVotes(t *testing.T) {
	tests := []struct {
		name       string
		data       []string
		wantStatus MaterializationStatus
		wantData   string
		wantExtra  bool
	}{
		{
			name: "majority present",
			data: []string{
				`{"base":1,"extra":2}`,
				`{"base":1,"extra":2}`,
				`{"base":1}`,
			},
			wantStatus: MaterializationStatusComplete,
			wantData:   `{"base":1,"extra":2}`,
			wantExtra:  true,
		},
		{
			name: "minority extra remains only in consensus",
			data: []string{
				`{"base":1,"extra":2}`,
				`{"base":1}`,
				`{"base":1}`,
			},
			wantStatus: MaterializationStatusComplete,
			wantData:   `{"base":1}`,
			wantExtra:  true,
		},
		{
			name: "presence tie",
			data: []string{
				`{"base":1,"extra":2}`,
				`{"base":1}`,
			},
			wantStatus: MaterializationStatusAmbiguous,
			wantExtra:  true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inputs := materialSources(test.data)
			result, materialization, err := MergeWithMaterialization(inputs)
			if err != nil {
				t.Fatalf("MergeWithMaterialization() error = %v", err)
			}
			if materialization.Status != test.wantStatus || string(materialization.Data) != test.wantData {
				t.Fatalf("materialization = (%q, %s), want (%q, %s)", materialization.Status, materialization.Data, test.wantStatus, test.wantData)
			}
			_, hasExtra := result.Fields["extra"]
			if hasExtra != test.wantExtra {
				t.Fatalf("consensus extra presence = %v, want %v", hasExtra, test.wantExtra)
			}
			if test.wantStatus == MaterializationStatusAmbiguous && materialization.Data != nil {
				t.Fatalf("ambiguous materialization exposed partial data: %s", materialization.Data)
			}
		})
	}
}

func TestMergeWithMaterializationNodeKindVotes(t *testing.T) {
	t.Run("majority", func(t *testing.T) {
		result, materialization, err := MergeWithMaterialization(materialSources([]string{
			`{"node":{"value":"object"}}`,
			`{"node":{"value":"object"}}`,
			`{"node":["array"]}`,
		}))
		if err != nil {
			t.Fatalf("MergeWithMaterialization() error = %v", err)
		}
		if materialization.Status != MaterializationStatusComplete || string(materialization.Data) != `{"node":{"value":"object"}}` {
			t.Fatalf("materialization = %#v", materialization)
		}
		if _, exists := result.Fields["node.0"]; !exists {
			t.Fatalf("minority topology disappeared from field consensus: %#v", result.Fields)
		}
	})

	t.Run("tie", func(t *testing.T) {
		_, materialization, err := MergeWithMaterialization(materialSources([]string{
			`{"node":{"value":1}}`,
			`{"node":[1]}`,
		}))
		if err != nil {
			t.Fatalf("MergeWithMaterialization() error = %v", err)
		}
		if materialization.Status != MaterializationStatusAmbiguous || materialization.Data != nil {
			t.Fatalf("kind tie = %#v", materialization)
		}
	})

	t.Run("topology minority cannot alter scalar", func(t *testing.T) {
		_, materialization, err := MergeWithMaterialization(materialSources([]string{
			`{"node":7}`,
			`{"node":7}`,
			`{"node":{"other":8},"other":9}`,
		}))
		if err != nil {
			t.Fatalf("MergeWithMaterialization() error = %v", err)
		}
		if materialization.Status != MaterializationStatusComplete || string(materialization.Data) != `{"node":7}` {
			t.Fatalf("scalar materialization = %#v", materialization)
		}
	})
}

func TestMergeWithMaterializationUsesIndependentRootsBeforePages(t *testing.T) {
	inputs := []SourceResult{
		testSource("https://truth-one.com/a", `{"items":[true],"kind":{"v":1},"optional":"yes","scalar":"truth"}`, 0),
		testSource("https://truth-two.net/b", `{"items":[true],"kind":{"v":1},"optional":"yes","scalar":"truth"}`, 0),
	}
	for index, domain := range []string{"mirror-a.com", "mirror-b.net", "mirror-c.org", "mirror-d.io", "mirror-e.dev"} {
		inputs = append(inputs, testSource(
			fmt.Sprintf("https://%s/%d", domain, index),
			`{"items":[false,false],"kind":[1],"scalar":"mirror"}`,
			0x1234,
		))
	}

	_, materialization, err := MergeWithMaterialization(inputs)
	if err != nil {
		t.Fatalf("MergeWithMaterialization() error = %v", err)
	}
	want := `{"items":[true],"kind":{"v":1},"optional":"yes","scalar":"truth"}`
	if materialization.Status != MaterializationStatusComplete || string(materialization.Data) != want {
		t.Fatalf("two independent roots did not beat five mirrors: (%q, %s)", materialization.Status, materialization.Data)
	}
}

func TestMergeWithMaterializationPresenceOnlyPollsParentObjects(t *testing.T) {
	inputs := []SourceResult{
		testSource("https://alpha.com/a", `{"parent":{"base":1,"optional":2}}`, 0),
		testSource("https://bravo.net/b", `{"parent":{"base":1,"optional":2}}`, 0),
		testSource("https://charlie.org/c", `{"parent":{"base":1}}`, 0),
		testSource("https://delta.io/d", `{"parent":[]}`, 0),
		testSource("https://echo.dev/e", `{"parent":[]}`, 0),
	}
	_, materialization, err := MergeWithMaterialization(inputs)
	if err != nil {
		t.Fatalf("MergeWithMaterialization() error = %v", err)
	}
	if materialization.Status != MaterializationStatusComplete || string(materialization.Data) != `{"parent":{"base":1,"optional":2}}` {
		t.Fatalf("non-object parents were counted as key absence: %#v", materialization)
	}
}

func TestMergeWithMaterializationArrayLengthVotes(t *testing.T) {
	t.Run("majority", func(t *testing.T) {
		result, materialization, err := MergeWithMaterialization(materialSources([]string{
			`{"items":[1,2]}`,
			`{"items":[1,2]}`,
			`{"items":[1,2,3]}`,
		}))
		if err != nil {
			t.Fatalf("MergeWithMaterialization() error = %v", err)
		}
		if materialization.Status != MaterializationStatusComplete || string(materialization.Data) != `{"items":[1,2]}` {
			t.Fatalf("materialization = %#v", materialization)
		}
		if _, exists := result.Fields["items.2"]; !exists {
			t.Fatalf("minority array element disappeared from consensus: %#v", result.Fields)
		}
	})

	t.Run("tie", func(t *testing.T) {
		_, materialization, err := MergeWithMaterialization(materialSources([]string{
			`{"items":[1]}`,
			`{"items":[1,2]}`,
		}))
		if err != nil {
			t.Fatalf("MergeWithMaterialization() error = %v", err)
		}
		if materialization.Status != MaterializationStatusAmbiguous || materialization.Data != nil {
			t.Fatalf("array-length tie = %#v", materialization)
		}
	})

	t.Run("losing length cannot reverse element winner", func(t *testing.T) {
		inputs := []SourceResult{
			testSource("https://alpha.com/a", `{"items":[1]}`, 0),
			testSource("https://bravo.net/b", `{"items":[1]}`, 0),
			testSource("https://charlie.org/c", `{"items":[2]}`, 0),
			testSource("https://delta.io/d", `{"items":[2,0]}`, 0),
			testSource("https://echo.dev/e", `{"items":[2,0]}`, 0),
		}
		result, materialization, err := MergeWithMaterialization(inputs)
		if err != nil {
			t.Fatalf("MergeWithMaterialization() error = %v", err)
		}
		if string(result.Fields["items.0"].Value) != "2" {
			t.Fatalf("counterexample no longer exercises global field reversal: %#v", result.Fields["items.0"])
		}
		if materialization.Status != MaterializationStatusComplete || string(materialization.Data) != `{"items":[1]}` {
			t.Fatalf("losing length cohort influenced elements: %#v", materialization)
		}
	})
}

func TestMergeWithMaterializationScalarTieIsDocumentAmbiguity(t *testing.T) {
	result, materialization, err := MergeWithMaterialization(materialSources([]string{
		`{"stable":true,"value":"a"}`,
		`{"stable":true,"value":"b"}`,
	}))
	if err != nil {
		t.Fatalf("MergeWithMaterialization() error = %v", err)
	}
	if !result.Fields["value"].Ambiguous {
		t.Fatalf("field tie was not retained: %#v", result.Fields["value"])
	}
	if materialization.Status != MaterializationStatusAmbiguous || materialization.Data != nil {
		t.Fatalf("scalar tie = %#v", materialization)
	}
}

func TestMergeWithMaterializationIsStableAcrossPermutations(t *testing.T) {
	inputs := []SourceResult{
		testSource("https://delta.io/d", `{"base":1}`, 0),
		testSource("https://alpha.com/a", `{"base":1,"extra":{"x":2}}`, 0),
		testSource("https://charlie.org/c", `{"extra":{"x":2},"base":1}`, 0),
		testSource("https://bravo.net/b", `{"base":1,"extra":{"x":2}}`, 0),
	}
	var want []byte
	permutations(inputs, func(candidate []SourceResult) {
		result, materialization, err := MergeWithMaterialization(candidate)
		if err != nil {
			t.Fatalf("MergeWithMaterialization(permutation) error = %v", err)
		}
		encoded, err := json.Marshal(struct {
			Result          Result          `json:"result"`
			Materialization Materialization `json:"materialization"`
		}{Result: result, Materialization: materialization})
		if err != nil {
			t.Fatalf("Marshal(permutation) error = %v", err)
		}
		if want == nil {
			want = append([]byte(nil), encoded...)
			return
		}
		if string(encoded) != string(want) {
			t.Fatalf("order-dependent output:\n got %s\nwant %s", encoded, want)
		}
	})
}

func TestMergeWithMaterializationCopiesCallerData(t *testing.T) {
	input := SourceResult{URL: "https://alpha.com/a", Data: json.RawMessage(`{"nested":{"value":"original"}}`)}
	result, materialization, err := MergeWithMaterialization([]SourceResult{input})
	if err != nil {
		t.Fatalf("MergeWithMaterialization() error = %v", err)
	}
	input.Data[0] = '['
	result.Fields["nested.value"].Value[1] = 'X'
	if string(materialization.Data) != `{"nested":{"value":"original"}}` {
		t.Fatalf("materialization aliases input or Result: %s", materialization.Data)
	}
}

func TestMaterializedDocumentBudgetExactBoundary(t *testing.T) {
	exact := `"` + strings.Repeat("a", MaxMaterializedDataBytes-2) + `"`
	_, materialization, err := MergeWithMaterialization([]SourceResult{testSource("https://alpha.com/a", exact, 0)})
	if err != nil {
		t.Fatalf("N-byte MergeWithMaterialization() error = %v", err)
	}
	if materialization.Status != MaterializationStatusComplete || len(materialization.Data) != MaxMaterializedDataBytes {
		t.Fatalf("N-byte materialization = (%q, %d bytes)", materialization.Status, len(materialization.Data))
	}

	// 1e10 expands by seven bytes during exact-number canonicalization. This
	// document is six bytes below the source limit but exactly N+1 after merge.
	tooLarge := `[1e10,"` + strings.Repeat("a", MaxMaterializedDataBytes-15) + `"]`
	if len(tooLarge) != MaxMaterializedDataBytes-6 {
		t.Fatalf("oversized fixture input = %d bytes", len(tooLarge))
	}
	result, oversized, err := MergeWithMaterialization([]SourceResult{testSource("https://alpha.com/a", tooLarge, 0)})
	if !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("N+1 MergeWithMaterialization() = (%#v, %#v, %v), want ErrResourceLimit", result, oversized, err)
	}
	if oversized.Data != nil {
		t.Fatalf("N+1 error exposed partial data: %d bytes", len(oversized.Data))
	}

	legacy, err := Merge([]SourceResult{testSource("https://alpha.com/a", tooLarge, 0)})
	if err != nil {
		t.Fatalf("legacy Merge() inherited materialization bound: %v", err)
	}
	if len(legacy.Fields) != 2 {
		t.Fatalf("legacy field consensus = %#v", legacy.Fields)
	}
}

func TestMergeWithMaterializationAmbiguityPrecedesOutputBudget(t *testing.T) {
	const canonicalNumberBytes = 21 // 1e20 canonicalizes to 100000000000000000000.
	paddingBytes := MaxMaterializedDataBytes - canonicalNumberBytes - len(`""`) - len(`"left"`)
	document := func(tiedValue string) string {
		return `{"a":[1e20,"` + strings.Repeat("a", paddingBytes) + `"],"z":"` + tiedValue + `"}`
	}
	left := document("left")
	right := document("rght")
	if len(left) != MaxMaterializedDataBytes-3 || len(right) != len(left) {
		t.Fatalf("probe fixture sizes = (%d, %d), want %d", len(left), len(right), MaxMaterializedDataBytes-3)
	}

	result, materialization, err := MergeWithMaterialization([]SourceResult{
		testSource("https://alpha.com/a", left, 0),
		testSource("https://bravo.net/b", right, 0),
	})
	if err != nil {
		t.Fatalf("later tie was hidden by an earlier oversized branch: %v", err)
	}
	if !result.Fields["z"].Ambiguous {
		t.Fatalf("fixture did not retain its later field tie: %#v", result.Fields["z"])
	}
	if materialization.Status != MaterializationStatusAmbiguous || materialization.Data != nil {
		t.Fatalf("oversized tree with later tie = %#v, want ambiguous with nil data", materialization)
	}
}

func TestMergeWithMaterializationDuplicateConflict(t *testing.T) {
	duplicate := []SourceResult{
		testSource("https://example.com/a#one", `{"x":1}`, 0),
		testSource("https://example.com/a#two", `{"x":2}`, 0),
	}
	_, materialization, err := MergeWithMaterialization(duplicate)
	if !errors.Is(err, ErrDuplicateSourceConflict) {
		t.Fatalf("duplicate error = %v, want ErrDuplicateSourceConflict", err)
	}
	if materialization.Data != nil {
		t.Fatalf("invalid input exposed materialized data: %s", materialization.Data)
	}
}

func materialSources(data []string) []SourceResult {
	sources := make([]SourceResult, len(data))
	for index, document := range data {
		sources[index] = testSource(fmt.Sprintf("https://source-%d.example.com/data", index), document, 0)
	}
	return sources
}

func FuzzMergeWithMaterializationNeverPanics(f *testing.F) {
	f.Add([]byte(`{"x":1}`), []byte(`{"x":1.0}`))
	f.Add([]byte(`{"a.b":[{},[],true]}`), []byte(`{"a.b":{"0":false}}`))
	f.Add([]byte(`[]`), []byte(`{}`))
	f.Fuzz(func(t *testing.T, first, second []byte) {
		if !utf8.Valid(first) || !utf8.Valid(second) || len(first) > MaxSourceDataBytes || len(second) > MaxSourceDataBytes {
			return
		}
		result, materialization, err := MergeWithMaterialization([]SourceResult{
			{URL: "https://alpha.com/", Data: append(json.RawMessage(nil), first...)},
			{URL: "https://bravo.net/", Data: append(json.RawMessage(nil), second...)},
		})
		if err != nil {
			if !errors.Is(err, ErrInvalidInput) && !errors.Is(err, ErrResourceLimit) {
				t.Fatalf("unclassified error = %v", err)
			}
			if materialization.Data != nil {
				t.Fatalf("error exposed %d materialized bytes", len(materialization.Data))
			}
			return
		}
		if _, err := json.Marshal(result); err != nil {
			t.Fatalf("Marshal(Result) error = %v", err)
		}
		switch materialization.Status {
		case MaterializationStatusComplete:
			if !json.Valid(materialization.Data) || len(materialization.Data) > MaxMaterializedDataBytes {
				t.Fatalf("complete materialization is invalid or oversized: %d bytes", len(materialization.Data))
			}
		case MaterializationStatusAmbiguous:
			if materialization.Data != nil {
				t.Fatalf("ambiguous materialization exposed data: %s", materialization.Data)
			}
		default:
			t.Fatalf("unknown materialization status %q", materialization.Status)
		}
	})
}
