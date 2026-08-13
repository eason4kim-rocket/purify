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
	return s.enqueue(ctx, items, 0)
}

// EnqueueBounded inserts pending URLs while the root's total frontier
// footprint — pending, leased, done, and failed rows alike — stays below
// perRootBudget. Done rows count because they are spent crawl budget; a root
// that consumed its budget must not win more by linking to itself. A budget
// of zero or less means unbounded.
func (s *Store) EnqueueBounded(ctx context.Context, items []FrontierItem, perRootBudget int) (int, error) {
	return s.enqueue(ctx, items, perRootBudget)
}

func (s *Store) enqueue(ctx context.Context, items []FrontierItem, perRootBudget int) (int, error) {
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
		var result sql.Result
		var execErr error
		if perRootBudget > 0 {
			// The guarded INSERT re-counts inside the transaction, so budget
			// checks see rows added earlier in this same batch. A duplicate URL
			// passes the WHERE and lands on the conflict clause, leaving the
			// count — and therefore the budget — untouched.
			result, execErr = tx.ExecContext(ctx, `
				INSERT INTO frontier(url, root, state)
				SELECT ?1, ?2, ?3
				WHERE (SELECT COUNT(*) FROM frontier WHERE root = ?2) < ?4
				ON CONFLICT(url) DO NOTHING`,
				item.URL, item.Root, FrontierPending, perRootBudget,
			)
		} else {
			result, execErr = tx.ExecContext(ctx, `
				INSERT OR IGNORE INTO frontier(url, root, state) VALUES(?, ?, ?)`,
				item.URL, item.Root, FrontierPending,
			)
		}
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

// Requeue returns the named done or failed rows to pending so they get
// fetched again. On a frontier drained by earlier runs every row is closed,
// no page is ever refetched, and in-crawl link discovery has nothing to grow
// from; requeuing the seed pages restarts the cascade. Pending and leased
// rows, and URLs with no row at all, are left untouched.
func (s *Store) Requeue(ctx context.Context, urls []string) (int, error) {
	if err := s.guard(ctx); err != nil {
		return 0, err
	}
	if len(urls) == 0 {
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
		return 0, fmt.Errorf("searchindex: begin requeue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	reopened := 0
	for _, rawURL := range urls {
		rawURL = strings.TrimSpace(rawURL)
		if rawURL == "" {
			continue
		}
		result, execErr := tx.ExecContext(ctx, `
			UPDATE frontier SET state=?, leased_at=0, attempts=0
			WHERE url=? AND state IN (?, ?)`,
			FrontierPending, rawURL, FrontierDone, FrontierFailed,
		)
		if execErr != nil {
			return 0, fmt.Errorf("searchindex: requeue: %w", execErr)
		}
		affected, _ := result.RowsAffected()
		reopened += int(affected)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("searchindex: commit requeue: %w", err)
	}
	return reopened, nil
}

// RequeueStarvedRoots reopens the done and failed rows of every root holding
// fewer than maxRows frontier rows. Indexes built before in-crawl link
// discovery never mined their fetched pages, and once such a root's rows all
// close it can never grow again; a small row count marks exactly those
// starved roots, while sitemap-fed roots with thousands of rows stay closed
// instead of being refetched wholesale. A maxRows of zero or less reopens
// nothing.
func (s *Store) RequeueStarvedRoots(ctx context.Context, maxRows int) (int, error) {
	if err := s.guard(ctx); err != nil {
		return 0, err
	}
	if maxRows <= 0 {
		return 0, nil
	}
	s.gate.RLock()
	defer s.gate.RUnlock()
	if s.closed {
		return 0, ErrClosed
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	result, err := s.db.ExecContext(ctx, `
		UPDATE frontier SET state=?, leased_at=0, attempts=0
		WHERE state IN (?, ?)
		  AND root IN (SELECT root FROM frontier GROUP BY root HAVING COUNT(*) < ?)`,
		FrontierPending, FrontierDone, FrontierFailed, maxRows,
	)
	if err != nil {
		return 0, fmt.Errorf("searchindex: requeue starved roots: %w", err)
	}
	reopened, _ := result.RowsAffected()
	return int(reopened), nil
}

// ActiveFrontier counts pending and leased rows. Link discovery lets an
// in-flight page refill an empty frontier, so a crawler may only stop when
// this count reaches zero.
func (s *Store) ActiveFrontier(ctx context.Context) (int, error) {
	if err := s.guard(ctx); err != nil {
		return 0, err
	}
	s.gate.RLock()
	defer s.gate.RUnlock()
	if s.closed {
		return 0, ErrClosed
	}
	var active int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM frontier WHERE state IN (?, ?)`,
		FrontierPending, FrontierLeased,
	).Scan(&active); err != nil {
		return 0, fmt.Errorf("searchindex: count active frontier: %w", err)
	}
	return active, nil
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

	// Roots are picked uniformly, not by global URL order. Link discovery
	// keeps refilling the queue, and under one global ordering the crawl
	// wedges into whatever sorts first (every http:// URL precedes every
	// https:// one) while later roots starve; a fair root pick spreads the
	// politeness-limited workers across sites. Within a root, URL order
	// keeps section locality.
	var item FrontierItem
	err = tx.QueryRowContext(ctx, `
		SELECT url, root, attempts FROM frontier
		WHERE state = ?
		  AND root = (
			SELECT root FROM frontier
			WHERE state = ?
			  AND root NOT IN (SELECT root FROM frontier WHERE state = ?)
			GROUP BY root ORDER BY RANDOM() LIMIT 1)
		ORDER BY url LIMIT 1`,
		FrontierPending, FrontierPending, FrontierLeased,
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

// ReleaseStaleLeases returns every leased row to pending. Leases carry no
// expiry, so rows left behind by a killed crawler block their root forever —
// and when the remaining pending URLs cluster on a few slow hosts, two stale
// rows are enough to make a full frontier look drained. One crawler owns a
// database at a time, so at startup every surviving lease is stale.
func (s *Store) ReleaseStaleLeases(ctx context.Context) (int, error) {
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
	result, err := s.db.ExecContext(ctx, `
		UPDATE frontier SET state=?, leased_at=0 WHERE state=?`,
		FrontierPending, FrontierLeased,
	)
	if err != nil {
		return 0, fmt.Errorf("searchindex: release stale leases: %w", err)
	}
	released, _ := result.RowsAffected()
	return int(released), nil
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
