package publicnet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
)

func TestPolicyRejectsNonPublicDNSAnswersBeforeDial(t *testing.T) {
	for _, rawAddress := range []string{
		"0.1.2.3",
		"10.0.0.1",
		"100.64.0.1",
		"127.0.0.1",
		"169.254.169.254",
		"192.0.0.1",
		"192.0.2.1",
		"192.88.99.1",
		"198.18.0.1",
		"198.51.100.1",
		"203.0.113.1",
		"240.0.0.1",
		"::1",
		"64:ff9b:1::1",
		"64:ff9b::a00:1",
		"100::1",
		"100:0:0:1::1",
		"2001:2::1",
		"2001:db8::1",
		"2002::1",
		"3ffe::1",
		"3fff::1",
		"4000::1",
		"5f00::1",
		"fec0::1",
		"fc00::1",
		"fe80::1",
	} {
		t.Run(rawAddress, func(t *testing.T) {
			var dialed atomic.Int32
			policy := NewPolicy(Options{
				Resolver: staticResolver{"blocked.test": {netip.MustParseAddr(rawAddress)}},
				DialContext: func(context.Context, string, string) (net.Conn, error) {
					dialed.Add(1)
					return nil, errors.New("unexpected dial")
				},
			})
			if _, err := policy.DialContext(context.Background(), "tcp", "blocked.test:443"); !errors.Is(err, ErrNotPublic) {
				t.Fatalf("DialContext() error = %v, want ErrNotPublic", err)
			}
			if got := dialed.Load(); got != 0 {
				t.Fatalf("underlying dial calls = %d, want zero", got)
			}
		})
	}
}

func TestPolicyRejectsMixedPublicPrivateAnswerFailClosed(t *testing.T) {
	var dialed atomic.Int32
	policy := NewPolicy(Options{
		Resolver: staticResolver{"mixed.test": {
			netip.MustParseAddr("1.1.1.1"),
			netip.MustParseAddr("127.0.0.1"),
		}},
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			dialed.Add(1)
			return nil, errors.New("unexpected dial")
		},
	})
	if _, err := policy.DialContext(context.Background(), "tcp", "mixed.test:443"); !errors.Is(err, ErrNotPublic) {
		t.Fatalf("DialContext() error = %v, want ErrNotPublic", err)
	}
	if got := dialed.Load(); got != 0 {
		t.Fatalf("underlying dial calls = %d, want zero", got)
	}
}

func TestPolicyResolvesEveryDialAndPinsLiteralIP(t *testing.T) {
	resolver := &sequenceResolver{answers: [][]netip.Addr{
		{netip.MustParseAddr("1.1.1.1")},
		{netip.MustParseAddr("2606:4700:4700::1111")},
	}}
	var mu sync.Mutex
	var targets []string
	policy := NewPolicy(Options{
		Resolver: resolver,
		DialContext: func(_ context.Context, network, address string) (net.Conn, error) {
			if network != "tcp" {
				t.Errorf("network = %q, want tcp", network)
			}
			mu.Lock()
			targets = append(targets, address)
			mu.Unlock()
			return closedPipe(), nil
		},
	})
	for range 2 {
		connection, err := policy.DialContext(context.Background(), "tcp", "Rebind.Test.:443")
		if err != nil {
			t.Fatalf("DialContext() error = %v", err)
		}
		_ = connection.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"1.1.1.1:443", "[2606:4700:4700::1111]:443"}
	if fmt.Sprint(targets) != fmt.Sprint(want) {
		t.Fatalf("underlying targets = %v, want %v", targets, want)
	}
	if got := resolver.calls.Load(); got != 2 {
		t.Fatalf("resolver calls = %d, want 2", got)
	}
}

func TestPolicyRejectsPrivateDNSRebindingOnLaterDial(t *testing.T) {
	resolver := &sequenceResolver{answers: [][]netip.Addr{
		{netip.MustParseAddr("1.1.1.1")},
		{netip.MustParseAddr("127.0.0.1")},
	}}
	var dialed atomic.Int32
	policy := NewPolicy(Options{
		Resolver: resolver,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			dialed.Add(1)
			return closedPipe(), nil
		},
	})
	connection, err := policy.DialContext(context.Background(), "tcp", "rebind.test:80")
	if err != nil {
		t.Fatalf("first DialContext() error = %v", err)
	}
	_ = connection.Close()
	if _, err := policy.DialContext(context.Background(), "tcp", "rebind.test:80"); err == nil {
		t.Fatal("second DialContext() error = nil")
	}
	if got := dialed.Load(); got != 1 {
		t.Fatalf("underlying dial calls = %d, want 1", got)
	}
}

