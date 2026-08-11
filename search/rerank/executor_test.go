package rerank

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRankCandidatesUsesExactTotalOrderAcrossPermutations(t *testing.T) {
	base := []Candidate{
		{CanonicalURL: "https://z.example/", ProviderRank: 5, Title: "z"},
		{CanonicalURL: "https://b.example/", ProviderRank: 1, Title: "b"},
		{CanonicalURL: "https://a.example/", ProviderRank: 1, Title: "a"},
		{CanonicalURL: "https://top.example/", ProviderRank: 3, Title: "top"},
		{CanonicalURL: "https://near.example/", ProviderRank: 4, Title: "near"},
	}
	scoreByText := map[string]float64{
		"z\n":    0.5,
		"b\n":    0.5,
		"a\n":    0.5,
		"top\n":  0.9,
		"near\n": math.Nextafter(0.9, 0),
	}
	scoreByID := make(map[string]float64, len(base))
	idByURL := make(map[string]string, len(base))
	scoreByURL := make(map[string]float64, len(base))
	metadataByURL := make(map[string]Candidate, len(base))
	for _, candidate := range base {
		stableID, err := CandidateID(candidate.CanonicalURL)
		if err != nil {
			t.Fatalf("CandidateID(%q) error = %v", candidate.CanonicalURL, err)
		}
		score := scoreByText[candidate.Title+"\n"+candidate.Snippet]
		scoreByID[stableID] = score
		idByURL[candidate.CanonicalURL] = stableID
		scoreByURL[candidate.CanonicalURL] = score
		metadataByURL[candidate.CanonicalURL] = candidate
	}
	scorer := ScorerFunc(func(_ context.Context, request ScoreRequest) ([]ScoreResult, error) {
		results := make([]ScoreResult, 0, len(request.Candidates))
		for index := len(request.Candidates) - 1; index >= 0; index-- {
			candidate := request.Candidates[index]
			results = append(results, ScoreResult{StableID: candidate.StableID, RelevanceScore: scoreByID[candidate.StableID]})
		}
		return results, nil
	})

	permutations := candidatePermutations(base)
	wantURLs := []string{
		"https://top.example/",
		"https://near.example/",
		"https://a.example/",
		"https://b.example/",
		"https://z.example/",
	}
	var baseline []RankedCandidate
	for permutationIndex, candidates := range permutations {
		original := append([]Candidate(nil), candidates...)
		ranked, err := RankCandidates(context.Background(), scorer, "query", candidates)
		if err != nil {
			t.Fatalf("permutation %d RankCandidates() error = %v", permutationIndex, err)
		}
		gotURLs := make([]string, len(ranked))
		for index := range ranked {
			gotURLs[index] = ranked[index].CanonicalURL
			if ranked[index].Rank != index+1 {
				t.Fatalf("permutation %d result %d rank = %d", permutationIndex, index, ranked[index].Rank)
			}
			source := metadataByURL[ranked[index].CanonicalURL]
			if ranked[index].StableID != idByURL[ranked[index].CanonicalURL] ||
				ranked[index].ProviderRank != source.ProviderRank ||
				ranked[index].RelevanceScore != scoreByURL[ranked[index].CanonicalURL] {
				t.Fatalf("permutation %d result %d lost authoritative metadata: %#v", permutationIndex, index, ranked[index])
			}
		}
		if !reflect.DeepEqual(gotURLs, wantURLs) {
			t.Fatalf("permutation %d order = %v, want %v", permutationIndex, gotURLs, wantURLs)
		}
		if !reflect.DeepEqual(candidates, original) {
			t.Fatalf("permutation %d mutated input: got %#v want %#v", permutationIndex, candidates, original)
		}
		if permutationIndex == 0 {
			baseline = append([]RankedCandidate(nil), ranked...)
		} else if !reflect.DeepEqual(ranked, baseline) {
			t.Fatalf("permutation %d full result = %#v, want %#v", permutationIndex, ranked, baseline)
		}
	}
}

func candidatePermutations(source []Candidate) [][]Candidate {
	permutations := make([][]Candidate, 0, len(source)+10)
	for offset := range source {
		rotated := make([]Candidate, 0, len(source))
		rotated = append(rotated, source[offset:]...)
		rotated = append(rotated, source[:offset]...)
		permutations = append(permutations, rotated)
	}
	reversed := append([]Candidate(nil), source...)
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	permutations = append(permutations, reversed)
	random := rand.New(rand.NewSource(20260812))
	for range 8 {
		shuffled := append([]Candidate(nil), source...)
		random.Shuffle(len(shuffled), func(first, second int) {
			shuffled[first], shuffled[second] = shuffled[second], shuffled[first]
		})
		permutations = append(permutations, shuffled)
	}
	return permutations
}

