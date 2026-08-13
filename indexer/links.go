package indexer

import (
	"net/url"
	"path"
	"strings"

	"github.com/use-agent/purify/cleaner"
	"github.com/use-agent/purify/searchindex"
)

// maxLinksPerPage bounds one page's frontier contribution. Documentation
// sidebars run to a few hundred links; hub pages beyond that are link
// dumps whose tail adds noise faster than coverage.
const maxLinksPerPage = 400

// assetExtensions are path suffixes that never lead to an indexable document.
var assetExtensions = map[string]struct{}{
	".7z": {}, ".apk": {}, ".atom": {}, ".avi": {}, ".bz2": {}, ".css": {},
	".deb": {}, ".dmg": {}, ".eot": {}, ".exe": {}, ".gif": {}, ".gz": {},
	".ico": {}, ".iso": {}, ".jar": {}, ".jpeg": {}, ".jpg": {}, ".js": {},
	".json": {}, ".m4a": {}, ".m4v": {}, ".map": {}, ".mjs": {}, ".mkv": {},
	".mov": {}, ".mp3": {}, ".mp4": {}, ".msi": {}, ".otf": {}, ".pdf": {},
	".png": {}, ".rar": {}, ".rpm": {}, ".rss": {}, ".svg": {}, ".tar": {},
	".tgz": {}, ".ttf": {}, ".wasm": {}, ".webm": {}, ".webp": {}, ".woff": {},
	".woff2": {}, ".xml": {}, ".xz": {}, ".zip": {},
}

// collectLinks extracts one fetched page's outbound links and returns the
// frontier candidates the crawl may grow into: same registrable root only, so
// link discovery deepens the curated seed sites instead of wandering off
// them; no query strings, which on documentation sites mark search, facet,
// and version-picker permutations rather than distinct documents; and the
// same locale rules the seed pass applies.
func collectLinks(rawHTML, baseURL, root string, allowPrivate bool) []searchindex.FrontierItem {
	// Internal/external is a same-host split; the same-root rule below spans
	// subdomains, so both lists stay in play.
	extracted := cleaner.ExtractLinks(rawHTML, baseURL)
	candidates := make([]string, 0, len(extracted.Internal)+len(extracted.External))
	for _, link := range extracted.Internal {
		candidates = append(candidates, link.Href)
	}
	for _, link := range extracted.External {
		candidates = append(candidates, link.Href)
	}

	baseCanonical := ""
	if canonical, _, err := pageRoot(baseURL, allowPrivate); err == nil {
		baseCanonical = canonical
	}
	seen := map[string]struct{}{}
	items := make([]searchindex.FrontierItem, 0, min(len(candidates), maxLinksPerPage))
	for _, href := range candidates {
		if len(items) >= maxLinksPerPage {
			break
		}
		parsed, err := url.Parse(href)
		if err != nil || parsed.RawQuery != "" {
			continue
		}
		if _, skip := assetExtensions[strings.ToLower(path.Ext(parsed.Path))]; skip {
			continue
		}
		canonical, linkRoot, err := pageRoot(href, allowPrivate)
		if err != nil || linkRoot != root || canonical == baseCanonical {
			continue
		}
		if foreignLocalePath(canonical) {
			continue
		}
		if _, dup := seen[canonical]; dup {
			continue
		}
		seen[canonical] = struct{}{}
		items = append(items, searchindex.FrontierItem{URL: canonical, Root: linkRoot})
	}
	return items
}
