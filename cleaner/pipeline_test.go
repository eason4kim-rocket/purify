package cleaner

import (
	"errors"
	"html"
	"slices"
	"strings"
	"testing"

	readability "github.com/go-shiori/go-readability"

	"github.com/use-agent/purify/models"
)

type extractionFixture struct {
	readable       readability.Article
	readErr        error
	prunedHTML     string
	pruneExtracted bool
	pruneErr       error
	calls          []string
}

func (f *extractionFixture) cleaner() *Cleaner {
	c := NewCleaner()
	c.extractReadability = func(_, _ string) (readability.Article, error) {
		f.calls = append(f.calls, "readability")
		return f.readable, f.readErr
	}
	c.extractPruning = func(_, _ string) (string, bool, error) {
		f.calls = append(f.calls, "pruning")
		return f.prunedHTML, f.pruneExtracted, f.pruneErr
	}
	return c
}

func TestAdaptiveExtractionChains(t *testing.T) {
	readableHigh := articleForTest("readability-high", repeatedForTest("readable", 60))
	readableMedium := articleForTest("readability-medium", repeatedForTest("readable", 30))
	readableLow := articleForTest("readability-low", repeatedForTest("tiny", 2))
	prunedHigh := htmlForTest("pruning-high", repeatedForTest("pruned", 60))
	prunedMedium := htmlForTest("pruning-medium", repeatedForTest("pruned", 30))
	prunedLow := htmlForTest("pruning-low", repeatedForTest("tiny", 2))
	rawHTML := metadataHTML(repeatedForTest("raw", 60))

	tests := []struct {
		name           string
		mode           string
		readable       readability.Article
		readErr        error
		prunedHTML     string
		pruneExtracted bool
		pruneErr       error
		wantMode       string
		wantContent    string
		wantCalls      []string
	}{
		{
			name: "default selects usable readability without touching pruning",
			mode: "", readable: readableHigh, prunedHTML: prunedHigh, pruneExtracted: true,
			wantMode: "readability", wantContent: readableHigh.Content, wantCalls: []string{"readability"},
		},
		{
			name: "explicit readability selects usable readability",
			mode: "readability", readable: readableHigh, prunedHTML: prunedHigh, pruneExtracted: true,
			wantMode: "readability", wantContent: readableHigh.Content, wantCalls: []string{"readability"},
		},
		{
			name: "readability low quality falls through to pruning",
			mode: "readability", readable: readableLow, prunedHTML: prunedHigh, pruneExtracted: true,
			wantMode: "pruning", wantContent: prunedHigh, wantCalls: []string{"readability", "pruning"},
		},
		{
			name: "readability extraction error falls through to pruning",
			mode: "readability", readErr: errors.New("readability failed"), prunedHTML: prunedHigh, pruneExtracted: true,
			wantMode: "pruning", wantContent: prunedHigh, wantCalls: []string{"readability", "pruning"},
		},
		{
			name: "readability and pruning low quality fall through to raw",
			mode: "readability", readable: readableLow, prunedHTML: prunedLow, pruneExtracted: true,
			wantMode: "raw", wantContent: rawHTML, wantCalls: []string{"readability", "pruning"},
		},
		{
			name: "auto selects higher readability quality",
			mode: "auto", readable: readableHigh, prunedHTML: prunedMedium, pruneExtracted: true,
			wantMode: "readability", wantContent: readableHigh.Content, wantCalls: []string{"readability", "pruning"},
		},
		{
			name: "auto selects higher pruning quality",
			mode: "auto", readable: readableMedium, prunedHTML: prunedHigh, pruneExtracted: true,
			wantMode: "pruning", wantContent: prunedHigh, wantCalls: []string{"readability", "pruning"},
		},
		{
			name: "auto breaks exact quality tie in favor of readability",
			mode: "auto", readable: readableMedium, prunedHTML: prunedMedium, pruneExtracted: true,
			wantMode: "readability", wantContent: readableMedium.Content, wantCalls: []string{"readability", "pruning"},
		},
		{
			name: "auto uses sole usable pruning candidate",
			mode: "auto", readable: readableLow, prunedHTML: prunedHigh, pruneExtracted: true,
			wantMode: "pruning", wantContent: prunedHigh, wantCalls: []string{"readability", "pruning"},
		},
		{
			name: "auto uses raw only when both candidates are unusable",
			mode: "auto", readable: readableLow, prunedHTML: prunedLow, pruneExtracted: true,
			wantMode: "raw", wantContent: rawHTML, wantCalls: []string{"readability", "pruning"},
		},
		{
			name:       "auto uses quality rather than longer text",
			mode:       "auto",
			readable:   articleForTest("long-noise", strings.Repeat("----- ", 50)),
			prunedHTML: htmlForTest("short-signal", repeatedForTest("signal", 12)), pruneExtracted: true,
			wantMode: "pruning", wantContent: htmlForTest("short-signal", repeatedForTest("signal", 12)), wantCalls: []string{"readability", "pruning"},
		},
		{
			name: "pruning selects usable pruning without readability",
			mode: "pruning", readable: readableHigh, prunedHTML: prunedHigh, pruneExtracted: true,
			wantMode: "pruning", wantContent: prunedHigh, wantCalls: []string{"pruning"},
		},
		{
			name: "pruning low quality falls through to raw",
			mode: "pruning", readable: readableHigh, prunedHTML: prunedLow, pruneExtracted: true,
			wantMode: "raw", wantContent: rawHTML, wantCalls: []string{"pruning"},
		},
		{
			name: "pruning internal raw fallback is reported as raw",
			mode: "pruning", readable: readableHigh, prunedHTML: rawHTML, pruneExtracted: false,
			wantMode: "raw", wantContent: rawHTML, wantCalls: []string{"pruning"},
		},
		{
			name: "raw never invokes another extractor",
			mode: "raw", readable: readableHigh, readErr: errors.New("must not run"), prunedHTML: prunedHigh, pruneExtracted: true, pruneErr: errors.New("must not run"),
			wantMode: "raw", wantContent: rawHTML, wantCalls: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := &extractionFixture{
				readable:       tt.readable,
				readErr:        tt.readErr,
				prunedHTML:     tt.prunedHTML,
				pruneExtracted: tt.pruneExtracted,
				pruneErr:       tt.pruneErr,
			}
			response, err := fixture.cleaner().Clean(rawHTML, "https://example.test/final/page", "html", tt.mode)
			if err != nil {
				t.Fatalf("Clean() error = %v", err)
			}
			if response.Content != tt.wantContent {
				t.Fatalf("content = %q, want %q", response.Content, tt.wantContent)
			}
			if response.Quality == nil {
				t.Fatal("quality = nil")
			}
			if response.Quality.ExtractModeUsed != tt.wantMode {
				t.Fatalf("extract_mode_used = %q, want %q", response.Quality.ExtractModeUsed, tt.wantMode)
			}
			if response.Quality.Status == models.QualityStatusUnusable {
				t.Fatalf("selected quality = %#v, want usable", response.Quality)
			}
			if response.Quality.Warnings == nil {
				t.Fatal("quality warnings must be an explicit empty slice")
			}
			if !slices.Equal(fixture.calls, tt.wantCalls) {
				t.Fatalf("extractor calls = %#v, want %#v", fixture.calls, tt.wantCalls)
			}
		})
	}
}

