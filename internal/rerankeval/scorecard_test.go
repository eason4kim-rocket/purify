package rerankeval

import (
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/use-agent/purify/search/rerank/replay"
)

func TestScorecardRunLocksScheduleRawPercentilesAndLocalCost(t *testing.T) {
	corpus, input := scorecardTestRun(t)
	wantInput := scorecardCloneTestRun(input)

	report, err := EvaluateScorecardRun(corpus, input)
	if !errors.Is(err, ErrScorecardGateFailed) {
		t.Fatalf("EvaluateScorecardRun() error = %v, want ErrScorecardGateFailed", err)
	}
	if !reflect.DeepEqual(input, wantInput) {
		t.Fatal("EvaluateScorecardRun mutated its input")
	}
	wantCaseIDs := make([]string, len(corpus.Cases))
	for index, goldenCase := range corpus.Cases {
		wantCaseIDs[index] = goldenCase.ID
	}
	if !reflect.DeepEqual(report.CaseIDs, wantCaseIDs) {
		t.Fatalf("report case order = %#v, want byte-lexical order", report.CaseIDs)
	}
	if report.RecordedAt != input.RecordedAt || report.PriceSnapshot.Date != input.PriceSnapshot.Date {
		t.Fatalf("report time/price snapshot drifted: %#v", report)
	}
	if report.ProviderLatency.P50US != 249 || report.ProviderLatency.P95US != 294 {
		t.Fatalf("provider percentiles = %#v, want p50=249 p95=294", report.ProviderLatency)
	}
	if report.RerankLatency.P50US != 149 || report.RerankLatency.P95US != 194 {
		t.Fatalf("rerank percentiles = %#v, want p50=149 p95=194", report.RerankLatency)
	}
	if len(report.ProviderSamplesUS) != ScorecardMeasuredPairs || report.ProviderSamplesUS[0] != 200 || report.ProviderSamplesUS[99] != 299 {
		t.Fatalf("provider raw samples were not retained in schedule order: %#v", report.ProviderSamplesUS)
	}
	if len(report.RerankSamplesUS) != ScorecardMeasuredPairs || report.RerankSamplesUS[0] != 100 || report.RerankSamplesUS[99] != 199 {
		t.Fatalf("rerank raw samples were not retained in schedule order: %#v", report.RerankSamplesUS)
	}
	if !reflect.DeepEqual(report.ProviderCacheHitSamplesUS, []int64{0, 1, 2}) {
		t.Fatalf("cache-hit samples = %#v", report.ProviderCacheHitSamplesUS)
	}
	wantLocalCost := input.PriceSnapshot.Local.GPUHourlyCost * float64(input.PriceSnapshot.Local.GPUCount) * 149.5 /
		float64(ScorecardMicrosecondsPerHour)
	if math.Abs(report.RerankCostPerBatch-wantLocalCost) > 1e-18 {
		t.Fatalf("local rerank cost = %.20g, want %.20g", report.RerankCostPerBatch, wantLocalCost)
	}
	if !report.LatencyPassed || !report.CostPassed || !report.Provisional || report.ProductionPassed {
		t.Fatalf("unexpected gates: %#v", report)
	}
}

func TestScorecardRunSupportsManagedPriceSnapshot(t *testing.T) {
	corpus, input := scorecardTestRun(t)
	input.PriceSnapshot.Local = nil
	input.PriceSnapshot.Managed = &ScorecardManagedPrice{RerankBatchCost: 0.001}

	report, err := EvaluateScorecardRun(corpus, input)
	if !errors.Is(err, ErrScorecardGateFailed) {
		t.Fatalf("EvaluateScorecardRun() error = %v, want ErrScorecardGateFailed", err)
	}
	if report.RerankCostPerBatch != 0.001 || !report.CostPassed || !report.Provisional || report.ProductionPassed {
		t.Fatalf("managed price gate = %#v", report)
	}
}

