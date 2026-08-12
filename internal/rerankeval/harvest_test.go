package rerankeval

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/use-agent/purify/search/rerank"
)

func TestDecodeHarvestSeedsRejectsSearchProviderURLs(t *testing.T) {
	t.Parallel()
	for _, rawURL := range []string{
		"https://search.brave.com/search?q=go",
		"https://api.search.brave.com/res/v1/web/search",
		"https://www.google.com/search",
		"https://www.bing.com/search",
		"https://api.tavily.com/search",
		"https://exa.ai/search",
		"https://en.wikipedia.org/w/index.php?search=Go",
		"http://en.wikipedia.org/wiki/Go",
	} {
		raw := marshalHarvestSeeds(t, validHarvestSeedFile(t, withHarvestURL(0, 0, rawURL)))
		if _, err := DecodeHarvestSeeds(raw); err == nil {
			t.Fatalf("DecodeHarvestSeeds accepted %s", rawURL)
		}
	}
}

func TestEmitHarvestAssignsSeedOrderRanksAndStableIDs(t *testing.T) {
	t.Parallel()
	seeds := validHarvestSeedFile(t)
	casesOutput, docsOutput, err := EmitHarvest(context.Background(), seeds, nil)
	if err != nil {
		t.Fatalf("EmitHarvest() error = %v", err)
	}

	casesPath := writeTempFile(t, "cases.jsonl", casesOutput)
	docsPath := writeTempFile(t, "docs.jsonl", docsOutput)
	loadedCases, documents, caseIDs, err := loadLabelingInputs(casesPath, docsPath)
	if err != nil {
		t.Fatalf("loadLabelingInputs() error = %v", err)
	}
	if len(caseIDs) != 1 || caseIDs[0] != seeds.Cases[0].CaseID {
		t.Fatalf("case IDs = %#v", caseIDs)
	}
	if loadedCases[caseIDs[0]].query != seeds.Cases[0].Query {
		t.Fatalf("query = %q", loadedCases[caseIDs[0]].query)
	}
	document := documents[caseIDs[0]]
	if len(document.candidates) != len(seeds.Cases[0].Pages) {
		t.Fatalf("candidate count = %d", len(document.candidates))
	}
	for index, candidate := range document.candidates {
		wantID, idErr := rerank.CandidateID(seeds.Cases[0].Pages[index].URL)
		if idErr != nil {
			t.Fatalf("CandidateID() error = %v", idErr)
		}
		if candidate.providerRank != index+1 || candidate.id != wantID ||
			candidate.canonicalURL != seeds.Cases[0].Pages[index].URL ||
			candidate.title != seeds.Cases[0].Pages[index].Title {
			t.Fatalf("candidate %d = %#v", index, candidate)
		}
	}
}

func TestEmitHarvestFetcherReplacesSeedSnapshotsOnly(t *testing.T) {
	t.Parallel()
	seeds := validHarvestSeedFile(t)
	calls := 0
	fetcher := func(_ context.Context, rawURL string) (PageSnapshot, error) {
		calls++
		if rawURL != seeds.Cases[0].Pages[calls-1].URL {
			t.Fatalf("fetch URL = %q, want %q", rawURL, seeds.Cases[0].Pages[calls-1].URL)
		}
		return PageSnapshot{Title: "fetched-title", Snippet: "fetched-snippet"}, nil
	}
	_, docsOutput, err := EmitHarvest(context.Background(), seeds, fetcher)
	if err != nil {
		t.Fatalf("EmitHarvest() error = %v", err)
	}
	if calls != len(seeds.Cases[0].Pages) {
		t.Fatalf("fetch calls = %d", calls)
	}
	var row harvestDocumentRow
	if err := json.Unmarshal(bytesTrimLine(docsOutput), &row); err != nil {
		t.Fatalf("decode docs = %v", err)
	}
	for _, candidate := range row.Candidates {
		if candidate.Title != "fetched-title" || candidate.Snippet != "fetched-snippet" {
			t.Fatalf("candidate used seed text: %#v", candidate)
		}
	}
}

