package verify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/cascadia"
	"github.com/use-agent/purify/evidence"
	"golang.org/x/net/html"
)

type scalarKind uint8

const (
	scalarString scalarKind = iota + 1
	scalarNumber
	scalarBoolean
)

type scalarValue struct {
	kind    scalarKind
	raw     json.RawMessage
	text    string
	number  *big.Rat
	boolean bool
}

func decodeScalar(raw json.RawMessage) (scalarValue, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return scalarValue{}, fmt.Errorf("value must be valid JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return scalarValue{}, errors.New("value must contain exactly one JSON value")
		}
		return scalarValue{}, fmt.Errorf("value must contain exactly one JSON value: %w", err)
	}

	switch value := decoded.(type) {
	case string:
		canonical, _ := json.Marshal(value)
		return scalarValue{kind: scalarString, raw: canonical, text: value}, nil
	case json.Number:
		number, err := parseDecimal(value.String())
		if err != nil {
			return scalarValue{}, fmt.Errorf("invalid JSON number: %w", err)
		}
		return scalarValue{
			kind:   scalarNumber,
			raw:    cloneRaw(json.RawMessage(value.String())),
			text:   value.String(),
			number: number,
		}, nil
	case bool:
		raw := json.RawMessage(strconv.FormatBool(value))
		return scalarValue{kind: scalarBoolean, raw: raw, text: string(raw), boolean: value}, nil
	case nil:
		return scalarValue{}, errors.New("null is not a verifiable scalar")
	case []any:
		return scalarValue{}, errors.New("arrays are not verifiable scalar claims")
	case map[string]any:
		return scalarValue{}, errors.New("objects are not verifiable scalar claims")
	default:
		return scalarValue{}, fmt.Errorf("unsupported JSON value type %T", decoded)
	}
}

func (value scalarValue) equal(other scalarValue) bool {
	if value.kind != other.kind {
		return false
	}
	switch value.kind {
	case scalarString:
		return normalizeString(value.text) == normalizeString(other.text)
	case scalarNumber:
		return value.number != nil && other.number != nil && value.number.Cmp(other.number) == 0
	case scalarBoolean:
		return value.boolean == other.boolean
	default:
		return false
	}
}

type selectorMatchState uint8

const (
	selectorMissing selectorMatchState = iota
	selectorValue
	selectorAmbiguous
)

type textSpan struct {
	start int
	end   int
}

type canonicalPage struct {
	document *goquery.Document
	text     string
	spans    map[*html.Node]textSpan
}

func parseCanonicalPage(rawHTML string) (*canonicalPage, error) {
	document, err := goquery.NewDocumentFromReader(strings.NewReader(rawHTML))
	if err != nil {
		return nil, err
	}
	nodes := document.Selection.Nodes
	if body := document.Find("body").First(); body.Length() == 1 {
		nodes = body.Nodes
	}
	builder := canonicalTextBuilder{spans: make(map[*html.Node]textSpan)}
	for _, node := range nodes {
		if _, _, err := builder.appendNode(node); err != nil {
			return nil, err
		}
	}
	return &canonicalPage{
		document: document,
		text:     builder.text.String(),
		spans:    builder.spans,
	}, nil
}

func (page *canonicalPage) scalarAtSelector(selector string, kind scalarKind) (scalarValue, string, textSpan, selectorMatchState) {
	if page == nil || page.document == nil {
		return scalarValue{}, "", textSpan{}, selectorAmbiguous
	}
	matcher, err := cascadia.Compile(selector)
	if err != nil {
		return scalarValue{}, "", textSpan{}, selectorAmbiguous
	}
	selection := page.document.FindMatcher(matcher)
	if selection.Length() == 0 {
		return scalarValue{}, "", textSpan{}, selectorMissing
	}
	if selection.Length() != 1 || len(selection.Nodes) != 1 {
		return scalarValue{}, "", textSpan{}, selectorAmbiguous
	}
	span, ok := page.spans[selection.Nodes[0]]
	if !ok || span.start < 0 || span.end <= span.start || span.end > len(page.text) {
		return scalarValue{}, "", textSpan{}, selectorAmbiguous
	}
	selectedText := page.text[span.start:span.end]
	if len(selectedText) > maximumScalarBytes {
		return scalarValue{}, "", textSpan{}, selectorAmbiguous
	}
	value, quote, ok := scalarFromText(selectedText, kind)
	if !ok {
		return scalarValue{}, "", textSpan{}, selectorAmbiguous
	}
	relative := strings.Index(selectedText, quote)
	if relative < 0 {
		return scalarValue{}, "", textSpan{}, selectorAmbiguous
	}
	quoteSpan := textSpan{start: span.start + relative, end: span.start + relative + len(quote)}
	return value, quote, quoteSpan, selectorValue
}

