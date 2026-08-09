package discovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/use-agent/purify/publicnet"
)

func TestSafeClientRejectsDNSPrivateTargetsBeforeDial(t *testing.T) {
	for _, address := range []string{"127.0.0.1", "10.0.0.1", "100.64.0.1", "169.254.169.254", "192.0.2.1", "::1", "2001:db8::1"} {
		t.Run(address, func(t *testing.T) {
			var dialed atomic.Int32
			service := mustService(t, nil, Config{
				Resolver: staticResolver{"private.test": {netip.MustParseAddr(address)}},
				DialContext: func(context.Context, string, string) (net.Conn, error) {
					dialed.Add(1)
					return nil, fmt.Errorf("must not dial")
				},
			})
			result, err := service.Discover(context.Background(), "http://private.test/")
			if !errors.Is(err, ErrAllSourcesFailed) || result == nil {
				t.Fatalf("Discover() = %#v, %v", result, err)
			}
			if dialed.Load() != 0 {
				t.Fatalf("raw dial calls = %d, want zero", dialed.Load())
			}
		})
	}
}

func TestSafeClientRejectsPrivateRedirectBeforeTargetRequest(t *testing.T) {
	var privateHits atomic.Int32
	privateServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		privateHits.Add(1)
		http.Error(writer, "private", http.StatusOK)
	}))
	defer privateServer.Close()

	publicServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/sitemap.xml":
			http.Redirect(writer, request, "http://private.test/secret", http.StatusFound)
		case "/robots.txt":
			writer.Header().Set("Content-Type", "text/plain")
		case "/":
			writer.Header().Set("Content-Type", "text/html")
		default:
			http.NotFound(writer, request)
		}
	}))
	defer publicServer.Close()

	publicAddress := netip.MustParseAddr("93.184.216.34")
	privateAddress := netip.MustParseAddr("127.0.0.1")
	service := mustService(t, nil, Config{
		Resolver: staticResolver{
			"public.test":  {publicAddress},
			"private.test": {privateAddress},
		},
		DialContext: mappedDialer(t, map[string]string{
			publicAddress.String():  serverAddress(t, publicServer.URL),
			privateAddress.String(): serverAddress(t, privateServer.URL),
		}),
	})
	result, err := service.Discover(context.Background(), "http://public.test/")
	if err != nil {
		t.Fatalf("Discover() error = %v, warnings=%#v", err, result.Warnings)
	}
	if privateHits.Load() != 0 {
		t.Fatalf("private redirect target received %d requests", privateHits.Load())
	}
	assertWarning(t, result.Warnings, "sitemap", "fetch_failed")
}

func TestSafeClientRejectsDNSRebindingAtDial(t *testing.T) {
	publicAddress := netip.MustParseAddr("93.184.216.34")
	privateAddress := netip.MustParseAddr("127.0.0.1")
	resolver := &sequenceResolver{answers: map[string][][]netip.Addr{
		"rebind.test": {{publicAddress}, {privateAddress}},
	}}
	policy := publicnet.NewPolicy(publicnet.Options{
		Resolver: resolver,
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("raw dial must not run after private rebinding")
			return nil, nil
		},
	})
	if _, err := policy.Resolve(context.Background(), "rebind.test"); err != nil {
		t.Fatalf("first resolve error = %v", err)
	}
	if _, err := policy.DialContext(context.Background(), "tcp", "rebind.test:80"); err == nil {
		t.Fatal("dialContext() error = nil")
	}
}

func TestSafeClientBoundsRedirectCycles(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		switch request.URL.Path {
		case "/sitemap.xml":
			http.Redirect(writer, request, "/loop", http.StatusFound)
		case "/loop":
			http.Redirect(writer, request, "/sitemap.xml", http.StatusFound)
		case "/robots.txt":
			writer.Header().Set("Content-Type", "text/plain")
		case "/":
			writer.Header().Set("Content-Type", "text/html")
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	address := netip.MustParseAddr("93.184.216.34")
	service := mustService(t, nil, Config{
		Resolver:     staticResolver{"public.test": {address}},
		DialContext:  mappedDialer(t, map[string]string{address.String(): serverAddress(t, server.URL)}),
		MaxRedirects: 2,
	})
	result, err := service.Discover(context.Background(), "http://public.test/")
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	assertWarning(t, result.Warnings, "sitemap", "fetch_failed")
	if got := requests.Load(); got > 5 {
		t.Fatalf("request count = %d, redirect loop was not bounded", got)
	}
}

func TestSafeClientEnforcesExactRedirectHopLimit(t *testing.T) {
	var chainHits [4]atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/sitemap.xml":
			chainHits[0].Add(1)
			http.Redirect(writer, request, "/r1", http.StatusFound)
		case "/r1":
			chainHits[1].Add(1)
			http.Redirect(writer, request, "/r2", http.StatusFound)
		case "/r2":
			chainHits[2].Add(1)
			http.Redirect(writer, request, "/r3", http.StatusFound)
		case "/r3":
			chainHits[3].Add(1)
			writer.Header().Set("Content-Type", "application/xml")
			fmt.Fprint(writer, "<urlset></urlset>")
		case "/robots.txt":
			writer.Header().Set("Content-Type", "text/plain")
		case "/":
			writer.Header().Set("Content-Type", "text/html")
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	address := netip.MustParseAddr("93.184.216.34")
	service := mustService(t, nil, Config{
		Resolver:     staticResolver{"public.test": {address}},
		DialContext:  mappedDialer(t, map[string]string{address.String(): serverAddress(t, server.URL)}),
		MaxRedirects: 2,
	})
	result, err := service.Discover(context.Background(), "http://public.test/")
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	assertWarning(t, result.Warnings, "sitemap", "fetch_failed")
	for index, want := range []int32{1, 1, 1, 0} {
		if got := chainHits[index].Load(); got != want {
			t.Fatalf("redirect hop %d hits = %d, want %d", index, got, want)
		}
	}
}

type staticResolver map[string][]netip.Addr

func (resolver staticResolver) LookupNetIP(_ context.Context, _ string, hostname string) ([]netip.Addr, error) {
	values, ok := resolver[strings.ToLower(hostname)]
	if !ok {
		return nil, fmt.Errorf("no DNS fixture for %s", hostname)
	}
	return append([]netip.Addr(nil), values...), nil
}

type sequenceResolver struct {
	mu      sync.Mutex
	answers map[string][][]netip.Addr
}

func (resolver *sequenceResolver) LookupNetIP(_ context.Context, _ string, hostname string) ([]netip.Addr, error) {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	answers := resolver.answers[hostname]
	if len(answers) == 0 {
		return nil, fmt.Errorf("no DNS answer for %s", hostname)
	}
	answer := append([]netip.Addr(nil), answers[0]...)
	if len(answers) > 1 {
		resolver.answers[hostname] = answers[1:]
	}
	return answer, nil
}

func mappedDialer(t *testing.T, destinations map[string]string) DialContextFunc {
	t.Helper()
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		destination, ok := destinations[host]
		if !ok {
			return nil, fmt.Errorf("unexpected dial target %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, destination)
	}
}

func serverAddress(t *testing.T, rawURL string) string {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Host
}
