package compiler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/use-agent/purify/ledger"
	"github.com/use-agent/purify/snapshot"
)

type coordinatorTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *coordinatorTestClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *coordinatorTestClock) Set(value time.Time) {
	clock.mu.Lock()
	clock.now = value
	clock.mu.Unlock()
}

func (clock *coordinatorTestClock) Add(delta time.Duration) { clock.Set(clock.Now().Add(delta)) }

type coordinatorHarness struct {
	coordinator *Coordinator
	catalog     *SampleCatalog
	registry    *Store
	durable     *ledger.Store
	snapshots   *snapshot.Store
	clock       *coordinatorTestClock
}

func TestCoordinatorObserveSingleflightDirtyAndRequestCancellation(t *testing.T) {
	harness := newCoordinatorHarness(t)
	coordinator := harness.coordinator
	var compileCalls atomic.Int32
	var unrelatedLoaded atomic.Bool
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	coordinator.compileIR = func(ctx context.Context, samples []Sample, schema json.RawMessage, _ TruthExtractor) (IR, ValidationReport, error) {
		call := compileCalls.Add(1)
		if len(samples) < MinCompileSamples || len(samples) > MaxCompileSamples {
			return IR{}, ValidationReport{}, fmt.Errorf("unexpected samples: %d", len(samples))
		}
		if call == 1 {
			close(firstStarted)
			select {
			case <-releaseFirst:
			case <-ctx.Done():
				return IR{}, ValidationReport{}, ctx.Err()
			}
		}
		if call == 2 && !unrelatedLoaded.Load() {
			return IR{}, ValidationReport{}, errors.New("dirty key bypassed queued work")
		}
		ir, report := extractorFixture("h1.name")
		return ir, report, nil
	}
	var saveCalls atomic.Int32
	var savedCenter atomic.Uint64
	coordinator.saveExtractor = func(_ context.Context, key PageKey, _ IR, _ ValidationReport) (Extractor, error) {
		saveCalls.Add(1)
		savedCenter.Store(key.TemplateSimHash)
		return Extractor{}, nil
	}

	schema := catalogSchema()
	var readySet SampleSet
	for index := 1; index <= 3; index++ {
		page, id, fetched := putCoordinatorPage(t, harness, index, schema)
		requestCtx, requestCancel := context.WithCancel(context.Background())
		if err := coordinator.Observe(requestCtx, page, string(id), fetched, schema); err != nil {
			t.Fatalf("Observe(%d) error = %v", index, err)
		}
		requestCancel()
		if index == 3 {
			var err error
			readySet, _, err = harness.catalog.Load(context.Background(), CompileKey{
				Host: page.Host, SchemaHash: page.SchemaHash, ContentProfile: DefaultCompilerProfile,
				TemplateClusterID: templateClusterID(page.TemplateSimHash), ClusterSimHash: page.TemplateSimHash,
			}, MaxCompileSamples)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	<-firstStarted

	// One hundred concurrent duplicate schedules remain one queued/running key.
	task := coordinatorTask{set: readySet, schema: append(json.RawMessage(nil), readySetSchema(t, schema)...)}
	var schedules sync.WaitGroup
	errCh := make(chan error, 100)
	for range 100 {
		schedules.Add(1)
		go func() {
			defer schedules.Done()
			errCh <- coordinator.schedule(task)
		}()
	}
	schedules.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("duplicate schedule error = %v", err)
		}
	}

	// A newer catalog revision arriving while the first compile runs marks the
	// key dirty and is compiled immediately after the first finishes.
	page, id, fetched := putCoordinatorPage(t, harness, 4, schema)
	newSet, ready, err := harness.catalog.Observe(context.Background(), page, string(id), fetched)
	if err != nil || !ready || newSet.Revision == readySet.Revision {
		t.Fatalf("new revision = %q ready=%v err=%v", newSet.Revision, ready, err)
	}
	if err := coordinator.schedule(coordinatorTask{set: newSet, schema: readySetSchema(t, schema)}); err != nil {
		t.Fatalf("schedule dirty revision: %v", err)
	}
	// Fill every unrelated queue slot after dirty B has already been accepted.
	// B must run directly after A rather than being dropped by a failed requeue.
	for index := 0; index < CoordinatorQueueSize; index++ {
		if err := coordinator.schedule(syntheticCoordinatorTask(900 + index)); err != nil {
			t.Fatalf("fill queue after dirty schedule %d: %v", index, err)
		}
	}
	originalLoad := coordinator.loadCatalog
	coordinator.loadCatalog = func(ctx context.Context, key CompileKey, limit int) (SampleSet, bool, error) {
		if key != newSet.Key {
			unrelatedLoaded.Store(true)
		}
		return originalLoad(ctx, key, limit)
	}
	close(releaseFirst)
	waitCoordinatorAttempt(t, harness.durable, newSet.Key, attemptSuccess, newSet.Revision)
	if got := compileCalls.Load(); got != 2 {
		t.Fatalf("compile calls = %d, want exactly old+dirty revisions", got)
	}
	if got := saveCalls.Load(); got != 2 {
		t.Fatalf("save calls = %d, want 2", got)
	}
	if got := savedCenter.Load(); got != newSet.Key.ClusterSimHash {
		t.Fatalf("saved template center = %#x, want fixed %#x", got, newSet.Key.ClusterSimHash)
	}
}

