package search

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/use-agent/purify/consensus"
	"github.com/use-agent/purify/evidence"
	"github.com/use-agent/purify/extract"
	"github.com/use-agent/purify/publicnet"
	"github.com/use-agent/purify/simhash"
)

const (
	maxTrustSnippetBytes  = 8 << 10
	maxTrustSelectorBytes = 4 << 10
	trustPageJSON         = `{"page":true}`
)

var errTrustAnalysisInput = errors.New("search: trust analysis input is invalid")

type trustAnalysisSource struct {
	result          consensus.SourceResult
	snippetAdmitted bool
}

func buildTrustAnalysisSource(ctx context.Context, artifact *extract.Artifact, snippet string) (trustAnalysisSource, error) {
	if ctx == nil {
		return trustAnalysisSource{}, errTrustAnalysisInput
	}
	if err := ctx.Err(); err != nil {
		return trustAnalysisSource{}, err
	}
	if artifact == nil || artifact.Public == nil || artifact.Source == nil ||
		artifact.Source.SnapshotID == "" || artifact.Source.FetchedAt.IsZero() {
		return trustAnalysisSource{}, errTrustAnalysisInput
	}
	rawFinal := strings.TrimSpace(artifact.Source.FinalURL)
	if rawFinal == "" {
		rawFinal = strings.TrimSpace(artifact.Public.FinalURL)
	}
	finalURL, _, err := publicnet.NormalizeHTTPURL(rawFinal, nil, false)
	if err != nil || len(finalURL) > consensus.MaxURLBytes {
		return trustAnalysisSource{}, errTrustAnalysisInput
	}
	cleaned := artifact.Public.Content
	if len(cleaned) > consensus.MaxCleanedTextBytes || !utf8.ValidString(cleaned) {
		return trustAnalysisSource{}, errTrustAnalysisInput
	}

	data := json.RawMessage(trustPageJSON)
	source := trustAnalysisSource{result: consensus.SourceResult{
		URL:         finalURL,
		Data:        append(json.RawMessage(nil), data...),
		SimText:     simhash.Fingerprint(cleaned),
		CleanedText: cleaned,
	}}
	admitted, snippetData, basis, err := admitTrustSnippet(ctx, snippet, cleaned, string(artifact.Source.SnapshotID), artifact.Source.FetchedAt)
	if err != nil {
		return trustAnalysisSource{}, err
	}
	if admitted {
		source.snippetAdmitted = true
		source.result.Data = snippetData
		source.result.Basis = basis
	}
	return source, nil
}

func admitTrustSnippet(ctx context.Context, snippet, cleaned, snapshotID string, fetchedAt time.Time) (bool, json.RawMessage, map[string]evidence.Anchor, error) {
	if err := ctx.Err(); err != nil {
		return false, nil, nil, err
	}
	if snippet == "" || len(snippet) > maxTrustSnippetBytes || !utf8.ValidString(snippet) || strings.TrimSpace(snippet) != snippet {
		return false, nil, nil, nil
	}
	anchor, err := evidence.AlignValueContext(ctx, snippet, cleaned, "")
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false, nil, nil, err
		}
		return false, nil, nil, nil
	}
	if !validAdmittedTrustAnchor(anchor) {
		return false, nil, nil, nil
	}
	encoded, err := json.Marshal(map[string]any{"page": true, "snippet": snippet})
	if err != nil {
		return false, nil, nil, nil
	}
	admitted := anchor
	admitted.Selector = ""
	admitted.SnapshotID = snapshotID
	admitted.FetchedAt = fetchedAt
	return true, append(json.RawMessage(nil), encoded...), map[string]evidence.Anchor{"snippet": admitted}, nil
}

func validAdmittedTrustAnchor(anchor evidence.Anchor) bool {
	if anchor.Method == evidence.MethodUnlocated || anchor.Quote == "" ||
		anchor.TextRange[1] <= anchor.TextRange[0] ||
		len(anchor.Selector) > maxTrustSelectorBytes {
		return false
	}
	switch anchor.Method {
	case evidence.MethodExact, evidence.MethodNormalized, evidence.MethodFuzzy, evidence.MethodCompiled:
		return true
	default:
		return false
	}
}

func analyzeTrustSources(ctx context.Context, sources []trustAnalysisSource) (consensus.IndependenceAnalysis, error) {
	results := make([]consensus.SourceResult, len(sources))
	for index, source := range sources {
		results[index] = source.result
	}
	return consensus.AnalyzeIndependence(ctx, results)
}
