package dockerengine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/use-agent/purify/search/rerank/deploy"
)

type recoveryFakeEngine struct {
	mu             sync.Mutex
	resolveResults []OwnershipInspection
	resolveErrs    []error
	createResults  []CreateResult
	createErrs     []error
	owned          OwnershipInspection
	ownershipErrs  []error
	createCalls    int
	resolveCalls   int
	startCalls     int
	killCalls      int
	waitCalls      int
	removeCalls    int
	closed         int
	createSpecs    []CreateSpec
	resolveSpecs   []CreateSpec
}

func (engine *recoveryFakeEngine) Close() error {
	engine.mu.Lock()
	engine.closed++
	engine.mu.Unlock()
	return nil
}
func (*recoveryFakeEngine) InspectDaemon(context.Context) (DaemonInspection, error) {
	return DaemonInspection{}, errors.New("unexpected InspectDaemon")
}
func (*recoveryFakeEngine) InspectImage(context.Context, string) (ImageInspection, error) {
	return ImageInspection{}, errors.New("unexpected InspectImage")
}
func (engine *recoveryFakeEngine) Create(_ context.Context, spec CreateSpec) (CreateResult, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	engine.createCalls++
	engine.createSpecs = append(engine.createSpecs, cloneCreateSpec(spec))
	var result CreateResult
	var err error
	if len(engine.createResults) > 0 {
		result, engine.createResults = engine.createResults[0], engine.createResults[1:]
	}
	if len(engine.createErrs) > 0 {
		err, engine.createErrs = engine.createErrs[0], engine.createErrs[1:]
	}
	return result, err
}
func (engine *recoveryFakeEngine) ResolveCreate(_ context.Context, spec CreateSpec) (OwnershipInspection, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	engine.resolveCalls++
	engine.resolveSpecs = append(engine.resolveSpecs, cloneCreateSpec(spec))
	var result OwnershipInspection
	var err error
	if len(engine.resolveResults) > 0 {
		result, engine.resolveResults = engine.resolveResults[0], engine.resolveResults[1:]
	}
	if len(engine.resolveErrs) > 0 {
		err, engine.resolveErrs = engine.resolveErrs[0], engine.resolveErrs[1:]
	}
	return result, err
}
func (engine *recoveryFakeEngine) Start(context.Context, string) error {
	engine.mu.Lock()
	engine.startCalls++
	engine.mu.Unlock()
	return errors.New("recovery must never start")
}
func (*recoveryFakeEngine) Inspect(context.Context, string) (ContainerInspection, error) {
	return ContainerInspection{}, errors.New("unexpected Inspect")
}
func (engine *recoveryFakeEngine) InspectOwnership(_ context.Context, _ string) (OwnershipInspection, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if len(engine.ownershipErrs) > 0 {
		err := engine.ownershipErrs[0]
		engine.ownershipErrs = engine.ownershipErrs[1:]
		if err != nil {
			return OwnershipInspection{}, err
		}
	}
	return cloneRecoveryOwnership(engine.owned), nil
}
func (*recoveryFakeEngine) ReadReferenceArchive(context.Context, string, string) (io.ReadCloser, error) {
	return nil, errors.New("unexpected archive")
}
func (*recoveryFakeEngine) ExpectReferenceArchiveEvents(context.Context, string, map[string]string, int) (<-chan struct{}, error) {
	return nil, errors.New("unexpected archive events")
}
func (*recoveryFakeEngine) Events(context.Context, string, int64) (<-chan LifecycleEvent, <-chan error) {
	return nil, nil
}
func (engine *recoveryFakeEngine) Kill(_ context.Context, _ string) error {
	engine.mu.Lock()
	engine.killCalls++
	engine.owned.State = ContainerState{Status: ContainerStatusExited, ExitCode: 137}
	engine.mu.Unlock()
	return nil
}
func (engine *recoveryFakeEngine) Wait(_ context.Context, id string) (WaitResult, error) {
	engine.mu.Lock()
	engine.waitCalls++
	engine.mu.Unlock()
	return WaitResult{ContainerID: id, ExitCode: 137}, nil
}
func (engine *recoveryFakeEngine) Remove(_ context.Context, _ string) error {
	engine.mu.Lock()
	engine.removeCalls++
	engine.mu.Unlock()
	return nil
}