func TestScorecardRunCannotSelfCertifyDeploymentEvidence(t *testing.T) {
	corpus, input := scorecardTestRun(t)
	input.DeploymentEvidenceID = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	input.PrefixCachingDisabledObserved = true

	report, err := EvaluateScorecardRun(corpus, input)
	if !errors.Is(err, ErrScorecardGateFailed) {
		t.Fatalf("EvaluateScorecardRun() error = %v, want ErrScorecardGateFailed", err)
	}
	if !report.Provisional || report.ProductionPassed || !report.LatencyPassed || !report.CostPassed ||
		report.DeploymentEvidenceID != input.DeploymentEvidenceID || !report.PrefixCachingDisabledObserved {
		t.Fatalf("self-asserted evidence report = %#v", report)
	}
	if validateErr := ValidateScorecardRun(corpus, input); validateErr != nil {
		t.Fatalf("ValidateScorecardRun() rejected a persistable provisional report: %v", validateErr)
	}
}

func TestScorecardRunWithoutDeploymentEvidenceRemainsProvisional(t *testing.T) {
	corpus, input := scorecardTestRun(t)

	report, err := EvaluateScorecardRun(corpus, input)
	if !errors.Is(err, ErrScorecardGateFailed) {
		t.Fatalf("EvaluateScorecardRun() error = %v, want ErrScorecardGateFailed", err)
	}
	if !report.Provisional || report.ProductionPassed || report.DeploymentEvidenceID != "" || report.PrefixCachingDisabledObserved {
		t.Fatalf("pre-R-6a report = %#v", report)
	}
}

func TestScorecardRunCannotBypassRelevanceQualityGate(t *testing.T) {
	corpus, input := scorecardTestRun(t)
	for index := range corpus.Cases {
		reverseScores(&corpus.Cases[index])
	}
	if err := ValidateScorecardRun(corpus, input); !errors.Is(err, ErrInvalidScorecard) {
		t.Fatalf("ValidateScorecardRun(adverse corpus) error = %v, want ErrInvalidScorecard", err)
	}
}

