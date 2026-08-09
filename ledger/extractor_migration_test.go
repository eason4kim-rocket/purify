package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

const (
	extractorMigrationTime      = "2026-08-09T00:00:00.000000000Z"
	extractorMigrationLaterTime = "2026-08-09T00:00:01.000000000Z"
)

type extractorMigrationRow struct {
	id, host, schemaJSON, schemaHash, clusterID any
	templateHash, ir, irHash                    any
	irVersion, version                          any
	report, validation, state, window, reason   any
	createdAt, lastUsedAt, updatedAt            any
}

func TestExtractorMigrationRejectsMalformedRows(t *testing.T) {
	store := openTestStore(t)
	cases := []struct {
		name   string
		mutate func(*extractorMigrationRow)
	}{
		{"null id", func(row *extractorMigrationRow) { row.id = nil }},
		{"extra UUID hyphen", func(row *extractorMigrationRow) { row.id = "00000000-0000-4000-8000-00000000000-" }},
		{"uppercase host", func(row *extractorMigrationRow) { row.host = "EXAMPLE.COM" }},
		{"space-padded host", func(row *extractorMigrationRow) { row.host = " example.com" }},
		{"blob host", func(row *extractorMigrationRow) { row.host = []byte("example.com") }},
		{"blob schema hash", func(row *extractorMigrationRow) { row.schemaHash = []byte(repeatHex("a")) }},
		{"blob cluster", func(row *extractorMigrationRow) { row.clusterID = []byte(repeatHex("b")) }},
		{"fractional version", func(row *extractorMigrationRow) { row.version = 1.5 }},
		{"zero simhash", func(row *extractorMigrationRow) { row.templateHash = make([]byte, 8) }},
		{"missing active gate", func(row *extractorMigrationRow) { row.report = `{}` }},
		{"false active gate", func(row *extractorMigrationRow) { row.report = `{"can_enable":false}` }},
		{"low active validation", func(row *extractorMigrationRow) { row.validation = 0.89 }},
		{"not-executed window", func(row *extractorMigrationRow) { row.window = `[{"not":"text"}]` }},
		{"unknown window value", func(row *extractorMigrationRow) { row.window = `["not_executed"]` }},
		{"oversized window", func(row *extractorMigrationRow) {
			values := make([]string, 21)
			for index := range values {
				values[index] = "succeeded"
			}
			encoded, _ := json.Marshal(values)
			row.window = string(encoded)
		}},
		{"malformed created time", func(row *extractorMigrationRow) { row.createdAt = "now" }},
		{"offset created time", func(row *extractorMigrationRow) { row.createdAt = "2026-08-09T08:00:00.000000000+08:00" }},
		{"updated before created", func(row *extractorMigrationRow) {
			row.createdAt = extractorMigrationLaterTime
			row.updatedAt = extractorMigrationTime
		}},
	}
	for index, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			row := validExtractorMigrationRow(index + 1)
			test.mutate(&row)
			var insertErr error
			if err := store.Update(context.Background(), func(tx WriteTx) error {
				insertErr = insertExtractorMigrationRow(context.Background(), tx, row)
				return nil
			}); err != nil {
				t.Fatalf("Update() error = %v", err)
			}
			if insertErr == nil {
				t.Fatal("malformed extractor insert succeeded")
			}
		})
	}
}

func TestExtractorMigrationStateImmutabilityAndDeleteGuards(t *testing.T) {
	store := openTestStore(t)
	row := validExtractorMigrationRow(1)
	if err := store.Update(context.Background(), func(tx WriteTx) error {
		return insertExtractorMigrationRow(context.Background(), tx, row)
	}); err != nil {
		t.Fatalf("insert valid extractor: %v", err)
	}

	immutable := []struct {
		column string
		value  any
	}{
		{"version", 2},
		{"ir", `{"version":2}`},
		{"validation", 0.95},
		{"template_simhash", []byte{0, 0, 0, 0, 0, 0, 0, 9}},
		{"created_at", extractorMigrationLaterTime},
	}
	for _, test := range immutable {
		t.Run("immutable "+test.column, func(t *testing.T) {
			var updateErr error
			if err := store.Update(context.Background(), func(tx WriteTx) error {
				updateErr = execExtractorUpdate(context.Background(), tx,
					"UPDATE extractors SET "+test.column+" = ? WHERE id = ?", test.value, row.id)
				return nil
			}); err != nil {
				t.Fatalf("Update() error = %v", err)
			}
			if updateErr == nil {
				t.Fatalf("updating immutable %s succeeded", test.column)
			}
		})
	}

	if err := store.Update(context.Background(), func(tx WriteTx) error {
		if _, err := tx.ExecContext(context.Background(), `UPDATE extractors
			SET empty_window = '["required_empty"]', last_used_at = ?, updated_at = ?
			WHERE id = ?`, extractorMigrationLaterTime, extractorMigrationLaterTime, row.id); err != nil {
			return err
		}
		_, err := tx.ExecContext(context.Background(), `UPDATE extractors
			SET state = 'stale', stale_reason = 'template_drift' WHERE id = ?`, row.id)
		return err
	}); err != nil {
		t.Fatalf("allowed health/state updates: %v", err)
	}

	var reason string
	if err := store.Update(context.Background(), func(tx WriteTx) error {
		if _, err := tx.ExecContext(context.Background(),
			"UPDATE extractors SET state = 'retired' WHERE id = ?", row.id); err != nil {
			return err
		}
		return tx.QueryRowContext(context.Background(),
			"SELECT stale_reason FROM extractors WHERE id = ?", row.id).Scan(&reason)
	}); err != nil {
		t.Fatalf("stale to retired: %v", err)
	}
	if reason != "template_drift" {
		t.Fatalf("retired stale_reason = %q", reason)
	}

	otherSchema := repeatHex("d")
	var crossSchemaErr error
	if err := store.Update(context.Background(), func(tx WriteTx) error {
		_, err := tx.ExecContext(context.Background(), `INSERT INTO extractor_page_bindings
			(page_hash, schema_hash, extractor_id, bound_at, last_seen_at)
			VALUES (?, ?, ?, ?, ?)`, repeatHex("e"), otherSchema, row.id,
			extractorMigrationTime, extractorMigrationTime)
		crossSchemaErr = err
		return nil
	}); err != nil {
		t.Fatalf("cross-schema Update() error = %v", err)
	}
	if crossSchemaErr == nil {
		t.Fatal("cross-schema binding succeeded")
	}

	if err := store.Update(context.Background(), func(tx WriteTx) error {
		_, err := tx.ExecContext(context.Background(), `INSERT INTO extractor_page_bindings
			(page_hash, schema_hash, extractor_id, bound_at, last_seen_at)
			VALUES (?, ?, ?, ?, ?)`, repeatHex("f"), row.schemaHash, row.id,
			extractorMigrationTime, extractorMigrationTime)
		return err
	}); err != nil {
		t.Fatalf("insert valid binding: %v", err)
	}

	var deleteErr error
	if err := store.Update(context.Background(), func(tx WriteTx) error {
		_, deleteErr = tx.ExecContext(context.Background(), "DELETE FROM extractors WHERE id = ?", row.id)
		return nil
	}); err != nil {
		t.Fatalf("guarded delete Update() error = %v", err)
	}
	if deleteErr == nil {
		t.Fatal("retired extractor with binding was deleted")
	}
	if err := store.Update(context.Background(), func(tx WriteTx) error {
		if _, err := tx.ExecContext(context.Background(),
			"DELETE FROM extractor_page_bindings WHERE extractor_id = ?", row.id); err != nil {
			return err
		}
		_, err := tx.ExecContext(context.Background(), "DELETE FROM extractors WHERE id = ?", row.id)
		return err
	}); err != nil {
		t.Fatalf("ordered retired cleanup: %v", err)
	}
}

