package dockerengine

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/use-agent/purify/search/rerank"
)

type sessionFakeEngine struct {
	mu               sync.Mutex
	daemon           DaemonInspection
	image            ImageInspection
	create           CreateResult
	createErr        error
	createHook       func(CreateSpec)
	inspections      []ContainerInspection
	inspectErrs      []error
	inspectHook      func()
	ownershipErrs    []error
	ownershipHook    func()
	afterKill        ContainerInspection
	killed           bool
	killErr          error
	events           chan LifecycleEvent
	eventErrors      chan error
	expectedArchives int
	eventsHook       func(context.Context, string, int64) (<-chan LifecycleEvent, <-chan error)
	archiveAck       chan struct{}
	archiveArmed     chan struct{}
	calls            []string
	kills            int
	waits            int
	waitErr          error
	removes          int
	removeErr        error
	closes           int
	closeHook        func()
	startEvent       LifecycleEvent
	suppressStart    bool
}

func (engine *sessionFakeEngine) Close() error {
	engine.mu.Lock()
	engine.closes++
	hook := engine.closeHook
	engine.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}

func (engine *sessionFakeEngine) record(call string) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	engine.calls = append(engine.calls, call)
}

func (engine *sessionFakeEngine) InspectDaemon(context.Context) (DaemonInspection, error) {
	engine.record("daemon")
	return engine.daemon, nil
}

func (engine *sessionFakeEngine) InspectImage(_ context.Context, reference string) (ImageInspection, error) {
	engine.record("image:" + reference)
	return cloneImageInspection(engine.image), nil
}

func (engine *sessionFakeEngine) Create(_ context.Context, spec CreateSpec) (CreateResult, error) {
	engine.record("create:" + spec.ImageID)
	if engine.createHook != nil {
		engine.createHook(spec)
	}
	return engine.create, engine.createErr
}

func (engine *sessionFakeEngine) ResolveCreate(context.Context, CreateSpec) (OwnershipInspection, error) {
	engine.record("resolve-create")
	return OwnershipInspection{}, errors.New("unexpected resolve create")
}

func (engine *sessionFakeEngine) Start(_ context.Context, id string) error {
	engine.record("start:" + id)
	engine.mu.Lock()
	suppress, event, output := engine.suppressStart, engine.startEvent, engine.events
	engine.mu.Unlock()
	if !suppress && output != nil {
		output <- event
	}
	return nil
}

func (engine *sessionFakeEngine) Inspect(_ context.Context, id string) (ContainerInspection, error) {
	engine.record("inspect:" + id)
	if engine.inspectHook != nil {
		engine.inspectHook()
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.killed {
		return cloneContainerInspection(engine.afterKill), nil
	}
	if len(engine.inspectErrs) > 0 {
		err := engine.inspectErrs[0]
		engine.inspectErrs = engine.inspectErrs[1:]
		if err != nil {
			return ContainerInspection{}, err
		}
	}
	if len(engine.inspections) == 0 {
		return ContainerInspection{}, errors.New("unexpected inspect")
	}
	value := cloneContainerInspection(engine.inspections[0])
	engine.inspections = engine.inspections[1:]
	return value, nil
}

func (engine *sessionFakeEngine) InspectOwnership(_ context.Context, id string) (OwnershipInspection, error) {
	engine.record("ownership:" + id)
	if engine.ownershipHook != nil {
		engine.ownershipHook()
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if len(engine.ownershipErrs) > 0 {
		err := engine.ownershipErrs[0]
		engine.ownershipErrs = engine.ownershipErrs[1:]
		if err != nil {
			return OwnershipInspection{}, err
		}
	}
	var value ContainerInspection
	if engine.killed {
		value = cloneContainerInspection(engine.afterKill)
	} else if len(engine.inspections) > 0 {
		value = cloneContainerInspection(engine.inspections[0])
	} else {
		return OwnershipInspection{}, errors.New("unexpected ownership inspect")
	}
	return OwnershipInspection{ID: value.ID, Name: value.Name, Labels: ownershipLabels(value.Labels), State: value.State}, nil
}

func (engine *sessionFakeEngine) ReadReferenceArchive(context.Context, string, string) (io.ReadCloser, error) {
	engine.record("archive")
	return io.NopCloser(strings.NewReader("archive")), nil
}

func (engine *sessionFakeEngine) ExpectReferenceArchiveEvents(_ context.Context, _ string, _ map[string]string, count int) (<-chan struct{}, error) {
	engine.mu.Lock()
	engine.expectedArchives += count
	ack := engine.archiveAck
	armed := engine.archiveArmed
	engine.archiveArmed = nil
	engine.mu.Unlock()
	if armed != nil {
		close(armed)
	}
	if ack != nil {
		return ack, nil
	}
	done := make(chan struct{})
	close(done)
	return done, nil
}

func (engine *sessionFakeEngine) Events(ctx context.Context, id string, sinceUnixNano int64) (<-chan LifecycleEvent, <-chan error) {
	engine.record("events:" + id)
	if engine.eventsHook != nil {
		return engine.eventsHook(ctx, id, sinceUnixNano)
	}
	return engine.events, engine.eventErrors
}

func (engine *sessionFakeEngine) Kill(_ context.Context, id string) error {
	engine.record("kill:" + id)
	engine.mu.Lock()
	engine.killed = true
	engine.kills++
	err := engine.killErr
	engine.mu.Unlock()
	return err
}

func (engine *sessionFakeEngine) Wait(_ context.Context, id string) (WaitResult, error) {
	engine.record("wait:" + id)
	engine.mu.Lock()
	engine.waits++
	err := engine.waitErr
	engine.mu.Unlock()
	return WaitResult{ContainerID: id, ExitCode: 137}, err
}

func (engine *sessionFakeEngine) Remove(_ context.Context, id string) error {
	engine.record("remove:" + id)
	engine.mu.Lock()
	engine.removes++
	err := engine.removeErr
	engine.mu.Unlock()
	return err
}

func (engine *sessionFakeEngine) cleanupCounts() (int, int, int) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	return engine.kills, engine.waits, engine.removes
}

func (engine *sessionFakeEngine) closeCount() int {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	return engine.closes
}

type sessionFakeRecorder struct {
	mu      sync.Mutex
	closed  bool
	records int
	block   <-chan struct{}
}

func (recorder *sessionFakeRecorder) Record(ctx context.Context, request rerank.ScoreRequest) (rerank.ReferenceRecording, error) {
	recorder.mu.Lock()
	if recorder.closed {
		recorder.mu.Unlock()
		return rerank.ReferenceRecording{}, rerank.ErrRuntimeClosed
	}
	recorder.records++
	recorder.mu.Unlock()
	if recorder.block != nil {
		select {
		case <-recorder.block:
		case <-ctx.Done():
			return rerank.ReferenceRecording{}, ctx.Err()
		}
	}
	return rerank.ReferenceRecording{InputDigest: strings.Repeat("a", 64), CandidateDigest: strings.Repeat("b", 64), LatencyUS: 1}, nil
}

func (recorder *sessionFakeRecorder) Close() {
	recorder.mu.Lock()
	recorder.closed = true
	recorder.mu.Unlock()
}

func TestReferenceSessionStartsRecordsEmitsEvidenceAndCleansOwnedChild(t *testing.T) {
	dependencies, engine, recorder := validSessionDependenciesForTest(t)
	session, err := startReferenceSession(context.Background(), dependencies)
	if err != nil {
		t.Fatalf("startReferenceSession() error = %v", err)
	}
	if reflect.TypeOf(session).NumMethod() != 4 {
		t.Fatalf("ReferenceSession exported method surface = %d, want Record/Evidence/Done/Close", reflect.TypeOf(session).NumMethod())
	}
	recording, err := session.Record(context.Background(), validSessionScoreRequest(t))
	if err != nil || recording.InputDigest == "" || recording.CandidateDigest == "" {
		t.Fatalf("Record() = %#v, %v", recording, err)
	}
	evidence, err := session.Evidence(context.Background())
	if err != nil {
		t.Fatalf("Evidence() error = %v", err)
	}
	encoded, err := evidence.MarshalJSON()
	if err != nil || strings.Contains(string(encoded), testAPIKey) || !strings.Contains(string(encoded), `"recording_only":true`) {
		t.Fatalf("evidence = %s, %v", encoded, err)
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("Close(second) error = %v", err)
	}
	select {
	case <-session.Done():
	default:
		t.Fatal("Done was not closed before Close returned")
	}
	if _, err := session.Record(context.Background(), validSessionScoreRequest(t)); !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("Record(after close) = %v", err)
	}
	if _, err := session.Evidence(context.Background()); !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("Evidence(after close) = %v", err)
	}
	if kills, waits, removes := engine.cleanupCounts(); kills != 1 || waits != 1 || removes != 1 {
		t.Fatalf("cleanup counts = %d/%d/%d", kills, waits, removes)
	}
	if engine.closeCount() != 1 {
		t.Fatalf("engine close count = %d", engine.closeCount())
	}
	recorder.mu.Lock()
	closed, records := recorder.closed, recorder.records
	recorder.mu.Unlock()
	if !closed || records != 2 { // authenticated readiness plus caller Record
		t.Fatalf("recorder closed/records = %v/%d", closed, records)
	}
}

