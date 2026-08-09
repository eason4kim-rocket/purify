package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOpenAppliesMigrationsIdempotently(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if got, want := store.Path(), filepath.Join(dir, Filename); got != want {
		t.Fatalf("Path() = %q, want %q", got, want)
	}
	assertMigrationState(t, store.db)
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	assertMigrationState(t, reopened.db)

	var count int
	if err := reopened.db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if count != len(migrations) {
		t.Fatalf("migration count = %d, want %d", count, len(migrations))
	}
	info, err := os.Stat(reopened.Path())
	if err != nil {
		t.Fatalf("database stat: %v", err)
	}
	if info.IsDir() {
		t.Fatal("database path is a directory")
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("database mode = %o, want 600", got)
	}
}

func TestConcurrentOpenAppliesMigrationOnce(t *testing.T) {
	dir := t.TempDir()
	const openers = 32
	stores := make(chan *Store, openers)
	errorsByOpener := make(chan error, openers)
	start := make(chan struct{})
	var group sync.WaitGroup
	for index := 0; index < openers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			store, err := Open(dir)
			if err != nil {
				errorsByOpener <- err
				return
			}
			stores <- store
		}()
	}
	close(start)
	group.Wait()
	close(stores)
	close(errorsByOpener)
	for err := range errorsByOpener {
		t.Fatalf("concurrent Open() error = %v", err)
	}
	var first *Store
	for store := range stores {
		if first == nil {
			first = store
		}
		t.Cleanup(func() { _ = store.Close() })
	}
	if first == nil {
		t.Fatal("no store opened")
	}
	var count int
	if err := first.db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if count != len(migrations) {
		t.Fatalf("migration count = %d, want %d", count, len(migrations))
	}
}

func TestOpenCoordinatorDoesNotSerializeDifferentPaths(t *testing.T) {
	var coordinator openCoordinator
	firstRelease := coordinator.lock("first")
	defer firstRelease()

	secondEntered := make(chan struct{})
	go func() {
		release := coordinator.lock("second")
		close(secondEntered)
		release()
	}()

	select {
	case <-secondEntered:
	case <-time.After(time.Second):
		t.Fatal("different database paths were serialized")
	}
}

