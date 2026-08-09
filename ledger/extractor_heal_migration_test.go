package ledger

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

const extractorHealLeaseTime = "2026-08-09T00:00:02.000000000Z"

type extractorHealMigrationRow struct {
	id, sourceID, sourceVersion, host, schemaJSON, schemaHash any
	profile, targetID, targetHash, revision                   any
	ir, irHash, irVersion, report, validation, samples        any
	state, terminalReason, leaseID, leaseUntil                any
	replayTotal, replayMatched, replayRatio, promotedID       any
	createdAt, updatedAt, completedAt                         any
}

func TestExtractorHealMigrationIsStrictBoundedAuditState(t *testing.T) {
	store := openTestStore(t)
	var definition string
	if err := store.db.QueryRow(`SELECT sql FROM sqlite_master
		WHERE type = 'table' AND name = 'extractor_heal_runs'`).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(definition), "strict") {
		t.Fatalf("extractor_heal_runs is not STRICT: %s", definition)
	}
	for _, object := range []string{
		"idx_extractor_heal_runs_pending_target", "idx_extractor_heal_runs_source",
		"idx_extractor_heal_runs_lease", "idx_extractor_heal_runs_terminal",
		"idx_verifications_exact_heal_replay",
		"trg_extractor_heal_runs_identity_immutable", "trg_extractor_heal_runs_source_exact",
		"trg_extractor_heal_runs_samples_insert", "trg_extractor_heal_runs_state_transition",
		"trg_extractor_heal_runs_terminal_immutable", "trg_extractor_heal_runs_updated_monotonic",
		"trg_extractor_heal_runs_delete_guard", "trg_extractors_delete_audit_guard",
	} {
		var name string
		if err := store.db.QueryRow("SELECT name FROM sqlite_master WHERE name = ?", object).Scan(&name); err != nil {
			t.Fatalf("schema object %q: %v", object, err)
		}
	}
}

func TestExtractorHealReplayQueryUsesExactProvenanceIndex(t *testing.T) {
	store := openTestStore(t)
	rows, err := store.db.Query(`EXPLAIN QUERY PLAN
		SELECT id, old_snapshot_id, path, old_value FROM verifications
		WHERE extractor_id = ? AND schema_hash = ? AND template_cluster_id = ?
			AND outcome = 'confirmed' AND extractor_id IS NOT NULL AND
			schema_hash IS NOT NULL AND template_cluster_id IS NOT NULL
		ORDER BY verified_at DESC, id LIMIT 5`,
		"00000000-0000-4000-8000-000000000001", repeatHex("a"), repeatHex("b"))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	plan := ""
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "idx_verifications_exact_heal_replay") {
		t.Fatalf("exact replay query plan does not use provenance index:\n%s", plan)
	}
}

func TestExtractorHealMigrationRejectsMalformedCandidates(t *testing.T) {
	cases := []struct {
		name         string
		mutateSource func(*extractorMigrationRow)
		mutate       func(*extractorHealMigrationRow)
	}{
		{"active source", func(row *extractorMigrationRow) { row.state, row.reason = "active", "" }, nil},
		{"wrong source version", nil, func(row *extractorHealMigrationRow) { row.sourceVersion = 2 }},
		{"weak validation", nil, func(row *extractorHealMigrationRow) { row.validation = 0.89 }},
		{"disabled validation report", nil, func(row *extractorHealMigrationRow) { row.report = `{"can_enable":false}` }},
		{"two samples", nil, func(row *extractorHealMigrationRow) { row.samples = validHealSamples(2) }},
		{"sample extra member", nil, func(row *extractorHealMigrationRow) {
			row.samples = `[{"page_hash":"` + repeatHex("a") + `","snapshot_id":"sha256:` + repeatHex("b") + `","sample_simhash":"8000000000000001","fetched_at":"` + extractorMigrationTime + `","extra":true},` + validHealSamples(2)[1:]
		}},
		{"sample renamed member", nil, func(row *extractorHealMigrationRow) {
			row.samples = `[{"renamed_page_hash":"` + repeatHex("a") + `","snapshot_id":"sha256:` + repeatHex("b") + `","sample_simhash":"8000000000000001","fetched_at":"` + extractorMigrationTime + `"},` + validHealSamples(2)[1:]
		}},
		{"pending lease", nil, func(row *extractorHealMigrationRow) {
			row.leaseID, row.leaseUntil = "00000000-0000-4000-8000-000000000099", extractorHealLeaseTime
		}},
		{"inconsistent replay ratio", nil, func(row *extractorHealMigrationRow) {
			row.state = "replaying"
			row.leaseID, row.leaseUntil = "00000000-0000-4000-8000-000000000099", extractorHealLeaseTime
			row.replayTotal, row.replayMatched, row.replayRatio = 10, 9, 0.8
		}},
	}
	for index, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			source := validExtractorMigrationRow(index + 100)
			source.state, source.reason = "stale", "template_drift"
			if test.mutateSource != nil {
				test.mutateSource(&source)
			}
			if err := store.Update(context.Background(), func(tx WriteTx) error {
				return insertExtractorMigrationRow(context.Background(), tx, source)
			}); err != nil {
				t.Fatal(err)
			}
			row := validExtractorHealMigrationRow(index+1, source)
			if test.mutate != nil {
				test.mutate(&row)
			}
			var insertErr error
			if err := store.Update(context.Background(), func(tx WriteTx) error {
				insertErr = insertExtractorHealMigrationRow(context.Background(), tx, row)
				return nil
			}); err != nil {
				t.Fatalf("Update() wrapper error = %v", err)
			}
			if insertErr == nil {
				t.Fatal("malformed extractor heal candidate insert succeeded")
			}
		})
	}
}

