package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/use-agent/purify/ledger"
)

const (
	// OutboxUserAgent is stable so webhook consumers can identify durable
	// verification deliveries independently from legacy job callbacks.
	OutboxUserAgent = "Purify-Outbox/1.0"
	// OutboxEventIDHeader carries the durable downstream idempotency key.
	OutboxEventIDHeader = "X-Purify-Event-ID"
	// MaxOutboxResponseDrainBytes bounds response reads used for connection reuse.
	MaxOutboxResponseDrainBytes = 4 << 10
)

var ErrInvalidOutboxConfig = errors.New("webhook: invalid outbox configuration")

const (
	errInvalidDelivery     safeDeliveryError = "webhook: invalid delivery request"
	errTransportFailure    safeDeliveryError = "webhook: transport failure"
	errTransportNoResponse safeDeliveryError = "webhook: transport returned no response"
	errDelivererPanic      safeDeliveryError = "webhook: deliverer panic"
)

// DeliveryResult classifies a synchronous delivery. Err == nil means success;
// otherwise Retryable determines whether the worker may schedule another try.
type DeliveryResult struct {
	Err       error
	Retryable bool
}

// Deliverer sends one already-serialized outbox event synchronously.
type Deliverer interface {
	Deliver(context.Context, ledger.OutboxEvent) DeliveryResult
}

// HTTPDoer is the part of http.Client used by RawDeliverer. Production clients
// must disable automatic redirects so every returned 3xx can be classified and
// every redirect hop can later receive the same public-network policy checks.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// RawDeliverer posts the exact JSON bytes persisted in the ledger.
type RawDeliverer struct {
	doer HTTPDoer
}

// NewRawDeliverer constructs a synchronous raw payload deliverer.
func NewRawDeliverer(doer HTTPDoer) (*RawDeliverer, error) {
	if doer == nil {
		return nil, fmt.Errorf("%w: HTTP doer is required", ErrInvalidOutboxConfig)
	}
	return &RawDeliverer{doer: doer}, nil
}

// Deliver sends one event. A returned 2xx response succeeds; 408, 425, 429,
// every 5xx, and transport errors are retryable. Every other HTTP response is
// a permanent failure.
func (d *RawDeliverer) Deliver(ctx context.Context, event ledger.OutboxEvent) DeliveryResult {
	if d == nil || d.doer == nil {
		return permanentDelivery(fmt.Errorf("%w: HTTP doer is required", ErrInvalidOutboxConfig))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, event.URL, bytes.NewReader(event.Payload))
	if err != nil {
		return permanentDelivery(errInvalidDelivery)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", OutboxUserAgent)
	req.Header.Set(OutboxEventIDHeader, event.ID)
	if event.Secret != "" {
		mac := hmac.New(sha256.New, []byte(event.Secret))
		_, _ = mac.Write(event.Payload)
		req.Header.Set("X-Purify-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}

	response, err := d.doer.Do(req)
	if err != nil {
		return retryableDelivery(errTransportFailure)
	}
	if response == nil {
		return retryableDelivery(errTransportNoResponse)
	}
	if response.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, MaxOutboxResponseDrainBytes))
		_ = response.Body.Close()
	}
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		return DeliveryResult{}
	}
	statusErr := httpStatusError(response.StatusCode)
	if retryableHTTPStatus(response.StatusCode) {
		return retryableDelivery(statusErr)
	}
	return permanentDelivery(statusErr)
}

func retryableDelivery(err error) DeliveryResult {
	return DeliveryResult{Err: err, Retryable: true}
}

func permanentDelivery(err error) DeliveryResult {
	return DeliveryResult{Err: err}
}

type httpStatusError int

func (e httpStatusError) Error() string {
	return fmt.Sprintf("webhook: endpoint returned status %d", e)
}

func (e httpStatusError) HTTPStatus() int { return int(e) }

func retryableHTTPStatus(status int) bool {
	return status == http.StatusRequestTimeout ||
		status == http.StatusTooEarly ||
		status == http.StatusTooManyRequests ||
		status >= http.StatusInternalServerError && status <= 599
}

type safeDeliveryError string

func (e safeDeliveryError) Error() string { return string(e) }
