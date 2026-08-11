package consensus

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestMergeExportsEachFoldReason(t *testing.T) {
	tests := []struct {
		name   string
		inputs []SourceResult
		reason FoldReason
	}{
		{
			name: "same root",
			inputs: []SourceResult{
				testSource("https://one.alpha.com/a", `{"claim":"same"}`, 0),
				testSource("https://two.alpha.com/b", `{"claim":"same"}`, 0),
			},
			reason: FoldReasonSameRoot,
		},
		{
			name: "legacy near duplicate",
			inputs: []SourceResult{
				testSource("https://alpha.com/a", `{"claim":"same"}`, 0x1234),
				testSource("https://bravo.net/b", `{"claim":"same"}`, 0x1234),
			},
			reason: FoldReasonNearDuplicate,
		},
	}
	short, medium, _ := lineageMergeNestedFragments()
	lineageData := map[string]string{"claim": "Ada"}
	lineageFirst := lineageMergeSource(t, "https://alpha.com/a", "claim", "Ada", lineageData, short, lineageMergeWords("aleft", 70), lineageMergeWords("aright", 70))
	lineageSecond := lineageMergeSource(t, "https://bravo.net/b", "claim", "Ada", lineageData, medium, lineageMergeWords("bleft", 70), lineageMergeWords("bright", 70))
	lineageMergeAssertIsolatedLineagePair(t, lineageFirst, lineageSecond)
	tests = append(tests, struct {
		name   string
		inputs []SourceResult
		reason FoldReason
	}{name: "quote lineage", inputs: []SourceResult{lineageFirst, lineageSecond}, reason: FoldReasonQuoteLineage})

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := Merge(test.inputs)
			if err != nil {
				t.Fatalf("Merge() error = %v", err)
			}
			field := result.Fields["claim"]
			if field.Agreement != (Agreement{Pages: 2, IndependentRoots: 1, FoldReason: test.reason}) {
				t.Fatalf("agreement = %#v", field.Agreement)
			}
			if len(field.Supports) != 2 || field.Supports[0].FoldReason != "" || field.Supports[1].FoldReason != test.reason {
				t.Fatalf("supports = %#v, want lexical representative then %q", field.Supports, test.reason)
			}
		})
	}
}

func TestSourcePairFoldReasonUsesStablePriority(t *testing.T) {
	value, err := canonicalScalar([]byte(`"Ada"`))
	if err != nil {
		t.Fatalf("canonicalScalar() error = %v", err)
	}
	fragment := strings.Repeat("lineage", 16)
	base := preparedSource{
		root:    "alpha.com",
		simText: 0x1234,
		fields:  map[string]scalarValue{"claim": value},
		lineage: lineageIndex{"claim": []string{fragment}},
		core:    coreDescriptor{fingerprint: 7, retainedShingles: 24, normalizedAlnumRunes: 128, valid: true},
	}
	allSignals := base
	if reason := sourcePairFoldReason(base, allSignals); reason != FoldReasonSameRoot {
		t.Fatalf("all-signal reason = %q, want %q", reason, FoldReasonSameRoot)
	}

	differentRoot := base
	differentRoot.root = "bravo.net"
	if reason := sourcePairFoldReason(base, differentRoot); reason != FoldReasonQuoteLineage {
		t.Fatalf("lineage+duplicate reason = %q, want %q", reason, FoldReasonQuoteLineage)
	}

	differentRoot.lineage = nil
	if reason := sourcePairFoldReason(base, differentRoot); reason != FoldReasonNearDuplicate {
		t.Fatalf("duplicate-only reason = %q, want %q", reason, FoldReasonNearDuplicate)
	}
}

