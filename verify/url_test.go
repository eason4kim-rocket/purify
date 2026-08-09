package verify

import "testing"

func TestCanonicalHTTPURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "scheme host trailing dot default port fragment and empty path",
			raw:  "  HTTP://Example.COM.:00080#section  ",
			want: "http://example.com/",
		},
		{
			name: "IDNA escaped path and query order",
			raw:  "https://BÜCHER.example:443/a%2Fb?b=2&a=1#ignored",
			want: "https://xn--bcher-kva.example/a%2Fb?b=2&a=1",
		},
		{
			name: "IPv6 and force query",
			raw:  "https://[2001:0db8::1]:00443?",
			want: "https://[2001:db8::1]/?",
		},
		{
			name: "nondefault port",
			raw:  "https://EXAMPLE.com:8443/path",
			want: "https://example.com:8443/path",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := canonicalHTTPURL(test.raw)
			if err != nil {
				t.Fatalf("canonicalHTTPURL() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("canonicalHTTPURL() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestCanonicalHTTPURLRejectsUnsafeOrAmbiguousInputs(t *testing.T) {
	inputs := []string{
		"",
		"/relative",
		"ftp://example.com/file",
		"http:opaque",
		"http://user:password@example.com/",
		"http://example.com:",
		"http://example.com:0/",
		"https://[fe80::1%25en0]/",
	}
	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			if got, err := canonicalHTTPURL(input); err == nil {
				t.Fatalf("canonicalHTTPURL(%q) = %q, want error", input, got)
			}
		})
	}
}
