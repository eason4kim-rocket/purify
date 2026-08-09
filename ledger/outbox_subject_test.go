package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEnqueueOutboxUsesDurableHealRunSubject(t *testing.T) {
	store := openTestStore(t)
	createdAt := outboxTestTime()
	promotedID := seedPendingHealRun(
		t,
		store,
		"11111111-2222-3333-4444-555555555555",
		1,
		true,
	)
	event := validHealOutboxEvent(
		"11111111-2222-3333-4444-555555555555",
		"heal-event-first",
		ExtractorPromotedEvent,
		createdAt,
	)

	if err := store.Update(context.Background(), func(tx WriteTx) error {
		if err := terminalizeHealRun(
			context.Background(),
			tx,
			event.SubjectID,
			"promoted",
			promotedID,
		); err != nil {
			return err
		}
		return EnqueueOutbox(context.Background(), tx, event)
	}); err != nil {
		t.Fatalf("enqueue heal event: %v", err)
	}

	pending := pendingOutbox(t, store, createdAt)
	if len(pending) != 1 {
		t.Fatalf("pending len = %d, want 1", len(pending))
	}
	got := pending[0]
	if got.ID != event.ID || got.VerificationID != "" ||
		got.SubjectType != SubjectExtractorHeal || got.SubjectID != event.SubjectID ||
		got.Type != ExtractorPromotedEvent || string(got.Payload) != string(event.Payload) {
		t.Fatalf("pending heal event = %#v", got)
	}

	// The logical subject key, not a freshly generated delivery ID, owns
	// idempotency across a retried promotion transaction.
	retry := event
	retry.ID = "heal-event-retry-id"
	if err := store.Update(context.Background(), func(tx WriteTx) error {
		return EnqueueOutbox(context.Background(), tx, retry)
	}); err != nil {
		t.Fatalf("idempotent enqueue retry: %v", err)
	}
	assertTableCount(t, store.db, "outbox_events", 1)
	if got := pendingOutbox(t, store, createdAt)[0].ID; got != event.ID {
		t.Fatalf("durable delivery id = %q, want first id %q", got, event.ID)
	}

	drifted := event
	drifted.Payload = json.RawMessage(`{"type":"extractor.promoted","different":true}`)
	err := store.Update(context.Background(), func(tx WriteTx) error {
		return EnqueueOutbox(context.Background(), tx, drifted)
	})
	if !errors.Is(err, ErrOutboxConflict) {
		t.Fatalf("logical drift error = %v, want ErrOutboxConflict", err)
	}
	if strings.Contains(err.Error(), "different") || strings.Contains(err.Error(), event.URL) ||
		strings.Contains(err.Error(), event.Secret) {
		t.Fatalf("logical conflict leaked event content: %v", err)
	}

	reusedID := validHealOutboxEvent(
		"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		event.ID,
		ExtractorDegradedEvent,
		createdAt,
	)
	seedPendingHealRun(t, store, reusedID.SubjectID, 2, false)
	err = store.Update(context.Background(), func(tx WriteTx) error {
		if err := terminalizeHealRun(
			context.Background(),
			tx,
			reusedID.SubjectID,
			"degraded",
			"",
		); err != nil {
			return err
		}
		return EnqueueOutbox(context.Background(), tx, reusedID)
	})
	if !errors.Is(err, ErrOutboxConflict) {
		t.Fatalf("reused id error = %v, want ErrOutboxConflict", err)
	}
}

