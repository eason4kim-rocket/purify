package rerankeval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/use-agent/purify/publicnet"
	"github.com/use-agent/purify/search/rerank"
)

const (
	// HarvestBaselineSeedOrder is the only baseline this harvest may emit.
	// Rank 1..N is the seed file order, not a search-provider ranking.
	HarvestBaselineSeedOrder = "seed_order"

	harvestSeedSchemaVersion = "rerank-harvest-seeds-v1"
	maxHarvestSeedBytes      = MaxJSONLFileBytes
	maxHarvestPages          = rerank.MaxCandidates
	minHarvestPages          = MinimumCaseCandidates
)

var (
	// ErrInvalidHarvest is returned when seeds or fetched snapshots cannot
	// become a licensed, provider-free construction corpus.
	ErrInvalidHarvest = errors.New("rerankeval: invalid harvest")

	searchProviderHostSuffixes = [...]string{
		"search.brave.com",
		"brave.com",
		"google.com",
		"google.com.hk",
		"googleapis.com",
		"gstatic.com",
		"bing.com",
		"microsoft.com",
		"tavily.com",
		"exa.ai",
		"serper.dev",
		"serpapi.com",
		"searchapi.io",
		"duckduckgo.com",
		"yandex.com",
		"yandex.ru",
		"baidu.com",
		"sogou.com",
	}
)

// HarvestSeedFile is the operator-owned source for a seed-order corpus.
// It never contains search-provider results.
type HarvestSeedFile struct {
	SchemaVersion string
	Baseline      string
	License       string
	Cases         []HarvestSeedCase
}

// HarvestSeedCase is one construction query and its ordered public pages.
type HarvestSeedCase struct {
	CaseID   string
	Query    string
	Language string
	Bucket   string
	Pages    []HarvestSeedPage
}

// HarvestSeedPage is one already-canonical public URL and the page's own
// title and snippet. Fetchers may replace title and snippet; they may not
// introduce a search-provider rank.
type HarvestSeedPage struct {
	URL     string
	Title   string
	Snippet string
}

// PageSnapshot is the title and snippet taken from a page itself.
type PageSnapshot struct {
	Title   string
	Snippet string
}

// PageFetcher returns one page's own title and snippet. A nil fetcher uses
// the title and snippet already stored on the seed page.
type PageFetcher func(context.Context, string) (PageSnapshot, error)

type harvestSeedFileJSON struct {
	SchemaVersion string                `json:"schema_version"`
	Baseline      string                `json:"baseline"`
	License       string                `json:"license"`
	Cases         []harvestSeedCaseJSON `json:"cases"`
}

type harvestSeedCaseJSON struct {
	CaseID   string                `json:"case_id"`
	Query    string                `json:"query"`
	Language string                `json:"language"`
	Bucket   string                `json:"bucket"`
	Pages    []harvestSeedPageJSON `json:"pages"`
}

type harvestSeedPageJSON struct {
	URL     string `json:"url"`
	Title   string `json:"title"`
	Snippet string `json:"snippet"`
}

type harvestCaseRow struct {
	CaseID   string `json:"case_id"`
	Query    string `json:"query"`
	Language string `json:"language"`
	Bucket   string `json:"bucket"`
}

type harvestDocumentRow struct {
	CaseID     string                     `json:"case_id"`
	Query      string                     `json:"query"`
	Candidates []harvestDocumentCandidate `json:"candidates"`
}

type harvestDocumentCandidate struct {
	CandidateID  string `json:"candidate_id"`
	URL          string `json:"url"`
	ProviderRank int    `json:"provider_rank"`
	Title        string `json:"title"`
	Snippet      string `json:"snippet"`
}

// LoadHarvestSeeds reads one strict seed file. It rejects search-provider
// hosts before any fetch or emit.
func LoadHarvestSeeds(path string) (HarvestSeedFile, error) {
	if path == "" {
		return HarvestSeedFile{}, ErrInvalidHarvest
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return HarvestSeedFile{}, err
	}
	return DecodeHarvestSeeds(raw)
}