func TestOutputFormatsUseSameSelectedCandidateAndFinalURL(t *testing.T) {
	finalURL := "https://example.test/redirected/page/"
	readableText := repeatedForTest("formatted", 40)
	readable := readability.Article{
		Title:       "Readability title",
		Byline:      "Readability author",
		Excerpt:     "Readability description",
		SiteName:    "Readability site",
		Language:    "en",
		Content:     `<article><p>` + html.EscapeString(readableText) + `</p><a href="guide">Guide</a></article>`,
		TextContent: readableText + " Guide",
	}
	rawHTML := `<html lang="en"><head><title>Raw title</title></head><body>` +
		`<main>` + html.EscapeString(repeatedForTest("raw", 40)) + `</main>` +
		`<a href="guide">Raw guide</a><img src="images/pic.png" alt="Picture"></body></html>`

	tests := []struct {
		name       string
		format     string
		wantExact  string
		contains   []string
		notContain []string
	}{
		{name: "default markdown", format: "", contains: []string{"formatted", "https://example.test/redirected/page/guide"}},
		{name: "markdown", format: "markdown", contains: []string{"formatted", "https://example.test/redirected/page/guide"}},
		{name: "html", format: "html", wantExact: readable.Content},
		{name: "text", format: "text", wantExact: readable.TextContent, notContain: []string{"<article>"}},
		{name: "citations", format: "markdown_citations", contains: []string{"[Guide][1]", "[1]: https://example.test/redirected/page/guide"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := &extractionFixture{readable: readable}
			response, err := fixture.cleaner().Clean(rawHTML, finalURL, tt.format, "readability")
			if err != nil {
				t.Fatalf("Clean() error = %v", err)
			}
			if tt.wantExact != "" && response.Content != tt.wantExact {
				t.Fatalf("content = %q, want exact %q", response.Content, tt.wantExact)
			}
			for _, expected := range tt.contains {
				if !strings.Contains(response.Content, expected) {
					t.Fatalf("content %q missing %q", response.Content, expected)
				}
			}
			for _, unexpected := range tt.notContain {
				if strings.Contains(response.Content, unexpected) {
					t.Fatalf("content %q unexpectedly contains %q", response.Content, unexpected)
				}
			}
			if response.Quality == nil || response.Quality.ExtractModeUsed != "readability" {
				t.Fatalf("quality = %#v, want readability", response.Quality)
			}
			if response.Metadata.SourceURL != finalURL {
				t.Fatalf("source_url = %q, want %q", response.Metadata.SourceURL, finalURL)
			}
			if len(response.Links.Internal) != 1 || response.Links.Internal[0].Href != "https://example.test/redirected/page/guide" {
				t.Fatalf("internal links = %#v", response.Links.Internal)
			}
			if len(response.Images) != 1 || response.Images[0].Src != "https://example.test/redirected/page/images/pic.png" {
				t.Fatalf("images = %#v", response.Images)
			}
		})
	}
}