func TestPolicyPinsLiteralIPv4AndIPv6WithoutDNS(t *testing.T) {
	resolver := resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		t.Fatal("literal IP target unexpectedly used DNS")
		return nil, nil
	})
	var targets []string
	policy := NewPolicy(Options{
		Resolver: resolver,
		DialContext: func(_ context.Context, network, address string) (net.Conn, error) {
			targets = append(targets, network+" "+address)
			return closedPipe(), nil
		},
	})
	for _, test := range []struct {
		network string
		target  string
	}{
		{network: "tcp4", target: "8.8.8.8:53"},
		{network: "tcp6", target: "[2606:4700:4700::1111]:443"},
	} {
		connection, err := policy.DialContext(context.Background(), test.network, test.target)
		if err != nil {
			t.Fatalf("DialContext(%q, %q) error = %v", test.network, test.target, err)
		}
		_ = connection.Close()
	}
	if fmt.Sprint(targets) != fmt.Sprint([]string{"tcp4 8.8.8.8:53", "tcp6 [2606:4700:4700::1111]:443"}) {
		t.Fatalf("underlying targets = %v", targets)
	}
}

func TestPolicyTriesValidatedPublicCandidatesInResolverOrder(t *testing.T) {
	var targets []string
	policy := NewPolicy(Options{
		Resolver: staticResolver{"fallback.test": {
			netip.MustParseAddr("1.1.1.1"),
			netip.MustParseAddr("2606:4700:4700::1111"),
		}},
		DialContext: func(_ context.Context, _ string, address string) (net.Conn, error) {
			targets = append(targets, address)
			if len(targets) == 1 {
				return nil, syscall.ECONNREFUSED
			}
			return closedPipe(), nil
		},
	})
	connection, err := policy.DialContext(context.Background(), "tcp", "fallback.test:443")
	if err != nil {
		t.Fatalf("DialContext() error = %v", err)
	}
	_ = connection.Close()
	want := []string{"1.1.1.1:443", "[2606:4700:4700::1111]:443"}
	if fmt.Sprint(targets) != fmt.Sprint(want) {
		t.Fatalf("underlying targets = %v, want %v", targets, want)
	}
}

func TestPolicyValidatesNetworkAndPortBeforeResolution(t *testing.T) {
	var resolved atomic.Int32
	policy := NewPolicy(Options{
		Resolver: resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
			resolved.Add(1)
			return []netip.Addr{netip.MustParseAddr("1.1.1.1")}, nil
		}),
	})
	for _, test := range []struct {
		network string
		address string
	}{
		{network: "udp", address: "example.test:443"},
		{network: "ip", address: "example.test:443"},
		{network: "tcp", address: "example.test"},
		{network: "tcp", address: "example.test:"},
		{network: "tcp", address: "example.test:0"},
		{network: "tcp", address: "example.test:65536"},
		{network: "tcp", address: "example.test:https"},
		{network: "tcp", address: "example.test:+80"},
	} {
		if _, err := policy.DialContext(context.Background(), test.network, test.address); err == nil {
			t.Errorf("DialContext(%q, %q) error = nil", test.network, test.address)
		}
	}
	if got := resolved.Load(); got != 0 {
		t.Fatalf("resolver calls = %d, want zero", got)
	}
}

func TestPolicyAllowPrivateNetworksLegacyOptOut(t *testing.T) {
	var target string
	policy := NewPolicy(Options{
		AllowPrivateNetworks: true,
		Resolver:             staticResolver{"private.test": {netip.MustParseAddr("127.0.0.1")}},
		DialContext: func(_ context.Context, _ string, address string) (net.Conn, error) {
			target = address
			return closedPipe(), nil
		},
	})
	connection, err := policy.DialContext(context.Background(), "tcp", "private.test:8080")
	if err != nil {
		t.Fatalf("DialContext() error = %v", err)
	}
	_ = connection.Close()
	if target != "127.0.0.1:8080" {
		t.Fatalf("underlying target = %q", target)
	}
}

