package snapshot

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestStorePutGetRoundTripAndIdempotency(t *testing.T) {
	dataDir := t.TempDir()
	store, err := NewStore(dataDir)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer store.Close()

	html := []byte("<!doctype html><title>same</title><p>content</p>")
	first := Meta{URL: "https://one.example/page", FetchedAt: time.Unix(10, 0).UTC(), Engine: "http", StatusCode: 200, ContentType: "text/html"}
	second := Meta{URL: "https://two.example/copy", FetchedAt: time.Unix(20, 0).UTC(), Engine: "rod", StatusCode: 200, ContentType: "text/html; charset=utf-8"}

	firstID, err := store.Put(html, first)
	if err != nil {
		t.Fatalf("first Put() error = %v", err)
	}
	secondID, err := store.Put(html, second)
	if err != nil {
		t.Fatalf("second Put() error = %v", err)
	}
	if firstID != secondID {
		t.Fatalf("IDs differ: %q != %q", firstID, secondID)
	}

	gotHTML, gotMeta, err := store.Get(firstID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !bytes.Equal(gotHTML, html) {
		t.Fatalf("Get() html = %q, want %q", gotHTML, html)
	}
	if gotMeta != second {
		t.Fatalf("Get() meta = %#v, want latest %#v", gotMeta, second)
	}
	observations, err := store.Observations(firstID)
	if err != nil {
		t.Fatalf("Observations() error = %v", err)
	}
	if len(observations) != 2 || observations[0] != first || observations[1] != second {
		t.Fatalf("observations = %#v", observations)
	}

	blobs, err := filepath.Glob(filepath.Join(dataDir, "snapshots", "*", "*", "*.html.zst"))
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	if len(blobs) != 1 {
		t.Fatalf("blob count = %d, want 1", len(blobs))
	}
	if !store.Has(firstID) {
		t.Fatal("Has() = false after Put")
	}
}

func TestStoreConcurrentDuplicatePut(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer store.Close()

	const writes = 40
	var wg sync.WaitGroup
	errs := make(chan error, writes)
	ids := make(chan ID, writes)
	for i := 0; i < writes; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id, err := store.Put([]byte("same bytes"), Meta{URL: "https://example.com", FetchedAt: time.Unix(int64(i), 0).UTC()})
			if err != nil {
				errs <- err
				return
			}
			ids <- id
		}(i)
	}
	wg.Wait()
	close(errs)
	close(ids)
	for err := range errs {
		t.Fatalf("concurrent Put() error = %v", err)
	}
	var expected ID
	for id := range ids {
		if expected == "" {
			expected = id
		} else if id != expected {
			t.Fatalf("ID = %q, want %q", id, expected)
		}
	}
	observations, err := store.Observations(expected)
	if err != nil {
		t.Fatalf("Observations() error = %v", err)
	}
	if len(observations) != writes {
		t.Fatalf("observation count = %d, want %d", len(observations), writes)
	}
}

func TestStoreDetectsCorruptBlob(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer store.Close()
	id, err := store.Put([]byte("intact"), Meta{URL: "https://example.com"})
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	_, blobPath, _, err := store.paths(id)
	if err != nil {
		t.Fatalf("paths() error = %v", err)
	}
	if err := os.WriteFile(blobPath, []byte("not zstd"), 0o600); err != nil {
		t.Fatalf("corrupt blob: %v", err)
	}
	if _, _, err := store.Get(id); err == nil {
		t.Fatal("Get() error = nil for corrupt blob")
	}
}

func TestStoreRejectsInvalidID(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer store.Close()
	for _, id := range []ID{"", "sha256:../../etc/passwd", "md5:00000000000000000000000000000000", "sha256:not-hex000000000000000000000000000000000000000000000000000000000"} {
		if store.Has(id) {
			t.Fatalf("Has(%q) = true", id)
		}
		if _, _, err := store.Get(id); err == nil {
			t.Fatalf("Get(%q) error = nil", id)
		}
	}
}

func TestStoreAtomicWritesLeaveNoTemporaryFiles(t *testing.T) {
	dataDir := t.TempDir()
	store, err := NewStore(dataDir)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer store.Close()
	if _, err := store.Put([]byte("atomic"), Meta{URL: "https://example.com"}); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	temps, err := filepath.Glob(filepath.Join(dataDir, "snapshots", "*", "*", ".snapshot-tmp-*"))
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	if len(temps) != 0 {
		t.Fatalf("temporary files remain: %v", temps)
	}
}