func TestRecoveryCoordinatorDrainsPreparedAndOwnedRecordsSeriallyWithoutStart(t *testing.T) {
	journal, _ := newTestRecoveryJournal(t)
	prepared := createRecoveryPrepared(t, journal, strings.Repeat("1", 32))
	creating := createRecoveryCreating(t, journal, strings.Repeat("2", 32))
	ownedID := strings.Repeat("a", 64)
	owned, err := journal.markOwned(creating, ownedID)
	if err != nil {
		t.Fatal(err)
	}
	plan := recoveryPlanForRecord(t, owned)
	engine := &recoveryFakeEngine{owned: recoveryOwnership(plan, ownedID, ContainerState{Status: ContainerStatusExited})}
	cleaned := make([]HostPaths, 0, 2)
	dependencies := validRecoveryDependencies(journal, engine)
	dependencies.cleanupHost = func(paths HostPaths) error {
		cleaned = append(cleaned, paths)
		return nil
	}
	if err := recoverReferenceSessions(context.Background(), dependencies); err != nil {
		t.Fatalf("recoverReferenceSessions() = %v", err)
	}
	if len(cleaned) != 2 || cleaned[0] != recoveryHostPaths(prepared) || cleaned[1] != recoveryHostPaths(owned) {
		t.Fatalf("cleanup order = %#v", cleaned)
	}
	assertRecoveryDrained(t, journal)
	if engine.startCalls != 0 || engine.createCalls != 0 || engine.resolveCalls != 0 || engine.removeCalls != 1 {
		t.Fatalf("calls create=%d resolve=%d start=%d remove=%d", engine.createCalls, engine.resolveCalls, engine.startCalls, engine.removeCalls)
	}
}

func TestRecoveryCoordinatorCreatingNotFoundUsesPreparedHostAndCreateFence(t *testing.T) {
	for _, test := range []struct {
		name          string
		createResult  CreateResult
		createErr     error
		resolveErrs   []error
		resolveResult []OwnershipInspection
	}{
		{name: "fence success", createResult: CreateResult{ContainerID: strings.Repeat("b", 64)}, resolveErrs: []error{errContainerNotFound}, resolveResult: []OwnershipInspection{{}, {}}},
		{name: "partial id", createResult: CreateResult{ContainerID: strings.Repeat("b", 64)}, createErr: ErrEngineOperation, resolveErrs: []error{errContainerNotFound}, resolveResult: []OwnershipInspection{{}, {}}},
		{name: "conflict then resolve", createErr: errContainerConflict, resolveErrs: []error{errContainerNotFound, nil}, resolveResult: []OwnershipInspection{{}, {}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			journal, _ := newTestRecoveryJournal(t)
			record := createRecoveryCreating(t, journal, strings.Repeat("3", 32))
			plan := recoveryPlanForRecord(t, record)
			id := strings.Repeat("b", 64)
			resolved := recoveryOwnership(plan, id, ContainerState{Status: ContainerStatusExited})
			results := append([]OwnershipInspection(nil), test.resolveResult...)
			for index := range results {
				if index == len(results)-1 {
					results[index] = resolved
				}
			}
			engine := &recoveryFakeEngine{
				resolveResults: results, resolveErrs: test.resolveErrs,
				createResults: []CreateResult{test.createResult}, createErrs: []error{test.createErr}, owned: resolved,
			}
			preparedHost := 0
			dependencies := validRecoveryDependencies(journal, engine)
			dependencies.prepareCreateHost = func(context.Context, HostPaths) error { preparedHost++; return nil }
			if err := recoverReferenceSessions(context.Background(), dependencies); err != nil {
				t.Fatalf("recoverReferenceSessions() = %v", err)
			}
			assertRecoveryDrained(t, journal)
			if preparedHost != 1 || engine.createCalls != 1 || engine.startCalls != 0 || engine.removeCalls != 1 {
				t.Fatalf("calls prepare=%d create=%d start=%d remove=%d", preparedHost, engine.createCalls, engine.startCalls, engine.removeCalls)
			}
			assertRecoveryEngineSpecs(t, engine, record)
		})
	}
}

func TestRecoveryCoordinatorRetriesTemporaryResolveAndCreateFailures(t *testing.T) {
	journal, _ := newTestRecoveryJournal(t)
	record := createRecoveryCreating(t, journal, strings.Repeat("4", 32))
	plan := recoveryPlanForRecord(t, record)
	id := strings.Repeat("c", 64)
	resolved := recoveryOwnership(plan, id, ContainerState{Status: ContainerStatusExited})
	engine := &recoveryFakeEngine{
		resolveResults: []OwnershipInspection{{}, {}, {}, resolved},
		resolveErrs:    []error{ErrEngineOperation, errContainerNotFound, errContainerNotFound, nil},
		createResults:  []CreateResult{{}, {}},
		createErrs:     []error{ErrEngineOperation, errContainerConflict},
		owned:          resolved,
	}
	dependencies := validRecoveryDependencies(journal, engine)
	if err := recoverReferenceSessions(context.Background(), dependencies); err != nil {
		t.Fatalf("recoverReferenceSessions() = %v", err)
	}
	assertRecoveryDrained(t, journal)
	if engine.resolveCalls != 4 || engine.createCalls != 2 || engine.startCalls != 0 {
		t.Fatalf("calls resolve=%d create=%d start=%d", engine.resolveCalls, engine.createCalls, engine.startCalls)
	}
}

