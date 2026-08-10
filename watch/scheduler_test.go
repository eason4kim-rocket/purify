package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/use-agent/purify/ledger"
	"github.com/use-agent/purify/models"
)

type schedulerFixture struct {
	durable  *ledger.Store
	store    *Store
	clock    *testClock
	verifier *fakeSchedulerVerifier
	answerer *fakeSchedulerAnswerer
}

func newSchedulerFixture(t *testing.T) schedulerFixture {
	t.Helper()
	clock := &testClock{now: testEpoch}
	ids := &testIDs{}
	durable, store := openWatchStore(t, WithClock(clock.Now), WithIDGenerator(ids.Generate))
	return schedulerFixture{
		durable:  durable,
		store:    store,
		clock:    clock,
		verifier: &fakeSchedulerVerifier{},
		answerer: &fakeSchedulerAnswerer{},
	}
}

func (fixture schedulerFixture) scheduler(t *testing.T) *Scheduler {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &Scheduler{
		store:         fixture.store,
		verifier:      fixture.verifier,
		answerer:      fixture.answerer,
		tick:          time.Hour,
		claimsPerTick: 8,
		logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		ctx:           ctx,
		cancel:        cancel,
	}
}

// createDueWatch creates one pending watch that is immediately due.
func (fixture schedulerFixture) createDueWatch(t *testing.T, subject string) Watch {
	t.Helper()
	created, made, err := fixture.store.Create(context.Background(), validSpec(subject))
	if err != nil || !made {
		t.Fatalf("Create() = (%#v,%v,%v)", created, made, err)
	}
	return created
}

func (fixture schedulerFixture) watch(t *testing.T, id string) Watch {
	t.Helper()
	value, err := fixture.store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	return value
}

const schedulerSourceURL = "https://www.example.com/pricing"

func schedulerBelief(value string) *models.AnswerBelief {
	return &models.AnswerBelief{
		Value: json.RawMessage(value),
		Evidence: []models.AnswerEvidence{{
			URL:        schedulerSourceURL,
			Root:       "example.com",
			SnapshotID: recorderSnapshot("0"),
		}},
		Receipts: map[string]string{schedulerSourceURL: "old-receipt-token"},
	}
}

func knownAnswer(value string) *models.AnswerResponse {
	return &models.AnswerResponse{Status: models.AnswerStatusKnown, Belief: schedulerBelief(value)}
}

type fakeSchedulerAnswerer struct {
	mu       sync.Mutex
	response *models.AnswerResponse
	err      error
	calls    int
}

func (answerer *fakeSchedulerAnswerer) Answer(ctx context.Context, request *models.AnswerRequest) (*models.AnswerResponse, error) {
	answerer.mu.Lock()
	defer answerer.mu.Unlock()
	answerer.calls++
	if request == nil || request.Spec.Subject == "" {
		return nil, errors.New("fake answerer received an invalid request")
	}
	if answerer.err != nil {
		return nil, answerer.err
	}
	return answerer.response, nil
}

// fakeSchedulerVerifier commits crafted ledger rows through the supplied
// recorder, mirroring how verify.Service records receipt-form verifications.
type fakeSchedulerVerifier struct {
	mu       sync.Mutex
	respond  func(call int, request models.VerifyRequest, recorder *VerificationRecorder) error
	receipts []string
	calls    int
}

func (verifier *fakeSchedulerVerifier) VerifyWithRecorder(
	ctx context.Context,
	request models.VerifyRequest,
	recorder *VerificationRecorder,
) (*models.VerifyResponse, error) {
	verifier.mu.Lock()
	verifier.calls++
	call := verifier.calls
	verifier.receipts = append(verifier.receipts, request.Receipt)
	respond := verifier.respond
	verifier.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if respond == nil {
		return nil, errors.New("fake verifier has no response configured")
	}
	if err := respond(call, request, recorder); err != nil {
		return nil, err
	}
	return &models.VerifyResponse{}, nil
}