func TestCoordinatorHandoffHonorsTargetTerminalWatermark(t *testing.T) {
	tests := []struct {
		name    string
		outcome attemptOutcome
		reason  attemptReason
		delay   time.Duration
	}{
		{"success", attemptSuccess, reasonCompiled, 0},
		{"weak", attemptWeak, reasonValidationBelowThreshold, CoordinatorWeakDelay + time.Second},
		{"no candidate", attemptNoCandidate, reasonNoExtractorCandidate, CoordinatorWeakDelay + time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newCoordinatorHarness(t)
			set, schema := readyCoordinatorSet(t, harness, 1_100, MinCompileSamples)
			token, claimed, err := harness.coordinator.claim(context.Background(), set.Key, set.Revision)
			if err != nil || !claimed {
				t.Fatalf("target claim = %v/%v", claimed, err)
			}
			if err := harness.coordinator.finalize(context.Background(), set.Key, set.Revision, token,
				attemptResult{outcome: test.outcome, reason: test.reason}); err != nil {
				t.Fatal(err)
			}
			before := readCoordinatorAttempt(t, harness.durable, set.Key)
			harness.clock.Add(test.delay)
			harness.coordinator.loadCatalog = func(context.Context, CompileKey, int) (SampleSet, bool, error) {
				return cloneSampleSet(set), true, nil
			}
			var compileCalls atomic.Int32
			harness.coordinator.compileIR = func(context.Context, []Sample, json.RawMessage, TruthExtractor) (IR, ValidationReport, error) {
				compileCalls.Add(1)
				ir, report := extractorFixture("h1.name")
				return ir, report, nil
			}
			harness.coordinator.process(coordinatorTask{
				set:    SampleSet{Key: set.Key, Revision: coordinatorDigest(test.name + "-stale-a")},
				schema: schema,
			})
			after := readCoordinatorAttempt(t, harness.durable, set.Key)
			if compileCalls.Load() != 0 || after.attemptRevision != before.attemptRevision ||
				after.outcome != before.outcome || after.reason != before.reason || after.leaseID != "" {
				t.Fatalf("terminal watermark changed or recompiled: before=%#v after=%#v calls=%d",
					before, after, compileCalls.Load())
			}
		})
	}
}

func TestCoordinatorRevisionHandoffDoesNotReapplyExpiredCooldown(t *testing.T) {
	tests := []struct {
		name    string
		outcome attemptOutcome
		reason  attemptReason
		delay   time.Duration
	}{
		{"weak", attemptWeak, reasonValidationBelowThreshold, CoordinatorWeakDelay},
		{"transient", attemptTransient, reasonCompileFailed, CoordinatorTransientDelay},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			harness := newCoordinatorHarness(t)
			set, schema := readyCoordinatorSet(t, harness, 1_000, MinCompileSamples)
			priorRevision := coordinatorDigest(test.name + "-prior")
			token, claimed, err := harness.coordinator.claim(context.Background(), set.Key, priorRevision)
			if err != nil || !claimed {
				t.Fatalf("prior claim = %v/%v", claimed, err)
			}
			if err := harness.coordinator.finalize(context.Background(), set.Key, priorRevision, token,
				attemptResult{outcome: test.outcome, reason: test.reason}); err != nil {
				t.Fatal(err)
			}
			harness.clock.Add(test.delay + time.Second)
			queuedRevision := coordinatorDigest(test.name + "-stale-queued")
			harness.coordinator.loadCatalog = func(context.Context, CompileKey, int) (SampleSet, bool, error) {
				return cloneSampleSet(set), true, nil
			}
			harness.coordinator.compileIR = func(context.Context, []Sample, json.RawMessage, TruthExtractor) (IR, ValidationReport, error) {
				ir, report := extractorFixture("h1.name")
				return ir, report, nil
			}
			harness.coordinator.saveExtractor = func(context.Context, PageKey, IR, ValidationReport) (Extractor, error) {
				return Extractor{}, nil
			}
			harness.coordinator.process(coordinatorTask{
				set: SampleSet{Key: set.Key, Revision: queuedRevision}, schema: schema,
			})
			state := readCoordinatorAttempt(t, harness.durable, set.Key)
			if state.attemptRevision != set.Revision || state.outcome != attemptSuccess ||
				state.reason != reasonCompiled || state.leaseID != "" {
				t.Fatalf("handoff state = %#v, want immediate success for revision %s", state, set.Revision)
			}
		})
	}
}

