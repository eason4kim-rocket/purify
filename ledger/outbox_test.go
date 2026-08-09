package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRecordVerificationBatchRoundTripAndLegacyCompatibility(t *testing.T) {
	store := openTestStore(t)
	createdAt := time.Date(2026, 8, 9, 12, 13, 14, 567890123, time.FixedZone("fixture", 8*60*60))
	rows := []Verification{outboxVerification("verification-outbox", 0), outboxVerification("verification-outbox", 1)}
	event := validOutboxEvent("verification-outbox", "event-outbox", createdAt)
	event.Secret = "hmac-secret"
	event.Payload = json.RawMessage(`{"type":"fact.changed","changes":[{"path":"price"}]}`)

	if err := store.RecordVerificationBatch(context.Background(), rows, &event); err != nil {
		t.Fatalf("RecordVerificationBatch() error = %v", err)
	}
	assertTableCount(t, store.db, "verifications", 2)
	assertTableCount(t, store.db, "outbox_events", 1)

	pending, err := store.PendingOutbox(
		context.Background(),
		createdAt.Add(time.Second),
		10,
		MaxOutboxPayloadBytes,
	)
	if err != nil {
		t.Fatalf("PendingOutbox() error = %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("PendingOutbox() len = %d, want 1", len(pending))
	}
	got := pending[0]
	if got.ID != event.ID || got.VerificationID != event.VerificationID || got.Type != event.Type ||
		got.URL != event.URL || got.Secret != event.Secret || !got.CreatedAt.Equal(createdAt) ||
		!got.NextAttemptAt.Equal(createdAt) || got.Attempts != 0 || got.LastAttemptAt != nil ||
		got.DeliveredAt != nil || got.FailedAt != nil {
		t.Fatalf("pending event = %#v", got)
	}
	if string(got.Payload) != string(event.Payload) {
		t.Fatalf("payload = %s, want %s", got.Payload, event.Payload)
	}
	pending[0].Payload[0] = '['
	again, err := store.PendingOutbox(context.Background(), createdAt.Add(time.Second), 10, MaxOutboxPayloadBytes)
	if err != nil || len(again) != 1 || string(again[0].Payload) != string(event.Payload) {
		t.Fatalf("pending payload was not isolated: events=%#v err=%v", again, err)
	}

	legacy := outboxVerification("verification-legacy", 0)
	if err := store.RecordVerifications(context.Background(), []Verification{legacy}); err != nil {
		t.Fatalf("RecordVerifications() error = %v", err)
	}
	assertTableCount(t, store.db, "verifications", 3)
	assertTableCount(t, store.db, "outbox_events", 1)
}

func TestRecordVerificationBatchIsIdempotentAndRejectsDrift(t *testing.T) {
	store := openTestStore(t)
	createdAt := outboxTestTime()
	row := outboxVerification("verification-idempotent", 0)
	event := validOutboxEvent(row.VerificationID, "event-first", createdAt)
	if err := store.RecordVerificationBatch(context.Background(), []Verification{row}, &event); err != nil {
		t.Fatalf("first RecordVerificationBatch() error = %v", err)
	}

	retry := event
	retry.ID = "event-retry-generated-id"
	if err := store.RecordVerificationBatch(context.Background(), []Verification{row}, &retry); err != nil {
		t.Fatalf("idempotent retry error = %v", err)
	}
	if err := store.RecordVerifications(context.Background(), []Verification{row}); err != nil {
		t.Fatalf("legacy idempotent retry error = %v", err)
	}
	assertTableCount(t, store.db, "verifications", 1)
	assertTableCount(t, store.db, "outbox_events", 1)
	pending, err := store.PendingOutbox(context.Background(), createdAt, 10, MaxOutboxPayloadBytes)
	if err != nil || len(pending) != 1 || pending[0].ID != event.ID {
		t.Fatalf("logical event identity = %#v, err=%v", pending, err)
	}

	driftedEvent := event
	driftedEvent.Payload = json.RawMessage(`{"different":true}`)
	if err := store.RecordVerificationBatch(context.Background(), []Verification{row}, &driftedEvent); !errors.Is(err, ErrOutboxConflict) {
		t.Fatalf("event drift error = %v, want ErrOutboxConflict", err)
	}
	driftedRow := row
	driftedRow.Path = "/different"
	if err := store.RecordVerificationBatch(context.Background(), []Verification{driftedRow}, &event); !errors.Is(err, ErrInvalidVerification) {
		t.Fatalf("row drift error = %v, want ErrInvalidVerification", err)
	}
	assertTableCount(t, store.db, "verifications", 1)
	assertTableCount(t, store.db, "outbox_events", 1)
}

func TestRecordVerificationBatchRollsBackRowsAndOutboxTogether(t *testing.T) {
	store := openTestStore(t)
	createdAt := outboxTestTime()
	seedRow := outboxVerification("verification-seed", 0)
	seedEvent := validOutboxEvent(seedRow.VerificationID, "shared-event-id", createdAt)
	if err := store.RecordVerificationBatch(context.Background(), []Verification{seedRow}, &seedEvent); err != nil {
		t.Fatalf("seed batch error = %v", err)
	}

	conflictingRow := outboxVerification("verification-conflict", 0)
	conflictingEvent := validOutboxEvent(conflictingRow.VerificationID, seedEvent.ID, createdAt)
	if err := store.RecordVerificationBatch(
		context.Background(),
		[]Verification{conflictingRow},
		&conflictingEvent,
	); err == nil {
		t.Fatal("outbox primary-key conflict error = nil")
	}
	assertVerificationCount(t, store.db, conflictingRow.VerificationID, 0)
	assertTableCount(t, store.db, "outbox_events", 1)

	first := outboxVerification("verification-row-conflict", 0)
	first.ID = "duplicate-row-id"
	second := outboxVerification(first.VerificationID, 1)
	second.ID = first.ID
	event := validOutboxEvent(first.VerificationID, "event-must-rollback", createdAt)
	if err := store.RecordVerificationBatch(context.Background(), []Verification{first, second}, &event); err == nil {
		t.Fatal("verification primary-key conflict error = nil")
	}
	assertVerificationCount(t, store.db, first.VerificationID, 0)
	var eventCount int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM outbox_events WHERE id = ?", event.ID).Scan(&eventCount); err != nil {
		t.Fatalf("count rolled-back event: %v", err)
	}
	if eventCount != 0 {
		t.Fatalf("rolled-back event count = %d, want 0", eventCount)
	}
}