// schedulerRow builds one durable verification row consistent with a baseline
// whose provenance the test controls.
func schedulerRow(
	id string,
	outcome ledger.Outcome,
	path string,
	oldValue json.RawMessage,
	oldSnapshot string,
	oldReceipt string,
	verifiedAt time.Time,
) ledger.Verification {
	row := ledger.Verification{
		VerificationID: id,
		ClaimIndex:     0,
		URL:            schedulerSourceURL,
		FinalURL:       schedulerSourceURL,
		Path:           path,
		OldValue:       append(json.RawMessage(nil), oldValue...),
		Outcome:        outcome,
		OldSnapshotID:  oldSnapshot,
		NewSnapshotID:  recorderSnapshot("a"),
		OldReceipt:     oldReceipt,
		VerifiedAt:     verifiedAt,
	}
	switch outcome {
	case ledger.OutcomeConfirmed:
		row.Receipt = "refreshed-receipt-token"
	case ledger.OutcomeChanged:
		row.NewValue = json.RawMessage(`"20"`)
		row.Receipt = "changed-receipt-token"
	case ledger.OutcomeGone:
		row.GoneScope = ledger.GoneScopeField
	}
	return row
}

func TestSchedulerBootstrapConfirmedCreatesFactAndActivates(t *testing.T) {
	fixture := newSchedulerFixture(t)
	created := fixture.createDueWatch(t, "scheduler bootstrap")
	fixture.answerer.response = knownAnswer(`"19"`)
	path := escapedPredicatePath(created.Spec.Predicate)
	fixture.verifier.respond = func(call int, request models.VerifyRequest, recorder *VerificationRecorder) error {
		if request.Receipt != "old-receipt-token" || request.URL != "" || len(request.Claims) != 0 {
			t.Fatalf("unexpected verify request %#v", request)
		}
		fixture.clock.Set(fixture.clock.Now().Add(time.Second))
		row := schedulerRow("verification-bootstrap", ledger.OutcomeConfirmed,
			path, json.RawMessage(`"19"`), recorderSnapshot("0"), "old-receipt-token", fixture.clock.Now())
		return recorder.RecordVerificationBatch(context.Background(), []ledger.Verification{row}, nil)
	}

	fixture.scheduler(t).drain()

	stored := fixture.watch(t, created.ID)
	if stored.State != StateActive || stored.ConsecutiveFailures != 0 || stored.LastErrorCode != "" ||
		stored.NextCheckAt == nil || stored.LastCheckedAt == nil {
		t.Fatalf("watch after bootstrap = %#v", stored)
	}
	facts := recorderFacts(t, fixture.durable, created.ID)
	if len(facts) != 1 || facts[0].ValidTo != nil || !bytes.Equal(facts[0].Value, json.RawMessage(`"19"`)) {
		t.Fatalf("facts after bootstrap = %#v", facts)
	}
	if leaseID, leaseUntil := recorderLease(t, fixture.durable, created.ID); leaseID != "" || leaseUntil != "" {
		t.Fatalf("lease survived bootstrap: (%q,%q)", leaseID, leaseUntil)
	}
	if fixture.answerer.calls != 1 || fixture.verifier.calls != 1 {
		t.Fatalf("calls = answer %d verify %d", fixture.answerer.calls, fixture.verifier.calls)
	}
}

func TestSchedulerBootstrapUnknownReleasesLeaseWithBackoff(t *testing.T) {
	fixture := newSchedulerFixture(t)
	created := fixture.createDueWatch(t, "scheduler unknown")
	fixture.answerer.response = &models.AnswerResponse{
		Status: models.AnswerStatusUnknown,
		Reason: models.AnswerUnknownInsufficient,
	}

	fixture.scheduler(t).drain()

	stored := fixture.watch(t, created.ID)
	if stored.State != StatePending || stored.ConsecutiveFailures != 1 ||
		stored.LastErrorCode != schedulerCodeAnswerUnknown || stored.NextCheckAt == nil {
		t.Fatalf("watch after unknown answer = %#v", stored)
	}
	wantRetry := fixture.clock.Now().Add(schedulerBaseRetryInterval)
	if !stored.NextCheckAt.Equal(wantRetry) {
		t.Fatalf("NextCheckAt = %v, want %v", stored.NextCheckAt, wantRetry)
	}
	if fixture.verifier.calls != 0 {
		t.Fatalf("verifier calls = %d", fixture.verifier.calls)
	}
	if count := recorderRowCount(t, fixture.durable, "facts"); count != 0 {
		t.Fatalf("facts = %d", count)
	}
}

