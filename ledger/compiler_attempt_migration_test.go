package ledger

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type compilerAttemptMigrationRow struct {
	host, schemaHash, profile, clusterID, clusterHash any
	attemptedRevision, outcome, reason, cooldown      any
	leaseID, leaseRevision, leaseUntil                any
	createdAt, updatedAt                              any
}

func TestCompilerAttemptMigrationIsStrictStateOnly(t *testing.T) {
	store := openTestStore(t)
	rows, err := store.db.Query("PRAGMA table_info(compiler_attempts)")
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
	want := []string{
		"host", "schema_hash", "content_profile", "template_cluster_id", "cluster_simhash",
		"attempted_revision", "outcome", "reason", "cooldown_until",
		"lease_id", "lease_revision", "lease_until", "created_at", "updated_at",
	}
	if !reflect.DeepEqual(columns, want) {
		t.Fatalf("compiler_attempts columns = %v, want %v", columns, want)
	}
	var definition string
	if err := store.db.QueryRow(`SELECT sql FROM sqlite_master
		WHERE type = 'table' AND name = 'compiler_attempts'`).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(definition)
	if !strings.Contains(lower, "strict") || !strings.Contains(lower, "without rowid") {
		t.Fatalf("compiler_attempts is not strict/without-rowid: %s", definition)
	}
	for _, forbidden := range []string{
		"schema_json", "url", "page_hash", "snapshot", "html", "content", "llm",
		"output", "api_key", "model", "base_url", "header", "cookie", "proxy",
	} {
		for _, column := range columns {
			if column == forbidden {
				t.Fatalf("compiler_attempts persists forbidden column %q", column)
			}
		}
	}
	for _, object := range []string{
		"idx_compiler_attempts_cooldown", "idx_compiler_attempts_lease",
		"idx_compiler_attempts_updated", "trg_compiler_attempts_identity_immutable",
		"trg_compiler_attempts_updated_monotonic",
	} {
		var name string
		if err := store.db.QueryRow("SELECT name FROM sqlite_master WHERE name = ?", object).Scan(&name); err != nil {
			t.Fatalf("schema object %q: %v", object, err)
		}
	}
}

func TestCompilerAttemptMigrationRejectsMalformedState(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*compilerAttemptMigrationRow)
	}{
		{"zero cluster", func(row *compilerAttemptMigrationRow) { row.clusterHash = make([]byte, 8) }},
		{"wrong profile", func(row *compilerAttemptMigrationRow) { row.profile = "custom" }},
		{"uppercase revision", func(row *compilerAttemptMigrationRow) { row.leaseRevision = strings.Repeat("A", 64) }},
		{"partial lease", func(row *compilerAttemptMigrationRow) { row.leaseUntil = nil }},
		{"expired lease", func(row *compilerAttemptMigrationRow) { row.leaseUntil = row.updatedAt }},
		{"result without revision", func(row *compilerAttemptMigrationRow) {
			row.leaseID, row.leaseRevision, row.leaseUntil = nil, nil, nil
			row.outcome, row.reason = "success", "compiled"
		}},
		{"freeform reason", func(row *compilerAttemptMigrationRow) {
			row.leaseID, row.leaseRevision, row.leaseUntil = nil, nil, nil
			row.attemptedRevision, row.outcome, row.reason = repeatHex("d"), "transient", "dial tcp secret"
			row.cooldown = extractorMigrationLaterTime
		}},
		{"cooldown not future", func(row *compilerAttemptMigrationRow) {
			row.leaseID, row.leaseRevision, row.leaseUntil = nil, nil, nil
			row.attemptedRevision, row.outcome, row.reason = repeatHex("d"), "transient", "compile_failed"
			row.cooldown = row.updatedAt
		}},
	}
	for index, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			row := validCompilerAttemptMigrationRow(index + 1)
			test.mutate(&row)
			var insertErr error
			if err := store.Update(context.Background(), func(tx WriteTx) error {
				insertErr = insertCompilerAttemptMigrationRow(context.Background(), tx, row)
				return nil
			}); err != nil {
				t.Fatalf("Update() wrapper error = %v", err)
			}
			if insertErr == nil {
				t.Fatal("malformed compiler attempt insert succeeded")
			}
		})
	}
}

