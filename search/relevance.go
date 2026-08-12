package search

import (
	"context"
	"sort"

	searchrerank "github.com/use-agent/purify/search/rerank"
)

// rankedBaselineCandidate is the package-private bridge between the pure
// rerank core and later Search wiring. Provider score remains in result.score;
// relevanceScore is independent model output for every successful candidate.
type rankedBaselineCandidate struct {
	rank           int
	relevanceScore float64
	result         baselineResult
}

// rankCandidatesOutcome keeps internal ranking diagnostics out of the public
// wire until the later models/HTTP/MCP card defines their exact shape.
type rankCandidatesOutcome struct {
	candidates     []rankedBaselineCandidate
	candidateCount int
	deduplicated   int
}

// rankCandidates filters the bounded provider baseline, scores the complete
// canonical candidate pool, selects metadata-component winners, and only then
// applies outputLimit. It is intentionally not connected to Service.Search in
// R-4; default provider fan-out and public responses remain unchanged.
func rankCandidates(
	ctx context.Context,
	scorer searchrerank.Scorer,
	query string,
	source []baselineResult,
	domains []string,
	deduplicate bool,
	outputLimit int,
) (rankCandidatesOutcome, error) {
	if ctx == nil || len(source) > searchrerank.MaxCandidates || outputLimit < 1 || outputLimit > searchrerank.MaxCandidates {
		return rankCandidatesOutcome{}, searchrerank.ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return rankCandidatesOutcome{}, err
	}

	filtered, exactDuplicates := filterBaselineCandidates(source, domains)
	outcome := rankCandidatesOutcome{
		candidates:     make([]rankedBaselineCandidate, 0, min(len(filtered), outputLimit)),
		candidateCount: len(filtered),
		deduplicated:   exactDuplicates,
	}
	requestCandidates := make([]searchrerank.Candidate, len(filtered))
	byURL := make(map[string]baselineResult, len(filtered))
	for index, candidate := range filtered {
		requestCandidates[index] = searchrerank.Candidate{
			CanonicalURL: candidate.url,
			ProviderRank: candidate.providerRank,
			Title:        candidate.title,
			Snippet:      candidate.snippet,
		}
		byURL[candidate.url] = candidate
	}

	ranked, err := searchrerank.RankCandidates(ctx, scorer, query, requestCandidates)
	if err != nil {
		return rankCandidatesOutcome{}, err
	}
	if err := ctx.Err(); err != nil {
		return rankCandidatesOutcome{}, err
	}

	rankingByURL := make(map[string]searchrerank.RankedCandidate, len(ranked))
	for _, candidate := range ranked {
		if _, exists := byURL[candidate.CanonicalURL]; !exists {
			return rankCandidatesOutcome{}, searchrerank.ErrInvalidScores
		}
		rankingByURL[candidate.CanonicalURL] = candidate
	}
	ordered := append([]baselineResult(nil), filtered...)
	if deduplicate {
		components := partitionSimhashComponents(filtered)
		ordered = make([]baselineResult, 0, len(components))
		for _, members := range components {
			winner := members[0]
			for _, candidate := range members[1:] {
				if relevanceRanksBefore(filtered[candidate], filtered[winner], rankingByURL) {
					winner = candidate
				}
			}
			ordered = append(ordered, filtered[winner])
		}
		outcome.deduplicated += len(filtered) - len(ordered)
	}
	sort.Slice(ordered, func(first, second int) bool {
		return relevanceRanksBefore(ordered[first], ordered[second], rankingByURL)
	})
	if len(ordered) > outputLimit {
		ordered = ordered[:outputLimit]
	}
	for index, baseline := range ordered {
		ranking, exists := rankingByURL[baseline.url]
		if !exists {
			return rankCandidatesOutcome{}, searchrerank.ErrInvalidScores
		}
		relevanceScore := ranking.RelevanceScore
		outcome.candidates = append(outcome.candidates, rankedBaselineCandidate{
			rank:           index + 1,
			relevanceScore: relevanceScore,
			result:         cloneBaselineResult(baseline),
		})
	}
	return outcome, nil
}

func relevanceRanksBefore(first, second baselineResult, rankingByURL map[string]searchrerank.RankedCandidate) bool {
	left := rankingByURL[first.url]
	right := rankingByURL[second.url]
	if left.RelevanceScore != right.RelevanceScore {
		return left.RelevanceScore > right.RelevanceScore
	}
	if first.providerRank != second.providerRank {
		return first.providerRank < second.providerRank
	}
	return first.url < second.url
}
