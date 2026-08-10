package ledger

import (
	"context"
	"path/filepath"
	"testing"
)

// A relative data directory must open correctly: the SQLite DSN is a file URI
// and an unresolved relative first segment would become its authority.
func TestOpenAcceptsRelativeDataDirectory(t *testing.T) {
	t.Chdir(t.TempDir())

	store, err := Open("relative-data")
	if err != nil {
		t.Fatalf("Open(relative) error = %v", err)
	}
	defer store.Close()

	if !filepath.IsAbs(store.Path()) {
		t.Fatalf("store path %q is not absolute", store.Path())
	}
	if err := store.View(context.Background(), func(tx ReadTx) error {
		var count int
		return tx.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM verifications").Scan(&count)
	}); err != nil {
		t.Fatalf("View() over relative-path store error = %v", err)
	}
}