func TestReferenceSessionPersistsRecoveryBoundariesBeforeSideEffects(t *testing.T) {
	dependencies, engine, _ := validSessionDependenciesForTest(t)
	journal, root := newTestRecoveryJournal(t)
	dependencies.journal = journal
	var order []string
	dependencies.prepareHost = func(context.Context, HostPaths) error {
		assertSessionJournalState(t, journal, recoveryStatePrepared)
		order = append(order, "prepared", "prepare-host")
		return nil
	}
	engine.createHook = func(CreateSpec) {
		assertSessionJournalState(t, journal, recoveryStateCreating)
		order = append(order, "creating", "create")
	}
	firstInspect := true
	engine.inspectHook = func() {
		if firstInspect {
			firstInspect = false
			assertSessionJournalState(t, journal, recoveryStateOwned)
			order = append(order, "owned", "inspect")
		}
	}
	engine.closeHook = func() {
		records, err := journal.records()
		if err != nil || len(records) != 0 {
			t.Fatalf("records before engine close = %#v, %v", records, err)
		}
		order = append(order, "record-removed", "engine-close")
	}
	session, err := startReferenceSession(context.Background(), dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := prepareRecoveryJournal(root, os.Geteuid()); second != nil || !errors.Is(err, errRecoveryJournal) {
		t.Fatalf("lease during live session = %#v, %v", second, err)
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"prepared", "prepare-host", "creating", "create", "owned", "inspect", "record-removed", "engine-close"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	reopened, err := prepareRecoveryJournal(root, os.Geteuid())
	if err != nil {
		t.Fatalf("lease after Close = %v", err)
	}
	defer reopened.close()
	if records, err := reopened.records(); err != nil || len(records) != 0 {
		t.Fatalf("records after Close = %#v, %v", records, err)
	}
}

func TestReferenceSessionRecoveryRecordFailureDiscipline(t *testing.T) {
	t.Run("partial host preparation removes prepared record", func(t *testing.T) {
		dependencies, _, _ := validSessionDependenciesForTest(t)
		journal, root := newTestRecoveryJournal(t)
		dependencies.journal = journal
		dependencies.prepareHost = func(context.Context, HostPaths) error {
			assertSessionJournalState(t, journal, recoveryStatePrepared)
			return io.ErrUnexpectedEOF
		}
		hostCleanups := 0
		dependencies.cleanupHost = func() error { hostCleanups++; return nil }
		if session, err := startReferenceSession(context.Background(), dependencies); session != nil || !errors.Is(err, ErrSessionUnavailable) {
			t.Fatalf("startReferenceSession() = %#v, %v", session, err)
		}
		if hostCleanups != 1 {
			t.Fatalf("host cleanups = %d", hostCleanups)
		}
		assertReopenedSessionJournal(t, root, "")
	})

	t.Run("id-less create retains creating record and host", func(t *testing.T) {
		dependencies, engine, _ := validSessionDependenciesForTest(t)
		journal, root := newTestRecoveryJournal(t)
		dependencies.journal = journal
		engine.create = CreateResult{}
		engine.createErr = io.ErrUnexpectedEOF
		hostCleanups := 0
		dependencies.cleanupHost = func() error { hostCleanups++; return nil }
		if session, err := startReferenceSession(context.Background(), dependencies); session != nil || !errors.Is(err, ErrSessionUnavailable) {
			t.Fatalf("startReferenceSession() = %#v, %v", session, err)
		}
		if hostCleanups != 0 {
			t.Fatalf("host cleanups = %d", hostCleanups)
		}
		if kills, waits, removes := engine.cleanupCounts(); kills != 0 || waits != 0 || removes != 0 {
			t.Fatalf("container cleanup = %d/%d/%d", kills, waits, removes)
		}
		assertReopenedSessionJournal(t, root, recoveryStateCreating)
	})

	t.Run("partial id is owned before cleanup", func(t *testing.T) {
		dependencies, engine, _ := validSessionDependenciesForTest(t)
		journal, root := newTestRecoveryJournal(t)
		dependencies.journal = journal
		engine.createErr = io.ErrUnexpectedEOF
		engine.ownershipHook = func() { assertSessionJournalState(t, journal, recoveryStateOwned) }
		if session, err := startReferenceSession(context.Background(), dependencies); session != nil || !errors.Is(err, ErrSessionUnavailable) {
			t.Fatalf("startReferenceSession() = %#v, %v", session, err)
		}
		if _, _, removes := engine.cleanupCounts(); removes != 1 {
			t.Fatalf("remove count = %d", removes)
		}
		assertReopenedSessionJournal(t, root, "")
	})

	t.Run("owned transition failure preserves ambiguous child and blocks recovery", func(t *testing.T) {
		dependencies, engine, _ := validSessionDependenciesForTest(t)
		dependencies.paths = recoveryHostPaths(recoveryRecord{
			RunID: dependencies.runID, SnapshotDir: dependencies.paths.SnapshotDir,
		})
		journal, root := newTestRecoveryJournal(t)
		dependencies.journal = journal
		hostCleanups := 0
		dependencies.cleanupHost = func() error { hostCleanups++; return nil }
		journalPath := filepath.Join(root, testGenerated.RunID+recoveryJournalSuffix)
		engine.createHook = func(CreateSpec) {
			// Make the already durable creating record temporarily inadmissible so
			// markOwned fails before it can replace the record. The controller
			// must preserve the child and the ambiguous durable state.
			if err := os.Chmod(journalPath, 0o640); err != nil {
				t.Fatal(err)
			}
		}
		if session, err := startReferenceSession(context.Background(), dependencies); session != nil || !errors.Is(err, ErrSessionUnavailable) {
			t.Fatalf("startReferenceSession() = %#v, %v", session, err)
		}
		if kills, waits, removes := engine.cleanupCounts(); kills != 0 || waits != 0 || removes != 0 {
			t.Fatalf("destructive cleanup after markOwned failure = %d/%d/%d", kills, waits, removes)
		}
		if hostCleanups != 0 {
			t.Fatalf("host cleanups after markOwned failure = %d", hostCleanups)
		}
		if err := os.Chmod(journalPath, 0o600); err != nil {
			t.Fatal(err)
		}

		recoveryJournal, err := prepareRecoveryJournal(root, os.Geteuid())
		if err != nil {
			t.Fatalf("prepare recovery journal = %v", err)
		}
		defer recoveryJournal.close()
		records, err := recoveryJournal.records()
		if err != nil || len(records) != 1 || records[0].State != recoveryStateCreating {
			t.Fatalf("retained records = %#v, %v", records, err)
		}
		retained := records[0]
		recoveryEngine := &recoveryFakeEngine{}
		recoveryHostCleanups := 0
		recoveryDependencies := validRecoveryDependencies(recoveryJournal, recoveryEngine)
		recoveryDependencies.cleanupHost = func(HostPaths) error { recoveryHostCleanups++; return nil }
		if err := recoverReferenceSessions(context.Background(), recoveryDependencies); !errors.Is(err, ErrSessionUnavailable) {
			t.Fatalf("recoverReferenceSessions() = %v", err)
		}
		assertRecoveryRecord(t, recoveryJournal, retained)
		if recoveryHostCleanups != 0 || recoveryEngine.resolveCalls != 0 || recoveryEngine.removeCalls != 0 ||
			recoveryEngine.killCalls != 0 || recoveryEngine.waitCalls != 0 || recoveryEngine.createCalls != 0 || recoveryEngine.startCalls != 0 {
			t.Fatalf("recovery effects = host:%d engine:%#v", recoveryHostCleanups, recoveryEngine)
		}
	})

	t.Run("foreign ownership retains owned record but releases lease", func(t *testing.T) {
		dependencies, engine, _ := validSessionDependenciesForTest(t)
		journal, root := newTestRecoveryJournal(t)
		dependencies.journal = journal
		engine.inspections[0].Labels[LabelRunID] = strings.Repeat("c", 32)
		hostCleanups := 0
		dependencies.cleanupHost = func() error { hostCleanups++; return nil }
		if session, err := startReferenceSession(context.Background(), dependencies); session != nil || !errors.Is(err, ErrSessionUnavailable) {
			t.Fatalf("startReferenceSession() = %#v, %v", session, err)
		}
		if hostCleanups != 0 {
			t.Fatalf("host cleanups = %d", hostCleanups)
		}
		assertReopenedSessionJournal(t, root, recoveryStateOwned)
	})
}

func TestReferenceSessionRecoveryLeaseWaitsForRetryableHostCleanup(t *testing.T) {
	dependencies, _, _ := validSessionDependenciesForTest(t)
	journal, root := newTestRecoveryJournal(t)
	dependencies.journal = journal
	hostCleanups := 0
	dependencies.cleanupHost = func() error {
		hostCleanups++
		if hostCleanups == 1 {
			return io.ErrUnexpectedEOF
		}
		return nil
	}
	session, err := startReferenceSession(context.Background(), dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Close(context.Background()); !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("Close(first) = %v", err)
	}
	if second, err := prepareRecoveryJournal(root, os.Geteuid()); second != nil || !errors.Is(err, errRecoveryJournal) {
		t.Fatalf("lease released before retry = %#v, %v", second, err)
	}
	assertSessionJournalState(t, journal, recoveryStateOwned)
	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("Close(second) = %v", err)
	}
	assertReopenedSessionJournal(t, root, "")
}

func TestReferenceSessionLifecycleDriftRevokesInflightRecordBeforeCleanup(t *testing.T) {
	dependencies, engine, _ := validSessionDependenciesForTest(t)
	release := make(chan struct{})
	blocking := &sessionFakeRecorder{block: release}
	dependencies.newRecorder = func(string, string) (sessionRecorder, error) { return blocking, nil }
	dependencies.probeAuthenticated = func(context.Context, sessionRecorder) error { return nil }
	session, err := startReferenceSession(context.Background(), dependencies)
	if err != nil {
		t.Fatal(err)
	}
	recordDone := make(chan error, 1)
	go func() {
		_, recordErr := session.Record(context.Background(), validSessionScoreRequest(t))
		recordDone <- recordErr
	}()
	deadline := time.Now().Add(time.Second)
	for {
		blocking.mu.Lock()
		records := blocking.records
		blocking.mu.Unlock()
		if records == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Record did not enter recorder")
		}
		time.Sleep(time.Millisecond)
	}
	engine.events <- LifecycleEvent{ContainerID: strings.Repeat("b", 64), Action: "restart", Labels: cloneMap(mustPlan(t).create.Labels)}
	select {
	case err := <-recordDone:
		if !errors.Is(err, ErrSessionUnavailable) {
			t.Fatalf("in-flight Record() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("revocation did not cancel in-flight Record")
	}
	select {
	case <-session.Done():
	case <-time.After(time.Second):
		t.Fatal("lifecycle drift did not revoke session")
	}
	close(release)
}

func TestReferenceSessionOwnedExitEventRevokesAuthority(t *testing.T) {
	for _, action := range []string{LifecycleActionKill, LifecycleActionDie} {
		t.Run(action, func(t *testing.T) {
			dependencies, engine, _ := validSessionDependenciesForTest(t)
			session, err := startReferenceSession(context.Background(), dependencies)
			if err != nil {
				t.Fatal(err)
			}
			engine.events <- LifecycleEvent{ContainerID: strings.Repeat("b", 64), Action: action, Labels: ownershipLabels(mustPlan(t).create.Labels)}
			select {
			case <-session.Done():
			case <-time.After(time.Second):
				t.Fatal("owned exit did not revoke authority")
			}
			if _, err := session.Evidence(context.Background()); !errors.Is(err, ErrSessionUnavailable) {
				t.Fatalf("Evidence(after %s) = %v", action, err)
			}
		})
	}
}

func TestReferenceSessionFailureCleansOnlyOwnedContainerAndRedactsDetails(t *testing.T) {
	dependencies, engine, _ := validSessionDependenciesForTest(t)
	dependencies.probeUnauthorized = func(context.Context, string) error {
		return errors.New("private " + testAPIKey + " " + testPaths.RunDir)
	}
	session, err := startReferenceSession(context.Background(), dependencies)
	if session != nil || !errors.Is(err, ErrSessionUnavailable) || strings.Contains(err.Error(), testAPIKey) || strings.Contains(err.Error(), testPaths.RunDir) {
		t.Fatalf("startReferenceSession() = %#v, %v", session, err)
	}
	if kills, waits, removes := engine.cleanupCounts(); kills != 1 || waits != 1 || removes != 1 {
		t.Fatalf("cleanup counts = %d/%d/%d", kills, waits, removes)
	}

	t.Run("ownership loss is never destructive", func(t *testing.T) {
		dependencies, engine, _ := validSessionDependenciesForTest(t)
		engine.inspections[0].Labels[LabelRunID] = strings.Repeat("c", 32)
		got, gotErr := startReferenceSession(context.Background(), dependencies)
		if got != nil || !errors.Is(gotErr, ErrSessionUnavailable) {
			t.Fatalf("startReferenceSession() = %#v, %v", got, gotErr)
		}
		if kills, waits, removes := engine.cleanupCounts(); kills != 0 || waits != 0 || removes != 0 {
			t.Fatalf("foreign cleanup counts = %d/%d/%d", kills, waits, removes)
		}
		if engine.closeCount() != 1 {
			t.Fatalf("foreign engine close count = %d", engine.closeCount())
		}
	})
}

func TestReferenceSessionRejectsCanceledAndIncompleteDependenciesBeforeEngine(t *testing.T) {
	dependencies, engine, _ := validSessionDependenciesForTest(t)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if session, err := startReferenceSession(canceled, dependencies); session != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("startReferenceSession(canceled) = %#v, %v", session, err)
	}
	engine.mu.Lock()
	calls := len(engine.calls)
	engine.mu.Unlock()
	if calls != 0 {
		t.Fatalf("engine calls = %d, want 0", calls)
	}
	if engine.closeCount() != 1 {
		t.Fatalf("canceled engine close count = %d", engine.closeCount())
	}
	incomplete, incompleteEngine, _ := validSessionDependenciesForTest(t)
	incomplete.newRecorder = nil
	if session, err := startReferenceSession(context.Background(), incomplete); session != nil || !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("startReferenceSession(incomplete) = %#v, %v", session, err)
	}
	if incompleteEngine.closeCount() != 1 {
		t.Fatalf("incomplete engine close count = %d", incompleteEngine.closeCount())
	}
}