func TestRecordVerificationBatchValidatesOutboxBeforeWriting(t *testing.T) {
	createdAt := outboxTestTime()
	baseRow := outboxVerification("verification-validation", 0)
	base := validOutboxEvent(baseRow.VerificationID, "event-validation", createdAt)
	lastAttempt := createdAt.Add(time.Second)
	deliveredAt := createdAt.Add(time.Minute)
	cases := []struct {
		name   string
		mutate func(*OutboxEvent)
	}{
		{name: "missing id", mutate: func(event *OutboxEvent) { event.ID = "" }},
		{name: "id surrounding whitespace", mutate: func(event *OutboxEvent) { event.ID = " event " }},
		{name: "id control character", mutate: func(event *OutboxEvent) { event.ID = "event\n" }},
		{name: "id too long", mutate: func(event *OutboxEvent) { event.ID = strings.Repeat("i", MaxOutboxIDBytes+1) }},
		{name: "verification mismatch", mutate: func(event *OutboxEvent) { event.VerificationID = "other" }},
		{name: "missing type", mutate: func(event *OutboxEvent) { event.Type = "" }},
		{name: "relative url", mutate: func(event *OutboxEvent) { event.URL = "/callback" }},
		{name: "non-http url", mutate: func(event *OutboxEvent) { event.URL = "file:///tmp/callback" }},
		{name: "uppercase scheme", mutate: func(event *OutboxEvent) { event.URL = "HTTPS://hook.example/callback" }},
		{name: "url userinfo", mutate: func(event *OutboxEvent) { event.URL = "https://user@example.com/callback" }},
		{name: "url whitespace", mutate: func(event *OutboxEvent) { event.URL = " https://hook.example/callback" }},
		{name: "url empty port", mutate: func(event *OutboxEvent) { event.URL = "https://hook.example:/callback" }},
		{name: "url zero port", mutate: func(event *OutboxEvent) { event.URL = "https://hook.example:0/callback" }},
		{name: "url large port", mutate: func(event *OutboxEvent) { event.URL = "https://hook.example:65536/callback" }},
		{name: "url root host", mutate: func(event *OutboxEvent) { event.URL = "https://./callback" }},
		{name: "url too long", mutate: func(event *OutboxEvent) { event.URL = "https://example.com/" + strings.Repeat("u", MaxOutboxURLBytes) }},
		{name: "secret too long", mutate: func(event *OutboxEvent) { event.Secret = strings.Repeat("s", MaxOutboxSecretBytes+1) }},
		{name: "missing payload", mutate: func(event *OutboxEvent) { event.Payload = nil }},
		{name: "invalid payload", mutate: func(event *OutboxEvent) { event.Payload = json.RawMessage(`{`) }},
		{name: "non utf8 payload", mutate: func(event *OutboxEvent) { event.Payload = json.RawMessage{'"', 0xff, '"'} }},
		{name: "payload too long", mutate: func(event *OutboxEvent) { event.Payload = make(json.RawMessage, MaxOutboxPayloadBytes+1) }},
		{name: "missing created time", mutate: func(event *OutboxEvent) { event.CreatedAt = time.Time{} }},
		{name: "unsupported created year", mutate: func(event *OutboxEvent) { event.CreatedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) }},
		{name: "next attempt before creation", mutate: func(event *OutboxEvent) { event.NextAttemptAt = createdAt.Add(-time.Second) }},
		{name: "attempt state", mutate: func(event *OutboxEvent) { event.Attempts = 1 }},
		{name: "last attempt state", mutate: func(event *OutboxEvent) { event.LastAttemptAt = &lastAttempt }},
		{name: "last error state", mutate: func(event *OutboxEvent) { event.LastError = "failed" }},
		{name: "delivered state", mutate: func(event *OutboxEvent) { event.DeliveredAt = &deliveredAt }},
		{name: "failed state", mutate: func(event *OutboxEvent) { event.FailedAt = &deliveredAt }},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			event := base
			test.mutate(&event)
			err := store.RecordVerificationBatch(context.Background(), []Verification{baseRow}, &event)
			if !errors.Is(err, ErrInvalidOutboxEvent) {
				t.Fatalf("error = %v, want ErrInvalidOutboxEvent", err)
			}
			assertTableCount(t, store.db, "verifications", 0)
			assertTableCount(t, store.db, "outbox_events", 0)
		})
	}

	store := openTestStore(t)
	if err := store.RecordVerificationBatch(context.Background(), nil, &base); !errors.Is(err, ErrInvalidOutboxEvent) {
		t.Fatalf("event without rows error = %v, want ErrInvalidOutboxEvent", err)
	}
}