func TestEnqueueOutboxRollsBackWithCallerTransaction(t *testing.T) {
	store := openTestStore(t)
	event := validHealOutboxEvent(
		"aaaaaaaa-1111-2222-3333-bbbbbbbbbbbb",
		"rolled-back-heal-event",
		ExtractorDegradedEvent,
		outboxTestTime(),
	)
	seedPendingHealRun(t, store, event.SubjectID, 3, false)
	rollback := errors.New("rollback promotion")
	err := store.Update(context.Background(), func(tx WriteTx) error {
		if err := terminalizeHealRun(
			context.Background(),
			tx,
			event.SubjectID,
			"degraded",
			"",
		); err != nil {
			return err
		}
		if err := EnqueueOutbox(context.Background(), tx, event); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("Update() error = %v, want rollback sentinel", err)
	}
	assertTableCount(t, store.db, "outbox_events", 0)
	var state string
	if err := store.db.QueryRow(
		"SELECT state FROM extractor_heal_runs WHERE id = ?",
		event.SubjectID,
	).Scan(&state); err != nil {
		t.Fatalf("read rolled-back heal state: %v", err)
	}
	if state != "pending" {
		t.Fatalf("heal state after transaction rollback = %q, want pending", state)
	}
}

func TestEnqueueOutboxRequiresMatchingTerminalHealRun(t *testing.T) {
	createdAt := outboxTestTime()
	t.Run("orphan", func(t *testing.T) {
		store := openTestStore(t)
		event := validHealOutboxEvent(
			"11111111-2222-3333-4444-555555555555",
			"orphan-heal-event",
			ExtractorPromotedEvent,
			createdAt,
		)
		err := store.Update(context.Background(), func(tx WriteTx) error {
			return EnqueueOutbox(context.Background(), tx, event)
		})
		if !errors.Is(err, ErrInvalidOutboxEvent) {
			t.Fatalf("orphan error = %v, want ErrInvalidOutboxEvent", err)
		}
		assertTableCount(t, store.db, "outbox_events", 0)
	})

	t.Run("pending", func(t *testing.T) {
		store := openTestStore(t)
		event := validHealOutboxEvent(
			"aaaaaaaa-2222-3333-4444-bbbbbbbbbbbb",
			"pending-heal-event",
			ExtractorDegradedEvent,
			createdAt,
		)
		seedPendingHealRun(t, store, event.SubjectID, 4, false)
		err := store.Update(context.Background(), func(tx WriteTx) error {
			return EnqueueOutbox(context.Background(), tx, event)
		})
		if !errors.Is(err, ErrInvalidOutboxEvent) {
			t.Fatalf("pending error = %v, want ErrInvalidOutboxEvent", err)
		}
		assertTableCount(t, store.db, "outbox_events", 0)
	})

	t.Run("wrong terminal type", func(t *testing.T) {
		store := openTestStore(t)
		runID := "bbbbbbbb-2222-3333-4444-cccccccccccc"
		seedPendingHealRun(t, store, runID, 5, false)
		if err := store.Update(context.Background(), func(tx WriteTx) error {
			return terminalizeHealRun(context.Background(), tx, runID, "degraded", "")
		}); err != nil {
			t.Fatalf("terminalize degraded run: %v", err)
		}
		event := validHealOutboxEvent(runID, "wrong-terminal-event", ExtractorPromotedEvent, createdAt)
		err := store.Update(context.Background(), func(tx WriteTx) error {
			return EnqueueOutbox(context.Background(), tx, event)
		})
		if !errors.Is(err, ErrInvalidOutboxEvent) {
			t.Fatalf("wrong terminal error = %v, want ErrInvalidOutboxEvent", err)
		}
		assertTableCount(t, store.db, "outbox_events", 0)
	})
}

func TestEnqueueOutboxStrictSubjectValidation(t *testing.T) {
	base := validHealOutboxEvent(
		"11111111-2222-3333-4444-555555555555",
		"validated-heal-event",
		ExtractorDegradedEvent,
		outboxTestTime(),
	)
	tests := []struct {
		name   string
		mutate func(*OutboxEvent)
	}{
		{name: "missing subject type", mutate: func(event *OutboxEvent) { event.SubjectType = "" }},
		{name: "missing subject id", mutate: func(event *OutboxEvent) { event.SubjectID = "" }},
		{name: "unsupported subject", mutate: func(event *OutboxEvent) { event.SubjectType = "extractor" }},
		{name: "verification alias on heal", mutate: func(event *OutboxEvent) { event.VerificationID = event.SubjectID }},
		{name: "uppercase heal uuid", mutate: func(event *OutboxEvent) { event.SubjectID = "AAAAAAAA-1111-2222-3333-BBBBBBBBBBBB" }},
		{name: "malformed heal uuid", mutate: func(event *OutboxEvent) { event.SubjectID = "not-a-uuid" }},
		{name: "unsupported heal type", mutate: func(event *OutboxEvent) { event.Type = "fact.changed" }},
		{name: "invalid url", mutate: func(event *OutboxEvent) { event.URL = "https://user:secret@hooks.example/event" }},
		{name: "invalid payload", mutate: func(event *OutboxEvent) { event.Payload = json.RawMessage(`{`) }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			event := base
			test.mutate(&event)
			err := store.Update(context.Background(), func(tx WriteTx) error {
				return EnqueueOutbox(context.Background(), tx, event)
			})
			if !errors.Is(err, ErrInvalidOutboxEvent) {
				t.Fatalf("error = %v, want ErrInvalidOutboxEvent", err)
			}
			if strings.Contains(err.Error(), "user:secret") || strings.Contains(err.Error(), string(event.Payload)) {
				t.Fatalf("validation error leaked URL or payload: %v", err)
			}
			assertTableCount(t, store.db, "outbox_events", 0)
		})
	}

	if err := EnqueueOutbox(context.Background(), nil, base); !errors.Is(err, ErrInvalidOutboxEvent) {
		t.Fatalf("nil transaction error = %v, want ErrInvalidOutboxEvent", err)
	}
}

