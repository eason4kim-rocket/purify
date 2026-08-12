package rerank

import (
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxReferenceUnixSocketPathBytes = 100
	referenceUnixEndpoint           = "http://localhost/v1/rerank"
)

// ReferenceRecorderConfig configures the independent, recording-only client.
// It intentionally has no profile, model, manifest, or certification override:
// every recording uses the repository-pinned reference tuple.
type ReferenceRecorderConfig struct {
	Endpoint     string
	APIKey       string
	AllowPrivate bool
	Timeout      time.Duration
}

// UnixReferenceRecorderConfig configures the recording-only transport owned
// by an authenticated local R-6a session. The HTTP destination is fixed and
// every connection is dialed through SocketPath; ambient proxy and DNS state
// are never consulted.
type UnixReferenceRecorderConfig struct {
	SocketPath string
	APIKey     string
	Timeout    time.Duration
}

// ReferenceRecording is one exact request/response observation. InputDigest
// identifies the canonical vLLM wire bytes while CandidateDigest additionally
// binds their ordered stable-ID/provider-rank mapping. Usage and scores come
// from the same strict response decoder.
type ReferenceRecording struct {
	InputDigest     string
	CandidateDigest string
	Observation     ReferenceObservation
	LatencyUS       int64
}

// ReferenceRecorder is deliberately not a Scorer and cannot be wired into
// Search. It exists only for the explicit offline record command. R-6a must
// still authenticate the launched sidecar before a recording can be promoted.
type ReferenceRecorder struct {
	mu        sync.Mutex
	gate      chan struct{}
	scorer    *VLLMScorer
	timeout   time.Duration
	now       func() time.Time
	closed    bool
	closeOnce sync.Once
}

// NewReferenceRecorder constructs the hardened recording transport without
// consulting or changing the production certified-profile registry.
func NewReferenceRecorder(config ReferenceRecorderConfig) (*ReferenceRecorder, error) {
	timeout := config.Timeout
	if timeout == 0 {
		timeout = DefaultScorerTimeout
	}
	if timeout < time.Second || timeout > MaximumScorerTimeout {
		return nil, ErrNotConfigured
	}
	endpoint, err := parseVLLMEndpoint(config.Endpoint)
	if err != nil {
		return nil, ErrNotConfigured
	}
	client, err := newRerankHTTPClient(rerankHTTPClientConfig{
		endpoint: endpoint, allowPrivate: config.AllowPrivate, timeout: timeout,
	})
	if err != nil {
		return nil, ErrNotConfigured
	}
	scorer, err := newReferenceVLLMScorer(client, endpoint.String(), config.APIKey)
	if err != nil {
		client.CloseIdleConnections()
		return nil, err
	}
	return newReferenceRecorder(scorer, timeout, time.Now)
}

// NewUnixReferenceRecorder constructs a recording-only client for a local
// Unix socket. It does not expose an endpoint, implement Scorer, or change the
// production profile/deployment admission gates.
func NewUnixReferenceRecorder(config UnixReferenceRecorderConfig) (*ReferenceRecorder, error) {
	timeout := config.Timeout
	if timeout == 0 {
		timeout = DefaultScorerTimeout
	}
	if timeout < time.Second || timeout > MaximumScorerTimeout ||
		!validReferenceUnixSocketPath(config.SocketPath) || !validRerankSecret(config.APIKey) {
		return nil, ErrNotConfigured
	}

	socketPath := strings.Clone(config.SocketPath)
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if ctx == nil || (network != "tcp" && network != "tcp4" && network != "tcp6") || address != "localhost:80" {
				return nil, errors.New("rerank: Unix destination rejected")
			}
			return dialer.DialContext(ctx, "unix", socketPath)
		},
		MaxIdleConns:           1,
		MaxIdleConnsPerHost:    1,
		MaxConnsPerHost:        1,
		IdleConnTimeout:        30 * time.Second,
		ResponseHeaderTimeout:  timeout,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: rerankMaximumResponseHeaderBytes,
	}
	client := &rerankHTTPClient{
		endpoint: referenceUnixEndpoint,
		client: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errRerankRedirectRejected
			},
		},
	}
	scorer, err := newReferenceVLLMScorer(client, referenceUnixEndpoint, config.APIKey)
	if err != nil {
		client.CloseIdleConnections()
		return nil, ErrNotConfigured
	}
	return newReferenceRecorder(scorer, timeout, time.Now)
}

