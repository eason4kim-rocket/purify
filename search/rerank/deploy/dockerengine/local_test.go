package dockerengine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/use-agent/purify/search/rerank/deploy"
)

func TestGenerateLocalSessionSecretsUsesExactEntropyAndNoOverrides(t *testing.T) {
	raw := append(bytes.Repeat([]byte{0x01}, 16), bytes.Repeat([]byte{0xa5}, 32)...)
	runID, key, ok := generateLocalSessionSecrets(bytes.NewReader(raw))
	if !ok || runID != strings.Repeat("01", 16) || key != strings.Repeat("a5", 32) {
		t.Fatalf("generated = %q / %q / %v", runID, key, ok)
	}
	for _, reader := range []io.Reader{nil, bytes.NewReader(raw[:15]), bytes.NewReader(raw[:47])} {
		if run, secret, valid := generateLocalSessionSecrets(reader); valid || run != "" || secret != "" {
			t.Fatalf("short entropy = %q / %q / %v", run, secret, valid)
		}
	}
}

func TestReadLocalProcessRequiresBoundedExactNULTerminatedEnvironment(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "42")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "environ")
	valid := []byte("PATH=/usr/bin\x00NVIDIA_VISIBLE_DEVICES=all\x00NVIDIA_VISIBLE_DEVICES=0\x00")
	if err := os.WriteFile(path, valid, 0o600); err != nil {
		t.Fatal(err)
	}
	inspection, err := readLocalProcess(context.Background(), root, 42)
	if err != nil || inspection.PID != 42 || len(inspection.Environment) != 3 || inspection.Environment[2] != "NVIDIA_VISIBLE_DEVICES=0" {
		t.Fatalf("readLocalProcess(valid) = %#v, %v", inspection, err)
	}
	for name, raw := range map[string][]byte{
		"unterminated":  []byte("A=B"),
		"empty entry":   []byte("A=B\x00\x00"),
		"missing name":  []byte("=B\x00"),
		"missing equal": []byte("AB\x00"),
		"invalid utf8":  {0xff, '=', 'x', 0},
		"oversize":      append(bytes.Repeat([]byte{'x'}, maximumProcessEnvBytes), 0),
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if value, err := readLocalProcess(context.Background(), root, 42); !errors.Is(err, ErrSessionUnavailable) || len(value.Environment) != 0 {
				t.Fatalf("readLocalProcess() = %#v, %v", value, err)
			}
		})
	}
}