func TestEnqueueOutboxRejectsDetachedVerificationSubject(t *testing.T) {
	store := openTestStore(t)
	event := validOutboxEvent("unused", "explicit-verification-event", outboxTestTime())
	event.VerificationID = ""
	event.SubjectType = SubjectVerification
	event.SubjectID = "existing-verification-contract"

	err := store.Update(context.Background(), func(tx WriteTx) error {
		return EnqueueOutbox(context.Background(), tx, event)
	})
	if !errors.Is(err, ErrInvalidOutboxEvent) {
		t.Fatalf("detached verification error = %v, want ErrInvalidOutboxEvent", err)
	}
	assertTableCount(t, store.db, "outbox_events", 0)
}

func TestRecordVerificationBatchRetryIgnoresMutableDeliveryState(t *testing.T) {
	store := openTestStore(t)
	createdAt := outboxTestTime()
	row := outboxVerification("verification-delivery-progress", 0)
	event := validOutboxEvent(row.VerificationID, "delivery-progress-event", createdAt)
	if err := store.RecordVerificationBatch(context.Background(), []Verification{row}, &event); err != nil {
		t.Fatalf("record verification batch: %v", err)
	}
	attemptedAt := createdAt.Add(time.Second)
	nextAttemptAt := createdAt.Add(time.Minute)
	if err := store.MarkOutboxAttempt(
		context.Background(),
		event.ID,
		attemptedAt,
		nextAttemptAt,
		"temporary failure",
	); err != nil {
		t.Fatalf("mark delivery attempt: %v", err)
	}

	retry := event
	retry.ID = "new-generated-delivery-id"
	if err := store.RecordVerificationBatch(context.Background(), []Verification{row}, &retry); err != nil {
		t.Fatalf("retry after delivery progress: %v", err)
	}
	got := loadOutboxEventForTest(t, store, event.ID)
	if got.ID != event.ID || got.Attempts != 1 || got.LastAttemptAt == nil ||
		!got.LastAttemptAt.Equal(attemptedAt) || got.LastError != "temporary failure" ||
		!got.NextAttemptAt.Equal(nextAttemptAt) {
		t.Fatalf("delivery progress changed by batch retry: %#v", got)
	}
}

