package rerankeval

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/use-agent/purify/search/rerank"
	"github.com/use-agent/purify/search/rerank/replay"
)

const (
	MinimumMacroDelta       = 0.05
	MinimumHardMacroDelta   = 0.03
	MinimumNonRegressionPct = 80
	MaximumSingleCaseDrop   = 0.15
)

var ErrGateFailed = errors.New("rerankeval: gate failed")

type CaseMetric struct {
	CaseID        string
	Language      string
	Bucket        string
	Hard          bool
	ProviderNDCG5 float64
	RerankedNDCG5 float64
	DeltaNDCG5    float64
}

type SliceMetric struct {
	Name       string
	Cases      int
	DeltaNDCG5 float64
}

type Scorecard struct {
	Cases               []CaseMetric
	TotalCandidates     int
	MacroDeltaNDCG5     float64
	HardMacroDeltaNDCG5 float64
	ByLanguage          []SliceMetric
	HardByBucket        []SliceMetric
	NonRegressionHits   int
	NonRegressionTotal  int
}

// Evaluate computes the scorecard and applies every locked construction gate.
// It returns the scorecard alongside a gate error so failures remain auditable.
func Evaluate(corpus Corpus) (Scorecard, error) {
	scorecard, err := EvaluateMetrics(corpus)
	if err != nil {
		return Scorecard{}, err
	}
	if scorecard.MacroDeltaNDCG5 < MinimumMacroDelta {
		return scorecard, fmt.Errorf("%w: macro delta %.17g is below %.17g", ErrGateFailed, scorecard.MacroDeltaNDCG5, MinimumMacroDelta)
	}
	for _, slice := range scorecard.ByLanguage {
		if slice.Cases == 0 || slice.DeltaNDCG5 < 0 {
			return scorecard, fmt.Errorf("%w: language %s delta %.17g", ErrGateFailed, slice.Name, slice.DeltaNDCG5)
		}
	}
	for _, slice := range scorecard.HardByBucket {
		if slice.Cases == 0 || slice.DeltaNDCG5 < 0 {
			return scorecard, fmt.Errorf("%w: hard bucket %s delta %.17g", ErrGateFailed, slice.Name, slice.DeltaNDCG5)
		}
	}
	if scorecard.HardMacroDeltaNDCG5 < MinimumHardMacroDelta {
		return scorecard, fmt.Errorf("%w: hard delta %.17g is below %.17g", ErrGateFailed, scorecard.HardMacroDeltaNDCG5, MinimumHardMacroDelta)
	}
	if scorecard.NonRegressionTotal == 0 || scorecard.NonRegressionHits*100 < scorecard.NonRegressionTotal*MinimumNonRegressionPct {
		return scorecard, fmt.Errorf("%w: non-regression %d/%d is below %d%%", ErrGateFailed, scorecard.NonRegressionHits, scorecard.NonRegressionTotal, MinimumNonRegressionPct)
	}
	for _, metric := range scorecard.Cases {
		if metric.DeltaNDCG5 < -MaximumSingleCaseDrop {
			return scorecard, fmt.Errorf("%w: case %q drops %.17g", ErrGateFailed, metric.CaseID, metric.DeltaNDCG5)
		}
	}
	return scorecard, nil
}