func TestOpenReleasesCoordinatorAfterFailure(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, Filename)
	if err := os.Mkdir(dbPath, 0o700); err != nil {
		t.Fatalf("create invalid database directory: %v", err)
	}
	if store, err := Open(dir); err == nil {
		_ = store.Close()
		t.Fatal("Open() with directory database path succeeded")
	}
	if err := os.Remove(dbPath); err != nil {
		t.Fatalf("remove invalid database directory: %v", err)
	}

	opened := make(chan struct{})
	var (
		store *Store
		err   error
	)
	go func() {
		store, err = Open(dir)
		close(opened)
	}()
	select {
	case <-opened:
	case <-time.After(2 * time.Second):
		t.Fatal("Open() remained blocked after an earlier failure")
	}
	if err != nil {
		t.Fatalf("Open() after failure error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
}

func TestOpenConfiguresEveryConnection(t *testing.T) {
	store := openTestStore(t)
	connections := make([]*sql.Conn, 4)
	for index := range connections {
		conn, err := store.db.Conn(context.Background())
		if err != nil {
			t.Fatalf("Conn(%d): %v", index, err)
		}
		connections[index] = conn
	}
	t.Cleanup(func() {
		for _, conn := range connections {
			_ = conn.Close()
		}
	})

	for index, conn := range connections {
		assertPragma(t, conn, index, "busy_timeout", "5000")
		assertPragma(t, conn, index, "foreign_keys", "1")
		assertPragma(t, conn, index, "journal_mode", "wal")
	}
}

func TestRecordVerificationsRoundTrip(t *testing.T) {
	store := openTestStore(t)
	similarity := 0.8125
	verifiedAt := time.Date(2026, 8, 9, 11, 12, 13, 456000000, time.FixedZone("test", 8*60*60))
	rows := []Verification{
		{
			ID:                "row-a",
			VerificationID:    "verification-a",
			ClaimIndex:        0,
			URL:               "https://EXAMPLE.com/product",
			FinalURL:          "https://example.com/product/",
			Path:              "/price",
			OldValue:          json.RawMessage(`19.99`),
			NewValue:          json.RawMessage(`21.50`),
			Outcome:           OutcomeChanged,
			PageSimilarity:    &similarity,
			OldSnapshotID:     "sha256:old",
			NewSnapshotID:     "sha256:new",
			OldReceipt:        "old.receipt.token",
			Receipt:           "new.receipt.token",
			SchemaHash:        strings.Repeat("a", 64),
			TemplateClusterID: strings.Repeat("b", 64),
			ExtractorID:       "123e4567-e89b-12d3-a456-426614174000",
			VerifiedAt:        verifiedAt,
		},
		{
			ID:             "row-b",
			VerificationID: "verification-a",
			ClaimIndex:     1,
			URL:            "https://example.com/product",
			Path:           "/name",
			OldValue:       json.RawMessage(`"Purify"`),
			NewValue:       json.RawMessage(`"Purify"`),
			Outcome:        OutcomeConfirmed,
			PageSimilarity: &similarity,
			OldSnapshotID:  "sha256:old",
			NewSnapshotID:  "sha256:new",
			VerifiedAt:     verifiedAt,
		},
	}
	if err := store.RecordVerifications(context.Background(), rows); err != nil {
		t.Fatalf("RecordVerifications() error = %v", err)
	}

	var (
		gotHost, gotOld, gotNew, gotOutcome, gotTime string
		gotSimilarity                                float64
	)
	if err := store.db.QueryRow(`SELECT host, old_value, new_value, outcome,
		page_similarity, verified_at FROM verifications WHERE id = 'row-a'`).Scan(
		&gotHost, &gotOld, &gotNew, &gotOutcome, &gotSimilarity, &gotTime,
	); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if gotHost != "example.com" || gotOld != "19.99" || gotNew != "21.50" || gotOutcome != "changed" {
		t.Fatalf("unexpected row: host=%q old=%q new=%q outcome=%q", gotHost, gotOld, gotNew, gotOutcome)
	}
	if gotSimilarity != similarity {
		t.Fatalf("page_similarity = %v, want %v", gotSimilarity, similarity)
	}
	if gotTime != verifiedAt.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("verified_at = %q, want %q", gotTime, verifiedAt.UTC().Format(time.RFC3339Nano))
	}
}

func TestRecordVerificationsRollsBackWholeRequest(t *testing.T) {
	store := openTestStore(t)
	first := validVerification(0)
	first.ID = "duplicate-row-id"
	second := validVerification(1)
	second.ID = first.ID

	err := store.RecordVerifications(context.Background(), []Verification{first, second})
	if err == nil {
		t.Fatal("RecordVerifications() error = nil, want primary-key failure")
	}
	var count int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM verifications").Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 0 {
		t.Fatalf("row count = %d, want atomic rollback to zero", count)
	}
}

func TestRecordVerificationsSerializesOneHundredConcurrentWrites(t *testing.T) {
	store := openTestStore(t)
	const workers = 100
	start := make(chan struct{})
	errorsByWorker := make(chan error, workers)
	var group sync.WaitGroup
	for index := 0; index < workers; index++ {
		index := index
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			row := validVerification(0)
			row.VerificationID = fmt.Sprintf("verification-%03d", index)
			errorsByWorker <- store.RecordVerifications(context.Background(), []Verification{row})
		}()
	}
	close(start)
	group.Wait()
	close(errorsByWorker)
	for err := range errorsByWorker {
		if err != nil {
			t.Fatalf("concurrent RecordVerifications() error = %v", err)
		}
	}
	var count int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM verifications").Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != workers {
		t.Fatalf("row count = %d, want %d", count, workers)
	}
}