func TestRankCandidatesJoinsByStableIDAndContainsBackendMutation(t *testing.T) {
	candidates := []Candidate{
		{CanonicalURL: "https://one.example/", ProviderRank: 1, Title: "one"},
		{CanonicalURL: "https://two.example/", ProviderRank: 3, Title: "two"},
	}
	original := append([]Candidate(nil), candidates...)
	scorer := ScorerFunc(func(_ context.Context, request ScoreRequest) ([]ScoreResult, error) {
		firstID := request.Candidates[0].StableID
		secondID := request.Candidates[1].StableID
		request.Query = "mutated"
		request.Candidates[0].StableID = "mutated"
		request.Candidates[0].Text = "mutated"
		return []ScoreResult{
			{StableID: secondID, RelevanceScore: 0.9},
			{StableID: firstID, RelevanceScore: 0.1},
		}, nil
	})
	ranked, err := RankCandidates(context.Background(), scorer, "query", candidates)
	if err != nil {
		t.Fatalf("RankCandidates() error = %v", err)
	}
	if got := ranked[0].CanonicalURL; got != "https://two.example/" {
		t.Fatalf("first URL = %q", got)
	}
	if !reflect.DeepEqual(candidates, original) {
		t.Fatalf("backend mutation reached caller: got %#v want %#v", candidates, original)
	}
}

func TestRankCandidatesRejectsInvalidScoreSets(t *testing.T) {
	candidates := []Candidate{
		{CanonicalURL: "https://example.com/a", ProviderRank: 1, Title: "a"},
		{CanonicalURL: "https://example.com/b", ProviderRank: 2, Title: "b"},
	}
	validScores := func(request ScoreRequest) []ScoreResult {
		return []ScoreResult{
			{StableID: request.Candidates[0].StableID, RelevanceScore: 0.5},
			{StableID: request.Candidates[1].StableID, RelevanceScore: 0.4},
		}
	}
	tests := []struct {
		name   string
		scorer ScorerFunc
		want   error
	}{
		{name: "missing", scorer: func(_ context.Context, request ScoreRequest) ([]ScoreResult, error) {
			return validScores(request)[:1], nil
		}},
		{name: "extra", scorer: func(_ context.Context, request ScoreRequest) ([]ScoreResult, error) {
			return append(validScores(request), ScoreResult{StableID: "extra", RelevanceScore: 0.1}), nil
		}},
		{name: "unknown", scorer: func(_ context.Context, request ScoreRequest) ([]ScoreResult, error) {
			results := validScores(request)
			results[1].StableID = strings.Repeat("0", 64)
			return results, nil
		}},
		{name: "duplicate", scorer: func(_ context.Context, request ScoreRequest) ([]ScoreResult, error) {
			results := validScores(request)
			results[1].StableID = results[0].StableID
			return results, nil
		}},
		{name: "empty ID", scorer: func(_ context.Context, request ScoreRequest) ([]ScoreResult, error) {
			results := validScores(request)
			results[0].StableID = ""
			return results, nil
		}},
		{name: "short ID", scorer: func(_ context.Context, request ScoreRequest) ([]ScoreResult, error) {
			results := validScores(request)
			results[0].StableID = strings.Repeat("0", 63)
			return results, nil
		}},
		{name: "long ID", scorer: func(_ context.Context, request ScoreRequest) ([]ScoreResult, error) {
			results := validScores(request)
			results[0].StableID = strings.Repeat("0", 65)
			return results, nil
		}},
		{name: "huge ID", scorer: func(_ context.Context, request ScoreRequest) ([]ScoreResult, error) {
			results := validScores(request)
			results[0].StableID = strings.Repeat("0", 1<<20)
			return results, nil
		}},
		{name: "uppercase ID", scorer: func(_ context.Context, request ScoreRequest) ([]ScoreResult, error) {
			results := validScores(request)
			results[0].StableID = strings.ToUpper(results[0].StableID)
			return results, nil
		}},
		{name: "nan", scorer: func(_ context.Context, request ScoreRequest) ([]ScoreResult, error) {
			results := validScores(request)
			results[0].RelevanceScore = math.NaN()
			return results, nil
		}},
		{name: "positive infinity", scorer: func(_ context.Context, request ScoreRequest) ([]ScoreResult, error) {
			results := validScores(request)
			results[0].RelevanceScore = math.Inf(1)
			return results, nil
		}},
		{name: "negative infinity", scorer: func(_ context.Context, request ScoreRequest) ([]ScoreResult, error) {
			results := validScores(request)
			results[0].RelevanceScore = math.Inf(-1)
			return results, nil
		}},
		{name: "negative", scorer: func(_ context.Context, request ScoreRequest) ([]ScoreResult, error) {
			results := validScores(request)
			results[0].RelevanceScore = -0.1
			return results, nil
		}},
		{name: "over one", scorer: func(_ context.Context, request ScoreRequest) ([]ScoreResult, error) {
			results := validScores(request)
			results[0].RelevanceScore = 1.1
			return results, nil
		}},
		{name: "results plus error", scorer: func(_ context.Context, request ScoreRequest) ([]ScoreResult, error) {
			return validScores(request), errors.New("private partial response")
		}, want: ErrScoringFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ranked, err := RankCandidates(context.Background(), test.scorer, "query", candidates)
			want := test.want
			if want == nil {
				want = ErrInvalidScores
			}
			if !errors.Is(err, want) || ranked != nil {
				t.Fatalf("RankCandidates() = (%#v, %v), want nil %v", ranked, err, want)
			}
			if strings.Contains(err.Error(), "private") {
				t.Fatalf("error leaked backend detail: %v", err)
			}
		})
	}
}