func TestCoordinatorUnverifiedCatalogFailuresPreserveDurableWatermark(t *testing.T) {
	for _, test := range []struct {
		name string
		load func(context.Context, CompileKey, int) (SampleSet, bool, error)
	}{
		{
			name: "load error",
			load: func(context.Context, CompileKey, int) (SampleSet, bool, error) {
				return SampleSet{}, false, errors.New("catalog unavailable")
			},
		},
		{
			name: "load panic",
			load: func(context.Context, CompileKey, int) (SampleSet, bool, error) {
				panic("catalog panic")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newCoordinatorHarness(t)
			current, schema := readyCoordinatorSet(t, harness, 1_400, MinCompileSamples)
			token, claimed, err := harness.coordinator.claim(context.Background(), current.Key, current.Revision)
			if err != nil || !claimed {
				t.Fatalf("current claim = %v/%v", claimed, err)
			}
			if err := harness.coordinator.finalize(context.Background(), current.Key, current.Revision, token,
				attemptResult{outcome: attemptSuccess, reason: reasonCompiled}); err != nil {
				t.Fatal(err)
			}
			before := readCoordinatorAttempt(t, harness.durable, current.Key)
			harness.coordinator.loadCatalog = test.load
			harness.coordinator.process(coordinatorTask{
				set:    SampleSet{Key: current.Key, Revision: coordinatorDigest(test.name + "-stale")},
				schema: schema,
			})
			after := readCoordinatorAttempt(t, harness.durable, current.Key)
			if after.attemptRevision != before.attemptRevision || after.outcome != before.outcome ||
				after.reason != before.reason || after.cooldownUntil != before.cooldownUntil || after.leaseID != "" {
				t.Fatalf("durable watermark changed: before=%#v after=%#v", before, after)
			}
		})
	}
}

func TestCoordinatorCanceledHandoffReleasesUnverifiedLease(t *testing.T) {
	harness := newCoordinatorHarness(t)
	current, schema := readyCoordinatorSet(t, harness, 1_500, MinCompileSamples)
	queuedRevision := coordinatorDigest("canceled-handoff-stale")
	harness.coordinator.loadCatalog = func(context.Context, CompileKey, int) (SampleSet, bool, error) {
		harness.coordinator.cancel()
		return cloneSampleSet(current), true, nil
	}
	harness.coordinator.process(coordinatorTask{
		set: SampleSet{Key: current.Key, Revision: queuedRevision}, schema: schema,
	})
	state := readCoordinatorAttempt(t, harness.durable, current.Key)
	if state.attemptRevision != queuedRevision || state.outcome != attemptTransient ||
		state.reason != reasonTaskCanceled || state.leaseID != "" {
		t.Fatalf("canceled handoff state = %#v", state)
	}
}

func TestCoordinatorNotReadyLoadRecordsCurrentRevision(t *testing.T) {
	harness := newCoordinatorHarness(t)
	current, schema := readyCoordinatorSet(t, harness, 1_600, MinCompileSamples)
	current.Samples = append([]SampleRef(nil), current.Samples[:MinCompileSamples-1]...)
	current.Count = len(current.Samples)
	current.Revision = sampleSetRevision(current)
	queuedRevision := coordinatorDigest("not-ready-stale")
	harness.coordinator.loadCatalog = func(context.Context, CompileKey, int) (SampleSet, bool, error) {
		return cloneSampleSet(current), false, nil
	}
	harness.coordinator.process(coordinatorTask{
		set: SampleSet{Key: current.Key, Revision: queuedRevision}, schema: schema,
	})
	state := readCoordinatorAttempt(t, harness.durable, current.Key)
	if state.attemptRevision != current.Revision || state.outcome != attemptNoCandidate ||
		state.reason != reasonSamplesUnavailable || state.leaseID != "" {
		t.Fatalf("not-ready current revision state = %#v", state)
	}
}

func TestCoordinatorQueueFullDoesNotClaim(t *testing.T) {
	harness := newCoordinatorHarness(t)
	coordinator := harness.coordinator
	loadStarted := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	coordinator.loadCatalog = func(ctx context.Context, key CompileKey, _ int) (SampleSet, bool, error) {
		once.Do(func() { close(loadStarted) })
		select {
		case <-release:
			return SampleSet{}, false, nil
		case <-ctx.Done():
			return SampleSet{}, false, ctx.Err()
		}
	}
	first := syntheticCoordinatorTask(0)
	if err := coordinator.schedule(first); err != nil {
		t.Fatal(err)
	}
	<-loadStarted
	for index := 1; index <= CoordinatorQueueSize; index++ {
		if err := coordinator.schedule(syntheticCoordinatorTask(index)); err != nil {
			t.Fatalf("queue item %d: %v", index, err)
		}
	}
	overflow := syntheticCoordinatorTask(CoordinatorQueueSize + 1)
	if err := coordinator.schedule(overflow); !errors.Is(err, ErrCoordinatorQueueFull) {
		t.Fatalf("overflow error = %v", err)
	}
	assertCoordinatorAttemptMissing(t, harness.durable, overflow.set.Key)
	close(release)
}

func TestCoordinatorObserveRejectsSchemaBeforeCatalogWrite(t *testing.T) {
	harness := newCoordinatorHarness(t)
	page := catalogPageKey(t, 700, uint64(1)<<63)
	for _, schema := range []json.RawMessage{
		json.RawMessage(`{"other":"string"}`),
		json.RawMessage(`{"broken":`),
	} {
		err := harness.coordinator.Observe(context.Background(), page,
			catalogSnapshotID("must-not-write"), harness.clock.Now(), schema)
		if !errors.Is(err, ErrInvalidCoordinator) {
			t.Fatalf("Observe(invalid schema) error = %v", err)
		}
	}
	assertCatalogRowCount(t, harness.durable, 0)
}