func TestMigration007PreservesMigration002OutboxRows(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, Filename)
	dsn := (&url.URL{Scheme: "file", Path: dbPath}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	for index := 0; index < 2; index++ {
		if _, err := db.Exec(migrations[index]); err != nil {
			t.Fatalf("apply migration %03d: %v", index+1, err)
		}
		if _, err := db.Exec(
			"INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)",
			index+1,
			formatOutboxTime(outboxTestTime()),
		); err != nil {
			t.Fatalf("record migration %03d: %v", index+1, err)
		}
	}
	createdAt := outboxTestTime()
	lastAttemptAt := createdAt.Add(time.Second)
	nextAttemptAt := createdAt.Add(time.Minute)
	for index, verificationID := range []string{
		"historical-verification-id",
		"historical-delivered-verification",
	} {
		if _, err := db.Exec(`INSERT INTO verifications (
			id, verification_id, claim_index, url, host, path, old_value,
			outcome, old_snapshot_id, verified_at
		) VALUES (?, ?, 0, 'https://example.com/value', 'example.com',
			'value', '"old"', 'confirmed', 'sha256:old', ?)`,
			"historical-verification-row-"+time.Unix(int64(index), 0).UTC().Format("150405"),
			verificationID,
			formatOutboxTime(createdAt),
		); err != nil {
			t.Fatalf("insert migration 001 verification row: %v", err)
		}
	}
	if _, err := db.Exec(`INSERT INTO outbox_events (
		id, verification_id, event_type, destination_url, secret, payload,
		created_at, attempt_count, last_attempt_at, last_error, next_attempt_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, 2, ?, ?, ?)`,
		"historical-event",
		"historical-verification-id",
		"legacy.custom-event",
		"https://hooks.example/historical",
		"historical-secret",
		`{"type":"legacy.custom-event"}`,
		formatOutboxTime(createdAt),
		formatOutboxTime(lastAttemptAt),
		"historical failure",
		formatOutboxTime(nextAttemptAt),
	); err != nil {
		t.Fatalf("insert migration 002 row: %v", err)
	}
	deliveredAt := createdAt.Add(2 * time.Minute)
	if _, err := db.Exec(`INSERT INTO outbox_events (
		id, verification_id, event_type, destination_url, secret, payload,
		created_at, next_attempt_at, delivered_at
	) VALUES (?, ?, ?, ?, '', ?, ?, ?, ?)`,
		"historical-delivered-event",
		"historical-delivered-verification",
		"fact.changed",
		"https://hooks.example/delivered",
		`{"type":"fact.changed"}`,
		formatOutboxTime(createdAt),
		formatOutboxTime(createdAt),
		formatOutboxTime(deliveredAt),
	); err != nil {
		t.Fatalf("insert delivered migration 002 row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close migration 002 database: %v", err)
	}

	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(upgrade) error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	pending, err := store.PendingOutbox(
		context.Background(),
		nextAttemptAt,
		MaxPendingOutbox,
		MaxPendingOutboxBytes,
	)
	if err != nil {
		t.Fatalf("PendingOutbox(upgraded) error = %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("upgraded pending len = %d, want 1", len(pending))
	}
	got := pending[0]
	if got.ID != "historical-event" || got.VerificationID != "historical-verification-id" ||
		got.SubjectType != SubjectVerification || got.SubjectID != got.VerificationID ||
		got.Type != "legacy.custom-event" || got.URL != "https://hooks.example/historical" ||
		got.Secret != "historical-secret" || string(got.Payload) != `{"type":"legacy.custom-event"}` ||
		got.Attempts != 2 || got.LastAttemptAt == nil || !got.LastAttemptAt.Equal(lastAttemptAt) ||
		got.LastError != "historical failure" || !got.NextAttemptAt.Equal(nextAttemptAt) {
		t.Fatalf("upgraded event = %#v", got)
	}
	var migrationCount int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&migrationCount); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if migrationCount != len(migrations) {
		t.Fatalf("migration count = %d, want %d", migrationCount, len(migrations))
	}
	delivered := loadOutboxEventForTest(t, store, "historical-delivered-event")
	if delivered.SubjectType != SubjectVerification ||
		delivered.SubjectID != "historical-delivered-verification" ||
		delivered.VerificationID != delivered.SubjectID || delivered.DeliveredAt == nil ||
		!delivered.DeliveredAt.Equal(deliveredAt) || delivered.FailedAt != nil {
		t.Fatalf("upgraded delivered event = %#v", delivered)
	}
	assertTableCount(t, store.db, "outbox_events", 2)
}

