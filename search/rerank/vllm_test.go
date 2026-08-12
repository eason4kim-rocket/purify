package rerank

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type requestCapture struct {
	mu            sync.Mutex
	calls         int
	method        string
	url           string
	contentType   string
	accept        string
	authorization string
	body          []byte
}

type closeCountingBody struct {
	io.Reader
	closed int
}

func (body *closeCountingBody) Close() error {
	body.closed++
	return nil
}

func (capture *requestCapture) roundTrip(body []byte, status int) roundTripFunc {
	return func(request *http.Request) (*http.Response, error) {
		encoded, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		capture.mu.Lock()
		capture.calls++
		capture.method = request.Method
		capture.url = request.URL.String()
		capture.contentType = request.Header.Get("Content-Type")
		capture.accept = request.Header.Get("Accept")
		capture.authorization = request.Header.Get("Authorization")
		capture.body = append([]byte(nil), encoded...)
		capture.mu.Unlock()
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(body)),
			Request:    request,
		}, nil
	}
}

func (capture *requestCapture) snapshot() (int, string, string, string, string, []byte) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return capture.calls, capture.method, capture.url, capture.contentType, capture.accept, append([]byte(nil), capture.body...)
}

func TestReferenceVLLMScorerUsesExactWireAndIndexJoin(t *testing.T) {
	request := validAdapterRequest(t)
	body := validVLLMResponse(request, []int{1, 0}, []float64{0.25, 1})
	capture := &requestCapture{}
	client := &http.Client{Transport: capture.roundTrip(body, http.StatusOK)}
	scorer, err := newReferenceVLLMScorer(client, "https://rerank.example.test/v1/rerank", "process-secret")
	if err != nil {
		t.Fatal(err)
	}

	results, err := scorer.Score(context.Background(), request)
	if err != nil {
		t.Fatalf("Score() error = %v", err)
	}
	if len(results) != 2 || results[0] != (ScoreResult{StableID: request.Candidates[0].StableID, RelevanceScore: 0.25}) ||
		results[1] != (ScoreResult{StableID: request.Candidates[1].StableID, RelevanceScore: 1}) {
		t.Fatalf("Score() results = %#v", results)
	}

	calls, method, requestURL, contentType, accept, encoded := capture.snapshot()
	if calls != 1 || capture.authorization != "Bearer process-secret" || method != http.MethodPost ||
		requestURL != "https://rerank.example.test/v1/rerank" || contentType != "application/json" || accept != "application/json" {
		t.Fatalf("request shape = calls=%d method=%q URL=%q content-type=%q accept=%q authorization=%q", calls, method, requestURL, contentType, accept, capture.authorization)
	}
	want := `{"model":"Qwen/Qwen3-Reranker-0.6B","query":"query","documents":["alpha\nfirst","beta\nsecond"],"top_n":2}`
	if string(encoded) != want {
		t.Fatalf("request body = %s, want %s", encoded, want)
	}
	for _, forbidden := range []string{"stable_id", "provider_rank", request.Candidates[0].StableID, "https://"} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("request body leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestReferenceVLLMScorerRejectsMalformedResponses(t *testing.T) {
	request := validAdapterRequest(t)
	valid := string(validVLLMResponse(request, []int{0, 1}, []float64{0, 1}))
	longID := strings.Repeat("i", MaxVLLMResponseIDBytes+1)
	tests := []struct {
		name string
		body string
	}{
		{name: "root missing", body: strings.Replace(valid, `"id":"request-id",`, "", 1)},
		{name: "root null", body: strings.Replace(valid, `"usage":{"prompt_tokens":7,"total_tokens":7}`, `"usage":null`, 1)},
		{name: "id null", body: strings.Replace(valid, `"id":"request-id"`, `"id":null`, 1)},
		{name: "model null", body: strings.Replace(valid, `"model":"`+ReferenceServedModel+`"`, `"model":null`, 1)},
		{name: "root unknown", body: strings.Replace(valid, `"results":`, `"unknown":1,"results":`, 1)},
		{name: "root case smuggle", body: strings.Replace(valid, `"id":`, `"ID":`, 1)},
		{name: "root duplicate", body: strings.Replace(valid, `"id":"request-id"`, `"id":"first","id":"request-id"`, 1)},
		{name: "root escaped duplicate", body: strings.Replace(valid, `"id":"request-id"`, `"id":"first","\u0069d":"request-id"`, 1)},
		{name: "trailing value", body: valid + `{}`},
		{name: "id too long", body: strings.Replace(valid, "request-id", longID, 1)},
		{name: "model mismatch", body: strings.Replace(valid, ReferenceServedModel, "other-model", 1)},
		{name: "usage missing", body: strings.Replace(valid, `"prompt_tokens":7,`, "", 1)},
		{name: "usage token null", body: strings.Replace(valid, `"prompt_tokens":7`, `"prompt_tokens":null`, 1)},
		{name: "usage fractional", body: strings.Replace(valid, `"prompt_tokens":7`, `"prompt_tokens":7.5`, 1)},
		{name: "usage unknown", body: strings.Replace(valid, `"total_tokens":7`, `"total_tokens":7,"extra":0`, 1)},
		{name: "usage case smuggle", body: strings.Replace(valid, `"prompt_tokens":7`, `"Prompt_Tokens":7`, 1)},
		{name: "usage duplicate", body: strings.Replace(valid, `"prompt_tokens":7`, `"prompt_tokens":0,"prompt_tokens":7`, 1)},
		{name: "usage escaped duplicate", body: strings.Replace(valid, `"prompt_tokens":7`, `"prompt_tokens":0,"\u0070rompt_tokens":7`, 1)},
		{name: "usage unequal", body: strings.Replace(valid, `"total_tokens":7`, `"total_tokens":8`, 1)},
		{name: "usage negative", body: strings.Replace(valid, `"prompt_tokens":7`, `"prompt_tokens":-1`, 1)},
		{name: "usage over", body: strings.Replace(valid, `"prompt_tokens":7`, `"prompt_tokens":1000001`, 1)},
		{name: "result unknown", body: strings.Replace(valid, `"index":0`, `"extra":0,"index":0`, 1)},
		{name: "result case smuggle", body: strings.Replace(valid, `"index":0`, `"Index":0`, 1)},
		{name: "result duplicate", body: strings.Replace(valid, `"index":0`, `"index":1,"index":0`, 1)},
		{name: "result escaped duplicate", body: strings.Replace(valid, `"index":0`, `"index":1,"\u0069ndex":0`, 1)},
		{name: "results missing one", body: strings.Replace(valid, `,{"index":1,"document":{"text":"beta\nsecond","multi_modal":null},"relevance_score":1`, ``, 1)},
		{name: "results extra one", body: strings.Replace(valid, `]}`, `,{"index":0,"document":{"text":"alpha\nfirst","multi_modal":null},"relevance_score":0}]}`, 1)},
		{name: "result index null", body: strings.Replace(valid, `"index":0`, `"index":null`, 1)},
		{name: "result document null", body: strings.Replace(valid, `"document":{"text":"alpha\nfirst","multi_modal":null}`, `"document":null`, 1)},
		{name: "result score null", body: strings.Replace(valid, `"relevance_score":0`, `"relevance_score":null`, 1)},
		{name: "document missing", body: strings.Replace(valid, `,"multi_modal":null`, "", 1)},
		{name: "document case smuggle", body: strings.Replace(valid, `"text":"alpha\nfirst"`, `"Text":"alpha\nfirst"`, 1)},
		{name: "document duplicate", body: strings.Replace(valid, `"text":"alpha\nfirst"`, `"text":"changed","text":"alpha\nfirst"`, 1)},
		{name: "document escaped duplicate", body: strings.Replace(valid, `"text":"alpha\nfirst"`, `"text":"changed","\u0074ext":"alpha\nfirst"`, 1)},
		{name: "document nonnull multimodal", body: strings.Replace(valid, `"multi_modal":null`, `"multi_modal":{}`, 1)},
		{name: "echo mismatch", body: strings.Replace(valid, "alpha\\nfirst", "changed\\ntext", 1)},
		{name: "duplicate index", body: strings.Replace(valid, `"index":1`, `"index":0`, 1)},
		{name: "index outside", body: strings.Replace(valid, `"index":1`, `"index":2`, 1)},
		{name: "score over", body: strings.Replace(valid, `"relevance_score":1`, `"relevance_score":1.1`, 1)},
		{name: "score negative", body: strings.Replace(valid, `"relevance_score":0`, `"relevance_score":-0.1`, 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			capture := &requestCapture{}
			client := &http.Client{Transport: capture.roundTrip([]byte(test.body), http.StatusOK)}
			scorer, err := newReferenceVLLMScorer(client, "https://rerank.example.test/v1/rerank", "do-not-leak-key")
			if err != nil {
				t.Fatal(err)
			}
			results, err := scorer.Score(context.Background(), request)
			if !errors.Is(err, ErrScoringFailed) || results != nil {
				t.Fatalf("Score() = %#v, %v; want nil ErrScoringFailed", results, err)
			}
			for _, secret := range []string{"do-not-leak-key", "rerank.example.test", ReferenceServedModel, "alpha", test.body} {
				if secret != "" && strings.Contains(err.Error(), secret) {
					t.Fatalf("error leaked provider input %q: %v", secret, err)
				}
			}
		})
	}
}

func TestReferenceVLLMScorerAcceptsLockedBoundaries(t *testing.T) {
	request := validAdapterRequest(t)
	body := string(validVLLMResponse(request, []int{0, 1}, []float64{math.Copysign(0, -1), 1}))
	body = strings.Replace(body, "request-id", strings.Repeat("i", MaxVLLMResponseIDBytes), 1)
	body = strings.ReplaceAll(body, `:7`, `:1000000`)
	scorer, err := newReferenceVLLMScorer(&http.Client{Transport: (&requestCapture{}).roundTrip([]byte(body), http.StatusOK)}, "https://rerank.example.test/v1/rerank", "key")
	if err != nil {
		t.Fatal(err)
	}
	results, err := scorer.Score(context.Background(), request)
	if err != nil || len(results) != 2 || results[0].RelevanceScore != 0 || math.Signbit(results[0].RelevanceScore) || results[1].RelevanceScore != 1 {
		t.Fatalf("Score(boundaries) = %#v, %v", results, err)
	}
}

func TestReferenceVLLMScorerValidatesStatusBodyLimitAndInputBeforeHTTP(t *testing.T) {
	request := validAdapterRequest(t)
	for _, test := range []struct {
		name   string
		status int
		body   []byte
	}{
		{name: "provider error", status: http.StatusBadRequest, body: []byte(`{"secret":"provider-body"}`)},
		{name: "response over limit", status: http.StatusOK, body: bytes.Repeat([]byte{' '}, MaxVLLMResponseBytes+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			capture := &requestCapture{}
			scorer, err := newReferenceVLLMScorer(
				&http.Client{Transport: capture.roundTrip(test.body, test.status)},
				"https://rerank.example.test/v1/rerank",
				"secret-key",
			)
			if err != nil {
				t.Fatal(err)
			}
			results, err := scorer.Score(context.Background(), request)
			if !errors.Is(err, ErrScoringFailed) || results != nil {
				t.Fatalf("Score() = %#v, %v", results, err)
			}
			for _, forbidden := range []string{"secret-key", "provider-body", "rerank.example.test"} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("error leaked %q: %v", forbidden, err)
				}
			}
		})
	}

	t.Run("response plus transport error closes body", func(t *testing.T) {
		body := &closeCountingBody{Reader: strings.NewReader(`{"secret":"provider-body"}`)}
		scorer, err := newReferenceVLLMScorer(rerankHTTPDoerFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusBadGateway, Body: body, Request: request}, errors.New("private transport detail")
		}), "https://rerank.example.test/v1/rerank", "secret-key")
		if err != nil {
			t.Fatal(err)
		}
		if results, err := scorer.Score(context.Background(), request); !errors.Is(err, ErrScoringFailed) || results != nil {
			t.Fatalf("Score() = %#v, %v", results, err)
		}
		if body.closed != 1 {
			t.Fatalf("body close count = %d, want 1", body.closed)
		}
	})

	calls := 0
	scorer, err := newReferenceVLLMScorer(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("must not run")
	})}, "https://rerank.example.test/v1/rerank", "key")
	if err != nil {
		t.Fatal(err)
	}
	tooMany := make([]ScoringCandidate, MaxCandidates+1)
	for index := range tooMany {
		digest := sha256.Sum256([]byte{byte(index)})
		tooMany[index] = ScoringCandidate{StableID: hex.EncodeToString(digest[:]), ProviderRank: 1, Text: "a\nb"}
	}
	for _, malformed := range []ScoreRequest{
		{Query: "query", Candidates: []ScoringCandidate{{StableID: strings.Repeat("a", 65), ProviderRank: 1, Text: "a\nb"}}},
		{Query: "query", Candidates: []ScoringCandidate{{StableID: strings.Repeat("A", 64), ProviderRank: 1, Text: "a\nb"}}},
		{Query: "query", Candidates: []ScoringCandidate{{StableID: strings.Repeat("a", 64), ProviderRank: 1, Text: "no newline"}}},
		{Query: "query", Candidates: []ScoringCandidate{{StableID: strings.Repeat("a", 64), ProviderRank: 0, Text: "a\nb"}}},
		{Query: "query", Candidates: []ScoringCandidate{{StableID: strings.Repeat("a", 64), ProviderRank: 1, Text: "a\nb"}, {StableID: strings.Repeat("a", 64), ProviderRank: 2, Text: "c\nd"}}},
		{Query: "query", Candidates: []ScoringCandidate{{StableID: strings.Repeat("a", 64), ProviderRank: 1, Text: "a\n\xff"}}},
		{Query: "query", Candidates: []ScoringCandidate{{StableID: strings.Repeat("a", 64), ProviderRank: 1, Text: "a\n" + strings.Repeat("b", MaxDocumentBytes)}}},
		{Query: "query", Candidates: tooMany},
	} {
		if results, err := scorer.Score(context.Background(), malformed); !errors.Is(err, ErrInvalidInput) || results != nil {
			t.Fatalf("malformed Score() = %#v, %v", results, err)
		}
	}
	empty, err := scorer.Score(context.Background(), ScoreRequest{Query: "query"})
	if err != nil || empty == nil || len(empty) != 0 || calls != 0 {
		t.Fatalf("empty Score() = %#v, %v; calls=%d", empty, err, calls)
	}
}

