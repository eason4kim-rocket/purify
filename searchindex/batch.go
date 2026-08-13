package searchindex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// UpsertMany writes pages in one transaction so the indexer does not commit
// every fetch. Duplicate content hashes for the same URL remain no-ops.
func (s *Store) UpsertMany(ctx context.Context, pages []Page) (int, error) {
	if err := s.guard(ctx); err != nil {
		return 0, err
	}
	if len(pages) == 0 {
		return 0, nil
	}
	normalized := make([]Page, 0, len(pages))
	for _, page := range pages {
		item, err := normalizePage(page)
		if err != nil {
			return 0, err
		}
		normalized = append(normalized, item)
	}

	s.gate.RLock()
	defer s.gate.RUnlock()
	if s.closed {
		return 0, ErrClosed
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("searchindex: begin batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	written := 0
	for _, page := range normalized {
		changed, err := upsertPageTx(ctx, tx, page)
		if err != nil {
			return 0, err
		}
		if changed {
			written++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("searchindex: commit batch: %w", err)
	}
	return written, nil
}

// Reindex rebuilds both FTS tables from the stored compressed bodies. This is
// what the compressed copy is for: a tokenizer or rewrite change costs one
// local pass instead of re-crawling every page.
func (s *Store) Reindex(ctx context.Context) (int, error) {
	if err := s.guard(ctx); err != nil {
		return 0, err
	}
	s.gate.RLock()
	defer s.gate.RUnlock()
	if s.closed {
		return 0, ErrClosed
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("searchindex: begin reindex: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, table := range []string{"pages_fts_en", "pages_fts_zh"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table); err != nil {
			return 0, fmt.Errorf("searchindex: clear %s: %w", table, err)
		}
	}

	rows, err := tx.QueryContext(ctx, `SELECT id, title, body_z, lang FROM pages ORDER BY id`)
	if err != nil {
		return 0, fmt.Errorf("searchindex: scan pages: %w", err)
	}
	type pending struct {
		id   int64
		page Page
	}
	batch := make([]pending, 0, 256)
	for rows.Next() {
		var id int64
		var title, lang string
		var blob []byte
		if err := rows.Scan(&id, &title, &blob, &lang); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("searchindex: scan page: %w", err)
		}
		body, decodeErr := decompressBody(blob)
		if decodeErr != nil {
			_ = rows.Close()
			return 0, decodeErr
		}
		batch = append(batch, pending{id: id, page: Page{Title: title, Body: body, Lang: lang}})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("searchindex: read pages: %w", err)
	}
	_ = rows.Close()

	for _, item := range batch {
		if err := insertFTS(ctx, tx, item.id, item.page); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("searchindex: commit reindex: %w", err)
	}
	return len(batch), nil
}

func upsertPageTx(ctx context.Context, tx *sql.Tx, page Page) (bool, error) {
	lead := buildLead(page.Title, page.Body)
	compressed, compressErr := compressBody(page.Body)
	if compressErr != nil {
		return false, compressErr
	}

	var existingID int64
	var existingHash, existingLang string
	err := tx.QueryRowContext(ctx, `SELECT id, content_hash, lang FROM pages WHERE url = ?`, page.URL).
		Scan(&existingID, &existingHash, &existingLang)
	switch {
	case err == nil && existingHash == page.ContentHash:
		return false, nil
	case err == nil:
		// The language may have flipped between revisions, so the retraction
		// has to target the table the previous revision was indexed into.
		if err := deleteFTS(ctx, tx, existingID, existingLang); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE pages SET root=?, title=?, lead=?, body_z=?, lang=?, fetched_at=?, content_hash=?, etag=?, last_mod=?, needs_render=?
			WHERE id=?`,
			page.Root, page.Title, lead, compressed, page.Lang, page.FetchedAt.Unix(),
			page.ContentHash, page.ETag, page.LastMod, boolToInt(page.NeedsRender), existingID,
		); err != nil {
			return false, fmt.Errorf("searchindex: update page: %w", err)
		}
		return true, insertFTS(ctx, tx, existingID, page)
	case errors.Is(err, sql.ErrNoRows):
		result, execErr := tx.ExecContext(ctx, `
			INSERT INTO pages(url, root, title, lead, body_z, lang, fetched_at, content_hash, etag, last_mod, needs_render)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			page.URL, page.Root, page.Title, lead, compressed, page.Lang, page.FetchedAt.Unix(),
			page.ContentHash, page.ETag, page.LastMod, boolToInt(page.NeedsRender),
		)
		if execErr != nil {
			return false, fmt.Errorf("searchindex: insert page: %w", execErr)
		}
		id, idErr := result.LastInsertId()
		if idErr != nil {
			return false, fmt.Errorf("searchindex: page id: %w", idErr)
		}
		return true, insertFTS(ctx, tx, id, page)
	default:
		return false, fmt.Errorf("searchindex: lookup page: %w", err)
	}
}