// DecodeHarvestSeeds strictly decodes one seed file from memory.
func DecodeHarvestSeeds(raw []byte) (HarvestSeedFile, error) {
	if len(raw) == 0 || len(raw) > maxHarvestSeedBytes || !utf8.Valid(raw) || bytes.Contains(raw, []byte{0xef, 0xbb, 0xbf}) {
		return HarvestSeedFile{}, ErrInvalidHarvest
	}
	if err := validateJSON(raw); err != nil {
		return HarvestSeedFile{}, fmt.Errorf("%w: seed JSON", ErrInvalidHarvest)
	}
	fields, err := exactObject(raw, []string{"schema_version", "baseline", "license", "cases"}, nil)
	if err != nil {
		return HarvestSeedFile{}, fmt.Errorf("%w: seed fields", ErrInvalidHarvest)
	}
	schema, schemaErr := jsonString(fields["schema_version"])
	baseline, baselineErr := jsonString(fields["baseline"])
	license, licenseErr := jsonString(fields["license"])
	if schemaErr != nil || baselineErr != nil || licenseErr != nil ||
		schema != harvestSeedSchemaVersion || baseline != HarvestBaselineSeedOrder ||
		strings.TrimSpace(license) == "" || strings.TrimSpace(license) != license {
		return HarvestSeedFile{}, ErrInvalidHarvest
	}
	rawCases, err := rawArray(fields["cases"], 1, 256)
	if err != nil {
		return HarvestSeedFile{}, fmt.Errorf("%w: seed cases", ErrInvalidHarvest)
	}
	file := HarvestSeedFile{
		SchemaVersion: schema, Baseline: baseline, License: strings.Clone(license),
		Cases: make([]HarvestSeedCase, len(rawCases)),
	}
	seenIDs := make(map[string]struct{}, len(rawCases))
	seenQueries := make(map[string]struct{}, len(rawCases))
	for index, rawCase := range rawCases {
		row, parseErr := parseHarvestSeedCase(rawCase)
		if parseErr != nil {
			return HarvestSeedFile{}, fmt.Errorf("case %d: %w", index, parseErr)
		}
		if _, duplicate := seenIDs[row.CaseID]; duplicate {
			return HarvestSeedFile{}, fmt.Errorf("%w: duplicate case %q", ErrInvalidHarvest, row.CaseID)
		}
		if _, duplicate := seenQueries[row.Query]; duplicate {
			return HarvestSeedFile{}, fmt.Errorf("%w: duplicate query %q", ErrInvalidHarvest, row.Query)
		}
		seenIDs[row.CaseID] = struct{}{}
		seenQueries[row.Query] = struct{}{}
		file.Cases[index] = row
	}
	return file, nil
}