type rerankHTTPDoerFunc func(*http.Request) (*http.Response, error)

func (function rerankHTTPDoerFunc) Do(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestCanonicalVLLMRequestDigestAndBudgets(t *testing.T) {
	request := ScoreRequest{Query: "query", Candidates: []ScoringCandidate{{
		StableID: strings.Repeat("a", 64), ProviderRank: 1, Text: "title\nsnippet",
	}}}
	encoded, digest, err := canonicalVLLMRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	wantBody := `{"model":"Qwen/Qwen3-Reranker-0.6B","query":"query","documents":["title\nsnippet"],"top_n":1}`
	if string(encoded) != wantBody {
		t.Fatalf("body = %s, want %s", encoded, wantBody)
	}
	sum := sha256.Sum256([]byte(wantBody))
	if digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("digest = %q, want %x", digest, sum)
	}
	if digest != "8927507a25a5ce509ec0390fb58c152b7eb177b853c022e3f1e50930742f3b93" {
		t.Fatalf("digest = %q, want locked vector", digest)
	}

	exact := exactSizedVLLMRequest(t, MaxVLLMRequestBytes)
	encoded, _, err = canonicalVLLMRequest(exact)
	if err != nil || len(encoded) != MaxVLLMRequestBytes {
		t.Fatalf("exact request bytes/error = %d/%v", len(encoded), err)
	}
	over := exact
	over.Candidates = append([]ScoringCandidate(nil), exact.Candidates...)
	for index := range over.Candidates {
		over.Candidates[index] = ScoringCandidate{
			StableID:     over.Candidates[index].StableID,
			ProviderRank: over.Candidates[index].ProviderRank,
			Text:         strings.Clone(over.Candidates[index].Text),
		}
	}
	changed := false
	for index := range over.Candidates {
		if strings.Contains(over.Candidates[index].Text, "a") {
			over.Candidates[index].Text = strings.Replace(over.Candidates[index].Text, "a", `"`, 1)
			changed = true
			break
		}
	}
	if !changed {
		t.Fatal("exact request has no tunable byte")
	}
	if encoded, _, err := canonicalVLLMRequest(over); !errors.Is(err, ErrInvalidInput) || encoded != nil {
		t.Fatalf("N+1 request = %d bytes, %v; want ErrInvalidInput", len(encoded), err)
	}

	response := validVLLMResponse(request, []int{0}, []float64{1})
	response = append(response, bytes.Repeat([]byte{' '}, MaxVLLMResponseBytes-len(response))...)
	if len(response) != MaxVLLMResponseBytes {
		t.Fatalf("exact response bytes = %d", len(response))
	}
	for _, size := range []int{MaxVLLMResponseBytes, MaxVLLMResponseBytes + 1} {
		capture := &requestCapture{}
		body := response
		if size > len(body) {
			body = append(append([]byte(nil), body...), ' ')
		}
		scorer, err := newReferenceVLLMScorer(&http.Client{Transport: capture.roundTrip(body, http.StatusOK)}, "https://rerank.example.test/v1/rerank", "key")
		if err != nil {
			t.Fatal(err)
		}
		results, scoreErr := scorer.Score(context.Background(), request)
		if size == MaxVLLMResponseBytes && (scoreErr != nil || len(results) != 1) {
			t.Fatalf("exact response Score() = %#v, %v", results, scoreErr)
		}
		if size > MaxVLLMResponseBytes && (!errors.Is(scoreErr, ErrScoringFailed) || results != nil) {
			t.Fatalf("N+1 response Score() = %#v, %v", results, scoreErr)
		}
	}
}