func TestRankCandidatesAcceptsClosedScoreRange(t *testing.T) {
	candidates := []Candidate{
		{CanonicalURL: "https://zero.example/", ProviderRank: 1},
		{CanonicalURL: "https://one.example/", ProviderRank: 2},
	}
	scorer := ScorerFunc(func(_ context.Context, request ScoreRequest) ([]ScoreResult, error) {
		return []ScoreResult{
			{StableID: request.Candidates[0].StableID, RelevanceScore: 0},
			{StableID: request.Candidates[1].StableID, RelevanceScore: 1},
		}, nil
	})
	ranked, err := RankCandidates(context.Background(), scorer, "query", candidates)
	if err != nil {
		t.Fatalf("RankCandidates() error = %v", err)
	}
	if ranked[0].CanonicalURL != "https://one.example/" || ranked[0].RelevanceScore != 1 ||
		ranked[1].CanonicalURL != "https://zero.example/" || ranked[1].RelevanceScore != 0 {
		t.Fatalf("closed-range result = %#v", ranked)
	}
}

func TestRankCandidatesSanitizesBackendErrorAndPanic(t *testing.T) {
	candidates := []Candidate{{CanonicalURL: "https://example.com/a", ProviderRank: 1}}
	tests := []struct {
		name   string
		scorer ScorerFunc
	}{
		{name: "private error", scorer: func(context.Context, ScoreRequest) ([]ScoreResult, error) {
			return nil, errors.New("secret endpoint token")
		}},
		{name: "private panic", scorer: func(context.Context, ScoreRequest) ([]ScoreResult, error) {
			panic("secret panic token")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ranked, err := RankCandidates(context.Background(), test.scorer, "query", candidates)
			if !errors.Is(err, ErrScoringFailed) || ranked != nil {
				t.Fatalf("RankCandidates() = (%#v, %v), want nil ErrScoringFailed", ranked, err)
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "token") {
				t.Fatalf("error leaked private detail: %v", err)
			}
		})
	}
}

func TestRankCandidatesNormalizesNegativeZero(t *testing.T) {
	scorer := ScorerFunc(func(_ context.Context, request ScoreRequest) ([]ScoreResult, error) {
		return []ScoreResult{{StableID: request.Candidates[0].StableID, RelevanceScore: math.Copysign(0, -1)}}, nil
	})
	ranked, err := RankCandidates(context.Background(), scorer, "query", []Candidate{{CanonicalURL: "https://example.com/a", ProviderRank: 1}})
	if err != nil {
		t.Fatalf("RankCandidates() error = %v", err)
	}
	if math.Signbit(ranked[0].RelevanceScore) {
		t.Fatalf("score retained negative zero: %v", ranked[0].RelevanceScore)
	}
}