func TestReferenceSessionDrainsFatalEventBeforePublishing(t *testing.T) {
	dependencies, engine, _ := validSessionDependenciesForTest(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	dependencies.probeAuthenticated = func(context.Context, sessionRecorder) error {
		close(entered)
		<-release
		return nil
	}
	result := make(chan error, 1)
	go func() {
		session, err := startReferenceSession(context.Background(), dependencies)
		if session != nil {
			result <- errors.New("fatal event published a session")
			return
		}
		result <- err
	}()
	<-entered
	engine.events <- LifecycleEvent{
		ContainerID: strings.Repeat("b", 64), Action: "restart", Labels: cloneMap(mustPlan(t).create.Labels),
	}
	deadline := time.Now().Add(time.Second)
	for len(engine.events) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("event guard did not consume fatal event")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	if err := <-result; !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("startReferenceSession() error = %v", err)
	}
	if kills, waits, removes := engine.cleanupCounts(); kills != 1 || waits != 1 || removes != 1 {
		t.Fatalf("cleanup counts = %d/%d/%d", kills, waits, removes)
	}
}

func TestReferenceSessionCanceledCloseStillRevokesAndCleans(t *testing.T) {
	dependencies, engine, _ := validSessionDependenciesForTest(t)
	session, err := startReferenceSession(context.Background(), dependencies)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := session.Close(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close(canceled) = %v", err)
	}
	if kills, waits, removes := engine.cleanupCounts(); kills != 1 || waits != 1 || removes != 1 {
		t.Fatalf("cleanup counts = %d/%d/%d", kills, waits, removes)
	}
	if engine.closeCount() != 1 {
		t.Fatalf("engine close count = %d", engine.closeCount())
	}
}