// EvaluateMetrics computes unrounded metrics after validating all construction
// denominators and coverage. Recorded array order is ignored.
func EvaluateMetrics(corpus Corpus) (Scorecard, error) {
	if err := validateConstructionCorpus(corpus); err != nil {
		return Scorecard{}, err
	}
	orderedCases := append([]Case(nil), corpus.Cases...)
	sort.Slice(orderedCases, func(left, right int) bool { return strings.Compare(orderedCases[left].ID, orderedCases[right].ID) < 0 })

	scorecard := Scorecard{
		Cases:              make([]CaseMetric, 0, len(orderedCases)),
		ByLanguage:         make([]SliceMetric, len(languages)),
		HardByBucket:       make([]SliceMetric, len(buckets)),
		NonRegressionTotal: len(orderedCases),
	}
	languageSums := make(map[string]float64, len(languages))
	languageCounts := make(map[string]int, len(languages))
	hardBucketSums := make(map[string]float64, len(buckets))
	hardBucketCounts := make(map[string]int, len(buckets))
	macroSum := 0.0
	hardSum := 0.0
	hardCases := 0
	for _, goldenCase := range orderedCases {
		providerGrades := providerOrderedGrades(goldenCase.Candidates)
		rerankedGrades, rankErr := productionRerankedGrades(goldenCase)
		if rankErr != nil {
			return Scorecard{}, fmt.Errorf("%w: case %q product ranking", ErrInvalidCorpus, goldenCase.ID)
		}
		providerNDCG, err := rerank.NDCGAt5(providerGrades)
		if err != nil {
			return Scorecard{}, fmt.Errorf("%w: case %q provider NDCG", ErrInvalidCorpus, goldenCase.ID)
		}
		rerankedNDCG, err := rerank.NDCGAt5(rerankedGrades)
		if err != nil {
			return Scorecard{}, fmt.Errorf("%w: case %q reranked NDCG", ErrInvalidCorpus, goldenCase.ID)
		}
		delta := rerankedNDCG - providerNDCG
		if math.IsNaN(delta) || math.IsInf(delta, 0) {
			return Scorecard{}, fmt.Errorf("%w: case %q non-finite metric", ErrInvalidCorpus, goldenCase.ID)
		}
		metric := CaseMetric{
			CaseID: goldenCase.ID, Language: goldenCase.Language, Bucket: goldenCase.Bucket, Hard: goldenCase.Hard,
			ProviderNDCG5: providerNDCG, RerankedNDCG5: rerankedNDCG, DeltaNDCG5: delta,
		}
		scorecard.Cases = append(scorecard.Cases, metric)
		scorecard.TotalCandidates += len(goldenCase.Candidates)
		macroSum += delta
		languageSums[goldenCase.Language] += delta
		languageCounts[goldenCase.Language]++
		if delta >= 0 {
			scorecard.NonRegressionHits++
		}
		if goldenCase.Hard {
			hardSum += delta
			hardCases++
			hardBucketSums[goldenCase.Bucket] += delta
			hardBucketCounts[goldenCase.Bucket]++
		}
	}
	scorecard.MacroDeltaNDCG5 = macroSum / float64(len(orderedCases))
	if hardCases == 0 {
		return Scorecard{}, fmt.Errorf("%w: empty hard aggregate", ErrInvalidCorpus)
	}
	scorecard.HardMacroDeltaNDCG5 = hardSum / float64(hardCases)
	for index, language := range languages {
		count := languageCounts[language]
		if count == 0 {
			return Scorecard{}, fmt.Errorf("%w: empty language %q", ErrInvalidCorpus, language)
		}
		scorecard.ByLanguage[index] = SliceMetric{Name: language, Cases: count, DeltaNDCG5: languageSums[language] / float64(count)}
	}
	for index, bucket := range buckets {
		count := hardBucketCounts[bucket]
		if count == 0 {
			return Scorecard{}, fmt.Errorf("%w: empty hard bucket %q", ErrInvalidCorpus, bucket)
		}
		scorecard.HardByBucket[index] = SliceMetric{Name: bucket, Cases: count, DeltaNDCG5: hardBucketSums[bucket] / float64(count)}
	}
	return scorecard, nil
}

