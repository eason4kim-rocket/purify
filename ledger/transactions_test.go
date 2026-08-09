package ledger

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestViewAndUpdateTransactions(t *testing.T) {
	store := openTestStore(t)
	want := errors.New("stop")

	if err := store.Update(context.Background(), func(tx WriteTx) error {
		if _, err := tx.ExecContext(context.Background(), "INSERT INTO extractor_page_bindings (page_hash, schema_hash, extractor_id, bound_at, last_seen_at) VALUES (?, ?, ?, ?, ?)",
			repeatHex("a"), repeatHex("b"), "00000000-0000-4000-8000-000000000099", "2026-08-09T00:00:00.000000000Z", "2026-08-09T00:00:00.000000000Z"); err == nil {
			t.Fatal("foreign-key violating insert succeeded")
		}
		return want
	}); !errors.Is(err, want) {
		t.Fatalf("Update() error = %v, want callback error", err)
	}

	if err := store.View(context.Background(), func(tx ReadTx) error {
		var count int
		if err := tx.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM extractors").Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatalf("extractor count = %d, want 0", count)
		}
		return nil
	}); err != nil {
		t.Fatalf("View() error = %v", err)
	}
}

func TestUpdateRollbackOnErrorAndPanic(t *testing.T) {
	store := openTestStore(t)
	templateHash := make([]byte, 8)
	templateHash[7] = 1
	insert := func(tx WriteTx, id string) error {
		_, err := tx.ExecContext(context.Background(), `INSERT INTO extractors (
			id, host, schema_json, schema_hash, template_cluster_id,
			template_simhash, ir, ir_hash, ir_format_version, version,
			validation_report, validation, state, created_at, updated_at
		) VALUES (?, 'example.com', '{}', ?, ?, ?, '{}', ?, 1, 1,
			'{"can_enable":true}', 1.0, 'active', ?, ?)`, id, repeatHex("a"), repeatHex("c"), templateHash, repeatHex("b"),
			"2026-08-09T00:00:00.000000000Z", "2026-08-09T00:00:00.000000000Z")
		return err
	}

	want := errors.New("rollback")
	if err := store.Update(context.Background(), func(tx WriteTx) error {
		if err := insert(tx, "00000000-0000-4000-8000-000000000001"); err != nil {
			return err
		}
		return want
	}); !errors.Is(err, want) {
		t.Fatalf("Update(error) = %v, want callback error", err)
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("Update panic was not propagated")
			}
		}()
		_ = store.Update(context.Background(), func(tx WriteTx) error {
			if err := insert(tx, "00000000-0000-4000-8000-000000000002"); err != nil {
				return err
			}
			panic("boom")
		})
	}()

	if err := store.Update(context.Background(), func(tx WriteTx) error {
		var count int
		if err := tx.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM extractors").Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatalf("extractor count after rollback = %d, want 0", count)
		}
		return nil
	}); err != nil {
		t.Fatalf("Update() after panic error = %v", err)
	}
}

func TestUpdateSerializesWriters(t *testing.T) {
	store := openTestStore(t)
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{})
	errorsByWriter := make(chan error, 2)

	go func() {
		errorsByWriter <- store.Update(context.Background(), func(WriteTx) error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	<-firstEntered
	go func() {
		errorsByWriter <- store.Update(context.Background(), func(WriteTx) error {
			close(secondEntered)
			return nil
		})
	}()

	select {
	case <-secondEntered:
		t.Fatal("second writer entered while first writer was active")
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseFirst)
	select {
	case <-secondEntered:
	case <-time.After(time.Second):
		t.Fatal("second writer did not enter after first writer completed")
	}
	for index := 0; index < 2; index++ {
		if err := <-errorsByWriter; err != nil {
			t.Fatalf("writer error = %v", err)
		}
	}
}

func TestTransactionsHonorContextAndClose(t *testing.T) {
	store := openTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	if err := store.View(ctx, func(ReadTx) error { called = true; return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("View(canceled) error = %v", err)
	}
	if err := store.Update(ctx, func(WriteTx) error { called = true; return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("Update(canceled) error = %v", err)
	}
	if called {
		t.Fatal("callback ran for canceled context")
	}

	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := store.View(context.Background(), func(ReadTx) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("View(closed) error = %v", err)
	}
	if err := store.Update(context.Background(), func(WriteTx) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("Update(closed) error = %v", err)
	}
}

func TestViewUsesReadOnlyTransaction(t *testing.T) {
	store := openTestStore(t)
	err := store.View(context.Background(), func(tx ReadTx) error {
		rows, err := tx.QueryContext(context.Background(), "DELETE FROM verifications RETURNING id")
		if rows != nil {
			_ = rows.Close()
		}
		return err
	})
	if err == nil {
		t.Fatal("View allowed a write through QueryContext")
	}
}

func repeatHex(value string) string {
	result := ""
	for len(result) < 64 {
		result += value
	}
	return result[:64]
}
