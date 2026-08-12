package rerankeval

import (
	"math"
	"strings"
	"testing"
)

func TestEvaluateRejectsEveryLockedGate(t *testing.T) {
	files := validTestCorpus(t)
	docsPath, labelsPath, recordingsPath := writeTestCorpus(t, files)
	baseline, err := LoadCorpus(docsPath, labelsPath, recordingsPath, LoadOptions{ExpectedManifestID: testManifestID})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		mutate func(*Corpus)
	}{
		{name: "minimum cases", mutate: func(corpus *Corpus) { corpus.Cases = corpus.Cases[:MinimumCases-1] }},
		{name: "minimum candidates", mutate: func(corpus *Corpus) { corpus.Cases[0].Candidates = corpus.Cases[0].Candidates[:9] }},
		{name: "language coverage", mutate: func(corpus *Corpus) {
			for i := range corpus.Cases {
				corpus.Cases[i].Language = LanguageEnglish
			}
		}},
		{name: "bucket coverage", mutate: func(corpus *Corpus) {
			for i := range corpus.Cases {
				corpus.Cases[i].Bucket = BucketLexical
			}
		}},
		{name: "hard bucket", mutate: func(corpus *Corpus) {
			for i := range corpus.Cases {
				if corpus.Cases[i].Bucket == BucketLexical {
					corpus.Cases[i].Hard = false
				}
			}
		}},
		{name: "objective hard mismatch", mutate: func(corpus *Corpus) { corpus.Cases[0].Hard = false }},
		{name: "manifest drift", mutate: func(corpus *Corpus) { corpus.Cases[0].ManifestID = strings.Repeat("a", 64) }},
		{name: "candidate id URL mismatch", mutate: func(corpus *Corpus) { corpus.Cases[0].Candidates[0].ID = strings.Repeat("f", 64) }},
		{name: "macro delta", mutate: func(corpus *Corpus) {
			for i := range corpus.Cases {
				flattenScores(&corpus.Cases[i])
			}
		}},
		{name: "language delta", mutate: func(corpus *Corpus) {
			for i := range corpus.Cases {
				if corpus.Cases[i].Language == LanguageEnglish {
					reverseScores(&corpus.Cases[i])
				}
			}
		}},
		{name: "hard bucket delta", mutate: func(corpus *Corpus) {
			for i := range corpus.Cases {
				if corpus.Cases[i].Bucket == BucketLexical {
					reverseScores(&corpus.Cases[i])
				}
			}
		}},
		{name: "hard aggregate", mutate: func(corpus *Corpus) {
			for i := range corpus.Cases {
				flattenScores(&corpus.Cases[i])
			}
		}},
		{name: "non regression", mutate: func(corpus *Corpus) {
			for i := 0; i < 5; i++ {
				reverseScores(&corpus.Cases[i])
			}
		}},
		{name: "single drop", mutate: func(corpus *Corpus) { reverseScores(&corpus.Cases[0]) }},
		{name: "nan", mutate: func(corpus *Corpus) { corpus.Cases[0].Candidates[0].RelevanceScore = math.NaN() }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			corpus := cloneCorpus(baseline)
			test.mutate(&corpus)
			if _, err := Evaluate(corpus); err == nil {
				t.Fatal("Evaluate() succeeded")
			}
		})
	}
}

func TestEvaluateUsesProductTieBreakAndDoesNotMutate(t *testing.T) {
	files := validTestCorpus(t)
	docsPath, labelsPath, recordingsPath := writeTestCorpus(t, files)
	corpus, err := LoadCorpus(docsPath, labelsPath, recordingsPath, LoadOptions{ExpectedManifestID: testManifestID})
	if err != nil {
		t.Fatal(err)
	}
	before := cloneCorpus(corpus)
	for caseIndex := range corpus.Cases {
		for candidateIndex := range corpus.Cases[caseIndex].Candidates {
			corpus.Cases[caseIndex].Candidates[candidateIndex].RelevanceScore = 0.5
		}
	}
	scorecard, err := EvaluateMetrics(corpus)
	if err != nil {
		t.Fatalf("EvaluateMetrics() error = %v", err)
	}
	for _, metric := range scorecard.Cases {
		if metric.DeltaNDCG5 != 0 {
			t.Fatalf("case %q tied scores delta = %.17g, want 0", metric.CaseID, metric.DeltaNDCG5)
		}
	}
	for caseIndex := range corpus.Cases {
		for candidateIndex := range corpus.Cases[caseIndex].Candidates {
			before.Cases[caseIndex].Candidates[candidateIndex].RelevanceScore = 0.5
		}
	}
	if !corporaEqual(corpus, before) {
		t.Fatal("EvaluateMetrics() mutated input")
	}
}

func flattenScores(goldenCase *Case) {
	for index := range goldenCase.Candidates {
		goldenCase.Candidates[index].RelevanceScore = float64(20-index) / 20
	}
}

func reverseScores(goldenCase *Case) {
	for index := range goldenCase.Candidates {
		goldenCase.Candidates[index].RelevanceScore = 1 - float64(goldenCase.Candidates[index].Grade)/3
	}
}
