package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/use-agent/purify/ledger"
)

func TestRawDelivererSendsExactBodySignatureAndIdentity(t *testing.T) {
	payload := []byte("{\n  \"type\": \"fact.changed\", \"raw\": true\n}")
	secret := "request-specific-secret"
	eventID := "fact.changed:verification-123"
	var received []byte
	var contentType, userAgent, receivedID, signature string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var err error
		received, err = io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("ReadAll(body) error = %v", err)
		}
		contentType = request.Header.Get("Content-Type")
		userAgent = request.Header.Get("User-Agent")
		receivedID = request.Header.Get(OutboxEventIDHeader)
		signature = request.Header.Get("X-Purify-Signature")
		writer.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	client := server.Client()
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	deliverer, err := NewRawDeliverer(client)
	if err != nil {
		t.Fatalf("NewRawDeliverer() error = %v", err)
	}

	result := deliverer.Deliver(context.Background(), ledger.OutboxEvent{
		ID:      eventID,
		URL:     server.URL + "/callback",
		Secret:  secret,
		Payload: payload,
	})
	if result.Err != nil {
		t.Fatalf("Deliver() result = %#v", result)
	}
	if string(received) != string(payload) {
		t.Fatalf("body = %q, want exact %q", received, payload)
	}
	if contentType != "application/json" || userAgent != OutboxUserAgent || receivedID != eventID {
		t.Fatalf("headers content-type=%q user-agent=%q event-id=%q", contentType, userAgent, receivedID)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(payload)
	wantSignature := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if signature != wantSignature {
		t.Fatalf("signature = %q, want %q", signature, wantSignature)
	}
	if string(payload) != "{\n  \"type\": \"fact.changed\", \"raw\": true\n}" {
		t.Fatalf("caller payload mutated: %q", payload)
	}
}

func TestRawDelivererClassifiesStatusesAndTransportFailures(t *testing.T) {
	networkError := errors.New("Get \"https://secret.example/hook?token=do-not-store\": private transport detail")
	tests := []struct {
		name      string
		status    int
		doErr     error
		nilResult bool
		wantError bool
		retryable bool
	}{
		{name: "200", status: http.StatusOK},
		{name: "299", status: 299},
		{name: "300", status: http.StatusMultipleChoices, wantError: true},
		{name: "307", status: http.StatusTemporaryRedirect, wantError: true},
		{name: "400", status: http.StatusBadRequest, wantError: true},
		{name: "404", status: http.StatusNotFound, wantError: true},
		{name: "408", status: http.StatusRequestTimeout, wantError: true, retryable: true},
		{name: "425", status: http.StatusTooEarly, wantError: true, retryable: true},
		{name: "429", status: http.StatusTooManyRequests, wantError: true, retryable: true},
		{name: "500", status: http.StatusInternalServerError, wantError: true, retryable: true},
		{name: "599", status: 599, wantError: true, retryable: true},
		{name: "network", doErr: networkError, wantError: true, retryable: true},
		{name: "nil response", nilResult: true, wantError: true, retryable: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			deliverer, err := NewRawDeliverer(httpDoerFunc(func(*http.Request) (*http.Response, error) {
				if test.doErr != nil {
					return nil, test.doErr
				}
				if test.nilResult {
					return nil, nil
				}
				return &http.Response{
					StatusCode: test.status,
					Body:       io.NopCloser(strings.NewReader("ignored")),
				}, nil
			}))
			if err != nil {
				t.Fatalf("NewRawDeliverer() error = %v", err)
			}
			result := deliverer.Deliver(context.Background(), ledger.OutboxEvent{
				ID:      "event-id",
				URL:     "https://hooks.example/callback?token=do-not-store",
				Payload: []byte(`{"type":"fact.changed"}`),
			})
			if (result.Err != nil) != test.wantError || result.Retryable != test.retryable {
				t.Fatalf("result = %#v, want error=%v retryable=%v", result, test.wantError, test.retryable)
			}
			if result.Err != nil && strings.Contains(result.Err.Error(), "token=") {
				t.Fatalf("result leaked URL query: %q", result.Err)
			}
		})
	}
}

func TestRawDelivererDoesNotFollowRedirectsWhenDoerDisablesThem(t *testing.T) {
	var redirectedHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectedHits.Add(1)
	}))
	t.Cleanup(target.Close)
	redirect := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL+"/must-not-run", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirect.Close)
	client := redirect.Client()
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	deliverer, err := NewRawDeliverer(client)
	if err != nil {
		t.Fatalf("NewRawDeliverer() error = %v", err)
	}
	result := deliverer.Deliver(context.Background(), ledger.OutboxEvent{
		ID:      "redirect-event",
		URL:     redirect.URL,
		Payload: []byte(`{"type":"fact.changed"}`),
	})
	if result.Err == nil || result.Retryable {
		t.Fatalf("redirect result = %#v, want permanent failure", result)
	}
	if got := redirectedHits.Load(); got != 0 {
		t.Fatalf("redirect target hits = %d, want 0", got)
	}
}

func TestRawDelivererBoundsResponseDrainAndClosesBody(t *testing.T) {
	body := &countingResponseBody{remaining: MaxOutboxResponseDrainBytes * 2}
	deliverer, err := NewRawDeliverer(httpDoerFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
	}))
	if err != nil {
		t.Fatalf("NewRawDeliverer() error = %v", err)
	}
	result := deliverer.Deliver(context.Background(), ledger.OutboxEvent{
		ID:      "bounded-response",
		URL:     "https://hooks.example/callback",
		Payload: []byte(`{"type":"fact.changed"}`),
	})
	if result.Err != nil {
		t.Fatalf("Deliver() result = %#v", result)
	}
	if body.read != MaxOutboxResponseDrainBytes || !body.closed {
		t.Fatalf("response body read=%d closed=%v", body.read, body.closed)
	}
}

func TestNewRawDelivererRejectsNilDoer(t *testing.T) {
	if _, err := NewRawDeliverer(nil); !errors.Is(err, ErrInvalidOutboxConfig) {
		t.Fatalf("NewRawDeliverer(nil) error = %v", err)
	}
}

type httpDoerFunc func(*http.Request) (*http.Response, error)

func (function httpDoerFunc) Do(request *http.Request) (*http.Response, error) {
	return function(request)
}

type countingResponseBody struct {
	remaining int
	read      int
	closed    bool
}

func (body *countingResponseBody) Read(destination []byte) (int, error) {
	if body.remaining == 0 {
		return 0, io.EOF
	}
	count := len(destination)
	if count > body.remaining {
		count = body.remaining
	}
	for index := 0; index < count; index++ {
		destination[index] = 'x'
	}
	body.remaining -= count
	body.read += count
	return count, nil
}

func (body *countingResponseBody) Close() error {
	body.closed = true
	return nil
}
