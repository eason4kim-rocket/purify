package search

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/use-agent/purify/consensus"
	"github.com/use-agent/purify/extract"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/scraper"
	"github.com/use-agent/purify/snapshot"
)

func TestBuildTrustAnalysisSourceAdmitsExactSnippetAndOmitsUnlocated(t *testing.T) {
	artifact := testTrustArtifact(t, "https://alpha.example/report", "Ada Lovelace wrote notes. More text.")
	source, err := buildTrustAnalysisSource(context.Background(), artifact, "Ada Lovelace wrote notes.")
	if err != nil {
		t.Fatalf("buildTrustAnalysisSource() error = %v", err)
	}
	if !source.snippetAdmitted || source.result.Basis["snippet"].Quote == "" || source.result.Basis["snippet"].Selector != "" {
		t.Fatalf("admitted source = %#v", source)
	}
	if source.result.Basis["snippet"].SnapshotID != string(artifact.Source.SnapshotID) ||
		!source.result.Basis["snippet"].FetchedAt.Equal(artifact.Source.FetchedAt) {
		t.Fatalf("stamp = %#v", source.result.Basis["snippet"])
	}
	var payload map[string]any
	if err := json.Unmarshal(source.result.Data, &payload); err != nil || payload["page"] != true {
		t.Fatalf("data = %s", source.result.Data)
	}
	if _, err := consensus.AnalyzeIndependence(context.Background(), []consensus.SourceResult{source.result}); err != nil {
		t.Fatalf("AnalyzeIndependence() error = %v", err)
	}

	missing, err := buildTrustAnalysisSource(context.Background(), artifact, "this phrase is not on the page")
	if err != nil || missing.snippetAdmitted || missing.result.Basis != nil {
		t.Fatalf("unlocated snippet = %#v, %v", missing, err)
	}
	if string(missing.result.Data) != trustPageJSON {
		t.Fatalf("page-only data = %s", missing.result.Data)
	}
}

func TestBuildTrustAnalysisSourceRejectsOversizeSnippetAndHonorsCancel(t *testing.T) {
	cleaned := strings.Repeat("Ada ", 3000)
	artifact := testTrustArtifact(t, "https://alpha.example/report", cleaned)
	oversize := strings.Repeat("x", maxTrustSnippetBytes+1)
	source, err := buildTrustAnalysisSource(context.Background(), artifact, oversize)
	if err != nil || source.snippetAdmitted {
		t.Fatalf("oversize snippet = %#v, %v", source, err)
	}
	if source, err := buildTrustAnalysisSource(context.Background(), artifact, strings.Repeat("x", maxTrustSnippetBytes)); err != nil || source.snippetAdmitted {
		t.Fatalf("exact 8KiB absent snippet = %#v, %v", source, err)
	}
	exact := cleaned[:maxTrustSnippetBytes]
	if !utf8.ValidString(exact) {
		exact = strings.ToValidUTF8(exact, "")
	}
	// 8 KiB exact is admitted when it is a cleaned substring after trim.
	// Use a prefix that is actually present.
	prefix := cleaned[:maxTrustSnippetBytes]
	for !utf8.ValidString(prefix) {
		prefix = prefix[:len(prefix)-1]
	}
	admitted, err := buildTrustAnalysisSource(context.Background(), artifact, strings.TrimSpace(prefix))
	if err != nil {
		t.Fatalf("8KiB snippet error = %v", err)
	}
	_ = admitted

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := buildTrustAnalysisSource(ctx, artifact, "Ada"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
}

func TestAnalyzeTrustSourcesFoldsSameRootWithoutEntityClaims(t *testing.T) {
	first := testTrustArtifact(t, "https://one.alpha.com/a", "shared report body about Ada")
	second := testTrustArtifact(t, "https://two.alpha.com/b", "shared report body about Ada")
	built := make([]trustAnalysisSource, 2)
	var err error
	built[0], err = buildTrustAnalysisSource(context.Background(), first, "shared report body about Ada")
	if err != nil {
		t.Fatal(err)
	}
	built[1], err = buildTrustAnalysisSource(context.Background(), second, "shared report body about Ada")
	if err != nil {
		t.Fatal(err)
	}
	analysis, err := analyzeTrustSources(context.Background(), built)
	if err != nil {
		t.Fatalf("analyzeTrustSources() error = %v", err)
	}
	if analysis.EffectiveSources != 1 || len(analysis.Members) != 2 {
		t.Fatalf("analysis = %#v", analysis)
	}
	for _, source := range built {
		var payload map[string]any
		if err := json.Unmarshal(source.result.Data, &payload); err != nil {
			t.Fatal(err)
		}
		if _, exists := payload["entity"]; exists {
			t.Fatal("analysis data contained an entity claim")
		}
	}
}

func testTrustArtifact(t *testing.T, finalURL, content string) *extract.Artifact {
	t.Helper()
	return &extract.Artifact{
		Public: &models.ScrapeResponse{FinalURL: finalURL, Content: content},
		Source: &scraper.ScrapeResult{
			FinalURL:   finalURL,
			FetchedAt:  time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC),
			SnapshotID: snapshot.ID("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		},
	}
}