func TestScorecardRunRejectsStructuralDrift(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ScorecardRun)
	}{
		{name: "no cases", mutate: func(run *ScorecardRun) { run.CaseIDs = nil }},
		{name: "manifest drift", mutate: func(run *ScorecardRun) {
			run.ManifestID = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
		}},
		{name: "missing recorded at", mutate: func(run *ScorecardRun) { run.RecordedAt = "" }},
		{name: "non canonical recorded at", mutate: func(run *ScorecardRun) { run.RecordedAt = "2026-08-12T08:00:00+08:00" }},
		{name: "price date drift", mutate: func(run *ScorecardRun) { run.PriceSnapshot.Date = "2026-08-11" }},
		{name: "bad evidence id", mutate: func(run *ScorecardRun) { run.DeploymentEvidenceID = "operator-approved" }},
		{name: "duplicate case", mutate: func(run *ScorecardRun) { run.CaseIDs = []string{"a", "a"} }},
		{name: "missing hardware", mutate: func(run *ScorecardRun) { run.Metadata.Hardware = "" }},
		{name: "missing gpu", mutate: func(run *ScorecardRun) { run.Metadata.GPU = "" }},
		{name: "missing region", mutate: func(run *ScorecardRun) { run.Metadata.Region = "" }},
		{name: "image drift", mutate: func(run *ScorecardRun) {
			run.Metadata.RuntimeImageDigest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
		}},
		{name: "runtime version drift", mutate: func(run *ScorecardRun) { run.Metadata.RuntimeVersion = "v0.23.1" }},
		{name: "model drift", mutate: func(run *ScorecardRun) { run.Metadata.ModelID = "different-model" }},
		{name: "revision drift", mutate: func(run *ScorecardRun) { run.Metadata.ModelRevision = "cccccccccccccccccccccccccccccccccccccccc" }},
		{name: "template drift", mutate: func(run *ScorecardRun) {
			run.Metadata.TemplateSHA256 = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
		}},
		{name: "missing provider", mutate: func(run *ScorecardRun) { run.Metadata.Provider = "" }},
		{name: "missing provider version", mutate: func(run *ScorecardRun) { run.Metadata.ProviderVersion = "" }},
		{name: "bad price date", mutate: func(run *ScorecardRun) { run.PriceSnapshot.Date = "12 August 2026" }},
		{name: "bad currency", mutate: func(run *ScorecardRun) { run.PriceSnapshot.Currency = "usd" }},
		{name: "missing price source", mutate: func(run *ScorecardRun) { run.PriceSnapshot.Source = "" }},
		{name: "missing price revision", mutate: func(run *ScorecardRun) { run.PriceSnapshot.Revision = "" }},
		{name: "bad price evidence", mutate: func(run *ScorecardRun) { run.PriceSnapshot.EvidenceSHA256 = "operator-approved" }},
		{name: "bad provider price", mutate: func(run *ScorecardRun) { run.PriceSnapshot.ProviderCallCost = math.Inf(1) }},
		{name: "no inference price", mutate: func(run *ScorecardRun) { run.PriceSnapshot.Local = nil }},
		{name: "both inference prices", mutate: func(run *ScorecardRun) { run.PriceSnapshot.Managed = &ScorecardManagedPrice{RerankBatchCost: 0.001} }},
		{name: "bad gpu price", mutate: func(run *ScorecardRun) { run.PriceSnapshot.Local.GPUHourlyCost = math.NaN() }},
		{name: "bad gpu count", mutate: func(run *ScorecardRun) { run.PriceSnapshot.Local.GPUCount = 0 }},
		{name: "warmup count", mutate: func(run *ScorecardRun) { run.Warmups = run.Warmups[:ScorecardWarmupsPerPath-1] }},
		{name: "unknown warmup case", mutate: func(run *ScorecardRun) { run.Warmups[0].CaseID = "unknown" }},
		{name: "bad warmup digest", mutate: func(run *ScorecardRun) { run.Warmups[0].InputDigest = "bad" }},
		{name: "bad warmup candidate digest", mutate: func(run *ScorecardRun) { run.Warmups[0].CandidateDigest = "bad" }},
		{name: "warmup provider zero", mutate: func(run *ScorecardRun) { run.Warmups[0].Provider.LatencyUS = 0 }},
		{name: "warmup provider input drift", mutate: func(run *ScorecardRun) {
			run.Warmups[0].Provider.CandidateInputDigest = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
		}},
		{name: "warmup rerank tokens", mutate: func(run *ScorecardRun) { run.Warmups[0].Rerank.TotalTokens++ }},
		{name: "pair count", mutate: func(run *ScorecardRun) { run.Pairs = run.Pairs[:ScorecardMeasuredPairs-1] }},
		{name: "pair index", mutate: func(run *ScorecardRun) { run.Pairs[1].Index = 4 }},
		{name: "case schedule", mutate: func(run *ScorecardRun) { run.Pairs[0].CaseID = "b" }},
		{name: "even order", mutate: func(run *ScorecardRun) { run.Pairs[0].Order = ScorecardOrderRerankProvider }},
		{name: "odd order", mutate: func(run *ScorecardRun) { run.Pairs[1].Order = ScorecardOrderProviderRerank }},
		{name: "provider not fresh", mutate: func(run *ScorecardRun) { run.Pairs[0].Provider.Fresh = false }},
		{name: "provider did not bypass", mutate: func(run *ScorecardRun) { run.Pairs[0].Provider.BypassProviderCache = false }},
		{name: "fresh provider cache hit", mutate: func(run *ScorecardRun) { run.Pairs[0].Provider.CacheHit = true }},
		{name: "provider zero", mutate: func(run *ScorecardRun) { run.Pairs[0].Provider.LatencyUS = 0 }},
		{name: "provider input drift", mutate: func(run *ScorecardRun) {
			run.Pairs[0].Provider.CandidateInputDigest = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
		}},
		{name: "rerank latency bound", mutate: func(run *ScorecardRun) { run.Pairs[0].Rerank.LatencyUS = ScorecardMaximumLatencyUS + 1 }},
		{name: "negative tokens", mutate: func(run *ScorecardRun) { run.Pairs[0].Rerank.PromptTokens = -1; run.Pairs[0].Rerank.TotalTokens = -1 }},
		{name: "token bound", mutate: func(run *ScorecardRun) {
			run.Pairs[0].Rerank.PromptTokens = ScorecardMaximumTokenCount + 1
			run.Pairs[0].Rerank.TotalTokens = ScorecardMaximumTokenCount + 1
		}},
		{name: "token mismatch", mutate: func(run *ScorecardRun) { run.Pairs[0].Rerank.TotalTokens++ }},
		{name: "input mismatch", mutate: func(run *ScorecardRun) {
			run.Pairs[2].InputDigest = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
		}},
		{name: "candidate input mismatch", mutate: func(run *ScorecardRun) {
			run.Pairs[2].CandidateDigest = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
		}},
		{name: "self-consistent caller digest", mutate: func(run *ScorecardRun) {
			caseID := run.Pairs[0].CaseID
			const claimed = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
			for index := range run.Warmups {
				if run.Warmups[index].CaseID == caseID {
					run.Warmups[index].CandidateDigest = claimed
					run.Warmups[index].Provider.CandidateInputDigest = claimed
				}
			}
			for index := range run.Pairs {
				if run.Pairs[index].CaseID == caseID {
					run.Pairs[index].CandidateDigest = claimed
					run.Pairs[index].Provider.CandidateInputDigest = claimed
				}
			}
		}},
		{name: "missing cache hit samples", mutate: func(run *ScorecardRun) { run.ProviderCacheHitSamplesUS = nil }},
		{name: "negative cache hit sample", mutate: func(run *ScorecardRun) { run.ProviderCacheHitSamplesUS[0] = -1 }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			corpus, input := scorecardTestRun(t)
			test.mutate(&input)
			if err := ValidateScorecardRun(corpus, input); !errors.Is(err, ErrInvalidScorecard) {
				t.Fatalf("ValidateScorecardRun() error = %v, want ErrInvalidScorecard", err)
			}
		})
	}
}

