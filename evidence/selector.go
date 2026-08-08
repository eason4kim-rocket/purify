package evidence

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
)

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
