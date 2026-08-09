package discovery

import (
	"container/heap"
	"fmt"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/use-agent/purify/publicnet"
	"golang.org/x/net/publicsuffix"
)

func normalizeURL(rawURL string, base *url.URL, allowPrivate bool) (string, *url.URL, error) {
	return publicnet.NormalizeHTTPURL(rawURL, base, allowPrivate)
}

type siteScope struct {
	authority   string
	hostname    string
	registrable string
	exact       bool
}

func newSiteScope(root *url.URL) siteScope {
	scope := siteScope{authority: root.Host, hostname: strings.ToLower(root.Hostname())}
	if _, err := netip.ParseAddr(scope.hostname); err == nil || scope.hostname == "localhost" {
		scope.exact = true
		return scope
	}
	registrable, err := publicsuffix.EffectiveTLDPlusOne(scope.hostname)
	if err != nil {
		scope.exact = true
		return scope
	}
	scope.registrable = strings.ToLower(registrable)
	return scope
}

func (scope siteScope) allows(candidate *url.URL) bool {
	if candidate == nil {
		return false
	}
	if scope.exact {
		return candidate.Host == scope.authority
	}
	registrable, err := publicsuffix.EffectiveTLDPlusOne(strings.ToLower(candidate.Hostname()))
	return err == nil && strings.EqualFold(registrable, scope.registrable)
}

type urlCollector struct {
	mu           sync.Mutex
	scope        siteScope
	maximum      int
	allowPrivate bool
	values       map[string]int
	candidates   rankedURLHeap
	warnings     *warningCollector
	truncated    bool
	limitWarned  bool
}

func newURLCollector(root *url.URL, maximum, maximumWarnings int, allowPrivate bool) *urlCollector {
	initialCapacity := maximum
	if initialCapacity > 1024 {
		initialCapacity = 1024
	}
	return &urlCollector{
		scope:        newSiteScope(root),
		maximum:      maximum,
		allowPrivate: allowPrivate,
		values:       make(map[string]int, initialCapacity),
		warnings:     newWarningCollector(maximumWarnings),
	}
}

func (collector *urlCollector) add(rawURL string, base *url.URL, source string) bool {
	canonical, parsed, err := normalizeURL(rawURL, base, collector.allowPrivate)
	if err != nil {
		collector.warnings.add(Warning{Source: source, URL: strings.TrimSpace(rawURL), Code: "invalid_url", Message: err.Error()})
		return false
	}
	if !collector.scope.allows(parsed) {
		return false
	}
	return collector.addCanonical(canonical, source)
}

func (collector *urlCollector) addCanonical(canonical, source string) bool {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	rank := sourceRank(source)
	if existingRank, exists := collector.values[canonical]; exists {
		if rank < existingRank {
			collector.values[canonical] = rank
			heap.Push(&collector.candidates, rankedURL{value: canonical, rank: rank})
		}
		return false
	}
	if len(collector.values) >= collector.maximum {
		collector.truncated = true
		if !collector.limitWarned {
			collector.limitWarned = true
			collector.warnings.add(Warning{
				Source:  source,
				URL:     canonical,
				Code:    "url_limit",
				Message: fmt.Sprintf("URL limit %d reached", collector.maximum),
			})
		}
		collector.pruneHeap()
		worst := collector.candidates[0]
		candidate := rankedURL{value: canonical, rank: rank}
		if !candidate.betterThan(worst) {
			return false
		}
		heap.Pop(&collector.candidates)
		delete(collector.values, worst.value)
	}
	collector.values[canonical] = rank
	heap.Push(&collector.candidates, rankedURL{value: canonical, rank: rank})
	return true
}

func (collector *urlCollector) pruneHeap() {
	for len(collector.candidates) > 0 {
		candidate := collector.candidates[0]
		if rank, exists := collector.values[candidate.value]; exists && rank == candidate.rank {
			return
		}
		heap.Pop(&collector.candidates)
	}
}

func (collector *urlCollector) sorted() []string {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	values := make([]string, 0, len(collector.values))
	for value := range collector.values {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

func sourceRank(source string) int {
	switch source {
	case "root":
		return 0
	case "sitemap":
		return 1
	case "homepage":
		return 2
	default:
		return 3
	}
}

type rankedURL struct {
	value string
	rank  int
}

func (candidate rankedURL) betterThan(other rankedURL) bool {
	return candidate.rank < other.rank || candidate.rank == other.rank && candidate.value < other.value
}

type rankedURLHeap []rankedURL

func (values rankedURLHeap) Len() int { return len(values) }
func (values rankedURLHeap) Less(i, j int) bool {
	// A max-heap keeps the worst retained candidate at index zero.
	return values[j].betterThan(values[i])
}
func (values rankedURLHeap) Swap(i, j int) { values[i], values[j] = values[j], values[i] }
func (values *rankedURLHeap) Push(value any) {
	*values = append(*values, value.(rankedURL))
}
func (values *rankedURLHeap) Pop() any {
	old := *values
	last := old[len(old)-1]
	*values = old[:len(old)-1]
	return last
}

func (collector *urlCollector) warningList() []Warning {
	return collector.warnings.list()
}

func (collector *urlCollector) wasTruncated() bool {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	return collector.truncated
}