func TestReferenceSessionCreateWarningStillRemovesInspectableOwnedChild(t *testing.T) {
	dependencies, engine, _ := validSessionDependenciesForTest(t)
	engine.create.Warnings = []string{"daemon warning with " + testAPIKey}
	hostCleanups := 0
	dependencies.cleanupHost = func() error { hostCleanups++; return nil }
	session, err := startReferenceSession(context.Background(), dependencies)
	if session != nil || !errors.Is(err, ErrSessionUnavailable) || strings.Contains(err.Error(), testAPIKey) {
		t.Fatalf("startReferenceSession() = %#v, %v", session, err)
	}
	if kills, waits, removes := engine.cleanupCounts(); kills != 0 || waits != 0 || removes != 1 {
		t.Fatalf("cleanup counts = %d/%d/%d", kills, waits, removes)
	}
	if hostCleanups != 1 || engine.closeCount() != 1 {
		t.Fatalf("host/engine cleanup = %d/%d", hostCleanups, engine.closeCount())
	}
}

func TestReferenceSessionCleanupUsesMinimalOwnershipDespiteStaticDrift(t *testing.T) {
	dependencies, engine, _ := validSessionDependenciesForTest(t)
	hostCleanups := 0
	dependencies.cleanupHost = func() error { hostCleanups++; return nil }
	session, err := startReferenceSession(context.Background(), dependencies)
	if err != nil {
		t.Fatal(err)
	}
	engine.mu.Lock()
	engine.afterKill.RestartCount = 1
	engine.mu.Unlock()
	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if kills, waits, removes := engine.cleanupCounts(); kills != 1 || waits != 1 || removes != 1 {
		t.Fatalf("cleanup counts = %d/%d/%d", kills, waits, removes)
	}
	if hostCleanups != 1 || engine.closeCount() != 1 {
		t.Fatalf("host/engine cleanup = %d/%d", hostCleanups, engine.closeCount())
	}
}