func TestCoordinatorCrossProcessLeaseExpiryAndPersistentWatermarks(t *testing.T) {
	harness := newCoordinatorHarness(t)
	second, err := NewCoordinator(harness.catalog, harness.registry, harness.snapshots,
		TruthExtractorFunc(func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"name":"Purify"}`), nil
		}), harness.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })

	key := syntheticCoordinatorTask(200).set.Key
	revision := coordinatorDigest("revision-a")
	type claimResult struct {
		coordinator *Coordinator
		token       string
		claimed     bool
		err         error
	}
	claims := make(chan claimResult, 2)
	for _, value := range []*Coordinator{harness.coordinator, second} {
		go func(candidate *Coordinator) {
			token, claimed, err := candidate.claim(context.Background(), key, revision)
			claims <- claimResult{candidate, token, claimed, err}
		}(value)
	}
	first := <-claims
	other := <-claims
	if first.err != nil || other.err != nil || first.claimed == other.claimed {
		t.Fatalf("cross-process claims = %#v / %#v", first, other)
	}
	winner := first
	if !winner.claimed {
		winner = other
	}
	if _, claimed, err := second.claim(context.Background(), key, revision); err != nil || claimed {
		t.Fatalf("active lease duplicate claim = %v/%v", claimed, err)
	}

	// A crashed owner is replaceable only after its durable lease expires.
	harness.clock.Add(CoordinatorLeaseDuration + time.Second)
	token, claimed, err := second.claim(context.Background(), key, revision)
	if err != nil || !claimed || token == winner.token {
		t.Fatalf("expired lease takeover = token %q claimed=%v err=%v", token, claimed, err)
	}
	if err := second.finalize(context.Background(), key, revision, token,
		attemptResult{outcome: attemptSuccess, reason: reasonCompiled}); err != nil {
		t.Fatalf("finalize success: %v", err)
	}
	if _, claimed, err := harness.coordinator.claim(context.Background(), key, revision); err != nil || claimed {
		t.Fatalf("success same revision reran = %v/%v", claimed, err)
	}
	if _, claimed, err := harness.coordinator.claim(context.Background(), key, coordinatorDigest("revision-b")); err != nil || !claimed {
		t.Fatalf("success new revision claim = %v/%v", claimed, err)
	}

	assertAttemptCooldownPolicy(t, harness, second, attemptWeak, reasonValidationBelowThreshold, 300)
	assertAttemptCooldownPolicy(t, harness, second, attemptNoCandidate, reasonNoExtractorCandidate, 301)
	assertTransientRetryPolicy(t, harness, second, 302)
}

func TestCoordinatorOutcomeClassificationAndHydrationFailures(t *testing.T) {
	harness := newCoordinatorHarness(t)
	set, schema := readyCoordinatorSet(t, harness, 400, 3)
	task := coordinatorTask{set: set, schema: schema}

	tests := []struct {
		name   string
		setup  func()
		want   attemptOutcome
		reason attemptReason
	}{
		{"weak", func() {
			harness.coordinator.compileIR = func(context.Context, []Sample, json.RawMessage, TruthExtractor) (IR, ValidationReport, error) {
				return IR{}, ValidationReport{CanEnable: false}, nil
			}
		}, attemptWeak, reasonValidationBelowThreshold},
		{"no candidate", func() {
			harness.coordinator.compileIR = func(context.Context, []Sample, json.RawMessage, TruthExtractor) (IR, ValidationReport, error) {
				return IR{}, ValidationReport{}, ErrNoCandidate
			}
		}, attemptNoCandidate, reasonNoExtractorCandidate},
		{"unsupported", func() {
			harness.coordinator.compileIR = func(context.Context, []Sample, json.RawMessage, TruthExtractor) (IR, ValidationReport, error) {
				return IR{}, ValidationReport{}, ErrUnsupportedSchema
			}
		}, attemptNoCandidate, reasonNoExtractorCandidate},
		{"truth transient", func() {
			harness.coordinator.compileIR = func(context.Context, []Sample, json.RawMessage, TruthExtractor) (IR, ValidationReport, error) {
				return IR{}, ValidationReport{}, ErrTruth
			}
		}, attemptTransient, reasonCompileFailed},
		{"save transient", func() {
			harness.coordinator.compileIR = func(context.Context, []Sample, json.RawMessage, TruthExtractor) (IR, ValidationReport, error) {
				ir, report := extractorFixture("h1.name")
				return ir, report, nil
			}
			harness.coordinator.saveExtractor = func(context.Context, PageKey, IR, ValidationReport) (Extractor, error) {
				return Extractor{}, errors.New("database detail must not persist")
			}
		}, attemptTransient, reasonSaveFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.setup()
			got := harness.coordinator.runClaimed(context.Background(), task)
			if got.outcome != test.want || got.reason != test.reason {
				t.Fatalf("result = %#v, want %s/%s", got, test.want, test.reason)
			}
		})
	}

	originalRead := harness.coordinator.readContent
	originalObservation := harness.coordinator.hasObservation
	harness.coordinator.readContent = func(snapshot.ID, int) ([]byte, error) { return nil, errors.New("missing") }
	assertHydrationResult(t, harness.coordinator, set, schema, attemptTransient, reasonSnapshotUnavailable)
	harness.coordinator.readContent = originalRead
	harness.coordinator.hasObservation = func(context.Context, snapshot.ID, func(snapshot.Meta) bool) (bool, error) { return false, nil }
	assertHydrationResult(t, harness.coordinator, set, schema, attemptNoCandidate, reasonSampleProvenanceMismatch)
	harness.coordinator.hasObservation = originalObservation
	corrupt := cloneSampleSet(set)
	corrupt.Samples[0].SampleSimHash ^= 1
	assertHydrationResult(t, harness.coordinator, corrupt, schema, attemptNoCandidate, reasonSampleFingerprintMismatch)

	harness.coordinator.compileIR = func(context.Context, []Sample, json.RawMessage, TruthExtractor) (IR, ValidationReport, error) {
		panic("secret panic")
	}
	got := harness.coordinator.runClaimed(context.Background(), task)
	if got.outcome != attemptTransient || got.reason != reasonPanicRecovered {
		t.Fatalf("panic result = %#v", got)
	}
}

func TestCoordinatorCatalogFailureTimeoutCloseAndNilValidation(t *testing.T) {
	harness := newCoordinatorHarness(t)
	coordinator := harness.coordinator
	panicTask := syntheticCoordinatorTask(490)
	coordinator.loadCatalog = func(context.Context, CompileKey, int) (SampleSet, bool, error) {
		panic("catalog panic detail")
	}
	if err := coordinator.schedule(panicTask); err != nil {
		t.Fatal(err)
	}
	waitCoordinatorAttempt(t, harness.durable, panicTask.set.Key, attemptTransient, panicTask.set.Revision)
	if got := readCoordinatorAttempt(t, harness.durable, panicTask.set.Key).reason; got != reasonPanicRecovered {
		t.Fatalf("load panic reason = %q", got)
	}

	key := syntheticCoordinatorTask(500).set.Key
	revision := coordinatorDigest("catalog-failure-revision")
	coordinator.loadCatalog = func(context.Context, CompileKey, int) (SampleSet, bool, error) {
		return SampleSet{}, false, errors.New("catalog secret")
	}
	coordinator.process(coordinatorTask{set: SampleSet{Key: key, Revision: revision}})
	waitCoordinatorAttempt(t, harness.durable, key, attemptTransient, revision)
	state := readCoordinatorAttempt(t, harness.durable, key)
	if state.reason != reasonCatalogUnavailable {
		t.Fatalf("catalog reason = %q", state.reason)
	}
	timeoutTask := syntheticCoordinatorTask(501)
	// Leave enough time for the durable claim under -race; the injected Load
	// itself deterministically waits for the task deadline.
	coordinator.taskTimeout = time.Second
	coordinator.loadCatalog = func(ctx context.Context, _ CompileKey, _ int) (SampleSet, bool, error) {
		<-ctx.Done()
		return SampleSet{}, false, ctx.Err()
	}
	coordinator.process(timeoutTask)
	waitCoordinatorAttempt(t, harness.durable, timeoutTask.set.Key, attemptTransient, timeoutTask.set.Revision)
	timeoutState := readCoordinatorAttempt(t, harness.durable, timeoutTask.set.Key)
	if timeoutState.reason != reasonTaskTimeout || timeoutState.leaseID != "" {
		t.Fatalf("load timeout state = reason %q lease %q", timeoutState.reason, timeoutState.leaseID)
	}

	set, schema := readyCoordinatorSet(t, harness, 510, 3)
	coordinator.compileIR = func(ctx context.Context, _ []Sample, _ json.RawMessage, _ TruthExtractor) (IR, ValidationReport, error) {
		<-ctx.Done()
		return IR{}, ValidationReport{}, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	got := coordinator.runClaimed(ctx, coordinatorTask{set: set, schema: schema})
	if got.outcome != attemptTransient || got.reason != reasonTaskTimeout {
		t.Fatalf("timeout result = %#v", got)
	}

	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Close(); err != nil {
		t.Fatalf("idempotent Close() error = %v", err)
	}
	page := catalogPageKey(t, 999, uint64(1)<<63)
	if err := coordinator.Observe(context.Background(), page, catalogSnapshotID("closed"), harness.clock.Now(), catalogSchema()); !errors.Is(err, ErrCoordinatorClosed) {
		t.Fatalf("Observe(after Close) error = %v", err)
	}
	if err := (&Coordinator{}).Close(); !errors.Is(err, ErrInvalidCoordinator) {
		t.Fatalf("zero coordinator Close() error = %v", err)
	}
	if err := (*Coordinator)(nil).Close(); err != nil {
		t.Fatalf("nil Close() error = %v", err)
	}
}

func TestCoordinatorCloseCancelsRunningAndDropsQueued(t *testing.T) {
	harness := newCoordinatorHarness(t)
	coordinator := harness.coordinator
	started := make(chan struct{})
	var loadCalls atomic.Int32
	coordinator.loadCatalog = func(ctx context.Context, _ CompileKey, _ int) (SampleSet, bool, error) {
		if loadCalls.Add(1) == 1 {
			close(started)
		}
		<-ctx.Done()
		return SampleSet{}, false, ctx.Err()
	}
	running := syntheticCoordinatorTask(800)
	if err := coordinator.schedule(running); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := coordinator.schedule(syntheticCoordinatorTask(801)); err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() { closed <- coordinator.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close() did not cancel running work")
	}
	if got := loadCalls.Load(); got != 1 {
		t.Fatalf("load calls = %d, queued work was not dropped", got)
	}
	state := readCoordinatorAttempt(t, harness.durable, running.set.Key)
	if state.outcome != attemptTransient || state.reason != reasonTaskCanceled || state.leaseID != "" {
		t.Fatalf("Close durable state = outcome %q reason %q lease %q", state.outcome, state.reason, state.leaseID)
	}
	if err := coordinator.schedule(syntheticCoordinatorTask(802)); !errors.Is(err, ErrCoordinatorClosed) {
		t.Fatalf("schedule after Close error = %v", err)
	}
}

func TestCoordinatorObservationCancellationFinalizesDurableReason(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		harness := newCoordinatorHarness(t)
		set, schema := readyCoordinatorSet(t, harness, 1_200, MinCompileSamples)
		harness.coordinator.taskTimeout = time.Second
		harness.coordinator.hasObservation = func(ctx context.Context, _ snapshot.ID, _ func(snapshot.Meta) bool) (bool, error) {
			<-ctx.Done()
			return false, ctx.Err()
		}
		harness.coordinator.process(coordinatorTask{set: set, schema: schema})
		state := readCoordinatorAttempt(t, harness.durable, set.Key)
		if state.outcome != attemptTransient || state.reason != reasonTaskTimeout || state.leaseID != "" {
			t.Fatalf("observation timeout state = %#v", state)
		}
	})

	t.Run("close", func(t *testing.T) {
		harness := newCoordinatorHarness(t)
		set, schema := readyCoordinatorSet(t, harness, 1_300, MinCompileSamples)
		started := make(chan struct{})
		var once sync.Once
		harness.coordinator.hasObservation = func(ctx context.Context, _ snapshot.ID, _ func(snapshot.Meta) bool) (bool, error) {
			once.Do(func() { close(started) })
			<-ctx.Done()
			return false, ctx.Err()
		}
		if err := harness.coordinator.schedule(coordinatorTask{set: set, schema: schema}); err != nil {
			t.Fatal(err)
		}
		<-started
		if err := harness.coordinator.Close(); err != nil {
			t.Fatal(err)
		}
		state := readCoordinatorAttempt(t, harness.durable, set.Key)
		if state.outcome != attemptTransient || state.reason != reasonTaskCanceled || state.leaseID != "" {
			t.Fatalf("observation Close state = %#v", state)
		}
	})
}

func TestCoordinatorHydrationEnforcesAggregateInputBeforeMaterializingAllSamples(t *testing.T) {
	harness := newCoordinatorHarness(t)
	schema := readySetSchema(t, catalogSchema())
	const prefix = `<html><body><p>`
	const suffix = `</p></body></html>`
	html := prefix + strings.Repeat("h", MaxHTMLBytes-len(prefix)-len(suffix)) + suffix
	content := strings.Repeat("c", MaxSampleContentBytes)

	set := SampleSet{Samples: make([]SampleRef, 0, 6)}
	observations := make(map[snapshot.ID]snapshot.Meta, 6)
	for index := 0; index < 6; index++ {
		rawURL := fmt.Sprintf("https://shop.example.com/aggregate/%d", index)
		page, err := BuildPageKey(rawURL, schema, html)
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			set.Key = CompileKey{
				Host: page.Host, SchemaHash: page.SchemaHash, ContentProfile: DefaultCompilerProfile,
				TemplateClusterID: templateClusterID(page.TemplateSimHash), ClusterSimHash: page.TemplateSimHash,
			}
		} else if page.TemplateSimHash != set.Key.ClusterSimHash {
			t.Fatalf("fixture template hash %d = %#x, want %#x", index, page.TemplateSimHash, set.Key.ClusterSimHash)
		}
		id := snapshot.ID(catalogSnapshotID(fmt.Sprintf("aggregate-%d", index)))
		fetchedAt := catalogTestTime.Add(time.Duration(index) * time.Second)
		set.Samples = append(set.Samples, SampleRef{
			PageHash: page.PageHash, SampleSimHash: page.TemplateSimHash,
			SnapshotID: string(id), FetchedAt: fetchedAt,
		})
		observations[id] = snapshot.Meta{URL: rawURL, FetchedAt: fetchedAt, StatusCode: 200}
	}
	set.Count = len(set.Samples)
	set.Revision = sampleSetRevision(set)

	var reads, observationScans, cleans, compiles atomic.Int32
	harness.coordinator.readContent = func(snapshot.ID, int) ([]byte, error) {
		reads.Add(1)
		return []byte(html), nil
	}
	harness.coordinator.hasObservation = func(_ context.Context, id snapshot.ID, match func(snapshot.Meta) bool) (bool, error) {
		observationScans.Add(1)
		meta, ok := observations[id]
		return ok && match(meta), nil
	}
	harness.coordinator.cleanContent = func(string, string) (string, error) {
		cleans.Add(1)
		return content, nil
	}
	harness.coordinator.compileIR = func(context.Context, []Sample, json.RawMessage, TruthExtractor) (IR, ValidationReport, error) {
		compiles.Add(1)
		return IR{}, ValidationReport{}, errors.New("compile must not run")
	}

	result := harness.coordinator.runClaimed(context.Background(), coordinatorTask{set: set, schema: schema})
	if result.outcome != attemptNoCandidate || result.reason != reasonNoExtractorCandidate {
		t.Fatalf("aggregate hydration result = %#v", result)
	}
	if reads.Load() != 5 || observationScans.Load() != 4 || cleans.Load() != 4 || compiles.Load() != 0 {
		t.Fatalf("reads/observations/cleans/compiles = %d/%d/%d/%d, want 5/4/4/0",
			reads.Load(), observationScans.Load(), cleans.Load(), compiles.Load())
	}
}

func TestCoordinatorRejectsInvalidConfiguration(t *testing.T) {
	if _, err := NewCoordinator(nil, nil, nil, nil); !errors.Is(err, ErrInvalidCoordinator) {
		t.Fatalf("nil NewCoordinator() error = %v", err)
	}
	harness := newCoordinatorHarness(t)
	if _, err := NewCoordinator(harness.catalog, harness.registry, harness.snapshots,
		harness.coordinator.extractor, func() time.Time { return time.Time{} }); !errors.Is(err, ErrInvalidCoordinator) {
		t.Fatalf("invalid clock NewCoordinator() error = %v", err)
	}
}

func TestCoordinatorAttemptBoundsTTLAndClockRollback(t *testing.T) {
	harness := newCoordinatorHarness(t)
	now := harness.clock.Now()
	old := now.Add(-CoordinatorAttemptTTL - time.Hour)
	if err := harness.durable.Update(context.Background(), func(tx ledger.WriteTx) error {
		for index := 0; index < MaxCoordinatorAttemptRows; index++ {
			key := syntheticCoordinatorTask(10_000 + index).set.Key
			stamp := now
			if index == 0 {
				stamp = old
			}
			if _, err := tx.ExecContext(context.Background(), `INSERT INTO compiler_attempts (
				host, schema_hash, content_profile, template_cluster_id, cluster_simhash,
				created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?)`, key.Host, key.SchemaHash, key.ContentProfile,
				key.TemplateClusterID, encodeSimHash(key.ClusterSimHash),
				formatExtractorTime(stamp), formatExtractorTime(stamp)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed attempt bound: %v", err)
	}
	newKey := syntheticCoordinatorTask(99_999).set.Key
	token, claimed, err := harness.coordinator.claim(context.Background(), newKey, coordinatorDigest("bounded"))
	if err != nil || !claimed {
		t.Fatalf("claim after TTL/global cleanup = %v/%v", claimed, err)
	}
	if got := countCoordinatorAttempts(t, harness.durable); got != MaxCoordinatorAttemptRows {
		t.Fatalf("attempt rows = %d, want %d", got, MaxCoordinatorAttemptRows)
	}
	if err := harness.coordinator.release(context.Background(), newKey, coordinatorDigest("bounded"), token); err != nil {
		t.Fatal(err)
	}
	before := readCoordinatorAttempt(t, harness.durable, newKey).updatedAt
	harness.clock.Set(now.Add(-24 * time.Hour))
	token, claimed, err = harness.coordinator.claim(context.Background(), newKey, coordinatorDigest("rollback"))
	if err != nil || !claimed {
		t.Fatalf("rollback claim = %v/%v", claimed, err)
	}
	after := readCoordinatorAttempt(t, harness.durable, newKey).updatedAt
	if after.Before(before) {
		t.Fatalf("attempt clock moved backward: %v -> %v", before, after)
	}
	_ = harness.coordinator.release(context.Background(), newKey, coordinatorDigest("rollback"), token)
}

func newCoordinatorHarness(t *testing.T) *coordinatorHarness {
	t.Helper()
	dataDir := t.TempDir()
	clock := &coordinatorTestClock{now: catalogTestTime}
	durable, err := ledger.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := NewSampleCatalog(durable, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewStore(durable, WithStoreClock(clock.Now))
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := snapshot.NewStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	extractor := TruthExtractorFunc(func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"name":"Purify"}`), nil
	})
	coordinator, err := NewCoordinator(catalog, registry, snapshots, extractor, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	harness := &coordinatorHarness{coordinator, catalog, registry, durable, snapshots, clock}
	t.Cleanup(func() {
		_ = coordinator.Close()
		snapshots.Close()
		_ = durable.Close()
	})
	return harness
}

