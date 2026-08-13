package search

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"sync"

	"github.com/use-agent/purify/consensus"
	"github.com/use-agent/purify/extract"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/verify/eav"
)

type entityDocumentJudge interface {
	JudgeDocument(ctx context.Context, subject eav.Subject, doc eav.Document) (eav.Judgment, error)
}

// WithTrustJudge installs the optional entity judge used only by explicit
// trust ranking. Independence analysis does not read its verdicts.
func WithTrustJudge(judge entityDocumentJudge) ServiceOption {
	return trustJudgeOption{judge: judge}
}

type trustJudgeOption struct {
	judge entityDocumentJudge
}

func (option trustJudgeOption) applySearchService(service *Service) error {
	if service == nil || isNilSearchDependency(option.judge) {
		return errTrustAnalysisInput
	}
	service.entityJudge = option.judge
	return nil
}

const searchComponentIDDomain = "search-component-v1\x00"

type trustPage struct {
	result             models.SearchResult
	providerRank       int
	relevanceScore     float64
	canonicalURL       string
	analyzed           bool
	mismatch           bool
	operationalFailure bool
	verdict            eav.Verdict
	member             consensus.IndependenceMember
	componentID        string
	foldReasons        []string
	leader             bool
}

type trustFusion struct {
	pages            []trustPage
	status           models.SearchRankingStatus
	reason           models.SearchRankingDegradedReason
	attemptedPages   int
	evaluatedPages   int
	effectiveSources int
	failedPages      int
}

func fuseTrustPages(pages []trustPage, analysis consensus.IndependenceAnalysis) trustFusion {
	membersByURL := make(map[string]consensus.IndependenceMember, len(analysis.Members))
	componentURLs := make(map[string][]string, len(analysis.Members))
	componentReasons := make(map[string]map[consensus.FoldReason]struct{}, len(analysis.Members))
	for _, member := range analysis.Members {
		membersByURL[member.URL] = member
		componentURLs[member.Representative] = append(componentURLs[member.Representative], member.URL)
		if member.ParentReason != "" {
			reasons := componentReasons[member.Representative]
			if reasons == nil {
				reasons = map[consensus.FoldReason]struct{}{}
				componentReasons[member.Representative] = reasons
			}
			reasons[member.ParentReason] = struct{}{}
		}
	}
	componentIDs := make(map[string]string, len(componentURLs))
	for representative, urls := range componentURLs {
		componentIDs[representative] = searchComponentID(urls)
	}

	evaluated := 0
	failed := 0
	effective := map[string]struct{}{}
	for index := range pages {
		page := &pages[index]
		if page.operationalFailure {
			failed++
		}
		member, ok := membersByURL[page.canonicalURL]
		if !ok {
			continue
		}
		page.analyzed = true
		page.member = member
		page.componentID = componentIDs[member.Representative]
		page.foldReasons = orderedFoldReasons(componentReasons[member.Representative])
		evaluated++
		if !page.mismatch {
			effective[member.Representative] = struct{}{}
		}
	}

	if evaluated < 2 {
		sortTrustPages(pages, func(page trustPage) int { return 0 })
		return trustFusion{
			pages: pages, status: models.SearchRankingDegraded,
			reason:         models.SearchRankingReasonTrustUnavailable,
			evaluatedPages: evaluated, failedPages: failed,
		}
	}

	leaders := map[string]string{}
	for _, page := range pages {
		if !page.analyzed || page.mismatch {
			continue
		}
		current, exists := leaders[page.member.Representative]
		if !exists || trustRelevanceBefore(page, pageByURL(pages, current)) {
			leaders[page.member.Representative] = page.canonicalURL
		}
	}
	for index := range pages {
		if pages[index].analyzed && leaders[pages[index].member.Representative] == pages[index].canonicalURL {
			pages[index].leader = true
		}
	}

	sortTrustPages(pages, func(page trustPage) int {
		switch {
		case page.analyzed && !page.mismatch && page.leader:
			return 0
		case !page.analyzed:
			return 1
		case page.analyzed && !page.mismatch:
			return 2
		default:
			return 3
		}
	})
	return trustFusion{
		pages: pages, status: models.SearchRankingApplied,
		evaluatedPages: evaluated, effectiveSources: len(effective), failedPages: failed,
	}
}