// EmitHarvest writes strict cases.jsonl and docs.jsonl. Provider rank is the
// seed page order. A nil fetcher uses the seed snapshots; a non-nil fetcher
// replaces every title and snippet from the page itself.
func EmitHarvest(ctx context.Context, seeds HarvestSeedFile, fetcher PageFetcher) (cases []byte, docs []byte, err error) {
	if ctx == nil {
		return nil, nil, ErrInvalidHarvest
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if seeds.SchemaVersion != harvestSeedSchemaVersion || seeds.Baseline != HarvestBaselineSeedOrder ||
		strings.TrimSpace(seeds.License) == "" || len(seeds.Cases) == 0 {
		return nil, nil, ErrInvalidHarvest
	}

	caseIDs := make([]string, 0, len(seeds.Cases))
	caseRows := make(map[string]harvestCaseRow, len(seeds.Cases))
	documentRows := make(map[string]harvestDocumentRow, len(seeds.Cases))
	for _, seed := range seeds.Cases {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		document, emitErr := emitHarvestDocument(ctx, seed, fetcher)
		if emitErr != nil {
			return nil, nil, fmt.Errorf("case %q: %w", seed.CaseID, emitErr)
		}
		caseRows[seed.CaseID] = harvestCaseRow{
			CaseID: seed.CaseID, Query: seed.Query, Language: seed.Language, Bucket: seed.Bucket,
		}
		documentRows[seed.CaseID] = document
		caseIDs = append(caseIDs, seed.CaseID)
	}
	sort.Strings(caseIDs)

	encodedCases := make([][]byte, 0, len(caseIDs))
	encodedDocs := make([][]byte, 0, len(caseIDs))
	for _, caseID := range caseIDs {
		caseLine, marshalErr := json.Marshal(caseRows[caseID])
		if marshalErr != nil {
			return nil, nil, fmt.Errorf("%w: encode case %q", ErrInvalidHarvest, caseID)
		}
		docLine, marshalErr := json.Marshal(documentRows[caseID])
		if marshalErr != nil {
			return nil, nil, fmt.Errorf("%w: encode document %q", ErrInvalidHarvest, caseID)
		}
		encodedCases = append(encodedCases, caseLine)
		encodedDocs = append(encodedDocs, docLine)
	}
	casesOutput, err := encodeStrictJSONLines(encodedCases)
	if err != nil {
		return nil, nil, err
	}
	docsOutput, err := encodeStrictJSONLines(encodedDocs)
	if err != nil {
		return nil, nil, err
	}
	return casesOutput, docsOutput, nil
}

func parseHarvestSeedCase(raw []byte) (HarvestSeedCase, error) {
	fields, err := exactObject(raw, []string{"case_id", "query", "language", "bucket", "pages"}, nil)
	if err != nil {
		return HarvestSeedCase{}, ErrInvalidHarvest
	}
	id, idErr := fixtureID(fields["case_id"])
	query, queryErr := jsonString(fields["query"])
	language, languageErr := jsonString(fields["language"])
	bucket, bucketErr := jsonString(fields["bucket"])
	if idErr != nil || queryErr != nil || languageErr != nil || bucketErr != nil ||
		!validLanguage(language) || !validBucket(bucket) {
		return HarvestSeedCase{}, ErrInvalidHarvest
	}
	if err := validateHarvestQuery(query); err != nil {
		return HarvestSeedCase{}, err
	}
	rawPages, err := rawArray(fields["pages"], minHarvestPages, maxHarvestPages)
	if err != nil {
		return HarvestSeedCase{}, ErrInvalidHarvest
	}
	row := HarvestSeedCase{
		CaseID: id, Query: query, Language: language, Bucket: bucket,
		Pages: make([]HarvestSeedPage, len(rawPages)),
	}
	seenURLs := make(map[string]struct{}, len(rawPages))
	for index, rawPage := range rawPages {
		page, parseErr := parseHarvestSeedPage(rawPage)
		if parseErr != nil {
			return HarvestSeedCase{}, fmt.Errorf("page %d: %w", index, parseErr)
		}
		if _, duplicate := seenURLs[page.URL]; duplicate {
			return HarvestSeedCase{}, fmt.Errorf("%w: duplicate URL", ErrInvalidHarvest)
		}
		seenURLs[page.URL] = struct{}{}
		row.Pages[index] = page
	}
	return row, nil
}

func parseHarvestSeedPage(raw []byte) (HarvestSeedPage, error) {
	fields, err := exactObject(raw, []string{"url", "title", "snippet"}, nil)
	if err != nil {
		return HarvestSeedPage{}, ErrInvalidHarvest
	}
	rawURL, urlErr := jsonString(fields["url"])
	title, titleErr := jsonString(fields["title"])
	snippet, snippetErr := jsonString(fields["snippet"])
	if urlErr != nil || titleErr != nil || snippetErr != nil {
		return HarvestSeedPage{}, ErrInvalidHarvest
	}
	canonical, err := canonicalPublicPageURL(rawURL)
	if err != nil {
		return HarvestSeedPage{}, err
	}
	if err := validateHarvestSnapshot(title, snippet); err != nil {
		return HarvestSeedPage{}, err
	}
	return HarvestSeedPage{URL: canonical, Title: title, Snippet: snippet}, nil
}

func emitHarvestDocument(ctx context.Context, seed HarvestSeedCase, fetcher PageFetcher) (harvestDocumentRow, error) {
	if !validLanguage(seed.Language) || !validBucket(seed.Bucket) ||
		len(seed.Pages) < minHarvestPages || len(seed.Pages) > maxHarvestPages {
		return harvestDocumentRow{}, ErrInvalidHarvest
	}
	if err := validateHarvestQuery(seed.Query); err != nil {
		return harvestDocumentRow{}, err
	}
	row := harvestDocumentRow{
		CaseID: seed.CaseID, Query: seed.Query,
		Candidates: make([]harvestDocumentCandidate, len(seed.Pages)),
	}
	candidates := make([]rerank.Candidate, len(seed.Pages))
	seenURLs := make(map[string]struct{}, len(seed.Pages))
	for index, page := range seed.Pages {
		if err := ctx.Err(); err != nil {
			return harvestDocumentRow{}, err
		}
		canonical, err := canonicalPublicPageURL(page.URL)
		if err != nil {
			return harvestDocumentRow{}, err
		}
		if _, duplicate := seenURLs[canonical]; duplicate {
			return harvestDocumentRow{}, fmt.Errorf("%w: duplicate URL", ErrInvalidHarvest)
		}
		seenURLs[canonical] = struct{}{}
		snapshot := PageSnapshot{Title: page.Title, Snippet: page.Snippet}
		if fetcher != nil {
			fetched, fetchErr := fetcher(ctx, canonical)
			if fetchErr != nil {
				return harvestDocumentRow{}, fetchErr
			}
			snapshot = fetched
		}
		if err := validateHarvestSnapshot(snapshot.Title, snapshot.Snippet); err != nil {
			return harvestDocumentRow{}, err
		}
		candidateID, err := rerank.CandidateID(canonical)
		if err != nil {
			return harvestDocumentRow{}, ErrInvalidHarvest
		}
		rank := index + 1
		row.Candidates[index] = harvestDocumentCandidate{
			CandidateID: candidateID, URL: canonical, ProviderRank: rank,
			Title: snapshot.Title, Snippet: snapshot.Snippet,
		}
		candidates[index] = rerank.Candidate{
			CanonicalURL: canonical, ProviderRank: rank,
			Title: snapshot.Title, Snippet: snapshot.Snippet,
		}
	}
	if _, err := rerank.BuildRequest(seed.Query, candidates); err != nil {
		return harvestDocumentRow{}, fmt.Errorf("%w: production request builder rejected harvest", ErrInvalidHarvest)
	}
	return row, nil
}

func canonicalPublicPageURL(rawURL string) (string, error) {
	if rawURL == "" || strings.TrimSpace(rawURL) != rawURL {
		return "", ErrInvalidHarvest
	}
	canonical, parsed, err := publicnet.NormalizeHTTPURL(rawURL, nil, false)
	if err != nil || parsed == nil || canonical != rawURL {
		return "", ErrInvalidHarvest
	}
	if parsed.Scheme != "https" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", ErrInvalidHarvest
	}
	if searchProviderHost(parsed.Hostname()) || searchProviderPath(parsed.Path) {
		return "", fmt.Errorf("%w: search provider URL", ErrInvalidHarvest)
	}
	if _, err := rerank.CandidateID(canonical); err != nil {
		return "", ErrInvalidHarvest
	}
	return canonical, nil
}