func TestRecordVerificationsValidation(t *testing.T) {
	valid := validVerification(0)
	nan := math.NaN()
	overOne := 1.01
	cases := []struct {
		name    string
		mutate  func(*Verification)
		wantErr error
	}{
		{"missing verification id", func(v *Verification) { v.VerificationID = "" }, ErrInvalidVerification},
		{"negative claim index", func(v *Verification) { v.ClaimIndex = -1 }, ErrInvalidVerification},
		{"relative url", func(v *Verification) { v.URL = "/relative" }, ErrInvalidVerification},
		{"missing path", func(v *Verification) { v.Path = "" }, ErrInvalidVerification},
		{"bad old json", func(v *Verification) { v.OldValue = json.RawMessage(`{`) }, ErrInvalidVerification},
		{"bad new json", func(v *Verification) { v.NewValue = json.RawMessage(`{`) }, ErrInvalidVerification},
		{"missing old snapshot", func(v *Verification) { v.OldSnapshotID = "" }, ErrInvalidVerification},
		{"missing time", func(v *Verification) { v.VerifiedAt = time.Time{} }, ErrInvalidVerification},
		{"unknown outcome", func(v *Verification) { v.Outcome = "unknown" }, ErrUnsupportedOutcome},
		{"changed without new value", func(v *Verification) { v.Outcome = OutcomeChanged; v.NewValue = nil }, ErrInvalidVerification},
		{"gone without scope", func(v *Verification) { v.Outcome = OutcomeGone; v.NewValue = nil }, ErrInvalidVerification},
		{"gone with new value", func(v *Verification) { v.Outcome = OutcomeGone; v.GoneScope = GoneScopeField }, ErrInvalidVerification},
		{"scope on confirmed", func(v *Verification) { v.GoneScope = GoneScopeField }, ErrInvalidVerification},
		{"similarity nan", func(v *Verification) { v.PageSimilarity = &nan }, ErrInvalidVerification},
		{"similarity too high", func(v *Verification) { v.PageSimilarity = &overOne }, ErrInvalidVerification},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			row := valid
			test.mutate(&row)
			store := openTestStore(t)
			err := store.RecordVerifications(context.Background(), []Verification{row})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want errors.Is(%v)", err, test.wantErr)
			}
		})
	}
}

func TestRecordVerificationBatchRequiresCompleteExtractorProvenance(t *testing.T) {
	const extractorID = "123e4567-e89b-12d3-a456-426614174000"
	schemaHash := strings.Repeat("a", 64)
	clusterID := strings.Repeat("b", 64)

	for mask := 0; mask < 8; mask++ {
		t.Run(fmt.Sprintf("presence mask %03b", mask), func(t *testing.T) {
			store := openTestStore(t)
			row := validVerification(0)
			if mask&1 != 0 {
				row.SchemaHash = schemaHash
			}
			if mask&2 != 0 {
				row.TemplateClusterID = clusterID
			}
			if mask&4 != 0 {
				row.ExtractorID = extractorID
			}

			err := store.RecordVerificationBatch(context.Background(), []Verification{row}, nil)
			if mask == 0 || mask == 7 {
				if err != nil {
					t.Fatalf("RecordVerificationBatch() error = %v", err)
				}
				assertStoredVerificationCount(t, store, 1)
				return
			}
			if !errors.Is(err, ErrInvalidVerification) {
				t.Fatalf("RecordVerificationBatch() error = %v, want ErrInvalidVerification", err)
			}
			assertStoredVerificationCount(t, store, 0)
		})
	}
}

