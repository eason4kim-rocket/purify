package crawl

import (
	"errors"
	"net/url"
	"testing"
)

func TestNormalizeURLCanonicalizesAuthorityAndPreservesResourceIdentity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "idna case trailing dot default port empty path and fragment",
			raw:  "HTTPS://BÜCHER.Example.:443#section",
			want: "https://xn--bcher-kva.example/",
		},
		{
			name: "http default port with leading zero",
			raw:  "http://Example.COM:080",
			want: "http://example.com/",
		},
		{
			name: "nondefault port and query order",
			raw:  "https://Example.COM:8443/path?z=2&a=1#discarded",
			want: "https://example.com:8443/path?z=2&a=1",
		},
		{
			name: "canonical ipv6",
			raw:  "http://[2001:0db8:0:0:0:0:0:1]:80",
			want: "http://[2001:db8::1]/",
		},
		{
			name: "escaped slash raw path",
			raw:  "https://example.com/a%2Fb/c?q=one%2Ftwo#fragment",
			want: "https://example.com/a%2Fb/c?q=one%2Ftwo",
		},
		{
			name: "absolute dot segments and repeated slashes are preserved",
			raw:  "https://example.com/a/../b//c",
			want: "https://example.com/a/../b//c",
		},
		{
			name: "empty query marker",
			raw:  "https://example.com/path?",
			want: "https://example.com/path?",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, parsed, err := normalizeURL(test.raw, nil)
			if err != nil {
				t.Fatalf("normalizeURL() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("normalizeURL() = %q, want %q", got, test.want)
			}
			if parsed.String() != test.want {
				t.Fatalf("parsed.String() = %q, want %q", parsed.String(), test.want)
			}
			if parsed.Fragment != "" || parsed.RawFragment != "" {
				t.Fatalf("fragment was not removed: %#v", parsed)
			}
		})
	}
}

func TestNormalizeURLResolvesRelativeReferencesWithoutCleaningPathOrQuery(t *testing.T) {
	t.Parallel()

	_, base, err := normalizeURL("https://Example.COM/docs/a%2Fb/page?old=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		raw  string
		want string
	}{
		{raw: "../next?q=2&q=1#part", want: "https://example.com/docs/next?q=2&q=1"},
		{raw: "?replacement=1", want: "https://example.com/docs/a%2Fb/page?replacement=1"},
		{raw: "#fragment-only", want: "https://example.com/docs/a%2Fb/page?old=1"},
		{raw: "//API.Example.COM:443/v1", want: "https://api.example.com/v1"},
	}
	for _, test := range tests {
		got, _, err := normalizeURL(test.raw, base)
		if err != nil {
			t.Fatalf("normalizeURL(%q) error = %v", test.raw, err)
		}
		if got != test.want {
			t.Fatalf("normalizeURL(%q) = %q, want %q", test.raw, got, test.want)
		}
	}
}

func TestNormalizeRootURLRejectsUnsafeOrMalformedAuthorities(t *testing.T) {
	t.Parallel()

	tests := []string{
		"",
		"/relative",
		"ftp://example.com/file",
		"http:opaque",
		"https://user:pass@example.com/",
		"http://[fe80::1%25en0]/",
		"https:///missing-host",
		"https://example.com:",
		"https://example.com:0/",
		"https://example.com:65536/",
		"https://example.com:invalid/",
		"https://\u200d.example/",
	}

	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			canonical, parsed, err := normalizeRootURL(raw)
			if !errors.Is(err, ErrInvalidURL) {
				t.Fatalf("normalizeRootURL() error = %v, want %v", err, ErrInvalidURL)
			}
			if canonical != "" || parsed != nil {
				t.Fatalf("normalizeRootURL() = (%q, %#v), want empty result", canonical, parsed)
			}
		})
	}
}