func TestPendingOutboxAndDeliveryStateMachine(t *testing.T) {
	store := openTestStore(t)
	createdAt := outboxTestTime()
	row := outboxVerification("verification-state", 0)
	event := validOutboxEvent(row.VerificationID, "event-state", createdAt)
	event.NextAttemptAt = createdAt.Add(time.Minute)
	if err := store.RecordVerificationBatch(context.Background(), []Verification{row}, &event); err != nil {
		t.Fatalf("RecordVerificationBatch() error = %v", err)
	}

	assertPendingCount(t, store, createdAt, 0)
	assertPendingCount(t, store, event.NextAttemptAt, 1)
	attemptedAt := createdAt.Add(2 * time.Minute)
	nextAttemptAt := createdAt.Add(time.Hour)
	if err := store.MarkOutboxAttempt(context.Background(), event.ID, attemptedAt, nextAttemptAt, "temporary failure"); err != nil {
		t.Fatalf("MarkOutboxAttempt() error = %v", err)
	}
	if err := store.MarkOutboxAttempt(context.Background(), event.ID, attemptedAt, nextAttemptAt, "temporary failure"); err != nil {
		t.Fatalf("idempotent MarkOutboxAttempt() error = %v", err)
	}
	assertPendingCount(t, store, nextAttemptAt.Add(-time.Nanosecond), 0)
	pending := pendingOutbox(t, store, nextAttemptAt)
	if len(pending) != 1 || pending[0].Attempts != 1 || pending[0].LastAttemptAt == nil ||
		!pending[0].LastAttemptAt.Equal(attemptedAt) || pending[0].LastError != "temporary failure" {
		t.Fatalf("attempt state = %#v", pending)
	}

	deliveredAt := nextAttemptAt.Add(time.Minute)
	if err := store.MarkOutboxDelivered(context.Background(), event.ID, deliveredAt); err != nil {
		t.Fatalf("MarkOutboxDelivered() error = %v", err)
	}
	if err := store.MarkOutboxDelivered(context.Background(), event.ID, deliveredAt.Add(time.Minute)); err != nil {
		t.Fatalf("idempotent MarkOutboxDelivered() error = %v", err)
	}
	if err := store.MarkOutboxFailed(context.Background(), event.ID, deliveredAt.Add(2*time.Minute), "late failure"); err != nil {
		t.Fatalf("opposite terminal no-op error = %v", err)
	}
	if err := store.MarkOutboxAttempt(
		context.Background(),
		event.ID,
		deliveredAt.Add(3*time.Minute),
		deliveredAt.Add(4*time.Minute),
		"late attempt",
	); err != nil {
		t.Fatalf("terminal attempt no-op error = %v", err)
	}
	assertPendingCount(t, store, deliveredAt.Add(24*time.Hour), 0)
	stored := loadOutboxEventForTest(t, store, event.ID)
	if stored.DeliveredAt == nil || !stored.DeliveredAt.Equal(deliveredAt) || stored.FailedAt != nil || stored.Attempts != 1 {
		t.Fatalf("delivered state = %#v", stored)
	}

	failedRow := outboxVerification("verification-failed", 0)
	failedEvent := validOutboxEvent(failedRow.VerificationID, "event-failed", createdAt)
	if err := store.RecordVerificationBatch(context.Background(), []Verification{failedRow}, &failedEvent); err != nil {
		t.Fatalf("failed-event batch error = %v", err)
	}
	failedAt := createdAt.Add(time.Minute)
	if err := store.MarkOutboxFailed(context.Background(), failedEvent.ID, failedAt, "SSRF policy rejected target"); err != nil {
		t.Fatalf("MarkOutboxFailed() error = %v", err)
	}
	if err := store.MarkOutboxFailed(context.Background(), failedEvent.ID, failedAt.Add(time.Minute), "again"); err != nil {
		t.Fatalf("idempotent MarkOutboxFailed() error = %v", err)
	}
	if err := store.MarkOutboxDelivered(context.Background(), failedEvent.ID, failedAt.Add(time.Minute)); err != nil {
		t.Fatalf("opposite delivered no-op error = %v", err)
	}
	stored = loadOutboxEventForTest(t, store, failedEvent.ID)
	if stored.FailedAt == nil || !stored.FailedAt.Equal(failedAt) || stored.DeliveredAt != nil ||
		stored.Attempts != 1 || stored.LastAttemptAt == nil || !stored.LastAttemptAt.Equal(failedAt) ||
		stored.LastError != "SSRF policy rejected target" {
		t.Fatalf("failed state = %#v", stored)
	}
}