func TestReferenceSessionCleanupRejectsRenamedContainerBeforeMutation(t *testing.T) {
	dependencies, engine, _ := validSessionDependenciesForTest(t)
	hostCleanups := 0
	dependencies.cleanupHost = func() error { hostCleanups++; return nil }
	session, err := startReferenceSession(context.Background(), dependencies)
	if err != nil {
		t.Fatal(err)
	}
	engine.mu.Lock()
	for index := range engine.inspections {
		engine.inspections[index].Name += "-renamed"
	}
	engine.mu.Unlock()
	if err := session.Close(context.Background()); !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("Close(renamed) = %v", err)
	}
	if kills, waits, removes := engine.cleanupCounts(); kills != 0 || waits != 0 || removes != 0 {
		t.Fatalf("renamed cleanup = %d/%d/%d", kills, waits, removes)
	}
	if hostCleanups != 0 {
		t.Fatalf("renamed host cleanup = %d", hostCleanups)
	}
}

func TestReferenceSessionKillErrorThenForeignOwnershipNeverRemoves(t *testing.T) {
	dependencies, engine, _ := validSessionDependenciesForTest(t)
	hostCleanups := 0
	dependencies.cleanupHost = func() error { hostCleanups++; return nil }
	session, err := startReferenceSession(context.Background(), dependencies)
	if err != nil {
		t.Fatal(err)
	}
	engine.mu.Lock()
	engine.killErr = errors.New("kill response lost")
	engine.afterKill.Labels[LabelRunID] = strings.Repeat("c", 32)
	engine.mu.Unlock()
	if err := session.Close(context.Background()); !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("Close() = %v", err)
	}
	if err := session.Close(context.Background()); !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("Close(second) = %v", err)
	}
	if kills, waits, removes := engine.cleanupCounts(); kills != 1 || waits != 0 || removes != 0 {
		t.Fatalf("foreign cleanup = %d/%d/%d", kills, waits, removes)
	}
	if hostCleanups != 0 || engine.closeCount() != 1 {
		t.Fatalf("foreign host/engine cleanup = %d/%d", hostCleanups, engine.closeCount())
	}
}

func TestReferenceSessionCleansInspectablePartialCreateResult(t *testing.T) {
	dependencies, engine, _ := validSessionDependenciesForTest(t)
	engine.createErr = io.ErrUnexpectedEOF
	session, err := startReferenceSession(context.Background(), dependencies)
	if session != nil || !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("startReferenceSession(partial create) = %#v, %v", session, err)
	}
	if kills, waits, removes := engine.cleanupCounts(); kills != 0 || waits != 0 || removes != 1 {
		t.Fatalf("partial create cleanup = %d/%d/%d", kills, waits, removes)
	}
}

func TestReferenceSessionCleanupRetriesTransientInspectFailure(t *testing.T) {
	dependencies, engine, _ := validSessionDependenciesForTest(t)
	session, err := startReferenceSession(context.Background(), dependencies)
	if err != nil {
		t.Fatal(err)
	}
	engine.mu.Lock()
	engine.ownershipErrs = []error{errors.New("temporary daemon failure")}
	engine.mu.Unlock()
	if err := session.Close(context.Background()); !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("Close(transient) = %v", err)
	}
	if kills, waits, removes := engine.cleanupCounts(); kills != 0 || waits != 0 || removes != 0 {
		t.Fatalf("transient cleanup = %d/%d/%d", kills, waits, removes)
	}
	if engine.closeCount() != 0 {
		t.Fatalf("engine closed before retry = %d", engine.closeCount())
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("Close(retry) = %v", err)
	}
	if kills, waits, removes := engine.cleanupCounts(); kills != 1 || waits != 1 || removes != 1 {
		t.Fatalf("retry cleanup = %d/%d/%d", kills, waits, removes)
	}
}