func pageByURL(pages []trustPage, url string) trustPage {
	for _, page := range pages {
		if page.canonicalURL == url {
			return page
		}
	}
	return trustPage{}
}

func sortTrustPages(pages []trustPage, bucket func(trustPage) int) {
	sort.SliceStable(pages, func(i, j int) bool {
		left, right := bucket(pages[i]), bucket(pages[j])
		if left != right {
			return left < right
		}
		return trustRelevanceBefore(pages[i], pages[j])
	})
}

func trustRelevanceBefore(first, second trustPage) bool {
	if first.relevanceScore != second.relevanceScore {
		return first.relevanceScore > second.relevanceScore
	}
	if first.providerRank != second.providerRank {
		return first.providerRank < second.providerRank
	}
	return first.canonicalURL < second.canonicalURL
}

func searchComponentID(urls []string) string {
	cloned := append([]string(nil), urls...)
	sort.Strings(cloned)
	digest := sha256.New()
	_, _ = digest.Write([]byte(searchComponentIDDomain))
	for _, rawURL := range cloned {
		var prefix [binary.MaxVarintLen64]byte
		n := binary.PutUvarint(prefix[:], uint64(len(rawURL)))
		_, _ = digest.Write(prefix[:n])
		_, _ = digest.Write([]byte(rawURL))
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func orderedFoldReasons(set map[consensus.FoldReason]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	order := []consensus.FoldReason{consensus.FoldReasonSameRoot, consensus.FoldReasonQuoteLineage, consensus.FoldReasonNearDuplicate}
	reasons := make([]string, 0, len(set))
	for _, reason := range order {
		if _, ok := set[reason]; ok {
			reasons = append(reasons, string(reason))
		}
	}
	return reasons
}

func projectTrustPages(fusion trustFusion, limit int) []models.SearchResult {
	if limit < 1 {
		limit = 1
	}
	if limit > len(fusion.pages) {
		limit = len(fusion.pages)
	}
	results := make([]models.SearchResult, 0, limit)
	for index, page := range fusion.pages[:limit] {
		result := cloneSearchResult(page.result)
		result.Rank = index + 1
		score := page.relevanceScore
		ranking := &models.SearchResultRanking{ProviderRank: page.providerRank, RelevanceScore: &score}
		if page.verdict != "" {
			ranking.Entity = &models.SearchEntityVerdict{Verdict: string(page.verdict)}
		}
		if page.analyzed {
			ranking.Independence = &models.SearchIndependence{
				ComponentID: page.componentID, ComponentSize: page.member.ComponentSize,
				ComponentLeader: page.leader, FoldReasons: append([]string(nil), page.foldReasons...),
			}
		}
		if page.operationalFailure {
			result.Errors = append(append([]models.SearchResultError(nil), result.Errors...), models.SearchResultError{
				Stage: models.SearchResultStageTrust, Code: models.ErrCodeSearchFailed, Message: "trust evaluation failed",
			})
		}
		result.Ranking = ranking
		results = append(results, result)
	}
	return results
}

func (service *Service) evaluateTrust(
	ctx context.Context,
	ranked []rankedBaselineCandidate,
	subject *models.SubjectSpec,
	memo *memoArtifactService,
) (trustFusion, error) {
	if ctx == nil || ctx.Err() != nil {
		return trustFusion{}, ctx.Err()
	}
	limit := models.MaxSearchTrustCandidates
	if len(ranked) < limit {
		limit = len(ranked)
	}
	pages := make([]trustPage, 0, len(ranked))
	sources := make([]trustAnalysisSource, 0, limit)
	indexByURL := map[string]int{}
	attempted := 0
	for _, candidate := range ranked[:limit] {
		attempted++
		page := trustPage{
			result:         projectRankedBaseline([]rankedBaselineCandidate{candidate})[0],
			providerRank:   candidate.result.providerRank,
			relevanceScore: candidate.relevanceScore,
			canonicalURL:   candidate.result.url,
		}
		artifact, fetchErr := memo.FetchPublicArtifact(ctx, candidate.result.url)
		if fetchErr != nil || artifact == nil {
			page.operationalFailure = true
			pages = append(pages, page)
			continue
		}
		source, err := buildTrustAnalysisSource(ctx, artifact, candidate.result.snippet)
		if err != nil {
			page.operationalFailure = true
			pages = append(pages, page)
			continue
		}
		page.canonicalURL = source.result.URL
		if prior, exists := indexByURL[page.canonicalURL]; exists {
			pages[prior].operationalFailure = pages[prior].operationalFailure || page.operationalFailure
			continue
		}
		if service.entityJudge != nil && subject != nil {
			judgment, judgeErr := service.entityJudge.JudgeDocument(ctx, eav.Subject{Name: subject.Name, Hint: subject.Hint}, eav.Document{
				URL: source.result.URL, Cleaned: source.result.CleanedText,
			})
			if judgeErr != nil {
				page.operationalFailure = true
			} else {
				page.verdict = judgment.Verdict
				page.mismatch = judgment.Verdict == eav.VerdictMismatch
			}
		}
		indexByURL[page.canonicalURL] = len(pages)
		pages = append(pages, page)
		if !page.operationalFailure {
			sources = append(sources, source)
		}
	}
	for _, candidate := range ranked[limit:] {
		pages = append(pages, trustPage{
			result:         projectRankedBaseline([]rankedBaselineCandidate{candidate})[0],
			providerRank:   candidate.result.providerRank,
			relevanceScore: candidate.relevanceScore,
			canonicalURL:   candidate.result.url,
		})
	}
	fusion := trustFusion{pages: pages, attemptedPages: attempted, status: models.SearchRankingDegraded, reason: models.SearchRankingReasonTrustUnavailable}
	if len(sources) >= 2 {
		analysis, err := analyzeTrustSources(ctx, sources)
		if err != nil {
			if ctx.Err() != nil {
				return trustFusion{}, ctx.Err()
			}
			fusion.failedPages += 1
			return fusion, nil
		}
		fusion = fuseTrustPages(pages, analysis)
		fusion.attemptedPages = attempted
		if fusion.failedPages > 0 {
			fusion.status = models.SearchRankingPartial
			fusion.reason = ""
		}
	} else {
		fusion.evaluatedPages = len(sources)
	}
	return fusion, nil
}

// memoArtifactService reuses the pages trust analysis already fetched. Trust
// analysis fills it sequentially, but the same memo is then handed to result
// enrichment, which fetches from several goroutines at once, so every map
// access is guarded. A page trust analysis failed to fetch is absent from the
// memo, so concurrent enrichment misses are ordinary rather than exceptional.
type memoArtifactService struct {
	inner ArtifactService
	mutex sync.Mutex
	byURL map[string]*extract.Artifact
}

func newMemoArtifactService(inner ArtifactService) *memoArtifactService {
	return &memoArtifactService{inner: inner, byURL: map[string]*extract.Artifact{}}
}

func (memo *memoArtifactService) FetchPublicArtifact(ctx context.Context, rawURL string) (*extract.Artifact, error) {
	if memo == nil || memo.inner == nil {
		return nil, errTrustAnalysisInput
	}
	if artifact, ok := memo.lookup(rawURL); ok {
		return artifact, nil
	}
	// The inner fetch runs outside the lock so one slow page cannot serialize
	// the bounded enrichment workers. Two workers racing the same missing URL
	// therefore fetch twice; both artifacts are read-only, so the loser is
	// simply discarded rather than shared.
	artifact, err := memo.inner.FetchPublicArtifact(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	return memo.store(rawURL, artifact), nil
}

func (memo *memoArtifactService) lookup(rawURL string) (*extract.Artifact, bool) {
	memo.mutex.Lock()
	defer memo.mutex.Unlock()
	artifact, ok := memo.byURL[rawURL]
	return artifact, ok
}

func (memo *memoArtifactService) store(rawURL string, artifact *extract.Artifact) *extract.Artifact {
	memo.mutex.Lock()
	defer memo.mutex.Unlock()
	if existing, ok := memo.byURL[rawURL]; ok {
		return existing
	}
	memo.byURL[rawURL] = artifact
	return artifact
}

func (memo *memoArtifactService) ExtractArtifact(ctx context.Context, artifact *extract.Artifact, request *models.ExtractRequest) (*models.ExtractResponse, error) {
	return memo.inner.ExtractArtifact(ctx, artifact, request)
}
