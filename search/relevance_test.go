package search

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/use-agent/purify/models"
	searchrerank "github.com/use-agent/purify/search/rerank"
	"github.com/use-agent/purify/simhash"
)

func TestNormalizeProviderResultsPreservesOriginalRankThroughCache(t *testing.T) {
	score := 0.42
	baseline, err := normalizeProviderResults([]ProviderResult{
		{Rank: 1, URL: "javascript:discard()"},
		{Rank: 2, URL: "https://example.com/kept", Title: "kept", Score: &score},
	})
	if err != nil || len(baseline) != 1 {
		t.Fatalf("normalizeProviderResults() = (%#v, %v)", baseline, err)
	}
	if baseline[0].providerRank != 2 {
		t.Fatalf("provider rank = %d, want 2", baseline[0].providerRank)
	}

	clock := &searchTestClock{now: time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)}
	cache := newBaselineCache(time.Minute, 2, 1<<20, clock.Now)
	key := testBaselineKey(91)
	cache.set(key, baseline)
	baseline[0].providerRank = 19
	read, ok := cache.get(key)
	if !ok || len(read) != 1 || read[0].providerRank != 2 {
		t.Fatalf("cached baseline = (%#v, %v)", read, ok)
	}
	read[0].providerRank = 20
	again, ok := cache.get(key)
	if !ok || again[0].providerRank != 2 {
		t.Fatalf("second cached baseline = (%#v, %v)", again, ok)
	}

	if baselineResultCost != 72 {
		t.Fatalf("baselineResultCost = %d, want 72 after provider rank", baselineResultCost)
	}
	wantBytes := baselineCacheEntryCost + baselineResultCost + len("kept") + len("https://example.com/kept")
	if got, ok := baselineResultsSize(again, wantBytes); !ok || got != wantBytes {
		t.Fatalf("baselineResultsSize() = (%d, %v), want (%d, true)", got, ok, wantBytes)
	}
	if _, ok := baselineResultsSize(again, wantBytes-1); ok {
		t.Fatal("baselineResultsSize accepted N+1 byte entry")
	}

	projected, _ := projectBaseline(again, nil, false, 1)
	if len(projected) != 1 || projected[0].Rank != 1 {
		t.Fatalf("public projection recomputed contiguous rank incorrectly: %#v", projected)
	}
}

func TestRankCandidatesFiltersBeforeScoringAndKeepsOriginalMetadata(t *testing.T) {
	providerScore := 0.42
	source := make([]baselineResult, searchrerank.MaxCandidates)
	for index := range source {
		rank := index + 1
		score := providerScore
		host := "keep.example"
		if rank == 1 {
			host = "drop.example"
		}
		source[index] = baselineResult{
			providerRank: rank,
			title:        "title " + string(rune('a'+index)),
			url:          "https://" + host + "/" + string(rune('a'+index)),
			snippet:      "snippet",
			score:        &score,
		}
	}
	// Exact canonical URL dedup is always applied before scorer invocation.
	source[2].url = source[1].url

	scoreByID := make(map[string]float64, len(source))
	for _, candidate := range source {
		id, err := searchrerank.CandidateID(candidate.url)
		if err == nil {
			scoreByID[id] = float64(candidate.providerRank) / 20
		}
	}
	var captured searchrerank.ScoreRequest
	scorer := searchrerank.ScorerFunc(func(_ context.Context, request searchrerank.ScoreRequest) ([]searchrerank.ScoreResult, error) {
		captured = cloneTestScoreRequest(request)
		results := make([]searchrerank.ScoreResult, 0, len(request.Candidates))
		for index := len(request.Candidates) - 1; index >= 0; index-- {
			candidate := request.Candidates[index]
			results = append(results, searchrerank.ScoreResult{StableID: candidate.StableID, RelevanceScore: scoreByID[candidate.StableID]})
		}
		if len(request.Candidates) > 0 {
			request.Candidates[0].StableID = strings.Repeat("f", 64)
			request.Candidates[0].Text = "mutated by scorer"
		}
		return results, nil
	})

	outcome, err := rankCandidates(context.Background(), scorer, "query", source, []string{"keep.example"}, false, 1)
	if err != nil {
		t.Fatalf("rankCandidates() error = %v", err)
	}
	if outcome.candidateCount != 18 || outcome.deduplicated != 1 || len(outcome.candidates) != 1 {
		t.Fatalf("outcome = %#v", outcome)
	}
	wantRanks := []int{2}
	for rank := 4; rank <= 20; rank++ {
		wantRanks = append(wantRanks, rank)
	}
	gotRanks := make([]int, len(captured.Candidates))
	for index, candidate := range captured.Candidates {
		gotRanks[index] = candidate.ProviderRank
	}
	if !reflect.DeepEqual(gotRanks, wantRanks) {
		t.Fatalf("scorer provider ranks = %v, want %v", gotRanks, wantRanks)
	}
	winner := outcome.candidates[0]
	if winner.rank != 1 || winner.result.providerRank != 20 || winner.result.url != source[19].url ||
		winner.relevanceScore != 1 || winner.result.score == nil || *winner.result.score != providerScore {
		t.Fatalf("winner = %#v", winner)
	}
	if source[1].url != "https://keep.example/b" || source[1].title != "title b" || source[1].providerRank != 2 {
		t.Fatalf("source mutated: %#v", source[1])
	}
	// The internal result must not retain mutable score pointers from baseline.
	*winner.result.score = 0.99
	winner.result.title = "caller mutation"
	if *source[19].score != 0.42 || providerScore != 0.42 || source[19].title == "caller mutation" {
		t.Fatalf("outcome mutation reached source: %#v", source[19])
	}
}

