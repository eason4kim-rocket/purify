package rerank

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type recordingDoer struct {
	mu        sync.Mutex
	call      func(*http.Request) (*http.Response, error)
	closeCall func()
	calls     int
	closes    int
}

func (doer *recordingDoer) Do(request *http.Request) (*http.Response, error) {
	doer.mu.Lock()
	doer.calls++
	doer.mu.Unlock()
	return doer.call(request)
}

func (doer *recordingDoer) CloseIdleConnections() {
	doer.mu.Lock()
	closeCall := doer.closeCall
	doer.mu.Unlock()
	if closeCall != nil {
		closeCall()
	}
	doer.mu.Lock()
	doer.closes++
	doer.mu.Unlock()
}

func (doer *recordingDoer) snapshot() (int, int) {
	doer.mu.Lock()
	defer doer.mu.Unlock()
	return doer.calls, doer.closes
}

func TestReferenceRecorderUsesProductionCodecAndReturnsDetachedObservation(t *testing.T) {
	request := validAdapterRequest(t)
	wantBody := []byte(`{"model":"Qwen/Qwen3-Reranker-0.6B","query":"query","documents":["alpha\nfirst","beta\nsecond"],"top_n":2}`)
	responseBody := validVLLMResponse(request, []int{1, 0}, []float64{0.25, 1})
	doer := &recordingDoer{}
	doer.call = func(httpRequest *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(httpRequest.Body)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(body, wantBody) || httpRequest.Header.Get("Authorization") != "Bearer record-secret" {
			t.Fatalf("record request = %s / %#v", body, httpRequest.Header)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(responseBody)), Request: httpRequest}, nil
	}
	scorer, err := newReferenceVLLMScorer(doer, "https://rerank.example.test/v1/rerank", "record-secret")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Unix(1_700_000_000, 0)
	times := []time.Time{base, base.Add(1250 * time.Microsecond)}
	recorder, err := newReferenceRecorder(scorer, time.Second, func() time.Time {
		value := times[0]
		times = times[1:]
		return value
	})
	if err != nil {
		t.Fatal(err)
	}

	recording, err := recorder.Record(context.Background(), request)
	if err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	if recording.InputDigest != "14e834df3abec747e19e9939d9eaad3c2b094b2d5fd3bb587d6ddc4f796e65b1" ||
		recording.CandidateDigest != "6b41daa452fbfae20bbd2caa2380be68219e6535baf42cf04814a5357e686bf0" ||
		recording.LatencyUS != 1250 || recording.Observation.ResponseID != "request-id" ||
		recording.Observation.Usage != (ReferenceUsage{PromptTokens: 7, TotalTokens: 7}) ||
		len(recording.Observation.Scores) != 2 || recording.Observation.Scores[0].StableID != request.Candidates[0].StableID {
		t.Fatalf("recording = %#v", recording)
	}
	recording.Observation.Scores[0].StableID = "mutated"
	if request.Candidates[0].StableID == "mutated" {
		t.Fatal("recording shares caller input")
	}
	recorder.Close()
	recorder.Close()
	if calls, closes := doer.snapshot(); calls != 1 || closes != 1 {
		t.Fatalf("calls/close = %d/%d, want 1/1", calls, closes)
	}
	if got, err := recorder.Record(context.Background(), request); !errors.Is(err, ErrRuntimeClosed) || !emptyReferenceRecording(got) {
		t.Fatalf("Record(after Close) = %#v, %v", got, err)
	}
}