func TestMigration007OutboxStorageContract(t *testing.T) {
	store := openTestStore(t)
	var definition string
	if err := store.db.QueryRow(`SELECT sql FROM sqlite_master
		WHERE type = 'table' AND name = 'outbox_events'`).Scan(&definition); err != nil {
		t.Fatalf("read outbox table definition: %v", err)
	}
	if !strings.Contains(definition, "STRICT") {
		t.Fatalf("outbox_events is not STRICT: %s", definition)
	}

	rows, err := store.db.Query("PRAGMA table_info(outbox_events)")
	if err != nil {
		t.Fatalf("PRAGMA table_info(outbox_events): %v", err)
	}
	defer rows.Close()
	foundID := false
	for rows.Next() {
		var (
			cid, notNull, primaryKey int
			name, columnType         string
			defaultValue             any
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatalf("scan outbox table_info: %v", err)
		}
		if name == "id" {
			foundID = true
			if columnType != "TEXT" || notNull != 1 || primaryKey != 1 {
				t.Fatalf("outbox id metadata = type %q notnull %d pk %d", columnType, notNull, primaryKey)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate outbox table_info: %v", err)
	}
	if !foundID {
		t.Fatal("outbox id column is missing")
	}
}

func TestMigration007RejectsDetachedVerificationDirectSQL(t *testing.T) {
	store := openTestStore(t)
	now := formatOutboxTime(outboxTestTime())
	insert := func(id, verificationID string) error {
		_, err := store.db.Exec(`INSERT INTO outbox_events (
			id, verification_id, subject_type, subject_id, event_type,
			destination_url, payload, created_at, next_attempt_at
		) VALUES (?, ?, 'verification', ?, 'fact.changed',
			'https://hooks.example/events', '{}', ?, ?)`,
			id,
			verificationID,
			verificationID,
			now,
			now,
		)
		return err
	}
	if err := insert("orphan-verification-event", "orphan-verification"); err == nil {
		t.Fatal("direct event for missing verification batch succeeded")
	}

	row := outboxVerification("durable-verification", 0)
	if err := store.RecordVerifications(context.Background(), []Verification{row}); err != nil {
		t.Fatalf("record verification batch: %v", err)
	}
	if err := insert("durable-verification-event", row.VerificationID); err != nil {
		t.Fatalf("direct event for durable verification batch: %v", err)
	}
	assertTableCount(t, store.db, "outbox_events", 1)
}

func TestMigration007EnforcesOutboxResourceBoundsAndTypes(t *testing.T) {
	store := openTestStore(t)
	row := outboxVerification("resource-verification", 0)
	if err := store.RecordVerifications(context.Background(), []Verification{row}); err != nil {
		t.Fatalf("record verification batch: %v", err)
	}
	now := formatOutboxTime(outboxTestTime())
	insert := `INSERT INTO outbox_events (
		id, verification_id, subject_type, subject_id, event_type,
		destination_url, secret, payload, created_at, next_attempt_at
	) VALUES (?, ?, 'verification', ?, ?, ?, ?, ?, ?, ?)`
	base := []any{
		"bounded-event",
		row.VerificationID,
		row.VerificationID,
		"fact.changed",
		"https://hooks.example/events",
		"secret",
		`{"type":"fact.changed"}`,
		now,
		now,
	}
	tests := []struct {
		name   string
		mutate func([]any)
	}{
		{name: "null id", mutate: func(values []any) { values[0] = nil }},
		{name: "blob id", mutate: func(values []any) { values[0] = []byte("blob-id") }},
		{name: "oversized id", mutate: func(values []any) { values[0] = strings.Repeat("i", MaxOutboxIDBytes+1) }},
		{name: "oversized type", mutate: func(values []any) { values[3] = strings.Repeat("t", MaxOutboxTypeBytes+1) }},
		{name: "oversized url", mutate: func(values []any) {
			values[4] = "https://hooks.example/" + strings.Repeat("u", MaxOutboxURLBytes)
		}},
		{name: "oversized secret", mutate: func(values []any) { values[5] = strings.Repeat("s", MaxOutboxSecretBytes+1) }},
		{name: "blob payload", mutate: func(values []any) { values[6] = []byte(`{}`) }},
		{name: "invalid created type", mutate: func(values []any) { values[7] = []byte(now) }},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := append([]any(nil), base...)
			values[0] = "invalid-resource-" + time.Unix(int64(index), 0).UTC().Format("150405")
			test.mutate(values)
			if _, err := store.db.Exec(insert, values...); err == nil {
				t.Fatal("invalid direct outbox insert succeeded")
			}
		})
	}

	t.Run("oversized payload", func(t *testing.T) {
		values := append([]any(nil), base...)
		values[0] = "oversized-payload-event"
		values[6] = `"` + strings.Repeat("p", MaxOutboxPayloadBytes) + `"`
		if _, err := store.db.Exec(insert, values...); err == nil {
			t.Fatal("oversized valid JSON payload insert succeeded")
		}
	})
	assertTableCount(t, store.db, "outbox_events", 0)
}