func TestRankCandidatesExactURLDuplicateUsesLowestProviderRankIndependentOfInputOrder(t *testing.T) {
	const duplicateURL = "https://duplicate.example/article"
	earlier := baselineResult{
		providerRank: 2,
		title:        "earlier provider result",
		url:          duplicateURL,
		snippet:      "earlier snippet",
	}
	later := baselineResult{
		providerRank: 3,
		title:        "later provider result",
		url:          duplicateURL,
		snippet:      "later snippet",
	}
	other := baselineResult{
		providerRank: 4,
		title:        "other result",
		url:          "https://other.example/article",
		snippet:      "other snippet",
	}

	for name, source := range map[string][]baselineResult{
		"later duplicate first":   {later, other, earlier},
		"earlier duplicate first": {earlier, other, later},
	} {
		t.Run(name, func(t *testing.T) {
			var captured searchrerank.ScoreRequest
			scorer := searchrerank.ScorerFunc(func(_ context.Context, request searchrerank.ScoreRequest) ([]searchrerank.ScoreResult, error) {
				captured = cloneTestScoreRequest(request)
				results := make([]searchrerank.ScoreResult, len(request.Candidates))
				for index, candidate := range request.Candidates {
					results[index] = searchrerank.ScoreResult{StableID: candidate.StableID, RelevanceScore: 0.5}
				}
				return results, nil
			})

			outcome, err := rankCandidates(context.Background(), scorer, "query", source, nil, false, 2)
			if err != nil {
				t.Fatalf("rankCandidates() error = %v", err)
			}
			if outcome.candidateCount != 2 || outcome.deduplicated != 1 || len(captured.Candidates) != 2 {
				t.Fatalf("outcome/captured = (%#v, %#v)", outcome, captured)
			}
			if candidate := captured.Candidates[0]; candidate.ProviderRank != 2 || candidate.Text != "earlier provider result\nearlier snippet" {
				t.Fatalf("duplicate survivor sent to scorer = %#v", candidate)
			}
			if outcome.candidates[0].result.providerRank != 2 || outcome.candidates[0].result.title != earlier.title {
				t.Fatalf("duplicate survivor returned = %#v", outcome.candidates[0])
			}
		})
	}
}

