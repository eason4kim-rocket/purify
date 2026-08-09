package compiler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/use-agent/purify/ledger"
	"github.com/use-agent/purify/simhash"
	"github.com/use-agent/purify/snapshot"
)

var catalogTestTime = time.Date(2026, 8, 9, 12, 0, 0, 123456789, time.UTC)

func TestSampleCatalogEligibilityRevisionAndClone(t *testing.T) {
	now := catalogTestTime
	catalog, durable := openSampleCatalog(t, func() time.Time { return now })
	base := uint64(1) << 63
	keys := []PageKey{
		catalogPageKey(t, 1, base),
		catalogPageKey(t, 2, base|1),
		catalogPageKey(t, 3, base|3),
	}
	snapshots := []string{
		catalogSnapshotID("one"),
		catalogSnapshotID("two"),
		catalogSnapshotID("three"),
	}

	for index := range keys {
		now = catalogTestTime.Add(time.Duration(index) * time.Second)
		set, ready, err := catalog.Observe(context.Background(), keys[index], snapshots[index], now)
		if err != nil {
			t.Fatalf("Observe(%d) error = %v", index, err)
		}
		if ready != (index == 2) || set.Count != index+1 {
			t.Fatalf("Observe(%d) = count %d ready %v", index, set.Count, ready)
		}
	}

	loaded, ready, err := catalog.Load(context.Background(), CompileKey{
		Host: keys[0].Host, SchemaHash: keys[0].SchemaHash,
		ContentProfile:    DefaultCompilerProfile,
		TemplateClusterID: templateClusterID(base), ClusterSimHash: base,
	}, MaxSampleCatalogClusterEntries)
	if err != nil || !ready || loaded.Count != MinCompileSamples || !validLowerHex(loaded.Revision, 64) {
		t.Fatalf("Load() = %#v, ready=%v, err=%v", loaded, ready, err)
	}
	if !sort.SliceIsSorted(loaded.Samples, func(i, j int) bool {
		return loaded.Samples[i].PageHash < loaded.Samples[j].PageHash
	}) {
		t.Fatalf("samples are not in stable identity order: %#v", loaded.Samples)
	}
	wantFingerprints := map[string]uint64{
		keys[0].PageHash: base,
		keys[1].PageHash: base | 1,
		keys[2].PageHash: base | 3,
	}
	for _, sample := range loaded.Samples {
		if sample.SampleSimHash != wantFingerprints[sample.PageHash] {
			t.Fatalf("sample %s fingerprint = %#x", sample.PageHash, sample.SampleSimHash)
		}
	}

	original := cloneSampleSet(loaded)
	loaded.Samples[0].SnapshotID = catalogSnapshotID("mutated")
	loaded.Key.Host = "mutated.example"
	again, ready, err := catalog.Load(context.Background(), original.Key, MaxSampleCatalogClusterEntries)
	if err != nil || !ready || !reflect.DeepEqual(again, original) {
		t.Fatalf("Load(after caller mutation) = %#v/%v/%v, want %#v", again, ready, err, original)
	}

	// An exact reference refresh advances last_seen_at but leaves the stable
	// membership watermark unchanged, including under a rolled-back clock.
	var beforeLastSeen string
	if err := durable.View(context.Background(), func(tx ledger.ReadTx) error {
		return tx.QueryRowContext(context.Background(), `SELECT last_seen_at FROM compiler_samples
			WHERE host = ? AND schema_hash = ? AND content_profile = ? AND page_hash = ?`,
			keys[2].Host, keys[2].SchemaHash, DefaultCompilerProfile, keys[2].PageHash).Scan(&beforeLastSeen)
	}); err != nil {
		t.Fatalf("read last_seen_at: %v", err)
	}
	now = catalogTestTime.Add(-time.Hour)
	refreshed, ready, err := catalog.Observe(context.Background(), keys[2], snapshots[2],
		catalogTestTime.Add(2*time.Second))
	if err != nil || !ready || refreshed.Revision != original.Revision {
		t.Fatalf("Observe(refresh) revision = %q/%v/%v, want %q", refreshed.Revision, ready, err, original.Revision)
	}
	var afterLastSeen string
	if err := durable.View(context.Background(), func(tx ledger.ReadTx) error {
		return tx.QueryRowContext(context.Background(), `SELECT last_seen_at FROM compiler_samples
			WHERE host = ? AND schema_hash = ? AND content_profile = ? AND page_hash = ?`,
			keys[2].Host, keys[2].SchemaHash, DefaultCompilerProfile, keys[2].PageHash).Scan(&afterLastSeen)
	}); err != nil {
		t.Fatalf("read refreshed last_seen_at: %v", err)
	}
	if afterLastSeen <= beforeLastSeen {
		t.Fatalf("last_seen_at did not advance monotonically: %q -> %q", beforeLastSeen, afterLastSeen)
	}

	now = catalogTestTime.Add(time.Hour)
	replacement := catalogSnapshotID("three-new")
	replaced, ready, err := catalog.Observe(context.Background(), keys[2], replacement, now)
	if err != nil || !ready || replaced.Count != 3 || replaced.Revision == original.Revision {
		t.Fatalf("Observe(replacement) = %#v/%v/%v", replaced, ready, err)
	}
	for _, sample := range replaced.Samples {
		if sample.SnapshotID == snapshots[2] {
			t.Fatal("superseded same-page snapshot remained in sample set")
		}
	}
}