func TestSchedulerBootstrapChangedRetriesOnceThenConfirms(t *testing.T) {
	fixture := newSchedulerFixture(t)
	created := fixture.createDueWatch(t, "scheduler retry")
	fixture.answerer.response = knownAnswer(`"19"`)
	path := escapedPredicatePath(created.Spec.Predicate)
	fixture.verifier.respond = func(call int, request models.VerifyRequest, recorder *VerificationRecorder) error {
		fixture.clock.Set(fixture.clock.Now().Add(time.Second))
		switch call {
		case 1:
			row := schedulerRow("verification-first", ledger.OutcomeChanged,
				path, json.RawMessage(`"19"`), recorderSnapshot("0"), "old-receipt-token", fixture.clock.Now())
			return recorder.RecordVerificationBatch(context.Background(), []ledger.Verification{row}, nil)
		case 2:
			if request.Receipt != "changed-receipt-token" {
				t.Fatalf("retry receipt = %q", request.Receipt)
			}
			row := schedulerRow("verification-second", ledger.OutcomeConfirmed,
				path, json.RawMessage(`"20"`), recorderSnapshot("a"), "changed-receipt-token", fixture.clock.Now())
			return recorder.RecordVerificationBatch(context.Background(), []ledger.Verification{row}, nil)
		default:
			t.Fatalf("unexpected verify call %d", call)
			return nil
		}
	}

	fixture.scheduler(t).drain()

	if fixture.verifier.calls != 2 {
		t.Fatalf("verifier calls = %d", fixture.verifier.calls)
	}
	stored := fixture.watch(t, created.ID)
	if stored.State != StateActive || stored.ConsecutiveFailures != 0 {
		t.Fatalf("watch after retry bootstrap = %#v", stored)
	}
	facts := recorderFacts(t, fixture.durable, created.ID)
	if len(facts) != 1 || !bytes.Equal(facts[0].Value, json.RawMessage(`"20"`)) {
		t.Fatalf("facts after retry bootstrap = %#v", facts)
	}
}

func TestSchedulerBootstrapChangedTwiceReleasesUnstable(t *testing.T) {
	fixture := newSchedulerFixture(t)
	created := fixture.createDueWatch(t, "scheduler unstable")
	fixture.answerer.response = knownAnswer(`"19"`)
	path := escapedPredicatePath(created.Spec.Predicate)
	values := []json.RawMessage{json.RawMessage(`"19"`), json.RawMessage(`"20"`)}
	snapshots := []string{recorderSnapshot("0"), recorderSnapshot("a")}
	receipts := []string{"old-receipt-token", "changed-receipt-token"}
	fixture.verifier.respond = func(call int, request models.VerifyRequest, recorder *VerificationRecorder) error {
		if call > 2 {
			t.Fatalf("unexpected verify call %d", call)
		}
		fixture.clock.Set(fixture.clock.Now().Add(time.Second))
		row := schedulerRow("verification-"+request.Receipt, ledger.OutcomeChanged,
			path, values[call-1], snapshots[call-1], receipts[call-1], fixture.clock.Now())
		return recorder.RecordVerificationBatch(context.Background(), []ledger.Verification{row}, nil)
	}

	fixture.scheduler(t).drain()

	stored := fixture.watch(t, created.ID)
	if stored.State != StatePending || stored.ConsecutiveFailures != 1 ||
		stored.LastErrorCode != schedulerCodeBootstrapUnstable {
		t.Fatalf("watch after unstable bootstrap = %#v", stored)
	}
	if count := recorderRowCount(t, fixture.durable, "facts"); count != 0 {
		t.Fatalf("facts = %d", count)
	}
}

func TestSchedulerBootstrapGoneReleasesLease(t *testing.T) {
	fixture := newSchedulerFixture(t)
	created := fixture.createDueWatch(t, "scheduler gone bootstrap")
	fixture.answerer.response = knownAnswer(`"19"`)
	path := escapedPredicatePath(created.Spec.Predicate)
	fixture.verifier.respond = func(call int, request models.VerifyRequest, recorder *VerificationRecorder) error {
		fixture.clock.Set(fixture.clock.Now().Add(time.Second))
		row := schedulerRow("verification-gone", ledger.OutcomeGone,
			path, json.RawMessage(`"19"`), recorderSnapshot("0"), "old-receipt-token", fixture.clock.Now())
		return recorder.RecordVerificationBatch(context.Background(), []ledger.Verification{row}, nil)
	}

	fixture.scheduler(t).drain()

	stored := fixture.watch(t, created.ID)
	if stored.State != StatePending || stored.ConsecutiveFailures != 1 ||
		stored.LastErrorCode != schedulerCodeBootstrapGone {
		t.Fatalf("watch after gone bootstrap = %#v", stored)
	}
}