func TestScorecardRunGateFailuresRemainAuditable(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ScorecardRun)
		check  func(ScorecardReport) bool
	}{
		{
			name: "latency",
			mutate: func(run *ScorecardRun) {
				for index := range run.Pairs {
					run.Pairs[index].Rerank.LatencyUS = run.Pairs[index].Provider.LatencyUS
				}
			},
			check: func(report ScorecardReport) bool { return !report.LatencyPassed && report.CostPassed },
		},
		{
			name: "cost",
			mutate: func(run *ScorecardRun) {
				run.PriceSnapshot.ProviderCallCost = 1e-15
			},
			check: func(report ScorecardReport) bool { return report.LatencyPassed && !report.CostPassed },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			corpus, input := scorecardTestRun(t)
			test.mutate(&input)
			report, err := EvaluateScorecardRun(corpus, input)
			if !errors.Is(err, ErrScorecardGateFailed) {
				t.Fatalf("EvaluateScorecardRun() error = %v, want ErrScorecardGateFailed", err)
			}
			if !test.check(report) || report.ProductionPassed {
				t.Fatalf("gate report = %#v", report)
			}
		})
	}
}

func TestScorecardLocalCostArithmeticFailureRetainsRawReport(t *testing.T) {
	for _, price := range []float64{math.MaxFloat64, math.SmallestNonzeroFloat64} {
		corpus, input := scorecardTestRun(t)
		input.PriceSnapshot.Local.GPUHourlyCost = price
		report, err := EvaluateScorecardRun(corpus, input)
		if !errors.Is(err, ErrInvalidScorecard) {
			t.Fatalf("EvaluateScorecardRun(price=%g) error = %v, want ErrInvalidScorecard", price, err)
		}
		if len(report.ProviderSamplesUS) != ScorecardMeasuredPairs || len(report.RerankSamplesUS) != ScorecardMeasuredPairs ||
			len(report.MeasuredPairs) != ScorecardMeasuredPairs {
			t.Fatalf("arithmetic failure discarded auditable raw report: %#v", report)
		}
	}
}