func TestMigration007EnforcesSubjectInvariants(t *testing.T) {
	store := openTestStore(t)
	insert := `INSERT INTO outbox_events (
		id, verification_id, subject_type, subject_id, event_type,
		destination_url, payload, created_at, next_attempt_at
	) VALUES (?, ?, ?, ?, ?, 'https://hooks.example/events', '{}', ?, ?)`
	now := formatOutboxTime(outboxTestTime())
	tests := []struct {
		name           string
		verificationID any
		subjectType    string
		subjectID      string
		eventType      string
	}{
		{name: "unsupported subject", subjectType: "extractor", subjectID: "value", eventType: "extractor.promoted"},
		{name: "verification alias missing", subjectType: SubjectVerification, subjectID: "one", eventType: "fact.changed"},
		{name: "verification mismatch", verificationID: "one", subjectType: SubjectVerification, subjectID: "two", eventType: "fact.changed"},
		{name: "heal verification alias", verificationID: "heal", subjectType: SubjectExtractorHeal, subjectID: "11111111-2222-3333-4444-555555555555", eventType: ExtractorPromotedEvent},
		{name: "heal malformed uuid", subjectType: SubjectExtractorHeal, subjectID: "not-a-uuid", eventType: ExtractorPromotedEvent},
		{name: "heal uppercase uuid", subjectType: SubjectExtractorHeal, subjectID: "AAAAAAAA-1111-2222-3333-BBBBBBBBBBBB", eventType: ExtractorPromotedEvent},
		{name: "heal unsupported event", subjectType: SubjectExtractorHeal, subjectID: "11111111-2222-3333-4444-555555555555", eventType: "fact.changed"},
		{name: "heal run missing", subjectType: SubjectExtractorHeal, subjectID: "99999999-2222-3333-4444-555555555555", eventType: ExtractorPromotedEvent},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := store.db.Exec(
				insert,
				"invalid-subject-"+time.Unix(int64(index), 0).UTC().Format("150405"),
				test.verificationID,
				test.subjectType,
				test.subjectID,
				test.eventType,
				now,
				now,
			)
			if err == nil {
				t.Fatal("invalid subject insert error = nil")
			}
		})
	}
	assertTableCount(t, store.db, "outbox_events", 0)
}

func TestMigration007HealTriggerRequiresMatchingTerminalRun(t *testing.T) {
	store := openTestStore(t)
	runID := "77777777-2222-3333-4444-555555555555"
	seedPendingHealRun(t, store, runID, 6, false)
	now := formatOutboxTime(outboxTestTime())
	insert := func(id, eventType string) error {
		_, err := store.db.Exec(`INSERT INTO outbox_events (
			id, verification_id, subject_type, subject_id, event_type,
			destination_url, payload, created_at, next_attempt_at
		) VALUES (?, NULL, ?, ?, ?, 'https://hooks.example/events', '{}', ?, ?)`,
			id,
			SubjectExtractorHeal,
			runID,
			eventType,
			now,
			now,
		)
		return err
	}
	if err := insert("pending-direct-event", ExtractorDegradedEvent); err == nil {
		t.Fatal("direct event for pending heal run succeeded")
	}
	if err := store.Update(context.Background(), func(tx WriteTx) error {
		return terminalizeHealRun(context.Background(), tx, runID, "degraded", "")
	}); err != nil {
		t.Fatalf("terminalize degraded run: %v", err)
	}
	if err := insert("wrong-terminal-direct-event", ExtractorPromotedEvent); err == nil {
		t.Fatal("direct promoted event for degraded heal run succeeded")
	}
	if err := insert("matching-terminal-direct-event", ExtractorDegradedEvent); err != nil {
		t.Fatalf("direct degraded event for degraded heal run: %v", err)
	}
	for _, query := range []string{
		"UPDATE outbox_events SET payload = '{\"changed\":true}' WHERE id = 'matching-terminal-direct-event'",
		"UPDATE outbox_events SET subject_id = '88888888-2222-3333-4444-555555555555' WHERE id = 'matching-terminal-direct-event'",
	} {
		if _, err := store.db.Exec(query); err == nil {
			t.Fatalf("immutable outbox update succeeded: %s", query)
		}
	}
	assertTableCount(t, store.db, "outbox_events", 1)
}

