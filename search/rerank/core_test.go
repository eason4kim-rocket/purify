package rerank

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCandidateIDGoldenAndCanonicalByteIdentity(t *testing.T) {
	got, err := CandidateID("https://example.com/")
	if err != nil {
		t.Fatalf("CandidateID() error = %v", err)
	}
	if want := "e7269fe34487e7b178e00ecc0042034c78b3be66c571bcd0e464b9fe1df061f7"; got != want {
		t.Fatalf("CandidateID() = %q, want %q", got, want)
	}
	changed, err := CandidateID("https://example.com/?source=one")
	if err != nil {
		t.Fatalf("changed CandidateID() error = %v", err)
	}
	if changed == got {
		t.Fatal("different canonical URL bytes produced the same stable ID")
	}
	request, err := BuildRequest("query", []Candidate{{
		CanonicalURL: "https://example.com/",
		ProviderRank: 7,
		Title:        "metadata does not enter the digest",
		Snippet:      "nor does this text",
	}})
	if err != nil {
		t.Fatalf("BuildRequest() error = %v", err)
	}
	if request.Candidates[0].StableID != got {
		t.Fatalf("metadata changed stable ID: got %q want %q", request.Candidates[0].StableID, got)
	}
	maximumURL := "https://example.com/" + strings.Repeat("a", MaxCanonicalURLBytes-len("https://example.com/"))
	if _, err := CandidateID(maximumURL); err != nil {
		t.Fatalf("CandidateID(maximum URL) error = %v", err)
	}
	if _, err := CandidateID(maximumURL + "a"); !errorsIs(err, ErrInvalidInput) {
		t.Fatalf("CandidateID(overlong URL) error = %v, want ErrInvalidInput", err)
	}

	invalid := []string{
		"https://EXAMPLE.com/",
		"https://example.com",
		"https://user@example.com/",
		"https://example.com/#fragment",
		"/relative",
		" http://example.com/",
	}
	for _, raw := range invalid {
		if _, err := CandidateID(raw); !errorsIs(err, ErrInvalidInput) {
			t.Fatalf("CandidateID(%q) error = %v, want ErrInvalidInput", raw, err)
		}
	}
}

func TestBuildRequestStableIDAndDocument(t *testing.T) {
	candidates := []Candidate{{
		CanonicalURL: "https://example.com/a",
		ProviderRank: 3,
		Title:        "Exact Title",
		Snippet:      "Exact snippet.",
	}}
	original := append([]Candidate(nil), candidates...)

	request, err := BuildRequest("exact query", candidates)
	if err != nil {
		t.Fatalf("BuildRequest() error = %v", err)
	}
	if request.Query != "exact query" || len(request.Candidates) != 1 {
		t.Fatalf("request = %#v", request)
	}
	got := request.Candidates[0]
	if got.StableID != "6b004532e4087a8153ed525156075395a0bb333651b38a2b1a268d0878012d20" {
		t.Fatalf("stable ID = %q", got.StableID)
	}
	if got.ProviderRank != 3 || got.Text != "Exact Title\nExact snippet." {
		t.Fatalf("candidate = %#v", got)
	}
	if !reflect.DeepEqual(candidates, original) {
		t.Fatalf("BuildRequest mutated candidates: got %#v want %#v", candidates, original)
	}
}

func TestBuildRequestUTF8BoundariesAndBudgets(t *testing.T) {
	t.Run("query boundaries and cjk are preserved", func(t *testing.T) {
		queries := []string{
			strings.TrimSpace(strings.Repeat("word ", MaxQueryWords)),
			strings.Repeat("界", MaxQueryRunes),
		}
		for _, query := range queries {
			request, err := BuildRequest(query, []Candidate{{
				CanonicalURL: "https://example.com/cjk",
				ProviderRank: 1,
				Title:        "标题保留",
				Snippet:      "正文也完整保留。",
			}})
			if err != nil {
				t.Fatalf("BuildRequest(boundary query) error = %v", err)
			}
			if request.Query != query || request.Candidates[0].Text != "标题保留\n正文也完整保留。" {
				t.Fatalf("boundary request changed content: %#v", request)
			}
		}
	})

	t.Run("rune safe title and snippet cuts", func(t *testing.T) {
		title := strings.Repeat("t", MaxTitleBytes-1) + "界"
		snippetBudget := MaxDocumentBytes - 1 - (MaxTitleBytes - 1)
		snippet := strings.Repeat("s", snippetBudget-1) + "🙂tail"
		request, err := BuildRequest("query", []Candidate{{
			CanonicalURL: "https://example.com/utf8",
			ProviderRank: 1,
			Title:        title,
			Snippet:      snippet,
		}})
		if err != nil {
			t.Fatalf("BuildRequest() error = %v", err)
		}
		document := request.Candidates[0].Text
		if !utf8.ValidString(document) {
			t.Fatalf("document is not valid UTF-8: %q", document)
		}
		if len(document) != MaxDocumentBytes-1 {
			t.Fatalf("document bytes = %d, want %d", len(document), MaxDocumentBytes-1)
		}
		if strings.Contains(document, "界") || strings.Contains(document, "🙂") {
			t.Fatalf("document retained a rune crossing a byte boundary: %q", document)
		}
		if strings.Count(document, "\n") != 1 {
			t.Fatalf("document separator count = %d", strings.Count(document, "\n"))
		}
	})

	t.Run("empty metadata keeps separator", func(t *testing.T) {
		request, err := BuildRequest("query", []Candidate{{CanonicalURL: "https://example.com/empty", ProviderRank: 1}})
		if err != nil {
			t.Fatalf("BuildRequest() error = %v", err)
		}
		if got := request.Candidates[0].Text; got != "\n" {
			t.Fatalf("document = %q, want newline", got)
		}
	})

	t.Run("twenty full documents", func(t *testing.T) {
		candidates := make([]Candidate, MaxCandidates)
		for index := range candidates {
			candidates[index] = Candidate{
				CanonicalURL: "https://example.com/" + string(rune('a'+index)),
				ProviderRank: index + 1,
				Title:        strings.Repeat("t", MaxTitleBytes),
				Snippet:      strings.Repeat("s", MaxDocumentBytes-1-MaxTitleBytes),
			}
		}
		request, err := BuildRequest("query", candidates)
		if err != nil {
			t.Fatalf("BuildRequest() error = %v", err)
		}
		total := 0
		for _, candidate := range request.Candidates {
			if len(candidate.Text) != MaxDocumentBytes {
				t.Fatalf("document bytes = %d, want %d", len(candidate.Text), MaxDocumentBytes)
			}
			total += len(candidate.Text)
		}
		if total != MaxAggregateDocumentBytes {
			t.Fatalf("aggregate bytes = %d, want %d", total, MaxAggregateDocumentBytes)
		}
	})

	t.Run("raw metadata bounds are admitted before truncation", func(t *testing.T) {
		request, err := BuildRequest("query", []Candidate{{
			CanonicalURL: "https://example.com/raw-bounds",
			ProviderRank: 1,
			Title:        strings.Repeat("t", MaxRawTitleBytes),
			Snippet:      strings.Repeat("s", MaxRawSnippetBytes),
		}})
		if err != nil {
			t.Fatalf("BuildRequest() error = %v", err)
		}
		if got := len(request.Candidates[0].Text); got != MaxDocumentBytes {
			t.Fatalf("document bytes = %d, want %d", got, MaxDocumentBytes)
		}
	})
}

