package ledger

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type compilerSampleMigrationRow struct {
	host, schemaHash, profile, clusterID any
	clusterHash, sampleHash, pageHash    any
	snapshotID, fetchedAt, firstSeenAt   any
	lastSeenAt                           any
}

func TestCompilerSampleMigrationIsStrictRefsOnly(t *testing.T) {
	store := openTestStore(t)
	rows, err := store.db.Query("PRAGMA table_info(compiler_samples)")
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table_info: %v", err)
	}
	want := []string{
		"host", "schema_hash", "content_profile", "template_cluster_id",
		"cluster_simhash", "sample_simhash", "page_hash", "snapshot_id", "fetched_at",
		"first_seen_at", "last_seen_at",
	}
	if !reflect.DeepEqual(columns, want) {
		t.Fatalf("compiler_samples columns = %v, want refs-only %v", columns, want)
	}

	var definition string
	if err := store.db.QueryRow(`SELECT sql FROM sqlite_master
		WHERE type = 'table' AND name = 'compiler_samples'`).Scan(&definition); err != nil {
		t.Fatalf("read table definition: %v", err)
	}
	lowerDefinition := strings.ToLower(definition)
	if !strings.Contains(lowerDefinition, "strict") || !strings.Contains(lowerDefinition, "without rowid") {
		t.Fatalf("compiler_samples is not strict/without-rowid: %s", definition)
	}
	for _, forbidden := range []string{
		"source_url", "schema_json", "raw_html", "cleaned_content", "llm",
		"truth", "output", "api_key", "base_url", "header", "cookie", "proxy",
	} {
		if strings.Contains(lowerDefinition, forbidden) {
			t.Fatalf("compiler_samples persists forbidden field %q: %s", forbidden, definition)
		}
	}
	for _, name := range []string{
		"idx_compiler_samples_compile",
		"idx_compiler_samples_cluster",
		"trg_compiler_samples_fixed_cluster_insert",
		"trg_compiler_samples_identity_immutable",
		"trg_compiler_samples_last_seen_monotonic",
	} {
		var got string
		if err := store.db.QueryRow("SELECT name FROM sqlite_master WHERE name = ?", name).Scan(&got); err != nil {
			t.Fatalf("schema object %q: %v", name, err)
		}
	}
}