func TestExplicitRawHTMLIsUnchanged(t *testing.T) {
	rawHTML := `<html><head><title>Raw</title><script>ignored()</script></head><body><nav>Keep nav</nav><main>` +
		html.EscapeString(repeatedForTest("raw", 40)) + `</main></body></html>`
	fixture := &extractionFixture{
		readErr:        errors.New("readability must not run"),
		pruneErr:       errors.New("pruning must not run"),
		pruneExtracted: true,
	}

	response, err := fixture.cleaner().Clean(rawHTML, "https://example.test/final", "html", "raw")
	if err != nil {
		t.Fatalf("Clean() error = %v", err)
	}
	if response.Content != rawHTML {
		t.Fatalf("raw HTML changed:\n got: %q\nwant: %q", response.Content, rawHTML)
	}
	if response.Quality == nil || response.Quality.ExtractModeUsed != "raw" {
		t.Fatalf("quality = %#v, want raw", response.Quality)
	}
	if len(fixture.calls) != 0 {
		t.Fatalf("raw mode invoked extractors: %#v", fixture.calls)
	}

	textResponse, err := fixture.cleaner().Clean(rawHTML, "https://example.test/final", "text", "raw")
	if err != nil {
		t.Fatalf("Clean(text) error = %v", err)
	}
	if strings.Contains(textResponse.Content, "ignored()") || !strings.Contains(textResponse.Content, "Keep nav") {
		t.Fatalf("raw text conversion = %q", textResponse.Content)
	}
}

