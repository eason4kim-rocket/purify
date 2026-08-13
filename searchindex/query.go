package searchindex

import (
	"context"
	"fmt"
	"strings"
	"unicode"
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
	preferred := DetectLanguage(text)
	match := ftsMatch(text)
	preferredHits, err := s.queryTable(ctx, ftsTable(preferred), preferred, match, limit)
	if err != nil {
		return nil, err
	}
	other := LangEnglish
	if preferred == LangEnglish {
		other = LangChinese
	}
	otherHits, err := s.queryTable(ctx, ftsTable(other), other, match, limit)
	if err != nil {
		return nil, err
	}
	return mergeHitsByRank(preferredHits, otherHits, limit), nil
}

func (s *Store) queryTable(ctx context.Context, table, lang, match string, limit int) ([]Hit, error) {
	query := fmt.Sprintf(`
		SELECT p.url, p.title, p.root, p.fetched_at,
		       bm25(%s, 5.0, 1.0) AS score,
		       snippet(%s, 1, '', '', '…', 24) AS snip
		FROM %s JOIN pages p ON p.id = %s.rowid
		WHERE %s MATCH ? ORDER BY score LIMIT ?`, table, table, table, table, table)
	rows, err := s.db.QueryContext(ctx, query, match, limit)
	if err != nil {
		return nil, fmt.Errorf("searchindex: query %s: %w", table, err)
	}
	defer rows.Close()
	hits := make([]Hit, 0, limit)
	for rows.Next() {
		var hit Hit
		if err := rows.Scan(&hit.URL, &hit.Title, &hit.Root, &hit.FetchedAt, &hit.Score, &hit.Snippet); err != nil {
			return nil, fmt.Errorf("searchindex: scan hit: %w", err)
		}
		hit.Lang = lang
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

func ftsMatch(text string) string {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r)
	})
	if len(fields) == 0 {
		return `""`
	}
	quoted := make([]string, 0, len(fields))
	for _, field := range fields {
		field = strings.ReplaceAll(field, `"`, "")
		if field == "" {
			continue
		}
		quoted = append(quoted, `"`+field+`"`)
	}
	if len(quoted) == 0 {
		return `""`
	}
	return strings.Join(quoted, " ")
}