type canonicalTextBuilder struct {
	text         strings.Builder
	spans        map[*html.Node]textSpan
	pendingSpace bool
}

func (builder *canonicalTextBuilder) appendNode(node *html.Node) (textSpan, bool, error) {
	if node == nil || nodeIsHidden(node) {
		return textSpan{}, false, nil
	}
	if node.Type == html.TextNode {
		return builder.appendText(node.Data)
	}
	if node.Type == html.ElementNode && strings.EqualFold(node.Data, "br") {
		builder.boundary()
		return textSpan{}, false, nil
	}

	block := node.Type == html.ElementNode && nodeIsBlock(node)
	if block {
		builder.boundary()
	}
	combined := textSpan{start: -1}
	visible := false
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		childSpan, childVisible, err := builder.appendNode(child)
		if err != nil {
			return textSpan{}, false, err
		}
		if !childVisible {
			continue
		}
		if !visible {
			combined.start = childSpan.start
			visible = true
		}
		combined.end = childSpan.end
	}
	if block {
		builder.boundary()
	}
	if visible {
		builder.spans[node] = combined
	}
	return combined, visible, nil
}

func (builder *canonicalTextBuilder) appendText(value string) (textSpan, bool, error) {
	span := textSpan{start: -1}
	for _, current := range value {
		if unicode.IsSpace(current) {
			builder.boundary()
			continue
		}
		if builder.pendingSpace && builder.text.Len() > 0 {
			if builder.text.Len()+1 > maximumCanonicalTextBytes {
				return textSpan{}, false, fmt.Errorf("canonical text exceeds %d bytes", maximumCanonicalTextBytes)
			}
			builder.text.WriteByte(' ')
		}
		builder.pendingSpace = false
		encodedWidth := utf8.RuneLen(current)
		if encodedWidth < 1 || builder.text.Len()+encodedWidth > maximumCanonicalTextBytes {
			return textSpan{}, false, fmt.Errorf("canonical text exceeds %d bytes", maximumCanonicalTextBytes)
		}
		if span.start < 0 {
			span.start = builder.text.Len()
		}
		builder.text.WriteRune(current)
		span.end = builder.text.Len()
	}
	return span, span.start >= 0, nil
}

func (builder *canonicalTextBuilder) boundary() {
	if builder.text.Len() > 0 {
		builder.pendingSpace = true
	}
}