func TestExtractorMigrationRejectsActiveAndStaleDelete(t *testing.T) {
	store := openTestStore(t)
	for index, state := range []string{"active", "stale"} {
		row := validExtractorMigrationRow(index + 10)
		if state == "stale" {
			row.state = "stale"
			row.reason = "template_drift"
		}
		if err := store.Update(context.Background(), func(tx WriteTx) error {
			return insertExtractorMigrationRow(context.Background(), tx, row)
		}); err != nil {
			t.Fatalf("insert %s: %v", state, err)
		}
		var deleteErr error
		if err := store.Update(context.Background(), func(tx WriteTx) error {
			_, deleteErr = tx.ExecContext(context.Background(), "DELETE FROM extractors WHERE id = ?", row.id)
			return nil
		}); err != nil {
			t.Fatalf("delete %s Update() error = %v", state, err)
		}
		if deleteErr == nil {
			t.Fatalf("%s extractor delete succeeded", state)
		}
	}
}

func TestMigration003UpgradesVersionTwoDatabase(t *testing.T) {
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
	for index := 0; index < 2; index++ {
		if _, err := db.Exec(migrations[index]); err != nil {
			t.Fatalf("apply migration %03d: %v", index+1, err)
		}
		if _, err := db.Exec("INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)",
			index+1, extractorMigrationTime); err != nil {
			t.Fatalf("record migration %03d: %v", index+1, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close version-two database: %v", err)
	}

	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(upgrade) error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	assertMigrationState(t, store.db)
	var count int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if count != len(migrations) {
		t.Fatalf("migration count = %d, want %d", count, len(migrations))
	}
}

func validExtractorMigrationRow(index int) extractorMigrationRow {
	templateHash := []byte{0, 0, 0, 0, 0, 0, 0, byte(index + 1)}
	return extractorMigrationRow{
		id:           "00000000-0000-4000-8000-" + leftPadDecimal(index, 12),
		host:         "example.com",
		schemaJSON:   `{"type":"object"}`,
		schemaHash:   repeatHex("a"),
		clusterID:    repeatHex(string(rune('b' + index%4))),
		templateHash: templateHash,
		ir:           `{"version":1,"fields":[]}`,
		irHash:       repeatHex("c"),
		irVersion:    1,
		version:      1,
		report:       `{"can_enable":true}`,
		validation:   1.0,
		state:        "active",
		window:       `[]`,
		reason:       "",
		createdAt:    extractorMigrationTime,
		lastUsedAt:   nil,
		updatedAt:    extractorMigrationTime,
	}
}

func insertExtractorMigrationRow(ctx context.Context, tx WriteTx, row extractorMigrationRow) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO extractors (
		id, host, schema_json, schema_hash, template_cluster_id,
		template_simhash, ir, ir_hash, ir_format_version, version,
		validation_report, validation, state, empty_window, stale_reason,
		created_at, last_used_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.id, row.host, row.schemaJSON, row.schemaHash, row.clusterID,
		row.templateHash, row.ir, row.irHash, row.irVersion, row.version,
		row.report, row.validation, row.state, row.window, row.reason,
		row.createdAt, row.lastUsedAt, row.updatedAt)
	return err
}

func execExtractorUpdate(ctx context.Context, tx WriteTx, query string, args ...any) error {
	_, err := tx.ExecContext(ctx, query, args...)
	return err
}

func leftPadDecimal(value, width int) string {
	result := strings.Repeat("0", width) + strings.TrimSpace(sqlIntString(value))
	return result[len(result)-width:]
}

func sqlIntString(value int) string {
	digits := ""
	if value == 0 {
		return "0"
	}
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}