func validateConstructionCorpus(corpus Corpus) error {
	if len(corpus.Cases) < MinimumCases {
		return fmt.Errorf("%w: need at least %d cases", ErrInvalidCorpus, MinimumCases)
	}
	reference, err := replay.ReferenceManifest()
	if err != nil {
		return fmt.Errorf("%w: reference manifest", ErrInvalidCorpus)
	}
	seenCases := make(map[string]struct{}, len(corpus.Cases))
	seenQueries := make(map[string]struct{}, len(corpus.Cases))
	languageBucket := make(map[string]int, len(languages)*len(buckets))
	hardBuckets := make(map[string]int, len(buckets))
	hardLanguages := make(map[string]int, len(languages))
	totalCandidates := 0
	for _, goldenCase := range corpus.Cases {
		if !validPlainID(goldenCase.ID) || !validLanguage(goldenCase.Language) || !validBucket(goldenCase.Bucket) ||
			len(goldenCase.Candidates) < MinimumCaseCandidates || len(goldenCase.Candidates) > rerank.MaxCandidates ||
			goldenCase.ManifestID != reference.ManifestID || !validDigest(goldenCase.InputDigest) || !validDigest(goldenCase.CandidateDigest) ||
			goldenCase.LatencyUS <= 0 || goldenCase.LatencyUS > MaximumRecordingLatencyUS ||
			goldenCase.Usage.PromptTokens < 0 || goldenCase.Usage.PromptTokens > maxTokenCount || goldenCase.Usage.TotalTokens != goldenCase.Usage.PromptTokens {
			return fmt.Errorf("%w: invalid case %q", ErrInvalidCorpus, goldenCase.ID)
		}
		if _, duplicate := seenCases[goldenCase.ID]; duplicate {
			return fmt.Errorf("%w: duplicate case %q", ErrInvalidCorpus, goldenCase.ID)
		}
		if _, duplicate := seenQueries[goldenCase.Query]; duplicate {
			return fmt.Errorf("%w: duplicate query", ErrInvalidCorpus)
		}
		seenCases[goldenCase.ID] = struct{}{}
		seenQueries[goldenCase.Query] = struct{}{}
		seenIDs := make(map[string]struct{}, len(goldenCase.Candidates))
		seenRanks := make([]bool, len(goldenCase.Candidates))
		requestCandidates := make([]rerank.Candidate, len(goldenCase.Candidates))
		for index, candidate := range goldenCase.Candidates {
			if !validPlainID(candidate.ID) || candidate.ProviderRank < 1 || candidate.ProviderRank > len(goldenCase.Candidates) || seenRanks[candidate.ProviderRank-1] ||
				candidate.Grade < 0 || candidate.Grade > 3 || math.IsNaN(candidate.RelevanceScore) || math.IsInf(candidate.RelevanceScore, 0) ||
				candidate.RelevanceScore < 0 || candidate.RelevanceScore > 1 {
				return fmt.Errorf("%w: invalid candidate in case %q", ErrInvalidCorpus, goldenCase.ID)
			}
			if _, duplicate := seenIDs[candidate.ID]; duplicate {
				return fmt.Errorf("%w: duplicate candidate %q", ErrInvalidCorpus, candidate.ID)
			}
			stableID, stableErr := rerank.CandidateID(candidate.CanonicalURL)
			if stableErr != nil || candidate.ID != stableID {
				return fmt.Errorf("%w: candidate %q does not match canonical URL", ErrInvalidCorpus, candidate.ID)
			}
			seenIDs[candidate.ID] = struct{}{}
			seenRanks[candidate.ProviderRank-1] = true
			requestCandidates[index] = rerank.Candidate{CanonicalURL: candidate.CanonicalURL, ProviderRank: candidate.ProviderRank, Title: candidate.Title, Snippet: candidate.Snippet}
		}
		request, err := rerank.BuildRequest(goldenCase.Query, requestCandidates)
		if err != nil {
			return fmt.Errorf("%w: case %q production input", ErrInvalidCorpus, goldenCase.ID)
		}
		digest, err := rerank.ReferenceInputDigest(request)
		if err != nil || digest != goldenCase.InputDigest {
			return fmt.Errorf("%w: case %q input digest", ErrInvalidCorpus, goldenCase.ID)
		}
		candidateDigest, err := rerank.ReferenceCandidateDigest(request)
		if err != nil || candidateDigest != goldenCase.CandidateDigest {
			return fmt.Errorf("%w: case %q candidate digest", ErrInvalidCorpus, goldenCase.ID)
		}
		if objectiveHard(goldenCase.Candidates) != goldenCase.Hard {
			return fmt.Errorf("%w: case %q hard label mismatch", ErrInvalidCorpus, goldenCase.ID)
		}
		if _, err := rerank.NDCGAt5(providerOrderedGrades(goldenCase.Candidates)); err != nil {
			return fmt.Errorf("%w: case %q has no valid IDCG", ErrInvalidCorpus, goldenCase.ID)
		}
		totalCandidates += len(goldenCase.Candidates)
		languageBucket[goldenCase.Language+"\x00"+goldenCase.Bucket]++
		if goldenCase.Hard {
			hardBuckets[goldenCase.Bucket]++
			hardLanguages[goldenCase.Language]++
		}
	}
	if totalCandidates < MinimumCandidates {
		return fmt.Errorf("%w: need at least %d candidates", ErrInvalidCorpus, MinimumCandidates)
	}
	for _, language := range languages {
		for _, bucket := range buckets {
			if languageBucket[language+"\x00"+bucket] == 0 {
				return fmt.Errorf("%w: empty language/bucket slice %s/%s", ErrInvalidCorpus, language, bucket)
			}
		}
		if hardLanguages[language] < 2 {
			return fmt.Errorf("%w: language %s needs at least two hard cases", ErrInvalidCorpus, language)
		}
	}
	for _, bucket := range buckets {
		if hardBuckets[bucket] == 0 {
			return fmt.Errorf("%w: bucket %s needs a hard case", ErrInvalidCorpus, bucket)
		}
	}
	return nil
}