func TestBuildRequestRejectsInvalidInputBeforeScoring(t *testing.T) {
	valid := Candidate{CanonicalURL: "https://example.com/a", ProviderRank: 1, Title: "title", Snippet: "snippet"}
	tests := []struct {
		name       string
		query      string
		candidates []Candidate
	}{
		{name: "empty query", candidates: []Candidate{valid}},
		{name: "invalid query utf8", query: string([]byte{0xff}), candidates: []Candidate{valid}},
		{name: "leading query whitespace", query: " query", candidates: []Candidate{valid}},
		{name: "trailing query whitespace", query: "query ", candidates: []Candidate{valid}},
		{name: "repeated query whitespace", query: "two  words", candidates: []Candidate{valid}},
		{name: "too many query words", query: strings.TrimSpace(strings.Repeat("word ", MaxQueryWords+1)), candidates: []Candidate{valid}},
		{name: "too many query runes", query: strings.Repeat("界", MaxQueryRunes+1), candidates: []Candidate{valid}},
		{name: "query control", query: "bad\nquery", candidates: []Candidate{valid}},
		{name: "too many candidates", query: "query", candidates: make([]Candidate, MaxCandidates+1)},
		{name: "empty URL", query: "query", candidates: []Candidate{{ProviderRank: 1}}},
		{name: "duplicate URL", query: "query", candidates: []Candidate{valid, valid}},
		{name: "rank zero", query: "query", candidates: []Candidate{{CanonicalURL: "https://example.com/a"}}},
		{name: "rank over maximum", query: "query", candidates: []Candidate{{CanonicalURL: "https://example.com/a", ProviderRank: MaxCandidates + 1}}},
		{name: "title over raw bound", query: "query", candidates: []Candidate{{CanonicalURL: "https://example.com/a", ProviderRank: 1, Title: strings.Repeat("t", MaxRawTitleBytes+1)}}},
		{name: "snippet over raw bound", query: "query", candidates: []Candidate{{CanonicalURL: "https://example.com/a", ProviderRank: 1, Snippet: strings.Repeat("s", MaxRawSnippetBytes+1)}}},
		{name: "invalid title utf8", query: "query", candidates: []Candidate{{CanonicalURL: "https://example.com/a", ProviderRank: 1, Title: string([]byte{0xff})}}},
		{name: "invalid snippet utf8", query: "query", candidates: []Candidate{{CanonicalURL: "https://example.com/a", ProviderRank: 1, Snippet: string([]byte{0xff})}}},
		{name: "invalid utf8 after title crop", query: "query", candidates: []Candidate{{CanonicalURL: "https://example.com/a", ProviderRank: 1, Title: strings.Repeat("t", MaxTitleBytes) + string([]byte{0xff})}}},
		{name: "invalid utf8 after snippet crop", query: "query", candidates: []Candidate{{CanonicalURL: "https://example.com/a", ProviderRank: 1, Snippet: strings.Repeat("s", MaxDocumentBytes) + string([]byte{0xff})}}},
		{name: "candidate control", query: "query", candidates: []Candidate{{CanonicalURL: "https://example.com/a", ProviderRank: 1, Snippet: "bad\ntext"}}},
		{name: "control after title crop", query: "query", candidates: []Candidate{{CanonicalURL: "https://example.com/a", ProviderRank: 1, Title: strings.Repeat("t", MaxTitleBytes) + "\n"}}},
		{name: "control after snippet crop", query: "query", candidates: []Candidate{{CanonicalURL: "https://example.com/a", ProviderRank: 1, Snippet: strings.Repeat("s", MaxDocumentBytes) + "\x00"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := BuildRequest(test.query, test.candidates); !errorsIs(err, ErrInvalidInput) {
				t.Fatalf("BuildRequest() error = %v, want ErrInvalidInput", err)
			}
		})
	}
}
