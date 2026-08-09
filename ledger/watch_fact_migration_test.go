package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

const (
	watchFactT0 = "2026-08-10T00:00:00.000000000Z"
	watchFactT1 = "2026-08-10T01:00:00.000000000Z"
	watchFactT2 = "2026-08-10T02:00:00.000000000Z"
	watchFactT3 = "2026-08-10T03:00:00.000000000Z"
)

type watchMigrationRow struct {
	id, specHash, subject, predicate, freshness, conflict string
	minimum                                               int
	state, nextCheck                                      any
	ewma                                                  any
	lastChange, lastChecked                               any
	failures                                              int
	lastError                                             string
	lastVerificationID, lastVerificationIndex             any
	leaseID, leaseUntil                                   any
	createdAt, updatedAt                                  string
	pausedAt, deletedAt                                   any
}

type factVerificationMigrationRow struct {
	rowID, verificationID string
	claimIndex            int
	url, finalURL, path   string
	oldValue              string
	newValue              any
	outcome               string
	goneScope             any
	oldSnapshot           string
	newSnapshot           any
	oldReceipt            any
	receipt               any
	verifiedAt            string
}

type factMigrationRow struct {
	id, watchID, subject, predicate, path string
	value, root, sourceURL, receipt       string
	snapshot                              string
	createdVerificationID                 string
	createdClaimIndex                     int
	latestVerificationID                  string
	latestClaimIndex                      int
	closedVerificationID                  any
	closedClaimIndex                      any
	observedAt, validFrom                 string
	validTo                               any
	lastVerifiedAt                        string
	supersededBy                          any
	closedOutcome, goneScope              string
}

func TestWatchFactMigrationCreatesStrictSchemaAndCoveringIndexes(t *testing.T) {
	store := openTestStore(t)
	for _, table := range []string{"watches", "facts"} {
		var definition string
		if err := store.db.QueryRow(`SELECT sql FROM sqlite_master
			WHERE type = 'table' AND name = ?`, table).Scan(&definition); err != nil {
			t.Fatalf("read %s definition: %v", table, err)
		}
		if !strings.Contains(strings.ToUpper(definition), "STRICT") {
			t.Fatalf("%s is not STRICT: %s", table, definition)
		}
		var strict int
		if err := store.db.QueryRow(`SELECT strict FROM pragma_table_list
			WHERE schema = 'main' AND name = ?`, table).Scan(&strict); err != nil {
			t.Fatalf("read %s PRAGMA strict bit: %v", table, err)
		}
		if strict != 1 {
			t.Fatalf("%s PRAGMA strict bit = %d, want 1", table, strict)
		}
	}

	for _, object := range []string{
		"idx_watches_live_spec", "idx_watches_live_created", "idx_watches_due", "idx_facts_open_watch",
		"idx_facts_watch_history", "idx_facts_subject_predicate_valid",
		"trg_watches_spec_immutable", "trg_watches_state_transition",
		"trg_watches_deleted_immutable", "trg_watches_updated_monotonic",
		"trg_watches_delete_guard", "trg_facts_watch_spec_insert",
		"trg_facts_insert_open",
		"trg_facts_created_provenance", "trg_facts_latest_provenance_insert",
		"trg_facts_changed_successor_insert", "trg_facts_identity_immutable",
		"trg_facts_closed_immutable", "trg_facts_open_transition",
		"trg_facts_delete_guard",
	} {
		var name string
		if err := store.db.QueryRow("SELECT name FROM sqlite_master WHERE name = ?", object).Scan(&name); err != nil {
			t.Fatalf("schema object %q: %v", object, err)
		}
	}

	var factsDefinition string
	if err := store.db.QueryRow(`SELECT sql FROM sqlite_master
		WHERE type = 'table' AND name = 'facts'`).Scan(&factsDefinition); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(factsDefinition, "REFERENCES verifications(verification_id, claim_index) ON DELETE RESTRICT"); got != 3 {
		t.Fatalf("verification composite FK count = %d, want 3: %s", got, factsDefinition)
	}
	if !strings.Contains(factsDefinition, "DEFERRABLE INITIALLY DEFERRED") {
		t.Fatalf("fact successor FK is not deferred: %s", factsDefinition)
	}
	var dueIndexDefinition string
	if err := store.db.QueryRow(`SELECT sql FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_watches_due'`).Scan(&dueIndexDefinition); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dueIndexDefinition, "WHERE state IN ('pending', 'active')") {
		t.Fatalf("due index retains terminal watch history: %s", dueIndexDefinition)
	}

	for index := 0; index < 256; index++ {
		row := validWatchMigrationRow(7000 + index)
		row.specHash = fmt.Sprintf("%064x", 7000+index)
		row.nextCheck = nil
		row.updatedAt = watchFactT1
		if index%2 == 0 {
			row.state, row.pausedAt = "paused", watchFactT1
		} else {
			row.state, row.deletedAt = "deleted", watchFactT1
		}
		if err := insertWatchMigrationRow(context.Background(), store.db, row); err != nil {
			t.Fatalf("seed terminal watch %d: %v", index, err)
		}
	}
	for index := 0; index < 3; index++ {
		row := validWatchMigrationRow(8000 + index)
		row.specHash = fmt.Sprintf("%064x", 8000+index)
		row.state = []string{"pending", "active", "active"}[index]
		if err := insertWatchMigrationRow(context.Background(), store.db, row); err != nil {
			t.Fatalf("seed actionable watch %d: %v", index, err)
		}
	}
	if _, err := store.db.Exec("ANALYZE"); err != nil {
		t.Fatal(err)
	}

	rows, err := store.db.Query(`EXPLAIN QUERY PLAN
		SELECT id, next_check_at, lease_until FROM watches
		WHERE state IN ('pending', 'active') AND next_check_at <= ? AND
			(lease_until IS NULL OR lease_until <= ?)
		ORDER BY next_check_at, lease_until, id LIMIT 32`, watchFactT3, watchFactT3)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan string
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
	if !strings.Contains(plan, "USING COVERING INDEX idx_watches_due") {
		t.Fatalf("due poll does not use covering index:\n%s", plan)
	}
	if strings.Contains(strings.ToUpper(plan), "TEMP B-TREE") {
		t.Fatalf("due poll needs a temporary sort:\n%s", plan)
	}

	rows, err = store.db.Query(`EXPLAIN QUERY PLAN
		SELECT id, subject, predicate, state, created_at FROM watches
		WHERE state <> 'deleted'
		ORDER BY created_at, id LIMIT 100`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	plan = ""
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
	if !strings.Contains(plan, "USING INDEX idx_watches_live_created") {
		t.Fatalf("live watch list does not use creation index:\n%s", plan)
	}
	if strings.Contains(strings.ToUpper(plan), "TEMP B-TREE") {
		t.Fatalf("live watch list needs a temporary sort:\n%s", plan)
	}

	rows, err = store.db.Query(`EXPLAIN QUERY PLAN
		SELECT id, value, valid_from, valid_to FROM facts
		WHERE subject = ? AND predicate = ? AND valid_from <= ? AND
			(valid_to IS NULL OR valid_to > ?)
		ORDER BY valid_from DESC, id LIMIT 1`, "subject", "price", watchFactT3, watchFactT3)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	plan = ""
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	if !strings.Contains(plan, "idx_facts_subject_predicate_valid") {
		t.Fatalf("as_of query does not use fact lookup index:\n%s", plan)
	}
	if strings.Contains(strings.ToUpper(plan), "TEMP B-TREE") {
		t.Fatalf("as_of query needs a temporary sort:\n%s", plan)
	}
}

