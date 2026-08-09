package llm

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/publicnet"
)

func TestNewPublicHTTPClientUsesBoundedHardenedTransport(t *testing.T) {
	if client, err := NewPublicHTTPClient(nil, time.Second); err == nil || client != nil {
		t.Fatalf("NewPublicHTTPClient(nil) = %#v, %v", client, err)
	}
	if client, err := NewPublicHTTPClient(publicnet.NewPolicy(publicnet.Options{}), -time.Second); err == nil || client != nil {
		t.Fatalf("NewPublicHTTPClient(negative) = %#v, %v", client, err)
	}

	client, err := NewPublicHTTPClient(publicnet.NewPolicy(publicnet.Options{}), 0)
	if err != nil {
		t.Fatalf("NewPublicHTTPClient() error = %v", err)
	}
	if client.Timeout != defaultHTTPTimeout || client.CheckRedirect == nil {
		t.Fatalf("client timeout/redirect configured = %s/%t", client.Timeout, client.CheckRedirect != nil)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy != nil || transport.DialContext == nil || transport.TLSClientConfig == nil ||
		transport.TLSClientConfig.MinVersion != tls.VersionTLS12 || transport.MaxConnsPerHost <= 0 ||
		transport.ResponseHeaderTimeout != defaultHTTPTimeout || transport.TLSHandshakeTimeout != defaultHTTPTimeout {
		t.Fatalf("unsafe or unbounded transport = %#v", transport)
	}
}

func TestPublicHTTPClientRejectsPrivateAndMixedDNSBeforeDial(t *testing.T) {
	for _, test := range []struct {
		name      string
		addresses []netip.Addr
	}{
		{name: "private", addresses: []netip.Addr{netip.MustParseAddr("127.0.0.1")}},
		{name: "mixed", addresses: []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("10.0.0.1")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var dials atomic.Int32
			policy := publicnet.NewPolicy(publicnet.Options{
				Resolver: llmPublicResolver{"provider.test": test.addresses},
				DialContext: func(context.Context, string, string) (net.Conn, error) {
					dials.Add(1)
					return nil, errors.New("unexpected dial")
				},
			})
			client, err := NewPublicHTTPClient(policy, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Get("http://provider.test/v1")
			if response != nil {
				response.Body.Close()
			}
			if !errors.Is(err, publicnet.ErrNotPublic) {
				t.Fatalf("GET error = %v, want ErrNotPublic", err)
			}
			if dials.Load() != 0 {
				t.Fatalf("underlying dial calls = %d, want zero", dials.Load())
			}
		})
	}
}

func TestClientMapsRejectedPrivateDNSWithoutProviderCallOrCredentialLeak(t *testing.T) {
	var dials atomic.Int32
	policy := publicnet.NewPolicy(publicnet.Options{
		Resolver: llmPublicResolver{"private-provider.test": {netip.MustParseAddr("169.254.169.254")}},
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			dials.Add(1)
			return nil, errors.New("unexpected dial")
		},
	})
	httpClient, err := NewPublicHTTPClient(policy, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	const credential = "request-provider-secret"
	_, err = NewClient(httpClient).Extract(
		context.Background(),
		"content",
		json.RawMessage(`{"type":"object"}`),
		ExtractParams{APIKey: credential, Model: "test", BaseURL: "http://private-provider.test/v1"},
	)
	var scrapeError *models.ScrapeError
	if !errors.As(err, &scrapeError) || scrapeError.Code != models.ErrCodeLLMFailure || scrapeError.Message != "LLM request failed" {
		t.Fatalf("Extract() error = %#v", err)
	}
	if strings.Contains(scrapeError.ToDetail().Message, credential) || dials.Load() != 0 {
		t.Fatalf("public error/dials = %#v/%d", scrapeError.ToDetail(), dials.Load())
	}
}

func TestPublicHTTPClientReturnsRedirectWithoutFollowingAndLeavesBodyClosable(t *testing.T) {
	var redirectedHits atomic.Int32
	redirected := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectedHits.Add(1)
	}))
	t.Cleanup(redirected.Close)

	for _, status := range []int{
		http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusSeeOther,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			redirector := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				http.Redirect(writer, request, redirected.URL+"/credential-target", status)
			}))
			t.Cleanup(redirector.Close)

			policy := mappedPublicPolicy(t, "provider.test", redirector)
			client, err := NewPublicHTTPClient(policy, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Get("http://provider.test/v1")
			if err != nil {
				t.Fatalf("GET redirect error = %v", err)
			}
			if response == nil || response.StatusCode != status || response.Body == nil {
				t.Fatalf("redirect response = %#v", response)
			}
			if err := response.Body.Close(); err != nil {
				t.Fatalf("close redirect response body: %v", err)
			}
		})
	}
	if redirectedHits.Load() != 0 {
		t.Fatalf("redirect target hits = %d, want zero", redirectedHits.Load())
	}
}

func TestPublicHTTPClientIgnoresEnvironmentProxyAndReachesPublicTarget(t *testing.T) {
	var proxyHits atomic.Int32
	proxyServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		proxyHits.Add(1)
		http.Error(writer, "unexpected proxy", http.StatusBadGateway)
	}))
	t.Cleanup(proxyServer.Close)
	for _, variable := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(variable, proxyServer.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	var providerHits atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		providerHits.Add(1)
		writer.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(provider.Close)
	policy := mappedPublicPolicy(t, "provider.test", provider)
	client, err := NewPublicHTTPClient(policy, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Get("http://provider.test/v1")
	if err != nil {
		t.Fatalf("GET public target error = %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent || providerHits.Load() != 1 || proxyHits.Load() != 0 {
		t.Fatalf("status/provider/proxy = %d/%d/%d", response.StatusCode, providerHits.Load(), proxyHits.Load())
	}
}

func mappedPublicPolicy(t *testing.T, hostname string, server *httptest.Server) *publicnet.Policy {
	t.Helper()
	target := strings.TrimPrefix(server.URL, "http://")
	return publicnet.NewPolicy(publicnet.Options{
		Resolver: llmPublicResolver{hostname: {netip.MustParseAddr("93.184.216.34")}},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, target)
		},
	})
}

type llmPublicResolver map[string][]netip.Addr

func (resolver llmPublicResolver) LookupNetIP(_ context.Context, _, hostname string) ([]netip.Addr, error) {
	addresses, ok := resolver[strings.ToLower(hostname)]
	if !ok {
		return nil, errors.New("missing DNS fixture")
	}
	return append([]netip.Addr(nil), addresses...), nil
}
