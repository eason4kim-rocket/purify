package eav

import (
	"encoding/json"
	"net/url"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/PuerkitoBio/goquery"
)

// Candidate signal literals in harvest priority order. They become prompt and
// response vocabulary for the blind extractor, so they are part of the
// package contract.
const (
	SignalTitle      = "title"
	SignalH1         = "h1"
	SignalOGTitle    = "og:title"
	SignalOGSiteName = "og:site_name"
	SignalJSONLD     = "jsonld"
	SignalSlug       = "slug"
	SignalFrequency  = "frequency"
)

const (
	maxTitleSegments = 6
	maxH1Candidates  = 2
	maxJSONLDNames   = 16
	maxJSONLDDepth   = 8
	maxJSONLDNodes   = 256

	maxFrequencyLatin = 4
	maxFrequencyCJK   = 4
	// A multi-word capitalized run is topical at two occurrences; a single
	// word needs three because every sentence start is capitalized.
	minMultiWordCount  = 2
	minSingleWordCount = 3
	minCJKGramCount    = 3
	maxRunTokens       = 6
	minSlugRunes       = 3
	maxSlugRunes       = 64
)

// HarvestCandidates collects the deterministic entity-candidate slate of one
// document, in fixed signal priority: title segments, h1 headings, og:title
// and og:site_name, JSON-LD names (mainEntity, about, and top-level
// Product/Organization/Person nodes), the URL slug, and frequent
// capitalized/CJK phrases from the cleaned head window.
//
// Every candidate is anchored: its Quote occurs verbatim in the document
// title or cleaned text, with an ASCII-case-insensitive fallback that adopts
// the document's own casing. Unanchorable candidates are dropped, duplicates
// merge on their Normalize form keeping the highest-priority signal, and the
// slate is capped at MaxCandidates. Oversized fields are treated as absent
// rather than partially processed. The harvest never errors: a malformed
// document simply yields a smaller slate.
func HarvestCandidates(doc Document) []Candidate {
	title := doc.Title
	if len(title) > MaxQuoteBytes {
		title = ""
	}
	cleaned := doc.Cleaned
	if len(cleaned) > MaxDocumentBytes {
		cleaned = ""
	}
	rawHTML := doc.RawHTML
	if len(rawHTML) > MaxDocumentBytes {
		rawHTML = ""
	}

	type prototype struct {
		surface      string
		signal       string
		adoptLocated bool
	}
	prototypes := make([]prototype, 0, 32)
	for _, segment := range titleSegments(title) {
		prototypes = append(prototypes, prototype{segment, SignalTitle, false})
	}
	if rawHTML != "" {
		if parsed, err := goquery.NewDocumentFromReader(strings.NewReader(rawHTML)); err == nil {
			headings := 0
			parsed.Find("h1").EachWithBreak(func(_ int, selection *goquery.Selection) bool {
				if text := collapseSpace(selection.Text()); text != "" {
					prototypes = append(prototypes, prototype{text, SignalH1, false})
					headings++
				}
				return headings < maxH1Candidates
			})
			for _, meta := range []struct{ selector, signal string }{
				{`meta[property="og:title"], meta[name="og:title"]`, SignalOGTitle},
				{`meta[property="og:site_name"], meta[name="og:site_name"]`, SignalOGSiteName},
			} {
				if content, ok := parsed.Find(meta.selector).First().Attr("content"); ok {
					if text := collapseSpace(content); text != "" {
						prototypes = append(prototypes, prototype{text, meta.signal, false})
					}
				}
			}
			parsed.Find(`script[type="application/ld+json"]`).Each(func(_ int, selection *goquery.Selection) {
				for _, name := range jsonldNames(selection.Text()) {
					prototypes = append(prototypes, prototype{name, SignalJSONLD, false})
				}
			})
		}
	}
	if slug := slugPhrase(doc.URL); slug != "" {
		prototypes = append(prototypes, prototype{slug, SignalSlug, true})
	}
	for _, frequent := range frequencyPhrases(headWindow(cleaned)) {
		prototypes = append(prototypes, prototype{frequent, SignalFrequency, false})
	}

	locator := newQuoteLocator(title, cleaned)
	candidates := make([]Candidate, 0, MaxCandidates)
	seen := make(map[string]struct{}, MaxCandidates)
	for _, entry := range prototypes {
		if len(candidates) == MaxCandidates {
			break
		}
		surface := strings.TrimSpace(entry.surface)
		if surface == "" || len(surface) > MaxEntityBytes {
			continue
		}
		quote, located := locator.locate(surface)
		if !located {
			continue
		}
		if entry.adoptLocated {
			surface = quote
		}
		key := Normalize(surface)
		if key == "" {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		candidates = append(candidates, Candidate{Surface: surface, Signal: entry.signal, Quote: quote})
	}
	return candidates
}

// titleSeparators split a page title into site-name and topic segments.
// Hyphens and dashes split only when surrounded by spaces so hyphenated
// names (Mercedes-Benz) stay whole; pipes and underscores split bare because
// they never occur inside an entity name.
var titleSeparators = []string{" | ", " - ", " – ", " — ", "｜", "|", "_"}

func titleSegments(title string) []string {
	if strings.TrimSpace(title) == "" {
		return nil
	}
	segments := []string{title}
	for _, separator := range titleSeparators {
		split := make([]string, 0, len(segments))
		for _, segment := range segments {
			split = append(split, strings.Split(segment, separator)...)
		}
		segments = split
	}
	kept := make([]string, 0, len(segments))
	for _, segment := range segments {
		if trimmed := strings.TrimSpace(segment); trimmed != "" {
			kept = append(kept, trimmed)
			if len(kept) == maxTitleSegments {
				break
			}
		}
	}
	return kept
}

func collapseSpace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// jsonldNames extracts candidate names from one JSON-LD script body. Only
// @graph, mainEntity, and about edges are traversed plus top-level typed
// nodes, so publisher/offers/itemListElement subtrees never leak candidates;
// walking named keys instead of ranging maps keeps the order deterministic.
// Malformed JSON is silently skipped.
func jsonldNames(raw string) []string {
	if len(raw) > MaxDocumentBytes {
		return nil
	}
	var root any
	if err := json.Unmarshal([]byte(raw), &root); err != nil {
		return nil
	}
	walk := &jsonldWalk{}
	walk.node(root, 0)
	return walk.names
}

type jsonldWalk struct {
	names []string
	nodes int
}

func (walk *jsonldWalk) node(value any, depth int) {
	if depth > maxJSONLDDepth || walk.nodes >= maxJSONLDNodes || len(walk.names) >= maxJSONLDNames {
		return
	}
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			walk.node(item, depth+1)
		}
	case map[string]any:
		walk.nodes++
		if graph, ok := typed["@graph"]; ok {
			walk.node(graph, depth+1)
		}
		if entityType(typed["@type"]) {
			walk.name(typed["name"])
		}
		walk.entity(typed["mainEntity"], depth+1)
		walk.entity(typed["about"], depth+1)
	}
}

