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

// wikiEditionSubtags are sizeable three-letter language editions seen on
// Wikipedia-style interlanguage links; three-letter labels are otherwise
// legitimate service names (doc, api, dev) and cannot be blocked wholesale.
var wikiEditionSubtags = map[string]struct{}{
	"ace": {}, "arz": {}, "azb": {}, "bar": {}, "bcl": {}, "ceb": {},
	"ckb": {}, "hak": {}, "ilo": {}, "min": {}, "nap": {}, "nds": {},
	"pms": {}, "scn": {}, "sco": {}, "szl": {}, "vec": {}, "war": {},
	"wuu": {}, "yue": {}, "zea": {},
}

// foreignLocaleHost reports whether the host's first label marks a foreign
// language edition. Multi-locale sites key translations on subdomains
// (es.tldp.org, bn.wikipedia.org), which the path check never sees, and
// Wikipedia alone has over three hundred language editions — far more than
// any subtag table enumerates. On hosts with a subdomain under a
// registrable domain (three labels or more), every alphabetic two-letter
// first label except the languages the index serves is treated as a locale
// — a two-letter service name caught by that net (hg.mozilla.org) is
// sacrificed for the budget — plus the known three-letter editions. A bare
// registrable domain (go.dev) is never a locale host.
func foreignLocaleHost(hostname string) bool {
	hostname = strings.ToLower(hostname)
	if strings.Count(hostname, ".") < 2 {
		return false
	}
	label, _, _ := strings.Cut(hostname, ".")
	if label == "en" || label == "zh" {
		return false
	}
	if len(label) == 2 && isAlpha(label) {
		return true
	}
	if _, wiki := wikiEditionSubtags[label]; wiki {
		return true
	}
	_, foreign := foreignLocaleSubtags[label]
	return foreign
}

func isAlpha(value string) bool {
	for _, r := range value {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

const (
	maxPathSegments   = 16
	maxSegmentRepeats = 3
)

// trappedPath reports whether a URL path walks into crawler-trap shapes:
// self-referential relative links that stack segments endlessly — the same
// segment three times (vger.kernel.org/_sources/_sources/_sources/), or two
// different segments cycling (smalltalk.gnu.org/manual-base/manual-libs/
// manual-base/manual-libs/) — content-addressed archive metadata trees
// (dists/.../by-hash/SHA256/<digest>), pre-release documentation channels
// that mirror the stable tree page for page, or absurd depth.
func trappedPath(escapedPath string) bool {
	segments := strings.Split(strings.Trim(escapedPath, "/"), "/")
	if len(segments) > maxPathSegments {
		return true
	}
	if first := segments[0]; first == "beta" || first == "nightly" {
		return true
	}
	counts := map[string]int{}
	repeated := 0
	for _, segment := range segments {
		if segment == "by-hash" {
			return true
		}
		if segment == "" {
			continue
		}
		counts[segment]++
		switch counts[segment] {
		case maxSegmentRepeats:
			return true
		case 2:
			repeated++
			if repeated >= 2 {
				return true
			}
		}
	}
	return false
}