func TestReferenceSessionCleanupTreatsConfirmedContainerAbsenceAsSettled(t *testing.T) {
	dependencies, engine, _ := validSessionDependenciesForTest(t)
	session, err := startReferenceSession(context.Background(), dependencies)
	if err != nil {
		t.Fatal(err)
	}
	engine.mu.Lock()
	engine.ownershipErrs = []error{errContainerNotFound}
	engine.mu.Unlock()
	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("Close(not found) = %v", err)
	}
	if kills, waits, removes := engine.cleanupCounts(); kills != 0 || waits != 0 || removes != 0 {
		t.Fatalf("not-found cleanup = %d/%d/%d", kills, waits, removes)
	}
	if engine.closeCount() != 1 {
		t.Fatalf("engine close count = %d", engine.closeCount())
	}
}

func TestReferenceSessionCleanupReconcilesLostWaitAndRemoveResponses(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*sessionFakeEngine)
	}{
		{name: "wait response", configure: func(engine *sessionFakeEngine) { engine.waitErr = io.ErrUnexpectedEOF }},
		{name: "remove response", configure: func(engine *sessionFakeEngine) { engine.removeErr = errContainerNotFound }},
	} {
		t.Run(test.name, func(t *testing.T) {
			dependencies, engine, _ := validSessionDependenciesForTest(t)
			session, err := startReferenceSession(context.Background(), dependencies)
			if err != nil {
				t.Fatal(err)
			}
			engine.mu.Lock()
			test.configure(engine)
			engine.mu.Unlock()
			if err := session.Close(context.Background()); err != nil {
				t.Fatalf("Close() = %v", err)
			}
			if _, _, removes := engine.cleanupCounts(); removes != 1 {
				t.Fatalf("remove count = %d", removes)
			}
		})
	}
}

func TestReferenceSessionStartupFailureRetriesTransientCleanup(t *testing.T) {
	dependencies, engine, _ := validSessionDependenciesForTest(t)
	dependencies.probeUnauthorized = func(context.Context, string) error { return ErrSessionUnavailable }
	// Three startup inspections are consumed before the failing probe. The first
	// cleanup inspection then fails transiently and the bounded retry succeeds.
	engine.mu.Lock()
	engine.ownershipErrs = []error{errors.New("temporary daemon failure")}
	engine.mu.Unlock()
	session, err := startReferenceSession(context.Background(), dependencies)
	if session != nil || !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("startReferenceSession() = %#v, %v", session, err)
	}
	if kills, waits, removes := engine.cleanupCounts(); kills != 1 || waits != 1 || removes != 1 {
		t.Fatalf("startup retry cleanup = %d/%d/%d", kills, waits, removes)
	}
	if engine.closeCount() != 1 {
		t.Fatalf("startup retry engine close = %d", engine.closeCount())
	}
}

func TestReferenceSessionEventSubscriptionHonorsStartupCancellation(t *testing.T) {
	dependencies, engine, _ := validSessionDependenciesForTest(t)
	created := cloneContainerInspection(engine.inspections[0])
	engine.inspections = []ContainerInspection{created, created}
	engine.eventsHook = func(ctx context.Context, _ string, _ int64) (<-chan LifecycleEvent, <-chan error) {
		<-ctx.Done()
		return make(chan LifecycleEvent), make(chan error)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	session, err := startReferenceSession(ctx, dependencies)
	if session != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("startReferenceSession(events deadline) = %#v, %v", session, err)
	}
	if _, _, removes := engine.cleanupCounts(); removes != 1 {
		t.Fatalf("startup cancellation remove count = %d", removes)
	}
}

func TestReferenceSessionRequiresExactStartEventAfterSubscriptionBarrier(t *testing.T) {
	dependencies, engine, _ := validSessionDependenciesForTest(t)
	engine.suppressStart = true
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	session, err := startReferenceSession(ctx, dependencies)
	if session != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("startReferenceSession(missing start event) = %#v, %v", session, err)
	}
	if kills, waits, removes := engine.cleanupCounts(); kills != 1 || waits != 1 || removes != 1 {
		t.Fatalf("missing start cleanup = %d/%d/%d", kills, waits, removes)
	}
}

func TestReferenceSessionFinalCancellationNeverPublishes(t *testing.T) {
	dependencies, engine, _ := validSessionDependenciesForTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	inspections := 0
	runningSocket := validRunningInspection(mustPlan(t), ownership{
		containerID: strings.Repeat("b", 64), labels: ownershipLabels(mustPlan(t).create.Labels),
	}).Socket
	dependencies.inspectSocket = func(context.Context, string) (SocketInspection, error) {
		inspections++
		if inspections == 2 {
			cancel()
		}
		return runningSocket, nil
	}
	session, err := startReferenceSession(ctx, dependencies)
	if session != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("startReferenceSession(final cancel) = %#v, %v", session, err)
	}
	if kills, waits, removes := engine.cleanupCounts(); kills != 1 || waits != 1 || removes != 1 {
		t.Fatalf("final cancel cleanup = %d/%d/%d", kills, waits, removes)
	}
}