func TestScopeRuleDomainUsesCanonicalAuthorityAndIgnoresScheme(t *testing.T) {
	t.Parallel()

	root := mustNormalizedURL(t, "https://Example.COM:443/start")
	rule := newScopeRule(scopeDomain, root)
	tests := []struct {
		raw  string
		want bool
	}{
		{raw: "http://example.com:80/other", want: true},
		{raw: "https://example.com/other?q=1", want: true},
		{raw: "https://example.com:8443/other", want: false},
		{raw: "https://api.example.com/other", want: false},
	}
	for _, test := range tests {
		if got := rule.allows(mustNormalizedURL(t, test.raw)); got != test.want {
			t.Fatalf("domain allows(%q) = %v, want %v", test.raw, got, test.want)
		}
	}

	nondefault := newScopeRule(scopeDomain, mustNormalizedURL(t, "https://example.com:8443/start"))
	if !nondefault.allows(mustNormalizedURL(t, "http://EXAMPLE.com:8443/other")) {
		t.Fatal("domain scope treated scheme as part of canonical authority")
	}
}

func TestScopeRuleSubdomainUsesPublicSuffixAndExactLocalFallback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		root       string
		candidate  string
		wantAllows bool
	}{
		{name: "co uk sibling", root: "https://www.example.co.uk/", candidate: "https://api.example.co.uk/v1", wantAllows: true},
		{name: "co uk attacker", root: "https://www.example.co.uk/", candidate: "https://example.co.uk.evil.test/", wantAllows: false},
		{name: "different co uk registrant", root: "https://www.example.co.uk/", candidate: "https://other.co.uk/", wantAllows: false},
		{name: "private suffix isolated", root: "https://one.github.io/", candidate: "https://two.github.io/", wantAllows: false},
		{name: "private suffix child", root: "https://one.github.io/", candidate: "https://docs.one.github.io/", wantAllows: true},
		{name: "unicode sibling", root: "https://www.bücher.example/", candidate: "https://api.bücher.example/", wantAllows: true},
		{name: "same ipv4 authority", root: "http://127.0.0.1:8080/", candidate: "https://127.0.0.1:8080/other", wantAllows: true},
		{name: "same ipv4 different port", root: "http://127.0.0.1:8080/", candidate: "http://127.0.0.1:9090/other", wantAllows: true},
		{name: "same localhost authority", root: "http://localhost:3000/", candidate: "https://LOCALHOST:3000/other", wantAllows: true},
		{name: "same localhost different port", root: "http://localhost:3000/", candidate: "http://localhost:4000/other", wantAllows: true},
		{name: "localhost subname", root: "http://localhost:3000/", candidate: "http://api.localhost:3000/other", wantAllows: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rule := newScopeRule(scopeSubdomain, mustNormalizedURL(t, test.root))
			if got := rule.allows(mustNormalizedURL(t, test.candidate)); got != test.wantAllows {
				t.Fatalf("allows() = %v, want %v", got, test.wantAllows)
			}
		})
	}

	page := newScopeRule(scopePage, mustNormalizedURL(t, "https://example.com/"))
	if page.allows(mustNormalizedURL(t, "https://example.com/other")) {
		t.Fatal("page scope followed a link")
	}
}

func TestExcludePatternsMatchNormalizedPathAndURL(t *testing.T) {
	t.Parallel()

	if err := validateExcludePatterns([]string{"/private/*", "*.pdf", "https://example.com/query*"}); err != nil {
		t.Fatalf("validateExcludePatterns() error = %v", err)
	}
	if err := validateExcludePatterns([]string{"["}); !errors.Is(err, ErrInvalidExcludePattern) {
		t.Fatalf("validateExcludePatterns(invalid) error = %v, want %v", err, ErrInvalidExcludePattern)
	}

	tests := []struct {
		raw      string
		patterns []string
		want     bool
	}{
		{raw: "https://example.com/private/report", patterns: []string{"/private/*"}, want: true},
		{raw: "https://example.com/files/report.pdf", patterns: []string{"*.pdf"}, want: true},
		{raw: "https://example.com/query?a=1", patterns: []string{"https://example.com/query*"}, want: true},
		{raw: "https://example.com/public/report.html", patterns: []string{"/private/*", "*.pdf"}, want: false},
		{raw: "https://example.com/?q=1", patterns: []string{""}, want: false},
	}
	for _, test := range tests {
		canonical, parsed, err := normalizeURL(test.raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := isExcluded(canonical, parsed, test.patterns); got != test.want {
			t.Fatalf("isExcluded(%q) = %v, want %v", test.raw, got, test.want)
		}
	}
}

func mustNormalizedURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	_, parsed, err := normalizeURL(raw, nil)
	if err != nil {
		t.Fatalf("normalizeURL(%q) error = %v", raw, err)
	}
	return parsed
}