func providerOrderedGrades(candidates []Candidate) []int {
	ordered := append([]Candidate(nil), candidates...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left].ProviderRank < ordered[right].ProviderRank })
	grades := make([]int, len(ordered))
	for index, candidate := range ordered {
		grades[index] = candidate.Grade
	}
	return grades
}

func productionRerankedGrades(goldenCase Case) ([]int, error) {
	candidates := make([]rerank.Candidate, len(goldenCase.Candidates))
	scoreByID := make(map[string]float64, len(goldenCase.Candidates))
	gradeByID := make(map[string]int, len(goldenCase.Candidates))
	for index, candidate := range goldenCase.Candidates {
		candidates[index] = rerank.Candidate{
			CanonicalURL: candidate.CanonicalURL,
			ProviderRank: candidate.ProviderRank,
			Title:        candidate.Title,
			Snippet:      candidate.Snippet,
		}
		scoreByID[candidate.ID] = candidate.RelevanceScore
		gradeByID[candidate.ID] = candidate.Grade
	}
	scorer := rerank.ScorerFunc(func(_ context.Context, request rerank.ScoreRequest) ([]rerank.ScoreResult, error) {
		results := make([]rerank.ScoreResult, len(request.Candidates))
		for index := range request.Candidates {
			stableID := request.Candidates[len(request.Candidates)-1-index].StableID
			score, present := scoreByID[stableID]
			if !present {
				return nil, ErrInvalidCorpus
			}
			results[index] = rerank.ScoreResult{StableID: stableID, RelevanceScore: score}
		}
		return results, nil
	})
	ranked, err := rerank.RankCandidates(context.Background(), scorer, goldenCase.Query, candidates)
	if err != nil {
		return nil, err
	}
	grades := make([]int, len(ranked))
	for index, candidate := range ranked {
		grade, present := gradeByID[candidate.StableID]
		if !present {
			return nil, ErrInvalidCorpus
		}
		grades[index] = grade
	}
	return grades, nil
}

func validPlainID(value string) bool {
	if value == "" || len(value) > MaxJSONStringBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}