func TestReferenceSessionArchiveAcknowledgementIsRequiredAndGuardFailureUnblocksStartup(t *testing.T) {
	t.Run("publication waits for exact acknowledgement", func(t *testing.T) {
		dependencies, engine, _ := validSessionDependenciesForTest(t)
		ack := make(chan struct{})
		armed := make(chan struct{})
		engine.archiveAck = ack
		engine.archiveArmed = armed
		type result struct {
			session *ReferenceSession
			err     error
		}
		resultChannel := make(chan result, 1)
		go func() {
			session, err := startReferenceSession(context.Background(), dependencies)
			resultChannel <- result{session: session, err: err}
		}()
		select {
		case <-armed:
		case <-time.After(time.Second):
			t.Fatal("archive expectation was not armed")
		}
		engine.mu.Lock()
		callsBeforeAck := append([]string(nil), engine.calls...)
		engine.mu.Unlock()
		for _, call := range callsBeforeAck {
			if strings.HasPrefix(call, "start:") {
				t.Fatalf("Start called before archive subscription acknowledgement: %v", callsBeforeAck)
			}
		}
		select {
		case got := <-resultChannel:
			t.Fatalf("session published before archive acknowledgement: %#v, %v", got.session, got.err)
		case <-time.After(20 * time.Millisecond):
		}
		close(ack)
		got := <-resultChannel
		if got.err != nil || got.session == nil {
			t.Fatalf("startReferenceSession() = %#v, %v", got.session, got.err)
		}
		if err := got.session.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("fatal guard state cancels acknowledgement wait", func(t *testing.T) {
		dependencies, engine, _ := validSessionDependenciesForTest(t)
		ack := make(chan struct{})
		armed := make(chan struct{})
		engine.archiveAck = ack
		engine.archiveArmed = armed
		resultChannel := make(chan error, 1)
		go func() {
			session, err := startReferenceSession(context.Background(), dependencies)
			if session != nil {
				resultChannel <- errors.New("fatal event published a session")
				return
			}
			resultChannel <- err
		}()
		select {
		case <-armed:
		case <-time.After(time.Second):
			t.Fatal("archive expectation was not armed")
		}
		engine.events <- LifecycleEvent{
			ContainerID: strings.Repeat("b", 64), Action: "restart", Labels: cloneMap(mustPlan(t).create.Labels),
		}
		select {
		case err := <-resultChannel:
			if !errors.Is(err, ErrSessionUnavailable) {
				t.Fatalf("startReferenceSession() error = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("fatal event left startup waiting for archive acknowledgement")
		}
	})
}

func TestReferenceSessionInternalRevocationSettlesTransientCleanupWithoutCallerRetry(t *testing.T) {
	dependencies, engine, _ := validSessionDependenciesForTest(t)
	session, err := startReferenceSession(context.Background(), dependencies)
	if err != nil {
		t.Fatal(err)
	}
	engine.mu.Lock()
	engine.ownershipErrs = []error{errors.New("temporary daemon failure")}
	engine.mu.Unlock()
	cleanupContext, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := session.revokeUntilSettled(cleanupContext); err != nil {
		t.Fatalf("revokeUntilSettled() = %v", err)
	}
	if kills, waits, removes := engine.cleanupCounts(); kills != 1 || waits != 1 || removes != 1 {
		t.Fatalf("settled cleanup = %d/%d/%d", kills, waits, removes)
	}
	if engine.closeCount() != 1 {
		t.Fatalf("engine close count = %d", engine.closeCount())
	}
}

func TestReferenceSessionArchiveEventIsBenignButForeignEventDisablesCleanup(t *testing.T) {
	t.Run("archive", func(t *testing.T) {
		dependencies, engine, _ := validSessionDependenciesForTest(t)
		session, err := startReferenceSession(context.Background(), dependencies)
		if err != nil {
			t.Fatal(err)
		}
		engine.events <- LifecycleEvent{ContainerID: strings.Repeat("b", 64), Action: LifecycleActionArchive, Labels: ownershipLabels(mustPlan(t).create.Labels)}
		time.Sleep(5 * time.Millisecond)
		if _, err := session.Evidence(context.Background()); !errors.Is(err, ErrSessionUnavailable) {
			t.Fatalf("unregistered archive event remained live: %v", err)
		}
	})

	t.Run("foreign queued before close", func(t *testing.T) {
		dependencies, engine, _ := validSessionDependenciesForTest(t)
		session, err := startReferenceSession(context.Background(), dependencies)
		if err != nil {
			t.Fatal(err)
		}
		engine.events <- LifecycleEvent{ContainerID: strings.Repeat("b", 64), Action: "update", Labels: map[string]string{}}
		if err := session.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if kills, waits, removes := engine.cleanupCounts(); kills != 0 || waits != 0 || removes != 0 {
			t.Fatalf("foreign event cleanup = %d/%d/%d", kills, waits, removes)
		}
	})
}

type stubbornSessionRecorder struct {
	entered chan struct{}
	release chan struct{}
}

func (recorder *stubbornSessionRecorder) Record(context.Context, rerank.ScoreRequest) (rerank.ReferenceRecording, error) {
	close(recorder.entered)
	<-recorder.release
	return rerank.ReferenceRecording{InputDigest: strings.Repeat("a", 64), CandidateDigest: strings.Repeat("b", 64), LatencyUS: 1}, nil
}

func (*stubbornSessionRecorder) Close() {}

func TestReferenceSessionDiscardsRecorderDataReturnedAfterRevocation(t *testing.T) {
	dependencies, _, _ := validSessionDependenciesForTest(t)
	recorder := &stubbornSessionRecorder{entered: make(chan struct{}), release: make(chan struct{})}
	dependencies.newRecorder = func(string, string) (sessionRecorder, error) { return recorder, nil }
	dependencies.probeAuthenticated = func(context.Context, sessionRecorder) error { return nil }
	session, err := startReferenceSession(context.Background(), dependencies)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, recordErr := session.Record(context.Background(), validSessionScoreRequest(t))
		result <- recordErr
	}()
	<-recorder.entered
	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(recorder.release)
	if err := <-result; !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("late Record() = %v", err)
	}
}

func TestReferenceSessionFreshValidationRejectsSilentDrift(t *testing.T) {
	t.Run("pre-record drift calls no backend", func(t *testing.T) {
		dependencies, engine, recorder := validSessionDependenciesForTest(t)
		session, err := startReferenceSession(context.Background(), dependencies)
		if err != nil {
			t.Fatal(err)
		}
		engine.mu.Lock()
		engine.inspections[0].RestartCount = 1
		engine.mu.Unlock()
		if _, err := session.Record(context.Background(), validSessionScoreRequest(t)); !errors.Is(err, ErrSessionUnavailable) {
			t.Fatalf("Record(pre drift) = %v", err)
		}
		recorder.mu.Lock()
		records := recorder.records
		recorder.mu.Unlock()
		if records != 1 { // authenticated readiness only
			t.Fatalf("backend records = %d, want readiness only", records)
		}
		select {
		case <-session.Done():
		case <-time.After(time.Second):
			t.Fatal("silent drift did not revoke authority")
		}
	})

	t.Run("post-record PID drift discards data", func(t *testing.T) {
		dependencies, engine, _ := validSessionDependenciesForTest(t)
		release := make(chan struct{})
		recorder := &sessionFakeRecorder{block: release}
		dependencies.newRecorder = func(string, string) (sessionRecorder, error) { return recorder, nil }
		dependencies.probeAuthenticated = func(context.Context, sessionRecorder) error { return nil }
		session, err := startReferenceSession(context.Background(), dependencies)
		if err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			_, recordErr := session.Record(context.Background(), validSessionScoreRequest(t))
			result <- recordErr
		}()
		deadline := time.Now().Add(time.Second)
		for {
			recorder.mu.Lock()
			records := recorder.records
			recorder.mu.Unlock()
			if records == 1 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("Record did not reach backend")
			}
			time.Sleep(time.Millisecond)
		}
		engine.mu.Lock()
		engine.inspections[0].State.PID++
		engine.inspections[1].State.PID++
		engine.mu.Unlock()
		close(release)
		if err := <-result; !errors.Is(err, ErrSessionUnavailable) {
			t.Fatalf("Record(post PID drift) = %v", err)
		}
	})

	t.Run("evidence is fresh and caller cancellation is inert", func(t *testing.T) {
		dependencies, engine, _ := validSessionDependenciesForTest(t)
		session, err := startReferenceSession(context.Background(), dependencies)
		if err != nil {
			t.Fatal(err)
		}
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := session.Evidence(canceled); !errors.Is(err, context.Canceled) {
			t.Fatalf("Evidence(canceled) = %v", err)
		}
		select {
		case <-session.Done():
			t.Fatal("caller cancellation revoked a healthy session")
		default:
		}
		engine.mu.Lock()
		engine.inspections[0].RestartCount = 1
		engine.mu.Unlock()
		if _, err := session.Evidence(context.Background()); !errors.Is(err, ErrSessionUnavailable) {
			t.Fatalf("Evidence(silent drift) = %v", err)
		}
		select {
		case <-session.Done():
		case <-time.After(time.Second):
			t.Fatal("stale Evidence did not revoke authority")
		}
	})
}

