package search

import (
	"context"
	"testing"
	"time"

	"github.com/use-agent/purify/consensus"
	"github.com/use-agent/purify/extract"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/scraper"
	searchrerank "github.com/use-agent/purify/search/rerank"
	"github.com/use-agent/purify/snapshot"
	"github.com/use-agent/purify/verify/eav"
)

func TestFuseTrustPagesLeadersBeforeDuplicatesAndMismatches(t *testing.T) {
	analysis := consensus.IndependenceAnalysis{
		EffectiveSources: 2,
		Members: []consensus.IndependenceMember{
			{URL: "https://a.alpha.com/1", Representative: "https://a.alpha.com/1", ComponentSize: 2},
			{URL: "https://b.alpha.com/2", Representative: "https://a.alpha.com/1", ComponentSize: 2, ParentURL: "https://a.alpha.com/1", ParentReason: consensus.FoldReasonSameRoot},
			{URL: "https://bravo.net/3", Representative: "https://bravo.net/3", ComponentSize: 1},
		},
	}
	pages := []trustPage{
		{canonicalURL: "https://b.alpha.com/2", providerRank: 1, relevanceScore: 0.9, analyzed: true, member: analysis.Members[1]},
		{canonicalURL: "https://bravo.net/3", providerRank: 2, relevanceScore: 0.4, analyzed: true, member: analysis.Members[2]},
		{canonicalURL: "https://a.alpha.com/1", providerRank: 3, relevanceScore: 0.8, analyzed: true, member: analysis.Members[0]},
		{canonicalURL: "https://other.example/4", providerRank: 4, relevanceScore: 0.95, mismatch: true, verdict: eav.VerdictMismatch, analyzed: true, member: consensus.IndependenceMember{URL: "https://other.example/4", Representative: "https://other.example/4", ComponentSize: 1}},
		{canonicalURL: "https://unknown.example/5", providerRank: 5, relevanceScore: 0.1},
	}
	// include the mismatch in analysis so evaluated >= 2 and it can be last
	analysis.Members = append(analysis.Members, consensus.IndependenceMember{URL: "https://other.example/4", Representative: "https://other.example/4", ComponentSize: 1})
	fusion := fuseTrustPages(pages, analysis)
	if fusion.status != models.SearchRankingApplied || fusion.effectiveSources != 2 {
		t.Fatalf("fusion = %#v", fusion)
	}
	got := make([]string, len(fusion.pages))
	for index, page := range fusion.pages {
		got[index] = page.canonicalURL
	}
	want := []string{
		"https://b.alpha.com/2",
		"https://bravo.net/3",
		"https://unknown.example/5",
		"https://a.alpha.com/1",
		"https://other.example/4",
	}
	if len(got) != len(want) {
		t.Fatalf("order = %v", got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
	if !fusion.pages[0].leader || fusion.pages[3].leader || fusion.pages[0].canonicalURL != "https://b.alpha.com/2" {
		t.Fatalf("leaders = %#v", fusion.pages)
	}
}

func TestSearchTrustFoldsSameRootAndKeepsRelevanceDiagnostics(t *testing.T) {
	provider := &stubSearchProvider{name: "stub", results: []ProviderResult{
		{Rank: 1, URL: "https://one.alpha.com/a", Title: "mirror", Snippet: "Ada wrote notes about engines."},
		{Rank: 2, URL: "https://two.alpha.com/b", Title: "copy", Snippet: "Ada wrote notes about engines."},
		{Rank: 3, URL: "https://bravo.net/c", Title: "other", Snippet: "Ada wrote notes about engines."},
	}}
	artifacts := &trustArtifactStub{pages: map[string]string{
		"https://one.alpha.com/a": "Ada wrote notes about engines. Extra one.",
		"https://two.alpha.com/b": "Ada wrote notes about engines. Extra two.",
		"https://bravo.net/c":     "Ada wrote notes about engines. Extra three.",
	}}
	scorer := identityTrustScorer()
	service, err := newService(provider, timeNowUTC, WithReranker(scorer), WithEnrichment(artifacts, &stubSearchReceiptSigner{token: "receipt"}), WithTrustJudge(staticTrustJudge{verdict: eav.VerdictUncertain}))
	if err != nil {
		t.Fatal(err)
	}
	response, err := service.Search(context.Background(), &models.SearchRequest{
		Query: "Ada engines", Ranking: models.SearchRankingTrust,
		ExpectedSubject: &models.SubjectSpec{Name: "Ada Lovelace"},
	})
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if response.Ranking == nil || response.Ranking.Mode != models.SearchRankingTrust || response.Ranking.Status != models.SearchRankingApplied {
		t.Fatalf("ranking = %#v", response.Ranking)
	}
	if response.Ranking.EvaluatedPages < 2 || response.Ranking.EffectiveSources != 2 {
		t.Fatalf("counters = %#v", response.Ranking)
	}
	if len(response.Results) != 3 {
		t.Fatalf("results = %d", len(response.Results))
	}
	if response.Results[0].Ranking == nil || response.Results[0].Ranking.Independence == nil || !response.Results[0].Ranking.Independence.ComponentLeader {
		t.Fatalf("first result = %#v", response.Results[0].Ranking)
	}
	if response.Results[2].Ranking != nil && response.Results[2].Ranking.Independence != nil && response.Results[2].Ranking.Independence.ComponentLeader {
		t.Fatalf("duplicate projected as leader: %#v", response.Results[2].Ranking)
	}
	if response.Timing.TrustMs == nil || response.Timing.RerankMs == nil {
		t.Fatalf("timing = %#v", response.Timing)
	}
}

func identityTrustScorer() searchrerank.Scorer {
	return searchrerank.ScorerFunc(func(_ context.Context, request searchrerank.ScoreRequest) ([]searchrerank.ScoreResult, error) {
		results := make([]searchrerank.ScoreResult, len(request.Candidates))
		for index, candidate := range request.Candidates {
			results[index] = searchrerank.ScoreResult{StableID: candidate.StableID, RelevanceScore: 1 - float64(candidate.ProviderRank)/100}
		}
		return results, nil
	})
}

type trustArtifactStub struct {
	pages   map[string]string
	fetches int
}

func (stub *trustArtifactStub) FetchPublicArtifact(_ context.Context, rawURL string) (*extract.Artifact, error) {
	stub.fetches++
	content, ok := stub.pages[rawURL]
	if !ok {
		content = "page body"
	}
	return &extract.Artifact{
		Public: &models.ScrapeResponse{FinalURL: rawURL, Content: content},
		Source: &scraper.ScrapeResult{
			FinalURL:   rawURL,
			FetchedAt:  time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC),
			SnapshotID: snapshot.ID("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		},
	}, nil
}

func (stub *trustArtifactStub) ExtractArtifact(context.Context, *extract.Artifact, *models.ExtractRequest) (*models.ExtractResponse, error) {
	return &models.ExtractResponse{Success: true}, nil
}

type staticTrustJudge struct{ verdict eav.Verdict }

func (judge staticTrustJudge) JudgeDocument(context.Context, eav.Subject, eav.Document) (eav.Judgment, error) {
	return eav.Judgment{Verdict: judge.verdict}, nil
}

func timeNowUTC() time.Time { return time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC) }