func TestRecoveryCoordinatorForeignResolutionRetainsJournalAndHost(t *testing.T) {
	for _, test := range []struct {
		name       string
		inspection func(*OwnershipInspection)
		resolveErr error
	}{
		{name: "adapter foreign", resolveErr: ErrOwnershipLost},
		{name: "different id after partial", inspection: func(value *OwnershipInspection) { value.ID = strings.Repeat("e", 64) }},
		{name: "wrong name", inspection: func(value *OwnershipInspection) { value.Name += "-foreign" }},
		{name: "wrong labels", inspection: func(value *OwnershipInspection) { value.Labels[LabelRunID] = strings.Repeat("f", 32) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			journal, _ := newTestRecoveryJournal(t)
			record := createRecoveryCreating(t, journal, strings.Repeat("5", 32))
			plan := recoveryPlanForRecord(t, record)
			id := strings.Repeat("d", 64)
			resolved := recoveryOwnership(plan, id, ContainerState{Status: ContainerStatusExited})
			if test.inspection != nil {
				test.inspection(&resolved)
			}
			engine := &recoveryFakeEngine{
				resolveResults: []OwnershipInspection{{}, resolved}, resolveErrs: []error{errContainerNotFound, test.resolveErr},
				createResults: []CreateResult{{ContainerID: id}}, owned: resolved,
			}
			cleaned := 0
			dependencies := validRecoveryDependencies(journal, engine)
			dependencies.cleanupHost = func(HostPaths) error { cleaned++; return nil }
			err := recoverReferenceSessions(context.Background(), dependencies)
			if !errors.Is(err, ErrOwnershipLost) || cleaned != 0 || engine.removeCalls != 0 || engine.startCalls != 0 {
				t.Fatalf("recover = %v cleaned=%d remove=%d start=%d", err, cleaned, engine.removeCalls, engine.startCalls)
			}
			assertRecoveryRecord(t, journal, record)
		})
	}
}

func TestRecoveryCoordinatorOwnedNotFoundConvergesButForeignIsRetained(t *testing.T) {
	for _, test := range []struct {
		name         string
		ownershipErr error
		mutate       func(*OwnershipInspection)
		wantErr      error
		wantRetained bool
	}{
		{name: "confirmed not found", ownershipErr: errContainerNotFound},
		{name: "foreign name", mutate: func(value *OwnershipInspection) { value.Name += "-foreign" }, wantErr: ErrOwnershipLost, wantRetained: true},
		{name: "foreign labels", mutate: func(value *OwnershipInspection) { value.Labels[LabelSpecDigest] = strings.Repeat("0", 64) }, wantErr: ErrOwnershipLost, wantRetained: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			journal, _ := newTestRecoveryJournal(t)
			creating := createRecoveryCreating(t, journal, strings.Repeat("6", 32))
			id := strings.Repeat("e", 64)
			owned, err := journal.markOwned(creating, id)
			if err != nil {
				t.Fatal(err)
			}
			plan := recoveryPlanForRecord(t, owned)
			inspection := recoveryOwnership(plan, id, ContainerState{Status: ContainerStatusExited})
			if test.mutate != nil {
				test.mutate(&inspection)
			}
			engine := &recoveryFakeEngine{owned: inspection, ownershipErrs: []error{test.ownershipErr}}
			cleaned := 0
			dependencies := validRecoveryDependencies(journal, engine)
			dependencies.cleanupHost = func(HostPaths) error { cleaned++; return nil }
			err = recoverReferenceSessions(context.Background(), dependencies)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("recover = %v, want %v", err, test.wantErr)
			}
			if test.wantRetained {
				assertRecoveryRecord(t, journal, owned)
				if cleaned != 0 {
					t.Fatalf("foreign host cleaned = %d", cleaned)
				}
			} else {
				assertRecoveryDrained(t, journal)
				if cleaned != 1 {
					t.Fatalf("host cleanup = %d", cleaned)
				}
			}
		})
	}
}