func TestRecordVerificationBatchRejectsInvalidExtractorProvenanceWithoutWriting(t *testing.T) {
	validSchemaHash := strings.Repeat("a", 64)
	validClusterID := strings.Repeat("b", 64)
	const validExtractorID = "123e4567-e89b-12d3-a456-426614174000"
	tests := []struct {
		name              string
		schemaHash        string
		templateClusterID string
		extractorID       string
	}{
		{name: "schema surrounding whitespace", schemaHash: " " + validSchemaHash, templateClusterID: validClusterID, extractorID: validExtractorID},
		{name: "whitespace-only partial provenance", schemaHash: " ", templateClusterID: "", extractorID: ""},
		{name: "cluster surrounding whitespace", schemaHash: validSchemaHash, templateClusterID: validClusterID + "\n", extractorID: validExtractorID},
		{name: "extractor surrounding whitespace", schemaHash: validSchemaHash, templateClusterID: validClusterID, extractorID: "\t" + validExtractorID},
		{name: "schema uppercase", schemaHash: strings.Repeat("A", 64), templateClusterID: validClusterID, extractorID: validExtractorID},
		{name: "cluster not digest", schemaHash: validSchemaHash, templateClusterID: "cluster", extractorID: validExtractorID},
		{name: "extractor uppercase", schemaHash: validSchemaHash, templateClusterID: validClusterID, extractorID: strings.ToUpper(validExtractorID)},
		{name: "extractor not uuid", schemaHash: validSchemaHash, templateClusterID: validClusterID, extractorID: "extractor"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			row := validVerification(0)
			row.SchemaHash = test.schemaHash
			row.TemplateClusterID = test.templateClusterID
			row.ExtractorID = test.extractorID
			if err := store.RecordVerificationBatch(context.Background(), []Verification{row}, nil); !errors.Is(err, ErrInvalidVerification) {
				t.Fatalf("RecordVerificationBatch() error = %v, want ErrInvalidVerification", err)
			}
			assertStoredVerificationCount(t, store, 0)
		})
	}

	t.Run("invalid row rejects whole batch", func(t *testing.T) {
		store := openTestStore(t)
		first := validVerification(0)
		first.SchemaHash = validSchemaHash
		first.TemplateClusterID = validClusterID
		first.ExtractorID = validExtractorID
		second := validVerification(1)
		second.SchemaHash = validSchemaHash
		if err := store.RecordVerificationBatch(context.Background(), []Verification{first, second}, nil); !errors.Is(err, ErrInvalidVerification) {
			t.Fatalf("RecordVerificationBatch() error = %v, want ErrInvalidVerification", err)
		}
		assertStoredVerificationCount(t, store, 0)
	})
}

func assertStoredVerificationCount(t *testing.T, store *Store, want int) {
	t.Helper()
	var count int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM verifications").Scan(&count); err != nil {
		t.Fatalf("count verifications: %v", err)
	}
	if count != want {
		t.Fatalf("verification row count = %d, want %d", count, want)
	}
}

func TestRecordVerificationsRequiresOneRequest(t *testing.T) {
	store := openTestStore(t)
	first := validVerification(0)
	second := validVerification(1)
	second.VerificationID = "another-verification"
	if err := store.RecordVerifications(context.Background(), []Verification{first, second}); !errors.Is(err, ErrInvalidVerification) {
		t.Fatalf("different verification IDs error = %v", err)
	}

	second = validVerification(0)
	if err := store.RecordVerifications(context.Background(), []Verification{first, second}); !errors.Is(err, ErrInvalidVerification) {
		t.Fatalf("duplicate claim indexes error = %v", err)
	}
}

func TestCloseIsConcurrentSafeAndRejectsNewWrites(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	const workers = 32
	start := make(chan struct{})
	var group sync.WaitGroup
	var unexpected atomic.Value
	for index := 0; index < workers; index++ {
		index := index
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			row := validVerification(0)
			row.VerificationID = fmt.Sprintf("close-race-%d", index)
			err := store.RecordVerifications(context.Background(), []Verification{row})
			if err != nil && !errors.Is(err, ErrClosed) {
				unexpected.Store(err)
			}
		}()
	}
	close(start)
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	group.Wait()
	if value := unexpected.Load(); value != nil {
		t.Fatalf("unexpected concurrent error: %v", value)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if err := store.RecordVerifications(context.Background(), []Verification{validVerification(0)}); !errors.Is(err, ErrClosed) {
		t.Fatalf("write after Close error = %v, want ErrClosed", err)
	}
}