func putCoordinatorPage(t *testing.T, harness *coordinatorHarness, index int, schema json.RawMessage) (PageKey, snapshot.ID, time.Time) {
	t.Helper()
	rawURL := fmt.Sprintf("https://shop.example.com/coordinator/%d", index)
	html := fmt.Sprintf(`<html><body><main><h1 class="name">Item %d</h1><p>Details</p></main></body></html>`, index)
	page, err := BuildPageKey(rawURL, schema, html)
	if err != nil {
		t.Fatal(err)
	}
	fetched := catalogTestTime.Add(time.Duration(index) * time.Second)
	id, err := harness.snapshots.Put([]byte(html), snapshot.Meta{URL: rawURL, FetchedAt: fetched, StatusCode: 200, Engine: "test", ContentType: "text/html"})
	if err != nil {
		t.Fatal(err)
	}
	return page, id, fetched
}

func readyCoordinatorSet(t *testing.T, harness *coordinatorHarness, base, count int) (SampleSet, json.RawMessage) {
	t.Helper()
	schema := catalogSchema()
	var set SampleSet
	for index := 0; index < count; index++ {
		page, id, fetched := putCoordinatorPage(t, harness, base+index, schema)
		var err error
		set, _, err = harness.catalog.Observe(context.Background(), page, string(id), fetched)
		if err != nil {
			t.Fatal(err)
		}
	}
	return set, readySetSchema(t, schema)
}