func TestCompilerAttemptMigrationIdentityAndTimeAreMonotonic(t *testing.T) {
	store := openTestStore(t)
	row := validCompilerAttemptMigrationRow(50)
	if err := store.Update(context.Background(), func(tx WriteTx) error {
		return insertCompilerAttemptMigrationRow(context.Background(), tx, row)
	}); err != nil {
		t.Fatalf("insert attempt: %v", err)
	}

	updates := []struct {
		name  string
		query string
		arg   any
	}{
		{"host", "UPDATE compiler_attempts SET host = ?", "other.example"},
		{"schema", "UPDATE compiler_attempts SET schema_hash = ?", repeatHex("f")},
		{"cluster", "UPDATE compiler_attempts SET cluster_simhash = ?", []byte{1, 2, 3, 4, 5, 6, 7, 8}},
		{"created", "UPDATE compiler_attempts SET created_at = ?", extractorMigrationLaterTime},
		{"clock rollback", "UPDATE compiler_attempts SET updated_at = ?", "2026-08-08T23:59:59.000000000Z"},
	}
	for _, test := range updates {
		t.Run(test.name, func(t *testing.T) {
			var updateErr error
			if err := store.Update(context.Background(), func(tx WriteTx) error {
				_, updateErr = tx.ExecContext(context.Background(), test.query, test.arg)
				return nil
			}); err != nil {
				t.Fatalf("Update() wrapper error = %v", err)
			}
			if updateErr == nil {
				t.Fatal("guarded update succeeded")
			}
		})
	}

	if err := store.Update(context.Background(), func(tx WriteTx) error {
		_, err := tx.ExecContext(context.Background(), `UPDATE compiler_attempts SET
			attempted_revision = ?, outcome = 'success', reason = 'compiled', cooldown_until = NULL,
			lease_id = NULL, lease_revision = NULL, lease_until = NULL, updated_at = ?`,
			repeatHex("e"), extractorMigrationLaterTime)
		return err
	}); err != nil {
		t.Fatalf("valid finalization: %v", err)
	}
}

func TestMigration005UpgradesVersionFourDatabaseAndReopens(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, Filename))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 4; index++ {
		if _, err := db.Exec(migrations[index]); err != nil {
			t.Fatalf("apply migration %03d: %v", index+1, err)
		}
		if _, err := db.Exec("INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)",
			index+1, extractorMigrationTime); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(upgrade) error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(repeat) error = %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	var versions, tables int
	if err := reopened.db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'compiler_attempts'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if versions != len(migrations) || tables != 1 {
		t.Fatalf("upgrade state migrations=%d/%d tables=%d", versions, len(migrations), tables)
	}
}

func validCompilerAttemptMigrationRow(index int) compilerAttemptMigrationRow {
	cluster := []byte{0x80, 0, 0, 0, 0, 0, 0, byte(index + 1)}
	return compilerAttemptMigrationRow{
		host:              "example.com",
		schemaHash:        repeatHex("a"),
		profile:           "extract-default-v1",
		clusterID:         compilerMigrationDigestBytes(cluster),
		clusterHash:       cluster,
		attemptedRevision: "",
		outcome:           "",
		reason:            "",
		leaseID:           "00000000-0000-4000-8000-000000000001",
		leaseRevision:     repeatHex("b"),
		leaseUntil:        extractorMigrationLaterTime,
		createdAt:         extractorMigrationTime,
		updatedAt:         extractorMigrationTime,
	}
}

func insertCompilerAttemptMigrationRow(ctx context.Context, tx WriteTx, row compilerAttemptMigrationRow) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO compiler_attempts (
		host, schema_hash, content_profile, template_cluster_id, cluster_simhash,
		attempted_revision, outcome, reason, cooldown_until,
		lease_id, lease_revision, lease_until, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.host, row.schemaHash, row.profile, row.clusterID, row.clusterHash,
		row.attemptedRevision, row.outcome, row.reason, row.cooldown,
		row.leaseID, row.leaseRevision, row.leaseUntil, row.createdAt, row.updatedAt)
	return err
}