func (walk *jsonldWalk) entity(value any, depth int) {
	if value == nil || depth > maxJSONLDDepth || walk.nodes >= maxJSONLDNodes {
		return
	}
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			walk.entity(item, depth+1)
		}
	case map[string]any:
		walk.nodes++
		walk.name(typed["name"])
	}
}

func (walk *jsonldWalk) name(value any) {
	if len(walk.names) >= maxJSONLDNames {
		return
	}
	if text, ok := value.(string); ok {
		if collapsed := collapseSpace(text); collapsed != "" {
			walk.names = append(walk.names, collapsed)
		}
	}
}

func entityType(value any) bool {
	named := func(text string) bool {
		return text == "Product" || text == "Organization" || text == "Person"
	}
	switch typed := value.(type) {
	case string:
		return named(typed)
	case []any:
		for _, item := range typed {
			if text, ok := item.(string); ok && named(text) {
				return true
			}
		}
	}
	return false
}

// slugSkip holds structural last segments that name no entity.
var slugSkip = map[string]struct{}{"index": {}, "home": {}, "default": {}}

// slugPhrase reconstructs a phrase from the last meaningful URL path segment.
// Identifier-shaped segments (digits, long hex) yield nothing; the phrase
// still has to anchor in the document, which recovers its display casing.
func slugPhrase(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	segments := strings.Split(parsed.Path, "/")
	for index := len(segments) - 1; index >= 0; index-- {
		segment := segments[index]
		if segment == "" {
			continue
		}
		if decoded, decodeErr := url.PathUnescape(segment); decodeErr == nil {
			segment = decoded
		}
		lower := strings.ToLower(segment)
		for _, extension := range []string{".html", ".htm", ".shtml", ".php", ".asp", ".aspx"} {
			if strings.HasSuffix(lower, extension) {
				segment = segment[:len(segment)-len(extension)]
				break
			}
		}
		phrase := collapseSpace(strings.Map(func(r rune) rune {
			if r == '-' || r == '_' || r == '+' {
				return ' '
			}
			return r
		}, segment))
		if !slugViable(phrase) {
			return ""
		}
		return phrase
	}
	return ""
}