func TestRankCandidatesScoresTwentyBeforeOutputLimit(t *testing.T) {
	source := make([]baselineResult, searchrerank.MaxCandidates)
	scores := make(map[string]float64, len(source))
	for index := range source {
		rank := index + 1
		url := "https://candidate" + string(rune('a'+index)) + ".example/"
		source[index] = baselineResult{providerRank: rank, url: url, title: "candidate"}
		scores[url] = float64(rank) / float64(searchrerank.MaxCandidates)
	}
	seenCandidates := 0
	baseScorer := scorerForURLs(t, scores)
	scorer := searchrerank.ScorerFunc(func(ctx context.Context, request searchrerank.ScoreRequest) ([]searchrerank.ScoreResult, error) {
		seenCandidates = len(request.Candidates)
		return baseScorer.Score(ctx, request)
	})
	outcome, err := rankCandidates(context.Background(), scorer, "query", source, nil, false, 1)
	if err != nil {
		t.Fatalf("rankCandidates() = (%#v, %v)", outcome, err)
	}
	if seenCandidates != searchrerank.MaxCandidates || outcome.candidateCount != searchrerank.MaxCandidates || len(outcome.candidates) != 1 {
		t.Fatalf("candidate counts = scorer:%d outcome:%d output:%d", seenCandidates, outcome.candidateCount, len(outcome.candidates))
	}
	if outcome.candidates[0].result.providerRank != searchrerank.MaxCandidates {
		t.Fatalf("winner = %#v, want provider rank %d", outcome.candidates[0], searchrerank.MaxCandidates)
	}
	full, err := rankCandidates(context.Background(), scorer, "query", source, nil, false, searchrerank.MaxCandidates)
	if err != nil || full.candidateCount != searchrerank.MaxCandidates || len(full.candidates) != searchrerank.MaxCandidates {
		t.Fatalf("output N = (%#v, %v)", full, err)
	}
	for index, candidate := range full.candidates {
		if candidate.rank != index+1 || candidate.result.providerRank != searchrerank.MaxCandidates-index {
			t.Fatalf("full candidate[%d] = %#v", index, candidate)
		}
	}
}

func TestRankCandidatesUsesRelevanceWinnerAcrossFullSimhashComponent(t *testing.T) {
	const (
		textA = "variant0 word1 word2 word3 word4 word5 word6 word7 word8 word9 word10 word11 word12 word13 word14 word15 word16 word17 word18 word19 word20 word21 word22 word23 word24 word25 word26 word27 word28 word29 word30"
		textB = "variant1 word1 word2 word3 word4 word5 word6 word7 word8 word9 word10 word11 word12 word13 word14 word15 word16 word17 word18 word19 word20 word21 word22 word23 word24 word25 word26 word27 word28 word29 word30"
		textC = "word0 variant1 word2 word3 word4 word5 word6 word7 word8 word9 word10 word11 word12 word13 word14 word15 word16 word17 word18 word19 word20 word21 word22 word23 word24 word25 word26 word27 word28 word29 word30"
	)
	source := []baselineResult{
		{providerRank: 1, title: textA, url: "https://alpha.example/"},
		{providerRank: 2, title: textB, url: "https://bravo.example/"},
		{providerRank: 3, title: textC, url: "https://charlie.example/"},
		{providerRank: 4, title: "independent result", url: "https://delta.example/"},
	}
	a, b, c := searchResultFingerprint(source[0]), searchResultFingerprint(source[1]), searchResultFingerprint(source[2])
	if simhash.Distance(a, b) > simhashDedupDistance || simhash.Distance(b, c) > simhashDedupDistance || simhash.Distance(a, c) <= simhashDedupDistance {
		t.Fatalf("invalid transitive fixture distances: %d %d %d", simhash.Distance(a, b), simhash.Distance(b, c), simhash.Distance(a, c))
	}
	scorer := scorerForURLs(t, map[string]float64{
		source[0].url: 0.2,
		source[1].url: 0.4,
		source[2].url: 0.9,
		source[3].url: 0.3,
	})

	permutations := [][]int{
		{0, 1, 2, 3}, {3, 2, 1, 0}, {1, 2, 3, 0}, {2, 0, 3, 1},
	}
	for permutationIndex, permutation := range permutations {
		permuted := make([]baselineResult, len(source))
		for index, sourceIndex := range permutation {
			permuted[index] = source[sourceIndex]
		}
		deduped, err := rankCandidates(context.Background(), scorer, "query", permuted, nil, true, 4)
		if err != nil || deduped.candidateCount != 4 || deduped.deduplicated != 2 || len(deduped.candidates) != 2 {
			t.Fatalf("permutation %d deduped outcome = (%#v, %v)", permutationIndex, deduped, err)
		}
		if deduped.candidates[0].result.url != source[2].url || deduped.candidates[0].result.providerRank != 3 || deduped.candidates[0].rank != 1 {
			t.Fatalf("permutation %d component winner = %#v, want rank-3 relevance winner", permutationIndex, deduped.candidates[0])
		}
		if deduped.candidates[1].result.url != source[3].url || deduped.candidates[1].rank != 2 {
			t.Fatalf("permutation %d contiguous second result = %#v", permutationIndex, deduped.candidates[1])
		}

		unfolded, err := rankCandidates(context.Background(), scorer, "query", permuted, nil, false, 4)
		if err != nil || unfolded.deduplicated != 0 || len(unfolded.candidates) != 4 {
			t.Fatalf("permutation %d unfolded outcome = (%#v, %v)", permutationIndex, unfolded, err)
		}
		wantURLs := []string{source[2].url, source[1].url, source[3].url, source[0].url}
		for index, candidate := range unfolded.candidates {
			if candidate.rank != index+1 || candidate.result.url != wantURLs[index] {
				t.Fatalf("permutation %d unfolded[%d] = %#v, want URL %s", permutationIndex, index, candidate, wantURLs[index])
			}
		}
	}
}