func validReferenceUnixSocketPath(value string) bool {
	if len(value) == 0 || len(value) > maxReferenceUnixSocketPathBytes || !utf8.ValidString(value) ||
		strings.TrimSpace(value) != value || !filepath.IsAbs(value) || filepath.Clean(value) != value || value == string(filepath.Separator) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func newReferenceRecorder(scorer *VLLMScorer, timeout time.Duration, now func() time.Time) (*ReferenceRecorder, error) {
	if scorer == nil || scorer.client == nil || timeout <= 0 || timeout > MaximumScorerTimeout || now == nil {
		return nil, ErrNotConfigured
	}
	return &ReferenceRecorder{gate: make(chan struct{}, 1), scorer: scorer, timeout: timeout, now: now}, nil
}

// Record performs exactly one synchronous observation. Calls on one recorder
// are serialized so a caller cannot accidentally turn the corpus command into
// an unbounded load generator. The caller's cancellation always wins; the
// recorder's own child deadline collapses to ErrScoringFailed.
func (recorder *ReferenceRecorder) Record(ctx context.Context, request ScoreRequest) (ReferenceRecording, error) {
	if recorder == nil || recorder.gate == nil || recorder.scorer == nil || recorder.now == nil {
		return ReferenceRecording{}, ErrNotConfigured
	}
	if ctx == nil || len(request.Candidates) == 0 {
		return ReferenceRecording{}, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return ReferenceRecording{}, err
	}
	frozenRequest := cloneScoreRequest(request)
	_, inputDigest, err := canonicalVLLMRequest(frozenRequest)
	if err != nil {
		return ReferenceRecording{}, err
	}
	candidateDigest, err := ReferenceCandidateDigest(frozenRequest)
	if err != nil {
		return ReferenceRecording{}, err
	}

	select {
	case recorder.gate <- struct{}{}:
		defer func() { <-recorder.gate }()
	case <-ctx.Done():
		return ReferenceRecording{}, ctx.Err()
	}
	recorder.mu.Lock()
	closed := recorder.closed
	recorder.mu.Unlock()
	if closed || recorder.scorer == nil {
		return ReferenceRecording{}, ErrRuntimeClosed
	}
	if err := ctx.Err(); err != nil {
		return ReferenceRecording{}, err
	}
	callContext, cancel := context.WithTimeout(ctx, recorder.timeout)
	defer cancel()
	started := recorder.now()
	observation, scoreErr := invokeReferenceObservation(callContext, recorder.scorer, cloneScoreRequest(frozenRequest))
	finished := recorder.now()
	if err := ctx.Err(); err != nil {
		return ReferenceRecording{}, err
	}
	if callContext.Err() != nil || scoreErr != nil {
		return ReferenceRecording{}, ErrScoringFailed
	}
	latency := finished.Sub(started)
	if latency.Microseconds() <= 0 || latency > maximumReferenceRecordingLatency {
		return ReferenceRecording{}, ErrScoringFailed
	}
	return ReferenceRecording{
		InputDigest:     strings.Clone(inputDigest),
		CandidateDigest: strings.Clone(candidateDigest),
		Observation:     cloneReferenceObservation(observation),
		LatencyUS:       latency.Microseconds(),
	}, nil
}

// Close is idempotent. It waits for the one serialized call, then closes idle
// transport connections and permanently rejects later observations.
func (recorder *ReferenceRecorder) Close() {
	if recorder == nil {
		return
	}
	recorder.closeOnce.Do(func() {
		recorder.mu.Lock()
		recorder.closed = true
		gate := recorder.gate
		scorer := recorder.scorer
		recorder.mu.Unlock()

		if gate != nil {
			gate <- struct{}{}
			defer func() { <-gate }()
		}
		if scorer != nil {
			scorer.CloseIdleConnections()
		}
	})
}

// maximumReferenceRecordingLatency is a defensive bound aligned with the
// persisted evaluation record without importing the internal harness.
const maximumReferenceRecordingLatency = time.Hour

func cloneReferenceObservation(source ReferenceObservation) ReferenceObservation {
	cloned := source
	cloned.ResponseID = strings.Clone(source.ResponseID)
	cloned.Scores = make([]ScoreResult, len(source.Scores))
	for index, score := range source.Scores {
		cloned.Scores[index] = ScoreResult{
			StableID: strings.Clone(score.StableID), RelevanceScore: score.RelevanceScore,
		}
	}
	return cloned
}

func invokeReferenceObservation(ctx context.Context, scorer *VLLMScorer, request ScoreRequest) (observation ReferenceObservation, err error) {
	defer func() {
		if recover() != nil {
			observation = ReferenceObservation{}
			err = ErrScoringFailed
		}
	}()
	return scorer.scoreObservation(ctx, request)
}