func searchProviderHost(hostname string) bool {
	host := strings.ToLower(strings.TrimSuffix(hostname, "."))
	if host == "" {
		return true
	}
	for _, suffix := range searchProviderHostSuffixes {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

func searchProviderPath(path string) bool {
	lower := strings.ToLower(path)
	return strings.Contains(lower, "/search") || strings.Contains(lower, "/serp") ||
		strings.Contains(lower, "/results")
}

func validateHarvestQuery(query string) error {
	if err := rerankQueryOnly(query); err != nil {
		return ErrInvalidHarvest
	}
	return nil
}

func rerankQueryOnly(query string) error {
	if len(query) == 0 || !utf8.ValidString(query) || strings.TrimSpace(query) != query {
		return ErrInvalidHarvest
	}
	for _, character := range query {
		if unicode.IsControl(character) {
			return ErrInvalidHarvest
		}
	}
	if utf8.RuneCountInString(query) > rerank.MaxQueryRunes {
		return ErrInvalidHarvest
	}
	words := strings.Fields(query)
	if len(words) == 0 || len(words) > rerank.MaxQueryWords || strings.Join(words, " ") != query {
		return ErrInvalidHarvest
	}
	return nil
}

func validateHarvestSnapshot(title, snippet string) error {
	if title == "" || snippet == "" ||
		len(title) > rerank.MaxRawTitleBytes || len(snippet) > rerank.MaxRawSnippetBytes {
		return ErrInvalidHarvest
	}
	if !utf8.ValidString(title) || !utf8.ValidString(snippet) ||
		strings.TrimSpace(title) != title || strings.TrimSpace(snippet) != snippet {
		return ErrInvalidHarvest
	}
	for _, value := range []string{title, snippet} {
		for _, character := range value {
			if unicode.IsControl(character) {
				return ErrInvalidHarvest
			}
		}
	}
	return nil
}