func TestCompilerSampleMigrationRejectsMalformedAndDuplicateRows(t *testing.T) {
	store := openTestStore(t)
	cases := []struct {
		name   string
		mutate func(*compilerSampleMigrationRow)
	}{
		{"uppercase host", func(row *compilerSampleMigrationRow) { row.host = "EXAMPLE.COM" }},
		{"space-padded host", func(row *compilerSampleMigrationRow) { row.host = " example.com" }},
		{"uppercase schema hash", func(row *compilerSampleMigrationRow) { row.schemaHash = strings.Repeat("A", 64) }},
		{"wrong profile", func(row *compilerSampleMigrationRow) { row.profile = "extract-custom-v1" }},
		{"short cluster ID", func(row *compilerSampleMigrationRow) { row.clusterID = "abc" }},
		{"zero cluster simhash", func(row *compilerSampleMigrationRow) { row.clusterHash = make([]byte, 8) }},
		{"integer cluster simhash", func(row *compilerSampleMigrationRow) { row.clusterHash = 1 }},
		{"zero sample simhash", func(row *compilerSampleMigrationRow) { row.sampleHash = make([]byte, 8) }},
		{"integer sample simhash", func(row *compilerSampleMigrationRow) { row.sampleHash = 1 }},
		{"uppercase page hash", func(row *compilerSampleMigrationRow) { row.pageHash = strings.Repeat("F", 64) }},
		{"snapshot without prefix", func(row *compilerSampleMigrationRow) { row.snapshotID = compilerMigrationDigest("snapshot") }},
		{"uppercase snapshot", func(row *compilerSampleMigrationRow) { row.snapshotID = "sha256:" + strings.Repeat("F", 64) }},
		{"offset fetched time", func(row *compilerSampleMigrationRow) { row.fetchedAt = "2026-08-09T08:00:00.000000000+08:00" }},
		{"malformed first seen", func(row *compilerSampleMigrationRow) { row.firstSeenAt = "now" }},
		{"last seen before first", func(row *compilerSampleMigrationRow) {
			row.firstSeenAt = extractorMigrationLaterTime
			row.lastSeenAt = extractorMigrationTime
		}},
	}
	for index, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			row := validCompilerSampleMigrationRow(index + 1)
			test.mutate(&row)
			var insertErr error
			if err := store.Update(context.Background(), func(tx WriteTx) error {
				insertErr = insertCompilerSampleMigrationRow(context.Background(), tx, row)
				return nil
			}); err != nil {
				t.Fatalf("Update() error = %v", err)
			}
			if insertErr == nil {
				t.Fatal("malformed compiler sample insert succeeded")
			}
		})
	}

	first := validCompilerSampleMigrationRow(100)
	if err := store.Update(context.Background(), func(tx WriteTx) error {
		return insertCompilerSampleMigrationRow(context.Background(), tx, first)
	}); err != nil {
		t.Fatalf("insert valid row: %v", err)
	}

	duplicatePage := validCompilerSampleMigrationRow(101)
	duplicatePage.pageHash = first.pageHash
	assertCompilerSampleInsertRejected(t, store, duplicatePage, "duplicate page")

	duplicateSnapshot := validCompilerSampleMigrationRow(102)
	duplicateSnapshot.snapshotID = first.snapshotID
	duplicateSnapshot.clusterID = first.clusterID
	duplicateSnapshot.clusterHash = first.clusterHash
	duplicateSnapshot.sampleHash = first.sampleHash
	if err := store.Update(context.Background(), func(tx WriteTx) error {
		return insertCompilerSampleMigrationRow(context.Background(), tx, duplicateSnapshot)
	}); err != nil {
		t.Fatalf("same snapshot on a distinct page: %v", err)
	}

	differentRepresentative := validCompilerSampleMigrationRow(103)
	differentRepresentative.clusterID = first.clusterID
	assertCompilerSampleInsertRejected(t, store, differentRepresentative, "changed cluster representative")

	differentID := validCompilerSampleMigrationRow(104)
	differentID.clusterHash = first.clusterHash
	assertCompilerSampleInsertRejected(t, store, differentID, "changed representative cluster ID")
}

func TestCompilerSampleMigrationIdentityIsImmutableForSoleRow(t *testing.T) {
	store := openTestStore(t)
	row := validCompilerSampleMigrationRow(200)
	if err := store.Update(context.Background(), func(tx WriteTx) error {
		return insertCompilerSampleMigrationRow(context.Background(), tx, row)
	}); err != nil {
		t.Fatalf("insert valid row: %v", err)
	}

	otherClusterHash := []byte{0x40, 0, 0, 0, 0, 0, 0, 1}
	updates := []struct {
		name  string
		query string
		args  []any
	}{
		{"host", "UPDATE compiler_samples SET host = ? WHERE page_hash = ?", []any{"other.example", row.pageHash}},
		{"schema", "UPDATE compiler_samples SET schema_hash = ? WHERE page_hash = ?", []any{compilerMigrationDigest("other-schema"), row.pageHash}},
		{"cluster pair", "UPDATE compiler_samples SET template_cluster_id = ?, cluster_simhash = ? WHERE page_hash = ?", []any{compilerMigrationDigestBytes(otherClusterHash), otherClusterHash, row.pageHash}},
		{"sample simhash", "UPDATE compiler_samples SET sample_simhash = ? WHERE page_hash = ?", []any{otherClusterHash, row.pageHash}},
		{"page hash", "UPDATE compiler_samples SET page_hash = ? WHERE page_hash = ?", []any{compilerMigrationDigest("other-page"), row.pageHash}},
		{"snapshot", "UPDATE compiler_samples SET snapshot_id = ? WHERE page_hash = ?", []any{"sha256:" + compilerMigrationDigest("other-snapshot"), row.pageHash}},
		{"fetched time", "UPDATE compiler_samples SET fetched_at = ? WHERE page_hash = ?", []any{extractorMigrationLaterTime, row.pageHash}},
		{"first seen", "UPDATE compiler_samples SET first_seen_at = ? WHERE page_hash = ?", []any{"2026-08-08T00:00:00.000000000Z", row.pageHash}},
	}
	for _, test := range updates {
		t.Run(test.name, func(t *testing.T) {
			var updateErr error
			if err := store.Update(context.Background(), func(tx WriteTx) error {
				_, updateErr = tx.ExecContext(context.Background(), test.query, test.args...)
				return nil
			}); err != nil {
				t.Fatalf("Update() wrapper error = %v", err)
			}
			if updateErr == nil {
				t.Fatal("identity update succeeded")
			}
		})
	}

	if err := store.Update(context.Background(), func(tx WriteTx) error {
		_, err := tx.ExecContext(context.Background(),
			"UPDATE compiler_samples SET last_seen_at = ? WHERE page_hash = ?",
			extractorMigrationLaterTime, row.pageHash)
		return err
	}); err != nil {
		t.Fatalf("advance last_seen_at: %v", err)
	}
	var rollbackErr error
	if err := store.Update(context.Background(), func(tx WriteTx) error {
		_, rollbackErr = tx.ExecContext(context.Background(),
			"UPDATE compiler_samples SET last_seen_at = ? WHERE page_hash = ?",
			extractorMigrationTime, row.pageHash)
		return nil
	}); err != nil {
		t.Fatalf("rollback Update() wrapper error = %v", err)
	}
	if rollbackErr == nil {
		t.Fatal("last_seen_at moved backward")
	}
}

