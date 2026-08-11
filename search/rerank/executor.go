package rerank

import (
	"context"
	"math"
	"reflect"
	"sort"
	"strings"
)

// ScoreResult is one scorer output keyed by a stable candidate ID.
type ScoreResult struct {
	StableID       string
	RelevanceScore float64
}

// Scorer is the transport-neutral scoring boundary. Implementations must
// honor ctx and must return exactly one result for every candidate.
type Scorer interface {
	Score(context.Context, ScoreRequest) ([]ScoreResult, error)
}

// ScorerFunc adapts a function to Scorer.
type ScorerFunc func(context.Context, ScoreRequest) ([]ScoreResult, error)

func (function ScorerFunc) Score(ctx context.Context, request ScoreRequest) ([]ScoreResult, error) {
	if function == nil {
		return nil, ErrNotConfigured
	}
	return function(ctx, request)
}

// RankedCandidate is a deterministic relevance result. Rank is reassigned
// contiguously; ProviderRank remains the original provider position.
type RankedCandidate struct {
	Rank           int
	StableID       string
	CanonicalURL   string
	ProviderRank   int
	RelevanceScore float64
}

// RankCandidates builds one bounded batch, invokes the scorer synchronously,
// validates an exact result-set join, and applies the locked total order.
// Runtime timeouts, concurrency slots, and fallback behavior are intentionally
// owned by later layers.
func RankCandidates(ctx context.Context, scorer Scorer, query string, candidates []Candidate) ([]RankedCandidate, error) {
	if ctx == nil {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if isNilScorer(scorer) {
		return nil, ErrNotConfigured
	}

	request, err := BuildRequest(query, candidates)
	if err != nil {
		return nil, err
	}
	if len(request.Candidates) == 0 {
		return make([]RankedCandidate, 0), nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	metadata := make(map[string]RankedCandidate, len(request.Candidates))
	for index, scoringCandidate := range request.Candidates {
		candidate := candidates[index]
		metadata[scoringCandidate.StableID] = RankedCandidate{
			StableID:     strings.Clone(scoringCandidate.StableID),
			CanonicalURL: strings.Clone(candidate.CanonicalURL),
			ProviderRank: candidate.ProviderRank,
		}
	}

	results, scoringErr := invokeScorer(ctx, scorer, cloneScoreRequest(request))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if scoringErr != nil {
		return nil, ErrScoringFailed
	}
	if len(results) != len(request.Candidates) {
		return nil, ErrInvalidScores
	}

	ranked := make([]RankedCandidate, 0, len(results))
	seen := make(map[string]struct{}, len(results))
	for _, result := range results {
		if len(result.StableID) != sha256HexBytes {
			return nil, ErrInvalidScores
		}
		candidate, exists := metadata[result.StableID]
		if result.StableID == "" || !exists {
			return nil, ErrInvalidScores
		}
		if _, duplicate := seen[result.StableID]; duplicate {
			return nil, ErrInvalidScores
		}
		if math.IsNaN(result.RelevanceScore) || math.IsInf(result.RelevanceScore, 0) ||
			result.RelevanceScore < 0 || result.RelevanceScore > 1 {
			return nil, ErrInvalidScores
		}
		seen[result.StableID] = struct{}{}
		if result.RelevanceScore == 0 {
			result.RelevanceScore = 0
		}
		candidate.RelevanceScore = result.RelevanceScore
		ranked = append(ranked, candidate)
	}
	if len(seen) != len(metadata) {
		return nil, ErrInvalidScores
	}

	sort.Slice(ranked, func(first, second int) bool {
		left, right := ranked[first], ranked[second]
		if left.RelevanceScore != right.RelevanceScore {
			return left.RelevanceScore > right.RelevanceScore
		}
		if left.ProviderRank != right.ProviderRank {
			return left.ProviderRank < right.ProviderRank
		}
		return left.CanonicalURL < right.CanonicalURL
	})
	for index := range ranked {
		ranked[index].Rank = index + 1
	}
	return ranked, nil
}

func invokeScorer(ctx context.Context, scorer Scorer, request ScoreRequest) (results []ScoreResult, err error) {
	defer func() {
		if recover() != nil {
			results = nil
			err = ErrScoringFailed
		}
	}()
	return scorer.Score(ctx, request)
}

func cloneScoreRequest(source ScoreRequest) ScoreRequest {
	cloned := ScoreRequest{
		Query:      strings.Clone(source.Query),
		Candidates: make([]ScoringCandidate, len(source.Candidates)),
	}
	for index, candidate := range source.Candidates {
		cloned.Candidates[index] = ScoringCandidate{
			StableID:     strings.Clone(candidate.StableID),
			ProviderRank: candidate.ProviderRank,
			Text:         strings.Clone(candidate.Text),
		}
	}
	return cloned
}

func isNilScorer(scorer Scorer) bool {
	if scorer == nil {
		return true
	}
	value := reflect.ValueOf(scorer)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