func slugViable(phrase string) bool {
	runes := []rune(phrase)
	if len(runes) < minSlugRunes || len(runes) > maxSlugRunes {
		return false
	}
	if _, skip := slugSkip[strings.ToLower(phrase)]; skip {
		return false
	}
	letters := false
	digitsOnly := true
	hexOnly := true
	for _, r := range runes {
		if unicode.IsLetter(r) {
			letters = true
		}
		if r != ' ' && (r < '0' || r > '9') {
			digitsOnly = false
		}
		isHex := r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F'
		if r != ' ' && !isHex {
			hexOnly = false
		}
	}
	if !letters || digitsOnly {
		return false
	}
	if hexOnly && len(runes) >= 16 {
		return false
	}
	return true
}

// headWindow bounds frequency scanning to the document head on a rune
// boundary.
func headWindow(cleaned string) string {
	if len(cleaned) <= MaxHeadWindowBytes {
		return cleaned
	}
	cut := MaxHeadWindowBytes
	for cut > 0 && !utf8.RuneStart(cleaned[cut]) {
		cut--
	}
	return cleaned[:cut]
}

func frequencyPhrases(head string) []string {
	if head == "" {
		return nil
	}
	return append(latinRunPhrases(head), cjkGramPhrases(head)...)
}

type frequencyEntry struct {
	phrase string
	count  int
}

func sortFrequencyEntries(entries []frequencyEntry) {
	sort.SliceStable(entries, func(first, second int) bool {
		if entries[first].count != entries[second].count {
			return entries[first].count > entries[second].count
		}
		if len(entries[first].phrase) != len(entries[second].phrase) {
			return len(entries[first].phrase) > len(entries[second].phrase)
		}
		return entries[first].phrase < entries[second].phrase
	})
}

// runConnectors may appear inside a capitalized run ("Bank of America") but
// never start or end one.
var runConnectors = map[string]struct{}{
	"of": {}, "the": {}, "and": {}, "de": {}, "la": {}, "du": {}, "von": {}, "van": {},
}

// runStopwords are single capitalized words that are sentence mechanics, not
// entities.
var runStopwords = map[string]struct{}{
	"the": {}, "a": {}, "an": {}, "i": {}, "he": {}, "she": {}, "it": {}, "we": {},
	"they": {}, "you": {}, "this": {}, "that": {}, "these": {}, "those": {}, "in": {},
	"on": {}, "at": {}, "by": {}, "but": {}, "and": {}, "or": {}, "if": {}, "as": {},
	"so": {}, "for": {}, "when": {}, "while": {}, "after": {}, "before": {},
	"however": {}, "meanwhile": {}, "according": {}, "with": {}, "from": {},
	"into": {}, "about": {}, "its": {}, "his": {}, "her": {}, "their": {}, "our": {},
	"your": {}, "my": {}, "is": {}, "are": {}, "was": {}, "were": {}, "be": {},
	"been": {}, "not": {}, "no": {}, "yes": {}, "now": {}, "here": {}, "there": {},
	"today": {},
}

type headToken struct {
	text       string
	start, end int
	capital    bool
	digits     bool
	connector  bool
}

// latinRunPhrases counts maximal runs of capitalized words (allowing inner
// connectors and trailing numbers, so "Bank of America" and "iPhone 15"
// survive) and keeps the most frequent ones. Run text is sliced from the
// original head so anchoring stays verbatim.
func latinRunPhrases(head string) []string {
	tokens := headTokens(head)
	counts := make(map[string]int)
	run := make([]headToken, 0, maxRunTokens)
	flush := func() {
		for len(run) > 0 && (run[len(run)-1].connector || run[len(run)-1].digits) {
			if run[len(run)-1].digits && len(run) > 1 {
				break
			}
			run = run[:len(run)-1]
		}
		if len(run) > 0 {
			counts[head[run[0].start:run[len(run)-1].end]]++
		}
		run = run[:0]
	}
	for _, token := range tokens {
		switch {
		case token.capital:
			run = append(run, token)
		case (token.connector || token.digits) && len(run) > 0:
			run = append(run, token)
		default:
			flush()
		}
		if len(run) == maxRunTokens {
			flush()
		}
	}
	flush()

	entries := make([]frequencyEntry, 0, len(counts))
	for phrase, count := range counts {
		multiWord := strings.ContainsRune(phrase, ' ')
		if multiWord && count >= minMultiWordCount {
			entries = append(entries, frequencyEntry{phrase, count})
			continue
		}
		if multiWord || count < minSingleWordCount {
			continue
		}
		lower := strings.ToLower(phrase)
		if _, stop := runStopwords[lower]; stop || utf8.RuneCountInString(phrase) < minSlugRunes {
			continue
		}
		entries = append(entries, frequencyEntry{phrase, count})
	}
	sortFrequencyEntries(entries)
	phrases := make([]string, 0, maxFrequencyLatin)
	for _, entry := range entries {
		if len(phrases) == maxFrequencyLatin {
			break
		}
		phrases = append(phrases, entry.phrase)
	}
	return phrases
}

