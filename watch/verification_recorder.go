package watch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/use-agent/purify/ledger"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/publicnet"
)

const factIdentityDomain = "purify.watch.fact.v1"

var (
	// ErrInvalidVerificationBinding identifies a malformed lease-bound recorder
	// configuration. It deliberately does not reveal a receipt or lease token.
	ErrInvalidVerificationBinding = errors.New("watch: invalid verification binding")
	// ErrVerificationMismatch means the durable verification row did not match
	// the exact fact expectation captured before the revisit.
	ErrVerificationMismatch = errors.New("watch: verification does not match leased fact")
	// ErrVerificationLeaseLost means the capability was stale, expired, paused,
	// deleted, or superseded before the verification transaction could commit.
	ErrVerificationLeaseLost = errors.New("watch: verification lease is no longer live")
)

// VerificationBaseline is the exact evidence-backed scalar used to begin a
// watch. Path uses Extract/Answer's escaped dotted-path convention.
type VerificationBaseline struct {
	Path       string
	Value      json.RawMessage
	SourceURL  string
	SnapshotID string
	Receipt    string
}

// VerificationRecordResult is published only after the verification rows,
// fact mutation, watch schedule, and optional outbox event all commit.
// Retry is present only when a bootstrap's first formal verification changed;
// it binds the one permitted follow-up verification to refreshed evidence.
type VerificationRecordResult struct {
	VerificationID string
	ClaimIndex     int
	Outcome        ledger.Outcome
	GoneScope      ledger.GoneScope
	Fact           *Fact
	Retry          *VerificationBaseline
	LeaseReleased  bool
}

type verificationRecorderMode uint8

const (
	verificationRecorderBootstrap verificationRecorderMode = iota + 1
	verificationRecorderFact
)

// VerificationRecorder is a single-watch VerificationRecorder for
// verify.Service.VerifyWithRecorder. It never performs network I/O and its
// transaction extension uses only ledger's supplied WriteTx.
type VerificationRecorder struct {
	store       *Store
	claim       Claim
	expectation VerificationBaseline
	mode        verificationRecorderMode

	callMu sync.Mutex
	mu     sync.RWMutex
	result VerificationRecordResult
	has    bool
}

// NewBootstrapVerificationRecorder binds a watch with no open fact to one
// Answer-derived baseline: either a new pending watch or an active watch
// whose previous fact closed gone and needs fresh evidence. The baseline must
// be canonical and complete.
func NewBootstrapVerificationRecorder(
	store *Store,
	claim Claim,
	baseline VerificationBaseline,
) (*VerificationRecorder, error) {
	if !validRecorderStore(store) || claim.Fact != nil ||
		claim.Watch.State != StatePending && claim.Watch.State != StateActive ||
		!validClaimBinding(claim) {
		return nil, ErrInvalidVerificationBinding
	}
	prepared, err := prepareVerificationBaseline(claim.Watch.Spec.Predicate, baseline)
	if err != nil {
		return nil, err
	}
	return &VerificationRecorder{
		store:       store,
		claim:       cloneClaim(claim),
		expectation: prepared,
		mode:        verificationRecorderBootstrap,
	}, nil
}

// NewFactVerificationRecorder binds an active watch and its exact open fact.
// No caller-supplied provenance is accepted in this mode.
func NewFactVerificationRecorder(store *Store, claim Claim) (*VerificationRecorder, error) {
	if !validRecorderStore(store) || claim.Fact == nil || claim.Watch.State != StateActive ||
		!validClaimBinding(claim) || validateOpenFact(*claim.Fact, claim.Watch) != nil {
		return nil, ErrInvalidVerificationBinding
	}
	baseline, err := prepareVerificationBaseline(claim.Watch.Spec.Predicate, VerificationBaseline{
		Path:       claim.Fact.Path,
		Value:      claim.Fact.Value,
		SourceURL:  claim.Fact.SourceURL,
		SnapshotID: claim.Fact.SnapshotID,
		Receipt:    claim.Fact.Receipt,
	})
	if err != nil {
		return nil, err
	}
	return &VerificationRecorder{
		store:       store,
		claim:       cloneClaim(claim),
		expectation: baseline,
		mode:        verificationRecorderFact,
	}, nil
}