func TestMigration004UpgradesVersionThreeDatabaseAndReopens(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, Filename))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	for index := 0; index < 3; index++ {
		if _, err := db.Exec(migrations[index]); err != nil {
			t.Fatalf("apply migration %03d: %v", index+1, err)
		}
		if _, err := db.Exec("INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)",
			index+1, extractorMigrationTime); err != nil {
			t.Fatalf("record migration %03d: %v", index+1, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close version-three database: %v", err)
	}

	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(upgrade) error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close(upgraded) error = %v", err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(repeat) error = %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	var versionCount, tableCount int
	if err := reopened.db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&versionCount); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'compiler_samples'`).Scan(&tableCount); err != nil {
		t.Fatalf("count compiler_samples table: %v", err)
	}
	if versionCount != len(migrations) || tableCount != 1 {
		t.Fatalf("upgrade state = migrations %d/%d, compiler_samples tables %d", versionCount, len(migrations), tableCount)
	}
}

func validCompilerSampleMigrationRow(index int) compilerSampleMigrationRow {
	clusterHash := []byte{0x80, 0, 0, 0, 0, 0, 0, byte(index%250 + 1)}
	return compilerSampleMigrationRow{
		host:        "example.com",
		schemaHash:  compilerMigrationDigest("schema"),
		profile:     "extract-default-v1",
		clusterID:   compilerMigrationDigestBytes(clusterHash),
		clusterHash: clusterHash,
		sampleHash:  append([]byte(nil), clusterHash...),
		pageHash:    compilerMigrationDigest("page-" + sqlIntString(index)),
		snapshotID:  "sha256:" + compilerMigrationDigest("snapshot-"+sqlIntString(index)),
		fetchedAt:   extractorMigrationTime,
		firstSeenAt: extractorMigrationTime,
		lastSeenAt:  extractorMigrationTime,
	}
}

func insertCompilerSampleMigrationRow(ctx context.Context, tx WriteTx, row compilerSampleMigrationRow) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO compiler_samples (
		host, schema_hash, content_profile, template_cluster_id,
		cluster_simhash, sample_simhash, page_hash, snapshot_id, fetched_at,
		first_seen_at, last_seen_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.host, row.schemaHash, row.profile, row.clusterID, row.clusterHash, row.sampleHash,
		row.pageHash, row.snapshotID, row.fetchedAt, row.firstSeenAt, row.lastSeenAt)
	return err
}

func assertCompilerSampleInsertRejected(t *testing.T, store *Store, row compilerSampleMigrationRow, label string) {
	t.Helper()
	var insertErr error
	if err := store.Update(context.Background(), func(tx WriteTx) error {
		insertErr = insertCompilerSampleMigrationRow(context.Background(), tx, row)
		return nil
	}); err != nil {
		t.Fatalf("%s Update() error = %v", label, err)
	}
	if insertErr == nil {
		t.Fatalf("%s insert succeeded", label)
	}
}

func compilerMigrationDigest(value string) string {
	return compilerMigrationDigestBytes([]byte(value))
}

func compilerMigrationDigestBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