func TestNormalizeHTTPURLCanonicalizesAndChecksLiteralIP(t *testing.T) {
	base, _ := url.Parse("https://example.com/dir/page")
	for _, test := range []struct {
		raw     string
		base    *url.URL
		private bool
		want    string
	}{
		{raw: "HTTP://Example.COM:80/a#fragment", want: "http://example.com/a"},
		{raw: "https://Example.COM", want: "https://example.com/"},
		{raw: "http://bücher.example:80/a%2Fb?x=1#f", want: "http://xn--bcher-kva.example/a%2Fb?x=1"},
		{raw: "../next?x=1", base: base, want: "https://example.com/next?x=1"},
		{raw: "https://[2606:4700:4700::1111]:443/", want: "https://[2606:4700:4700::1111]/"},
		{raw: "http://127.0.0.1:8080/a", private: true, want: "http://127.0.0.1:8080/a"},
	} {
		got, _, err := NormalizeHTTPURL(test.raw, test.base, test.private)
		if err != nil {
			t.Fatalf("NormalizeHTTPURL(%q) error = %v", test.raw, err)
		}
		if got != test.want {
			t.Fatalf("NormalizeHTTPURL(%q) = %q, want %q", test.raw, got, test.want)
		}
	}
	for _, raw := range []string{
		"", "/relative", "ftp://example.com/", "https://user@example.com/",
		"https://example.com:/", "https://example.com:65536/", "http://127.0.0.1/",
		"http://192.0.2.1/", "http://[::1]/", "http://localhost/", "http://name.localhost/",
		"http://[fe80::1%25en0]/",
	} {
		if _, _, err := NormalizeHTTPURL(raw, nil, false); err == nil {
			t.Errorf("NormalizeHTTPURL(%q) error = nil", raw)
		}
	}
}

func TestIsPublicAddressCoversIPv4AndIPv6(t *testing.T) {
	for _, raw := range []string{
		"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111", "2001:4860:4860::8888",
		"2001:1::1", "2001:1::2", "2001:1::3", "2001:3::1", "2001:4:112::1", "2001:20::1", "2001:30::1",
		"2620:4f:8000::1", "64:ff9b::808:808",
	} {
		if !IsPublicAddress(netip.MustParseAddr(raw)) {
			t.Errorf("IsPublicAddress(%s) = false", raw)
		}
	}
	for _, raw := range []string{
		"10.0.0.1", "100.64.0.1", "192.0.2.1", "192.88.99.1", "127.0.0.1",
		"::1", "64:ff9b:1::1", "64:ff9b::a00:1", "100::1", "100:0:0:1::1", "2001:2::1",
		"2001:db8::1", "2002::1", "3ffe::1", "3fff::1", "4000::1", "5f00::1", "fec0::1", "fe80::1",
	} {
		if IsPublicAddress(netip.MustParseAddr(raw)) {
			t.Errorf("IsPublicAddress(%s) = true", raw)
		}
	}
}

type staticResolver map[string][]netip.Addr

func (resolver staticResolver) LookupNetIP(_ context.Context, _, hostname string) ([]netip.Addr, error) {
	addresses, ok := resolver[strings.ToLower(hostname)]
	if !ok {
		return nil, fmt.Errorf("no DNS fixture for %s", hostname)
	}
	return append([]netip.Addr(nil), addresses...), nil
}

type resolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (resolve resolverFunc) LookupNetIP(ctx context.Context, network, hostname string) ([]netip.Addr, error) {
	return resolve(ctx, network, hostname)
}

type sequenceResolver struct {
	mu      sync.Mutex
	answers [][]netip.Addr
	calls   atomic.Int32
}

func (resolver *sequenceResolver) LookupNetIP(_ context.Context, _, _ string) ([]netip.Addr, error) {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	resolver.calls.Add(1)
	if len(resolver.answers) == 0 {
		return nil, errors.New("no DNS answer")
	}
	answer := append([]netip.Addr(nil), resolver.answers[0]...)
	resolver.answers = resolver.answers[1:]
	return answer, nil
}

func closedPipe() net.Conn {
	client, server := net.Pipe()
	_ = server.Close()
	return client
}
