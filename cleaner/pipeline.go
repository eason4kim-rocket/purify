package cleaner

import (
	"math"
	"strings"

	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/PuerkitoBio/goquery"
	readability "github.com/go-shiori/go-readability"

	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/quality"
)

// Cleaner orchestrates adaptive extraction and output-format conversion.
// The converter and extraction functions are immutable after construction, so
// a Cleaner can be reused concurrently.
type Cleaner struct {
	mdConverter        *converter.Converter
	extractReadability func(string, string) (readability.Article, error)
	extractPruning     func(string, string) (string, bool, error)
}

// NewCleaner initialises the Cleaner with a pre-configured Markdown converter.
func NewCleaner() *Cleaner {
	return &Cleaner{
		mdConverter:        newMarkdownConverter(),
		extractReadability: extractReadabilityArticle,
		extractPruning:     pruneContentDetailed,
	}
}

// CleanOptions carries optional content-filtering parameters for the pipeline.
type CleanOptions struct {
	IncludeTags []string
	ExcludeTags []string
	CSSSelector string
}

type extractionCandidate struct {
	article    readability.Article
	assessment quality.Assessment
	available  bool
}

// Clean runs the adaptive extraction pipeline and returns a partial
// ScrapeResponse (Timing and transport fields are left to the scrape service).
//
// Extraction order is deliberately independent of output format:
//   - default/readability: readability -> pruning -> raw
//   - auto: score readability and pruning, then use raw only if both are unusable
//   - pruning: pruning -> raw
//   - raw: raw only
//
// sourceURL is the caller-provided final URL. It is used for Readability,
// relative Markdown links, extracted links/images, and response metadata.
func (c *Cleaner) Clean(rawHTML string, sourceURL string, format string, extractMode string, opts ...CleanOptions) (*models.ScrapeResponse, error) {
	originalTokens := EstimateTokens(rawHTML)

	filteredHTML, err := applyCleanOptions(rawHTML, opts)
	if err != nil {
		return nil, err
	}

	selected := c.selectCandidate(filteredHTML, sourceURL, extractMode)
	content, err := c.convertArticle(selected.article, format, sourceURL)
	if err != nil {
		return nil, err
	}

	cleanedTokens := EstimateTokens(content)
	savingsPercent := 0.0
	if originalTokens > 0 {
		savingsPercent = float64(originalTokens-cleanedTokens) / float64(originalTokens) * 100
		savingsPercent = math.Round(savingsPercent*100) / 100
	}

	qualityInfo := selected.assessment.Info
	qualityInfo.Warnings = append([]models.QualityReason(nil), qualityInfo.Warnings...)
	if qualityInfo.Warnings == nil {
		qualityInfo.Warnings = []models.QualityReason{}
	}

	return &models.ScrapeResponse{
		Success: true,
		Content: content,
		Metadata: models.Metadata{
			Title:       selected.article.Title,
			Description: selected.article.Excerpt,
			SiteName:    selected.article.SiteName,
			Author:      selected.article.Byline,
			Language:    selected.article.Language,
			SourceURL:   sourceURL,
		},
		Links:      ExtractLinks(filteredHTML, sourceURL),
		Images:     ExtractImages(filteredHTML, sourceURL),
		OGMetadata: ExtractOGMetadata(filteredHTML),
		Tokens: models.TokenInfo{
			OriginalEstimate: originalTokens,
			CleanedEstimate:  cleanedTokens,
			SavingsPercent:   savingsPercent,
		},
		Quality: &qualityInfo,
		// Timing, StatusCode, and FinalURL are populated by the scrape service.
	}, nil
}

func applyCleanOptions(rawHTML string, opts []CleanOptions) (string, error) {
	if len(opts) == 0 {
		return rawHTML, nil
	}

	o := opts[0]
	if o.CSSSelector != "" {
		filtered, err := ApplyCSSSelector(rawHTML, o.CSSSelector)
		if err != nil {
			return "", models.NewScrapeError(
				models.ErrCodeInvalidInput,
				"invalid CSS selector: "+err.Error(),
				err,
			)
		}
		rawHTML = filtered
	}

	return FilterContent(rawHTML, o.IncludeTags, o.ExcludeTags), nil
}

func (c *Cleaner) selectCandidate(rawHTML, sourceURL, extractMode string) extractionCandidate {
	metadata := extractDocumentMetadata(rawHTML)

	var (
		readabilityCandidate extractionCandidate
		readabilityArticle   readability.Article
		readabilityCalled    bool
	)
	getReadability := func() extractionCandidate {
		if readabilityCalled {
			return readabilityCandidate
		}
		readabilityCalled = true

		extractor := c.extractReadability
		if extractor == nil {
			extractor = extractReadabilityArticle
		}
		article, err := extractor(rawHTML, sourceURL)
		if err != nil {
			return extractionCandidate{}
		}
		readabilityArticle = article
		readabilityCandidate = candidateFor("readability", article)
		return readabilityCandidate
	}

	getPruning := func() extractionCandidate {
		extractor := c.extractPruning
		if extractor == nil {
			extractor = pruneContentDetailed
		}
		prunedHTML, extracted, err := extractor(rawHTML, sourceURL)
		if err != nil || !extracted {
			return extractionCandidate{}
		}
		return candidateFor("pruning", readability.Article{
			Content:     prunedHTML,
			TextContent: stripTags(prunedHTML),
		})
	}

	getRaw := func() extractionCandidate {
		return candidateFor("raw", readability.Article{
			Content:     rawHTML,
			TextContent: stripTags(rawHTML),
		})
	}

	var selected extractionCandidate
	switch normalizeExtractMode(extractMode) {
	case "raw":
		selected = getRaw()
	case "pruning":
		selected = getPruning()
		if !selected.assessment.Usable() {
			selected = getRaw()
		}
	case "auto":
		readable := getReadability()
		pruned := getPruning()
		selected = selectHigherQuality(readable, pruned)
		if !selected.assessment.Usable() {
			selected = getRaw()
		}
	default:
		selected = getReadability()
		if !selected.assessment.Usable() {
			selected = getPruning()
		}
		if !selected.assessment.Usable() {
			selected = getRaw()
		}
	}

	// Preserve the richest metadata independently of the selected extraction
	// mode. A low-quality Readability body can still carry valid page metadata.
	if readabilityCalled {
		metadata = mergeArticleMetadata(readabilityArticle, metadata)
	}
	selected.article = mergeArticleMetadata(selected.article, metadata)
	return selected
}

func normalizeExtractMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "raw":
		return "raw"
	case "pruning":
		return "pruning"
	case "auto":
		return "auto"
	default:
		return "readability"
	}
}

func candidateFor(mode string, article readability.Article) extractionCandidate {
	text := strings.TrimSpace(article.TextContent)
	if text == "" {
		text = stripTags(article.Content)
	}
	return extractionCandidate{
		article:    article,
		assessment: quality.EvaluateCleanedContent(text, mode),
		available:  true,
	}
}

func selectHigherQuality(readable, pruned extractionCandidate) extractionCandidate {
	readableUsable := readable.available && readable.assessment.Usable()
	prunedUsable := pruned.available && pruned.assessment.Usable()

	switch {
	case readableUsable && prunedUsable:
		// Readability wins an exact score tie. This makes auto deterministic and
		// preserves the documented extraction order.
		if pruned.assessment.Info.Score > readable.assessment.Info.Score {
			return pruned
		}
		return readable
	case readableUsable:
		return readable
	case prunedUsable:
		return pruned
	default:
		return extractionCandidate{}
	}
}

func (c *Cleaner) convertArticle(article readability.Article, format, sourceURL string) (string, error) {
	converterInstance := c.mdConverter
	if converterInstance == nil {
		converterInstance = newMarkdownConverter()
	}

	switch format {
	case "markdown", "":
		content, err := ToMarkdown(converterInstance, article.Content, sourceURL)
		if err != nil {
			return "", models.NewScrapeError(models.ErrCodeReadability, "markdown conversion failed", err)
		}
		return content, nil
	case "markdown_citations":
		content, err := ToMarkdown(converterInstance, article.Content, sourceURL)
		if err != nil {
			return "", models.NewScrapeError(models.ErrCodeReadability, "markdown conversion failed", err)
		}
		return ConvertToCitations(content), nil
	case "html":
		return article.Content, nil
	case "text":
		return strings.TrimSpace(article.TextContent), nil
	default:
		content, err := ToMarkdown(converterInstance, article.Content, sourceURL)
		if err != nil {
			return "", models.NewScrapeError(models.ErrCodeReadability, "markdown conversion failed", err)
		}
		return content, nil
	}
}

func extractDocumentMetadata(rawHTML string) readability.Article {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(rawHTML))
	if err != nil {
		return readability.Article{}
	}

	article := readability.Article{
		Title:    strings.TrimSpace(doc.Find("title").First().Text()),
		Excerpt:  firstMetaContent(doc, "meta[name='description']", "meta[property='og:description']"),
		Byline:   firstMetaContent(doc, "meta[name='author']", "meta[property='article:author']"),
		SiteName: firstMetaContent(doc, "meta[property='og:site_name']"),
	}
	if title := firstMetaContent(doc, "meta[property='og:title']"); title != "" {
		article.Title = title
	}
	article.Language, _ = doc.Find("html").First().Attr("lang")
	article.Language = strings.TrimSpace(article.Language)
	return article
}

func firstMetaContent(doc *goquery.Document, selectors ...string) string {
	for _, selector := range selectors {
		if content, exists := doc.Find(selector).First().Attr("content"); exists {
			if content = strings.TrimSpace(content); content != "" {
				return content
			}
		}
	}
	return ""
}

// mergeArticleMetadata fills missing metadata on primary from fallback while
// leaving primary's extracted content untouched.
func mergeArticleMetadata(primary, fallback readability.Article) readability.Article {
	if primary.Title == "" {
		primary.Title = fallback.Title
	}
	if primary.Byline == "" {
		primary.Byline = fallback.Byline
	}
	if primary.Excerpt == "" {
		primary.Excerpt = fallback.Excerpt
	}
	if primary.SiteName == "" {
		primary.SiteName = fallback.SiteName
	}
	if primary.Language == "" {
		primary.Language = fallback.Language
	}
	return primary
}

// stripTags extracts visible text from an HTML fragment.
func stripTags(htmlContent string) string {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(htmlContent))
	if err != nil {
		return strings.TrimSpace(htmlContent)
	}
	root := doc.Selection
	if body := doc.Find("body").First(); body.Length() > 0 {
		root = body
	}
	root.Find("script, style, noscript, template, svg").Remove()
	return strings.TrimSpace(root.Text())
}
