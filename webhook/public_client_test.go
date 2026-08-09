package webhook

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/use-agent/purify/ledger"
	"github.com/use-agent/purify/publicnet"
)

func TestPublicHTTPClientRejectsPrivateDNSBeforeDial(t *testing.T) {
	var dials atomic.Int32
	policy := publicnet.NewPolicy(publicnet.Options{
		Resolver: publicClientResolver{"private.test": {netip.MustParseAddr("127.0.0.1")}},
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			dials.Add(1)
			return nil, errors.New("unexpected dial")
		},
	})
	client, err := NewPublicHTTPClient(policy, time.Second)
	if err != nil {
		t.Fatalf("NewPublicHTTPClient() error = %v", err)
	}
	deliverer, err := NewRawDeliverer(client)
	if err != nil {
		t.Fatalf("NewRawDeliverer() error = %v", err)
	}
	result := deliverer.Deliver(context.Background(), ledger.OutboxEvent{
		ID: "private-event", URL: "http://private.test/hook", Payload: []byte(`{"type":"fact.changed"}`),
	})
	if result.Err == nil || !result.Retryable {
		t.Fatalf("private delivery result = %#v", result)
	}
	if dials.Load() != 0 {
		t.Fatalf("underlying dials = %d, want zero", dials.Load())
	}
}

func TestPublicHTTPClientDoesNotFollowSignedRedirect(t *testing.T) {
	var redirectedHits atomic.Int32
	redirected := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectedHits.Add(1)
	}))
	t.Cleanup(redirected.Close)
	redirect := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, redirected.URL+"/secret", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirect.Close)

	publicAddress := netip.MustParseAddr("93.184.216.34")
	policy := publicnet.NewPolicy(publicnet.Options{
		Resolver: publicClientResolver{"public.test": {publicAddress}},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, strings.TrimPrefix(redirect.URL, "http://"))
		},
	})
	client, err := NewPublicHTTPClient(policy, time.Second)
	if err != nil {
		t.Fatalf("NewPublicHTTPClient() error = %v", err)
	}
	deliverer, err := NewRawDeliverer(client)
	if err != nil {
		t.Fatalf("NewRawDeliverer() error = %v", err)
	}
	result := deliverer.Deliver(context.Background(), ledger.OutboxEvent{
		ID: "redirect-event", URL: "http://public.test/hook", Secret: "must-not-forward", Payload: []byte(`{"type":"fact.changed"}`),
	})
	if result.Err == nil || result.Retryable {
		t.Fatalf("redirect delivery result = %#v, want permanent failure", result)
	}
	if redirectedHits.Load() != 0 {
		t.Fatalf("redirect target hits = %d, want zero", redirectedHits.Load())
	}
}

func TestNewPublicHTTPClientValidatesConfiguration(t *testing.T) {
	if _, err := NewPublicHTTPClient(nil, time.Second); err == nil {
		t.Fatal("NewPublicHTTPClient(nil) error = nil")
	}
	if _, err := NewPublicHTTPClient(publicnet.NewPolicy(publicnet.Options{}), -time.Second); err == nil {
		t.Fatal("NewPublicHTTPClient(negative timeout) error = nil")
	}
}

type publicClientResolver map[string][]netip.Addr

func (resolver publicClientResolver) LookupNetIP(_ context.Context, _, hostname string) ([]netip.Addr, error) {
	addresses, ok := resolver[strings.ToLower(hostname)]
	if !ok {
		return nil, errors.New("missing DNS fixture")
	}
	return append([]netip.Addr(nil), addresses...), nil
}