func TestBuildIndependencePlanKruskalCycleOrder(t *testing.T) {
	value, err := canonicalScalar([]byte(`"Ada"`))
	if err != nil {
		t.Fatalf("canonicalScalar() error = %v", err)
	}
	fields := map[string]scalarValue{"claim": value}
	fragment := strings.Repeat("lineage", 16)

	t.Run("reason priority rejects the lower-priority cycle edge", func(t *testing.T) {
		sources := []preparedSource{
			{url: "https://a.alpha.com/report", root: "alpha.com", simText: 0x1234, fields: fields},
			{url: "https://b.alpha.com/report", root: "alpha.com", fields: fields, lineage: lineageIndex{"claim": []string{fragment}}},
			{url: "https://charlie.org/report", root: "charlie.org", simText: 0x1234, fields: fields, lineage: lineageIndex{"claim": []string{fragment}}},
		}
		plan := buildIndependencePlan(sources)
		wantForest := []independenceEdge{
			{first: 0, second: 1, reason: FoldReasonSameRoot},
			{first: 1, second: 2, reason: FoldReasonQuoteLineage},
		}
		if !foldForestsEqual(plan.forest, wantForest) ||
			plan.parent[0] != -1 || plan.parent[1] != 0 || plan.parent[2] != 1 ||
			plan.parentReason[1] != FoldReasonSameRoot || plan.parentReason[2] != FoldReasonQuoteLineage {
			t.Fatalf("priority forest/parents/reasons = %#v / %#v / %#v", plan.forest, plan.parent, plan.parentReason)
		}
	})

	t.Run("canonical endpoint order breaks equal-reason cycle ties", func(t *testing.T) {
		sources := []preparedSource{
			{url: "https://alpha.com/report", root: "alpha.com", simText: 0x1234},
			{url: "https://bravo.net/report", root: "bravo.net", simText: 0x1234},
			{url: "https://charlie.org/report", root: "charlie.org", simText: 0x1234},
		}
		plan := buildIndependencePlan(sources)
		wantForest := []independenceEdge{
			{first: 0, second: 1, reason: FoldReasonNearDuplicate},
			{first: 0, second: 2, reason: FoldReasonNearDuplicate},
		}
		if !foldForestsEqual(plan.forest, wantForest) ||
			plan.parent[0] != -1 || plan.parent[1] != 0 || plan.parent[2] != 0 {
			t.Fatalf("tie-break forest/parents = %#v / %#v", plan.forest, plan.parent)
		}
	})
}

func TestMergeFoldReasonPriorityAndMixedForestAreDeterministic(t *testing.T) {
	short, medium, _ := lineageMergeNestedFragments()
	data := map[string]string{"claim": "Ada"}
	first := lineageMergeSource(t, "https://one.alpha.com/report", "claim", "Ada", data,
		lineageMergeWords("unrelated", 18)+" Ada "+lineageMergeWords("source", 18),
		lineageMergeWords("firstleft", 70), lineageMergeWords("firstright", 70))
	secondFragment := strings.Replace(short, "shared000", "changed000", 1)
	second := lineageMergeSource(t, "https://two.alpha.com/report", "claim", "Ada", data,
		secondFragment, lineageMergeWords("coreleft", 70), lineageMergeWords("coreright", 70))
	third := lineageMergeSource(t, "https://bravo.net/report", "claim", "Ada", data,
		short, lineageMergeWords("coreleft", 70), lineageMergeWords("coreright", 70))
	fourth := lineageMergeSource(t, "https://charlie.org/report", "claim", "Ada", data,
		medium, lineageMergeWords("fourthleft", 70), lineageMergeWords("fourthright", 70))
	for _, source := range []*SourceResult{&first, &second, &third, &fourth} {
		source.SimText = 0
	}
	prepared := prepareSourcesForTest(t, []SourceResult{first, second, third, fourth})
	independence := buildIndependencePlan(prepared)
	if len(independence.forest) != len(prepared)-1 {
		t.Fatalf("forest edges = %d, want %d", len(independence.forest), len(prepared)-1)
	}
	for child, parent := range independence.parent {
		if parent < 0 {
			continue
		}
		reason := independence.parentReason[child]
		if reason == "" || sourcePairFoldReason(prepared[child], prepared[parent]) != reason ||
			!forestContainsFoldEdge(independence.forest, child, parent, reason) {
			t.Fatalf("parent edge %d-%d reason %q cannot be replayed from %#v", child, parent, reason, independence.forest)
		}
	}

	var want []byte
	lineageMergeForEachPermutation([]SourceResult{first, second, third, fourth}, func(candidate []SourceResult) {
		result, err := Merge(candidate)
		if err != nil {
			t.Fatalf("Merge(permutation) error = %v", err)
		}
		field := result.Fields["claim"]
		if field.Agreement.Pages != 4 || field.Agreement.IndependentRoots != 1 || field.Agreement.FoldReason != "" {
			t.Fatalf("mixed agreement = %#v", field.Agreement)
		}
		gotReasons := map[string]FoldReason{}
		for _, support := range field.Supports {
			gotReasons[support.URL] = support.FoldReason
		}
		wantReasons := map[string]FoldReason{
			"https://bravo.net/report":     "",
			"https://charlie.org/report":   FoldReasonQuoteLineage,
			"https://one.alpha.com/report": FoldReasonSameRoot,
			"https://two.alpha.com/report": FoldReasonNearDuplicate,
		}
		if !foldReasonMapsEqual(gotReasons, wantReasons) {
			t.Fatalf("support reasons = %#v, want %#v", gotReasons, wantReasons)
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}
		if want == nil {
			want = append([]byte(nil), encoded...)
		} else if !bytes.Equal(encoded, want) {
			t.Fatalf("permutation changed fold forest:\n got %s\nwant %s", encoded, want)
		}
	})
}