func TestOutboxStateMethodsValidateUnknownIDsAndTimes(t *testing.T) {
	store := openTestStore(t)
	now := outboxTestTime()
	if err := store.MarkOutboxAttempt(context.Background(), "missing", now, now.Add(time.Minute), "failed"); !errors.Is(err, ErrOutboxNotFound) {
		t.Fatalf("unknown attempt error = %v", err)
	}
	if err := store.MarkOutboxDelivered(context.Background(), "missing", now); !errors.Is(err, ErrOutboxNotFound) {
		t.Fatalf("unknown delivered error = %v", err)
	}
	if err := store.MarkOutboxFailed(context.Background(), "missing", now, "failed"); !errors.Is(err, ErrOutboxNotFound) {
		t.Fatalf("unknown failed error = %v", err)
	}
	if err := store.MarkOutboxAttempt(context.Background(), "missing", now, now, "failed"); !errors.Is(err, ErrInvalidOutboxEvent) {
		t.Fatalf("non-increasing retry error = %v", err)
	}
	if err := store.MarkOutboxDelivered(context.Background(), "missing", time.Time{}); !errors.Is(err, ErrInvalidOutboxEvent) {
		t.Fatalf("zero delivered time error = %v", err)
	}
	if _, err := store.PendingOutbox(context.Background(), now, 0, MaxOutboxPayloadBytes); !errors.Is(err, ErrInvalidOutboxLimit) {
		t.Fatalf("zero limit error = %v", err)
	}
	if _, err := store.PendingOutbox(context.Background(), now, MaxPendingOutbox+1, MaxOutboxPayloadBytes); !errors.Is(err, ErrInvalidOutboxLimit) {
		t.Fatalf("large limit error = %v", err)
	}
	if _, err := store.PendingOutbox(context.Background(), now, 1, MaxOutboxPayloadBytes-1); !errors.Is(err, ErrInvalidOutboxLimit) {
		t.Fatalf("small byte budget error = %v", err)
	}
	if _, err := store.PendingOutbox(context.Background(), now, 1, MaxPendingOutboxBytes+1); !errors.Is(err, ErrInvalidOutboxLimit) {
		t.Fatalf("large byte budget error = %v", err)
	}
	if _, err := store.PendingOutbox(context.Background(), time.Time{}, 1, MaxOutboxPayloadBytes); !errors.Is(err, ErrInvalidOutboxEvent) {
		t.Fatalf("zero due time error = %v", err)
	}
}