func TestRecoveryCoordinatorOwnedCleanupRetriesThenStopsActiveChild(t *testing.T) {
	journal, _ := newTestRecoveryJournal(t)
	creating := createRecoveryCreating(t, journal, strings.Repeat("b", 32))
	id := strings.Repeat("f", 64)
	owned, err := journal.markOwned(creating, id)
	if err != nil {
		t.Fatal(err)
	}
	plan := recoveryPlanForRecord(t, owned)
	engine := &recoveryFakeEngine{
		owned:         recoveryOwnership(plan, id, ContainerState{Status: ContainerStatusRunning, Running: true, PID: 42}),
		ownershipErrs: []error{ErrEngineOperation, nil},
	}
	if err := recoverReferenceSessions(context.Background(), validRecoveryDependencies(journal, engine)); err != nil {
		t.Fatalf("recoverReferenceSessions() = %v", err)
	}
	assertRecoveryDrained(t, journal)
	if engine.killCalls != 1 || engine.waitCalls != 1 || engine.removeCalls != 1 || engine.startCalls != 0 {
		t.Fatalf("kill=%d wait=%d remove=%d start=%d", engine.killCalls, engine.waitCalls, engine.removeCalls, engine.startCalls)
	}
}

func TestRecoveryCoordinatorCancellationAndCorruptJournalFailClosed(t *testing.T) {
	t.Run("cancellation stops retry and retains", func(t *testing.T) {
		journal, _ := newTestRecoveryJournal(t)
		record := createRecoveryCreating(t, journal, strings.Repeat("7", 32))
		engine := &recoveryFakeEngine{resolveErrs: []error{ErrEngineOperation, ErrEngineOperation, ErrEngineOperation, ErrEngineOperation}}
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		defer cancel()
		err := recoverReferenceSessions(ctx, validRecoveryDependencies(journal, engine))
		if !errors.Is(err, context.DeadlineExceeded) || engine.startCalls != 0 {
			t.Fatalf("recover = %v, start=%d", err, engine.startCalls)
		}
		assertRecoveryRecord(t, journal, record)
	})
	t.Run("corrupt journal", func(t *testing.T) {
		journal, root := newTestRecoveryJournal(t)
		runID := strings.Repeat("8", 32)
		if err := os.WriteFile(root+string(os.PathSeparator)+recoveryFilename(runID), []byte("{corrupt}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		engine := &recoveryFakeEngine{}
		if err := recoverReferenceSessions(context.Background(), validRecoveryDependencies(journal, engine)); !errors.Is(err, ErrSessionUnavailable) {
			t.Fatalf("recover = %v", err)
		}
		if engine.createCalls != 0 || engine.resolveCalls != 0 || engine.removeCalls != 0 || engine.startCalls != 0 {
			t.Fatalf("engine mutated on corruption: %#v", engine)
		}
	})
}

func TestRecoveryCoordinatorIdentityDriftDoesNotCallDockerOrMutateHost(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*recoveryRecord)
	}{
		{name: "spec digest", mutate: func(record *recoveryRecord) { record.SpecDigest = strings.Repeat("f", 64) }},
		{name: "container name", mutate: func(record *recoveryRecord) { record.ContainerName += "-foreign" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			journal, _ := newTestRecoveryJournal(t)
			record := createRecoveryCreating(t, journal, strings.Repeat("9", 32))
			stored := record
			if test.name == "spec digest" {
				mutated := record
				test.mutate(&mutated)
				journal.mu.Lock()
				err := journal.replace(&record, mutated)
				journal.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				record = mutated
			} else {
				test.mutate(&record)
			}
			engine := &recoveryFakeEngine{}
			cleaned, prepared := 0, 0
			dependencies := validRecoveryDependencies(journal, engine)
			dependencies.cleanupHost = func(HostPaths) error { cleaned++; return nil }
			dependencies.prepareCreateHost = func(context.Context, HostPaths) error { prepared++; return nil }
			var err error
			if test.name == "spec digest" {
				err = recoverReferenceSessions(context.Background(), dependencies)
			} else {
				err = recoverReferenceRecord(context.Background(), dependencies, record)
			}
			if !errors.Is(err, ErrSessionUnavailable) || cleaned != 0 || prepared != 0 ||
				engine.createCalls != 0 || engine.resolveCalls != 0 || engine.startCalls != 0 || engine.removeCalls != 0 {
				t.Fatalf("recover=%v clean=%d prepare=%d engine=%#v", err, cleaned, prepared, engine)
			}
			if test.name == "spec digest" {
				assertRecoveryRecord(t, journal, record)
			} else {
				assertRecoveryRecord(t, journal, stored)
			}
		})
	}
}

