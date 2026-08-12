package rerankeval

import (
	"errors"
	"math"
	"slices"
	"testing"
)

func TestPairedBootstrapLower95IsDeterministicAndOrderIndependent(t *testing.T) {
	deltas := make([]PairedDelta, MinimumSignificanceCases)
	for index := range deltas {
		deltas[index] = PairedDelta{CaseID: string(rune('z'-index%26)) + string(rune('A'+index/26)), DeltaNDCG5: 0.02 + float64(index%7)/100}
	}
	wantInput := slices.Clone(deltas)
	first, err := PairedBootstrapLower95(deltas)
	if err != nil {
		t.Fatalf("PairedBootstrapLower95() error = %v", err)
	}
	slices.Reverse(deltas)
	second, err := PairedBootstrapLower95(deltas)
	if err != nil {
		t.Fatalf("PairedBootstrapLower95(reverse) error = %v", err)
	}
	if first != second || first <= 0 {
		t.Fatalf("lower bounds = %.17g and %.17g", first, second)
	}
	slices.Reverse(deltas)
	if !slices.Equal(deltas, wantInput) {
		t.Fatal("bootstrap mutated input")
	}
}

func TestPairedBootstrapLower95MatchesLockedSplitMixSequence(t *testing.T) {
	deltas := make([]PairedDelta, MinimumSignificanceCases)
	for index := range deltas {
		deltas[index] = PairedDelta{CaseID: twoByteID(index), DeltaNDCG5: float64(index-25) / 100}
	}
	got, err := PairedBootstrapLower95(deltas)
	if err != nil {
		t.Fatal(err)
	}
	const want = -0.045
	if math.Abs(got-want) > 1e-15 {
		t.Fatalf("lower bound = %.17g, want %.17g", got, want)
	}
	if lower, err := RequirePositivePairedBootstrap(deltas); err == nil || lower != got {
		t.Fatalf("RequirePositivePairedBootstrap() = %.17g, %v", lower, err)
	}
}

func TestRequirePositivePairedBootstrapPassesPositiveLowerBound(t *testing.T) {
	deltas := make([]PairedDelta, MinimumSignificanceCases)
	for index := range deltas {
		deltas[index] = PairedDelta{CaseID: twoByteID(index), DeltaNDCG5: 0.01 + float64(index%3)/100}
	}
	if lower, err := RequirePositivePairedBootstrap(deltas); err != nil || lower <= 0 {
		t.Fatalf("RequirePositivePairedBootstrap() = %.17g, %v", lower, err)
	}
}

func TestRequireCorpusSignificanceDerivesOnlyValidatedMetrics(t *testing.T) {
	files := testCorpusWithCases(t, MinimumSignificanceCases)
	docsPath, labelsPath, recordingsPath := writeTestCorpus(t, files)
	corpus, err := LoadCorpus(docsPath, labelsPath, recordingsPath, LoadOptions{ExpectedManifestID: testManifestID})
	if err != nil {
		t.Fatal(err)
	}
	lower, err := RequireCorpusSignificance(corpus)
	if err != nil || lower <= 0 {
		t.Fatalf("RequireCorpusSignificance() = %.17g, %v", lower, err)
	}

	for index := range corpus.Cases {
		for candidate := range corpus.Cases[index].Candidates {
			corpus.Cases[index].Candidates[candidate].RelevanceScore = 1 - float64(corpus.Cases[index].Candidates[candidate].Grade)/3
		}
	}
	if lower, err := RequireCorpusSignificance(corpus); err == nil || lower > 0 {
		t.Fatalf("RequireCorpusSignificance(adverse) = %.17g, %v", lower, err)
	}
}