func TestMergeAgreementFoldReasonIncludesGroupExternalBridge(t *testing.T) {
	inputs := []SourceResult{
		testSource("https://a.alpha.com/a", `{"claim":"group"}`, 0),
		testSource("https://b.alpha.com/b", `{"claim":"bridge"}`, 0x1234),
		testSource("https://bravo.net/c", `{"claim":"group"}`, 0x1234),
	}
	result, err := Merge(inputs)
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	field := result.Fields["claim"]
	if field.Agreement.Pages != 2 || field.Agreement.IndependentRoots != 1 || field.Agreement.FoldReason != "" {
		t.Fatalf("bridge agreement = %#v, want mixed omission", field.Agreement)
	}
	if len(field.Supports) != 2 || field.Supports[0].FoldReason != "" || field.Supports[1].FoldReason != FoldReasonNearDuplicate {
		t.Fatalf("bridge supports = %#v", field.Supports)
	}
}

func TestMergeSupportReasonIsSourceGlobalWhenValueGroupDoesNotFold(t *testing.T) {
	result, err := Merge([]SourceResult{
		testSource("https://a.alpha.com/a", `{"claim":"other"}`, 0),
		testSource("https://b.alpha.com/b", `{"claim":"solo"}`, 0),
	})
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	var solo Conflict
	for _, conflict := range result.Fields["claim"].Conflicts {
		if string(conflict.Value) == `"solo"` {
			solo = conflict
		}
	}
	if solo.Agreement != (Agreement{Pages: 1, IndependentRoots: 1}) || len(solo.Supports) != 1 || solo.Supports[0].FoldReason != FoldReasonSameRoot {
		t.Fatalf("solo group = %#v", solo)
	}
}

func TestMergeAgreementSummarizesHomogeneousFoldsAcrossComponents(t *testing.T) {
	result, err := Merge([]SourceResult{
		testSource("https://a.alpha.com/a", `{"claim":"same"}`, 0),
		testSource("https://b.alpha.com/b", `{"claim":"same"}`, 0),
		testSource("https://a.bravo.net/a", `{"claim":"same"}`, 0),
		testSource("https://b.bravo.net/b", `{"claim":"same"}`, 0),
	})
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	agreement := result.Fields["claim"].Agreement
	if agreement != (Agreement{Pages: 4, IndependentRoots: 2, FoldReason: FoldReasonSameRoot}) {
		t.Fatalf("agreement = %#v", agreement)
	}
}