func TestCertifiedProfileRegistryStartsEmpty(t *testing.T) {
	for _, profile := range []string{"", ReferenceProfileID, "unknown", strings.ToUpper(ReferenceProfileID)} {
		if err := RequireCertifiedProfile(profile); !errors.Is(err, ErrProfileUnavailable) {
			t.Fatalf("RequireCertifiedProfile(%q) = %v, want ErrProfileUnavailable", profile, err)
		}
	}
}

func validAdapterRequest(t *testing.T) ScoreRequest {
	t.Helper()
	first, err := CandidateID("https://alpha.example.test/a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := CandidateID("https://beta.example.test/b")
	if err != nil {
		t.Fatal(err)
	}
	return ScoreRequest{Query: "query", Candidates: []ScoringCandidate{
		{StableID: first, ProviderRank: 1, Text: "alpha\nfirst"},
		{StableID: second, ProviderRank: 2, Text: "beta\nsecond"},
	}}
}

func validVLLMResponse(request ScoreRequest, order []int, scores []float64) []byte {
	var builder strings.Builder
	builder.WriteString(`{"id":"request-id","model":"` + ReferenceServedModel + `","usage":{"prompt_tokens":7,"total_tokens":7},"results":[`)
	for outputIndex, candidateIndex := range order {
		if outputIndex > 0 {
			builder.WriteByte(',')
		}
		score := scores[candidateIndex]
		builder.WriteString(`{"index":`)
		builder.WriteString(string(rune('0' + candidateIndex)))
		builder.WriteString(`,"document":{"text":"`)
		builder.WriteString(strings.ReplaceAll(request.Candidates[candidateIndex].Text, "\n", `\n`))
		builder.WriteString(`","multi_modal":null},"relevance_score":`)
		switch {
		case math.Signbit(score) && score == 0:
			builder.WriteString("-0")
		case score == 0:
			builder.WriteString("0")
		case score == 1:
			builder.WriteString("1")
		default:
			builder.WriteString("0.25")
		}
		builder.WriteByte('}')
	}
	builder.WriteString(`]}`)
	return []byte(builder.String())
}

func exactSizedVLLMRequest(t *testing.T, size int) ScoreRequest {
	t.Helper()
	request := ScoreRequest{Query: "query", Candidates: make([]ScoringCandidate, MaxCandidates)}
	for index := range request.Candidates {
		stableID := sha256.Sum256([]byte{byte(index)})
		request.Candidates[index] = ScoringCandidate{
			StableID:     hex.EncodeToString(stableID[:]),
			ProviderRank: index + 1,
			Text:         "a\n" + strings.Repeat("a", MaxDocumentBytes-2),
		}
	}
	base, _, err := canonicalVLLMRequestUnbounded(request)
	if err != nil {
		t.Fatal(err)
	}
	delta := size - len(base)
	if delta < 0 {
		t.Fatalf("base request already exceeds target: %d > %d", len(base), size)
	}
	remainingFive := delta / 5
	remainingOne := delta % 5
	for index := range request.Candidates {
		text := []byte(request.Candidates[index].Text)
		for position := range text {
			if text[position] != 'a' {
				continue
			}
			if remainingFive > 0 {
				text[position] = '<'
				remainingFive--
			} else if remainingOne > 0 {
				text[position] = '"'
				remainingOne--
			} else {
				break
			}
		}
		request.Candidates[index].Text = string(text)
	}
	if remainingFive != 0 || remainingOne != 0 {
		t.Fatalf("could not tune exact request: five=%d one=%d", remainingFive, remainingOne)
	}
	encoded, _, err := canonicalVLLMRequestUnbounded(request)
	if err != nil || len(encoded) != size {
		t.Fatalf("tuned request = %d bytes, %v; want %d", len(encoded), err, size)
	}
	return request
}