func readySetSchema(t *testing.T, schema json.RawMessage) json.RawMessage {
	t.Helper()
	canonical, err := canonicalSchemaJSON(schema)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func syntheticCoordinatorTask(index int) coordinatorTask {
	fingerprint := uint64(1)<<63 | uint64(index+1)
	key := CompileKey{
		Host: "example.com", SchemaHash: coordinatorDigest(fmt.Sprintf("schema-%d", index)),
		ContentProfile: DefaultCompilerProfile, TemplateClusterID: templateClusterID(fingerprint),
		ClusterSimHash: fingerprint,
	}
	return coordinatorTask{set: SampleSet{Key: key, Revision: coordinatorDigest(fmt.Sprintf("revision-%d", index))}}
}

func coordinatorDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func waitCoordinatorAttempt(t *testing.T, durable *ledger.Store, key CompileKey, outcome attemptOutcome, revision string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		row, found := tryReadCoordinatorAttempt(t, durable, key)
		if found && row.outcome == outcome && row.attemptRevision == revision && row.leaseID == "" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	row, _ := tryReadCoordinatorAttempt(t, durable, key)
	t.Fatalf("attempt did not reach %s/%s: %#v", outcome, revision, row)
}

func tryReadCoordinatorAttempt(t *testing.T, durable *ledger.Store, key CompileKey) (attemptRow, bool) {
	t.Helper()
	var row attemptRow
	var found bool
	if err := durable.View(context.Background(), func(tx ledger.ReadTx) error {
		var err error
		row, found, err = loadAttempt(context.Background(), tx, key)
		return err
	}); err != nil {
		t.Fatalf("read attempt: %v", err)
	}
	return row, found
}

func readCoordinatorAttempt(t *testing.T, durable *ledger.Store, key CompileKey) attemptRow {
	t.Helper()
	row, found := tryReadCoordinatorAttempt(t, durable, key)
	if !found {
		t.Fatal("attempt row missing")
	}
	return row
}

func assertCoordinatorAttemptMissing(t *testing.T, durable *ledger.Store, key CompileKey) {
	t.Helper()
	if _, found := tryReadCoordinatorAttempt(t, durable, key); found {
		t.Fatal("unexpected attempt row")
	}
}

func assertAttemptCooldownPolicy(t *testing.T, harness *coordinatorHarness, coordinator *Coordinator, outcome attemptOutcome, reason attemptReason, index int) {
	t.Helper()
	key := syntheticCoordinatorTask(index).set.Key
	revision := coordinatorDigest(fmt.Sprintf("policy-%d-a", index))
	token, claimed, err := coordinator.claim(context.Background(), key, revision)
	if err != nil || !claimed {
		t.Fatalf("initial claim = %v/%v", claimed, err)
	}
	if err := coordinator.finalize(context.Background(), key, revision, token, attemptResult{outcome, reason}); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := coordinator.claim(context.Background(), key, revision); err != nil || claimed {
		t.Fatalf("same revision reran = %v/%v", claimed, err)
	}
	newRevision := coordinatorDigest(fmt.Sprintf("policy-%d-b", index))
	if _, claimed, err := coordinator.claim(context.Background(), key, newRevision); err != nil || claimed {
		t.Fatalf("new revision ignored cooldown = %v/%v", claimed, err)
	}
	harness.clock.Add(CoordinatorWeakDelay + time.Second)
	if _, claimed, err := coordinator.claim(context.Background(), key, newRevision); err != nil || !claimed {
		t.Fatalf("new revision after cooldown = %v/%v", claimed, err)
	}
}

func assertTransientRetryPolicy(t *testing.T, harness *coordinatorHarness, coordinator *Coordinator, index int) {
	t.Helper()
	key := syntheticCoordinatorTask(index).set.Key
	revision := coordinatorDigest("transient-policy")
	token, claimed, err := coordinator.claim(context.Background(), key, revision)
	if err != nil || !claimed {
		t.Fatalf("initial transient claim = %v/%v", claimed, err)
	}
	if err := coordinator.finalize(context.Background(), key, revision, token,
		attemptResult{attemptTransient, reasonCompileFailed}); err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := coordinator.claim(context.Background(), key, revision); err != nil || claimed {
		t.Fatalf("transient ignored cooldown = %v/%v", claimed, err)
	}
	harness.clock.Add(CoordinatorTransientDelay + time.Second)
	if _, claimed, err := coordinator.claim(context.Background(), key, revision); err != nil || !claimed {
		t.Fatalf("transient retry after cooldown = %v/%v", claimed, err)
	}
}

func assertHydrationResult(t *testing.T, coordinator *Coordinator, set SampleSet, schema json.RawMessage, outcome attemptOutcome, reason attemptReason) {
	t.Helper()
	_, _, err := coordinator.hydrate(context.Background(), set, schema)
	var failure hydrationFailure
	if !errors.As(err, &failure) || failure.result.outcome != outcome || failure.result.reason != reason {
		t.Fatalf("hydrate error = %#v, want %s/%s", err, outcome, reason)
	}
}

func countCoordinatorAttempts(t *testing.T, durable *ledger.Store) int {
	t.Helper()
	var count int
	if err := durable.View(context.Background(), func(tx ledger.ReadTx) error {
		return tx.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM compiler_attempts").Scan(&count)
	}); err != nil {
		t.Fatal(err)
	}
	return count
}