func TestRankCandidatesZeroCandidatesAndCanceledContextSkipBackend(t *testing.T) {
	var calls atomic.Int32
	scorer := ScorerFunc(func(context.Context, ScoreRequest) ([]ScoreResult, error) {
		calls.Add(1)
		return nil, nil
	})

	ranked, err := RankCandidates(context.Background(), scorer, "query", nil)
	if err != nil || ranked == nil || len(ranked) != 0 {
		t.Fatalf("empty RankCandidates() = (%#v, %v)", ranked, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if ranked, err = RankCandidates(canceled, scorer, "query", []Candidate{{CanonicalURL: "https://example.com/a", ProviderRank: 1}}); !errors.Is(err, context.Canceled) || ranked != nil {
		t.Fatalf("canceled RankCandidates() = (%#v, %v)", ranked, err)
	}
	if calls.Load() != 0 {
		t.Fatalf("backend calls = %d, want 0", calls.Load())
	}
}

func TestRankCandidatesInvalidInputSkipsBackend(t *testing.T) {
	var calls atomic.Int32
	scorer := ScorerFunc(func(context.Context, ScoreRequest) ([]ScoreResult, error) {
		calls.Add(1)
		return nil, nil
	})
	candidates := make([]Candidate, MaxCandidates+1)
	if ranked, err := RankCandidates(context.Background(), scorer, "query", candidates); ranked != nil || !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("RankCandidates(over limit) = (%#v, %v), want nil ErrInvalidInput", ranked, err)
	}
	if ranked, err := RankCandidates(context.Background(), scorer, "bad  query", nil); ranked != nil || !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("RankCandidates(bad query) = (%#v, %v), want nil ErrInvalidInput", ranked, err)
	}
	if calls.Load() != 0 {
		t.Fatalf("backend calls = %d, want 0", calls.Load())
	}
}

func TestRankCandidatesPassesParentContextUnchanged(t *testing.T) {
	deadline := time.Now().Add(time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	scorer := ScorerFunc(func(received context.Context, request ScoreRequest) ([]ScoreResult, error) {
		gotDeadline, ok := received.Deadline()
		if !ok || !gotDeadline.Equal(deadline) {
			t.Fatalf("scorer deadline = (%v, %v), want %v", gotDeadline, ok, deadline)
		}
		return []ScoreResult{{StableID: request.Candidates[0].StableID, RelevanceScore: 0.5}}, nil
	})
	if _, err := RankCandidates(ctx, scorer, "query", []Candidate{{CanonicalURL: "https://example.com/context", ProviderRank: 1}}); err != nil {
		t.Fatalf("RankCandidates() error = %v", err)
	}
}

func TestRankCandidatesRejectsLateSuccessAfterContextDeadline(t *testing.T) {
	scorer := ScorerFunc(func(ctx context.Context, request ScoreRequest) ([]ScoreResult, error) {
		<-ctx.Done()
		return []ScoreResult{{StableID: request.Candidates[0].StableID, RelevanceScore: 1}}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	ranked, err := RankCandidates(ctx, scorer, "query", []Candidate{{CanonicalURL: "https://example.com/a", ProviderRank: 1}})
	if !errors.Is(err, context.DeadlineExceeded) || ranked != nil {
		t.Fatalf("RankCandidates() = (%#v, %v), want deadline nil result", ranked, err)
	}
}

func TestRankCandidatesDoesNotTrustDependencyContextError(t *testing.T) {
	scorer := ScorerFunc(func(context.Context, ScoreRequest) ([]ScoreResult, error) {
		return nil, context.DeadlineExceeded
	})
	ranked, err := RankCandidates(context.Background(), scorer, "query", []Candidate{{CanonicalURL: "https://example.com/a", ProviderRank: 1}})
	if !errors.Is(err, ErrScoringFailed) || errors.Is(err, context.DeadlineExceeded) || ranked != nil {
		t.Fatalf("RankCandidates() = (%#v, %v), want sanitized scorer failure", ranked, err)
	}
}

func TestRankCandidatesRejectsNilScorerAndNilContext(t *testing.T) {
	var typedNil *typedNilScorer
	candidate := []Candidate{{CanonicalURL: "https://example.com/a", ProviderRank: 1}}
	tests := []struct {
		name   string
		ctx    context.Context
		scorer Scorer
		want   error
	}{
		{name: "nil scorer", ctx: context.Background(), want: ErrNotConfigured},
		{name: "typed nil scorer", ctx: context.Background(), scorer: typedNil, want: ErrNotConfigured},
		{name: "nil context", scorer: ScorerFunc(func(context.Context, ScoreRequest) ([]ScoreResult, error) { return nil, nil }), want: ErrInvalidInput},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ranked, err := RankCandidates(test.ctx, test.scorer, "query", candidate)
			if !errors.Is(err, test.want) || ranked != nil {
				t.Fatalf("RankCandidates() = (%#v, %v), want nil %v", ranked, err, test.want)
			}
		})
	}
}

func TestNilScorerFuncDirectCallIsSafe(t *testing.T) {
	var scorer ScorerFunc
	results, err := scorer.Score(context.Background(), ScoreRequest{})
	if results != nil || !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("nil ScorerFunc.Score() = (%#v, %v), want nil ErrNotConfigured", results, err)
	}
}

type typedNilScorer struct{}

func (*typedNilScorer) Score(context.Context, ScoreRequest) ([]ScoreResult, error) { return nil, nil }

func errorsIs(err, target error) bool { return errors.Is(err, target) }