func validHealOutboxEvent(subjectID, id, eventType string, createdAt time.Time) OutboxEvent {
	return OutboxEvent{
		ID:          id,
		SubjectType: SubjectExtractorHeal,
		SubjectID:   subjectID,
		Type:        eventType,
		URL:         "https://hooks.example/extractors",
		Secret:      "heal-secret",
		Payload:     json.RawMessage(`{"type":"` + eventType + `"}`),
		CreatedAt:   createdAt,
	}
}

func seedPendingHealRun(t *testing.T, store *Store, runID string, index int, withPromoted bool) string {
	t.Helper()
	source := validExtractorMigrationRow(700 + index)
	source.state = "stale"
	source.reason = "template_drift"
	run := validExtractorHealMigrationRow(700+index, source)
	run.id = runID
	run.targetID = repeatHex(string(rune('0' + index)))
	run.targetHash = []byte{0x80, 0, 0, 0, 0, 0, 0, byte(index + 1)}

	var promoted extractorMigrationRow
	if withPromoted {
		promoted = validExtractorMigrationRow(800 + index)
		promoted.clusterID = run.targetID
		promoted.templateHash = run.targetHash
	}
	if err := store.Update(context.Background(), func(tx WriteTx) error {
		if err := insertExtractorMigrationRow(context.Background(), tx, source); err != nil {
			return err
		}
		if withPromoted {
			if err := insertExtractorMigrationRow(context.Background(), tx, promoted); err != nil {
				return err
			}
		}
		return insertExtractorHealMigrationRow(context.Background(), tx, run)
	}); err != nil {
		t.Fatalf("seed pending heal run: %v", err)
	}
	if withPromoted {
		return promoted.id.(string)
	}
	return ""
}

func terminalizeHealRun(
	ctx context.Context,
	tx WriteTx,
	runID string,
	state string,
	promotedID string,
) error {
	if _, err := tx.ExecContext(ctx, `UPDATE extractor_heal_runs SET
		state = 'replaying', lease_id = ?, lease_until = ?, updated_at = ?
		WHERE id = ?`,
		"00000000-0000-4000-8000-000000000088",
		extractorHealLeaseTime,
		extractorMigrationLaterTime,
		runID,
	); err != nil {
		return err
	}
	switch state {
	case "promoted":
		_, err := tx.ExecContext(ctx, `UPDATE extractor_heal_runs SET
			state = 'promoted', terminal_reason = 'replay_passed',
			lease_id = NULL, lease_until = NULL,
			replay_total = 10, replay_matched = 9, replay_ratio = 0.9,
			promoted_extractor_id = ?, updated_at = ?, completed_at = ?
			WHERE id = ?`,
			promotedID,
			extractorHealLeaseTime,
			extractorHealLeaseTime,
			runID,
		)
		return err
	case "degraded":
		_, err := tx.ExecContext(ctx, `UPDATE extractor_heal_runs SET
			state = 'degraded', terminal_reason = 'replay_below_threshold',
			lease_id = NULL, lease_until = NULL,
			replay_total = 10, replay_matched = 8, replay_ratio = 0.8,
			updated_at = ?, completed_at = ? WHERE id = ?`,
			extractorHealLeaseTime,
			extractorHealLeaseTime,
			runID,
		)
		return err
	default:
		return errors.New("unsupported test terminal state")
	}
}
