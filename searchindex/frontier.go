package searchindex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	FrontierPending = "pending"
	FrontierLeased  = "leased"
	FrontierDone    = "done"
	FrontierFailed  = "failed"
)

// FrontierItem is one URL in the crawl queue.
type FrontierItem struct {
	URL      string
	Root     string
	State    string
	LeasedAt int64
	Attempts int
}

// Enqueue inserts pending URLs. Existing rows are left untouched.
func (s *Store) Enqueue(ctx context.Context, items []FrontierItem) (int, error) {
	if err := s.guard(ctx); err != nil {
		return 0, err
	}
	if len(items) == 0 {
		return 0, nil
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
		return 0, fmt.Errorf("searchindex: begin enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	inserted := 0
	for _, item := range items {
		item.URL = strings.TrimSpace(item.URL)
		item.Root = strings.TrimSpace(item.Root)
		if item.URL == "" || item.Root == "" {
			continue
		}
		result, execErr := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO frontier(url, root, state) VALUES(?, ?, ?)`,
			item.URL, item.Root, FrontierPending,
		)
		if execErr != nil {
			return 0, fmt.Errorf("searchindex: enqueue: %w", execErr)
		}
		affected, _ := result.RowsAffected()
		inserted += int(affected)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("searchindex: commit enqueue: %w", err)
	}
	return inserted, nil
}

// Lease claims one pending URL whose root is not already leased.
func (s *Store) Lease(ctx context.Context, now time.Time) (FrontierItem, bool, error) {
	if err := s.guard(ctx); err != nil {
		return FrontierItem{}, false, err
	}
	s.gate.RLock()
	defer s.gate.RUnlock()
	if s.closed {
		return FrontierItem{}, false, ErrClosed
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return FrontierItem{}, false, fmt.Errorf("searchindex: begin lease: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var item FrontierItem
	err = tx.QueryRowContext(ctx, `
		SELECT url, root, attempts FROM frontier
		WHERE state = ?
		  AND root NOT IN (SELECT root FROM frontier WHERE state = ?)
		ORDER BY url LIMIT 1`,
		FrontierPending, FrontierLeased,
	).Scan(&item.URL, &item.Root, &item.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		if commitErr := tx.Commit(); commitErr != nil {
			return FrontierItem{}, false, commitErr
		}
		return FrontierItem{}, false, nil
	}
	if err != nil {
		return FrontierItem{}, false, fmt.Errorf("searchindex: pick frontier: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE frontier SET state=?, leased_at=?, attempts=attempts+1 WHERE url=?`,
		FrontierLeased, now.Unix(), item.URL,
	); err != nil {
		return FrontierItem{}, false, fmt.Errorf("searchindex: lease frontier: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return FrontierItem{}, false, err
	}
	item.State = FrontierLeased
	item.LeasedAt = now.Unix()
	item.Attempts++
	return item, true, nil
}

// Complete marks a leased URL done.
func (s *Store) Complete(ctx context.Context, rawURL string) error {
	return s.setFrontierState(ctx, rawURL, FrontierDone)
}

// Fail marks a leased URL failed after too many attempts, otherwise pending.
func (s *Store) Fail(ctx context.Context, rawURL string, maxAttempts int) error {
	if err := s.guard(ctx); err != nil {
		return err
	}
	s.gate.RLock()
	defer s.gate.RUnlock()
	if s.closed {
		return ErrClosed
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	state := FrontierPending
	var attempts int
	if err := s.db.QueryRowContext(ctx, `SELECT attempts FROM frontier WHERE url=?`, rawURL).Scan(&attempts); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("searchindex: read attempts: %w", err)
	}
	if maxAttempts > 0 && attempts >= maxAttempts {
		state = FrontierFailed
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE frontier SET state=?, leased_at=0 WHERE url=?`, state, rawURL); err != nil {
		return fmt.Errorf("searchindex: fail frontier: %w", err)
	}
	return nil
}

func (s *Store) setFrontierState(ctx context.Context, rawURL, state string) error {
	if err := s.guard(ctx); err != nil {
		return err
	}
	s.gate.RLock()
	defer s.gate.RUnlock()
	if s.closed {
		return ErrClosed
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.db.ExecContext(ctx, `UPDATE frontier SET state=?, leased_at=0 WHERE url=?`, state, rawURL); err != nil {
		return fmt.Errorf("searchindex: update frontier: %w", err)
	}
	return nil
}