func TestSchedulerVerifierErrorReleasesLease(t *testing.T) {
	fixture := newSchedulerFixture(t)
	created := fixture.createDueWatch(t, "scheduler verify error")
	fixture.answerer.response = knownAnswer(`"19"`)
	fixture.verifier.respond = func(int, models.VerifyRequest, *VerificationRecorder) error {
		return errors.New("verification transport failed")
	}

	fixture.scheduler(t).drain()

	stored := fixture.watch(t, created.ID)
	if stored.State != StatePending || stored.ConsecutiveFailures != 1 ||
		stored.LastErrorCode != schedulerCodeVerifyFailed {
		t.Fatalf("watch after verify error = %#v", stored)
	}
}

func TestSchedulerAnswerFailureUsesExponentialBackoff(t *testing.T) {
	fixture := newSchedulerFixture(t)
	created := fixture.createDueWatch(t, "scheduler backoff")
	fixture.answerer.err = errors.New("answer failed")
	scheduler := fixture.scheduler(t)

	for attempt := 1; attempt <= 3; attempt++ {
		scheduler.drain()
		stored := fixture.watch(t, created.ID)
		if stored.ConsecutiveFailures != attempt || stored.LastErrorCode != schedulerCodeAnswerFailed {
			t.Fatalf("attempt %d watch = %#v", attempt, stored)
		}
		want := fixture.clock.Now().Add(schedulerBaseRetryInterval << (attempt - 1))
		if stored.NextCheckAt == nil || !stored.NextCheckAt.Equal(want) {
			t.Fatalf("attempt %d NextCheckAt = %v, want %v", attempt, stored.NextCheckAt, want)
		}
		fixture.clock.Set(*stored.NextCheckAt)
	}
}

// seedActiveFact bootstraps one watch to an active open fact so fact-mode
// tests start from committed durable state.
func (fixture schedulerFixture) seedActiveFact(t *testing.T, subject string) (Watch, Fact) {
	t.Helper()
	created := fixture.createDueWatch(t, subject)
	claim, found, err := fixture.store.ClaimDue(context.Background())
	if err != nil || !found || claim.Watch.ID != created.ID {
		t.Fatalf("ClaimDue() = (%#v,%v,%v)", claim, found, err)
	}
	path := escapedPredicatePath(created.Spec.Predicate)
	baseline := VerificationBaseline{
		Path:       path,
		Value:      json.RawMessage(`"19"`),
		SourceURL:  schedulerSourceURL,
		SnapshotID: recorderSnapshot("0"),
		Receipt:    "old-receipt-token",
	}
	recorder, err := NewBootstrapVerificationRecorder(fixture.store, claim, baseline)
	if err != nil {
		t.Fatal(err)
	}
	fixture.clock.Set(fixture.clock.Now().Add(time.Second))
	row := schedulerRow("verification-seed", ledger.OutcomeConfirmed,
		path, baseline.Value, baseline.SnapshotID, baseline.Receipt, fixture.clock.Now())
	if err := recorder.RecordVerificationBatch(context.Background(), []ledger.Verification{row}, nil); err != nil {
		t.Fatalf("seed RecordVerificationBatch() error = %v", err)
	}
	result, has := recorder.Result()
	if !has || result.Fact == nil {
		t.Fatalf("seed result = (%#v,%v)", result, has)
	}
	stored := fixture.watch(t, created.ID)
	if stored.NextCheckAt == nil {
		t.Fatalf("seeded watch has no schedule: %#v", stored)
	}
	fixture.clock.Set(*stored.NextCheckAt)
	return stored, *result.Fact
}

func TestSchedulerFactModeConfirmedRefreshesFact(t *testing.T) {
	fixture := newSchedulerFixture(t)
	seeded, fact := fixture.seedActiveFact(t, "scheduler fact confirmed")
	path := fact.Path
	fixture.verifier.respond = func(call int, request models.VerifyRequest, recorder *VerificationRecorder) error {
		if request.Receipt != "refreshed-receipt-token" {
			t.Fatalf("fact-mode receipt = %q", request.Receipt)
		}
		fixture.clock.Set(fixture.clock.Now().Add(time.Second))
		row := schedulerRow("verification-refresh", ledger.OutcomeConfirmed,
			path, fact.Value, fact.SnapshotID, fact.Receipt, fixture.clock.Now())
		return recorder.RecordVerificationBatch(context.Background(), []ledger.Verification{row}, nil)
	}

	fixture.scheduler(t).drain()

	if fixture.answerer.calls != 0 {
		t.Fatalf("fact mode consulted answer %d times", fixture.answerer.calls)
	}
	facts := recorderFacts(t, fixture.durable, seeded.ID)
	if len(facts) != 1 || facts[0].ValidTo != nil || facts[0].Receipt != "refreshed-receipt-token" {
		t.Fatalf("facts after refresh = %#v", facts)
	}
	stored := fixture.watch(t, seeded.ID)
	if stored.State != StateActive || stored.ConsecutiveFailures != 0 || stored.LastErrorCode != "" {
		t.Fatalf("watch after refresh = %#v", stored)
	}
}

