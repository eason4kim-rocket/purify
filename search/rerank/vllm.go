package rerank

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MaxVLLMRequestBytes    = 256 << 10
	MaxVLLMResponseBytes   = 1 << 20
	MaxVLLMResponseIDBytes = 256
	maxVLLMTokenCount      = 1_000_000
	maxRerankSecretBytes   = 16 << 10
	maxRerankEndpointBytes = 16 << 10
)

type vllmRequest struct {
	Model     string   `json:"model"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
	TopN      int      `json:"top_n"`
}

// VLLMScorer is the low-level strict adapter for the pinned reference
// descriptor. R-3 exposes no production constructor: R-6a must define an
// authenticated deployment handoff and R-6 must admit its matching manifest.
type VLLMScorer struct {
	client   rerankHTTPDoer
	endpoint string
	apiKey   string
	profile  profileDescriptor
}

type rerankHTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

func newReferenceVLLMScorer(client rerankHTTPDoer, endpoint, apiKey string) (*VLLMScorer, error) {
	return newVLLMScorer(client, endpoint, apiKey, referenceProfile)
}

func newVLLMScorer(client rerankHTTPDoer, endpoint, apiKey string, profile profileDescriptor) (*VLLMScorer, error) {
	if client == nil || !validRerankSecret(apiKey) {
		return nil, ErrNotConfigured
	}
	parsed, err := parseVLLMEndpoint(endpoint)
	if err != nil {
		return nil, ErrNotConfigured
	}
	return &VLLMScorer{
		client:   client,
		endpoint: parsed.String(),
		apiKey:   strings.Clone(apiKey),
		profile:  profile,
	}, nil
}

// Score sends one exact vLLM rerank request and validates the complete
// response before exposing any score. Dependency details are deliberately
// collapsed into stable domain errors.
func (scorer *VLLMScorer) Score(ctx context.Context, request ScoreRequest) ([]ScoreResult, error) {
	if scorer == nil || scorer.client == nil {
		return nil, ErrNotConfigured
	}
	if ctx == nil {
		return nil, ErrInvalidInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateAdapterRequest(request); err != nil {
		return nil, ErrInvalidInput
	}
	if len(request.Candidates) == 0 {
		return make([]ScoreResult, 0), nil
	}
	encoded, _, err := canonicalVLLMRequest(request)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, scorer.endpoint, bytes.NewReader(encoded))
	if err != nil {
		return nil, ErrScoringFailed
	}
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+scorer.apiKey)

	response, err := scorer.client.Do(httpRequest)
	if ctxErr := ctx.Err(); ctxErr != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, ctxErr
	}
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, ErrScoringFailed
	}
	if response == nil || response.Body == nil {
		return nil, ErrScoringFailed
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, MaxVLLMResponseBytes+1))
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if readErr != nil || len(body) > MaxVLLMResponseBytes || response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, ErrScoringFailed
	}
	results, err := decodeVLLMResponse(body, request, scorer.profile)
	if err != nil {
		return nil, ErrScoringFailed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

func (scorer *VLLMScorer) CloseIdleConnections() {
	if scorer == nil || scorer.client == nil {
		return
	}
	if closer, ok := scorer.client.(idleConnectionCloser); ok {
		closer.CloseIdleConnections()
	}
}

func canonicalVLLMRequest(request ScoreRequest) ([]byte, string, error) {
	encoded, digest, err := canonicalVLLMRequestUnbounded(request)
	if err != nil || len(encoded) > MaxVLLMRequestBytes {
		return nil, "", ErrInvalidInput
	}
	return encoded, digest, nil
}

func canonicalVLLMRequestUnbounded(request ScoreRequest) ([]byte, string, error) {
	if err := validateAdapterRequest(request); err != nil {
		return nil, "", ErrInvalidInput
	}
	documents := make([]string, len(request.Candidates))
	for index, candidate := range request.Candidates {
		documents[index] = strings.Clone(candidate.Text)
	}
	encoded, err := json.Marshal(vllmRequest{
		Model:     ReferenceServedModel,
		Query:     strings.Clone(request.Query),
		Documents: documents,
		TopN:      len(documents),
	})
	if err != nil {
		return nil, "", ErrInvalidInput
	}
	digest := sha256.Sum256(encoded)
	return encoded, hex.EncodeToString(digest[:]), nil
}

func validateAdapterRequest(request ScoreRequest) error {
	if err := validateQuery(request.Query); err != nil || len(request.Candidates) > MaxCandidates {
		return ErrInvalidInput
	}
	seen := make(map[string]struct{}, len(request.Candidates))
	aggregate := 0
	for _, candidate := range request.Candidates {
		if !validStableID(candidate.StableID) || candidate.ProviderRank < 1 || candidate.ProviderRank > MaxCandidates ||
			len(candidate.Text) == 0 || len(candidate.Text) > MaxDocumentBytes || !utf8.ValidString(candidate.Text) {
			return ErrInvalidInput
		}
		if _, duplicate := seen[candidate.StableID]; duplicate {
			return ErrInvalidInput
		}
		seen[candidate.StableID] = struct{}{}
		newlines := 0
		for _, character := range candidate.Text {
			if character == '\n' {
				newlines++
				continue
			}
			if unicode.IsControl(character) {
				return ErrInvalidInput
			}
		}
		if newlines != 1 || len(candidate.Text) > MaxAggregateDocumentBytes-aggregate {
			return ErrInvalidInput
		}
		aggregate += len(candidate.Text)
	}
	return nil
}

func validStableID(value string) bool {
	if len(value) != sha256HexBytes || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func validRerankSecret(value string) bool {
	if len(value) == 0 || len(value) > maxRerankSecretBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func parseVLLMEndpoint(raw string) (*url.URL, error) {
	if len(raw) == 0 || len(raw) > maxRerankEndpointBytes || !utf8.ValidString(raw) || strings.TrimSpace(raw) != raw {
		return nil, ErrInvalidInput
	}
	for _, character := range raw {
		if unicode.IsControl(character) {
			return nil, ErrInvalidInput
		}
	}
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Opaque != "" || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawPath != "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Path != "/v1/rerank" || strings.HasSuffix(parsed.Host, ":") {
		return nil, ErrInvalidInput
	}
	if parsed.String() != raw {
		return nil, ErrInvalidInput
	}
	return parsed, nil
}

func decodeVLLMResponse(raw []byte, request ScoreRequest, profile profileDescriptor) ([]ScoreResult, error) {
	if len(raw) == 0 || !utf8.Valid(raw) || rejectDuplicateJSONFields(raw) != nil {
		return nil, ErrScoringFailed
	}
	root, err := exactJSONObject(raw, "id", "model", "usage", "results")
	if err != nil {
		return nil, ErrScoringFailed
	}
	var responseID, model string
	if isJSONNull(root["id"]) || isJSONNull(root["model"]) || isJSONNull(root["usage"]) || isJSONNull(root["results"]) ||
		json.Unmarshal(root["id"], &responseID) != nil || len(responseID) > MaxVLLMResponseIDBytes || !utf8.ValidString(responseID) || containsControl(responseID) ||
		json.Unmarshal(root["model"], &model) != nil || model != profile.servedModel {
		return nil, ErrScoringFailed
	}
	usage, err := exactJSONObject(root["usage"], "prompt_tokens", "total_tokens")
	if err != nil {
		return nil, ErrScoringFailed
	}
	var promptTokens, totalTokens int64
	if isJSONNull(usage["prompt_tokens"]) || isJSONNull(usage["total_tokens"]) ||
		json.Unmarshal(usage["prompt_tokens"], &promptTokens) != nil || json.Unmarshal(usage["total_tokens"], &totalTokens) != nil ||
		promptTokens < 0 || promptTokens > maxVLLMTokenCount || totalTokens < 0 || totalTokens > maxVLLMTokenCount || promptTokens != totalTokens {
		return nil, ErrScoringFailed
	}
	if isJSONNull(root["results"]) {
		return nil, ErrScoringFailed
	}
	var rawResults []json.RawMessage
	if json.Unmarshal(root["results"], &rawResults) != nil || len(rawResults) != len(request.Candidates) {
		return nil, ErrScoringFailed
	}
	results := make([]ScoreResult, len(request.Candidates))
	seen := make([]bool, len(request.Candidates))
	for _, rawResult := range rawResults {
		result, err := exactJSONObject(rawResult, "index", "document", "relevance_score")
		if err != nil {
			return nil, ErrScoringFailed
		}
		var index int
		var score float64
		if isJSONNull(result["index"]) || isJSONNull(result["document"]) || isJSONNull(result["relevance_score"]) ||
			json.Unmarshal(result["index"], &index) != nil || index < 0 || index >= len(request.Candidates) || seen[index] ||
			json.Unmarshal(result["relevance_score"], &score) != nil || math.IsNaN(score) || math.IsInf(score, 0) || score < 0 || score > 1 {
			return nil, ErrScoringFailed
		}
		document, err := exactJSONObject(result["document"], "text", "multi_modal")
		if err != nil || !isJSONNull(document["multi_modal"]) {
			return nil, ErrScoringFailed
		}
		var text string
		if json.Unmarshal(document["text"], &text) != nil || text != request.Candidates[index].Text {
			return nil, ErrScoringFailed
		}
		if score == 0 {
			score = 0
		}
		seen[index] = true
		results[index] = ScoreResult{StableID: strings.Clone(request.Candidates[index].StableID), RelevanceScore: score}
	}
	for _, present := range seen {
		if !present {
			return nil, ErrScoringFailed
		}
	}
	return results, nil
}

func exactJSONObject(raw []byte, fields ...string) (map[string]json.RawMessage, error) {
	if isJSONNull(raw) {
		return nil, ErrScoringFailed
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) != len(fields) {
		return nil, ErrScoringFailed
	}
	allowed := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		allowed[field] = struct{}{}
	}
	for key, value := range object {
		if _, ok := allowed[key]; !ok || len(value) == 0 {
			return nil, ErrScoringFailed
		}
	}
	for _, field := range fields {
		if _, ok := object[field]; !ok {
			return nil, ErrScoringFailed
		}
	}
	return object, nil
}

func isJSONNull(raw []byte) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func rejectDuplicateJSONFields(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var consumeValue func() error
	consumeValue = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]struct{}{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("rerank: invalid JSON object key")
				}
				if _, duplicate := seen[key]; duplicate {
					return errors.New("rerank: duplicate JSON field")
				}
				seen[key] = struct{}{}
				if err := consumeValue(); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim('}') {
				return errors.New("rerank: invalid JSON object")
			}
		case '[':
			for decoder.More() {
				if err := consumeValue(); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim(']') {
				return errors.New("rerank: invalid JSON array")
			}
		default:
			return errors.New("rerank: invalid JSON delimiter")
		}
		return nil
	}
	if err := consumeValue(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("rerank: trailing JSON value")
		}
		return err
	}
	return nil
}