// RecordVerificationBatch commits through the ledger's verification hook
// exactly once. The local result is not made visible until the outer ledger
// transaction, including its outbox write, has committed.
func (recorder *VerificationRecorder) RecordVerificationBatch(
	ctx context.Context,
	rows []ledger.Verification,
	event *ledger.OutboxEvent,
) error {
	if recorder == nil || !validRecorderStore(recorder.store) || ctx == nil {
		return ErrInvalidVerificationBinding
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(rows) != 1 || rows[0].ClaimIndex != 0 || strings.TrimSpace(rows[0].VerificationID) == "" {
		return ErrVerificationMismatch
	}

	recorder.callMu.Lock()
	defer recorder.callMu.Unlock()

	var committed VerificationRecordResult
	err := recorder.store.ledger.RecordVerificationBatchWithHook(
		ctx,
		rows,
		event,
		func(hookCtx context.Context, tx ledger.WriteTx, state ledger.VerificationBatchState) error {
			result, err := recorder.materialize(hookCtx, tx, state)
			if err != nil {
				return err
			}
			committed = result
			return nil
		},
	)
	if err != nil {
		return err
	}

	recorder.mu.Lock()
	recorder.result = cloneVerificationRecordResult(committed)
	recorder.has = true
	recorder.mu.Unlock()
	return nil
}

// Result returns the most recent fully committed result.
func (recorder *VerificationRecorder) Result() (VerificationRecordResult, bool) {
	if recorder == nil {
		return VerificationRecordResult{}, false
	}
	recorder.mu.RLock()
	defer recorder.mu.RUnlock()
	if !recorder.has {
		return VerificationRecordResult{}, false
	}
	return cloneVerificationRecordResult(recorder.result), true
}

type authoritativeVerification struct {
	verificationID string
	claimIndex     int
	url            string
	finalURL       string
	path           string
	oldValue       json.RawMessage
	newValue       json.RawMessage
	outcome        ledger.Outcome
	goneScope      ledger.GoneScope
	oldSnapshotID  string
	newSnapshotID  string
	oldReceipt     string
	receipt        string
	verifiedAt     time.Time
}

func (recorder *VerificationRecorder) materialize(
	ctx context.Context,
	tx ledger.WriteTx,
	state ledger.VerificationBatchState,
) (VerificationRecordResult, error) {
	if err := ctx.Err(); err != nil {
		return VerificationRecordResult{}, err
	}
	now, err := recorder.store.operationTime()
	if err != nil {
		return VerificationRecordResult{}, ErrInvalidVerificationBinding
	}
	row, err := loadAuthoritativeVerification(ctx, tx, state.VerificationID)
	if err != nil {
		return VerificationRecordResult{}, err
	}
	if err := recorder.validateVerificationRow(row); err != nil {
		return VerificationRecordResult{}, err
	}

	stored, err := loadWatchByID(ctx, tx, recorder.claim.Watch.ID)
	if err != nil {
		if errors.Is(err, ErrWatchNotFound) {
			return VerificationRecordResult{}, ErrVerificationLeaseLost
		}
		return VerificationRecordResult{}, err
	}
	liveLease := stored.leaseID == recorder.claim.Lease.ID && stored.leaseUntil != nil &&
		stored.leaseUntil.Equal(recorder.claim.Lease.Until) && now.Before(*stored.leaseUntil) &&
		row.verifiedAt.Before(*stored.leaseUntil)
	if !liveLease {
		if state.Existing && stored.leaseID == "" && stored.leaseUntil == nil {
			return recorder.replayCompleted(ctx, tx, stored, row)
		}
		return VerificationRecordResult{}, ErrVerificationLeaseLost
	}
	if !watchMatchesClaim(stored.Watch, recorder.claim.Watch) {
		return VerificationRecordResult{}, ErrVerificationMismatch
	}

	openFact, hasOpenFact, err := loadOpenFact(ctx, tx, stored.Watch)
	if err != nil {
		return VerificationRecordResult{}, err
	}
	switch recorder.mode {
	case verificationRecorderBootstrap:
		if stored.Watch.State != StatePending && stored.Watch.State != StateActive ||
			hasOpenFact || recorder.claim.Fact != nil {
			return VerificationRecordResult{}, ErrVerificationMismatch
		}
	case verificationRecorderFact:
		if stored.Watch.State != StateActive || !hasOpenFact || recorder.claim.Fact == nil ||
			!factsEqual(openFact, *recorder.claim.Fact) {
			return VerificationRecordResult{}, ErrVerificationMismatch
		}
	default:
		return VerificationRecordResult{}, ErrInvalidVerificationBinding
	}

	sourceURL, root, err := verificationSource(row)
	if err != nil {
		return VerificationRecordResult{}, err
	}
	result := VerificationRecordResult{
		VerificationID: strings.Clone(row.verificationID),
		ClaimIndex:     row.claimIndex,
		Outcome:        row.outcome,
		GoneScope:      row.goneScope,
	}

	if recorder.mode == verificationRecorderBootstrap {
		switch row.outcome {
		case ledger.OutcomeChanged:
			retry := VerificationBaseline{
				Path:       strings.Clone(row.path),
				Value:      append(json.RawMessage(nil), row.newValue...),
				SourceURL:  strings.Clone(sourceURL),
				SnapshotID: strings.Clone(row.newSnapshotID),
				Receipt:    strings.Clone(row.receipt),
			}
			prepared, err := prepareVerificationBaseline(stored.Watch.Spec.Predicate, retry)
			if err != nil {
				return VerificationRecordResult{}, ErrVerificationMismatch
			}
			result.Retry = &prepared
			return result, nil
		case ledger.OutcomeGone:
			return result, nil
		case ledger.OutcomeConfirmed:
			fact, err := insertInitialFact(ctx, tx, stored.Watch, row, sourceURL, root)
			if err != nil {
				return VerificationRecordResult{}, err
			}
			if err := updateSuccessfulWatch(ctx, tx, stored, recorder.claim.Lease, row, watchSuccessBootstrap, now); err != nil {
				return VerificationRecordResult{}, err
			}
			result.Fact = &fact
			result.LeaseReleased = true
			return result, nil
		default:
			return VerificationRecordResult{}, ErrVerificationMismatch
		}
	}

	var materialized Fact
	switch row.outcome {
	case ledger.OutcomeConfirmed:
		materialized, err = refreshOpenFact(ctx, tx, openFact, row, sourceURL, root)
	case ledger.OutcomeChanged:
		materialized, err = replaceOpenFact(ctx, tx, openFact, row, sourceURL, root)
	case ledger.OutcomeGone:
		materialized, err = closeGoneFact(ctx, tx, openFact, row)
	default:
		err = ErrVerificationMismatch
	}
	if err != nil {
		return VerificationRecordResult{}, err
	}
	successKind := watchSuccessConfirmed
	if row.outcome == ledger.OutcomeChanged || row.outcome == ledger.OutcomeGone {
		successKind = watchSuccessChanged
	}
	if err := updateSuccessfulWatch(ctx, tx, stored, recorder.claim.Lease, row, successKind, now); err != nil {
		return VerificationRecordResult{}, err
	}
	result.Fact = &materialized
	result.LeaseReleased = true
	return result, nil
}

func (recorder *VerificationRecorder) replayCompleted(
	ctx context.Context,
	tx ledger.ReadTx,
	stored storedWatch,
	row authoritativeVerification,
) (VerificationRecordResult, error) {
	kind := watchSuccessConfirmed
	if recorder.mode == verificationRecorderBootstrap {
		kind = watchSuccessBootstrap
	} else if row.outcome == ledger.OutcomeChanged || row.outcome == ledger.OutcomeGone {
		kind = watchSuccessChanged
	}
	schedule, err := successfulScheduleFor(recorder.claim.Watch, row, kind)
	if err != nil || !watchMatchesCompleted(stored, recorder.claim.Watch, row, schedule) {
		return VerificationRecordResult{}, ErrVerificationLeaseLost
	}
	sourceURL, root, err := verificationSource(row)
	if err != nil {
		return VerificationRecordResult{}, err
	}
	result := VerificationRecordResult{
		VerificationID: strings.Clone(row.verificationID), ClaimIndex: row.claimIndex,
		Outcome: row.outcome, GoneScope: row.goneScope, LeaseReleased: true,
	}

	if recorder.mode == verificationRecorderBootstrap {
		if row.outcome != ledger.OutcomeConfirmed || recorder.claim.Fact != nil {
			return VerificationRecordResult{}, ErrVerificationLeaseLost
		}
		expected := initialFactValue(recorder.claim.Watch, row, sourceURL, root)
		actual, found, err := loadFactByID(ctx, tx, expected.ID)
		if err != nil || !found || !factsEqual(actual, expected) {
			return VerificationRecordResult{}, ErrVerificationLeaseLost
		}
		result.Fact = &actual
		return result, nil
	}

	if recorder.claim.Fact == nil {
		return VerificationRecordResult{}, ErrVerificationLeaseLost
	}
	original := *recorder.claim.Fact
	switch row.outcome {
	case ledger.OutcomeConfirmed:
		expected := refreshedFactValue(original, row, sourceURL, root)
		actual, found, err := loadFactByID(ctx, tx, original.ID)
		if err != nil || !found || !factsEqual(actual, expected) {
			return VerificationRecordResult{}, ErrVerificationLeaseLost
		}
		result.Fact = &actual
	case ledger.OutcomeChanged:
		expectedClosed := changedPredecessorValue(original, row)
		actualClosed, found, err := loadFactByID(ctx, tx, original.ID)
		if err != nil || !found || !factsEqual(actualClosed, expectedClosed) {
			return VerificationRecordResult{}, ErrVerificationLeaseLost
		}
		expectedSuccessor := successorFactValue(original, row, sourceURL, root)
		actualSuccessor, found, err := loadFactByID(ctx, tx, expectedSuccessor.ID)
		if err != nil || !found || !factsEqual(actualSuccessor, expectedSuccessor) {
			return VerificationRecordResult{}, ErrVerificationLeaseLost
		}
		result.Fact = &actualSuccessor
	case ledger.OutcomeGone:
		expected := goneFactValue(original, row)
		actual, found, err := loadFactByID(ctx, tx, original.ID)
		if err != nil || !found || !factsEqual(actual, expected) {
			return VerificationRecordResult{}, ErrVerificationLeaseLost
		}
		var openCount int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM facts WHERE watch_id = ? AND valid_to IS NULL`, original.WatchID,
		).Scan(&openCount); err != nil {
			return VerificationRecordResult{}, recorderDatabaseError(ctx, err)
		}
		if openCount != 0 {
			return VerificationRecordResult{}, ErrVerificationLeaseLost
		}
		result.Fact = &actual
	default:
		return VerificationRecordResult{}, ErrVerificationLeaseLost
	}
	return result, nil
}

func loadAuthoritativeVerification(
	ctx context.Context,
	tx ledger.ReadTx,
	verificationID string,
) (authoritativeVerification, error) {
	var count int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM verifications WHERE verification_id = ?`, verificationID,
	).Scan(&count); err != nil {
		return authoritativeVerification{}, recorderDatabaseError(ctx, err)
	}
	if count != 1 {
		return authoritativeVerification{}, ErrVerificationMismatch
	}
	var row authoritativeVerification
	var finalURL, newValue, goneScope, newSnapshotID, oldReceipt, receipt sql.NullString
	var outcome, oldValue, verifiedAt string
	err := tx.QueryRowContext(ctx, `SELECT verification_id, claim_index, url, final_url,
		path, old_value, new_value, outcome, gone_scope, old_snapshot_id,
		new_snapshot_id, old_receipt, receipt, verified_at
		FROM verifications WHERE verification_id = ? AND claim_index = 0`, verificationID).Scan(
		&row.verificationID,
		&row.claimIndex,
		&row.url,
		&finalURL,
		&row.path,
		&oldValue,
		&newValue,
		&outcome,
		&goneScope,
		&row.oldSnapshotID,
		&newSnapshotID,
		&oldReceipt,
		&receipt,
		&verifiedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return authoritativeVerification{}, ErrVerificationMismatch
	}
	if err != nil {
		return authoritativeVerification{}, recorderDatabaseError(ctx, err)
	}
	parsedTime, err := time.Parse(time.RFC3339Nano, verifiedAt)
	if err != nil || parsedTime.Location() != time.UTC || !validPublicTime(parsedTime.UTC().Round(0)) {
		return authoritativeVerification{}, ErrVerificationMismatch
	}
	row.finalURL = finalURL.String
	row.oldValue = append(json.RawMessage(nil), oldValue...)
	if newValue.Valid {
		row.newValue = append(json.RawMessage(nil), newValue.String...)
	}
	row.outcome = ledger.Outcome(outcome)
	row.goneScope = ledger.GoneScope(goneScope.String)
	row.newSnapshotID = newSnapshotID.String
	row.oldReceipt = oldReceipt.String
	row.receipt = receipt.String
	row.verifiedAt = parsedTime.UTC().Round(0)
	return row, nil
}

func (recorder *VerificationRecorder) validateVerificationRow(row authoritativeVerification) error {
	if row.verificationID == "" || row.claimIndex != 0 || row.path != recorder.expectation.Path ||
		!bytes.Equal(row.oldValue, recorder.expectation.Value) ||
		row.url != recorder.expectation.SourceURL ||
		row.oldSnapshotID != recorder.expectation.SnapshotID ||
		row.oldReceipt != recorder.expectation.Receipt ||
		row.verifiedAt.Before(recorder.claim.Watch.UpdatedAt) {
		return ErrVerificationMismatch
	}
	if _, err := canonicalScalar(row.oldValue); err != nil {
		return ErrVerificationMismatch
	}
	if !validSnapshotID(row.newSnapshotID) {
		return ErrVerificationMismatch
	}
	switch row.outcome {
	case ledger.OutcomeConfirmed:
		if len(row.newValue) != 0 || row.goneScope != "" || !validReceipt(row.receipt) {
			return ErrVerificationMismatch
		}
	case ledger.OutcomeChanged:
		if row.goneScope != "" || !validReceipt(row.receipt) {
			return ErrVerificationMismatch
		}
		if _, err := canonicalScalar(row.newValue); err != nil {
			return ErrVerificationMismatch
		}
	case ledger.OutcomeGone:
		if len(row.newValue) != 0 || row.receipt != "" ||
			row.goneScope != ledger.GoneScopeField && row.goneScope != ledger.GoneScopePage {
			return ErrVerificationMismatch
		}
	default:
		return ErrVerificationMismatch
	}
	return nil
}

func insertInitialFact(
	ctx context.Context,
	tx ledger.WriteTx,
	watch Watch,
	row authoritativeVerification,
	sourceURL string,
	root string,
) (Fact, error) {
	fact := initialFactValue(watch, row, sourceURL, root)
	stamp := formatTime(row.verifiedAt)
	result, err := tx.ExecContext(ctx, `INSERT INTO facts (
		id, watch_id, subject, predicate, path, value, root, source_url, receipt,
		snapshot_id, created_verification_id, created_claim_index,
		latest_verification_id, latest_claim_index, observed_at, valid_from,
		last_verified_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		fact.ID, watch.ID, watch.Spec.Subject, watch.Spec.Predicate, row.path,
		string(row.oldValue), root, sourceURL, row.receipt, row.newSnapshotID,
		row.verificationID, row.claimIndex, row.verificationID, row.claimIndex,
		stamp, stamp, stamp)
	if err != nil {
		return Fact{}, recorderDatabaseError(ctx, err)
	}
	if err := recorderRequireOneRow(ctx, result); err != nil {
		return Fact{}, err
	}
	return fact, nil
}

func initialFactValue(
	watch Watch,
	row authoritativeVerification,
	sourceURL string,
	root string,
) Fact {
	return Fact{
		ID:      deterministicFactID(watch.ID, row.verificationID, row.claimIndex, row.path),
		WatchID: watch.ID, Subject: watch.Spec.Subject, Predicate: watch.Spec.Predicate,
		Path: row.path, Value: append(json.RawMessage(nil), row.oldValue...), Root: root,
		SourceURL: sourceURL, Receipt: row.receipt, SnapshotID: row.newSnapshotID,
		CreatedVerificationID: row.verificationID, CreatedClaimIndex: row.claimIndex,
		LatestVerificationID: row.verificationID, LatestClaimIndex: row.claimIndex,
		ObservedAt: row.verifiedAt, ValidFrom: row.verifiedAt, LastVerifiedAt: row.verifiedAt,
	}
}

func refreshOpenFact(
	ctx context.Context,
	tx ledger.WriteTx,
	old Fact,
	row authoritativeVerification,
	sourceURL string,
	root string,
) (Fact, error) {
	result, err := tx.ExecContext(ctx, `UPDATE facts SET
		root = ?, source_url = ?, receipt = ?, snapshot_id = ?,
		latest_verification_id = ?, latest_claim_index = ?, last_verified_at = ?
		WHERE id = ? AND watch_id = ? AND valid_to IS NULL`,
		root, sourceURL, row.receipt, row.newSnapshotID, row.verificationID,
		row.claimIndex, formatTime(row.verifiedAt), old.ID, old.WatchID)
	if err != nil {
		return Fact{}, recorderDatabaseError(ctx, err)
	}
	if err := recorderRequireOneRow(ctx, result); err != nil {
		return Fact{}, err
	}
	return refreshedFactValue(old, row, sourceURL, root), nil
}

func refreshedFactValue(old Fact, row authoritativeVerification, sourceURL, root string) Fact {
	old.Root = strings.Clone(root)
	old.SourceURL = strings.Clone(sourceURL)
	old.Receipt = strings.Clone(row.receipt)
	old.SnapshotID = strings.Clone(row.newSnapshotID)
	old.LatestVerificationID = strings.Clone(row.verificationID)
	old.LatestClaimIndex = row.claimIndex
	old.LastVerifiedAt = row.verifiedAt
	return cloneFact(old)
}

func replaceOpenFact(
	ctx context.Context,
	tx ledger.WriteTx,
	old Fact,
	row authoritativeVerification,
	sourceURL string,
	root string,
) (Fact, error) {
	if !row.verifiedAt.After(old.ValidFrom) {
		return Fact{}, ErrVerificationMismatch
	}
	successor := successorFactValue(old, row, sourceURL, root)
	newID := successor.ID
	stamp := formatTime(row.verifiedAt)
	closed, err := tx.ExecContext(ctx, `UPDATE facts SET
		valid_to = ?, closed_verification_id = ?, closed_claim_index = ?,
		superseded_by = ?, closed_outcome = 'changed', gone_scope = ''
		WHERE id = ? AND watch_id = ? AND valid_to IS NULL`,
		stamp, row.verificationID, row.claimIndex, newID, old.ID, old.WatchID)
	if err != nil {
		return Fact{}, recorderDatabaseError(ctx, err)
	}
	if err := recorderRequireOneRow(ctx, closed); err != nil {
		return Fact{}, err
	}
	inserted, err := tx.ExecContext(ctx, `INSERT INTO facts (
		id, watch_id, subject, predicate, path, value, root, source_url, receipt,
		snapshot_id, created_verification_id, created_claim_index,
		latest_verification_id, latest_claim_index, observed_at, valid_from,
		last_verified_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		newID, old.WatchID, old.Subject, old.Predicate, row.path,
		string(row.newValue), root, sourceURL, row.receipt, row.newSnapshotID,
		row.verificationID, row.claimIndex, row.verificationID, row.claimIndex,
		stamp, stamp, stamp)
	if err != nil {
		return Fact{}, recorderDatabaseError(ctx, err)
	}
	if err := recorderRequireOneRow(ctx, inserted); err != nil {
		return Fact{}, err
	}
	return successor, nil
}

func changedPredecessorValue(old Fact, row authoritativeVerification) Fact {
	old.ValidTo = timePointer(row.verifiedAt)
	old.ClosedVerificationID = strings.Clone(row.verificationID)
	index := row.claimIndex
	old.ClosedClaimIndex = &index
	old.SupersededBy = deterministicFactID(old.WatchID, row.verificationID, row.claimIndex, row.path)
	old.ClosedOutcome = ledger.OutcomeChanged
	old.GoneScope = ""
	return cloneFact(old)
}

func successorFactValue(old Fact, row authoritativeVerification, sourceURL, root string) Fact {
	return Fact{
		ID:      deterministicFactID(old.WatchID, row.verificationID, row.claimIndex, row.path),
		WatchID: old.WatchID, Subject: old.Subject, Predicate: old.Predicate,
		Path: row.path, Value: append(json.RawMessage(nil), row.newValue...), Root: root,
		SourceURL: sourceURL, Receipt: row.receipt, SnapshotID: row.newSnapshotID,
		CreatedVerificationID: row.verificationID, CreatedClaimIndex: row.claimIndex,
		LatestVerificationID: row.verificationID, LatestClaimIndex: row.claimIndex,
		ObservedAt: row.verifiedAt, ValidFrom: row.verifiedAt, LastVerifiedAt: row.verifiedAt,
	}
}

func closeGoneFact(
	ctx context.Context,
	tx ledger.WriteTx,
	old Fact,
	row authoritativeVerification,
) (Fact, error) {
	if !row.verifiedAt.After(old.ValidFrom) {
		return Fact{}, ErrVerificationMismatch
	}
	stamp := formatTime(row.verifiedAt)
	result, err := tx.ExecContext(ctx, `UPDATE facts SET
		valid_to = ?, closed_verification_id = ?, closed_claim_index = ?,
		superseded_by = NULL, closed_outcome = 'gone', gone_scope = ?
		WHERE id = ? AND watch_id = ? AND valid_to IS NULL`,
		stamp, row.verificationID, row.claimIndex, string(row.goneScope), old.ID, old.WatchID)
	if err != nil {
		return Fact{}, recorderDatabaseError(ctx, err)
	}
	if err := recorderRequireOneRow(ctx, result); err != nil {
		return Fact{}, err
	}
	return goneFactValue(old, row), nil
}

func goneFactValue(old Fact, row authoritativeVerification) Fact {
	old.ValidTo = timePointer(row.verifiedAt)
	old.ClosedVerificationID = strings.Clone(row.verificationID)
	index := row.claimIndex
	old.ClosedClaimIndex = &index
	old.SupersededBy = ""
	old.ClosedOutcome = ledger.OutcomeGone
	old.GoneScope = row.goneScope
	return cloneFact(old)
}

type watchSuccessKind uint8

const (
	watchSuccessBootstrap watchSuccessKind = iota + 1
	watchSuccessConfirmed
	watchSuccessChanged
)

type successfulSchedule struct {
	ewma       time.Duration
	lastChange *time.Time
	nextCheck  time.Time
}

func updateSuccessfulWatch(
	ctx context.Context,
	tx ledger.WriteTx,
	stored storedWatch,
	lease Lease,
	row authoritativeVerification,
	kind watchSuccessKind,
	now time.Time,
) error {
	if !row.verifiedAt.Before(lease.Until) {
		return ErrVerificationLeaseLost
	}
	schedule, err := successfulScheduleFor(stored.Watch, row, kind)
	if err != nil {
		return err
	}
	updated := now
	if row.verifiedAt.After(updated) {
		updated = row.verifiedAt
	}
	updated, err = advanceTime(updated, stored.Watch.UpdatedAt)
	if err != nil {
		return err
	}
	var lastChangeValue any
	if schedule.lastChange != nil {
		lastChangeValue = formatTime(*schedule.lastChange)
	}
	result, err := tx.ExecContext(ctx, `UPDATE watches SET
		state = 'active', next_check_at = ?, ewma_interval_s = ?,
		last_change_at = ?, last_checked_at = ?, consecutive_failures = 0,
		last_error_code = '', last_verification_id = ?,
		last_verification_claim_index = ?, lease_id = NULL, lease_until = NULL,
		updated_at = ?
		WHERE id = ? AND state = ? AND lease_id = ? AND lease_until = ?`,
		formatTime(schedule.nextCheck), float64(schedule.ewma)/float64(time.Second), lastChangeValue,
		formatTime(row.verifiedAt), row.verificationID, row.claimIndex,
		formatTime(updated), stored.Watch.ID, string(stored.Watch.State),
		lease.ID, formatTime(lease.Until))
	if err != nil {
		return recorderDatabaseError(ctx, err)
	}
	return recorderRequireOneRow(ctx, result)
}

func successfulScheduleFor(watch Watch, row authoritativeVerification, kind watchSuccessKind) (successfulSchedule, error) {
	schedule := successfulSchedule{
		ewma: watch.EWMAInterval, lastChange: cloneTimePointer(watch.LastChangeAt),
	}
	var nextInterval time.Duration
	switch kind {
	case watchSuccessBootstrap:
		schedule.lastChange = timePointer(row.verifiedAt)
		nextInterval = clampScheduleInterval(schedule.ewma / 2)
	case watchSuccessConfirmed:
		nextInterval = clampScheduleInterval(schedule.ewma + schedule.ewma/2)
	case watchSuccessChanged:
		observed := schedule.ewma
		if schedule.lastChange != nil {
			if !row.verifiedAt.After(*schedule.lastChange) {
				return successfulSchedule{}, ErrVerificationMismatch
			}
			observed = row.verifiedAt.Sub(*schedule.lastChange)
			if observed <= 0 {
				return successfulSchedule{}, ErrVerificationMismatch
			}
		}
		var err error
		schedule.ewma, err = weightedEWMA(observed, schedule.ewma)
		if err != nil {
			return successfulSchedule{}, err
		}
		schedule.lastChange = timePointer(row.verifiedAt)
		nextInterval = clampScheduleInterval(schedule.ewma / 2)
	default:
		return successfulSchedule{}, ErrVerificationMismatch
	}
	if schedule.ewma < 10*time.Minute || schedule.ewma > 7*24*time.Hour {
		return successfulSchedule{}, ErrVerificationMismatch
	}
	schedule.nextCheck = row.verifiedAt.Add(nextInterval)
	if !validPublicTime(schedule.nextCheck) || !schedule.nextCheck.After(row.verifiedAt) {
		return successfulSchedule{}, ErrVerificationMismatch
	}
	return schedule, nil
}

func weightedEWMA(observed, old time.Duration) (time.Duration, error) {
	if observed <= 0 || old < 10*time.Minute || old > 7*24*time.Hour {
		return 0, ErrVerificationMismatch
	}
	maximum := 7 * 24 * time.Hour
	threshold := (10*maximum - 7*old) / 3
	if observed > threshold {
		return maximum, nil
	}
	weighted := (3*observed + 7*old) / 10
	return clampScheduleInterval(weighted), nil
}

func clampScheduleInterval(value time.Duration) time.Duration {
	if value < 10*time.Minute {
		return 10 * time.Minute
	}
	if value > 7*24*time.Hour {
		return 7 * 24 * time.Hour
	}
	return value
}

func verificationSource(row authoritativeVerification) (string, string, error) {
	canonicalURL, _, err := publicnet.NormalizeHTTPURL(row.url, nil, false)
	if err != nil || canonicalURL != row.url {
		return "", "", ErrVerificationMismatch
	}
	source := row.url
	if row.finalURL != "" {
		source = row.finalURL
	}
	canonical, parsed, err := publicnet.NormalizeHTTPURL(source, nil, false)
	if err != nil || canonical != source || parsed == nil {
		return "", "", ErrVerificationMismatch
	}
	root, err := sourceRoot(parsed.Hostname())
	if err != nil {
		return "", "", ErrVerificationMismatch
	}
	return strings.Clone(canonical), strings.Clone(root), nil
}

func prepareVerificationBaseline(predicate string, source VerificationBaseline) (VerificationBaseline, error) {
	expectedPath := escapedPredicatePath(predicate)
	if source.Path != expectedPath || len(source.Path) < 1 || len(source.Path) > 4096 ||
		!validBoundedText(source.Path, true) || !validSnapshotID(source.SnapshotID) ||
		!validReceipt(source.Receipt) {
		return VerificationBaseline{}, ErrInvalidVerificationBinding
	}
	value, err := canonicalScalar(source.Value)
	if err != nil {
		return VerificationBaseline{}, ErrInvalidVerificationBinding
	}
	canonical, _, err := publicnet.NormalizeHTTPURL(source.SourceURL, nil, false)
	if err != nil || canonical != source.SourceURL || len(canonical) > models.MaxSearchURLBytes {
		return VerificationBaseline{}, ErrInvalidVerificationBinding
	}
	return VerificationBaseline{
		Path: strings.Clone(expectedPath), Value: value, SourceURL: strings.Clone(canonical),
		SnapshotID: strings.Clone(source.SnapshotID), Receipt: strings.Clone(source.Receipt),
	}, nil
}

func canonicalScalar(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || len(raw) > maximumFactValue || !json.Valid(raw) {
		return nil, ErrInvalidVerificationBinding
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, ErrInvalidVerificationBinding
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, ErrInvalidVerificationBinding
	}
	switch decoded.(type) {
	case string, json.Number, bool:
	default:
		return nil, ErrInvalidVerificationBinding
	}
	encoded, err := json.Marshal(decoded)
	if err != nil || !bytes.Equal(encoded, raw) {
		return nil, ErrInvalidVerificationBinding
	}
	return append(json.RawMessage(nil), encoded...), nil
}

func validSnapshotID(value string) bool {
	return len(value) == 71 && strings.HasPrefix(value, "sha256:") && validHex(value[7:], 64)
}

func validReceipt(value string) bool {
	return len(value) >= 1 && len(value) <= maximumFactReceipt &&
		value == strings.TrimSpace(value) && validBoundedText(value, true)
}

func escapedPredicatePath(predicate string) string {
	return strings.ReplaceAll(predicate, ".", `\.`)
}

func deterministicFactID(watchID, verificationID string, claimIndex int, path string) string {
	hash := sha256.New()
	writeFrame(hash, factIdentityDomain)
	writeFrame(hash, watchID)
	writeFrame(hash, verificationID)
	writeFrame(hash, strconv.Itoa(claimIndex))
	writeFrame(hash, path)
	return hex.EncodeToString(hash.Sum(nil))
}

func validRecorderStore(store *Store) bool {
	return store != nil && store.ledger != nil && store.clock != nil && store.idGenerator != nil &&
		store.leaseDuration > 0 && store.leaseDuration <= DefaultLeaseDuration
}

func validClaimBinding(claim Claim) bool {
	if !validUUIDv4(claim.Watch.ID) || claim.Lease.WatchID != claim.Watch.ID ||
		!validUUIDv4(claim.Lease.ID) || !validPublicTime(claim.Lease.Until) ||
		claim.Lease.Until.Location() != time.UTC || !claim.Lease.Until.After(claim.Watch.UpdatedAt) {
		return false
	}
	normalized, _, err := normalizeFactSpec(claim.Watch.Spec)
	return err == nil && normalized == claim.Watch.Spec &&
		validPublicTime(claim.Watch.CreatedAt) && validPublicTime(claim.Watch.UpdatedAt) &&
		!claim.Watch.UpdatedAt.Before(claim.Watch.CreatedAt)
}

func watchMatchesClaim(actual, expected Watch) bool {
	return actual.ID == expected.ID && actual.Spec == expected.Spec && actual.State == expected.State &&
		timePointersEqual(actual.NextCheckAt, expected.NextCheckAt) &&
		actual.EWMAInterval == expected.EWMAInterval &&
		timePointersEqual(actual.LastChangeAt, expected.LastChangeAt) &&
		timePointersEqual(actual.LastCheckedAt, expected.LastCheckedAt) &&
		actual.ConsecutiveFailures == expected.ConsecutiveFailures &&
		actual.LastErrorCode == expected.LastErrorCode &&
		actual.LastVerificationID == expected.LastVerificationID &&
		intPointersEqual(actual.LastVerificationClaimIndex, expected.LastVerificationClaimIndex) &&
		actual.CreatedAt.Equal(expected.CreatedAt) && actual.UpdatedAt.Equal(expected.UpdatedAt) &&
		timePointersEqual(actual.PausedAt, expected.PausedAt)
}

func factsEqual(actual, expected Fact) bool {
	return actual.ID == expected.ID && actual.WatchID == expected.WatchID &&
		actual.Subject == expected.Subject && actual.Predicate == expected.Predicate &&
		actual.Path == expected.Path && bytes.Equal(actual.Value, expected.Value) &&
		actual.Root == expected.Root && actual.SourceURL == expected.SourceURL &&
		actual.Receipt == expected.Receipt && actual.SnapshotID == expected.SnapshotID &&
		actual.CreatedVerificationID == expected.CreatedVerificationID &&
		actual.CreatedClaimIndex == expected.CreatedClaimIndex &&
		actual.LatestVerificationID == expected.LatestVerificationID &&
		actual.LatestClaimIndex == expected.LatestClaimIndex &&
		actual.ClosedVerificationID == expected.ClosedVerificationID &&
		intPointersEqual(actual.ClosedClaimIndex, expected.ClosedClaimIndex) &&
		actual.ObservedAt.Equal(expected.ObservedAt) && actual.ValidFrom.Equal(expected.ValidFrom) &&
		actual.LastVerifiedAt.Equal(expected.LastVerifiedAt) &&
		timePointersEqual(actual.ValidTo, expected.ValidTo) && actual.SupersededBy == expected.SupersededBy &&
		actual.ClosedOutcome == expected.ClosedOutcome && actual.GoneScope == expected.GoneScope
}

func loadFactByID(ctx context.Context, tx ledger.ReadTx, id string) (Fact, bool, error) {
	fact, err := scanFact(tx.QueryRowContext(ctx,
		`SELECT `+factColumns+` FROM facts WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Fact{}, false, nil
	}
	if err != nil {
		return Fact{}, false, recorderDatabaseError(ctx, err)
	}
	return cloneFact(fact), true, nil
}

func watchMatchesCompleted(
	actual storedWatch,
	original Watch,
	row authoritativeVerification,
	schedule successfulSchedule,
) bool {
	if actual.Watch.ID != original.ID || actual.Watch.Spec != original.Spec ||
		actual.Watch.State != StateActive || actual.Watch.CreatedAt != original.CreatedAt ||
		actual.Watch.UpdatedAt.Before(row.verifiedAt) || !actual.Watch.UpdatedAt.After(original.UpdatedAt) ||
		actual.leaseID != "" || actual.leaseUntil != nil || actual.deletedAt != nil ||
		actual.Watch.PausedAt != nil || actual.Watch.NextCheckAt == nil ||
		!actual.Watch.NextCheckAt.Equal(schedule.nextCheck) ||
		actual.Watch.EWMAInterval != schedule.ewma ||
		!timePointersEqual(actual.Watch.LastChangeAt, schedule.lastChange) ||
		actual.Watch.LastCheckedAt == nil || !actual.Watch.LastCheckedAt.Equal(row.verifiedAt) ||
		actual.Watch.ConsecutiveFailures != 0 || actual.Watch.LastErrorCode != "" ||
		actual.Watch.LastVerificationID != row.verificationID ||
		actual.Watch.LastVerificationClaimIndex == nil ||
		*actual.Watch.LastVerificationClaimIndex != row.claimIndex {
		return false
	}
	return true
}

func timePointersEqual(first, second *time.Time) bool {
	if first == nil || second == nil {
		return first == nil && second == nil
	}
	return first.Equal(*second)
}

func intPointersEqual(first, second *int) bool {
	if first == nil || second == nil {
		return first == nil && second == nil
	}
	return *first == *second
}

func recorderRequireOneRow(ctx context.Context, result sql.Result) error {
	changed, err := result.RowsAffected()
	if err != nil {
		return recorderDatabaseError(ctx, err)
	}
	if changed != 1 {
		return ErrVerificationMismatch
	}
	return nil
}

func recorderDatabaseError(ctx context.Context, err error) error {
	if ctx != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return ErrVerificationMismatch
}

func cloneVerificationBaseline(value VerificationBaseline) VerificationBaseline {
	value.Path = strings.Clone(value.Path)
	value.Value = append(json.RawMessage(nil), value.Value...)
	value.SourceURL = strings.Clone(value.SourceURL)
	value.SnapshotID = strings.Clone(value.SnapshotID)
	value.Receipt = strings.Clone(value.Receipt)
	return value
}

func cloneVerificationRecordResult(value VerificationRecordResult) VerificationRecordResult {
	value.VerificationID = strings.Clone(value.VerificationID)
	if value.Fact != nil {
		fact := cloneFact(*value.Fact)
		value.Fact = &fact
	}
	if value.Retry != nil {
		retry := cloneVerificationBaseline(*value.Retry)
		value.Retry = &retry
	}
	return value
}

// Compile-time contracts guard the Engine-facing recorder surface.
var _ interface {
	RecordVerificationBatch(context.Context, []ledger.Verification, *ledger.OutboxEvent) error
} = (*VerificationRecorder)(nil)