func TestWatchesRejectMalformedStateAndProtectSpecHistory(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	first := validWatchMigrationRow(1)
	if err := insertWatchMigrationRow(ctx, store.db, first); err != nil {
		t.Fatalf("insert watch: %v", err)
	}

	duplicate := validWatchMigrationRow(2)
	duplicate.specHash = first.specHash
	if err := insertWatchMigrationRow(ctx, store.db, duplicate); err == nil {
		t.Fatal("duplicate live spec_hash succeeded")
	}

	for _, test := range []struct {
		name   string
		mutate func(*watchMigrationRow)
	}{
		{"uppercase uuid", func(row *watchMigrationRow) { row.id = "A" + row.id[1:] }},
		{"short spec hash", func(row *watchMigrationRow) { row.specHash = "abc" }},
		{"non canonical subject", func(row *watchMigrationRow) { row.subject = "two  spaces" }},
		{"subject control", func(row *watchMigrationRow) { row.subject = "bad\nsubject" }},
		{"predicate whitespace", func(row *watchMigrationRow) { row.predicate = "unit price" }},
		{"freshness alias", func(row *watchMigrationRow) { row.freshness = "7d" }},
		{"zero minimum", func(row *watchMigrationRow) { row.minimum = 0 }},
		{"large minimum", func(row *watchMigrationRow) { row.minimum = 9 }},
		{"unsupported conflict", func(row *watchMigrationRow) { row.conflict = "choose" }},
		{"small ewma", func(row *watchMigrationRow) { row.ewma = 599.999 }},
		{"large ewma", func(row *watchMigrationRow) { row.ewma = 604800.001 }},
		{"failure without code", func(row *watchMigrationRow) { row.failures = 1 }},
		{"code without failure", func(row *watchMigrationRow) { row.lastError = "FETCH_FAILED" }},
		{"unstable error code", func(row *watchMigrationRow) { row.failures, row.lastError = 1, "fetch failed" }},
		{"half lease", func(row *watchMigrationRow) { row.leaseID = watchUUID(999) }},
		{"expired lease", func(row *watchMigrationRow) {
			row.leaseID, row.leaseUntil = watchUUID(999), watchFactT0
		}},
		{"paused with due time", func(row *watchMigrationRow) {
			row.state, row.pausedAt = "paused", watchFactT1
		}},
		{"deleted with pause timestamp", func(row *watchMigrationRow) {
			row.state, row.nextCheck, row.pausedAt, row.deletedAt = "deleted", nil, watchFactT1, watchFactT1
		}},
		{"half verification provenance", func(row *watchMigrationRow) {
			row.lastVerificationID = "missing"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			row := validWatchMigrationRow(100 + len(test.name))
			row.specHash = fmt.Sprintf("%064x", 100+len(test.name))
			test.mutate(&row)
			if err := insertWatchMigrationRow(ctx, store.db, row); err == nil {
				t.Fatal("malformed watch insert succeeded")
			}
		})
	}

	second := validWatchMigrationRow(3)
	second.specHash = fmt.Sprintf("%064x", 10001)
	second.ewma = 600.0
	if err := insertWatchMigrationRow(ctx, store.db, second); err != nil {
		t.Fatalf("minimum EWMA insert: %v", err)
	}
	third := validWatchMigrationRow(4)
	third.specHash = fmt.Sprintf("%064x", 10002)
	third.ewma = 604800.0
	third.failures, third.lastError = 2, "FETCH_FAILED"
	if err := insertWatchMigrationRow(ctx, store.db, third); err != nil {
		t.Fatalf("maximum EWMA/stable failure insert: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE watches SET next_check_at = ? WHERE id = ?`, watchFactT2, third.id); err == nil {
		t.Fatal("watch operational update without advancing updated_at succeeded")
	}
	var unchangedNextCheck string
	if err := store.db.QueryRow("SELECT next_check_at FROM watches WHERE id = ?", third.id).Scan(&unchangedNextCheck); err != nil {
		t.Fatal(err)
	}
	if unchangedNextCheck != watchFactT1 {
		t.Fatalf("failed operational update changed next_check_at to %q", unchangedNextCheck)
	}

	if _, err := store.db.Exec(`UPDATE watches SET state = 'paused', next_check_at = NULL,
		paused_at = ?, updated_at = ? WHERE id = ?`, watchFactT1, watchFactT1, second.id); err != nil {
		t.Fatalf("pause watch: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE watches SET state = 'active', next_check_at = ?,
		paused_at = NULL, updated_at = ? WHERE id = ?`, watchFactT2, watchFactT2, second.id); err != nil {
		t.Fatalf("resume watch: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE watches SET state = 'pending', updated_at = ? WHERE id = ?`, watchFactT3, second.id); err == nil {
		t.Fatal("active to pending transition succeeded")
	}
	if _, err := store.db.Exec(`UPDATE watches SET subject = 'changed', updated_at = ? WHERE id = ?`, watchFactT3, second.id); err == nil {
		t.Fatal("watch spec mutation succeeded")
	}
	if _, err := store.db.Exec(`UPDATE watches SET updated_at = ? WHERE id = ?`, watchFactT0, second.id); err == nil {
		t.Fatal("watch updated_at rollback succeeded")
	}

	if _, err := store.db.Exec(`UPDATE watches SET state = 'deleted', next_check_at = NULL,
		deleted_at = ?, updated_at = ? WHERE id = ?`, watchFactT2, watchFactT2, first.id); err != nil {
		t.Fatalf("soft delete watch: %v", err)
	}
	replacement := validWatchMigrationRow(5)
	replacement.specHash = first.specHash
	if err := insertWatchMigrationRow(ctx, store.db, replacement); err != nil {
		t.Fatalf("replace deleted spec: %v", err)
	}
	if _, err := store.db.Exec("UPDATE watches SET updated_at = ? WHERE id = ?", watchFactT3, first.id); err == nil {
		t.Fatal("deleted watch update succeeded")
	}
	if _, err := store.db.Exec("DELETE FROM watches WHERE id = ?", first.id); err == nil {
		t.Fatal("physical watch delete succeeded")
	}
}

func TestFactsRejectMalformedValuesAndMismatchedProvenance(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	for index, test := range []struct {
		name   string
		mutate func(*factMigrationRow)
	}{
		{"short id", func(row *factMigrationRow) { row.id = "abc" }},
		{"json null", func(row *factMigrationRow) { row.value = "null" }},
		{"json object", func(row *factMigrationRow) { row.value = `{}` }},
		{"json array", func(row *factMigrationRow) { row.value = `[]` }},
		{"invalid json", func(row *factMigrationRow) { row.value = `"` }},
		{"bad root", func(row *factMigrationRow) { row.root = "Example.COM" }},
		{"root control", func(row *factMigrationRow) { row.root = "example.com\n" }},
		{"bad url scheme", func(row *factMigrationRow) { row.sourceURL = "file:///tmp/a" }},
		{"fragment url", func(row *factMigrationRow) { row.sourceURL += "#fragment" }},
		{"empty receipt", func(row *factMigrationRow) { row.receipt = "" }},
		{"receipt control", func(row *factMigrationRow) { row.receipt = "bad\nreceipt" }},
		{"bad snapshot", func(row *factMigrationRow) { row.snapshot = "sha256:short" }},
		{"watch subject mismatch", func(row *factMigrationRow) { row.subject = "another subject" }},
		{"watch predicate mismatch", func(row *factMigrationRow) { row.predicate = "another_predicate" }},
		{"created path mismatch", func(row *factMigrationRow) { row.path = "another_path" }},
		{"created value mismatch", func(row *factMigrationRow) { row.value = `20` }},
		{"created snapshot mismatch", func(row *factMigrationRow) { row.snapshot = factSnapshot("f") }},
		{"created source mismatch", func(row *factMigrationRow) { row.sourceURL = "https://different.example/pricing" }},
		{"created receipt mismatch", func(row *factMigrationRow) { row.receipt = "wrong-receipt" }},
		{"latest differs on insert", func(row *factMigrationRow) { row.latestVerificationID = "other" }},
		{"created timestamp differs by one nanosecond", func(row *factMigrationRow) {
			row.observedAt = "2026-08-10T01:00:00.000000001Z"
			row.validFrom = row.observedAt
			row.lastVerifiedAt = row.observedAt
		}},
		{"closed on insert", func(row *factMigrationRow) {
			row.validTo, row.closedVerificationID, row.closedClaimIndex = watchFactT2, row.createdVerificationID, 0
			row.closedOutcome, row.goneScope = "gone", "page"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			watch := validWatchMigrationRow(1000 + index)
			watch.specHash = fmt.Sprintf("%064x", 20000+index)
			if err := insertWatchMigrationRow(ctx, store.db, watch); err != nil {
				t.Fatalf("seed watch: %v", err)
			}
			verification := initialConfirmedFactVerification(index)
			verification.verificationID = fmt.Sprintf("initial-%d", index)
			verification.rowID = fmt.Sprintf("row-initial-%d", index)
			if err := insertFactVerificationMigrationRow(ctx, store.db, verification); err != nil {
				t.Fatalf("seed verification: %v", err)
			}
			fact := factFromVerification(watch, verification, factHex(fmt.Sprintf("%x", (index+1)%15+1)))
			test.mutate(&fact)
			if err := insertFactMigrationRow(ctx, store.db, fact); err == nil {
				t.Fatal("malformed fact insert succeeded")
			}
		})
	}

	watch := validWatchMigrationRow(2000)
	watch.specHash = fmt.Sprintf("%064x", 30000)
	if err := insertWatchMigrationRow(ctx, store.db, watch); err != nil {
		t.Fatal(err)
	}
	verification := initialConfirmedFactVerification(2000)
	if err := insertFactVerificationMigrationRow(ctx, store.db, verification); err != nil {
		t.Fatal(err)
	}
	fact := factFromVerification(watch, verification, factHex("e"))
	if err := insertFactMigrationRow(ctx, store.db, fact); err != nil {
		t.Fatalf("valid fact insert: %v", err)
	}
	second := fact
	second.id = factHex("f")
	if err := insertFactMigrationRow(ctx, store.db, second); err == nil {
		t.Fatal("second open fact for one watch succeeded")
	}
	if _, err := store.db.Exec("UPDATE facts SET value = ? WHERE id = ?", `"mutated"`, fact.id); err == nil {
		t.Fatal("fact identity mutation succeeded")
	}
	if _, err := store.db.Exec("DELETE FROM verifications WHERE verification_id = ?", verification.verificationID); err == nil {
		t.Fatal("fact provenance verification delete succeeded")
	}

	for index, value := range []string{`"text"`, `42`, `1.25`, `true`, `false`} {
		scalarWatch := validWatchMigrationRow(4000 + index)
		scalarWatch.specHash = fmt.Sprintf("%064x", 40000+index)
		scalarVerification := initialConfirmedFactVerification(4000 + index)
		scalarVerification.oldValue = value
		scalarFact := factFromVerification(
			scalarWatch,
			scalarVerification,
			fmt.Sprintf("%064x", 50000+index),
		)
		if err := store.Update(ctx, func(tx WriteTx) error {
			if err := insertWatchMigrationRow(ctx, tx, scalarWatch); err != nil {
				return err
			}
			if err := insertFactVerificationMigrationRow(ctx, tx, scalarVerification); err != nil {
				return err
			}
			return insertFactMigrationRow(ctx, tx, scalarFact)
		}); err != nil {
			t.Fatalf("insert valid JSON scalar %s: %v", value, err)
		}
	}

	compactWatch := validWatchMigrationRow(5000)
	compactWatch.specHash = fmt.Sprintf("%064x", 50000)
	compactVerification := initialConfirmedFactVerification(5000)
	compactVerification.verifiedAt = "2026-08-10T01:00:00Z"
	compactFact := factFromVerification(compactWatch, compactVerification, fmt.Sprintf("%064x", 60000))
	compactFact.observedAt = watchFactT1
	compactFact.validFrom = watchFactT1
	compactFact.lastVerifiedAt = watchFactT1
	if err := store.Update(ctx, func(tx WriteTx) error {
		if err := insertWatchMigrationRow(ctx, tx, compactWatch); err != nil {
			return err
		}
		if err := insertFactVerificationMigrationRow(ctx, tx, compactVerification); err != nil {
			return err
		}
		return insertFactMigrationRow(ctx, tx, compactFact)
	}); err != nil {
		t.Fatalf("insert fact from compact RFC3339Nano verification time: %v", err)
	}
}

func TestFactsConfirmedRefreshRequiresExactOldAndNewProvenance(t *testing.T) {
	store, _, fact := seedOpenFact(t, 1)
	refresh := confirmedFactVerification("refresh", 0, fact, watchFactT2, "b", "receipt-2")
	if err := insertFactVerificationMigrationRow(context.Background(), store.db, refresh); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE facts SET root = ?, source_url = ?, receipt = ?, snapshot_id = ?,
		latest_verification_id = ?, latest_claim_index = ?, last_verified_at = ? WHERE id = ?`,
		"example.com", refresh.finalURL, refresh.receipt, refresh.newSnapshot,
		refresh.verificationID, refresh.claimIndex, watchFactT2, fact.id); err != nil {
		t.Fatalf("confirmed refresh: %v", err)
	}

	var receipt, snapshot, latestID, verifiedAt string
	if err := store.db.QueryRow(`SELECT receipt, snapshot_id, latest_verification_id,
		last_verified_at FROM facts WHERE id = ?`, fact.id).Scan(&receipt, &snapshot, &latestID, &verifiedAt); err != nil {
		t.Fatal(err)
	}
	if receipt != "receipt-2" || snapshot != factSnapshot("b") || latestID != "refresh" || verifiedAt != watchFactT2 {
		t.Fatalf("refreshed fact = receipt=%q snapshot=%q latest=%q time=%q", receipt, snapshot, latestID, verifiedAt)
	}

	bad := confirmedFactVerification("bad-refresh", 0, fact, watchFactT3, "c", "receipt-3")
	bad.oldReceipt = "not-the-current-receipt"
	if err := insertFactVerificationMigrationRow(context.Background(), store.db, bad); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE facts SET receipt = ?, snapshot_id = ?,
		latest_verification_id = ?, latest_claim_index = ?, last_verified_at = ? WHERE id = ?`,
		bad.receipt, bad.newSnapshot, bad.verificationID, bad.claimIndex, watchFactT3, fact.id); err == nil {
		t.Fatal("refresh from mismatched old receipt succeeded")
	}

	current := fact
	current.sourceURL = refresh.finalURL
	current.receipt = refresh.receipt.(string)
	current.snapshot = refresh.newSnapshot.(string)
	current.latestVerificationID = refresh.verificationID
	current.latestClaimIndex = refresh.claimIndex
	current.lastVerifiedAt = watchFactT2
	badTime := confirmedFactVerification("bad-refresh-time", 0, current, watchFactT3, "d", "receipt-4")
	if err := insertFactVerificationMigrationRow(context.Background(), store.db, badTime); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE facts SET receipt = ?, snapshot_id = ?,
		latest_verification_id = ?, latest_claim_index = ?, last_verified_at = ? WHERE id = ?`,
		badTime.receipt, badTime.newSnapshot, badTime.verificationID, badTime.claimIndex,
		"2026-08-10T03:00:00.000000001Z", fact.id); err == nil {
		t.Fatal("confirmed refresh accepted a one-nanosecond provenance mismatch")
	}
}

func TestFactsChangedCloseThenDeferredSuccessorIsAtomicAndHalfOpen(t *testing.T) {
	store, watch, fact := seedOpenFact(t, 10)
	changed := changedFactVerification("changed", 0, fact, watchFactT2, `"29"`, "b", "receipt-2")
	if err := insertFactVerificationMigrationRow(context.Background(), store.db, changed); err != nil {
		t.Fatal(err)
	}
	successorID := factHex("c")
	successor := factFromChangedVerification(watch, changed, successorID)

	if err := store.Update(context.Background(), func(tx WriteTx) error {
		if _, err := tx.ExecContext(context.Background(), `UPDATE facts SET valid_to = ?,
			closed_verification_id = ?, closed_claim_index = ?, superseded_by = ?,
			closed_outcome = 'changed' WHERE id = ?`, watchFactT2, changed.verificationID,
			changed.claimIndex, successorID, fact.id); err != nil {
			return err
		}
		return insertFactMigrationRow(context.Background(), tx, successor)
	}); err != nil {
		t.Fatalf("changed fact transaction: %v", err)
	}

	for _, query := range []struct {
		at, want string
	}{
		{"2026-08-10T01:59:59.999999999Z", fact.id},
		{watchFactT2, successorID},
	} {
		var got string
		if err := store.db.QueryRow(`SELECT id FROM facts WHERE subject = ? AND predicate = ? AND
			valid_from <= ? AND (valid_to IS NULL OR valid_to > ?)`, watch.subject, watch.predicate,
			query.at, query.at).Scan(&got); err != nil {
			t.Fatalf("as_of %s: %v", query.at, err)
		}
		if got != query.want {
			t.Fatalf("as_of %s fact = %q, want %q", query.at, got, query.want)
		}
	}
	var oldTo, oldOutcome, oldSuccessor, newValue string
	if err := store.db.QueryRow(`SELECT old.valid_to, old.closed_outcome, old.superseded_by,
		new.value FROM facts old JOIN facts new ON new.id = old.superseded_by WHERE old.id = ?`,
		fact.id).Scan(&oldTo, &oldOutcome, &oldSuccessor, &newValue); err != nil {
		t.Fatal(err)
	}
	if oldTo != watchFactT2 || oldOutcome != "changed" || oldSuccessor != successorID || newValue != `"29"` {
		t.Fatalf("changed lineage = %q/%q/%q/%q", oldTo, oldOutcome, oldSuccessor, newValue)
	}
	assertNoForeignKeyViolations(t, store.db)

	if _, err := store.db.Exec("UPDATE facts SET last_verified_at = last_verified_at WHERE id = ?", fact.id); err == nil {
		t.Fatal("closed fact update succeeded")
	}
}

func TestFactsChangedClosureWithoutSuccessorRollsBackAtCommit(t *testing.T) {
	store, _, fact := seedOpenFact(t, 20)
	changed := changedFactVerification("orphan-change", 0, fact, watchFactT2, `"31"`, "c", "receipt-c")
	if err := insertFactVerificationMigrationRow(context.Background(), store.db, changed); err != nil {
		t.Fatal(err)
	}
	err := store.Update(context.Background(), func(tx WriteTx) error {
		_, err := tx.ExecContext(context.Background(), `UPDATE facts SET valid_to = ?,
			closed_verification_id = ?, closed_claim_index = ?, superseded_by = ?,
			closed_outcome = 'changed' WHERE id = ?`, watchFactT2, changed.verificationID,
			changed.claimIndex, factHex("f"), fact.id)
		return err
	})
	if err == nil {
		t.Fatal("changed closure without successor committed")
	}
	var validTo sql.NullString
	if err := store.db.QueryRow("SELECT valid_to FROM facts WHERE id = ?", fact.id).Scan(&validTo); err != nil {
		t.Fatal(err)
	}
	if validTo.Valid {
		t.Fatalf("orphan closure was not rolled back: %q", validTo.String)
	}
}

func TestFactsChangedClosureRejectsExistingCrossWatchSuccessor(t *testing.T) {
	store, _, fact := seedOpenFact(t, 40)
	ctx := context.Background()
	otherWatch := validWatchMigrationRow(9040)
	otherWatch.specHash = fmt.Sprintf("%064x", 9040)
	otherWatch.subject = "different subject"
	otherWatch.predicate = "different_predicate"
	otherVerification := initialConfirmedFactVerification(9040)
	otherVerification.rowID = "other-initial-row"
	otherVerification.verificationID = "other-initial"
	otherVerification.url = "https://other.example/limits"
	otherVerification.finalURL = otherVerification.url
	otherVerification.path = otherWatch.predicate
	otherVerification.oldValue = `99`
	otherFact := factFromVerification(otherWatch, otherVerification, factHex("e"))
	otherFact.root = "other.example"
	if err := store.Update(ctx, func(tx WriteTx) error {
		if err := insertWatchMigrationRow(ctx, tx, otherWatch); err != nil {
			return err
		}
		if err := insertFactVerificationMigrationRow(ctx, tx, otherVerification); err != nil {
			return err
		}
		return insertFactMigrationRow(ctx, tx, otherFact)
	}); err != nil {
		t.Fatalf("seed unrelated successor candidate: %v", err)
	}

	changed := changedFactVerification("cross-watch-change", 0, fact, watchFactT2, `20`, "b", "receipt-2")
	if err := insertFactVerificationMigrationRow(ctx, store.db, changed); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE facts SET valid_to = ?,
		closed_verification_id = ?, closed_claim_index = ?, superseded_by = ?,
		closed_outcome = 'changed' WHERE id = ?`, watchFactT2, changed.verificationID,
		changed.claimIndex, otherFact.id, fact.id); err == nil {
		t.Fatal("changed fact linked to an existing cross-watch successor")
	}
	var validTo sql.NullString
	if err := store.db.QueryRow("SELECT valid_to FROM facts WHERE id = ?", fact.id).Scan(&validTo); err != nil {
		t.Fatal(err)
	}
	if validTo.Valid {
		t.Fatalf("rejected cross-watch closure persisted valid_to %q", validTo.String)
	}
}