func TestRecoveryCoordinatorPrepareCreateHostFailureRetainsCreatingAuthority(t *testing.T) {
	journal, _ := newTestRecoveryJournal(t)
	record := createRecoveryCreating(t, journal, strings.Repeat("a", 32))
	engine := &recoveryFakeEngine{resolveErrs: []error{errContainerNotFound}}
	cleaned := 0
	dependencies := validRecoveryDependencies(journal, engine)
	dependencies.cleanupHost = func(HostPaths) error { cleaned++; return nil }
	dependencies.prepareCreateHost = func(context.Context, HostPaths) error { return errors.New("host unavailable") }
	if err := recoverReferenceSessions(context.Background(), dependencies); !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("recoverReferenceSessions() = %v", err)
	}
	assertRecoveryRecord(t, journal, record)
	if cleaned != 0 || engine.resolveCalls != 1 || engine.createCalls != 0 || engine.removeCalls != 0 || engine.startCalls != 0 {
		t.Fatalf("clean=%d resolve=%d create=%d remove=%d start=%d", cleaned, engine.resolveCalls, engine.createCalls, engine.removeCalls, engine.startCalls)
	}
}

func validRecoveryDependencies(journal *recoveryJournal, engine Engine) recoveryDependencies {
	return recoveryDependencies{
		journal: journal, engine: engine,
		random:            bytes.NewReader(bytes.Repeat([]byte{0xa5}, 48)),
		cleanupHost:       func(HostPaths) error { return nil },
		prepareCreateHost: func(context.Context, HostPaths) error { return nil },
	}
}

func createRecoveryPrepared(t *testing.T, journal *recoveryJournal, runID string) recoveryRecord {
	t.Helper()
	paths := recoveryHostPaths(recoveryRecord{RunID: runID, SnapshotDir: testRecoverySnapshotDir})
	plan, err := buildReferencePlan(deploy.ReferenceDescriptor(), paths, generatedInputs{RunID: runID, APIKey: strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	record, err := journal.createPrepared(runID, testRecoverySnapshotDir, plan.specDigest)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func createRecoveryCreating(t *testing.T, journal *recoveryJournal, runID string) recoveryRecord {
	t.Helper()
	prepared := createRecoveryPrepared(t, journal, runID)
	record, err := journal.markCreating(prepared)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func recoveryPlanForRecord(t *testing.T, record recoveryRecord) *referencePlan {
	t.Helper()
	plan, err := buildReferencePlan(deploy.ReferenceDescriptor(), recoveryHostPaths(record), generatedInputs{
		RunID: record.RunID, APIKey: strings.Repeat("a", 64),
	})
	if err != nil || plan.specDigest != record.SpecDigest {
		t.Fatalf("recovery plan = %#v, %v", plan, err)
	}
	return plan
}

func recoveryOwnership(plan *referencePlan, id string, state ContainerState) OwnershipInspection {
	return OwnershipInspection{ID: id, Name: "/" + plan.create.Name, Labels: ownershipLabels(plan.create.Labels), State: state}
}

func cloneRecoveryOwnership(value OwnershipInspection) OwnershipInspection {
	value.Labels = cloneMap(value.Labels)
	return value
}

func assertRecoveryDrained(t *testing.T, journal *recoveryJournal) {
	t.Helper()
	records, err := journal.records()
	if err != nil || len(records) != 0 {
		t.Fatalf("recovery records = %#v, %v", records, err)
	}
}

func assertRecoveryRecord(t *testing.T, journal *recoveryJournal, want recoveryRecord) {
	t.Helper()
	records, err := journal.records()
	if err != nil || !reflect.DeepEqual(records, []recoveryRecord{want}) {
		t.Fatalf("recovery records = %#v, want %#v, %v", records, want, err)
	}
}

func assertRecoveryEngineSpecs(t *testing.T, engine *recoveryFakeEngine, record recoveryRecord) {
	t.Helper()
	engine.mu.Lock()
	specs := append([]CreateSpec(nil), engine.resolveSpecs...)
	specs = append(specs, engine.createSpecs...)
	engine.mu.Unlock()
	if len(specs) == 0 {
		t.Fatal("recovery made no identity-bound Docker request")
	}
	for _, spec := range specs {
		digest, err := digestCreateSpec(spec)
		if err != nil || digest != record.SpecDigest || spec.Name != record.ContainerName || spec.Hostname != record.ContainerName ||
			spec.Labels[LabelRunID] != record.RunID || spec.Labels[LabelSpecDigest] != record.SpecDigest {
			t.Fatalf("recovery spec drifted: name=%q host=%q labels=%#v digest=%q err=%v", spec.Name, spec.Hostname, spec.Labels, digest, err)
		}
	}
}