func TestRankCandidatesTotalOrderUsesProviderRankThenURL(t *testing.T) {
	source := []baselineResult{
		{providerRank: 2, url: "https://z.example/", title: "z"},
		{providerRank: 1, url: "https://m.example/", title: "m"},
		{providerRank: 2, url: "https://a.example/", title: "a"},
	}
	scorer := scorerForURLs(t, map[string]float64{
		source[0].url: 0.5,
		source[1].url: 0.5,
		source[2].url: 0.5,
	})
	outcome, err := rankCandidates(context.Background(), scorer, "query", source, nil, false, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{source[1].url, source[2].url, source[0].url}
	for index, candidate := range outcome.candidates {
		if candidate.result.url != want[index] || candidate.rank != index+1 {
			t.Fatalf("candidate[%d] = %#v, want %s", index, candidate, want[index])
		}
	}
}

func TestRankCandidatesMetadataDedupUsesFullNormalizedText(t *testing.T) {
	common := strings.Repeat("common ", 900)
	firstSnippet := strings.TrimSpace(common + strings.Repeat("alpha ", 8000))
	secondSnippet := strings.TrimSpace(common + strings.Repeat("omega ", 8000))
	source := []baselineResult{
		{providerRank: 1, url: "https://alpha.example/", snippet: firstSnippet},
		{providerRank: 2, url: "https://omega.example/", snippet: secondSnippet},
	}
	if distance := simhash.Distance(searchResultFingerprint(source[0]), searchResultFingerprint(source[1])); distance <= simhashDedupDistance {
		t.Fatalf("full metadata fixture distance = %d, want >%d", distance, simhashDedupDistance)
	}
	var documents []string
	scorer := searchrerank.ScorerFunc(func(_ context.Context, request searchrerank.ScoreRequest) ([]searchrerank.ScoreResult, error) {
		documents = []string{request.Candidates[0].Text, request.Candidates[1].Text}
		return []searchrerank.ScoreResult{
			{StableID: request.Candidates[0].StableID, RelevanceScore: 0.5},
			{StableID: request.Candidates[1].StableID, RelevanceScore: 0.5},
		}, nil
	})
	outcome, err := rankCandidates(context.Background(), scorer, "query", source, nil, true, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 2 || documents[0] != documents[1] {
		t.Fatalf("scorer documents differ; fixture does not isolate full metadata: lengths %d/%d", len(documents[0]), len(documents[1]))
	}
	if outcome.deduplicated != 0 || len(outcome.candidates) != 2 {
		t.Fatalf("metadata dedup used scorer truncation: %#v", outcome)
	}
}

func TestRankCandidatesScorerFailuresAreAtomic(t *testing.T) {
	score := 0.75
	source := []baselineResult{
		{providerRank: 1, title: "same", snippet: "copy", url: "https://first.example/", score: &score},
		{providerRank: 2, title: "same", snippet: "copy", url: "https://second.example/"},
		{providerRank: 3, title: "unrelated", snippet: "result", url: "https://third.example/"},
	}
	secret := "scorer-secret-that-must-not-escape"
	tests := []struct {
		name    string
		scorer  searchrerank.Scorer
		wantErr error
	}{
		{name: "error", scorer: searchrerank.ScorerFunc(func(context.Context, searchrerank.ScoreRequest) ([]searchrerank.ScoreResult, error) {
			return nil, errors.New(secret)
		}), wantErr: searchrerank.ErrScoringFailed},
		{name: "panic", scorer: searchrerank.ScorerFunc(func(context.Context, searchrerank.ScoreRequest) ([]searchrerank.ScoreResult, error) {
			panic(secret)
		}), wantErr: searchrerank.ErrScoringFailed},
		{name: "wrong set", scorer: searchrerank.ScorerFunc(func(context.Context, searchrerank.ScoreRequest) ([]searchrerank.ScoreResult, error) {
			return nil, nil
		}), wantErr: searchrerank.ErrInvalidScores},
		{name: "NaN", scorer: searchrerank.ScorerFunc(func(_ context.Context, request searchrerank.ScoreRequest) ([]searchrerank.ScoreResult, error) {
			results := make([]searchrerank.ScoreResult, len(request.Candidates))
			for index, candidate := range request.Candidates {
				results[index] = searchrerank.ScoreResult{StableID: candidate.StableID, RelevanceScore: 0.5}
			}
			results[0].RelevanceScore = math.NaN()
			return results, nil
		}), wantErr: searchrerank.ErrInvalidScores},
		{name: "dependency context error", scorer: searchrerank.ScorerFunc(func(context.Context, searchrerank.ScoreRequest) ([]searchrerank.ScoreResult, error) {
			return nil, context.DeadlineExceeded
		}), wantErr: searchrerank.ErrScoringFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			instrumented := searchrerank.ScorerFunc(func(ctx context.Context, request searchrerank.ScoreRequest) ([]searchrerank.ScoreResult, error) {
				calls++
				return test.scorer.Score(ctx, request)
			})
			outcome, err := rankCandidates(context.Background(), instrumented, "query", source, nil, true, 2)
			if !errors.Is(err, test.wantErr) || len(outcome.candidates) != 0 || outcome.candidateCount != 0 || outcome.deduplicated != 0 || calls != 1 {
				t.Fatalf("rankCandidates() = (%#v, %v)", outcome, err)
			}
			if strings.Contains(errString(err), secret) {
				t.Fatalf("error leaked dependency detail: %v", err)
			}
			if source[0].title != "same" || source[0].score == nil || *source[0].score != score {
				t.Fatalf("failure mutated source: %#v", source[0])
			}
		})
	}
}

func TestRankCandidatesCancellationNeverFallsBack(t *testing.T) {
	source := []baselineResult{{providerRank: 1, url: "https://example.com/", title: "one"}}
	preCanceled, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	scorer := searchrerank.ScorerFunc(func(context.Context, searchrerank.ScoreRequest) ([]searchrerank.ScoreResult, error) {
		calls++
		return nil, nil
	})
	if outcome, err := rankCandidates(preCanceled, scorer, "query", source, nil, false, 1); !errors.Is(err, context.Canceled) || len(outcome.candidates) != 0 || calls != 0 {
		t.Fatalf("pre-canceled = (%#v, %v), calls=%d", outcome, err, calls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	scorer = searchrerank.ScorerFunc(func(_ context.Context, request searchrerank.ScoreRequest) ([]searchrerank.ScoreResult, error) {
		calls++
		cancel()
		return []searchrerank.ScoreResult{{StableID: request.Candidates[0].StableID, RelevanceScore: 1}}, nil
	})
	outcome, err := rankCandidates(ctx, scorer, "query", source, nil, false, 1)
	if !errors.Is(err, context.Canceled) || len(outcome.candidates) != 0 {
		t.Fatalf("late success = (%#v, %v), want context cancellation", outcome, err)
	}
}

func TestRankCandidatesBoundsAndZeroPool(t *testing.T) {
	calls := 0
	scorer := searchrerank.ScorerFunc(func(context.Context, searchrerank.ScoreRequest) ([]searchrerank.ScoreResult, error) {
		calls++
		return nil, nil
	})
	filtered, err := rankCandidates(context.Background(), scorer, "query", []baselineResult{{
		providerRank: 1, url: "https://outside.example/",
	}}, []string{"inside.example"}, true, 1)
	if err != nil || filtered.candidateCount != 0 || filtered.candidates == nil || len(filtered.candidates) != 0 || calls != 0 {
		t.Fatalf("zero pool = (%#v, %v), calls=%d", filtered, err, calls)
	}

	twentyOne := make([]baselineResult, searchrerank.MaxCandidates+1)
	for index := range twentyOne {
		twentyOne[index] = baselineResult{providerRank: min(index+1, searchrerank.MaxCandidates), url: "https://example.com/" + string(rune('a'+index))}
	}
	for _, limit := range []int{0, searchrerank.MaxCandidates + 1} {
		if outcome, err := rankCandidates(context.Background(), scorer, "query", twentyOne[:1], nil, false, limit); !errors.Is(err, searchrerank.ErrInvalidInput) || len(outcome.candidates) != 0 {
			t.Fatalf("limit %d = (%#v, %v)", limit, outcome, err)
		}
	}
	if outcome, err := rankCandidates(context.Background(), scorer, "query", twentyOne, nil, false, 1); !errors.Is(err, searchrerank.ErrInvalidInput) || len(outcome.candidates) != 0 {
		t.Fatalf("candidate N+1 = (%#v, %v)", outcome, err)
	}
	if calls != 0 {
		t.Fatalf("invalid input invoked scorer %d times", calls)
	}
	if outcome, err := rankCandidates(context.Background(), nil, "query", twentyOne[:1], nil, false, 1); !errors.Is(err, searchrerank.ErrNotConfigured) || len(outcome.candidates) != 0 {
		t.Fatalf("nil scorer = (%#v, %v)", outcome, err)
	}
}

func TestR4LeavesDefaultServiceProviderOrdered(t *testing.T) {
	providerScore := 0.31
	provider := &stubSearchProvider{name: "stub", results: []ProviderResult{
		{Rank: 1, URL: "https://first.example/", Title: "first", Score: &providerScore},
		{Rank: 2, URL: "https://second.example/", Title: "second"},
	}}
	clock := &searchTestClock{now: time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)}
	service, err := newService(provider, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	response, err := service.Search(context.Background(), &models.SearchRequest{Query: "query", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	calls, queries := provider.snapshot()
	if calls != 1 || len(queries) != 1 || queries[0].Limit != 1 {
		t.Fatalf("provider calls/queries = (%d, %#v)", calls, queries)
	}
	if len(response.Results) != 1 || response.Results[0].URL != "https://first.example/" || response.Results[0].Score == nil || *response.Results[0].Score != providerScore {
		t.Fatalf("default response = %#v", response)
	}
	encoded, err := json.Marshal(response)
	if err != nil || strings.Contains(string(encoded), "relevance") || strings.Contains(string(encoded), "provider_rank") {
		t.Fatalf("default JSON = %s, error=%v", encoded, err)
	}
}

func scorerForURLs(t *testing.T, scoreByURL map[string]float64) searchrerank.Scorer {
	t.Helper()
	scoreByID := make(map[string]float64, len(scoreByURL))
	for rawURL, score := range scoreByURL {
		stableID, err := searchrerank.CandidateID(rawURL)
		if err != nil {
			t.Fatalf("CandidateID(%q) error = %v", rawURL, err)
		}
		scoreByID[stableID] = score
	}
	return searchrerank.ScorerFunc(func(_ context.Context, request searchrerank.ScoreRequest) ([]searchrerank.ScoreResult, error) {
		results := make([]searchrerank.ScoreResult, 0, len(request.Candidates))
		for index := len(request.Candidates) - 1; index >= 0; index-- {
			candidate := request.Candidates[index]
			score, ok := scoreByID[candidate.StableID]
			if !ok {
				return nil, errors.New("unexpected stable ID")
			}
			results = append(results, searchrerank.ScoreResult{StableID: candidate.StableID, RelevanceScore: score})
		}
		return results, nil
	})
}

func cloneTestScoreRequest(source searchrerank.ScoreRequest) searchrerank.ScoreRequest {
	cloned := searchrerank.ScoreRequest{Query: source.Query, Candidates: make([]searchrerank.ScoringCandidate, len(source.Candidates))}
	copy(cloned.Candidates, source.Candidates)
	return cloned
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