func TestMergeAmbiguousConflictsExportFoldReasons(t *testing.T) {
	result, err := Merge([]SourceResult{
		testSource("https://one.alpha.com/a", `{"claim":"a"}`, 0),
		testSource("https://two.alpha.com/b", `{"claim":"a"}`, 0),
		testSource("https://bravo.net/c", `{"claim":"b"}`, 0x1234),
		testSource("https://charlie.org/d", `{"claim":"b"}`, 0x1234),
	})
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	field := result.Fields["claim"]
	if !field.Ambiguous || field.Agreement != (Agreement{}) || len(field.Supports) != 0 || len(field.Conflicts) != 2 {
		t.Fatalf("ambiguous field = %#v", field)
	}
	want := []struct {
		value  string
		reason FoldReason
	}{
		{value: `"a"`, reason: FoldReasonSameRoot},
		{value: `"b"`, reason: FoldReasonNearDuplicate},
	}
	for index, expected := range want {
		conflict := field.Conflicts[index]
		if string(conflict.Value) != expected.value ||
			conflict.Agreement != (Agreement{Pages: 2, IndependentRoots: 1, FoldReason: expected.reason}) ||
			len(conflict.Supports) != 2 || conflict.Supports[0].FoldReason != "" ||
			conflict.Supports[1].FoldReason != expected.reason {
			t.Fatalf("conflict %d = %#v, want value=%s reason=%q", index, conflict, expected.value, expected.reason)
		}
	}
}

func TestMergeFoldReasonOmitEmptyAndBudgetMeasurementMatchesJSON(t *testing.T) {
	inputs := []SourceResult{
		testSource("https://alpha.com/a", `{"claim":"same"}`, 0),
		testSource("https://bravo.net/b", `{"claim":"same"}`, 0),
	}
	prepared := prepareSourcesForTest(t, inputs)
	plan := buildIndependencePlan(prepared)
	paths := collectPaths(prepared)
	predicted, err := measuredOutputSize(paths, prepared, plan)
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
		t.Fatalf("predicted bytes = %d, encoded = %d", predicted, len(encoded))
	}
	if bytes.Contains(encoded, []byte("fold_reason")) {
		t.Fatalf("unfolded response emitted fold_reason: %s", encoded)
	}
	const legacyJSON = `{"fields":{"claim":{"value":"same","agreement":{"pages":2,"independent_roots":2},"supports":[{"url":"https://alpha.com/a","root":"alpha.com"},{"url":"https://bravo.net/b","root":"bravo.net"}]}}}`
	if string(encoded) != legacyJSON {
		t.Fatalf("unfolded JSON changed:\n got %s\nwant %s", encoded, legacyJSON)
	}

	folded := prepareSourcesForTest(t, []SourceResult{
		testSource("https://one.alpha.com/a", `{"claim":"same"}`, 0),
		testSource("https://two.alpha.com/b", `{"claim":"same"}`, 0),
	})
	foldedPlan := buildIndependencePlan(folded)
	foldedPaths := collectPaths(folded)
	foldedPredicted, err := measuredOutputSize(foldedPaths, folded, foldedPlan)
	if err != nil {
		t.Fatalf("folded measuredOutputSize() error = %v", err)
	}
	foldedResult, err := Merge([]SourceResult{
		testSource("https://one.alpha.com/a", `{"claim":"same"}`, 0),
		testSource("https://two.alpha.com/b", `{"claim":"same"}`, 0),
	})
	if err != nil {
		t.Fatalf("folded Merge() error = %v", err)
	}
	foldedJSON, _ := json.Marshal(foldedResult)
	if foldedPredicted != len(foldedJSON) {
		t.Fatalf("folded predicted bytes = %d, encoded = %d", foldedPredicted, len(foldedJSON))
	}
}

