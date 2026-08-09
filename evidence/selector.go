package evidence

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
)

const (
	maxContextSelectorHTMLBytes      = 4 << 20
	maxContextSelectorDOMNodes       = 200_000
	maxContextSelectorDOMElements    = 100_000
	maxContextSelectorDepth          = 256
	maxContextSelectorTextWorkBytes  = 16 << 20
	maxContextSelectorMatchElements  = 1_000_000
	maxContextSelectorClassBytes     = alignmentContextCheckpointBytes
	maxContextSelectorClasses        = 256
	maxContextSelectorCandidateBytes = alignmentContextCheckpointBytes
)

var errContextSelectorBudget = errors.New("evidence: context selector complexity budget exceeded")

type contextSelectorBudget struct {
	domElements   int
	textWorkBytes int
	matchElements int
}

// FindUniqueSelector returns a CSS selector that resolves to exactly the
// deepest element containing quote. An empty string means no stable unique
// selector could be derived.
func FindUniqueSelector(rawHTML, quote string) string {
	return newSelectorDocument(rawHTML).find(quote)
}

type selectorDocument struct {
	doc *goquery.Document
}

func newSelectorDocument(rawHTML string) selectorDocument {
	if strings.TrimSpace(rawHTML) == "" {
		return selectorDocument{}
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(rawHTML))
	if err != nil {
		return selectorDocument{}
	}
	return selectorDocument{doc: doc}
}

func newSelectorDocumentContext(ctx context.Context, rawHTML string) (selectorDocument, error) {
	if err := ctx.Err(); err != nil {
		return selectorDocument{}, err
	}
	if len(rawHTML) > maxContextSelectorHTMLBytes {
		return selectorDocument{}, nil
	}
	blank, err := blankStringContext(ctx, rawHTML)
	if err != nil {
		return selectorDocument{}, err
	}
	if blank {
		return selectorDocument{}, nil
	}
	doc, err := goquery.NewDocumentFromReader(&contextCheckpointReader{
		ctx:    ctx,
		reader: strings.NewReader(rawHTML),
	})
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return selectorDocument{}, contextErr
		}
		return selectorDocument{}, nil
	}
	if err := ctx.Err(); err != nil {
		return selectorDocument{}, err
	}
	return selectorDocument{doc: doc}, nil
}

func (document selectorDocument) find(quote string) string {
	if document.doc == nil || normalizeText(quote) == "" {
		return ""
	}
	doc := document.doc
	wanted := normalizeText(quote)
	var best *goquery.Selection
	bestDepth := -1
	doc.Find("*").Each(func(_ int, selection *goquery.Selection) {
		if len(selection.Nodes) == 0 {
			return
		}
		tag := strings.ToLower(selection.Nodes[0].Data)
		if tag == "script" || tag == "style" || tag == "noscript" {
			return
		}
		if !strings.Contains(normalizeText(selection.Text()), wanted) {
			return
		}
		if depth := nodeDepth(selection.Nodes[0]); depth > bestDepth {
			best = selection
			bestDepth = depth
		}
	})
	if best == nil {
		return ""
	}
	return selectorFor(doc.Selection, best)
}

func (document selectorDocument) findContext(ctx context.Context, quote string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if document.doc == nil {
		return "", nil
	}
	wanted, err := normalizeTextContext(ctx, quote)
	if err != nil {
		return "", err
	}
	if wanted == "" {
		return "", nil
	}
	var best *html.Node
	bestDepth := -1
	budget := contextSelectorBudget{}
	err = walkElementsContext(ctx, document.doc.Selection.Nodes, func(node *html.Node) (bool, error) {
		budget.domElements++
		if budget.domElements > maxContextSelectorDOMElements {
			return false, contextSelectorBudgetError(ctx)
		}
		tag := node.Data
		if tag == "script" || tag == "style" || tag == "noscript" {
			return false, nil
		}
		text, err := nodeTextWithBudgetContext(ctx, node, &budget)
		if err != nil {
			return false, err
		}
		normalized, err := normalizeTextContext(ctx, text)
		if err != nil {
			return false, err
		}
		start, err := indexStringContext(ctx, normalized, wanted)
		if err != nil {
			return false, err
		}
		if start < 0 {
			return false, nil
		}
		depth, err := nodeDepthContext(ctx, node)
		if err != nil {
			return false, err
		}
		if depth > maxContextSelectorDepth {
			return false, contextSelectorBudgetError(ctx)
		}
		if depth > bestDepth {
			best = node
			bestDepth = depth
		}
		return false, nil
	})
	if err != nil {
		return contextSelectorResult(ctx, err)
	}
	if best == nil {
		return "", nil
	}
	selector, err := selectorForWithBudgetContext(ctx, document.doc.Selection.Nodes, best, &budget)
	if err != nil {
		return contextSelectorResult(ctx, err)
	}
	return selector, ctx.Err()
}