func TestFactsChangedSuccessorRequiresTheClosingVerification(t *testing.T) {
	store, watch, fact := seedOpenFact(t, 50)
	ctx := context.Background()
	changed := changedFactVerification("required-change", 0, fact, watchFactT2, `20`, "b", "receipt-2")
	if err := insertFactVerificationMigrationRow(ctx, store.db, changed); err != nil {
		t.Fatal(err)
	}
	unrelated := confirmedFactVerification("unrelated-confirmed", 0, fact, watchFactT2, "c", "receipt-3")
	unrelated.oldValue = `20`
	if err := insertFactVerificationMigrationRow(ctx, store.db, unrelated); err != nil {
		t.Fatal(err)
	}
	successorID := factHex("f")
	successor := factFromVerification(watch, unrelated, successorID)
	err := store.Update(ctx, func(tx WriteTx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE facts SET valid_to = ?,
			closed_verification_id = ?, closed_claim_index = ?, superseded_by = ?,
			closed_outcome = 'changed' WHERE id = ?`, watchFactT2, changed.verificationID,
			changed.claimIndex, successorID, fact.id); err != nil {
			return err
		}
		return insertFactMigrationRow(ctx, tx, successor)
	})
	if err == nil {
		t.Fatal("changed successor created from an unrelated confirmed verification")
	}
	var validTo sql.NullString
	if err := store.db.QueryRow("SELECT valid_to FROM facts WHERE id = ?", fact.id).Scan(&validTo); err != nil {
		t.Fatal(err)
	}
	if validTo.Valid {
		t.Fatalf("mismatched successor transaction persisted valid_to %q", validTo.String)
	}
}

func TestFactsGoneClosureHasNoSuccessorAndCannotBeRewritten(t *testing.T) {
	store, _, fact := seedOpenFact(t, 30)
	gone := goneFactVerification("gone", 0, fact, watchFactT2, "page")
	if err := insertFactVerificationMigrationRow(context.Background(), store.db, gone); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE facts SET valid_to = ?, closed_verification_id = ?,
		closed_claim_index = ?, closed_outcome = 'gone', gone_scope = 'page' WHERE id = ?`,
		"2026-08-10T02:00:00.000000001Z", gone.verificationID, gone.claimIndex, fact.id); err == nil {
		t.Fatal("gone closure accepted a one-nanosecond provenance mismatch")
	}
	if _, err := store.db.Exec(`UPDATE facts SET valid_to = ?, closed_verification_id = ?,
		closed_claim_index = ?, closed_outcome = 'gone', gone_scope = 'page' WHERE id = ?`,
		watchFactT2, gone.verificationID, gone.claimIndex, fact.id); err != nil {
		t.Fatalf("gone closure: %v", err)
	}
	var openCount int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM facts WHERE watch_id = ? AND valid_to IS NULL", fact.watchID).Scan(&openCount); err != nil {
		t.Fatal(err)
	}
	if openCount != 0 {
		t.Fatalf("open facts after gone = %d", openCount)
	}
	if _, err := store.db.Exec("UPDATE facts SET superseded_by = ? WHERE id = ?", factHex("e"), fact.id); err == nil {
		t.Fatal("gone fact was assigned a successor")
	}
	if _, err := store.db.Exec("DELETE FROM facts WHERE id = ?", fact.id); err == nil {
		t.Fatal("physical fact delete succeeded")
	}
	if _, err := store.db.Exec("DELETE FROM watches WHERE id = ?", fact.watchID); err == nil {
		t.Fatal("fact-owning watch delete succeeded")
	}
}

