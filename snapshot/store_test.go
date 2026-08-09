package snapshot

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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

func TestStoreContentIsBoundedAndSidecarIndependent(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer store.Close()

	small := []byte("content remains readable without metadata")
	smallID, err := store.Put(small, Meta{URL: "https://example.com/small"})
	if err != nil {
		t.Fatalf("Put(small) error = %v", err)
	}
	_, _, smallMetaPath, err := store.paths(smallID)
	if err != nil {
		t.Fatalf("paths(small) error = %v", err)
	}
	if err := os.Remove(smallMetaPath); err != nil {
		t.Fatalf("remove small sidecar: %v", err)
	}
	if err := os.Remove(observationLogPath(smallMetaPath)); err != nil {
		t.Fatalf("remove small observation log: %v", err)
	}
	got, err := store.Content(smallID, len(small))
	if err != nil || !bytes.Equal(got, small) {
		t.Fatalf("Content(without sidecar) = (%q, %v), want %q", got, err, small)
	}
	exactLimit := bytes.Repeat([]byte("abcd"), 1<<20)
	exactID, err := store.Put(exactLimit, Meta{URL: "https://example.com/exact-limit"})
	if err != nil {
		t.Fatalf("Put(exact limit) error = %v", err)
	}
	got, err = store.Content(exactID, len(exactLimit))
	if err != nil || !bytes.Equal(got, exactLimit) {
		t.Fatalf("Content(exact limit) = (%d bytes, %v), want %d bytes", len(got), err, len(exactLimit))
	}

	const contentLimit = 1 << 20
	large := bytes.Repeat([]byte{'x'}, 32<<20)
	largeID, err := store.Put(large, Meta{URL: "https://example.com/large"})
	if err != nil {
		t.Fatalf("Put(large) error = %v", err)
	}
	large = nil
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	got, err = store.Content(largeID, contentLimit)
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if got != nil || !errors.Is(err, ErrContentTooLarge) {
		t.Fatalf("Content(compression bomb) = (%d bytes, %v), want nil + ErrContentTooLarge", len(got), err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 12<<20 {
		t.Fatalf("bounded Content allocated %d bytes for a 1 MiB limit", allocated)
	}

	for _, limit := range []int{0, -1, maximumStoredContentBytes + 1} {
		if content, err := store.Content(largeID, limit); content != nil || !errors.Is(err, ErrInvalidContentLimit) {
			t.Fatalf("Content(limit=%d) = (%d bytes, %v), want nil + ErrInvalidContentLimit", limit, len(content), err)
		}
	}
}

func TestStoreHasObservationStreamsAndValidatesIdentity(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer store.Close()

	content := []byte("same observation content")
	observations := []Meta{
		{URL: "HTTPS://EXAMPLE.COM.:443/page#one", FetchedAt: time.Unix(10, 0).UTC(), StatusCode: 200},
		{URL: "https://example.com/page", FetchedAt: time.Unix(20, 0).UTC(), StatusCode: 201},
		{URL: "https://example.com/missing", FetchedAt: time.Unix(30, 0).UTC(), StatusCode: 404},
	}
	var id ID
	for _, observation := range observations {
		id, err = store.Put(content, observation)
		if err != nil {
			t.Fatalf("Put() error = %v", err)
		}
	}

	visited := 0
	found, err := store.HasObservation(id, func(meta Meta) bool {
		visited++
		return meta.FetchedAt.Equal(observations[1].FetchedAt) && meta.StatusCode == observations[1].StatusCode
	})
	if err != nil || !found || visited != 2 {
		t.Fatalf("HasObservation() = (%v, %v), visited=%d, want true, nil, 2", found, err, visited)
	}
	if found, err := store.HasObservation(id, func(meta Meta) bool { return meta.URL == "absent" }); err != nil || found {
		t.Fatalf("HasObservation(absent) = (%v, %v), want false, nil", found, err)
	}
	if found, err := store.HasObservation(id, nil); found || err == nil {
		t.Fatalf("HasObservation(nil) = (%v, %v), want false + error", found, err)
	}

	_, _, metaPath, err := store.paths(id)
	if err != nil {
		t.Fatalf("paths() error = %v", err)
	}
	digest, _ := digestFromID(id)
	corrupt := sidecar{ID: ID(idPrefix + "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"), SHA256: digest, Meta: observations[2]}
	encoded, err := json.Marshal(corrupt)
	if err != nil {
		t.Fatalf("Marshal(corrupt sidecar) error = %v", err)
	}
	if err := os.WriteFile(metaPath, encoded, 0o600); err != nil {
		t.Fatalf("write corrupt sidecar: %v", err)
	}
	called := false
	if found, err := store.HasObservation(id, func(Meta) bool { called = true; return true }); found || err == nil || called {
		t.Fatalf("HasObservation(corrupt ID) = (%v, %v), callback=%v", found, err, called)
	}
	corrupt = sidecar{ID: id, SHA256: "0000000000000000000000000000000000000000000000000000000000000000", Meta: observations[2]}
	encoded, err = json.Marshal(corrupt)
	if err != nil {
		t.Fatalf("Marshal(corrupt digest sidecar) error = %v", err)
	}
	if err := os.WriteFile(metaPath, encoded, 0o600); err != nil {
		t.Fatalf("write corrupt digest sidecar: %v", err)
	}
	called = false
	if found, err := store.HasObservation(id, func(Meta) bool { called = true; return true }); found || err == nil || called {
		t.Fatalf("HasObservation(corrupt SHA) = (%v, %v), callback=%v", found, err, called)
	}
}

func TestStoreMigratesLegacyObservationArrayToAppendOnlyLog(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer store.Close()

	html := []byte("legacy content")
	legacy := []Meta{
		{URL: "https://one.example", FetchedAt: time.Unix(1, 0).UTC(), StatusCode: 200},
		{URL: "https://two.example", FetchedAt: time.Unix(2, 0).UTC(), StatusCode: 200},
	}
	id, err := store.Put(html, legacy[0])
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	_, _, metaPath, err := store.paths(id)
	if err != nil {
		t.Fatalf("paths() error = %v", err)
	}
	observationsPath := observationLogPath(metaPath)
	if err := os.Remove(observationsPath); err != nil {
		t.Fatalf("remove new-format log: %v", err)
	}
	digest, _ := digestFromID(id)
	legacyRecord := sidecar{ID: id, SHA256: digest, Meta: legacy[1], Observations: legacy}
	encoded, err := json.MarshalIndent(legacyRecord, "", "  ")
	if err != nil {
		t.Fatalf("Marshal(legacy sidecar) error = %v", err)
	}
	if err := os.WriteFile(metaPath, encoded, 0o600); err != nil {
		t.Fatalf("write legacy sidecar: %v", err)
	}

	found, err := store.HasObservation(id, func(meta Meta) bool { return meta == legacy[0] })
	if err != nil || !found {
		t.Fatalf("HasObservation(legacy) = (%v, %v), want true, nil", found, err)
	}
	third := Meta{URL: "https://three.example", FetchedAt: time.Unix(3, 0).UTC(), StatusCode: 201}
	if _, err := store.Put(html, third); err != nil {
		t.Fatalf("Put(migrate legacy) error = %v", err)
	}
	beforeAppend, err := os.Stat(observationsPath)
	if err != nil {
		t.Fatalf("Stat(observation log) error = %v", err)
	}
	fourth := Meta{URL: "https://four.example", FetchedAt: time.Unix(4, 0).UTC(), StatusCode: 202}
	if _, err := store.Put(html, fourth); err != nil {
		t.Fatalf("Put(append) error = %v", err)
	}
	afterAppend, err := os.Stat(observationsPath)
	if err != nil {
		t.Fatalf("Stat(observation log after append) error = %v", err)
	}
	if !os.SameFile(beforeAppend, afterAppend) || afterAppend.Size() <= beforeAppend.Size() {
		t.Fatalf("observation log was replaced or did not grow: before=%v after=%v", beforeAppend, afterAppend)
	}

	got, err := store.Observations(id)
	want := append(append([]Meta(nil), legacy...), third, fourth)
	if err != nil || len(got) != len(want) {
		t.Fatalf("Observations(after migration) = (%#v, %v), want %#v", got, err, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("observation %d = %#v, want %#v", index, got[index], want[index])
		}
	}
	compact, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("ReadFile(compact sidecar) error = %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(compact, &decoded); err != nil {
		t.Fatalf("Unmarshal(compact sidecar) error = %v", err)
	}
	if _, exists := decoded["observations"]; exists {
		t.Fatalf("compact sidecar still contains observation history: %s", compact)
	}
}

func TestStoreIgnoresAndRepairsTornObservationAppend(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer store.Close()
	html := []byte("torn append content")
	first := Meta{URL: "https://one.example", FetchedAt: time.Unix(1, 0).UTC(), StatusCode: 200}
	second := Meta{URL: "https://two.example", FetchedAt: time.Unix(2, 0).UTC(), StatusCode: 200}
	id, err := store.Put(html, first)
	if err != nil {
		t.Fatalf("Put(first) error = %v", err)
	}
	if _, err := store.Put(html, second); err != nil {
		t.Fatalf("Put(second) error = %v", err)
	}
	_, _, metaPath, err := store.paths(id)
	if err != nil {
		t.Fatalf("paths() error = %v", err)
	}
	logPath := observationLogPath(metaPath)
	log, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open observation log: %v", err)
	}
	if _, err := log.WriteString(`{"url":"https://torn.example"`); err != nil {
		_ = log.Close()
		t.Fatalf("write torn observation: %v", err)
	}
	if err := log.Sync(); err != nil {
		_ = log.Close()
		t.Fatalf("sync torn observation: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("close torn observation: %v", err)
	}

	observations, err := store.Observations(id)
	if err != nil || len(observations) != 2 {
		t.Fatalf("Observations(with torn tail) = (%#v, %v), want two committed records", observations, err)
	}
	third := Meta{URL: "https://three.example", FetchedAt: time.Unix(3, 0).UTC(), StatusCode: 201}
	if _, err := store.Put(html, third); err != nil {
		t.Fatalf("Put(after torn tail) error = %v", err)
	}
	observations, err = store.Observations(id)
	if err != nil || len(observations) != 3 || observations[2] != third {
		t.Fatalf("Observations(after repair) = (%#v, %v), want repaired three-record history", observations, err)
	}
}

func TestStoreHasObservationLargeHistoryHasBoundedAllocations(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer store.Close()

	html := []byte("large history content")
	target := Meta{URL: "https://target.example", FetchedAt: time.Unix(1, 0).UTC(), StatusCode: 200}
	id, err := store.Put(html, target)
	if err != nil {
		t.Fatalf("Put(target) error = %v", err)
	}
	_, _, metaPath, err := store.paths(id)
	if err != nil {
		t.Fatalf("paths() error = %v", err)
	}
	log, err := os.OpenFile(observationLogPath(metaPath), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open observation log: %v", err)
	}
	encoder := json.NewEncoder(log)
	other := Meta{URL: "https://other.example", FetchedAt: time.Unix(2, 0).UTC(), StatusCode: 200}
	for range 100_000 {
		if err := encoder.Encode(other); err != nil {
			_ = log.Close()
			t.Fatalf("append large history: %v", err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatalf("close observation log: %v", err)
	}

	assertFound := func() {
		visited := 0
		found, err := store.HasObservation(id, func(meta Meta) bool {
			visited++
			return meta == target
		})
		if err != nil || !found || visited != 1 {
			panic("streaming observation lookup did not stop at the first match")
		}
	}
	assertFound()
	if allocations := testing.AllocsPerRun(5, assertFound); allocations > 500 {
		t.Fatalf("HasObservation allocated %.0f objects for an early match in 100k history", allocations)
	}
	latest := Meta{URL: "https://latest.example", FetchedAt: time.Unix(3, 0).UTC(), StatusCode: 201}
	if _, err := store.Put(html, latest); err != nil {
		t.Fatalf("Put(after 100k history) error = %v", err)
	}
	found, err := store.HasObservation(id, func(meta Meta) bool { return meta == latest })
	if err != nil || !found {
		t.Fatalf("HasObservation(after 100k history) = (%v, %v), want true, nil", found, err)
	}
}

func TestStoreConcurrentPutAndHasObservation(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	defer store.Close()
	html := []byte("concurrent observation content")
	initial := Meta{URL: "https://initial.example", FetchedAt: time.Unix(1, 0).UTC(), StatusCode: 200}
	id, err := store.Put(html, initial)
	if err != nil {
		t.Fatalf("Put(initial) error = %v", err)
	}

	const workers = 24
	start := make(chan struct{})
	errs := make(chan error, workers*2)
	var group sync.WaitGroup
	for index := range workers {
		group.Add(2)
		go func(index int) {
			defer group.Done()
			<-start
			_, err := store.Put(html, Meta{URL: "https://writer.example", FetchedAt: time.Unix(int64(index+2), 0).UTC(), StatusCode: 200})
			if err != nil {
				errs <- err
			}
		}(index)
		go func() {
			defer group.Done()
			<-start
			found, err := store.HasObservation(id, func(meta Meta) bool { return meta == initial })
			if err != nil {
				errs <- err
			} else if !found {
				errs <- errors.New("initial observation disappeared")
			}
		}()
	}
	close(start)
	group.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent operation error = %v", err)
	}
	observations, err := store.Observations(id)
	if err != nil || len(observations) != workers+1 {
		t.Fatalf("Observations() after concurrency = (%d, %v), want %d", len(observations), err, workers+1)
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
		if _, err := store.Content(id, 1); err == nil {
			t.Fatalf("Content(%q) error = nil", id)
		}
		if found, err := store.HasObservation(id, func(Meta) bool { return true }); found || err == nil {
			t.Fatalf("HasObservation(%q) = (%v, %v), want false + error", id, found, err)
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
