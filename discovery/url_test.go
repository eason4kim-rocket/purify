package discovery

import (
	"net/url"
	"testing"
)

func TestNormalizeURLCanonicalizesWithoutLosingQuerySemantics(t *testing.T) {
	base, _ := url.Parse("https://example.com/dir/page")
	tests := []struct {
		name    string
		raw     string
		base    *url.URL
		private bool
		want    string
	}{
		{name: "host scheme port fragment", raw: "HTTP://Example.COM:80/a#section", want: "http://example.com/a"},
		{name: "empty path", raw: "https://Example.COM", want: "https://example.com/"},
		{name: "https default port", raw: "https://example.com:443/path", want: "https://example.com/path"},
		{name: "nondefault port", raw: "https://example.com:8443/path", want: "https://example.com:8443/path"},
		{name: "unicode host raw path", raw: "http://bücher.example:80/a%2Fb?x=1#f", want: "http://xn--bcher-kva.example/a%2Fb?x=1"},
		{name: "relative query", raw: "../next?x=1", base: base, want: "https://example.com/next?x=1"},
		{name: "force query", raw: "https://example.com/path?", want: "https://example.com/path?"},
		{name: "IPv6", raw: "https://[2001:4860:4860::8888]:443/", want: "https://[2001:4860:4860::8888]/"},
		{name: "private test", raw: "http://127.0.0.1:8080/a", private: true, want: "http://127.0.0.1:8080/a"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, _, err := normalizeURL(test.raw, test.base, test.private)
			if err != nil {
				t.Fatalf("normalizeURL() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("normalizeURL() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestNormalizeURLRejectsUnsafeOrMalformedTargets(t *testing.T) {
	for _, raw := range []string{
		"",
		"/relative",
		"ftp://example.com/file",
		"http:opaque",
		"https://user:pass@example.com/",
		"https://example.com:/",
		"https://example.com:70000/",
		"http://127.0.0.1/",
		"http://10.0.0.1/",
		"http://169.254.1.1/",
		"http://[::1]/",
		"http://localhost/",
		"http://name.localhost/",
		"http://[fe80::1%25en0]/",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, _, err := normalizeURL(raw, nil, false); err == nil {
				t.Fatalf("normalizeURL(%q) error = nil", raw)
			}
		})
	}
}

func TestSiteScopeUsesPublicSuffixAndLocalFallback(t *testing.T) {
	tests := []struct {
		root, candidate string
		allow           bool
		want            bool
	}{
		{root: "https://shop.example.co.uk/", candidate: "https://docs.example.co.uk/x", want: true},
		{root: "https://shop.example.co.uk/", candidate: "https://other.co.uk/x", want: false},
		{root: "https://one.github.io/", candidate: "https://docs.one.github.io/x", want: true},
		{root: "https://one.github.io/", candidate: "https://two.github.io/x", want: false},
		{root: "http://127.0.0.1:8080/", candidate: "http://127.0.0.1:8080/x", allow: true, want: true},
		{root: "http://127.0.0.1:8080/", candidate: "http://127.0.0.1:8081/x", allow: true, want: false},
	}
	for _, test := range tests {
		_, root, err := normalizeURL(test.root, nil, test.allow)
		if err != nil {
			t.Fatal(err)
		}
		_, candidate, err := normalizeURL(test.candidate, nil, test.allow)
		if err != nil {
			t.Fatal(err)
		}
		if got := newSiteScope(root).allows(candidate); got != test.want {
			t.Fatalf("scope(%q).allows(%q) = %v, want %v", test.root, test.candidate, got, test.want)
		}
	}
}
