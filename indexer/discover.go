package indexer

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/use-agent/purify/discovery"
	"github.com/use-agent/purify/publicnet"
	"github.com/use-agent/purify/searchindex"
	"golang.org/x/net/publicsuffix"
)

// Discoverer expands a seed URL into a bounded set of same-site URLs.
type Discoverer interface {
	Discover(context.Context, string) (*discovery.Result, error)
}

// SeedFrontier runs discovery for each seed and enqueues unique URLs.
func SeedFrontier(ctx context.Context, store *searchindex.Store, discoverer Discoverer, seeds []string, allowPrivate bool) (int, error) {
	if store == nil || discoverer == nil {
		return 0, fmt.Errorf("indexer: store and discoverer are required")
	}
	inserted := 0
	seedCanonicals := make([]string, 0, len(seeds))
	for _, seed := range seeds {
		if err := ctx.Err(); err != nil {
			return inserted, err
		}
		result, err := discoverer.Discover(ctx, seed)
		if err != nil && result == nil {
			return inserted, err
		}
		items := make([]searchindex.FrontierItem, 0, len(result.URLs)+1)
		for _, rawURL := range append([]string{seed}, result.URLs...) {
			canonical, root, normErr := pageRoot(rawURL, allowPrivate)
			if normErr != nil {
				continue
			}
			if junkURL(canonical) {
				continue
			}
			if rawURL == seed {
				seedCanonicals = append(seedCanonicals, canonical)
			}
			items = append(items, searchindex.FrontierItem{URL: canonical, Root: root})
		}
		n, enqueueErr := store.Enqueue(ctx, items)
		if enqueueErr != nil {
			return inserted, enqueueErr
		}
		inserted += n
	}
	// On a frontier drained by earlier runs every seed row is already done,
	// so nothing would ever be fetched and in-crawl link discovery could not
	// start. Refetching the seed pages each run restarts the cascade and
	// picks up links the front pages gained since.
	if _, err := store.Requeue(ctx, seedCanonicals); err != nil {
		return inserted, err
	}
	return inserted, nil
}

// foreignLocaleSubtags are primary language subtags the index does not serve.
// Multi-locale documentation sites (MDN, python.org, learn.microsoft.com,
// kubernetes.io) key the locale on the first path segment, so sitemaps hand
// the crawler every translation of every page; this check keeps the budget on
// English and Chinese content. Codes that collide with common technical paths
// ("js", "go", "css") are not ISO 639-1 languages and stay crawlable.
var foreignLocaleSubtags = map[string]struct{}{
	"af": {}, "ar": {}, "az": {}, "bg": {}, "bn": {}, "bs": {}, "ca": {},
	"cs": {}, "cy": {}, "da": {}, "de": {}, "el": {}, "es": {}, "et": {},
	"eu": {}, "fa": {}, "fi": {}, "fr": {}, "ga": {}, "gl": {}, "gu": {},
	"he": {}, "hi": {}, "hr": {}, "hu": {}, "hy": {}, "id": {}, "is": {},
	"it": {}, "ja": {}, "ka": {}, "kk": {}, "km": {}, "kn": {}, "ko": {},
	"lo": {}, "lt": {}, "lv": {}, "mk": {}, "ml": {}, "mn": {}, "mr": {},
	"ms": {}, "my": {}, "nb": {}, "ne": {}, "nl": {}, "no": {}, "pl": {},
	"pt": {}, "ro": {}, "ru": {}, "si": {}, "sk": {}, "sl": {}, "sq": {},
	"sr": {}, "sv": {}, "sw": {}, "ta": {}, "te": {}, "th": {}, "tr": {},
	"uk": {}, "ur": {}, "uz": {}, "vi": {},
}

// foreignLocalePath reports whether the URL's first path segment is a locale
// tag for a language the index does not serve. en/zh variants always pass.
// The primary subtag is matched case-sensitively: locale segments are
// lowercase by convention ("/tr/", "/pt-BR/"), so an uppercase segment like
// W3C's "/TR/" (Technical Reports) is not a locale and stays crawlable.
func foreignLocalePath(pageURL string) bool {
	parsed, err := url.Parse(pageURL)
	if err != nil {
		return false
	}
	segment := strings.Trim(parsed.EscapedPath(), "/")
	if at := strings.IndexByte(segment, '/'); at >= 0 {
		segment = segment[:at]
	}
	primary, _, _ := strings.Cut(segment, "-")
	if len(primary) < 2 || len(primary) > 3 {
		return false
	}
	_, foreign := foreignLocaleSubtags[primary]
	return foreign
}

func pageRoot(rawURL string, allowPrivate bool) (string, string, error) {
	canonical, parsed, err := publicnet.NormalizeHTTPURL(rawURL, nil, allowPrivate)
	if err != nil {
		return "", "", err
	}
	host := strings.ToLower(parsed.Hostname())
	root, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		root = host
	}
	if parsed.Path == "" {
		canonical = strings.TrimRight(canonical, "/")
		if u, perr := url.Parse(canonical); perr == nil && u.Path == "" {
			canonical += "/"
		}
	}
	return canonical, strings.ToLower(root), nil
}