func TestSchedulerFactModeGoneClosesFactThenRebootstraps(t *testing.T) {
	fixture := newSchedulerFixture(t)
	seeded, fact := fixture.seedActiveFact(t, "scheduler fact gone")
	path := fact.Path
	fixture.verifier.respond = func(call int, request models.VerifyRequest, recorder *VerificationRecorder) error {
		fixture.clock.Set(fixture.clock.Now().Add(time.Second))
		row := schedulerRow("verification-gone", ledger.OutcomeGone,
			path, fact.Value, fact.SnapshotID, fact.Receipt, fixture.clock.Now())
		row.GoneScope = ledger.GoneScopePage
		return recorder.RecordVerificationBatch(context.Background(), []ledger.Verification{row}, nil)
	}

	fixture.scheduler(t).drain()

	facts := recorderFacts(t, fixture.durable, seeded.ID)
	if len(facts) != 1 || facts[0].ValidTo == nil || facts[0].ClosedOutcome != ledger.OutcomeGone {
		t.Fatalf("facts after gone = %#v", facts)
	}
	stored := fixture.watch(t, seeded.ID)
	if stored.State != StateActive || stored.NextCheckAt == nil {
		t.Fatalf("watch after gone = %#v", stored)
	}

	// The active watch now has no open fact: the next due claim re-bootstraps
	// through Answer and inserts a fresh open fact.
	fixture.clock.Set(*stored.NextCheckAt)
	fixture.answerer.response = knownAnswer(`"21"`)
	fixture.verifier.respond = func(call int, request models.VerifyRequest, recorder *VerificationRecorder) error {
		if request.Receipt != "old-receipt-token" {
			t.Fatalf("re-bootstrap receipt = %q", request.Receipt)
		}
		fixture.clock.Set(fixture.clock.Now().Add(time.Second))
		row := schedulerRow("verification-rebootstrap", ledger.OutcomeConfirmed,
			path, json.RawMessage(`"21"`), recorderSnapshot("0"), "old-receipt-token", fixture.clock.Now())
		return recorder.RecordVerificationBatch(context.Background(), []ledger.Verification{row}, nil)
	}

	fixture.scheduler(t).drain()

	if fixture.answerer.calls != 1 {
		t.Fatalf("re-bootstrap consulted answer %d times", fixture.answerer.calls)
	}
	facts = recorderFacts(t, fixture.durable, seeded.ID)
	if len(facts) != 2 {
		t.Fatalf("facts after re-bootstrap = %#v", facts)
	}
	var open *Fact
	for index := range facts {
		if facts[index].ValidTo == nil {
			open = &facts[index]
		}
	}
	if open == nil || !bytes.Equal(open.Value, json.RawMessage(`"21"`)) {
		t.Fatalf("open fact after re-bootstrap = %#v", open)
	}
}

func TestSchedulerDrainRespectsClaimBudgetAndEmptyQueue(t *testing.T) {
	fixture := newSchedulerFixture(t)
	for _, subject := range []string{"budget one", "budget two", "budget three"} {
		fixture.createDueWatch(t, subject)
	}
	fixture.answerer.err = errors.New("answer failed")
	scheduler := fixture.scheduler(t)
	scheduler.claimsPerTick = 2

	scheduler.drain()
	if fixture.answerer.calls != 2 {
		t.Fatalf("first drain processed %d claims", fixture.answerer.calls)
	}
	scheduler.drain()
	if fixture.answerer.calls != 3 {
		t.Fatalf("second drain processed %d total claims", fixture.answerer.calls)
	}
	// Every watch is now scheduled in the future; a further drain is a no-op.
	scheduler.drain()
	if fixture.answerer.calls != 3 {
		t.Fatalf("idle drain processed %d total claims", fixture.answerer.calls)
	}
}

