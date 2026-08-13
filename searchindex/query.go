package searchindex

import (
	"context"
	"fmt"
	"strings"
	"unicode"
)

const (
	// snippetLeadRunes is the context kept before the first term hit;
	// snippetWindowRunes is the fragment finally returned.
	snippetLeadRunes   = 20
	snippetWindowRunes = 120
)

// Hit is one ranked page from a single FTS table.
type Hit struct {
	URL       string
	Title     string
	Root      string
	FetchedAt int64
	Score     float64
	Snippet   string
	Lang      string
}

// Query searches both language indexes, takes the top limit from each, and
// merges by rank position. BM25 scores are never compared across tokenizers.
// The query language is preferred at each rank.
func (s *Store) Query(ctx context.Context, text string, limit int) ([]Hit, error) {
	if err := s.guard(ctx); err != nil {
		return nil, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	if limit < 1 {
		limit = 1
	}
	terms := queryTerms(text)
	if len(terms) == 0 {
		return nil, nil
	}
	preferred := DetectLanguage(text)
	other := LangEnglish
	if preferred == LangEnglish {
		other = LangChinese
	}
	// Strictness is chosen globally, not per table: the strictest tier that
	// yields any hit is applied to both languages. A per-table fallback would
	// interleave weak single-term matches from the other language in between
	// strong full matches from the preferred one.
	for _, tier := range []matchTier{tierNear, tierAnd, tierOr} {
		preferredHits, err := s.queryTable(ctx, preferred, terms, limit, tier)
		if err != nil {
			return nil, err
		}
		otherHits, err := s.queryTable(ctx, other, terms, limit, tier)
		if err != nil {
			return nil, err
		}
		if len(preferredHits)+len(otherHits) > 0 {
			return mergeHitsByRank(preferredHits, otherHits, limit), nil
		}
	}
	return nil, nil
}

// matchTier is one strictness level of the three-tier query plan.
type matchTier int

const (
	// tierNear requires every term within a small token window. Hub pages —
	// pagination indexes, link directories, sidebar soups — contain almost
	// every term somewhere, but only genuine content pages hold them close
	// together, so this tier keeps hubs out of the head of the ranking.
	tierNear matchTier = iota
	tierAnd
	tierOr
)

// nearWindow is the token distance allowed between query terms in tierNear.
const nearWindow = 20

func (s *Store) queryTable(ctx context.Context, lang string, terms []string, limit int, tier matchTier) ([]Hit, error) {
	match, err := ftsMatch(lang, terms, tier)
	if err != nil {
		return nil, err
	}
	if match == "" {
		return nil, nil
	}
	table := ftsTable(lang)
	// The FTS tables are contentless, so snippet() has no text to work from.
	// Fragments come from the stored plain lead instead, which also keeps the
	// compressed body off the query path entirely.
	query := fmt.Sprintf(`
		SELECT p.url, p.title, p.root, p.fetched_at, p.lead,
		       bm25(%s, 5.0, 1.0) AS score
		FROM %s JOIN pages p ON p.id = %s.rowid
		WHERE %s MATCH ? ORDER BY score LIMIT ?`, table, table, table, table)
	rows, err := s.db.QueryContext(ctx, query, match, limit)
	if err != nil {
		return nil, fmt.Errorf("searchindex: query %s: %w", table, err)
	}
	defer rows.Close()
	hits := make([]Hit, 0, limit)
	for rows.Next() {
		var hit Hit
		var lead string
		if err := rows.Scan(&hit.URL, &hit.Title, &hit.Root, &hit.FetchedAt, &lead, &hit.Score); err != nil {
			return nil, fmt.Errorf("searchindex: scan hit: %w", err)
		}
		hit.Lang = lang
		hit.Snippet = plainSnippet(lead, terms)
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}

func mergeHitsByRank(preferred, other []Hit, limit int) []Hit {
	merged := make([]Hit, 0, limit)
	seen := make(map[string]struct{}, limit)
	add := func(hit Hit) bool {
		if _, exists := seen[hit.URL]; exists {
			return len(merged) >= limit
		}
		seen[hit.URL] = struct{}{}
		merged = append(merged, hit)
		return len(merged) >= limit
	}
	maxRank := len(preferred)
	if len(other) > maxRank {
		maxRank = len(other)
	}
	for rank := 0; rank < maxRank; rank++ {
		if rank < len(preferred) && add(preferred[rank]) {
			return merged
		}
		if rank < len(other) && add(other[rank]) {
			return merged
		}
	}
	return merged
}

// queryTerms splits raw user text into candidate terms. Every FTS5 operator
// character is either a separator or ends up inside a quoted phrase, so user
// text can never reach the parser as syntax.
func queryTerms(text string) []string {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r)
	})
	terms := make([]string, 0, len(fields))
	for _, field := range fields {
		field = strings.ReplaceAll(field, `"`, "")
		if field == "" {
			continue
		}
		terms = append(terms, field)
	}
	return terms
}

// ftsMatch renders terms for one tier. Query walks NEAR, then AND, then OR:
// pure OR let a single common term ("policy", "变量") pull unrelated hub pages
// into the head of the ranking, pure AND still admitted link-directory pages
// that mention every term somewhere, and NEAR alone would return nothing for
// broad queries whose terms never sit in one passage.
func ftsMatch(lang string, terms []string, tier matchTier) (string, error) {
	phrases := make([]string, 0, len(terms))
	seen := make(map[string]struct{}, len(terms))
	for _, term := range terms {
		// A Chinese term is segmented the same way the index was, so each word
		// becomes its own OR operand instead of one long phrase and a page does
		// not have to contain the whole run verbatim.
		segmented, err := queryText(lang, term)
		if err != nil {
			return "", err
		}
		for _, token := range strings.Fields(segmented) {
			token = strings.ReplaceAll(token, `"`, "")
			if token == "" {
				continue
			}
			if _, duplicate := seen[token]; duplicate {
				continue
			}
			seen[token] = struct{}{}
			phrases = append(phrases, `"`+token+`"`)
		}
	}
	switch {
	case len(phrases) == 0:
		return "", nil
	case tier == tierOr:
		return strings.Join(phrases, " OR "), nil
	case tier == tierNear && len(phrases) > 1:
		return fmt.Sprintf("NEAR(%s, %d)", strings.Join(phrases, " "), nearWindow), nil
	default:
		// A single term makes NEAR meaningless, so it degrades to AND here
		// and Query's AND pass then finds nothing new to add.
		return strings.Join(phrases, " AND "), nil
	}
}

// plainSnippet cuts a window around the first term hit in the stored body.
func plainSnippet(body string, terms []string) string {
	body = strings.Join(strings.Fields(body), " ")
	if body == "" {
		return ""
	}
	runes := []rune(body)
	start := 0
	for _, term := range terms {
		if at := strings.Index(body, term); at >= 0 {
			start = len([]rune(body[:at]))
			break
		}
	}
	start -= snippetLeadRunes
	if start < 0 {
		start = 0
	}
	end := start + snippetWindowRunes
	if end > len(runes) {
		end = len(runes)
	}
	fragment := string(runes[start:end])
	if end < len(runes) {
		fragment += "…"
	}
	if start > 0 {
		fragment = "…" + fragment
	}
	return fragment
}
