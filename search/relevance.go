package search

import (
	"context"
	"sort"

	"github.com/use-agent/purify/models"
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

// rankCandidatesOutcome keeps orchestration state package-private. Service
// projects only the exact public relevance summary defined by the wire model.
type rankCandidatesOutcome struct {
	candidates     []rankedBaselineCandidate
	candidateCount int
	deduplicated   int
}

type preparedRankingPool struct {
	candidates   []baselineResult
	deduplicated int
}

// WithReranker installs the process-owned relevance scorer. The option is
// inert until a request explicitly selects relevance; provider mode never
// observes or invokes it.
func WithReranker(scorer searchrerank.Scorer) ServiceOption {
	return rerankerServiceOption{scorer: scorer}
}

type rerankerServiceOption struct {
	scorer searchrerank.Scorer
}

func (option rerankerServiceOption) applySearchService(service *Service) error {
	if service == nil || isNilSearchDependency(option.scorer) {
		return searchrerank.ErrNotConfigured
	}
	service.reranker = option.scorer
	return nil
}

// rankCandidates filters the bounded provider baseline, scores the complete
// canonical candidate pool, selects metadata-component winners, and only then
// applies outputLimit. Service.Search invokes it only for explicit relevance;
// default provider fan-out and responses remain unchanged.
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

	pool := prepareRankingPool(source, domains)
	return rankPreparedCandidates(ctx, scorer, query, pool, deduplicate, outputLimit)
}

func prepareRankingPool(source []baselineResult, domains []string) preparedRankingPool {
	filtered, exactDuplicates := filterBaselineCandidates(source, domains)
	return preparedRankingPool{candidates: filtered, deduplicated: exactDuplicates}
}

func rankPreparedCandidates(
	ctx context.Context,
	scorer searchrerank.Scorer,
	query string,
	pool preparedRankingPool,
	deduplicate bool,
	outputLimit int,
) (rankCandidatesOutcome, error) {
	if ctx == nil || len(pool.candidates) > searchrerank.MaxCandidates || outputLimit < 1 || outputLimit > searchrerank.MaxCandidates {
		return rankCandidatesOutcome{}, searchrerank.ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return rankCandidatesOutcome{}, err
	}
	filtered := pool.candidates
	outcome := rankCandidatesOutcome{
		candidates:     make([]rankedBaselineCandidate, 0, min(len(filtered), outputLimit)),
		candidateCount: len(filtered),
		deduplicated:   pool.deduplicated,
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

func projectRankedBaseline(source []rankedBaselineCandidate) []models.SearchResult {
	results := make([]models.SearchResult, 0, len(source))
	for _, candidate := range source {
		result := projectBaselineResult(candidate.result, candidate.rank)
		relevanceScore := candidate.relevanceScore
		result.Ranking = &models.SearchResultRanking{
			ProviderRank:   candidate.result.providerRank,
			RelevanceScore: &relevanceScore,
		}
		results = append(results, result)
	}
	return results
}