func TestPendingOutboxHonorsAggregatePayloadBudget(t *testing.T) {
	store := openTestStore(t)
	createdAt := outboxTestTime()
	const payloadBytes = 4 << 20
	payload := json.RawMessage(`"` + strings.Repeat("x", payloadBytes-2) + `"`)
	for index := 0; index < 9; index++ {
		verificationID := fmt.Sprintf("verification-large-%02d", index)
		row := outboxVerification(verificationID, 0)
		event := validOutboxEvent(verificationID, fmt.Sprintf("event-large-%02d", index), createdAt)
		event.Payload = payload
		if err := store.RecordVerificationBatch(context.Background(), []Verification{row}, &event); err != nil {
			t.Fatalf("RecordVerificationBatch(%d) error = %v", index, err)
		}
	}

	pending, err := store.PendingOutbox(context.Background(), createdAt, MaxPendingOutbox, MaxOutboxPayloadBytes)
	if err != nil {
		t.Fatalf("PendingOutbox() error = %v", err)
	}
	if len(pending) != MaxOutboxPayloadBytes/payloadBytes {
		t.Fatalf("pending len = %d, want %d", len(pending), MaxOutboxPayloadBytes/payloadBytes)
	}
	total := 0
	for _, event := range pending {
		total += len(event.Payload)
	}
	if total > MaxOutboxPayloadBytes {
		t.Fatalf("pending payload bytes = %d, budget %d", total, MaxOutboxPayloadBytes)
	}
	for _, event := range pending {
		if err := store.MarkOutboxDelivered(context.Background(), event.ID, createdAt.Add(time.Minute)); err != nil {
			t.Fatalf("MarkOutboxDelivered(%q) error = %v", event.ID, err)
		}
	}
	remaining := pendingOutbox(t, store, createdAt.Add(time.Minute))
	if len(remaining) != 1 {
		t.Fatalf("remaining pending len = %d, want 1", len(remaining))
	}
}

