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

func upsertPageTx(ctx context.Context, tx *sql.Tx, page Page) (bool, error) {
	var existingID int64
	var existingHash, existingLang, existingTitle, existingBody string
	err := tx.QueryRowContext(ctx, `SELECT id, content_hash, lang, title, body FROM pages WHERE url = ?`, page.URL).
		Scan(&existingID, &existingHash, &existingLang, &existingTitle, &existingBody)
	switch {
	case err == nil && existingHash == page.ContentHash:
		return false, nil
	case err == nil:
		if err := deleteFTS(ctx, tx, existingID, existingLang, existingTitle, existingBody); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE pages SET root=?, title=?, body=?, lang=?, fetched_at=?, content_hash=?, etag=?, last_mod=?, needs_render=?
			WHERE id=?`,
			page.Root, page.Title, page.Body, page.Lang, page.FetchedAt.Unix(),
			page.ContentHash, page.ETag, page.LastMod, boolToInt(page.NeedsRender), existingID,
		); err != nil {
			return false, fmt.Errorf("searchindex: update page: %w", err)
		}
		return true, insertFTS(ctx, tx, existingID, page)
	case errors.Is(err, sql.ErrNoRows):
		result, execErr := tx.ExecContext(ctx, `
			INSERT INTO pages(url, root, title, body, lang, fetched_at, content_hash, etag, last_mod, needs_render)
			VALUES(?,?,?,?,?,?,?,?,?,?)`,
			page.URL, page.Root, page.Title, page.Body, page.Lang, page.FetchedAt.Unix(),
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