func scorecardTestRun(t *testing.T) (Corpus, ScorecardRun) {
	t.Helper()
	files := validTestCorpus(t)
	docsPath, labelsPath, recordingsPath := writeTestCorpus(t, files)
	corpus, err := LoadCorpus(docsPath, labelsPath, recordingsPath, LoadOptions{ExpectedManifestID: testManifestID})
	if err != nil {
		t.Fatalf("LoadCorpus() error = %v", err)
	}
	manifest, err := replay.ReferenceManifest()
	if err != nil {
		t.Fatal(err)
	}
	caseIDs := make([]string, len(corpus.Cases))
	inputDigests := make(map[string]string, len(corpus.Cases))
	candidateDigests := make(map[string]string, len(corpus.Cases))
	for index, goldenCase := range corpus.Cases {
		caseIDs[index] = goldenCase.ID
		inputDigests[goldenCase.ID] = goldenCase.InputDigest
		candidateDigests[goldenCase.ID] = goldenCase.CandidateDigest
	}
	runCaseIDs := append([]string(nil), caseIDs...)
	reverse(runCaseIDs)
	run := ScorecardRun{
		ManifestID: manifest.ManifestID,
		RecordedAt: "2026-08-12T00:00:00Z",
		CaseIDs:    runCaseIDs,
		Metadata: ScorecardMetadata{
			Hardware:           "test-host-v1",
			GPU:                "test-gpu-v1",
			Region:             "test-region-1",
			RuntimeImageDigest: manifest.ImageDigest,
			RuntimeVersion:     manifest.VLLMVersion,
			ModelID:            manifest.ServedModelID,
			ModelRevision:      manifest.ModelRevision,
			TemplateSHA256:     manifest.TemplateSHA256,
			Provider:           "provider-v1",
			ProviderVersion:    "provider-revision-v1",
		},
		PriceSnapshot: ScorecardPriceSnapshot{
			Date:             "2026-08-12",
			Currency:         "USD",
			Source:           "https://prices.example.test/2026-08-12",
			Revision:         "price-snapshot-v1",
			EvidenceSHA256:   "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			ProviderCallCost: 0.01,
			Local: &ScorecardLocalPrice{
				GPUHourlyCost: 1,
				GPUCount:      1,
			},
		},
		ProviderCacheHitSamplesUS: []int64{0, 1, 2},
		Warmups:                   make([]ScorecardWarmupPair, ScorecardWarmupsPerPath),
		Pairs:                     make([]ScorecardMeasuredPair, ScorecardMeasuredPairs),
	}
	for index := range run.Warmups {
		caseID := caseIDs[index%len(caseIDs)]
		run.Warmups[index] = ScorecardWarmupPair{
			CaseID:          caseID,
			InputDigest:     inputDigests[caseID],
			CandidateDigest: candidateDigests[caseID],
			Provider: ScorecardProviderObservation{
				LatencyUS: 10, CandidateInputDigest: candidateDigests[caseID], Fresh: true, BypassProviderCache: true,
			},
			Rerank: ScorecardRerankObservation{LatencyUS: 5, PromptTokens: 10, TotalTokens: 10},
		}
	}
	for index := range run.Pairs {
		caseID := caseIDs[index%len(caseIDs)]
		order := ScorecardOrderProviderRerank
		if index%2 == 1 {
			order = ScorecardOrderRerankProvider
		}
		run.Pairs[index] = ScorecardMeasuredPair{
			Index:           index,
			CaseID:          caseID,
			InputDigest:     inputDigests[caseID],
			CandidateDigest: candidateDigests[caseID],
			Order:           order,
			Provider: ScorecardProviderObservation{
				LatencyUS: int64(200 + index), CandidateInputDigest: candidateDigests[caseID], Fresh: true, BypassProviderCache: true,
			},
			Rerank: ScorecardRerankObservation{
				LatencyUS: int64(100 + index), PromptTokens: int64(100 + index), TotalTokens: int64(100 + index),
			},
		}
	}
	return corpus, run
}

func scorecardCloneTestRun(source ScorecardRun) ScorecardRun {
	cloned := source
	cloned.CaseIDs = append([]string(nil), source.CaseIDs...)
	cloned.Warmups = append([]ScorecardWarmupPair(nil), source.Warmups...)
	cloned.Pairs = append([]ScorecardMeasuredPair(nil), source.Pairs...)
	cloned.ProviderCacheHitSamplesUS = append([]int64(nil), source.ProviderCacheHitSamplesUS...)
	if source.PriceSnapshot.Managed != nil {
		managed := *source.PriceSnapshot.Managed
		cloned.PriceSnapshot.Managed = &managed
	}
	if source.PriceSnapshot.Local != nil {
		local := *source.PriceSnapshot.Local
		cloned.PriceSnapshot.Local = &local
	}
	return cloned
}