func TestSampleSetRevisionTracksOnlyCompilationInputs(t *testing.T) {
	key := CompileKey{
		Host: "example.com", SchemaHash: catalogDigest("revision-schema"),
		ContentProfile:    DefaultCompilerProfile,
		TemplateClusterID: templateClusterID(0x8000), ClusterSimHash: 0x8000,
	}
	set := SampleSet{Key: key, Samples: []SampleRef{
		{PageHash: catalogDigest("revision-page-a"), SnapshotID: catalogSnapshotID("revision-a"), SampleSimHash: 0x8000, FetchedAt: catalogTestTime},
		{PageHash: catalogDigest("revision-page-b"), SnapshotID: catalogSnapshotID("revision-b"), SampleSimHash: 0x8001, FetchedAt: catalogTestTime},
	}}
	base := sampleSetRevision(set)
	set.Samples[0].FetchedAt = catalogTestTime.Add(24 * time.Hour)
	if refreshed := sampleSetRevision(set); refreshed != base {
		t.Fatalf("timestamp-only refresh changed revision: %q -> %q", base, refreshed)
	}
	set.Samples[0].SampleSimHash ^= 1
	if changed := sampleSetRevision(set); changed == base {
		t.Fatal("sample fingerprint change did not change revision")
	}
}

func TestSampleCatalogRejectsInvalidInputsAndCanceledContext(t *testing.T) {
	if _, err := NewSampleCatalog(nil); !errors.Is(err, ErrInvalidSampleCatalog) {
		t.Fatalf("NewSampleCatalog(nil) error = %v", err)
	}
	durable, err := ledger.Open(t.TempDir())
	if err != nil {
		t.Fatalf("ledger.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = durable.Close() })
	if _, err := NewSampleCatalog(durable, nil); !errors.Is(err, ErrInvalidSampleCatalog) {
		t.Fatalf("NewSampleCatalog(nil clock) error = %v", err)
	}
	if _, err := NewSampleCatalog(durable, time.Now, time.Now); !errors.Is(err, ErrInvalidSampleCatalog) {
		t.Fatalf("NewSampleCatalog(two clocks) error = %v", err)
	}

	var nilCatalog *SampleCatalog
	if _, _, err := nilCatalog.Observe(context.Background(), PageKey{}, "", time.Time{}); !errors.Is(err, ErrInvalidSampleCatalog) {
		t.Fatalf("nil Observe() error = %v", err)
	}
	if _, _, err := nilCatalog.Load(context.Background(), CompileKey{}, MinCompileSamples); !errors.Is(err, ErrInvalidSampleCatalog) {
		t.Fatalf("nil Load() error = %v", err)
	}

	now := catalogTestTime
	catalog, err := NewSampleCatalog(durable, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewSampleCatalog() error = %v", err)
	}
	page := catalogPageKey(t, 9, uint64(1)<<63)
	snapshotID := catalogSnapshotID("invalid-matrix")
	invalidObservations := []struct {
		name       string
		page       PageKey
		snapshotID string
		fetchedAt  time.Time
	}{
		{"page key", PageKey{}, snapshotID, now},
		{"snapshot prefix", page, strings.TrimPrefix(snapshotID, "sha256:"), now},
		{"snapshot uppercase", page, "sha256:" + strings.Repeat("F", 64), now},
		{"zero fetched time", page, snapshotID, time.Time{}},
		{"five-digit fetched year", page, snapshotID, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, test := range invalidObservations {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := catalog.Observe(context.Background(), test.page, test.snapshotID, test.fetchedAt); !errors.Is(err, ErrInvalidSampleCatalog) {
				t.Fatalf("Observe() error = %v", err)
			}
		})
	}

	validKey := CompileKey{
		Host: page.Host, SchemaHash: page.SchemaHash, ContentProfile: DefaultCompilerProfile,
		TemplateClusterID: templateClusterID(page.TemplateSimHash), ClusterSimHash: page.TemplateSimHash,
	}
	invalidKeys := []CompileKey{
		{},
		{Host: validKey.Host, SchemaHash: validKey.SchemaHash, ContentProfile: "custom", TemplateClusterID: validKey.TemplateClusterID, ClusterSimHash: validKey.ClusterSimHash},
		{Host: "shop.example.com", SchemaHash: validKey.SchemaHash, ContentProfile: validKey.ContentProfile, TemplateClusterID: validKey.TemplateClusterID, ClusterSimHash: validKey.ClusterSimHash},
		{Host: validKey.Host, SchemaHash: validKey.SchemaHash, ContentProfile: validKey.ContentProfile, TemplateClusterID: catalogDigest("wrong-cluster"), ClusterSimHash: validKey.ClusterSimHash},
	}
	for index, key := range invalidKeys {
		if _, _, err := catalog.Load(context.Background(), key, MinCompileSamples); !errors.Is(err, ErrInvalidSampleCatalog) {
			t.Fatalf("Load(invalid key %d) error = %v", index, err)
		}
	}
	for _, limit := range []int{MinCompileSamples - 1, MaxSampleCatalogClusterEntries + 1} {
		if _, _, err := catalog.Load(context.Background(), validKey, limit); !errors.Is(err, ErrInvalidSampleCatalog) {
			t.Fatalf("Load(invalid limit %d) error = %v", limit, err)
		}
	}

	zeroClockCatalog, err := NewSampleCatalog(durable, func() time.Time { return time.Time{} })
	if err != nil {
		t.Fatalf("NewSampleCatalog(zero clock) error = %v", err)
	}
	if _, _, err := zeroClockCatalog.Observe(context.Background(), page, snapshotID, now); !errors.Is(err, ErrInvalidSampleCatalog) {
		t.Fatalf("Observe(zero clock) error = %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := catalog.Observe(canceled, page, snapshotID, now); !errors.Is(err, context.Canceled) {
		t.Fatalf("Observe(canceled) error = %v", err)
	}
	if _, _, err := catalog.Load(canceled, validKey, MinCompileSamples); !errors.Is(err, context.Canceled) {
		t.Fatalf("Load(canceled) error = %v", err)
	}
	assertCatalogRowCount(t, durable, 0)
}

func TestSampleCatalogPerPageLatestAndLoadSnapshotDeduplication(t *testing.T) {
	now := catalogTestTime
	catalog, durable := openSampleCatalog(t, func() time.Time { return now })
	fingerprint := uint64(1) << 63
	first := catalogPageKey(t, 10, fingerprint)
	second := catalogPageKey(t, 11, fingerprint)
	sameSnapshot := catalogSnapshotID("identical-content")

	if _, _, err := catalog.Observe(context.Background(), first, sameSnapshot, now); err != nil {
		t.Fatalf("Observe(first) error = %v", err)
	}
	now = now.Add(time.Second)
	set, ready, err := catalog.Observe(context.Background(), second, sameSnapshot, now)
	if err != nil || ready || set.Count != 1 || set.Samples[0].PageHash != second.PageHash {
		t.Fatalf("same snapshot on two pages = %#v/%v/%v", set, ready, err)
	}
	assertCatalogRowCount(t, durable, 2)

	// Each page converges independently. An older same-page snapshot cannot
	// replace the latest one, while Load still emits repeated content once.
	now = now.Add(time.Second)
	set, _, err = catalog.Observe(context.Background(), first, sameSnapshot, catalogTestTime)
	if err != nil || set.Samples[0].PageHash != second.PageHash {
		t.Fatalf("repeated snapshot selection = %#v/%v", set, err)
	}
	olderSnapshot := catalogSnapshotID("older-content")
	set, _, err = catalog.Observe(context.Background(), second, olderSnapshot, catalogTestTime)
	if err != nil || set.Samples[0].SnapshotID != sameSnapshot {
		t.Fatalf("older same-page snapshot = %#v/%v", set, err)
	}
	assertCatalogRowCount(t, durable, 2)

	// Equal fetched_at duplicate snapshots are selected by the smaller stable
	// page hash during Load, independent of arrival order.
	tieTime := catalogTestTime.Add(10 * time.Second)
	third := catalogPageKey(t, 12, fingerprint)
	pageWinner, pageLoser := second, third
	if pageWinner.PageHash > pageLoser.PageHash {
		pageWinner, pageLoser = pageLoser, pageWinner
	}
	tieSnapshot := catalogSnapshotID("tie-content")
	now = tieTime
	if _, _, err := catalog.Observe(context.Background(), pageLoser, tieSnapshot, tieTime); err != nil {
		t.Fatalf("Observe(tie loser first) error = %v", err)
	}
	set, _, err = catalog.Observe(context.Background(), pageWinner, tieSnapshot, tieTime)
	if err != nil {
		t.Fatalf("Observe(tie winner) error = %v", err)
	}
	found := false
	for _, sample := range set.Samples {
		if sample.SnapshotID == tieSnapshot {
			found = sample.PageHash == pageWinner.PageHash
		}
	}
	if !found {
		t.Fatalf("equal-time snapshot dedup did not select smaller page hash: %#v", set.Samples)
	}

	if _, _, err := catalog.Observe(context.Background(), pageWinner, tieSnapshot,
		tieTime.Add(time.Second)); err != nil {
		t.Fatalf("Observe(existing snapshot) error = %v", err)
	}
	conflicting := pageWinner
	conflicting.TemplateSimHash ^= 1
	if _, _, err := catalog.Observe(context.Background(), conflicting, tieSnapshot,
		tieTime.Add(2*time.Second)); !errors.Is(err, ErrInvalidSampleCatalog) {
		t.Fatalf("same snapshot with changed simhash error = %v", err)
	}
}

func TestSampleCatalogEqualTimeChainConvergesAcrossAllPermutations(t *testing.T) {
	orders := [][]int{
		{0, 1, 2}, {0, 2, 1}, {1, 0, 2},
		{1, 2, 0}, {2, 0, 1}, {2, 1, 0},
	}
	fingerprint := uint64(1) << 63
	p0 := catalogPageKey(t, 13, fingerprint)
	p1 := catalogPageKey(t, 14, fingerprint)
	s0 := catalogSnapshotID("chain-s0")
	s1 := catalogSnapshotID("chain-s1")
	if s1 < s0 {
		s0, s1 = s1, s0
	}
	events := []struct {
		page       PageKey
		snapshotID string
	}{
		{p0, s0},
		{p0, s1},
		{p1, s1},
	}
	key := CompileKey{
		Host: p0.Host, SchemaHash: p0.SchemaHash, ContentProfile: DefaultCompilerProfile,
		TemplateClusterID: templateClusterID(fingerprint), ClusterSimHash: fingerprint,
	}
	var want SampleSet
	for orderIndex, order := range orders {
		now := catalogTestTime
		catalog, durable := openSampleCatalog(t, func() time.Time { return now })
		for _, eventIndex := range order {
			event := events[eventIndex]
			if _, _, err := catalog.Observe(context.Background(), event.page, event.snapshotID, catalogTestTime); err != nil {
				t.Fatalf("order %v Observe(%d) error = %v", order, eventIndex, err)
			}
		}
		assertCatalogRowCount(t, durable, 2)
		got, ready, err := catalog.Load(context.Background(), key, MaxSampleCatalogClusterEntries)
		if err != nil || ready || got.Count != 2 {
			t.Fatalf("order %v Load() = %#v/%v/%v", order, got, ready, err)
		}
		if orderIndex == 0 {
			want = got
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("order %v did not converge:\ngot  %#v\nwant %#v", order, got, want)
		}
	}
}

func TestSampleCatalogClusterDistanceAndFixedRepresentative(t *testing.T) {
	now := catalogTestTime
	catalog, _ := openSampleCatalog(t, func() time.Time { return now })
	base := uint64(1) << 63
	within := base | 0x3f
	beyond := base | 0x7f

	first, ready, err := catalog.Observe(context.Background(), catalogPageKey(t, 20, base),
		catalogSnapshotID("cluster-base"), now)
	if err != nil || ready {
		t.Fatalf("Observe(base) = %#v/%v/%v", first, ready, err)
	}
	now = now.Add(time.Second)
	second, _, err := catalog.Observe(context.Background(), catalogPageKey(t, 21, within),
		catalogSnapshotID("cluster-within"), now)
	if err != nil || second.Key != first.Key || second.Key.ClusterSimHash != base {
		t.Fatalf("distance-6 cluster = %#v/%v", second, err)
	}
	foundWithin := false
	for _, sample := range second.Samples {
		foundWithin = foundWithin || sample.SampleSimHash == within
	}
	if !foundWithin {
		t.Fatalf("distance-6 sample fingerprint was not retained: %#v", second.Samples)
	}
	now = now.Add(time.Second)
	third, _, err := catalog.Observe(context.Background(), catalogPageKey(t, 22, beyond),
		catalogSnapshotID("cluster-beyond"), now)
	if err != nil || third.Key.TemplateClusterID == first.Key.TemplateClusterID ||
		third.Key.ClusterSimHash != beyond {
		t.Fatalf("distance-7 cluster = %#v/%v", third, err)
	}

	// A nearest-cluster tie is broken by stable cluster ID, not query order.
	a := catalogCluster{Key: CompileKey{TemplateClusterID: templateClusterID(0x8000), ClusterSimHash: 0x8000}}
	b := catalogCluster{Key: CompileKey{TemplateClusterID: templateClusterID(0x2000), ClusterSimHash: 0x2000}}
	wantID := a.Key.TemplateClusterID
	if b.Key.TemplateClusterID < wantID {
		wantID = b.Key.TemplateClusterID
	}
	selected, ok := nearestCatalogCluster([]catalogCluster{b, a}, 0xa000)
	if !ok || selected.Key.TemplateClusterID != wantID {
		t.Fatalf("tie selected %q, want %q", selected.Key.TemplateClusterID, wantID)
	}
}

func TestSampleCatalogPerClusterCapacityAndLoadLimit(t *testing.T) {
	now := catalogTestTime
	catalog, durable := openSampleCatalog(t, func() time.Time { return now })
	base := uint64(1) << 63
	var firstSnapshot string
	for index := 0; index < MaxSampleCatalogClusterEntries+1; index++ {
		now = catalogTestTime.Add(time.Duration(index) * time.Second)
		snapshotID := catalogSnapshotID(fmt.Sprintf("capacity-%02d", index))
		if index == 0 {
			firstSnapshot = snapshotID
		}
		if _, _, err := catalog.Observe(context.Background(), catalogPageKey(t, 100+index, base), snapshotID, now); err != nil {
			t.Fatalf("Observe(%d) error = %v", index, err)
		}
	}
	assertCatalogRowCount(t, durable, MaxSampleCatalogClusterEntries)
	key := CompileKey{
		Host: "example.com", SchemaHash: catalogPageKey(t, 100, base).SchemaHash,
		ContentProfile:    DefaultCompilerProfile,
		TemplateClusterID: templateClusterID(base), ClusterSimHash: base,
	}
	loaded, ready, err := catalog.Load(context.Background(), key, MaxSampleCatalogClusterEntries)
	if err != nil || !ready || loaded.Count != MaxSampleCatalogClusterEntries {
		t.Fatalf("Load(capacity) = %d/%v/%v", loaded.Count, ready, err)
	}
	for _, sample := range loaded.Samples {
		if sample.SnapshotID == firstSnapshot {
			t.Fatal("N+1 cluster insert did not evict the oldest fetched sample")
		}
	}
	limited, ready, err := catalog.Load(context.Background(), key, MinCompileSamples)
	if err != nil || !ready || limited.Count != MinCompileSamples {
		t.Fatalf("Load(limit=3) = %d/%v/%v", limited.Count, ready, err)
	}
	newest := append([]SampleRef(nil), loaded.Samples...)
	sort.Slice(newest, func(i, j int) bool {
		if !newest[i].FetchedAt.Equal(newest[j].FetchedAt) {
			return newest[i].FetchedAt.After(newest[j].FetchedAt)
		}
		return newest[i].PageHash < newest[j].PageHash
	})
	wantPages := []string{newest[0].PageHash, newest[1].PageHash, newest[2].PageHash}
	gotPages := []string{limited.Samples[0].PageHash, limited.Samples[1].PageHash, limited.Samples[2].PageHash}
	sort.Strings(wantPages)
	if !reflect.DeepEqual(gotPages, wantPages) {
		t.Fatalf("Load(3) pages = %v, want newest %v", gotPages, wantPages)
	}
	for _, invalid := range []int{0, MinCompileSamples - 1, MaxSampleCatalogClusterEntries + 1} {
		if _, _, err := catalog.Load(context.Background(), key, invalid); !errors.Is(err, ErrInvalidSampleCatalog) {
			t.Fatalf("Load(limit=%d) error = %v", invalid, err)
		}
	}
}

func TestSampleCatalogTTLBoundaryNeverDeletesCAS(t *testing.T) {
	dataDir := t.TempDir()
	durable, err := ledger.Open(dataDir)
	if err != nil {
		t.Fatalf("ledger.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = durable.Close() })
	now := catalogTestTime
	catalog, err := NewSampleCatalog(durable, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewSampleCatalog() error = %v", err)
	}
	cas, err := snapshot.NewStore(dataDir)
	if err != nil {
		t.Fatalf("snapshot.NewStore() error = %v", err)
	}
	t.Cleanup(cas.Close)
	html := []byte(`<html><body><main><h1>durable CAS</h1></main></body></html>`)
	id, err := cas.Put(html, snapshot.Meta{URL: "https://example.com/ttl", FetchedAt: now})
	if err != nil {
		t.Fatalf("snapshot.Put() error = %v", err)
	}
	page, err := BuildPageKey("https://example.com/ttl", catalogSchema(), string(html))
	if err != nil {
		t.Fatalf("BuildPageKey() error = %v", err)
	}
	set, _, err := catalog.Observe(context.Background(), page, string(id), now)
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}

	now = catalogTestTime.Add(SampleCatalogTTL)
	atBoundary, ready, err := catalog.Load(context.Background(), set.Key, MaxSampleCatalogClusterEntries)
	if err != nil || ready || atBoundary.Count != 1 {
		t.Fatalf("Load(at TTL) = %d/%v/%v", atBoundary.Count, ready, err)
	}
	now = now.Add(time.Nanosecond)
	expired, ready, err := catalog.Load(context.Background(), set.Key, MaxSampleCatalogClusterEntries)
	if err != nil || ready || expired.Count != 0 {
		t.Fatalf("Load(after TTL) = %d/%v/%v", expired.Count, ready, err)
	}
	if !cas.Has(id) {
		t.Fatal("catalog TTL cleanup deleted immutable CAS content")
	}
}

func TestSampleCatalogGlobalCapacityNPlusOne(t *testing.T) {
	now := catalogTestTime
	catalog, durable := openSampleCatalog(t, func() time.Time { return now })
	clusterFingerprint := uint64(1) << 63
	clusterID := templateClusterID(clusterFingerprint)
	encodedTime := formatExtractorTime(now)
	schemaHash := sha256Hex([]byte("seed-schema"))
	if err := durable.Update(context.Background(), func(tx ledger.WriteTx) error {
		_, err := tx.ExecContext(context.Background(), `WITH digits(d) AS (
			VALUES (0),(1),(2),(3),(4),(5),(6),(7),(8),(9)
		), sequence(x) AS (
			SELECT a.d + 10*b.d + 100*c.d + 1000*d.d
			FROM digits a CROSS JOIN digits b CROSS JOIN digits c CROSS JOIN digits d
		)
		INSERT INTO compiler_samples (
			host, schema_hash, content_profile, template_cluster_id,
			cluster_simhash, sample_simhash, page_hash, snapshot_id,
			fetched_at, first_seen_at, last_seen_at
		)
		SELECT printf('seed%03d.example', x / 20), ?, ?, ?, ?, ?,
			printf('%064x', x + 1), 'sha256:' || printf('%064x', x + 1),
			?, ?, ? FROM sequence`, schemaHash, DefaultCompilerProfile, clusterID,
			encodeSimHash(clusterFingerprint), encodeSimHash(clusterFingerprint),
			encodedTime, encodedTime, encodedTime)
		return err
	}); err != nil {
		t.Fatalf("seed %d catalog rows: %v", MaxSampleCatalogEntries, err)
	}
	assertCatalogRowCount(t, durable, MaxSampleCatalogEntries)

	now = now.Add(time.Second)
	page := catalogPageKey(t, 5000, clusterFingerprint)
	set, _, err := catalog.Observe(context.Background(), page, catalogSnapshotID("global-n-plus-one"), now)
	if err != nil || set.Count != 1 {
		t.Fatalf("Observe(global N+1) = %#v/%v", set, err)
	}
	assertCatalogRowCount(t, durable, MaxSampleCatalogEntries)
	var targetCount int
	if err := durable.View(context.Background(), func(tx ledger.ReadTx) error {
		return tx.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM compiler_samples
			WHERE host = ? AND schema_hash = ? AND content_profile = ? AND page_hash = ?`,
			page.Host, page.SchemaHash, DefaultCompilerProfile, page.PageHash).Scan(&targetCount)
	}); err != nil || targetCount != 1 {
		t.Fatalf("newest global sample count = %d, err=%v", targetCount, err)
	}
}

func TestSampleCatalogClusterCapacityEvictsOldestCluster(t *testing.T) {
	now := catalogTestTime.Add(time.Hour)
	catalog, durable := openSampleCatalog(t, func() time.Time { return now })
	fingerprints := distantFingerprints(MaxSampleCatalogClusters + 1)
	page := catalogPageKey(t, 6000, fingerprints[len(fingerprints)-1])
	if err := durable.Update(context.Background(), func(tx ledger.WriteTx) error {
		for index, fingerprint := range fingerprints[:MaxSampleCatalogClusters] {
			stamp := formatExtractorTime(catalogTestTime.Add(time.Duration(index) * time.Nanosecond))
			if _, err := tx.ExecContext(context.Background(), `INSERT INTO compiler_samples (
				host, schema_hash, content_profile, template_cluster_id,
				cluster_simhash, sample_simhash, page_hash, snapshot_id,
				fetched_at, first_seen_at, last_seen_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				page.Host, page.SchemaHash, DefaultCompilerProfile, templateClusterID(fingerprint),
				encodeSimHash(fingerprint), encodeSimHash(fingerprint),
				catalogDigest(fmt.Sprintf("cluster-page-%d", index)),
				catalogSnapshotID(fmt.Sprintf("cluster-snapshot-%d", index)),
				stamp, stamp, stamp); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed clusters: %v", err)
	}
	set, _, err := catalog.Observe(context.Background(), page,
		catalogSnapshotID("cluster-n-plus-one"), now)
	if err != nil || set.Key.ClusterSimHash != page.TemplateSimHash {
		t.Fatalf("Observe(cluster N+1) = %#v/%v", set, err)
	}
	var clusters, oldest, newest int
	if err := durable.View(context.Background(), func(tx ledger.ReadTx) error {
		if err := tx.QueryRowContext(context.Background(), `SELECT COUNT(DISTINCT template_cluster_id)
			FROM compiler_samples WHERE host = ? AND schema_hash = ? AND content_profile = ?`,
			page.Host, page.SchemaHash, DefaultCompilerProfile).Scan(&clusters); err != nil {
			return err
		}
		if err := tx.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM compiler_samples
			WHERE host = ? AND schema_hash = ? AND content_profile = ? AND template_cluster_id = ?`,
			page.Host, page.SchemaHash, DefaultCompilerProfile,
			templateClusterID(fingerprints[0])).Scan(&oldest); err != nil {
			return err
		}
		return tx.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM compiler_samples
			WHERE host = ? AND schema_hash = ? AND content_profile = ? AND template_cluster_id = ?`,
			page.Host, page.SchemaHash, DefaultCompilerProfile,
			templateClusterID(fingerprints[len(fingerprints)-1])).Scan(&newest)
	}); err != nil {
		t.Fatalf("inspect cluster eviction: %v", err)
	}
	if clusters != MaxSampleCatalogClusters || oldest != 0 || newest != 1 {
		t.Fatalf("cluster eviction = clusters %d oldest %d newest %d", clusters, oldest, newest)
	}
}

func TestSampleCatalogConcurrentObserveIsBoundedAndDistinct(t *testing.T) {
	now := catalogTestTime
	catalog, durable := openSampleCatalog(t, func() time.Time { return now })
	base := uint64(1) << 63
	const workers = 100
	start := make(chan struct{})
	errorsByWorker := make(chan error, workers)
	var group sync.WaitGroup
	for index := 0; index < workers; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			<-start
			_, _, err := catalog.Observe(context.Background(), catalogPageKey(t, 7000+index, base),
				catalogSnapshotID(fmt.Sprintf("concurrent-%d", index)),
				catalogTestTime.Add(time.Duration(index)*time.Second))
			if err != nil {
				errorsByWorker <- err
			}
		}(index)
	}
	close(start)
	group.Wait()
	close(errorsByWorker)
	for err := range errorsByWorker {
		t.Errorf("concurrent Observe() error = %v", err)
	}
	assertCatalogRowCount(t, durable, MaxSampleCatalogClusterEntries)
	key := CompileKey{
		Host: "example.com", SchemaHash: catalogPageKey(t, 7000, base).SchemaHash,
		ContentProfile:    DefaultCompilerProfile,
		TemplateClusterID: templateClusterID(base), ClusterSimHash: base,
	}
	set, ready, err := catalog.Load(context.Background(), key, MaxSampleCatalogClusterEntries)
	if err != nil || !ready || set.Count != MaxSampleCatalogClusterEntries {
		t.Fatalf("Load(concurrent) = %d/%v/%v", set.Count, ready, err)
	}
	pages := make(map[string]struct{}, set.Count)
	snapshots := make(map[string]struct{}, set.Count)
	for _, sample := range set.Samples {
		pages[sample.PageHash] = struct{}{}
		snapshots[sample.SnapshotID] = struct{}{}
	}
	if len(pages) != set.Count || len(snapshots) != set.Count {
		t.Fatalf("concurrent set is not distinct: pages=%d snapshots=%d count=%d", len(pages), len(snapshots), set.Count)
	}
}

func TestSampleCatalogConcurrentRepeatedSnapshotsRemainBounded(t *testing.T) {
	now := catalogTestTime
	catalog, durable := openSampleCatalog(t, func() time.Time { return now })
	base := uint64(1) << 63
	const workers = 100
	pages := make([]PageKey, workers)
	for index := range pages {
		pages[index] = catalogPageKey(t, 7500+index, base)
	}
	snapshotID := catalogSnapshotID("shared-concurrent-content")
	start := make(chan struct{})
	errorsByWorker := make(chan error, workers)
	var group sync.WaitGroup
	for index := range pages {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			<-start
			_, _, err := catalog.Observe(context.Background(), pages[index], snapshotID,
				catalogTestTime.Add(time.Duration(index)*time.Second))
			if err != nil {
				errorsByWorker <- err
			}
		}(index)
	}
	close(start)
	group.Wait()
	close(errorsByWorker)
	for err := range errorsByWorker {
		t.Errorf("concurrent conflicting Observe() error = %v", err)
	}
	assertCatalogRowCount(t, durable, MaxSampleCatalogClusterEntries)
	key := CompileKey{
		Host: pages[0].Host, SchemaHash: pages[0].SchemaHash,
		ContentProfile:    DefaultCompilerProfile,
		TemplateClusterID: templateClusterID(base), ClusterSimHash: base,
	}
	set, ready, err := catalog.Load(context.Background(), key, MaxSampleCatalogClusterEntries)
	if err != nil || ready || set.Count != 1 ||
		set.Samples[0].PageHash != pages[workers-1].PageHash ||
		set.Samples[0].SnapshotID != snapshotID {
		t.Fatalf("deduplicated repeated snapshot set = %#v/%v/%v", set, ready, err)
	}
}

func TestSampleCatalogReaderFailsClosedOnOutOfClusterSample(t *testing.T) {
	catalog, durable := openSampleCatalog(t, func() time.Time { return catalogTestTime })
	cluster := uint64(1) << 63
	page := catalogPageKey(t, 8000, cluster)
	stamp := formatExtractorTime(catalogTestTime)
	if err := durable.Update(context.Background(), func(tx ledger.WriteTx) error {
		_, err := tx.ExecContext(context.Background(), `INSERT INTO compiler_samples (
			host, schema_hash, content_profile, template_cluster_id,
			cluster_simhash, sample_simhash, page_hash, snapshot_id,
			fetched_at, first_seen_at, last_seen_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			page.Host, page.SchemaHash, DefaultCompilerProfile, templateClusterID(cluster),
			encodeSimHash(cluster), encodeSimHash(cluster|0x7f), page.PageHash,
			catalogSnapshotID("corrupt-distance"), stamp, stamp, stamp)
		return err
	}); err != nil {
		t.Fatalf("insert direct out-of-cluster sample: %v", err)
	}
	key := CompileKey{
		Host: page.Host, SchemaHash: page.SchemaHash, ContentProfile: DefaultCompilerProfile,
		TemplateClusterID: templateClusterID(cluster), ClusterSimHash: cluster,
	}
	if _, _, err := catalog.Load(context.Background(), key, MinCompileSamples); !errors.Is(err, ErrSampleCatalogCorrupt) {
		t.Fatalf("Load(out-of-cluster) error = %v", err)
	}
}

func TestSampleCatalogReaderFailsClosedOnClusterIdentityMismatch(t *testing.T) {
	catalog, durable := openSampleCatalog(t, func() time.Time { return catalogTestTime })
	keyFingerprint := uint64(1) << 63
	storedFingerprint := keyFingerprint | 1
	page := catalogPageKey(t, 8100, keyFingerprint)
	stamp := formatExtractorTime(catalogTestTime)
	if err := durable.Update(context.Background(), func(tx ledger.WriteTx) error {
		_, err := tx.ExecContext(context.Background(), `INSERT INTO compiler_samples (
			host, schema_hash, content_profile, template_cluster_id,
			cluster_simhash, sample_simhash, page_hash, snapshot_id,
			fetched_at, first_seen_at, last_seen_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			page.Host, page.SchemaHash, DefaultCompilerProfile, templateClusterID(keyFingerprint),
			encodeSimHash(storedFingerprint), encodeSimHash(storedFingerprint), page.PageHash,
			catalogSnapshotID("corrupt-cluster-identity"), stamp, stamp, stamp)
		return err
	}); err != nil {
		t.Fatalf("insert mismatched cluster identity: %v", err)
	}
	key := CompileKey{
		Host: page.Host, SchemaHash: page.SchemaHash, ContentProfile: DefaultCompilerProfile,
		TemplateClusterID: templateClusterID(keyFingerprint), ClusterSimHash: keyFingerprint,
	}
	if _, _, err := catalog.Load(context.Background(), key, MinCompileSamples); !errors.Is(err, ErrSampleCatalogCorrupt) {
		t.Fatalf("Load(cluster identity mismatch) error = %v", err)
	}
}

func openSampleCatalog(t *testing.T, clock func() time.Time) (*SampleCatalog, *ledger.Store) {
	t.Helper()
	durable, err := ledger.Open(t.TempDir())
	if err != nil {
		t.Fatalf("ledger.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := durable.Close(); err != nil {
			t.Errorf("ledger.Close() error = %v", err)
		}
	})
	catalog, err := NewSampleCatalog(durable, clock)
	if err != nil {
		t.Fatalf("NewSampleCatalog() error = %v", err)
	}
	return catalog, durable
}

func catalogPageKey(t *testing.T, index int, fingerprint uint64) PageKey {
	t.Helper()
	key, err := BuildPageKey(fmt.Sprintf("https://shop.example.com/items/%d", index),
		catalogSchema(), `<html><body><main><h1>item</h1></main></body></html>`)
	if err != nil {
		t.Fatalf("BuildPageKey(%d) error = %v", index, err)
	}
	key.TemplateSimHash = fingerprint
	return key
}

func catalogSchema() json.RawMessage {
	return json.RawMessage(`{"name":"string"}`)
}

func catalogSnapshotID(label string) string {
	return "sha256:" + catalogDigest(label)
}

func catalogDigest(label string) string {
	digest := sha256.Sum256([]byte(label))
	return hex.EncodeToString(digest[:])
}

func assertCatalogRowCount(t *testing.T, durable *ledger.Store, want int) {
	t.Helper()
	var count int
	if err := durable.View(context.Background(), func(tx ledger.ReadTx) error {
		return tx.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM compiler_samples").Scan(&count)
	}); err != nil {
		t.Fatalf("count compiler_samples: %v", err)
	}
	if count != want {
		t.Fatalf("compiler_samples count = %d, want %d", count, want)
	}
}

func distantFingerprints(count int) []uint64 {
	values := make([]uint64, 0, count)
	state := uint64(0x9e3779b97f4a7c15)
	for len(values) < count {
		state += 0x9e3779b97f4a7c15
		candidate := state
		candidate = (candidate ^ (candidate >> 30)) * 0xbf58476d1ce4e5b9
		candidate = (candidate ^ (candidate >> 27)) * 0x94d049bb133111eb
		candidate ^= candidate >> 31
		if candidate == 0 {
			continue
		}
		separated := true
		for _, existing := range values {
			if simhash.Distance(existing, candidate) <= TemplateDistanceThreshold {
				separated = false
				break
			}
		}
		if separated {
			values = append(values, candidate)
		}
	}
	return values
}