func TestReferenceRecorderRejectsInvalidAndCanceledWorkBeforeHTTP(t *testing.T) {
	doer := &recordingDoer{call: func(*http.Request) (*http.Response, error) {
		return nil, errors.New("must not run")
	}}
	scorer, err := newReferenceVLLMScorer(doer, "https://rerank.example.test/v1/rerank", "key")
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := newReferenceRecorder(scorer, time.Second, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	request := validAdapterRequest(t)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if recording, err := recorder.Record(canceled, request); !errors.Is(err, context.Canceled) || !emptyReferenceRecording(recording) {
		t.Fatalf("Record(canceled) = %#v, %v", recording, err)
	}
	request.Query = ""
	if recording, err := recorder.Record(context.Background(), request); !errors.Is(err, ErrInvalidInput) || !emptyReferenceRecording(recording) {
		t.Fatalf("Record(invalid) = %#v, %v", recording, err)
	}
	if calls, _ := doer.snapshot(); calls != 0 {
		t.Fatalf("HTTP calls = %d, want 0", calls)
	}
}

func TestReferenceRecorderDeadlinePanicAndQueuedCancellationAreStable(t *testing.T) {
	request := validAdapterRequest(t)
	for _, test := range []struct {
		name string
		call func(*http.Request) (*http.Response, error)
	}{
		{name: "deadline", call: func(httpRequest *http.Request) (*http.Response, error) {
			<-httpRequest.Context().Done()
			return nil, httpRequest.Context().Err()
		}},
		{name: "panic", call: func(*http.Request) (*http.Response, error) { panic("private dependency detail") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			doer := &recordingDoer{call: test.call}
			scorer, err := newReferenceVLLMScorer(doer, "https://rerank.example.test/v1/rerank", "private-key")
			if err != nil {
				t.Fatal(err)
			}
			recorder, err := newReferenceRecorder(scorer, 10*time.Millisecond, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			if recording, err := recorder.Record(context.Background(), request); !errors.Is(err, ErrScoringFailed) || !emptyReferenceRecording(recording) {
				t.Fatalf("Record() = %#v, %v", recording, err)
			}
		})
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	doer := &recordingDoer{call: func(httpRequest *http.Request) (*http.Response, error) {
		close(entered)
		select {
		case <-release:
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(validVLLMResponse(request, []int{0, 1}, []float64{0.5, 0.4}))), Request: httpRequest}, nil
		case <-httpRequest.Context().Done():
			return nil, httpRequest.Context().Err()
		}
	}}
	scorer, _ := newReferenceVLLMScorer(doer, "https://rerank.example.test/v1/rerank", "key")
	recorder, _ := newReferenceRecorder(scorer, time.Second, time.Now)
	firstDone := make(chan struct{})
	go func() {
		_, _ = recorder.Record(context.Background(), request)
		close(firstDone)
	}()
	<-entered
	queued, cancel := context.WithCancel(context.Background())
	cancel()
	if recording, err := recorder.Record(queued, request); !errors.Is(err, context.Canceled) || !emptyReferenceRecording(recording) {
		t.Fatalf("queued Record() = %#v, %v", recording, err)
	}
	close(release)
	<-firstDone
	if calls, _ := doer.snapshot(); calls != 1 {
		t.Fatalf("HTTP calls = %d, want 1", calls)
	}
}

func TestReferenceRecorderConcurrentCloseWaitsForRecordAndTransportExactlyOnce(t *testing.T) {
	request := validAdapterRequest(t)
	recordEntered := make(chan struct{})
	releaseRecord := make(chan struct{})
	closeEntered := make(chan struct{})
	releaseClose := make(chan struct{})
	doer := &recordingDoer{
		call: func(httpRequest *http.Request) (*http.Response, error) {
			close(recordEntered)
			<-releaseRecord
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: io.NopCloser(bytes.NewReader(validVLLMResponse(
					request, []int{0, 1}, []float64{0.5, 0.4},
				))),
				Request: httpRequest,
			}, nil
		},
		closeCall: func() {
			close(closeEntered)
			<-releaseClose
		},
	}
	scorer, err := newReferenceVLLMScorer(doer, "https://rerank.example.test/v1/rerank", "key")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Unix(1_700_000_000, 0)
	times := []time.Time{base, base.Add(time.Millisecond)}
	recorder, err := newReferenceRecorder(scorer, time.Second, func() time.Time {
		value := times[0]
		times = times[1:]
		return value
	})
	if err != nil {
		t.Fatal(err)
	}
	recordDone := make(chan error, 1)
	go func() {
		_, recordErr := recorder.Record(context.Background(), request)
		recordDone <- recordErr
	}()
	<-recordEntered

	const closeCount = 8
	startClose := make(chan struct{})
	closeReady := make(chan struct{}, closeCount)
	closeDone := make(chan struct{}, closeCount)
	for range closeCount {
		go func() {
			closeReady <- struct{}{}
			<-startClose
			recorder.Close()
			closeDone <- struct{}{}
		}()
	}
	for range closeCount {
		<-closeReady
	}
	close(startClose)
	waitForReferenceRecorderClosed(t, recorder)
	assertNoReferenceRecorderClose(t, closeDone, "active Record")

	close(releaseRecord)
	if err := <-recordDone; err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	select {
	case <-closeEntered:
	case <-time.After(time.Second):
		t.Fatal("CloseIdleConnections() was not reached")
	}
	assertNoReferenceRecorderClose(t, closeDone, "transport close")

	close(releaseClose)
	for range closeCount {
		select {
		case <-closeDone:
		case <-time.After(time.Second):
			t.Fatal("concurrent Close() did not finish")
		}
	}
	if calls, closes := doer.snapshot(); calls != 1 || closes != 1 {
		t.Fatalf("calls/close = %d/%d, want 1/1", calls, closes)
	}
}