func headTokens(head string) []headToken {
	tokens := make([]headToken, 0, 256)
	start := -1
	capital, digits := false, true
	flush := func(end int) {
		if start < 0 {
			return
		}
		text := head[start:end]
		_, connector := runConnectors[text]
		tokens = append(tokens, headToken{
			text:      text,
			start:     start,
			end:       end,
			capital:   capital,
			digits:    digits,
			connector: connector && !capital,
		})
		start = -1
		capital, digits = false, true
	}
	for offset, r := range head {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if start < 0 {
				start = offset
			}
			if unicode.IsUpper(r) {
				capital = true
			}
			if !unicode.IsDigit(r) {
				digits = false
			}
			continue
		}
		flush(offset)
	}
	flush(len(head))
	return tokens
}

// cjkGramPhrases counts Han n-grams (n = 2..6) inside contiguous Han runs and
// keeps the highest-scoring non-overlapping grams, skipping fragments of
// legal-form wrappers. Frequency times length favors the longest recurring
// name over its own substrings.
func cjkGramPhrases(head string) []string {
	counts := make(map[string]int)
	runRunes := make([]rune, 0, 64)
	flush := func() {
		for size := 2; size <= 6 && size <= len(runRunes); size++ {
			for index := 0; index+size <= len(runRunes); index++ {
				counts[string(runRunes[index:index+size])]++
			}
		}
		runRunes = runRunes[:0]
	}
	for _, r := range head {
		if unicode.Is(unicode.Han, r) {
			runRunes = append(runRunes, r)
			continue
		}
		flush()
	}
	flush()

	entries := make([]frequencyEntry, 0, len(counts))
	for gram, count := range counts {
		if count < minCJKGramCount || wrapperFragment(gram) {
			continue
		}
		entries = append(entries, frequencyEntry{gram, count * utf8.RuneCountInString(gram)})
	}
	sortFrequencyEntries(entries)
	phrases := make([]string, 0, maxFrequencyCJK)
	for _, entry := range entries {
		if len(phrases) == maxFrequencyCJK {
			break
		}
		overlaps := false
		for _, picked := range phrases {
			if strings.Contains(picked, entry.phrase) || strings.Contains(entry.phrase, picked) {
				overlaps = true
				break
			}
		}
		if !overlaps {
			phrases = append(phrases, entry.phrase)
		}
	}
	return phrases
}

func wrapperFragment(gram string) bool {
	for _, suffix := range cjkLegalSuffixes {
		if strings.Contains(suffix, gram) {
			return true
		}
	}
	return false
}

// quoteLocator anchors candidate surfaces in the title or cleaned text. The
// ASCII-lowered copies are built once; because only ASCII bytes change, byte
// offsets into the lowered copies address the originals exactly, letting the
// case-insensitive fallback return the document's own casing as the quote.
type quoteLocator struct {
	title        string
	cleaned      string
	titleLower   string
	cleanedLower string
}

func newQuoteLocator(title, cleaned string) *quoteLocator {
	return &quoteLocator{
		title:        title,
		cleaned:      cleaned,
		titleLower:   asciiLower(title),
		cleanedLower: asciiLower(cleaned),
	}
}

func (locator *quoteLocator) locate(surface string) (string, bool) {
	if strings.Contains(locator.title, surface) || strings.Contains(locator.cleaned, surface) {
		return surface, true
	}
	needle := asciiLower(surface)
	if index := strings.Index(locator.titleLower, needle); index >= 0 {
		return locator.title[index : index+len(surface)], true
	}
	if index := strings.Index(locator.cleanedLower, needle); index >= 0 {
		return locator.cleaned[index : index+len(surface)], true
	}
	return "", false
}

func asciiLower(value string) string {
	lowered := []byte(value)
	changed := false
	for index, b := range lowered {
		if b >= 'A' && b <= 'Z' {
			lowered[index] = b + 'a' - 'A'
			changed = true
		}
	}
	if !changed {
		return value
	}
	return string(lowered)
}