func TestInspectAndCleanupLocalSocketAcceptOnlyFreshExactUnixEntry(t *testing.T) {
	runDir := shortSocketDir(t)
	socketPath := filepath.Join(runDir, "vllm.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	unixListener := listener.(*net.UnixListener)
	unixListener.SetUnlinkOnClose(false)
	inspection, err := inspectLocalSocket(context.Background(), runDir)
	if err != nil || inspection.Path != socketPath || !inspection.IsSocket || inspection.IsSymlink || !inspection.Fresh {
		t.Fatalf("inspectLocalSocket() = %#v, %v", inspection, err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	templateDirectory := filepath.Join(t.TempDir(), strings.Repeat("a", 32))
	if err := os.Mkdir(templateDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	templateFile := filepath.Join(templateDirectory, localTemplateName)
	if err := os.WriteFile(templateFile, deploy.ReferenceTemplate(), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := cleanupLocalHost(HostPaths{TemplateFile: templateFile, RunDir: runDir}); err != nil {
		t.Fatalf("cleanupLocalHost() = %v", err)
	}
	for _, path := range []string{socketPath, runDir, templateFile, templateDirectory} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("path still exists after cleanup: %s (%v)", path, err)
		}
	}
}

func TestInspectLocalSocketRejectsExtraAndNonSocketEntries(t *testing.T) {
	for name, setup := range map[string]func(string) error{
		"regular": func(dir string) error { return os.WriteFile(filepath.Join(dir, "vllm.sock"), []byte("x"), 0o600) },
		"extra": func(dir string) error {
			if err := os.WriteFile(filepath.Join(dir, "vllm.sock"), []byte("x"), 0o600); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, "other"), []byte("x"), 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := shortSocketDir(t)
			if err := setup(dir); err != nil {
				t.Fatal(err)
			}
			if value, err := inspectLocalSocket(context.Background(), dir); !errors.Is(err, ErrSessionUnavailable) || value.Path != "" {
				t.Fatalf("inspectLocalSocket() = %#v, %v", value, err)
			}
		})
	}
}

func TestProbeLocalUnauthorizedUsesExactUnixRouteWithoutCredential(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     int
		body       string
		wantOK     bool
		wantNoAuth bool
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"detail":"unauthorized"}`, wantOK: true, wantNoAuth: true},
		{name: "success rejected", status: http.StatusOK, body: `{}`, wantNoAuth: true},
		{name: "oversize rejected", status: http.StatusUnauthorized, body: strings.Repeat("x", maximumProbeBodyBytes+1), wantNoAuth: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := shortSocketDir(t)
			path := filepath.Join(dir, "vllm.sock")
			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodPost || request.URL.Path != "/v1/rerank" || request.Header.Get("Authorization") != "" || request.Host != "localhost" {
					t.Errorf("request = %s %s host=%q auth=%q", request.Method, request.URL.Path, request.Host, request.Header.Get("Authorization"))
				}
				writer.WriteHeader(test.status)
				_, _ = io.WriteString(writer, test.body)
			})}
			serveDone := make(chan struct{})
			go func() { _ = server.Serve(listener); close(serveDone) }()
			err = probeLocalUnauthorized(context.Background(), path)
			_ = server.Close()
			<-serveDone
			if test.wantOK && err != nil {
				t.Fatalf("probeLocalUnauthorized() = %v", err)
			}
			if !test.wantOK && !errors.Is(err, ErrSessionUnavailable) {
				t.Fatalf("probeLocalUnauthorized() = %v", err)
			}
		})
	}
}

func TestStartLocalReferenceSessionCanceledIsInert(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	session, err := StartLocalReferenceSession(ctx, LocalReferenceSessionOptions{SnapshotDir: "/does/not/matter"})
	if session != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("StartLocalReferenceSession(canceled) = %#v, %v", session, err)
	}
}

type orderedLocalReader struct {
	order  *[]string
	reader io.Reader
}

func (reader *orderedLocalReader) Read(value []byte) (int, error) {
	*reader.order = append(*reader.order, "entropy")
	return reader.reader.Read(value)
}

func TestStartLocalReferenceSessionAcquiresRecoversThenGeneratesRun(t *testing.T) {
	root := filepath.Join(canonicalTempDir(t), "recovery")
	engine := &sessionFakeEngine{}
	var order []string
	raw := append(bytes.Repeat([]byte{0x01}, 16), bytes.Repeat([]byte{0xa5}, 32)...)
	local := localStartDependencies{
		recoveryRoot: root, recoveryUID: os.Geteuid(), random: &orderedLocalReader{order: &order, reader: bytes.NewReader(raw)},
		prepareJournal: func(path string, uid int) (*recoveryJournal, error) {
			order = append(order, "journal")
			return prepareRecoveryJournal(path, uid)
		},
		newEngine: func() (Engine, error) {
			order = append(order, "engine")
			return engine, nil
		},
		recover: func(ctx context.Context, dependencies recoveryDependencies) error {
			order = append(order, "recover")
			if _, ok := ctx.Deadline(); !ok || dependencies.journal == nil || dependencies.engine != engine {
				t.Fatalf("recovery dependencies = %#v", dependencies)
			}
			if records, err := dependencies.journal.records(); err != nil || len(records) != 0 {
				t.Fatalf("records before new run = %#v, %v", records, err)
			}
			return nil
		},
		start: func(_ context.Context, dependencies sessionDependencies) (*ReferenceSession, error) {
			order = append(order, "start")
			if dependencies.journal == nil || dependencies.runID != strings.Repeat("01", 16) || dependencies.apiKey != strings.Repeat("a5", 32) {
				t.Fatalf("new session dependencies = %#v", dependencies)
			}
			_ = dependencies.engine.Close()
			_ = dependencies.journal.close()
			return nil, ErrSessionUnavailable
		},
		cleanupHost: func(HostPaths) error { return nil },
	}
	controller := ControllerInspection{GOOS: "linux", GOARCH: "amd64", EffectiveUIDKnown: true, EffectiveUID: 0}
	session, err := startLocalReferenceSession(context.Background(), LocalReferenceSessionOptions{SnapshotDir: testPaths.SnapshotDir}, controller, local)
	if session != nil || !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("startLocalReferenceSession() = %#v, %v", session, err)
	}
	want := []string{"journal", "engine", "recover", "entropy", "entropy", "start"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	reopened, err := prepareRecoveryJournal(root, os.Geteuid())
	if err != nil {
		t.Fatalf("lease after delegated start = %v", err)
	}
	_ = reopened.close()
}

func TestStartLocalReferenceSessionDoesNotGenerateRunUntilRecoveryIsEmpty(t *testing.T) {
	root := filepath.Join(canonicalTempDir(t), "recovery")
	engine := &sessionFakeEngine{}
	randomReads := 0
	starts := 0
	local := localStartDependencies{
		recoveryRoot: root, recoveryUID: os.Geteuid(),
		random:         readerFunc(func([]byte) (int, error) { randomReads++; return 0, io.EOF }),
		prepareJournal: prepareRecoveryJournal,
		newEngine:      func() (Engine, error) { return engine, nil },
		recover: func(_ context.Context, dependencies recoveryDependencies) error {
			_, err := dependencies.journal.createPrepared(strings.Repeat("1a", 16), testPaths.SnapshotDir, testRecoverySpecDigest)
			return err
		},
		start: func(context.Context, sessionDependencies) (*ReferenceSession, error) {
			starts++
			return nil, ErrSessionUnavailable
		},
		cleanupHost: func(HostPaths) error { return nil },
	}
	controller := ControllerInspection{GOOS: "linux", GOARCH: "amd64", EffectiveUIDKnown: true, EffectiveUID: 0}
	if session, err := startLocalReferenceSession(context.Background(), LocalReferenceSessionOptions{SnapshotDir: testPaths.SnapshotDir}, controller, local); session != nil || !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("startLocalReferenceSession() = %#v, %v", session, err)
	}
	if randomReads != 0 || starts != 0 || engine.closeCount() != 1 {
		t.Fatalf("new-run effects = entropy:%d starts:%d closes:%d", randomReads, starts, engine.closeCount())
	}
	assertReopenedSessionJournal(t, root, recoveryStatePrepared)
}

func TestStartLocalReferenceSessionRecoveryFailureClosesEngineAndLease(t *testing.T) {
	root := filepath.Join(canonicalTempDir(t), "recovery")
	engine := &sessionFakeEngine{}
	local := localStartDependencies{
		recoveryRoot: root, recoveryUID: os.Geteuid(), random: bytes.NewReader(make([]byte, 48)),
		prepareJournal: prepareRecoveryJournal, newEngine: func() (Engine, error) { return engine, nil },
		recover: func(ctx context.Context, _ recoveryDependencies) error {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > sessionCleanupTimeout {
				t.Fatalf("unbounded recovery context: %v, %v", deadline, ok)
			}
			return ErrSessionUnavailable
		},
		start: func(context.Context, sessionDependencies) (*ReferenceSession, error) {
			t.Fatal("new session started after recovery failure")
			return nil, nil
		},
		cleanupHost: func(HostPaths) error { return nil },
	}
	controller := ControllerInspection{GOOS: "linux", GOARCH: "amd64", EffectiveUIDKnown: true, EffectiveUID: 0}
	if session, err := startLocalReferenceSession(context.Background(), LocalReferenceSessionOptions{SnapshotDir: testPaths.SnapshotDir}, controller, local); session != nil || !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("startLocalReferenceSession() = %#v, %v", session, err)
	}
	if engine.closeCount() != 1 {
		t.Fatalf("engine close count = %d", engine.closeCount())
	}
	reopened, err := prepareRecoveryJournal(root, os.Geteuid())
	if err != nil {
		t.Fatalf("lease after recovery failure = %v", err)
	}
	_ = reopened.close()
}

func TestStartLocalReferenceSessionRecoveryTimeoutClosesEngineAndLease(t *testing.T) {
	root := filepath.Join(canonicalTempDir(t), "recovery")
	engine := &sessionFakeEngine{}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	local := localStartDependencies{
		recoveryRoot: root, recoveryUID: os.Geteuid(), random: bytes.NewReader(make([]byte, 48)),
		prepareJournal: prepareRecoveryJournal, newEngine: func() (Engine, error) { return engine, nil },
		recover: func(ctx context.Context, _ recoveryDependencies) error {
			<-ctx.Done()
			return ctx.Err()
		},
		start: func(context.Context, sessionDependencies) (*ReferenceSession, error) {
			t.Fatal("new session started after recovery timeout")
			return nil, nil
		},
		cleanupHost: func(HostPaths) error { return nil },
	}
	controller := ControllerInspection{GOOS: "linux", GOARCH: "amd64", EffectiveUIDKnown: true, EffectiveUID: 0}
	if session, err := startLocalReferenceSession(ctx, LocalReferenceSessionOptions{SnapshotDir: testPaths.SnapshotDir}, controller, local); session != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("startLocalReferenceSession() = %#v, %v", session, err)
	}
	if engine.closeCount() != 1 {
		t.Fatalf("engine close count = %d", engine.closeCount())
	}
	if reopened, err := prepareRecoveryJournal(root, os.Geteuid()); err != nil {
		t.Fatalf("lease after recovery timeout = %v", err)
	} else {
		_ = reopened.close()
	}
}

func TestStartLocalReferenceSessionTransfersLeaseToSuccessfulStart(t *testing.T) {
	root := filepath.Join(canonicalTempDir(t), "recovery")
	engine := &sessionFakeEngine{}
	local := localStartDependencies{
		recoveryRoot: root, recoveryUID: os.Geteuid(), random: bytes.NewReader(make([]byte, 48)),
		prepareJournal: prepareRecoveryJournal, newEngine: func() (Engine, error) { return engine, nil },
		recover: func(context.Context, recoveryDependencies) error { return nil },
		start: func(_ context.Context, dependencies sessionDependencies) (*ReferenceSession, error) {
			return &ReferenceSession{engine: dependencies.engine, journal: dependencies.journal}, nil
		},
		cleanupHost: func(HostPaths) error { return nil },
	}
	controller := ControllerInspection{GOOS: "linux", GOARCH: "amd64", EffectiveUIDKnown: true, EffectiveUID: 0}
	session, err := startLocalReferenceSession(context.Background(), LocalReferenceSessionOptions{SnapshotDir: testPaths.SnapshotDir}, controller, local)
	if err != nil || session == nil || session.journal == nil {
		t.Fatalf("startLocalReferenceSession() = %#v, %v", session, err)
	}
	if engine.closeCount() != 0 {
		t.Fatalf("engine closed after successful transfer = %d", engine.closeCount())
	}
	if second, err := prepareRecoveryJournal(root, os.Geteuid()); second != nil || !errors.Is(err, errRecoveryJournal) {
		t.Fatalf("transferred lease not held = %#v, %v", second, err)
	}
	if err := session.engine.Close(); err != nil {
		t.Fatal(err)
	}
	if err := session.journal.close(); err != nil {
		t.Fatal(err)
	}
	if reopened, err := prepareRecoveryJournal(root, os.Geteuid()); err != nil {
		t.Fatalf("transferred lease did not release = %v", err)
	} else {
		_ = reopened.close()
	}
}

func TestStartLocalReferenceSessionRejectsPlatformAndPathBeforeJournal(t *testing.T) {
	called := 0
	base := localStartDependencies{
		recoveryRoot: "/unused", recoveryUID: 0, random: bytes.NewReader(make([]byte, 48)),
		prepareJournal: func(string, int) (*recoveryJournal, error) { called++; return nil, nil },
		newEngine:      func() (Engine, error) { return &sessionFakeEngine{}, nil },
		recover:        func(context.Context, recoveryDependencies) error { return nil },
		start:          func(context.Context, sessionDependencies) (*ReferenceSession, error) { return nil, nil },
		cleanupHost:    func(HostPaths) error { return nil },
	}
	for _, test := range []struct {
		name       string
		controller ControllerInspection
		snapshot   string
	}{
		{name: "platform", controller: ControllerInspection{GOOS: "darwin", GOARCH: "amd64", EffectiveUIDKnown: true, EffectiveUID: 0}, snapshot: testPaths.SnapshotDir},
		{name: "root", controller: ControllerInspection{GOOS: "linux", GOARCH: "amd64", EffectiveUIDKnown: true, EffectiveUID: 1}, snapshot: testPaths.SnapshotDir},
		{name: "path", controller: ControllerInspection{GOOS: "linux", GOARCH: "amd64", EffectiveUIDKnown: true, EffectiveUID: 0}, snapshot: testPaths.SnapshotDir + "/../snapshot"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if session, err := startLocalReferenceSession(context.Background(), LocalReferenceSessionOptions{SnapshotDir: test.snapshot}, test.controller, base); session != nil || !errors.Is(err, ErrSessionUnavailable) {
				t.Fatalf("startLocalReferenceSession() = %#v, %v", session, err)
			}
		})
	}
	if called != 0 {
		t.Fatalf("journal calls before admission = %d", called)
	}
}

type readerFunc func([]byte) (int, error)

func (function readerFunc) Read(value []byte) (int, error) { return function(value) }

func TestValidSnapshotTrustBoundaryRequiresOwnedNonWritableTreeAndAncestors(t *testing.T) {
	trustedRoot := t.TempDir()
	ancestor := filepath.Join(trustedRoot, "cache")
	snapshot := filepath.Join(ancestor, "snapshots", "revision")
	nested := filepath.Join(snapshot, "model")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(nested, "weights.safetensors")
	if err := os.WriteFile(artifact, []byte("weights"), 0o640); err != nil {
		t.Fatal(err)
	}
	uid := os.Geteuid()
	if !validSnapshotTrustBoundary(snapshot, trustedRoot, uid) {
		t.Fatal("valid owned snapshot boundary rejected")
	}

	for _, test := range []struct {
		name string
		path string
		mode os.FileMode
	}{
		{name: "writable ancestor", path: ancestor, mode: 0o770},
		{name: "writable snapshot root", path: snapshot, mode: 0o702},
		{name: "writable nested directory", path: nested, mode: 0o772},
		{name: "writable artifact", path: artifact, mode: 0o662},
	} {
		t.Run(test.name, func(t *testing.T) {
			info, err := os.Stat(test.path)
			if err != nil {
				t.Fatal(err)
			}
			original := info.Mode().Perm()
			if err := os.Chmod(test.path, test.mode); err != nil {
				t.Fatal(err)
			}
			if validSnapshotTrustBoundary(snapshot, trustedRoot, uid) {
				t.Fatal("writable snapshot boundary accepted")
			}
			if err := os.Chmod(test.path, original); err != nil {
				t.Fatal(err)
			}
		})
	}
	if !validSnapshotTrustBoundary(snapshot, trustedRoot, uid) {
		t.Fatal("restored snapshot boundary rejected")
	}
}

func TestValidSnapshotTrustBoundaryRejectsOwnerMismatchAndEscapes(t *testing.T) {
	trustedRoot := t.TempDir()
	snapshot := filepath.Join(trustedRoot, "snapshot")
	if err := os.Mkdir(snapshot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshot, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	uid := os.Geteuid()
	if validSnapshotTrustBoundary(snapshot, trustedRoot, uid+1) {
		t.Fatal("non-owner trust boundary accepted")
	}
	outside := t.TempDir()
	if validSnapshotTrustBoundary(outside, trustedRoot, uid) {
		t.Fatal("snapshot outside trusted root accepted")
	}
	symlink := filepath.Join(trustedRoot, "snapshot-link")
	if err := os.Symlink(snapshot, symlink); err != nil {
		t.Fatal(err)
	}
	if validSnapshotTrustBoundary(symlink, trustedRoot, uid) {
		t.Fatal("symlink snapshot boundary accepted")
	}
}

func shortSocketDir(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "r6a-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}