func TestMergeFoldReasonCompleteOutputBudgetBoundary(t *testing.T) {
	const lastReceiptSlack = 128 << 10
	inputs := foldReasonBudgetInputs(MaxReceiptBytes - lastReceiptSlack)
	prepared := prepareSourcesForTest(t, inputs)
	plan := buildIndependencePlan(prepared)
	predicted, err := measuredOutputSize(collectPaths(prepared), prepared, plan)
	if err != nil {
		t.Fatalf("base measuredOutputSize() error = %v", err)
	}
	delta := MaxOutputBytes - predicted
	if delta <= 0 || delta > lastReceiptSlack {
		t.Fatalf("base output leaves %d bytes, want within (0,%d]", delta, lastReceiptSlack)
	}
	inputs[1].Receipts["f07"] += strings.Repeat("x", delta)

	exactPrepared := prepareSourcesForTest(t, inputs)
	exactPlan := buildIndependencePlan(exactPrepared)
	exactPredicted, err := measuredOutputSize(collectPaths(exactPrepared), exactPrepared, exactPlan)
	if err != nil || exactPredicted != MaxOutputBytes {
		t.Fatalf("exact measured bytes/error = %d/%v, want %d/nil", exactPredicted, err, MaxOutputBytes)
	}
	result, err := Merge(inputs)
	if err != nil {
		t.Fatalf("Merge(exact) error = %v", err)
	}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) != MaxOutputBytes {
		t.Fatalf("Marshal(exact) bytes/error = %d/%v, want %d/nil", len(encoded), err, MaxOutputBytes)
	}
	if result.Fields["f00"].Agreement.FoldReason != FoldReasonSameRoot ||
		len(result.Fields["f00"].Supports) != 2 ||
		result.Fields["f00"].Supports[1].FoldReason != FoldReasonSameRoot {
		t.Fatalf("exact folded field = %#v", result.Fields["f00"])
	}

	inputs[1].Receipts["f07"] += "x"
	if _, err := Merge(inputs); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("Merge(N+1) error = %v, want ErrResourceLimit", err)
	}
}

func foldReasonMapsEqual(first, second map[string]FoldReason) bool {
	if len(first) != len(second) {
		return false
	}
	for key, value := range first {
		if second[key] != value {
			return false
		}
	}
	return true
}

func foldForestsEqual(first, second []independenceEdge) bool {
	if len(first) != len(second) {
		return false
	}
	for index := range first {
		if first[index] != second[index] {
			return false
		}
	}
	return true
}

func forestContainsFoldEdge(forest []independenceEdge, first, second int, reason FoldReason) bool {
	for _, edge := range forest {
		if edge.reason == reason &&
			((edge.first == first && edge.second == second) || (edge.first == second && edge.second == first)) {
			return true
		}
	}
	return false
}

func foldReasonBudgetInputs(lastReceiptBytes int) []SourceResult {
	var data strings.Builder
	data.WriteByte('{')
	for field := 0; field < 8; field++ {
		if field > 0 {
			data.WriteByte(',')
		}
		fmt.Fprintf(&data, `"f%02d":true`, field)
	}
	data.WriteByte('}')
	fullReceipt := strings.Repeat("x", MaxReceiptBytes)
	receipts := [2]map[string]string{make(map[string]string, 8), make(map[string]string, 8)}
	for source := range receipts {
		for field := 0; field < 8; field++ {
			receipts[source][fmt.Sprintf("f%02d", field)] = fullReceipt
		}
	}
	receipts[1]["f07"] = strings.Repeat("x", lastReceiptBytes)
	raw := json.RawMessage(data.String())
	return []SourceResult{
		{URL: "https://one.alpha.com/report", Data: raw, Receipts: receipts[0]},
		{URL: "https://two.alpha.com/report", Data: raw, Receipts: receipts[1]},
	}
}