func waitForReferenceRecorderClosed(t *testing.T, recorder *ReferenceRecorder) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		recorder.mu.Lock()
		closed := recorder.closed
		recorder.mu.Unlock()
		if closed {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("Close() did not mark recorder closed")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func assertNoReferenceRecorderClose(t *testing.T, closeDone <-chan struct{}, waitingFor string) {
	t.Helper()
	select {
	case <-closeDone:
		t.Fatalf("Close() returned before %s completed", waitingFor)
	case <-time.After(20 * time.Millisecond):
	}
}

func emptyReferenceRecording(recording ReferenceRecording) bool {
	return recording.InputDigest == "" && recording.CandidateDigest == "" && recording.LatencyUS == 0 && recording.Observation.ResponseID == "" &&
		recording.Observation.Usage == (ReferenceUsage{}) && recording.Observation.Scores == nil
}

func TestNewReferenceRecorderIsRecordingOnlyAndValidatesConfiguration(t *testing.T) {
	recorder, err := NewReferenceRecorder(ReferenceRecorderConfig{
		Endpoint: "http://127.0.0.1:8000/v1/rerank", APIKey: "key", AllowPrivate: true,
	})
	if err != nil {
		t.Fatalf("NewReferenceRecorder() error = %v", err)
	}
	recorder.Close()
	for _, config := range []ReferenceRecorderConfig{
		{},
		{Endpoint: "http://public.example/v1/rerank", APIKey: "key"},
		{Endpoint: "https://example.test/v1/rerank", APIKey: " key"},
		{Endpoint: "https://example.test/v1/rerank", APIKey: "key", Timeout: MaximumScorerTimeout + time.Second},
	} {
		if got, err := NewReferenceRecorder(config); !errors.Is(err, ErrNotConfigured) || got != nil {
			t.Fatalf("NewReferenceRecorder(%#v) = %#v, %v", config, got, err)
		}
	}
}

func TestNewUnixReferenceRecorderUsesOnlyPinnedSocketAndIgnoresAmbientProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")

	temporaryDirectory, err := os.MkdirTemp("", "purify-r6a-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(temporaryDirectory) })
	socketPath := filepath.Join(temporaryDirectory, "vllm.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	request := validAdapterRequest(t)
	wantResponse := validVLLMResponse(request, []int{1, 0}, []float64{0.25, 1})
	requestSeen := make(chan error, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(response http.ResponseWriter, incoming *http.Request) {
		body, readErr := io.ReadAll(incoming.Body)
		if readErr != nil {
			requestSeen <- readErr
			return
		}
		wantBody := []byte(`{"model":"Qwen/Qwen3-Reranker-0.6B","query":"query","documents":["alpha\nfirst","beta\nsecond"],"top_n":2}`)
		if incoming.URL.Path != "/v1/rerank" || incoming.Host != "localhost" ||
			incoming.Header.Get("Authorization") != "Bearer unix-record-secret" || !bytes.Equal(body, wantBody) {
			requestSeen <- errors.New("unexpected UDS request")
			return
		}
		requestSeen <- nil
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write(wantResponse)
	})}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		if serveErr := <-serveDone; serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			t.Errorf("Serve() error = %v", serveErr)
		}
	})

	recorder, err := NewUnixReferenceRecorder(UnixReferenceRecorderConfig{
		SocketPath: socketPath,
		APIKey:     "unix-record-secret",
		Timeout:    time.Second,
	})
	if err != nil {
		t.Fatalf("NewUnixReferenceRecorder() error = %v", err)
	}
	t.Cleanup(recorder.Close)
	recording, err := recorder.Record(context.Background(), request)
	if err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	if err := <-requestSeen; err != nil {
		t.Fatal(err)
	}
	if recording.InputDigest == "" || recording.CandidateDigest == "" || len(recording.Observation.Scores) != 2 {
		t.Fatalf("recording = %#v", recording)
	}
}

func TestNewUnixReferenceRecorderRejectsInvalidConfiguration(t *testing.T) {
	validPath := "/run/purify-r6a/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/vllm.sock"
	for _, config := range []UnixReferenceRecorderConfig{
		{},
		{SocketPath: "relative.sock", APIKey: "key", Timeout: time.Second},
		{SocketPath: validPath + "/../vllm.sock", APIKey: "key", Timeout: time.Second},
		{SocketPath: validPath + "\n", APIKey: "key", Timeout: time.Second},
		{SocketPath: "/" + string(bytes.Repeat([]byte{'a'}, maxReferenceUnixSocketPathBytes)), APIKey: "key", Timeout: time.Second},
		{SocketPath: validPath, APIKey: " key", Timeout: time.Second},
		{SocketPath: validPath, APIKey: "key", Timeout: time.Millisecond},
		{SocketPath: validPath, APIKey: "key", Timeout: MaximumScorerTimeout + time.Second},
	} {
		if recorder, err := NewUnixReferenceRecorder(config); !errors.Is(err, ErrNotConfigured) || recorder != nil {
			t.Fatalf("NewUnixReferenceRecorder(%#v) = %#v, %v", config, recorder, err)
		}
	}
}