func validSessionDependenciesForTest(t *testing.T) (sessionDependencies, *sessionFakeEngine, *sessionFakeRecorder) {
	t.Helper()
	plan := mustPlan(t)
	owner, err := plan.establishOwnership(CreateResult{ContainerID: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	created := validCreatedInspection(plan, owner)
	running := validRunningInspection(plan, owner)
	exited := cloneContainerInspection(running.After)
	exited.State = ContainerState{Status: ContainerStatusExited, ExitCode: 137}
	events := make(chan LifecycleEvent, 4)
	inspections := []ContainerInspection{created}
	// Startup consumes two running brackets. Each Record/Evidence call performs
	// its own fresh bracket, so keep a detached deterministic stream available.
	for index := 0; index < 32; index++ {
		inspections = append(inspections, cloneContainerInspection(running.Before), cloneContainerInspection(running.After))
	}
	engine := &sessionFakeEngine{
		daemon: DaemonInspection{APIVersion: "1.51", OSType: "linux", Architecture: "amd64", Rootful: true, UserNamespaceMode: UserNamespaceDisabled},
		image:  validImageInspection(), create: CreateResult{ContainerID: owner.containerID},
		inspections: inspections,
		afterKill:   exited,
		events:      events, eventErrors: make(chan error, 1),
		startEvent: LifecycleEvent{ContainerID: owner.containerID, Action: LifecycleActionStart, Labels: cloneMap(owner.labels)},
	}
	recorder := &sessionFakeRecorder{}
	dependencies := sessionDependencies{
		engine:     engine,
		controller: ControllerInspection{Endpoint: DockerSocketPath, GOOS: "linux", GOARCH: "amd64", EffectiveUIDKnown: true, EffectiveUID: 0},
		paths:      testPaths, runID: testGenerated.RunID, apiKey: testGenerated.APIKey,
		prepareHost:              func(context.Context, HostPaths) error { return nil },
		verifyHostArtifacts:      func(context.Context) error { return nil },
		verifyContainerArtifacts: func(context.Context, Engine, string) error { return nil },
		readProcess: func(_ context.Context, pid int) (ProcessInspection, error) {
			return ProcessInspection{PID: pid, Environment: append([]string(nil), running.Process.Environment...)}, nil
		},
		inspectSocket:     func(context.Context, string) (SocketInspection, error) { return running.Socket, nil },
		probeUnauthorized: func(context.Context, string) error { return nil },
		newRecorder:       func(string, string) (sessionRecorder, error) { return recorder, nil },
		probeAuthenticated: func(ctx context.Context, value sessionRecorder) error {
			_, err := value.Record(ctx, validSessionScoreRequest(t))
			return err
		},
		cleanupHost: func() error { return nil },
	}
	return dependencies, engine, recorder
}

func assertSessionJournalState(t *testing.T, journal *recoveryJournal, want string) {
	t.Helper()
	records, err := journal.records()
	if err != nil || len(records) != 1 || records[0].State != want {
		t.Fatalf("journal records = %#v, %v; want state %q", records, err, want)
	}
}

func assertReopenedSessionJournal(t *testing.T, root, want string) {
	t.Helper()
	reopened, err := prepareRecoveryJournal(root, os.Geteuid())
	if err != nil {
		t.Fatalf("reopen recovery journal = %v", err)
	}
	defer reopened.close()
	records, err := reopened.records()
	if err != nil {
		t.Fatalf("reopened records = %v", err)
	}
	if want == "" {
		if len(records) != 0 {
			t.Fatalf("reopened records = %#v, want empty", records)
		}
		return
	}
	if len(records) != 1 || records[0].State != want {
		t.Fatalf("reopened records = %#v, want state %q", records, want)
	}
}

func validSessionScoreRequest(t *testing.T) rerank.ScoreRequest {
	t.Helper()
	stableID, err := rerank.CandidateID("https://example.com/a")
	if err != nil {
		t.Fatal(err)
	}
	return rerank.ScoreRequest{Query: "query", Candidates: []rerank.ScoringCandidate{{
		StableID: stableID, ProviderRank: 1, Text: "alpha",
	}}}
}