func TestCJKContentUsesRuneAwareQuality(t *testing.T) {
	content := strings.Repeat("可靠内容", 50)
	fixture := &extractionFixture{readable: articleForTest("cjk", content)}
	response, err := fixture.cleaner().Clean(metadataHTML(repeatedForTest("raw", 40)), "https://example.test/最终", "text", "readability")
	if err != nil {
		t.Fatalf("Clean() error = %v", err)
	}
	if response.Quality == nil || response.Quality.ExtractModeUsed != "readability" {
		t.Fatalf("quality = %#v", response.Quality)
	}
	if response.Quality.Status != models.QualityStatusGood || response.Quality.Score != 0.90 {
		t.Fatalf("CJK quality = %#v, want score 0.90 good", response.Quality)
	}
	if response.Content != content {
		t.Fatalf("content = %q, want CJK source text", response.Content)
	}
}

func TestMetadataSurvivesAdaptiveFallback(t *testing.T) {
	rawHTML := metadataHTML(repeatedForTest("raw", 50))
	readabilityMetadata := readability.Article{
		Title:       "Readability title",
		Byline:      "Readability author",
		Excerpt:     "Readability description",
		SiteName:    "Readability site",
		Language:    "fr",
		Content:     `<article>tiny</article>`,
		TextContent: "tiny",
	}
	pruned := htmlForTest("pruned", repeatedForTest("pruned", 50))

	tests := []struct {
		name         string
		mode         string
		readable     readability.Article
		wantMode     string
		wantMetadata models.Metadata
	}{
		{
			name: "readability metadata accompanies pruning fallback", mode: "readability", readable: readabilityMetadata, wantMode: "pruning",
			wantMetadata: models.Metadata{Title: "Readability title", Author: "Readability author", Description: "Readability description", SiteName: "Readability site", Language: "fr"},
		},
		{
			name: "explicit pruning uses document metadata without running readability", mode: "pruning", wantMode: "pruning",
			wantMetadata: models.Metadata{Title: "OG raw title", Author: "Raw author", Description: "Raw description", SiteName: "Raw site", Language: "zh-CN"},
		},
		{
			name: "explicit raw uses document metadata without running readability", mode: "raw", wantMode: "raw",
			wantMetadata: models.Metadata{Title: "OG raw title", Author: "Raw author", Description: "Raw description", SiteName: "Raw site", Language: "zh-CN"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := &extractionFixture{readable: tt.readable, prunedHTML: pruned, pruneExtracted: true}
			finalURL := "https://example.test/final/path"
			response, err := fixture.cleaner().Clean(rawHTML, finalURL, "html", tt.mode)
			if err != nil {
				t.Fatalf("Clean() error = %v", err)
			}
			if response.Quality == nil || response.Quality.ExtractModeUsed != tt.wantMode {
				t.Fatalf("quality = %#v, want mode %q", response.Quality, tt.wantMode)
			}
			want := tt.wantMetadata
			want.SourceURL = finalURL
			if response.Metadata != want {
				t.Fatalf("metadata = %#v, want %#v", response.Metadata, want)
			}
			if tt.mode == "pruning" || tt.mode == "raw" {
				for _, call := range fixture.calls {
					if call == "readability" {
						t.Fatalf("explicit %s invoked readability for metadata", tt.mode)
					}
				}
			}
		})
	}
}

func articleForTest(marker, text string) readability.Article {
	return readability.Article{
		Content:     htmlForTest(marker, text),
		TextContent: text,
	}
}

func htmlForTest(marker, text string) string {
	return `<article data-marker="` + marker + `"><p>` + html.EscapeString(text) + `</p></article>`
}

func repeatedForTest(token string, count int) string {
	return strings.TrimSpace(strings.Repeat(token+" ", count))
}

func metadataHTML(bodyText string) string {
	return `<html lang="zh-CN"><head>` +
		`<title>Raw title</title>` +
		`<meta name="description" content="Raw description">` +
		`<meta name="author" content="Raw author">` +
		`<meta property="og:title" content="OG raw title">` +
		`<meta property="og:site_name" content="Raw site">` +
		`</head><body><main>` + html.EscapeString(bodyText) + `</main></body></html>`
}