func TestRecordVerificationBatchConcurrentAcrossStores(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(dir)
	if err != nil {
		t.Fatalf("first Open() error = %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := Open(dir)
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	const workers = 40
	start := make(chan struct{})
	errorsByWorker := make(chan error, workers)
	var group sync.WaitGroup
	for index := 0; index < workers; index++ {
		index := index
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			verificationID := fmt.Sprintf("verification-concurrent-%02d", index)
			row := outboxVerification(verificationID, 0)
			event := validOutboxEvent(verificationID, fmt.Sprintf("event-concurrent-%02d", index), outboxTestTime())
			store := first
			if index%2 == 1 {
				store = second
			}
			errorsByWorker <- store.RecordVerificationBatch(context.Background(), []Verification{row}, &event)
		}()
	}
	close(start)
	group.Wait()
	close(errorsByWorker)
	for err := range errorsByWorker {
		if err != nil {
			t.Fatalf("concurrent RecordVerificationBatch() error = %v", err)
		}
	}
	assertTableCount(t, first.db, "verifications", workers)
	assertTableCount(t, first.db, "outbox_events", workers)

	row := outboxVerification("verification-one-logical-event", 0)
	event := validOutboxEvent(row.VerificationID, "event-one-logical-event", outboxTestTime())
	start = make(chan struct{})
	errorsByWorker = make(chan error, workers)
	for index := 0; index < workers; index++ {
		index := index
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			store := first
			if index%2 == 1 {
				store = second
			}
			errorsByWorker <- store.RecordVerificationBatch(context.Background(), []Verification{row}, &event)
		}()
	}
	close(start)
	group.Wait()
	close(errorsByWorker)
	for err := range errorsByWorker {
		if err != nil {
			t.Fatalf("idempotent concurrent retry error = %v", err)
		}
	}
	assertVerificationCount(t, first.db, row.VerificationID, 1)
	var eventCount int
	if err := first.db.QueryRow("SELECT COUNT(*) FROM outbox_events WHERE verification_id = ?", row.VerificationID).Scan(&eventCount); err != nil {
		t.Fatalf("count logical event: %v", err)
	}
	if eventCount != 1 {
		t.Fatalf("logical event count = %d, want 1", eventCount)
	}
}

func TestOutboxTerminalRaceKeepsExactlyOneTerminalState(t *testing.T) {
	store := openTestStore(t)
	second, err := Open(filepath.Dir(store.Path()))
	if err != nil {
		t.Fatalf("Open(second store) error = %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	createdAt := outboxTestTime()
	row := outboxVerification("verification-terminal-race", 0)
	event := validOutboxEvent(row.VerificationID, "event-terminal-race", createdAt)
	if err := store.RecordVerificationBatch(context.Background(), []Verification{row}, &event); err != nil {
		t.Fatalf("RecordVerificationBatch() error = %v", err)
	}

	start := make(chan struct{})
	errorsSeen := make(chan error, 2)
	var group sync.WaitGroup
	group.Add(2)
	go func() {
		defer group.Done()
		<-start
		errorsSeen <- store.MarkOutboxDelivered(context.Background(), event.ID, createdAt.Add(time.Minute))
	}()
	go func() {
		defer group.Done()
		<-start
		errorsSeen <- second.MarkOutboxFailed(context.Background(), event.ID, createdAt.Add(time.Minute), "permanent")
	}()
	close(start)
	group.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("terminal race error = %v", err)
		}
	}
	stored := loadOutboxEventForTest(t, store, event.ID)
	if (stored.DeliveredAt == nil) == (stored.FailedAt == nil) {
		t.Fatalf("terminal state = delivered %v failed %v", stored.DeliveredAt, stored.FailedAt)
	}
}

