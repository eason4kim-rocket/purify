// Package rerank provides the provider-neutral, deterministic core used to
// prepare search candidates for a relevance scorer and order its results.
// Network transports and Search orchestration live in later layers.
package rerank

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/use-agent/purify/publicnet"
)

const (
	// MaxCandidates is the largest relevance batch accepted by the pure core.
	MaxCandidates = 20

	MaxQueryRunes = 400
	MaxQueryWords = 50

	// Raw provider metadata is validated before any allocation or UTF-8 scan.
	MaxRawTitleBytes     = 16 << 10
	MaxRawSnippetBytes   = 64 << 10
	MaxCanonicalURLBytes = 16 << 10

	// Scorer documents are a rune-safe title prefix, one newline byte, and a
	// rune-safe snippet prefix. Twenty full documents total exactly 120 KiB.
	MaxTitleBytes             = 1536
	MaxDocumentBytes          = 6 << 10
	MaxAggregateDocumentBytes = 120 << 10

	stableIDDomain = "rerank-candidate-v1\x00"
	sha256HexBytes = sha256.Size * 2
)

var (
	ErrInvalidInput  = errors.New("rerank: invalid input")
	ErrNotConfigured = errors.New("rerank: scorer is not configured")
	ErrScoringFailed = errors.New("rerank: scoring failed")
	ErrInvalidScores = errors.New("rerank: invalid scores")
	ErrInvalidGrades = errors.New("rerank: invalid grades")
)

// Candidate is the already-normalized provider metadata consumed by the pure
// rerank core. ProviderRank is the original one-based provider position; it is
// deliberately not recomputed after filtering.
type Candidate struct {
	CanonicalURL string
	ProviderRank int
	Title        string
	Snippet      string
}

// ScoringCandidate is the transport-neutral model input. URLs and provider
// metadata are deliberately absent from the text sent to a scorer.
type ScoringCandidate struct {
	StableID     string
	ProviderRank int
	Text         string
}

// ScoreRequest is the complete pure request passed to a Scorer.
type ScoreRequest struct {
	Query      string
	Candidates []ScoringCandidate
}

// CandidateID returns the versioned stable ID for an already-canonical URL.
// Validation never rewrites the URL: the digest covers its exact UTF-8 bytes.
func CandidateID(canonicalURL string) (string, error) {
	if len(canonicalURL) == 0 || len(canonicalURL) > MaxCanonicalURLBytes ||
		!utf8.ValidString(canonicalURL) || containsControl(canonicalURL) ||
		strings.TrimSpace(canonicalURL) != canonicalURL {
		return "", ErrInvalidInput
	}
	canonical, _, err := publicnet.NormalizeHTTPURL(canonicalURL, nil, false)
	if err != nil || canonical != canonicalURL {
		return "", ErrInvalidInput
	}

	digest := sha256.New()
	_, _ = digest.Write([]byte(stableIDDomain))
	_, _ = digest.Write([]byte(canonicalURL))
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// BuildRequest validates and bounds already-normalized candidates without
// mutating caller-owned data.
func BuildRequest(query string, candidates []Candidate) (ScoreRequest, error) {
	if err := validateQuery(query); err != nil || len(candidates) > MaxCandidates {
		return ScoreRequest{}, ErrInvalidInput
	}

	request := ScoreRequest{
		Query:      strings.Clone(query),
		Candidates: make([]ScoringCandidate, 0, len(candidates)),
	}
	seenURLs := make(map[string]struct{}, len(candidates))
	aggregateBytes := 0
	for _, candidate := range candidates {
		if candidate.ProviderRank < 1 || candidate.ProviderRank > MaxCandidates ||
			len(candidate.Title) > MaxRawTitleBytes || len(candidate.Snippet) > MaxRawSnippetBytes {
			return ScoreRequest{}, ErrInvalidInput
		}
		if err := validateProviderText(candidate.Title); err != nil {
			return ScoreRequest{}, ErrInvalidInput
		}
		if err := validateProviderText(candidate.Snippet); err != nil {
			return ScoreRequest{}, ErrInvalidInput
		}
		stableID, err := CandidateID(candidate.CanonicalURL)
		if err != nil {
			return ScoreRequest{}, ErrInvalidInput
		}
		if _, exists := seenURLs[candidate.CanonicalURL]; exists {
			return ScoreRequest{}, ErrInvalidInput
		}
		seenURLs[candidate.CanonicalURL] = struct{}{}

		title := utf8Prefix(candidate.Title, MaxTitleBytes)
		snippet := utf8Prefix(candidate.Snippet, MaxDocumentBytes-1-len(title))
		document := title + "\n" + snippet
		if len(document) > MaxDocumentBytes || len(document) > MaxAggregateDocumentBytes-aggregateBytes {
			return ScoreRequest{}, ErrInvalidInput
		}
		aggregateBytes += len(document)
		request.Candidates = append(request.Candidates, ScoringCandidate{
			StableID:     stableID,
			ProviderRank: candidate.ProviderRank,
			Text:         document,
		})
	}
	return request, nil
}

func validateQuery(query string) error {
	if len(query) == 0 || len(query) > MaxQueryRunes*utf8.UTFMax || !utf8.ValidString(query) ||
		containsControl(query) || utf8.RuneCountInString(query) > MaxQueryRunes {
		return ErrInvalidInput
	}
	words := strings.Fields(query)
	if len(words) == 0 || len(words) > MaxQueryWords || strings.Join(words, " ") != query {
		return ErrInvalidInput
	}
	return nil
}

func validateProviderText(value string) error {
	if !utf8.ValidString(value) || containsControl(value) || strings.TrimSpace(value) != value {
		return ErrInvalidInput
	}
	return nil
}

func containsControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}

func utf8Prefix(value string, maximumBytes int) string {
	if maximumBytes <= 0 || value == "" {
		return ""
	}
	if len(value) <= maximumBytes {
		return strings.Clone(value)
	}
	end := maximumBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return strings.Clone(value[:end])
}