func TestRequireCorpusSignificanceCannotBypassConstructionDeltaGate(t *testing.T) {
	files := testCorpusWithCases(t, MinimumSignificanceCases)
	docsPath, labelsPath, recordingsPath := writeTestCorpus(t, files)
	corpus, err := LoadCorpus(docsPath, labelsPath, recordingsPath, LoadOptions{ExpectedManifestID: testManifestID})
	if err != nil {
		t.Fatal(err)
	}
	// This ordering improves every case by about 0.0265, so its paired
	// bootstrap lower bound is positive while the locked 0.05 construction
	// macro gate still fails. The release entrypoint must enforce both gates.
	const order = "0123745986"
	for caseIndex := range corpus.Cases {
		for scoreRank, rawIndex := range []byte(order) {
			candidateIndex := int(rawIndex - '0')
			corpus.Cases[caseIndex].Candidates[candidateIndex].RelevanceScore = float64(len(order)-scoreRank) / float64(len(order))
		}
	}
	metrics, err := EvaluateMetrics(corpus)
	if err != nil || metrics.MacroDeltaNDCG5 <= 0 || metrics.MacroDeltaNDCG5 >= MinimumMacroDelta {
		t.Fatalf("fixture macro delta = %.17g, %v", metrics.MacroDeltaNDCG5, err)
	}
	deltas := make([]PairedDelta, len(metrics.Cases))
	for index, metric := range metrics.Cases {
		deltas[index] = PairedDelta{CaseID: metric.CaseID, DeltaNDCG5: metric.DeltaNDCG5}
	}
	if lower, err := RequirePositivePairedBootstrap(deltas); err != nil || lower <= 0 {
		t.Fatalf("bootstrap control = %.17g, %v", lower, err)
	}
	if lower, err := RequireCorpusSignificance(corpus); !errors.Is(err, ErrGateFailed) || lower != 0 {
		t.Fatalf("RequireCorpusSignificance() = %.17g, %v; want construction gate", lower, err)
	}
}

func TestPairedBootstrapLower95RejectsInvalidInput(t *testing.T) {
	valid := make([]PairedDelta, MinimumSignificanceCases)
	for index := range valid {
		valid[index] = PairedDelta{CaseID: twoByteID(index), DeltaNDCG5: 0.1}
	}
	boundaries := slices.Clone(valid)
	boundaries[0].DeltaNDCG5 = -1
	boundaries[1].DeltaNDCG5 = 1
	if _, err := PairedBootstrapLower95(boundaries); err != nil {
		t.Fatalf("PairedBootstrapLower95() rejected valid delta boundaries: %v", err)
	}
	tests := []struct {
		name   string
		mutate func([]PairedDelta) []PairedDelta
	}{
		{name: "too few", mutate: func(values []PairedDelta) []PairedDelta { return values[:MinimumSignificanceCases-1] }},
		{name: "empty id", mutate: func(values []PairedDelta) []PairedDelta { values[0].CaseID = ""; return values }},
		{name: "duplicate id", mutate: func(values []PairedDelta) []PairedDelta { values[1].CaseID = values[0].CaseID; return values }},
		{name: "nan", mutate: func(values []PairedDelta) []PairedDelta { values[0].DeltaNDCG5 = math.NaN(); return values }},
		{name: "infinity", mutate: func(values []PairedDelta) []PairedDelta { values[0].DeltaNDCG5 = math.Inf(1); return values }},
		{name: "below N plus 1", mutate: func(values []PairedDelta) []PairedDelta {
			values[0].DeltaNDCG5 = math.Nextafter(-1, math.Inf(-1))
			return values
		}},
		{name: "above N plus 1", mutate: func(values []PairedDelta) []PairedDelta {
			values[0].DeltaNDCG5 = math.Nextafter(1, math.Inf(1))
			return values
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := slices.Clone(valid)
			if _, err := PairedBootstrapLower95(test.mutate(values)); err == nil {
				t.Fatal("PairedBootstrapLower95() succeeded")
			}
		})
	}
}

func twoByteID(index int) string {
	return string([]byte{byte('a' + index/26), byte('a' + index%26)})
}