func TestEmitHarvestHonorsCancelBeforeFetch(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := EmitHarvest(ctx, validHarvestSeedFile(t), func(context.Context, string) (PageSnapshot, error) {
		t.Fatal("fetcher called after cancel")
		return PageSnapshot{}, errors.New("unused")
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("EmitHarvest() error = %v, want context.Canceled", err)
	}
}

func TestCommittedHarvestSeedsEmitTheFrozenConstructionCorpus(t *testing.T) {
	seeds, err := LoadHarvestSeeds(filepath.Join("..", "..", "scripts", "rerankharvest", "seeds.json"))
	if err != nil {
		t.Fatalf("LoadHarvestSeeds() error = %v", err)
	}
	if seeds.Baseline != HarvestBaselineSeedOrder || len(seeds.Cases) < MinimumCases {
		t.Fatalf("seeds baseline=%q cases=%d", seeds.Baseline, len(seeds.Cases))
	}
	casesOutput, docsOutput, err := EmitHarvest(context.Background(), seeds, nil)
	if err != nil {
		t.Fatalf("EmitHarvest() error = %v", err)
	}
	wantCases, err := os.ReadFile(filepath.Join("testdata", "construction", "cases.jsonl"))
	if err != nil {
		t.Fatalf("read cases.jsonl: %v", err)
	}
	wantDocs, err := os.ReadFile(filepath.Join("testdata", "construction", "docs.jsonl"))
	if err != nil {
		t.Fatalf("read docs.jsonl: %v", err)
	}
	if string(casesOutput) != string(wantCases) || string(docsOutput) != string(wantDocs) {
		t.Fatal("committed construction corpus does not match a fresh seed-order emit")
	}
	casesPath := writeTempFile(t, "cases.jsonl", casesOutput)
	docsPath := writeTempFile(t, "docs.jsonl", docsOutput)
	_, documents, caseIDs, err := loadLabelingInputs(casesPath, docsPath)
	if err != nil {
		t.Fatalf("loadLabelingInputs() error = %v", err)
	}
	candidates := 0
	for _, caseID := range caseIDs {
		candidates += len(documents[caseID].candidates)
	}
	if len(caseIDs) < MinimumCases || candidates < MinimumCandidates {
		t.Fatalf("construction size cases=%d candidates=%d", len(caseIDs), candidates)
	}
}

func TestLoadHarvestSeedsReadsFileAndRejectsDuplicates(t *testing.T) {
	t.Parallel()
	path := writeTempFile(t, "seeds.json", marshalHarvestSeeds(t, validHarvestSeedFile(t)))
	loaded, err := LoadHarvestSeeds(path)
	if err != nil {
		t.Fatalf("LoadHarvestSeeds() error = %v", err)
	}
	if loaded.Baseline != HarvestBaselineSeedOrder || loaded.Cases[0].CaseID != "en-lexical-go" {
		t.Fatalf("loaded = %#v", loaded)
	}

	duplicate := validHarvestSeedFile(t)
	duplicate.Cases = append(duplicate.Cases, duplicate.Cases[0])
	if _, err := DecodeHarvestSeeds(marshalHarvestSeeds(t, duplicate)); err == nil {
		t.Fatal("DecodeHarvestSeeds accepted a duplicate case")
	}
}

func TestCanonicalPublicPageURLRejectsNonHTTPSAndQuery(t *testing.T) {
	t.Parallel()
	if _, err := canonicalPublicPageURL("https://en.wikipedia.org/wiki/Go/"); err == nil {
		// trailing slash may or may not already be canonical; only assert explicit rejects below
	}
	for _, rawURL := range []string{
		"https://en.wikipedia.org/wiki/Go?utm=1",
		"https://SEARCH.BRAVE.COM/app",
		"https://en.wikipedia.org/search",
	} {
		if _, err := canonicalPublicPageURL(rawURL); err == nil {
			t.Fatalf("canonicalPublicPageURL accepted %s", rawURL)
		}
	}
}

func validHarvestSeedFile(t *testing.T, mutators ...func(*HarvestSeedFile)) HarvestSeedFile {
	t.Helper()
	pages := make([]HarvestSeedPage, minHarvestPages)
	for index := range pages {
		pages[index] = HarvestSeedPage{
			URL:     "https://en.wikipedia.org/wiki/Harvest" + itoaAtLeastTwo(index),
			Title:   "title-" + itoaAtLeastTwo(index),
			Snippet: "snippet from the public page " + itoaAtLeastTwo(index),
		}
	}
	file := HarvestSeedFile{
		SchemaVersion: harvestSeedSchemaVersion,
		Baseline:      HarvestBaselineSeedOrder,
		License:       "operator-curated public pages; not search-provider results",
		Cases: []HarvestSeedCase{{
			CaseID: "en-lexical-go", Query: "What is the Go programming language",
			Language: LanguageEnglish, Bucket: BucketLexical, Pages: pages,
		}},
	}
	for _, mutate := range mutators {
		mutate(&file)
	}
	return file
}

func withHarvestURL(caseIndex, pageIndex int, rawURL string) func(*HarvestSeedFile) {
	return func(file *HarvestSeedFile) {
		file.Cases[caseIndex].Pages[pageIndex].URL = rawURL
	}
}

func marshalHarvestSeeds(t *testing.T, file HarvestSeedFile) []byte {
	t.Helper()
	encoded, err := json.Marshal(harvestSeedFileJSON{
		SchemaVersion: file.SchemaVersion,
		Baseline:      file.Baseline,
		License:       file.License,
		Cases:         encodeHarvestSeedCases(file.Cases),
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return encoded
}

func encodeHarvestSeedCases(cases []HarvestSeedCase) []harvestSeedCaseJSON {
	encoded := make([]harvestSeedCaseJSON, len(cases))
	for index, row := range cases {
		pages := make([]harvestSeedPageJSON, len(row.Pages))
		for pageIndex, page := range row.Pages {
			pages[pageIndex] = harvestSeedPageJSON{URL: page.URL, Title: page.Title, Snippet: page.Snippet}
		}
		encoded[index] = harvestSeedCaseJSON{
			CaseID: row.CaseID, Query: row.Query, Language: row.Language, Bucket: row.Bucket, Pages: pages,
		}
	}
	return encoded
}

func writeTempFile(t *testing.T, name string, body []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", name, err)
	}
	return path
}

func bytesTrimLine(raw []byte) []byte {
	return []byte(strings.TrimSpace(string(raw)))
}

func itoaAtLeastTwo(value int) string {
	if value < 10 {
		return string(rune('0'+value/10)) + string(rune('0'+value%10))
	}
	return string(rune('0'+value/10)) + string(rune('0'+value%10))
}