func TestMigration002UpgradesVersionOneDatabaseIdempotently(t *testing.T) {
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
	if _, err := db.Exec(migrations[0]); err != nil {
		t.Fatalf("apply migration 001: %v", err)
	}
	if _, err := db.Exec("INSERT INTO schema_migrations(version, applied_at) VALUES (1, ?)", formatOutboxTime(outboxTestTime())); err != nil {
		t.Fatalf("record migration 001: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO verifications (
		id, verification_id, claim_index, url, host, path, old_value,
		outcome, old_snapshot_id, verified_at
	) VALUES ('historical-row', 'historical-verification', 0,
		'https://example.com/item', 'example.com', '/price', '19.99',
		'confirmed', 'sha256:old', ?)`, formatOutboxTime(outboxTestTime())); err != nil {
		t.Fatalf("insert historical verification: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close version-one database: %v", err)
	}

	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(upgrade) error = %v", err)
	}
	assertMigrationState(t, store.db)
	assertTableCount(t, store.db, "verifications", 1)
	if err := store.Close(); err != nil {
		t.Fatalf("Close(upgraded) error = %v", err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(reopened) error = %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	assertMigrationState(t, reopened.db)
	var migrationCount int
	if err := reopened.db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&migrationCount); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if migrationCount != len(migrations) {
		t.Fatalf("migration count = %d, want %d", migrationCount, len(migrations))
	}
}

func TestOutboxMethodsHonorContextAndClose(t *testing.T) {
	store := openTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	now := outboxTestTime()
	row := outboxVerification("verification-canceled", 0)
	event := validOutboxEvent(row.VerificationID, "event-canceled", now)
	if err := store.RecordVerificationBatch(ctx, []Verification{row}, &event); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled batch error = %v", err)
	}
	if _, err := store.PendingOutbox(ctx, now, 1, MaxOutboxPayloadBytes); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled pending error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := store.RecordVerificationBatch(context.Background(), []Verification{row}, &event); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed batch error = %v", err)
	}
	if _, err := store.PendingOutbox(context.Background(), now, 1, MaxOutboxPayloadBytes); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed pending error = %v", err)
	}
	if err := store.MarkOutboxDelivered(context.Background(), event.ID, now); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed mark error = %v", err)
	}
}

func outboxVerification(verificationID string, claimIndex int) Verification {
	row := validVerification(claimIndex)
	row.VerificationID = verificationID
	row.VerifiedAt = outboxTestTime()
	return row
}

func validOutboxEvent(verificationID, id string, createdAt time.Time) OutboxEvent {
	return OutboxEvent{
		ID:             id,
		VerificationID: verificationID,
		Type:           "fact.changed",
		URL:            "https://hooks.example/events",
		Secret:         "secret",
		Payload:        json.RawMessage(`{"type":"fact.changed"}`),
		CreatedAt:      createdAt,
	}
}

func outboxTestTime() time.Time {
	return time.Date(2026, 8, 9, 12, 13, 14, 567890123, time.UTC)
}

func pendingOutbox(t *testing.T, store *Store, dueAt time.Time) []OutboxEvent {
	t.Helper()
	events, err := store.PendingOutbox(context.Background(), dueAt, MaxPendingOutbox, MaxPendingOutboxBytes)
	if err != nil {
		t.Fatalf("PendingOutbox() error = %v", err)
	}
	return events
}

func assertPendingCount(t *testing.T, store *Store, dueAt time.Time, want int) {
	t.Helper()
	if got := len(pendingOutbox(t, store, dueAt)); got != want {
		t.Fatalf("pending count = %d, want %d", got, want)
	}
}

func loadOutboxEventForTest(t *testing.T, store *Store, id string) OutboxEvent {
	t.Helper()
	event, err := scanOutboxEvent(store.db.QueryRow(`SELECT
		id, verification_id, event_type, destination_url, secret, payload,
		created_at, attempt_count, last_attempt_at, last_error,
		next_attempt_at, delivered_at, failed_at
		FROM outbox_events WHERE id = ?`, id))
	if err != nil {
		t.Fatalf("load outbox event %q: %v", id, err)
	}
	return event
}

func assertTableCount(t *testing.T, db *sql.DB, table string, want int) {
	t.Helper()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if count != want {
		t.Fatalf("%s count = %d, want %d", table, count, want)
	}
}

func assertVerificationCount(t *testing.T, db *sql.DB, verificationID string, want int) {
	t.Helper()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM verifications WHERE verification_id = ?", verificationID).Scan(&count); err != nil {
		t.Fatalf("count verification %q: %v", verificationID, err)
	}
	if count != want {
		t.Fatalf("verification %q count = %d, want %d", verificationID, count, want)
	}
}
