package indexer

import (
	"net/url"
	"path"
	"strings"
)

// junkURL reports whether a URL can never lead to an indexable document the
// index serves: a foreign-locale variant, a non-document asset, or a crawler
// trap. It guards both link admission and frontier pruning, so tightening a
// rule here both stops new junk and lets an existing queue be cleaned of it.
func junkURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return true
	}
	if _, skip := assetExtensions[strings.ToLower(path.Ext(parsed.Path))]; skip {
		return true
	}
	return foreignLocalePath(rawURL) || foreignLocaleHost(parsed.Hostname()) || trappedPath(parsed.EscapedPath())
}

// foreignLocaleHost reports whether the host's first label is a foreign
// language subtag. Multi-locale sites also key translations on subdomains
// (es.tldp.org, bn.wikipedia.org), which the path check never sees. en and
// zh labels are not in the table and pass, as do ordinary service names
// (www, docs, developer). A service that happens to use a two-letter code
// for something else (id.example.com) is sacrificed for the budget.
func foreignLocaleHost(hostname string) bool {
	label, _, found := strings.Cut(strings.ToLower(hostname), ".")
	if !found {
		return false
	}
	_, foreign := foreignLocaleSubtags[label]
	return foreign
}

const (
	maxPathSegments   = 16
	maxSegmentRepeats = 3
)

// trappedPath reports whether a URL path walks into crawler-trap shapes:
// self-referential relative links that stack the same segment endlessly
// (vger.kernel.org/_sources/_sources/...), content-addressed archive
// metadata trees (dists/.../by-hash/SHA256/<digest>), or absurd depth.
func trappedPath(escapedPath string) bool {
	segments := strings.Split(strings.Trim(escapedPath, "/"), "/")
	if len(segments) > maxPathSegments {
		return true
	}
	counts := map[string]int{}
	for _, segment := range segments {
		if segment == "by-hash" {
			return true
		}
		if segment == "" {
			continue
		}
		counts[segment]++
		if counts[segment] >= maxSegmentRepeats {
			return true
		}
	}
	return false
}
