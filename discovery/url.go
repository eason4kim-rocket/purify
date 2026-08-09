package discovery

import (
	"container/heap"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
)

func normalizeURL(rawURL string, base *url.URL, allowPrivate bool) (string, *url.URL, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", nil, errors.New("URL is empty")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", nil, fmt.Errorf("parse URL: %w", err)
	}
	if base != nil && !parsed.IsAbs() {
		parsed = base.ResolveReference(parsed)
	}
	if !parsed.IsAbs() || parsed.Host == "" {
		return "", nil, errors.New("URL must be absolute")
	}
	if parsed.Opaque != "" {
		return "", nil, errors.New("opaque URLs are not supported")
	}
	if parsed.User != nil {
		return "", nil, errors.New("URL userinfo is not supported")
	}

	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", nil, errors.New("URL scheme must be http or https")
	}
	hostname := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if hostname == "" || strings.Contains(hostname, "%") {
		return "", nil, errors.New("URL host is invalid")
	}
	canonicalHost := ""
	if address, parseErr := netip.ParseAddr(hostname); parseErr == nil {
		if address.Zone() != "" {
			return "", nil, errors.New("IPv6 zones are not supported")
		}
		if !allowPrivate && !isPublicAddress(address) {
			return "", nil, errors.New("private or local address is not allowed")
		}
		canonicalHost = address.String()
	} else {
		canonicalHost, err = idna.Lookup.ToASCII(hostname)
		if err != nil {
			return "", nil, fmt.Errorf("normalize host: %w", err)
		}
		canonicalHost = strings.TrimSuffix(strings.ToLower(canonicalHost), ".")
		if !allowPrivate && (canonicalHost == "localhost" || strings.HasSuffix(canonicalHost, ".localhost")) {
			return "", nil, errors.New("localhost is not allowed")
		}
	}

	port := parsed.Port()
	if port == "" && explicitEmptyPort(parsed.Host) {
		return "", nil, errors.New("URL port is empty")
	}
	if port != "" {
		number, convertErr := strconv.Atoi(port)
		if convertErr != nil || number < 1 || number > 65_535 {
			return "", nil, errors.New("URL port must be between 1 and 65535")
		}
		port = strconv.Itoa(number)
		if scheme == "http" && port == "80" || scheme == "https" && port == "443" {
			port = ""
		}
	}
	parsed.Scheme = scheme
	switch {
	case port != "":
		if strings.Contains(canonicalHost, ":") {
			parsed.Host = "[" + canonicalHost + "]:" + port
		} else {
			parsed.Host = canonicalHost + ":" + port
		}
	case strings.Contains(canonicalHost, ":"):
		parsed.Host = "[" + canonicalHost + "]"
	default:
		parsed.Host = canonicalHost
	}
	if parsed.Path == "" {
		parsed.Path = "/"
		parsed.RawPath = ""
	}
	parsed.Fragment = ""
	parsed.RawFragment = ""
	return parsed.String(), parsed, nil
}

func explicitEmptyPort(authority string) bool {
	if strings.HasPrefix(authority, "[") {
		closing := strings.LastIndex(authority, "]")
		return closing >= 0 && authority[closing+1:] == ":"
	}
	return strings.HasSuffix(authority, ":")
}

func isPublicAddress(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() ||
		address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsUnspecified() {
		return false
	}
	for _, prefix := range reservedAddressPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

var reservedAddressPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001:db8::/32"),
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