func TestSchedulerRetryIntervalIsBoundedAndMonotonic(t *testing.T) {
	previous := time.Duration(0)
	for failures := 0; failures <= schedulerMaximumRetryShift+3; failures++ {
		interval := schedulerRetryInterval(failures)
		if interval < schedulerBaseRetryInterval || interval > maximumRetryInterval {
			t.Fatalf("interval(%d) = %v out of bounds", failures, interval)
		}
		if interval < previous {
			t.Fatalf("interval(%d) = %v decreased from %v", failures, interval, previous)
		}
		previous = interval
	}
	if schedulerRetryInterval(-1) != schedulerBaseRetryInterval {
		t.Fatalf("negative failures interval = %v", schedulerRetryInterval(-1))
	}
	if schedulerRetryInterval(1000) != maximumRetryInterval {
		t.Fatalf("saturated interval = %v", schedulerRetryInterval(1000))
	}
}

func TestNewSchedulerValidatesConfigurationAndLifecycle(t *testing.T) {
	fixture := newSchedulerFixture(t)
	if _, err := NewScheduler(nil, fixture.store, fixture.verifier, fixture.answerer, SchedulerOptions{}); !errors.Is(err, ErrInvalidScheduler) {
		t.Fatalf("nil parent error = %v", err)
	}
	if _, err := NewScheduler(context.Background(), nil, fixture.verifier, fixture.answerer, SchedulerOptions{}); !errors.Is(err, ErrInvalidScheduler) {
		t.Fatalf("nil store error = %v", err)
	}
	if _, err := NewScheduler(context.Background(), fixture.store, nil, fixture.answerer, SchedulerOptions{}); !errors.Is(err, ErrInvalidScheduler) {
		t.Fatalf("nil verifier error = %v", err)
	}
	var typedNil *fakeSchedulerAnswerer
	if _, err := NewScheduler(context.Background(), fixture.store, fixture.verifier, typedNil, SchedulerOptions{}); !errors.Is(err, ErrInvalidScheduler) {
		t.Fatalf("typed-nil answerer error = %v", err)
	}
	if _, err := NewScheduler(context.Background(), fixture.store, fixture.verifier, fixture.answerer, SchedulerOptions{Tick: -time.Second}); !errors.Is(err, ErrInvalidScheduler) {
		t.Fatalf("negative tick error = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewScheduler(canceled, fixture.store, fixture.verifier, fixture.answerer, SchedulerOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled parent error = %v", err)
	}

	scheduler, err := NewScheduler(context.Background(), fixture.store, fixture.verifier, fixture.answerer, SchedulerOptions{Tick: time.Hour})
	if err != nil {
		t.Fatalf("NewScheduler() error = %v", err)
	}
	var closers sync.WaitGroup
	for range 8 {
		closers.Add(1)
		go func() {
			defer closers.Done()
			if err := scheduler.Close(); err != nil {
				t.Errorf("Close() error = %v", err)
			}
		}()
	}
	closers.Wait()
	if err := (*Scheduler)(nil).Close(); err != nil {
		t.Fatalf("nil Close() error = %v", err)
	}
}

func TestSchedulerInitialDrainProcessesDueWatch(t *testing.T) {
	fixture := newSchedulerFixture(t)
	created := fixture.createDueWatch(t, "scheduler initial drain")
	processed := make(chan struct{})
	fixture.answerer.response = knownAnswer(`"19"`)
	path := escapedPredicatePath(created.Spec.Predicate)
	fixture.verifier.respond = func(call int, request models.VerifyRequest, recorder *VerificationRecorder) error {
		defer close(processed)
		fixture.clock.Set(fixture.clock.Now().Add(time.Second))
		row := schedulerRow("verification-initial", ledger.OutcomeConfirmed,
			path, json.RawMessage(`"19"`), recorderSnapshot("0"), "old-receipt-token", fixture.clock.Now())
		return recorder.RecordVerificationBatch(context.Background(), []ledger.Verification{row}, nil)
	}

	scheduler, err := NewScheduler(context.Background(), fixture.store, fixture.verifier, fixture.answerer, SchedulerOptions{Tick: time.Hour})
	if err != nil {
		t.Fatalf("NewScheduler() error = %v", err)
	}
	defer scheduler.Close()

	select {
	case <-processed:
	case <-time.After(10 * time.Second):
		t.Fatal("initial drain did not process the due watch")
	}
	if err := scheduler.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	stored := fixture.watch(t, created.ID)
	if stored.State != StateActive {
		t.Fatalf("watch after initial drain = %#v", stored)
	}
}
