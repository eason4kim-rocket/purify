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
	"strings"
	"testing"

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