func TestWatchFactMigrationUpgradesVersionEightWithoutChangingOldRows(t *testing.T) {
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
	for index := 0; index < 8; index++ {
		if _, err := db.Exec(migrations[index]); err != nil {
			t.Fatalf("apply migration %03d: %v", index+1, err)
		}
		if _, err := db.Exec("INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)", index+1, watchFactT0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO verifications (
		id, verification_id, claim_index, url, host, path, old_value,
		outcome, old_snapshot_id, verified_at
	) VALUES ('legacy-row', 'legacy-batch', 0, 'https://legacy.example/item',
		'legacy.example', 'price', '19', 'confirmed', 'sha256:legacy', ?)`, watchFactT0); err != nil {
		t.Fatalf("seed v8 row: %v", err)
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
		t.Fatalf("Open(reopen) error = %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	var versions, watches, facts, legacy int
	if err := reopened.db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if err := reopened.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'watches'").Scan(&watches); err != nil {
		t.Fatal(err)
	}
	if err := reopened.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'facts'").Scan(&facts); err != nil {
		t.Fatal(err)
	}
	if err := reopened.db.QueryRow("SELECT COUNT(*) FROM verifications WHERE id = 'legacy-row'").Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	if versions != len(migrations) || watches != 1 || facts != 1 || legacy != 1 {
		t.Fatalf("upgrade state migrations=%d/%d watches=%d facts=%d legacy=%d",
			versions, len(migrations), watches, facts, legacy)
	}
	assertNoForeignKeyViolations(t, reopened.db)
}

func validWatchMigrationRow(index int) watchMigrationRow {
	return watchMigrationRow{
		id:        watchUUID(index),
		specHash:  factHex("a"),
		subject:   "anthropic claude-fable-5",
		predicate: "price_per_mtok_input",
		freshness: "week",
		minimum:   2,
		conflict:  "expose",
		state:     "pending",
		nextCheck: watchFactT1,
		ewma:      86400.0,
		failures:  0,
		lastError: "",
		createdAt: watchFactT0,
		updatedAt: watchFactT0,
	}
}

func insertWatchMigrationRow(ctx context.Context, executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, row watchMigrationRow) error {
	_, err := executor.ExecContext(ctx, `INSERT INTO watches (
		id, spec_hash, subject, predicate, freshness, min_independent_sources,
		on_conflict, state, next_check_at, ewma_interval_s, last_change_at,
		last_checked_at, consecutive_failures, last_error_code,
		last_verification_id, last_verification_claim_index, lease_id, lease_until,
		created_at, updated_at, paused_at, deleted_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.id, row.specHash, row.subject, row.predicate, row.freshness, row.minimum,
		row.conflict, row.state, row.nextCheck, row.ewma, row.lastChange,
		row.lastChecked, row.failures, row.lastError, row.lastVerificationID,
		row.lastVerificationIndex, row.leaseID, row.leaseUntil, row.createdAt,
		row.updatedAt, row.pausedAt, row.deletedAt)
	return err
}

func initialConfirmedFactVerification(index int) factVerificationMigrationRow {
	return factVerificationMigrationRow{
		rowID:          fmt.Sprintf("initial-row-%d", index),
		verificationID: fmt.Sprintf("initial-verification-%d", index),
		claimIndex:     0,
		url:            "https://example.com/pricing",
		finalURL:       "https://example.com/pricing",
		path:           "price_per_mtok_input",
		oldValue:       `"19"`,
		outcome:        "confirmed",
		oldSnapshot:    factSnapshot("0"),
		newSnapshot:    factSnapshot("a"),
		oldReceipt:     "baseline-receipt",
		receipt:        "receipt-1",
		verifiedAt:     watchFactT1,
	}
}

func confirmedFactVerification(id string, index int, fact factMigrationRow, at, snapshotDigit, receipt string) factVerificationMigrationRow {
	return factVerificationMigrationRow{
		rowID:          "row-" + id,
		verificationID: id,
		claimIndex:     index,
		url:            fact.sourceURL,
		finalURL:       fact.sourceURL,
		path:           fact.path,
		oldValue:       fact.value,
		outcome:        "confirmed",
		oldSnapshot:    fact.snapshot,
		newSnapshot:    factSnapshot(snapshotDigit),
		oldReceipt:     fact.receipt,
		receipt:        receipt,
		verifiedAt:     at,
	}
}

func changedFactVerification(id string, index int, fact factMigrationRow, at, newValue, snapshotDigit, receipt string) factVerificationMigrationRow {
	row := confirmedFactVerification(id, index, fact, at, snapshotDigit, receipt)
	row.outcome = "changed"
	row.newValue = newValue
	return row
}

func goneFactVerification(id string, index int, fact factMigrationRow, at, scope string) factVerificationMigrationRow {
	return factVerificationMigrationRow{
		rowID:          "row-" + id,
		verificationID: id,
		claimIndex:     index,
		url:            fact.sourceURL,
		path:           fact.path,
		oldValue:       fact.value,
		outcome:        "gone",
		goneScope:      scope,
		oldSnapshot:    fact.snapshot,
		oldReceipt:     fact.receipt,
		verifiedAt:     at,
	}
}

func insertFactVerificationMigrationRow(ctx context.Context, executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, row factVerificationMigrationRow) error {
	var finalURL any
	if row.finalURL != "" {
		finalURL = row.finalURL
	}
	_, err := executor.ExecContext(ctx, `INSERT INTO verifications (
		id, verification_id, claim_index, url, host, final_url, path,
		old_value, new_value, outcome, gone_scope, old_snapshot_id,
		new_snapshot_id, old_receipt, receipt, verified_at
	) VALUES (?, ?, ?, ?, 'example.com', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.rowID, row.verificationID, row.claimIndex, row.url, finalURL, row.path,
		row.oldValue, row.newValue, row.outcome, row.goneScope, row.oldSnapshot,
		row.newSnapshot, row.oldReceipt, row.receipt, row.verifiedAt)
	return err
}

func factFromVerification(watch watchMigrationRow, verification factVerificationMigrationRow, id string) factMigrationRow {
	value := verification.oldValue
	if verification.outcome == "changed" {
		value = verification.newValue.(string)
	}
	return factMigrationRow{
		id:                    id,
		watchID:               watch.id,
		subject:               watch.subject,
		predicate:             watch.predicate,
		path:                  verification.path,
		value:                 value,
		root:                  "example.com",
		sourceURL:             verification.finalURL,
		receipt:               verification.receipt.(string),
		snapshot:              verification.newSnapshot.(string),
		createdVerificationID: verification.verificationID,
		createdClaimIndex:     verification.claimIndex,
		latestVerificationID:  verification.verificationID,
		latestClaimIndex:      verification.claimIndex,
		observedAt:            verification.verifiedAt,
		validFrom:             verification.verifiedAt,
		lastVerifiedAt:        verification.verifiedAt,
		closedOutcome:         "",
		goneScope:             "",
	}
}

func factFromChangedVerification(watch watchMigrationRow, verification factVerificationMigrationRow, id string) factMigrationRow {
	return factFromVerification(watch, verification, id)
}

func insertFactMigrationRow(ctx context.Context, executor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, row factMigrationRow) error {
	_, err := executor.ExecContext(ctx, `INSERT INTO facts (
		id, watch_id, subject, predicate, path, value, root, source_url, receipt,
		snapshot_id, created_verification_id, created_claim_index,
		latest_verification_id, latest_claim_index, closed_verification_id,
		closed_claim_index, observed_at, valid_from, valid_to, last_verified_at,
		superseded_by, closed_outcome, gone_scope
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.id, row.watchID, row.subject, row.predicate, row.path, row.value, row.root,
		row.sourceURL, row.receipt, row.snapshot, row.createdVerificationID,
		row.createdClaimIndex, row.latestVerificationID, row.latestClaimIndex,
		row.closedVerificationID, row.closedClaimIndex, row.observedAt, row.validFrom,
		row.validTo, row.lastVerifiedAt, row.supersededBy, row.closedOutcome, row.goneScope)
	return err
}

func seedOpenFact(t *testing.T, index int) (*Store, watchMigrationRow, factMigrationRow) {
	t.Helper()
	store := openTestStore(t)
	watch := validWatchMigrationRow(3000 + index)
	watch.specHash = factHex(fmt.Sprintf("%x", index%15+1))
	verification := initialConfirmedFactVerification(3000 + index)
	fact := factFromVerification(watch, verification, factHex(fmt.Sprintf("%x", index%15+1)))
	if err := store.Update(context.Background(), func(tx WriteTx) error {
		if err := insertWatchMigrationRow(context.Background(), tx, watch); err != nil {
			return err
		}
		if err := insertFactVerificationMigrationRow(context.Background(), tx, verification); err != nil {
			return err
		}
		return insertFactMigrationRow(context.Background(), tx, fact)
	}); err != nil {
		t.Fatalf("seed open fact: %v", err)
	}
	return store, watch, fact
}

func assertNoForeignKeyViolations(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		var table string
		var rowID any
		var parent string
		var constraint int
		if err := rows.Scan(&table, &rowID, &parent, &constraint); err != nil {
			t.Fatal(err)
		}
		t.Fatalf("foreign key violation table=%s row=%v parent=%s constraint=%d", table, rowID, parent, constraint)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func watchUUID(index int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", index)
}

func factHex(digit string) string {
	return strings.Repeat(digit, 64)
}

func factSnapshot(digit string) string {
	return "sha256:" + factHex(digit)
}