func TestExtractorHealMigrationUniquePendingStateAndAuditGuards(t *testing.T) {
	store := openTestStore(t)
	source := validExtractorMigrationRow(200)
	source.state, source.reason = "stale", "template_drift"
	promoted := validExtractorMigrationRow(201)
	row := validExtractorHealMigrationRow(1, source)
	if err := store.Update(context.Background(), func(tx WriteTx) error {
		if err := insertExtractorMigrationRow(context.Background(), tx, source); err != nil {
			return err
		}
		if err := insertExtractorMigrationRow(context.Background(), tx, promoted); err != nil {
			return err
		}
		return insertExtractorHealMigrationRow(context.Background(), tx, row)
	}); err != nil {
		t.Fatalf("seed heal audit: %v", err)
	}

	duplicate := row
	duplicate.id = "00000000-0000-4000-8000-000000000099"
	var duplicateErr error
	if err := store.Update(context.Background(), func(tx WriteTx) error {
		duplicateErr = insertExtractorHealMigrationRow(context.Background(), tx, duplicate)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if duplicateErr == nil {
		t.Fatal("second pending candidate for exact target succeeded")
	}

	for _, test := range []struct {
		name  string
		query string
		arg   any
	}{
		{"source", "UPDATE extractor_heal_runs SET source_version = ? WHERE id = ?", 2},
		{"catalog", "UPDATE extractor_heal_runs SET catalog_revision = ? WHERE id = ?", repeatHex("f")},
		{"clock rollback", "UPDATE extractor_heal_runs SET updated_at = ? WHERE id = ?", "2026-08-08T23:59:59.000000000Z"},
	} {
		t.Run("immutable "+test.name, func(t *testing.T) {
			var updateErr error
			if err := store.Update(context.Background(), func(tx WriteTx) error {
				_, updateErr = tx.ExecContext(context.Background(), test.query, test.arg, row.id)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if updateErr == nil {
				t.Fatal("guarded heal update succeeded")
			}
		})
	}

	if err := store.Update(context.Background(), func(tx WriteTx) error {
		_, err := tx.ExecContext(context.Background(), `UPDATE extractor_heal_runs SET
			state = 'replaying', lease_id = ?, lease_until = ?, updated_at = ? WHERE id = ?`,
			"00000000-0000-4000-8000-000000000088", extractorHealLeaseTime,
			extractorMigrationLaterTime, row.id)
		return err
	}); err != nil {
		t.Fatalf("pending to replaying: %v", err)
	}
	if err := store.Update(context.Background(), func(tx WriteTx) error {
		_, err := tx.ExecContext(context.Background(), `UPDATE extractor_heal_runs SET
			state = 'promoted', terminal_reason = 'replay_passed', lease_id = NULL,
			lease_until = NULL, replay_total = 10, replay_matched = 9, replay_ratio = 0.9,
			promoted_extractor_id = ?, updated_at = ?, completed_at = ? WHERE id = ?`,
			promoted.id, extractorHealLeaseTime, extractorHealLeaseTime, row.id)
		return err
	}); err != nil {
		t.Fatalf("replaying to promoted: %v", err)
	}
	var terminalErr error
	if err := store.Update(context.Background(), func(tx WriteTx) error {
		_, terminalErr = tx.ExecContext(context.Background(),
			"UPDATE extractor_heal_runs SET terminal_reason = 'changed' WHERE id = ?", row.id)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if terminalErr == nil {
		t.Fatal("terminal heal audit update succeeded")
	}

	if err := store.Update(context.Background(), func(tx WriteTx) error {
		_, err := tx.ExecContext(context.Background(),
			"UPDATE extractors SET state = 'retired' WHERE id = ?", source.id)
		return err
	}); err != nil {
		t.Fatalf("retire heal source: %v", err)
	}
	var deleteSourceErr, deleteHealErr error
	if err := store.Update(context.Background(), func(tx WriteTx) error {
		_, deleteSourceErr = tx.ExecContext(context.Background(), "DELETE FROM extractors WHERE id = ?", source.id)
		_, deleteHealErr = tx.ExecContext(context.Background(), "DELETE FROM extractor_heal_runs WHERE id = ?", row.id)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if deleteSourceErr == nil || deleteHealErr == nil {
		t.Fatalf("audit deletes source/heal errors = %v/%v", deleteSourceErr, deleteHealErr)
	}
}

func TestExtractorDeleteAuditGuardProtectsVerificationProvenance(t *testing.T) {
	store := openTestStore(t)
	row := validExtractorMigrationRow(300)
	if err := store.Update(context.Background(), func(tx WriteTx) error {
		if err := insertExtractorMigrationRow(context.Background(), tx, row); err != nil {
			return err
		}
		if _, err := tx.ExecContext(context.Background(),
			"UPDATE extractors SET state = 'retired' WHERE id = ?", row.id); err != nil {
			return err
		}
		_, err := tx.ExecContext(context.Background(), `INSERT INTO verifications (
			id, verification_id, claim_index, url, host, path, old_value, outcome,
			old_snapshot_id, schema_hash, template_cluster_id, extractor_id, verified_at
		) VALUES (?, ?, 0, ?, ?, ?, ?, 'confirmed', ?, ?, ?, ?, ?)`,
			"verification-row", "verification-batch", "https://example.com/", "example.com",
			"name", `"Purify"`, "sha256:"+repeatHex("e"), row.schemaHash, row.clusterID,
			row.id, extractorMigrationTime)
		return err
	}); err != nil {
		t.Fatalf("seed verification audit: %v", err)
	}
	var deleteErr error
	if err := store.Update(context.Background(), func(tx WriteTx) error {
		_, deleteErr = tx.ExecContext(context.Background(), "DELETE FROM extractors WHERE id = ?", row.id)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if deleteErr == nil {
		t.Fatal("verification-referenced retired extractor was deleted")
	}
}

func TestMigration006UpgradesVersionFiveDatabaseAndReopens(t *testing.T) {
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
	for index := 0; index < 5; index++ {
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
		WHERE type = 'table' AND name = 'extractor_heal_runs'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if versions != len(migrations) || tables != 1 {
		t.Fatalf("upgrade state migrations=%d/%d tables=%d", versions, len(migrations), tables)
	}
}

func validExtractorHealMigrationRow(index int, source extractorMigrationRow) extractorHealMigrationRow {
	return extractorHealMigrationRow{
		id:             "00000000-0000-4000-8001-" + leftPadDecimal(index, 12),
		sourceID:       source.id,
		sourceVersion:  source.version,
		host:           source.host,
		schemaJSON:     source.schemaJSON,
		schemaHash:     source.schemaHash,
		profile:        "extract-default-v1",
		targetID:       repeatHex("f"),
		targetHash:     []byte{0x80, 0, 0, 0, 0, 0, 0, byte(index + 1)},
		revision:       repeatHex("d"),
		ir:             `{"version":1,"fields":[]}`,
		irHash:         repeatHex("c"),
		irVersion:      1,
		report:         `{"can_enable":true}`,
		validation:     1.0,
		samples:        validHealSamples(3),
		state:          "pending",
		terminalReason: "",
		replayTotal:    0,
		replayMatched:  0,
		createdAt:      extractorMigrationTime,
		updatedAt:      extractorMigrationTime,
	}
}

func validHealSamples(count int) string {
	parts := make([]string, count)
	for index := range parts {
		pageDigit := string(rune('a' + index))
		snapshotDigit := string(rune('d' + index))
		parts[index] = `{"page_hash":"` + repeatHex(pageDigit) +
			`","snapshot_id":"sha256:` + repeatHex(snapshotDigit) +
			`","sample_simhash":"800000000000000` + sqlIntString(index+1) +
			`","fetched_at":"` + extractorMigrationTime + `"}`
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func insertExtractorHealMigrationRow(ctx context.Context, tx WriteTx, row extractorHealMigrationRow) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO extractor_heal_runs (
		id, source_extractor_id, source_version, host, schema_json, schema_hash,
		content_profile, target_template_cluster_id, target_cluster_simhash,
		catalog_revision, candidate_ir, candidate_ir_hash, candidate_ir_format_version,
		validation_report, validation, samples_json, state, terminal_reason,
		lease_id, lease_until, replay_total, replay_matched, replay_ratio,
		promoted_extractor_id, created_at, updated_at, completed_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.id, row.sourceID, row.sourceVersion, row.host, row.schemaJSON, row.schemaHash,
		row.profile, row.targetID, row.targetHash, row.revision, row.ir, row.irHash,
		row.irVersion, row.report, row.validation, row.samples, row.state, row.terminalReason,
		row.leaseID, row.leaseUntil, row.replayTotal, row.replayMatched, row.replayRatio,
		row.promotedID, row.createdAt, row.updatedAt, row.completedAt)
	return err
}