func TestRecordVerificationsHonorsCanceledContext(t *testing.T) {
	store := openTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.RecordVerifications(ctx, []Verification{validVerification(0)}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context error = %v", err)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return store
}

func validVerification(claimIndex int) Verification {
	similarity := 0.9
	return Verification{
		VerificationID: "verification-valid",
		ClaimIndex:     claimIndex,
		URL:            "https://example.com/item",
		Path:           fmt.Sprintf("/field/%d", claimIndex),
		OldValue:       json.RawMessage(`"old"`),
		NewValue:       json.RawMessage(`"old"`),
		Outcome:        OutcomeConfirmed,
		PageSimilarity: &similarity,
		OldSnapshotID:  "sha256:old",
		NewSnapshotID:  "sha256:new",
		VerifiedAt:     time.Now().UTC(),
	}
}

func assertMigrationState(t *testing.T, db *sql.DB) {
	t.Helper()
	var table string
	if err := db.QueryRow(`SELECT name FROM sqlite_master
		WHERE type = 'table' AND name = 'verifications'`).Scan(&table); err != nil {
		t.Fatalf("verifications table: %v", err)
	}
	wantIndexes := []string{
		"idx_verifications_url_time",
		"idx_verifications_host_time",
		"idx_verifications_old_snapshot",
		"idx_verifications_new_snapshot",
		"idx_verifications_outcome_time",
		"idx_verifications_replay",
	}
	for _, name := range wantIndexes {
		var got string
		if err := db.QueryRow(`SELECT name FROM sqlite_master
			WHERE type = 'index' AND name = ?`, name).Scan(&got); err != nil {
			t.Fatalf("index %q: %v", name, err)
		}
	}
	if err := db.QueryRow(`SELECT name FROM sqlite_master
		WHERE type = 'table' AND name = 'outbox_events'`).Scan(&table); err != nil {
		t.Fatalf("outbox_events table: %v", err)
	}
	for _, name := range []string{"idx_outbox_events_pending", "idx_outbox_events_verification"} {
		var got string
		if err := db.QueryRow(`SELECT name FROM sqlite_master
			WHERE type = 'index' AND name = ?`, name).Scan(&got); err != nil {
			t.Fatalf("index %q: %v", name, err)
		}
	}
	for _, name := range []string{"extractors", "extractor_page_bindings"} {
		if err := db.QueryRow(`SELECT name FROM sqlite_master
			WHERE type = 'table' AND name = ?`, name).Scan(&table); err != nil {
			t.Fatalf("table %q: %v", name, err)
		}
	}
	for _, name := range []string{
		"idx_extractors_active_cluster",
		"idx_extractors_lookup",
		"idx_extractors_ir_hash",
		"idx_extractor_page_bindings_extractor",
	} {
		var got string
		if err := db.QueryRow(`SELECT name FROM sqlite_master
			WHERE type = 'index' AND name = ?`, name).Scan(&got); err != nil {
			t.Fatalf("index %q: %v", name, err)
		}
	}
	for _, name := range []string{
		"trg_extractors_state_transition",
		"trg_extractors_immutable_revision",
		"trg_extractors_delete_retired_only",
		"trg_extractors_empty_window_insert",
		"trg_extractors_empty_window_update",
	} {
		var got string
		if err := db.QueryRow(`SELECT name FROM sqlite_master
			WHERE type = 'trigger' AND name = ?`, name).Scan(&got); err != nil {
			t.Fatalf("trigger %q: %v", name, err)
		}
	}
}

type pragmaQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func assertPragma(t *testing.T, conn pragmaQuerier, index int, name, want string) {
	t.Helper()
	var got string
	if err := conn.QueryRowContext(context.Background(), "PRAGMA "+name).Scan(&got); err != nil {
		t.Fatalf("connection %d PRAGMA %s: %v", index, name, err)
	}
	if got != want {
		t.Fatalf("connection %d PRAGMA %s = %q, want %q", index, name, got, want)
	}
}