func contextSelectorBudgetError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return errContextSelectorBudget
}

func contextSelectorResult(ctx context.Context, err error) (string, error) {
	if contextErr := ctx.Err(); contextErr != nil {
		return "", contextErr
	}
	if errors.Is(err, errContextSelectorBudget) {
		return "", nil
	}
	return "", err
}

type contextCheckpointReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextCheckpointReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	if len(buffer) > alignmentContextCheckpointBytes {
		buffer = buffer[:alignmentContextCheckpointBytes]
	}
	read, err := reader.reader.Read(buffer)
	if read == 0 {
		if contextErr := reader.ctx.Err(); contextErr != nil {
			return 0, contextErr
		}
	}
	return read, err
}

func blankStringContext(ctx context.Context, value string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	nextCheckpoint := alignmentContextCheckpointBytes
	for offset, character := range value {
		if offset >= nextCheckpoint {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			for nextCheckpoint <= offset {
				nextCheckpoint += alignmentContextCheckpointBytes
			}
		}
		if !unicode.IsSpace(character) {
			return false, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return true, nil
}

func walkElementsContext(
	ctx context.Context,
	roots []*html.Node,
	visit func(*html.Node) (bool, error),
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stack := make([]*html.Node, 0, len(roots))
	for index := len(roots) - 1; index >= 0; index-- {
		stack = append(stack, roots[index])
	}
	if len(stack) > maxContextSelectorDOMNodes {
		return contextSelectorBudgetError(ctx)
	}
	discovered := len(stack)
	processed := 0
	for len(stack) > 0 {
		last := len(stack) - 1
		node := stack[last]
		stack = stack[:last]
		if node == nil {
			continue
		}
		processed++
		if processed%256 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if node.Type == html.ElementNode {
			stop, err := visit(node)
			if err != nil {
				return err
			}
			if stop {
				return ctx.Err()
			}
		}
		children := 0
		for child := node.LastChild; child != nil; child = child.PrevSibling {
			children++
			if children%256 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			discovered++
			if discovered > maxContextSelectorDOMNodes {
				return contextSelectorBudgetError(ctx)
			}
			stack = append(stack, child)
		}
	}
	return ctx.Err()
}

func nodeTextContext(ctx context.Context, node *html.Node) (string, error) {
	budget := contextSelectorBudget{}
	return nodeTextWithBudgetContext(ctx, node, &budget)
}

func nodeTextWithBudgetContext(
	ctx context.Context,
	node *html.Node,
	budget *contextSelectorBudget,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var text strings.Builder
	stack := []*html.Node{node}
	discovered := 1
	processed := 0
	for len(stack) > 0 {
		last := len(stack) - 1
		current := stack[last]
		stack = stack[:last]
		if current == nil {
			continue
		}
		processed++
		if processed > maxContextSelectorDOMNodes {
			return "", contextSelectorBudgetError(ctx)
		}
		if processed%256 == 0 {
			if err := ctx.Err(); err != nil {
				return "", err
			}
		}
		if current.Type == html.TextNode {
			for start := 0; start < len(current.Data); start += alignmentContextCheckpointBytes {
				if err := ctx.Err(); err != nil {
					return "", err
				}
				end := start + alignmentContextCheckpointBytes
				if end > len(current.Data) {
					end = len(current.Data)
				}
				budget.textWorkBytes += end - start
				if budget.textWorkBytes > maxContextSelectorTextWorkBytes {
					return "", contextSelectorBudgetError(ctx)
				}
				text.WriteString(current.Data[start:end])
			}
		}
		children := 0
		for child := current.LastChild; child != nil; child = child.PrevSibling {
			children++
			if children%256 == 0 {
				if err := ctx.Err(); err != nil {
					return "", err
				}
			}
			discovered++
			if discovered > maxContextSelectorDOMNodes {
				return "", contextSelectorBudgetError(ctx)
			}
			stack = append(stack, child)
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return text.String(), nil
}

func selectorForContext(
	ctx context.Context,
	roots []*html.Node,
	targetNode *html.Node,
) (string, error) {
	budget := contextSelectorBudget{}
	selector, err := selectorForWithBudgetContext(ctx, roots, targetNode, &budget)
	if err != nil {
		return contextSelectorResult(ctx, err)
	}
	return selector, ctx.Err()
}

func selectorForWithBudgetContext(
	ctx context.Context,
	roots []*html.Node,
	targetNode *html.Node,
	budget *contextSelectorBudget,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if targetNode == nil {
		return "", nil
	}
	if id, ok, err := nodeAttributeContext(ctx, targetNode, "id"); err != nil {
		return "", err
	} else if ok && id != "" && len(id) <= maxContextSelectorCandidateBytes {
		escaped, err := escapeIdentifierContext(ctx, id)
		if err != nil {
			return "", err
		}
		valid, err := legacyIDSelectorValidContext(ctx, id)
		if err != nil {
			return "", err
		}
		valid = valid && len(escaped) <= maxContextSelectorCandidateBytes-1
		candidate := ""
		if valid {
			candidate = "#" + escaped
		}
		unique, err := candidateUniquelyMatchesContext(ctx, roots, targetNode, valid, budget, func(node *html.Node) (bool, error) {
			candidateID, present, err := nodeAttributeContext(ctx, node, "id")
			return present && candidateID == id, err
		})
		if err != nil {
			return "", err
		}
		if unique {
			return candidate, nil
		}
	}

	tag, tagValid, err := legacyCandidateTagContext(ctx, targetNode.Data)
	if err != nil {
		return "", err
	}
	classes, err := classNamesNodeContext(ctx, targetNode)
	if err != nil {
		return "", err
	}
	for _, className := range classes {
		escaped, err := escapeIdentifierContext(ctx, className)
		if err != nil {
			return "", err
		}
		classValid, err := legacyClassSelectorValidContext(ctx, className)
		if err != nil {
			return "", err
		}
		candidateValid := tagValid && classValid && len(tag)+1+len(escaped) <= maxContextSelectorCandidateBytes
		candidate := ""
		if candidateValid {
			candidate = tag + "." + escaped
		}
		unique, err := candidateUniquelyMatchesContext(ctx, roots, targetNode, candidateValid, budget, func(node *html.Node) (bool, error) {
			if node.Data != tag {
				return false, nil
			}
			return elementHasClassesContext(ctx, node, []string{className})
		})
		if err != nil {
			return "", err
		}
		if unique {
			return candidate, nil
		}
	}
	if len(classes) > 1 {
		candidate := ""
		candidateLength := len(tag)
		candidateValid := tagValid && candidateLength <= maxContextSelectorCandidateBytes
		if candidateValid {
			candidate = tag
		}
		for _, className := range classes {
			escaped, err := escapeIdentifierContext(ctx, className)
			if err != nil {
				return "", err
			}
			classValid, err := legacyClassSelectorValidContext(ctx, className)
			if err != nil {
				return "", err
			}
			if candidateValid && (len(escaped) > maxContextSelectorCandidateBytes-candidateLength-1) {
				candidateValid = false
			}
			if candidateValid {
				candidate += "." + escaped
				candidateLength += 1 + len(escaped)
			}
			candidateValid = candidateValid && classValid
		}
		unique, err := candidateUniquelyMatchesContext(ctx, roots, targetNode, candidateValid, budget, func(node *html.Node) (bool, error) {
			if node.Data != tag {
				return false, nil
			}
			return elementHasClassesContext(ctx, node, classes)
		})
		if err != nil {
			return "", err
		}
		if unique {
			return candidate, nil
		}
	}

	parts := make([]selectorPart, 0, 6)
	candidateValid := true
	candidate := ""
	for current := targetNode; current != nil && current.Type == html.ElementNode; current = current.Parent {
		if len(parts) >= maxContextSelectorDepth {
			return "", contextSelectorBudgetError(ctx)
		}
		part, segment, err := nodeSelectorPartContext(ctx, current)
		if err != nil {
			return "", err
		}
		parts = append([]selectorPart{part}, parts...)
		candidateValid = candidateValid && part.valid
		newLength := len(segment)
		if candidate != "" {
			newLength += 3 + len(candidate)
		}
		if newLength > maxContextSelectorCandidateBytes {
			return "", contextSelectorBudgetError(ctx)
		}
		if candidate == "" {
			candidate = segment
		} else {
			candidate = segment + " > " + candidate
		}
		unique, err := candidateUniquelyMatchesContext(ctx, roots, targetNode, candidateValid, budget, func(node *html.Node) (bool, error) {
			return matchesSelectorPartsContext(ctx, node, parts)
		})
		if err != nil {
			return "", err
		}
		if unique {
			return candidate, nil
		}
		if current.Data == "html" {
			break
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
	}
	return "", ctx.Err()
}

type selectorPart struct {
	tag     string
	classes []string
	nth     int
	valid   bool
}

func candidateUniquelyMatchesContext(
	ctx context.Context,
	roots []*html.Node,
	target *html.Node,
	candidateValid bool,
	budget *contextSelectorBudget,
	match func(*html.Node) (bool, error),
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !candidateValid {
		return false, nil
	}
	return uniquelyMatchesContext(ctx, roots, target, budget, match)
}

func legacyIDSelectorValidContext(ctx context.Context, value string) (bool, error) {
	if err := scanStringContext(ctx, value); err != nil {
		return false, err
	}
	// IDs are parsed with cascadia's parseName. escapeIdentifierContext emits
	// only name characters or complete numeric escapes, so every non-empty ID
	// remains syntactically valid and decodes to the original value.
	return value != "", nil
}

func legacyClassSelectorValidContext(ctx context.Context, value string) (bool, error) {
	if err := scanStringContext(ctx, value); err != nil {
		return false, err
	}
	// Classes are parsed with parseIdentifier. The legacy escaper leaves
	// hyphens and non-leading digits literal; therefore the only invalid outputs
	// are all-hyphen names and a hyphen prefix followed by a digit.
	index := 0
	for index < len(value) && value[index] == '-' {
		index++
	}
	if index == len(value) {
		return false, nil
	}
	if index > 0 && value[index] >= '0' && value[index] <= '9' {
		return false, nil
	}
	return true, nil
}

func legacyTypeSelectorValidContext(ctx context.Context, value string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	index := 0
	for index < len(value) && value[index] == '-' {
		index++
		if index%alignmentContextCheckpointBytes == 0 {
			if err := ctx.Err(); err != nil {
				return false, err
			}
		}
	}
	if index == len(value) || !legacyIdentifierStart(value[index]) {
		return false, nil
	}
	for ; index < len(value); index++ {
		if index > 0 && index%alignmentContextCheckpointBytes == 0 {
			if err := ctx.Err(); err != nil {
				return false, err
			}
		}
		if !legacyIdentifierCharacter(value[index]) {
			return false, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return true, nil
}

func legacyCandidateTagContext(ctx context.Context, nodeData string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if len(nodeData) > maxContextSelectorCandidateBytes {
		return "", false, nil
	}
	tag := strings.ToLower(nodeData)
	valid, err := legacyTypeSelectorValidContext(ctx, tag)
	if err != nil {
		return "", false, err
	}
	// cascadia lowercases the parsed type selector but compares it exactly with
	// html.Node.Data. SVG adjusted names and Unicode case changes therefore do
	// not match the lowercased candidate emitted by the legacy formatter.
	return tag, valid && tag == nodeData, nil
}

func legacyIdentifierStart(value byte) bool {
	return value >= 0x80 || value == '_' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}

func legacyIdentifierCharacter(value byte) bool {
	return legacyIdentifierStart(value) || value == '-' || value >= '0' && value <= '9'
}

func scanStringContext(ctx context.Context, value string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for offset := alignmentContextCheckpointBytes; offset < len(value); offset += alignmentContextCheckpointBytes {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func uniquelyMatchesContext(
	ctx context.Context,
	roots []*html.Node,
	target *html.Node,
	budget *contextSelectorBudget,
	match func(*html.Node) (bool, error),
) (bool, error) {
	matches := 0
	targetMatched := false
	err := walkElementsContext(ctx, roots, func(node *html.Node) (bool, error) {
		budget.matchElements++
		if budget.matchElements > maxContextSelectorMatchElements {
			return false, contextSelectorBudgetError(ctx)
		}
		matched, err := match(node)
		if err != nil {
			return false, err
		}
		if !matched {
			return false, nil
		}
		matches++
		if node == target {
			targetMatched = true
		}
		return matches > 1, nil
	})
	if err != nil {
		return false, err
	}
	return matches == 1 && targetMatched, nil
}

func nodeSelectorPartContext(ctx context.Context, node *html.Node) (selectorPart, string, error) {
	tag, valid, err := legacyCandidateTagContext(ctx, node.Data)
	if err != nil {
		return selectorPart{}, "", err
	}
	part := selectorPart{tag: tag, valid: valid}
	segment := part.tag
	classes, err := classNamesNodeContext(ctx, node)
	if err != nil {
		return selectorPart{}, "", err
	}
	if len(classes) > 0 {
		part.classes = []string{classes[0]}
		classValid, err := legacyClassSelectorValidContext(ctx, classes[0])
		if err != nil {
			return selectorPart{}, "", err
		}
		part.valid = part.valid && classValid
		escaped, err := escapeIdentifierContext(ctx, classes[0])
		if err != nil {
			return selectorPart{}, "", err
		}
		if len(part.tag)+1+len(escaped) > maxContextSelectorCandidateBytes {
			return selectorPart{}, "", contextSelectorBudgetError(ctx)
		}
		segment += "." + escaped
	}
	index, total, err := siblingIndexContext(ctx, node)
	if err != nil {
		return selectorPart{}, "", err
	}
	if total > 1 {
		part.nth = index
		segment += fmt.Sprintf(":nth-of-type(%d)", index)
	}
	return part, segment, nil
}

func matchesSelectorPartsContext(ctx context.Context, node *html.Node, parts []selectorPart) (bool, error) {
	current := node
	for index := len(parts) - 1; index >= 0; index-- {
		if current == nil || current.Type != html.ElementNode || current.Data != parts[index].tag {
			return false, nil
		}
		matched, err := elementHasClassesContext(ctx, current, parts[index].classes)
		if err != nil || !matched {
			return false, err
		}
		if parts[index].nth > 0 {
			position, _, err := siblingIndexContext(ctx, current)
			if err != nil {
				return false, err
			}
			if position != parts[index].nth {
				return false, nil
			}
		}
		if index > 0 {
			current = current.Parent
		}
	}
	return true, ctx.Err()
}

func classNamesNodeContext(ctx context.Context, node *html.Node) ([]string, error) {
	classAttr, _, err := nodeAttributeContext(ctx, node, "class")
	if err != nil {
		return nil, err
	}
	if len(classAttr) > maxContextSelectorClassBytes {
		return nil, contextSelectorBudgetError(ctx)
	}
	classes, err := fieldsContext(ctx, classAttr)
	if err != nil {
		return nil, err
	}
	if err := sortStringsContext(ctx, classes); err != nil {
		return nil, err
	}
	return classes, nil
}

func sortStringsContext(ctx context.Context, values []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ticker := newContextScanTicker()
	for start := len(values)/2 - 1; start >= 0; start-- {
		if err := siftDownStringsContext(ctx, values, start, len(values), &ticker); err != nil {
			return err
		}
	}
	for end := len(values) - 1; end > 0; end-- {
		values[0], values[end] = values[end], values[0]
		if err := ticker.step(ctx); err != nil {
			return err
		}
		if err := siftDownStringsContext(ctx, values, 0, end, &ticker); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func siftDownStringsContext(
	ctx context.Context,
	values []string,
	root, end int,
	ticker *contextScanTicker,
) error {
	for {
		child := root*2 + 1
		if child >= end {
			return nil
		}
		if child+1 < end {
			less, err := stringLessContext(ctx, values[child], values[child+1], ticker)
			if err != nil {
				return err
			}
			if less {
				child++
			}
		}
		less, err := stringLessContext(ctx, values[root], values[child], ticker)
		if err != nil {
			return err
		}
		if !less {
			return nil
		}
		values[root], values[child] = values[child], values[root]
		if err := ticker.step(ctx); err != nil {
			return err
		}
		root = child
	}
}

func stringLessContext(
	ctx context.Context,
	left, right string,
	ticker *contextScanTicker,
) (bool, error) {
	length := len(left)
	if len(right) < length {
		length = len(right)
	}
	for index := 0; index < length; index++ {
		if err := ticker.step(ctx); err != nil {
			return false, err
		}
		if left[index] != right[index] {
			return left[index] < right[index], nil
		}
	}
	if err := ticker.step(ctx); err != nil {
		return false, err
	}
	return len(left) < len(right), nil
}

func elementHasClassesContext(ctx context.Context, node *html.Node, required []string) (bool, error) {
	if len(required) == 0 {
		return true, ctx.Err()
	}
	classAttr, _, err := nodeAttributeContext(ctx, node, "class")
	if err != nil {
		return false, err
	}
	if len(classAttr) > maxContextSelectorClassBytes {
		return false, contextSelectorBudgetError(ctx)
	}
	available, err := cssClassFieldsContext(ctx, classAttr)
	if err != nil {
		return false, err
	}
	for _, wanted := range required {
		found := false
		for index, candidate := range available {
			if index%256 == 0 {
				if err := ctx.Err(); err != nil {
					return false, err
				}
			}
			if candidate == wanted {
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
	}
	return true, ctx.Err()
}

// cssClassFieldsContext uses CSS's ASCII whitespace definition. Candidate
// generation intentionally uses Unicode fields above to preserve the legacy
// output ordering, while selector matching follows cascadia/CSS semantics.
func cssClassFieldsContext(ctx context.Context, value string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fields := make([]string, 0)
	start := -1
	for index := 0; index < len(value); index++ {
		if index > 0 && index%alignmentContextCheckpointBytes == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		space := false
		switch value[index] {
		case ' ', '\t', '\r', '\n', '\f':
			space = true
		}
		if space {
			if start >= 0 {
				if len(fields) >= maxContextSelectorClasses {
					return nil, contextSelectorBudgetError(ctx)
				}
				fields = append(fields, value[start:index])
				start = -1
			}
			continue
		}
		if start < 0 {
			start = index
		}
	}
	if start >= 0 {
		if len(fields) >= maxContextSelectorClasses {
			return nil, contextSelectorBudgetError(ctx)
		}
		fields = append(fields, value[start:])
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return fields, nil
}

func nodeAttributeContext(ctx context.Context, node *html.Node, name string) (string, bool, error) {
	for index, attribute := range node.Attr {
		if index%256 == 0 {
			if err := ctx.Err(); err != nil {
				return "", false, err
			}
		}
		if attribute.Key == name {
			return attribute.Val, true, nil
		}
	}
	return "", false, ctx.Err()
}

func fieldsContext(ctx context.Context, value string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fields := make([]string, 0)
	start := -1
	nextCheckpoint := alignmentContextCheckpointBytes
	for offset, character := range value {
		if offset >= nextCheckpoint {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			for nextCheckpoint <= offset {
				nextCheckpoint += alignmentContextCheckpointBytes
			}
		}
		if unicode.IsSpace(character) {
			if start >= 0 {
				if len(fields) >= maxContextSelectorClasses {
					return nil, contextSelectorBudgetError(ctx)
				}
				fields = append(fields, value[start:offset])
				start = -1
			}
			continue
		}
		if start < 0 {
			start = offset
		}
	}
	if start >= 0 {
		if len(fields) >= maxContextSelectorClasses {
			return nil, contextSelectorBudgetError(ctx)
		}
		fields = append(fields, value[start:])
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return fields, nil
}

func siblingIndexContext(ctx context.Context, node *html.Node) (index, total int, err error) {
	index = 1
	if node.Parent == nil {
		return index, 1, ctx.Err()
	}
	processed := 0
	for sibling := node.Parent.FirstChild; sibling != nil; sibling = sibling.NextSibling {
		processed++
		if processed%256 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, 0, err
			}
		}
		if sibling.Type != html.ElementNode || sibling.Data != node.Data {
			continue
		}
		total++
		if sibling == node {
			index = total
		}
	}
	return index, total, ctx.Err()
}

func nodeDepthContext(ctx context.Context, node *html.Node) (int, error) {
	depth := 0
	for current := node.Parent; current != nil; current = current.Parent {
		depth++
		if depth%256 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
	}
	return depth, ctx.Err()
}

func escapeIdentifierContext(ctx context.Context, value string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var escaped strings.Builder
	nextCheckpoint := alignmentContextCheckpointBytes
	for index, r := range value {
		if index >= nextCheckpoint {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			for nextCheckpoint <= index {
				nextCheckpoint += alignmentContextCheckpointBytes
			}
		}
		if (unicode.IsLetter(r) || r == '_' || r == '-' || (index > 0 && unicode.IsDigit(r))) && r < unicode.MaxASCII {
			escaped.WriteRune(r)
			continue
		}
		fmt.Fprintf(&escaped, `\%x `, r)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return escaped.String(), nil
}

func selectorFor(doc, target *goquery.Selection) string {
	node := target.Nodes[0]
	if id, ok := target.Attr("id"); ok && id != "" {
		candidate := "#" + escapeIdentifier(id)
		if uniquelySelects(doc, target, candidate) {
			return candidate
		}
	}

	tag := strings.ToLower(node.Data)
	classes := classNames(target)
	for _, className := range classes {
		candidate := tag + "." + escapeIdentifier(className)
		if uniquelySelects(doc, target, candidate) {
			return candidate
		}
	}
	if len(classes) > 1 {
		candidate := tag
		for _, className := range classes {
			candidate += "." + escapeIdentifier(className)
		}
		if uniquelySelects(doc, target, candidate) {
			return candidate
		}
	}

	segments := make([]string, 0, 6)
	current := node
	for current != nil && current.Type == html.ElementNode {
		segment := nodeSegment(current)
		segments = append([]string{segment}, segments...)
		candidate := strings.Join(segments, " > ")
		if uniquelySelects(doc, target, candidate) {
			return candidate
		}
		if current.Data == "html" {
			break
		}
		current = current.Parent
	}
	return ""
}

func nodeSegment(node *html.Node) string {
	segment := strings.ToLower(node.Data)
	selection := goquery.NewDocumentFromNode(node).Selection
	classes := classNames(selection)
	if len(classes) > 0 {
		segment += "." + escapeIdentifier(classes[0])
	}
	if index, total := siblingIndex(node); total > 1 {
		segment += fmt.Sprintf(":nth-of-type(%d)", index)
	}
	return segment
}

func siblingIndex(node *html.Node) (index, total int) {
	index = 1
	if node.Parent == nil {
		return index, 1
	}
	for sibling := node.Parent.FirstChild; sibling != nil; sibling = sibling.NextSibling {
		if sibling.Type != html.ElementNode || sibling.Data != node.Data {
			continue
		}
		total++
		if sibling == node {
			index = total
		}
	}
	return index, total
}

func uniquelySelects(doc, target *goquery.Selection, selector string) bool {
	matched := doc.Find(selector)
	return matched.Length() == 1 && matched.Nodes[0] == target.Nodes[0]
}

func classNames(selection *goquery.Selection) []string {
	classAttr, _ := selection.Attr("class")
	classes := strings.Fields(classAttr)
	filtered := classes[:0]
	for _, className := range classes {
		if className != "" {
			filtered = append(filtered, className)
		}
	}
	sort.Strings(filtered)
	return filtered
}

func nodeDepth(node *html.Node) int {
	depth := 0
	for current := node.Parent; current != nil; current = current.Parent {
		depth++
	}
	return depth
}

func escapeIdentifier(value string) string {
	var escaped strings.Builder
	for index, r := range value {
		if (unicode.IsLetter(r) || r == '_' || r == '-' || (index > 0 && unicode.IsDigit(r))) && r < unicode.MaxASCII {
			escaped.WriteRune(r)
			continue
		}
		fmt.Fprintf(&escaped, `\%x `, r)
	}
	return escaped.String()
}
