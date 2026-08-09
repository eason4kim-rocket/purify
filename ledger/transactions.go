package ledger

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"time"
)

// ReadTx is the deliberately narrow read surface exposed to ledger clients.
// The underlying database and transaction are never exposed, so callers
// cannot commit, roll back, or retain a pooled connection.
type ReadTx interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// WriteTx extends ReadTx with transactional writes. Transactions are owned by
// Store.Update; callers cannot commit or roll them back themselves.
type WriteTx interface {
	ReadTx
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

type readTx struct {
	tx *sql.Tx
}

func (tx readTx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return tx.tx.QueryContext(ctx, query, args...)
}

func (tx readTx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return tx.tx.QueryRowContext(ctx, query, args...)
}

type writeTx struct {
	readTx
}

func (tx writeTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return tx.tx.ExecContext(ctx, query, args...)
}

// View runs view inside a read-only transaction. The callback must not retain
// tx or rows after it returns. A callback error or panic rolls the transaction
// back; a panic is propagated after rollback and lock release.
func (s *Store) View(ctx context.Context, view func(ReadTx) error) (result error) {
	if s == nil || view == nil {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	s.gate.RLock()
	defer s.gate.RUnlock()
	if s.closed {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("ledger: acquire read connection: %w", err)
	}
	readOnly := false
	defer func() {
		if readOnly {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			_, resetErr := conn.ExecContext(cleanupCtx, "PRAGMA query_only = OFF")
			cancel()
			if resetErr != nil {
				// A connection whose query_only flag cannot be restored must not
				// return to the pool and unexpectedly reject a future writer.
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
				if result == nil {
					result = fmt.Errorf("ledger: restore read connection: %w", resetErr)
				}
			}
		}
		_ = conn.Close()
	}()
	if _, err := conn.ExecContext(ctx, "PRAGMA query_only = ON"); err != nil {
		return fmt.Errorf("ledger: configure read-only transaction: %w", err)
	}
	readOnly = true

	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("ledger: begin read transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := view(readTx{tx: tx}); err != nil {
		return fmt.Errorf("ledger: read transaction callback: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ledger: commit read transaction: %w", err)
	}
	return nil
}

// Update serializes a callback with every other process-local ledger writer
// and commits all of its statements atomically. The callback must not retain
// tx or rows after it returns. A callback error or panic rolls the transaction
// back; a panic is propagated after rollback and lock release.
func (s *Store) Update(ctx context.Context, update func(WriteTx) error) error {
	if s == nil || update == nil {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	s.gate.RLock()
	defer s.gate.RUnlock()
	if s.closed {
		return ErrClosed
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ledger: begin write transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := update(writeTx{readTx{tx: tx}}); err != nil {
		return fmt.Errorf("ledger: write transaction callback: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ledger: commit write transaction: %w", err)
	}
	return nil
}