func nodeIsHidden(node *html.Node) bool {
	if node.Type != html.ElementNode {
		return false
	}
	switch strings.ToLower(node.Data) {
	case "head", "script", "style", "noscript", "template", "svg", "canvas":
		return true
	case "input":
		if value, ok := nodeAttribute(node, "type"); ok && strings.EqualFold(strings.TrimSpace(value), "hidden") {
			return true
		}
	}
	if _, ok := nodeAttribute(node, "hidden"); ok {
		return true
	}
	if _, ok := nodeAttribute(node, "inert"); ok {
		return true
	}
	if value, ok := nodeAttribute(node, "aria-hidden"); ok && strings.EqualFold(strings.TrimSpace(value), "true") {
		return true
	}
	style, _ := nodeAttribute(node, "style")
	for _, declaration := range strings.Split(style, ";") {
		key, value, ok := strings.Cut(declaration, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = normalizeCSSValue(value)
		switch key {
		case "display":
			if value == "none" {
				return true
			}
		case "visibility":
			if value == "hidden" || value == "collapse" {
				return true
			}
		case "content-visibility":
			if value == "hidden" {
				return true
			}
		case "opacity":
			opacity, parseErr := strconv.ParseFloat(strings.TrimSuffix(value, "%"), 64)
			if parseErr == nil && opacity == 0 {
				return true
			}
		}
	}
	return false
}

func nodeIsBlock(node *html.Node) bool {
	if style, ok := nodeAttribute(node, "style"); ok {
		for _, declaration := range strings.Split(style, ";") {
			key, value, found := strings.Cut(declaration, ":")
			if !found || !strings.EqualFold(strings.TrimSpace(key), "display") {
				continue
			}
			display := normalizeCSSValue(value)
			if strings.HasPrefix(display, "inline") {
				return false
			}
			if display != "" && display != "contents" && display != "none" {
				return true
			}
		}
	}
	switch strings.ToLower(node.Data) {
	case "address", "article", "aside", "blockquote", "body", "dd", "details", "dialog", "div", "dl", "dt", "fieldset", "figcaption", "figure", "footer", "form", "h1", "h2", "h3", "h4", "h5", "h6", "header", "hgroup", "hr", "li", "main", "menu", "nav", "ol", "p", "pre", "section", "summary", "table", "tbody", "td", "tfoot", "th", "thead", "tr", "ul":
		return true
	default:
		return false
	}
}

func nodeAttribute(node *html.Node, name string) (string, bool) {
	for _, attribute := range node.Attr {
		if strings.EqualFold(attribute.Key, name) {
			return attribute.Val, true
		}
	}
	return "", false
}

func normalizeCSSValue(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.TrimSpace(strings.TrimSuffix(value, "!important"))
}

func quoteRepresentsScalar(quote string, scalar scalarValue) bool {
	_, ok := buildQuotePlan(quote, scalar)
	return ok
}

type quoteMatch struct {
	quote                          string
	start, end                     int
	normalizedStart, normalizedEnd int
	method                         evidence.Method
}

type tokenSpan struct {
	start int
	end   int
}

type quotePlan struct {
	scalar          scalarValue
	normalizedQuote string
	exactToken      tokenSpan
	normalizedToken tokenSpan
}

type quoteCorpus struct {
	original   string
	normalized normalizedStrictText
}

func newQuoteCorpus(text string) quoteCorpus {
	return quoteCorpus{
		original:   text,
		normalized: normalizeStrictWithOffsets(text),
	}
}

// match accepts only one unambiguous, type-correct occurrence. Numeric and
// boolean claims must contain a complete lexical token; strings must align to
// Unicode word boundaries. Exact matches take precedence over a normalized
// match at the same original byte range.
func (corpus quoteCorpus) match(quote string, scalar scalarValue) (quoteMatch, bool) {
	plan, ok := buildQuotePlan(quote, scalar)
	if !ok || corpus.original == "" {
		return quoteMatch{}, false
	}

	var found quoteMatch
	foundMatch := false
	ambiguous := false
	accept := func(candidate quoteMatch) bool {
		if !corpus.supports(candidate, plan) {
			return false
		}
		if !foundMatch {
			found = candidate
			foundMatch = true
			return false
		}
		if found.start == candidate.start && found.end == candidate.end {
			return false
		}
		ambiguous = true
		return true
	}

	complete := forEachOccurrence(corpus.original, quote, func(start, end int) bool {
		return accept(quoteMatch{
			quote:           corpus.original[start:end],
			start:           start,
			end:             end,
			normalizedStart: -1,
			normalizedEnd:   -1,
			method:          evidence.MethodExact,
		})
	})
	if !complete || ambiguous {
		return quoteMatch{}, false
	}

	if plan.normalizedQuote != "" {
		complete = forEachOccurrence(corpus.normalized.text, plan.normalizedQuote, func(start, end int) bool {
			originalStart, originalEnd, ok := corpus.normalized.originalRange(start, end)
			if !ok {
				return false
			}
			return accept(quoteMatch{
				quote:           corpus.original[originalStart:originalEnd],
				start:           originalStart,
				end:             originalEnd,
				normalizedStart: start,
				normalizedEnd:   end,
				method:          evidence.MethodNormalized,
			})
		})
	}
	if !complete || ambiguous || !foundMatch {
		return quoteMatch{}, false
	}
	return found, true
}

func (corpus quoteCorpus) matchAt(quote string, scalar scalarValue, start, end int) (quoteMatch, bool) {
	plan, ok := buildQuotePlan(quote, scalar)
	if !ok || start < 0 || end <= start || end > len(corpus.original) {
		return quoteMatch{}, false
	}
	actual := corpus.original[start:end]
	method := evidence.MethodExact
	if actual != quote {
		if normalizeString(actual) != plan.normalizedQuote {
			return quoteMatch{}, false
		}
		method = evidence.MethodNormalized
	}
	candidate := quoteMatch{quote: actual, start: start, end: end, normalizedStart: -1, normalizedEnd: -1, method: method}
	if method == evidence.MethodNormalized {
		if plan.scalar.kind == scalarString {
			return candidate, stringBoundary(corpus.original, start, end)
		}
		value, token, ok := scalarFromText(actual, plan.scalar.kind)
		if !ok || !plan.scalar.equal(value) {
			return quoteMatch{}, false
		}
		relative := strings.Index(actual, token)
		if relative < 0 || !typedTokenBoundary(corpus.original, start+relative, start+relative+len(token), plan.scalar.kind) {
			return quoteMatch{}, false
		}
		return candidate, true
	}
	return candidate, corpus.supports(candidate, plan)
}

func buildQuotePlan(quote string, scalar scalarValue) (quotePlan, bool) {
	if strings.TrimSpace(quote) == "" {
		return quotePlan{}, false
	}
	plan := quotePlan{scalar: scalar, normalizedQuote: normalizeString(quote)}
	switch scalar.kind {
	case scalarString:
		if plan.normalizedQuote == "" || plan.normalizedQuote != normalizeString(scalar.text) {
			return quotePlan{}, false
		}
	case scalarNumber, scalarBoolean:
		candidate, _, ok := scalarFromText(quote, scalar.kind)
		if !ok || !scalar.equal(candidate) {
			return quotePlan{}, false
		}
		plan.exactToken, ok = singleTypedToken(quote, scalar.kind)
		if !ok {
			return quotePlan{}, false
		}
		plan.normalizedToken, ok = singleTypedToken(plan.normalizedQuote, scalar.kind)
		if !ok {
			return quotePlan{}, false
		}
	default:
		return quotePlan{}, false
	}
	return plan, true
}

func singleTypedToken(text string, kind scalarKind) (tokenSpan, bool) {
	var matches [][]int
	switch kind {
	case scalarNumber:
		matches = numberPattern.FindAllStringIndex(text, 2)
	case scalarBoolean:
		matches = booleanPattern.FindAllStringIndex(text, 2)
	default:
		return tokenSpan{}, false
	}
	if len(matches) != 1 || matches[0][1]-matches[0][0] > maximumNumberTokenBytes {
		return tokenSpan{}, false
	}
	return tokenSpan{start: matches[0][0], end: matches[0][1]}, true
}

func (corpus quoteCorpus) supports(candidate quoteMatch, plan quotePlan) bool {
	switch plan.scalar.kind {
	case scalarNumber:
		start, end, ok := corpus.candidateTokenRange(candidate, plan)
		return ok && numberTokenBoundary(corpus.original, start, end)
	case scalarBoolean:
		start, end, ok := corpus.candidateTokenRange(candidate, plan)
		return ok && wordBoundary(corpus.original, start, end)
	case scalarString:
		return stringBoundary(corpus.original, candidate.start, candidate.end)
	default:
		return false
	}
}

func (corpus quoteCorpus) candidateTokenRange(candidate quoteMatch, plan quotePlan) (int, int, bool) {
	if candidate.method == evidence.MethodExact {
		start := candidate.start + plan.exactToken.start
		end := candidate.start + plan.exactToken.end
		return start, end, start >= candidate.start && end <= candidate.end
	}
	if candidate.normalizedStart < 0 {
		return 0, 0, false
	}
	start, _, startOK := corpus.normalized.originalRange(
		candidate.normalizedStart+plan.normalizedToken.start,
		candidate.normalizedStart+plan.normalizedToken.start+1,
	)
	_, end, endOK := corpus.normalized.originalRange(
		candidate.normalizedStart+plan.normalizedToken.end-1,
		candidate.normalizedStart+plan.normalizedToken.end,
	)
	if !startOK || !endOK || start < candidate.start || end > candidate.end {
		return 0, 0, false
	}
	actual, _, ok := scalarFromText(corpus.original[start:end], plan.scalar.kind)
	if !ok || !plan.scalar.equal(actual) {
		return 0, 0, false
	}
	return start, end, true
}

func typedTokenBoundary(text string, start, end int, kind scalarKind) bool {
	switch kind {
	case scalarNumber:
		return numberTokenBoundary(text, start, end)
	case scalarBoolean:
		return wordBoundary(text, start, end)
	default:
		return false
	}
}

func forEachOccurrence(text, value string, visit func(start, end int) bool) bool {
	if value == "" || len(value) > len(text) {
		return true
	}
	attempts := 0
	for offset := 0; offset <= len(text)-len(value); {
		relative := strings.Index(text[offset:], value)
		if relative < 0 {
			return true
		}
		attempts++
		if attempts > maximumQuoteOccurrences {
			return false
		}
		start := offset + relative
		end := start + len(value)
		if visit(start, end) {
			return true
		}
		_, width := utf8.DecodeRuneInString(text[start:])
		if width < 1 {
			width = 1
		}
		offset = start + width
	}
	return true
}

func numberTokenBoundary(text string, start, end int) bool {
	if previous, ok := previousRune(text, start); ok && isNumberContinuation(previous) {
		return false
	}
	if next, ok := nextRune(text, end); ok && isNumberContinuation(next) {
		return false
	}
	return true
}

func stringBoundary(text string, start, end int) bool {
	start, end = trimUnicodeSpaceRange(text, start, end)
	if start >= end {
		return false
	}
	first, _ := utf8.DecodeRuneInString(text[start:end])
	last, _ := utf8.DecodeLastRuneInString(text[start:end])
	if previous, ok := previousRune(text, start); ok && isStringTokenRune(first) {
		if isWordRune(previous) || isStringConnector(previous) && connectorRunReachesWordBackward(text, start) {
			return false
		}
	}
	if next, ok := nextRune(text, end); ok && isStringTokenRune(last) {
		if isWordRune(next) || isStringConnector(next) && connectorRunReachesWordForward(text, end) {
			return false
		}
	}

	hasDigit := false
	for _, current := range text[start:end] {
		if unicode.IsDigit(current) {
			hasDigit = true
			break
		}
	}
	if !hasDigit {
		return true
	}
	if previous, ok := previousRune(text, start); ok && isNumericStringAffix(previous) {
		return false
	}
	if next, ok := nextRune(text, end); ok && isNumericStringAffix(next) {
		return false
	}
	return true
}

func wordBoundary(text string, start, end int) bool {
	if previous, ok := previousRune(text, start); ok && isWordRune(previous) {
		return false
	}
	if next, ok := nextRune(text, end); ok && isWordRune(next) {
		return false
	}
	return true
}

func trimUnicodeSpaceRange(text string, start, end int) (int, int) {
	for start < end {
		current, width := utf8.DecodeRuneInString(text[start:end])
		if !unicode.IsSpace(current) {
			break
		}
		start += width
	}
	for end > start {
		current, width := utf8.DecodeLastRuneInString(text[start:end])
		if !unicode.IsSpace(current) {
			break
		}
		end -= width
	}
	return start, end
}

func previousRune(text string, offset int) (rune, bool) {
	if offset <= 0 || offset > len(text) {
		return 0, false
	}
	current, _ := utf8.DecodeLastRuneInString(text[:offset])
	return current, true
}

func nextRune(text string, offset int) (rune, bool) {
	if offset < 0 || offset >= len(text) {
		return 0, false
	}
	current, _ := utf8.DecodeRuneInString(text[offset:])
	return current, true
}

func isWordRune(value rune) bool {
	return value == '_' || unicode.IsLetter(value) || unicode.IsDigit(value) || unicode.IsMark(value)
}

func isStringTokenRune(value rune) bool {
	return isWordRune(value) || isStringConnector(value)
}

func isStringConnector(value rune) bool {
	return strings.ContainsRune("-‐‑‒–—―+'’./:@#&", value)
}

func connectorRunReachesWordForward(text string, offset int) bool {
	for offset < len(text) {
		current, width := utf8.DecodeRuneInString(text[offset:])
		if !isStringConnector(current) {
			return isWordRune(current)
		}
		offset += width
	}
	return false
}

func connectorRunReachesWordBackward(text string, offset int) bool {
	for offset > 0 {
		current, width := utf8.DecodeLastRuneInString(text[:offset])
		if !isStringConnector(current) {
			return isWordRune(current)
		}
		offset -= width
	}
	return false
}

func isNumberContinuation(value rune) bool {
	return isWordRune(value) || strings.ContainsRune(".,+-", value)
}

func isNumericStringAffix(value rune) bool {
	return isNumberContinuation(value) || strings.ContainsRune("%‰$€£¥₹", value)
}

type normalizedStrictText struct {
	text         string
	offsets      []uint32
	sourceLength uint32
}

func normalizeStrictWithOffsets(input string) normalizedStrictText {
	var text strings.Builder
	text.Grow(len(input))
	offsets := make([]uint32, 0, len(input))
	lastSpace := false
	for byteOffset, original := range input {
		r := unicode.ToLower(foldWidth(original))
		if unicode.IsSpace(r) {
			if text.Len() == 0 {
				continue
			}
			if lastSpace {
				continue
			}
			r = ' '
			lastSpace = true
		} else {
			lastSpace = false
		}
		encoded := string(r)
		text.WriteString(encoded)
		for range len(encoded) {
			offsets = append(offsets, uint32(byteOffset))
		}
	}
	normalized := text.String()
	if strings.HasSuffix(normalized, " ") {
		normalized = strings.TrimSuffix(normalized, " ")
		offsets = offsets[:len(offsets)-1]
	}
	return normalizedStrictText{text: normalized, offsets: offsets, sourceLength: uint32(len(input))}
}

func (text normalizedStrictText) originalRange(start, end int) (int, int, bool) {
	if start < 0 || end <= start || end > len(text.text) || start >= len(text.offsets) {
		return 0, 0, false
	}
	originalStart := int(text.offsets[start])
	originalEnd := int(text.sourceLength)
	if end < len(text.offsets) {
		originalEnd = int(text.offsets[end])
	}
	if originalEnd <= originalStart {
		return 0, 0, false
	}
	return originalStart, originalEnd, true
}

var (
	booleanPattern = regexp.MustCompile(`(?i)\b(?:true|false)\b`)
	numberPattern  = regexp.MustCompile(`[-+]?(?:(?:\d{1,3}(?:,\d{3})+|\d+)(?:\.\d*)?|\.\d+)(?:[eE][-+]?\d+)?`)
)

func scalarFromText(input string, kind scalarKind) (scalarValue, string, bool) {
	trimmed := strings.TrimSpace(input)
	switch kind {
	case scalarString:
		encoded, err := json.Marshal(trimmed)
		if err != nil {
			return scalarValue{}, "", false
		}
		return scalarValue{kind: scalarString, raw: encoded, text: trimmed}, trimmed, true
	case scalarBoolean:
		matches := booleanPattern.FindAllStringIndex(trimmed, 2)
		if len(matches) != 1 {
			return scalarValue{}, "", false
		}
		token := trimmed[matches[0][0]:matches[0][1]]
		value, err := strconv.ParseBool(strings.ToLower(token))
		if err != nil {
			return scalarValue{}, "", false
		}
		raw := json.RawMessage(strconv.FormatBool(value))
		return scalarValue{kind: scalarBoolean, raw: raw, text: string(raw), boolean: value}, token, true
	case scalarNumber:
		matches := numberPattern.FindAllStringIndex(trimmed, 2)
		if len(matches) != 1 || matches[0][1]-matches[0][0] > maximumNumberTokenBytes {
			return scalarValue{}, "", false
		}
		token := trimmed[matches[0][0]:matches[0][1]]
		jsonNumber, number, ok := parseDisplayedNumber(token)
		if !ok {
			return scalarValue{}, "", false
		}
		return scalarValue{
			kind:   scalarNumber,
			raw:    json.RawMessage(jsonNumber),
			text:   jsonNumber,
			number: number,
		}, token, true
	default:
		return scalarValue{}, "", false
	}
}

func parseDisplayedNumber(display string) (string, *big.Rat, bool) {
	if len(display) == 0 || len(display) > maximumNumberTokenBytes {
		return "", nil, false
	}
	value := strings.ReplaceAll(display, ",", "")
	value = strings.TrimPrefix(value, "+")
	if strings.HasPrefix(value, ".") {
		value = "0" + value
	} else if strings.HasPrefix(value, "-.") {
		value = "-0" + value[1:]
	}
	if strings.HasSuffix(value, ".") {
		value += "0"
	}
	if !json.Valid([]byte(value)) {
		return "", nil, false
	}
	number, err := parseDecimal(value)
	if err != nil {
		return "", nil, false
	}
	return value, number, true
}

var decimalPattern = regexp.MustCompile(`^(-?)(\d+)(?:\.(\d*))?(?:[eE]([+-]?\d+))?$`)

func parseDecimal(value string) (*big.Rat, error) {
	if len(value) == 0 || len(value) > maximumNumberTokenBytes {
		return nil, errors.New("decimal exceeds the supported length")
	}
	parts := decimalPattern.FindStringSubmatch(value)
	if parts == nil {
		return nil, errors.New("unsupported decimal syntax")
	}
	digits := strings.TrimLeft(parts[2]+parts[3], "0")
	if digits == "" {
		digits = "0"
	}
	integer := new(big.Int)
	if _, ok := integer.SetString(digits, 10); !ok {
		return nil, errors.New("invalid decimal digits")
	}
	if parts[1] == "-" {
		integer.Neg(integer)
	}
	exponent := -len(parts[3])
	if parts[4] != "" {
		parsedExponent, err := strconv.Atoi(parts[4])
		if err != nil || parsedExponent < -10_000 || parsedExponent > 10_000 {
			return nil, errors.New("decimal exponent is out of range")
		}
		exponent += parsedExponent
	}
	if exponent < -10_000 || exponent > 10_000 {
		return nil, errors.New("decimal exponent is out of range")
	}

	result := new(big.Rat).SetInt(integer)
	if exponent == 0 || integer.Sign() == 0 {
		return result, nil
	}
	power := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(abs(exponent))), nil)
	if exponent > 0 {
		result.Mul(result, new(big.Rat).SetInt(power))
	} else {
		result.Quo(result, new(big.Rat).SetInt(power))
	}
	return result, nil
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func normalizeString(input string) string {
	var normalized strings.Builder
	lastSpace := false
	for _, original := range input {
		r := foldWidth(original)
		r = unicode.ToLower(r)
		if unicode.IsSpace(r) {
			if normalized.Len() == 0 || lastSpace {
				continue
			}
			normalized.WriteByte(' ')
			lastSpace = true
			continue
		}
		normalized.WriteRune(r)
		lastSpace = false
	}
	return strings.TrimSpace(normalized.String())
}

func foldWidth(r rune) rune {
	switch {
	case r == '\u3000':
		return ' '
	case r >= '\uff01' && r <= '\uff5e':
		return r - 0xfee0
	default:
		return r
	}
}
