package dockerengine

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/use-agent/purify/search/rerank"
	"github.com/use-agent/purify/search/rerank/deploy"
	"github.com/use-agent/purify/search/rerank/deploy/runtimeverify"
)

const (
	localRunRoot             = "/run/purify-r6a"
	localTemplateRoot        = "/run/purify-r6a-template"
	localTemplateName        = "qwen3_reranker.jinja"
	localTrustedSnapshotRoot = "/"
	localTrustedSnapshotUID  = 0
	maximumProcessEnvBytes   = 256 << 10
	maximumProcessEnvEntries = 128
	maximumProbeBodyBytes    = 64 << 10
	localProbeTimeout        = 10 * time.Second
)

// LocalReferenceSessionOptions contains the only operator-selected input to
// the R-6a controller. SnapshotDir is admitted only after an exact inventory
// verification; image, argv, environment, network, key, and Docker endpoint
// are all repository- or controller-owned.
type LocalReferenceSessionOptions struct {
	SnapshotDir string
}

// StartLocalReferenceSession launches the pinned local Docker child and
// returns a recording-only live handle. It never creates a production Scorer
// or changes either certified admission gate.
func StartLocalReferenceSession(ctx context.Context, options LocalReferenceSessionOptions) (*ReferenceSession, error) {
	if ctx == nil {
		return nil, ErrSessionUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	controller := ControllerInspection{
		Endpoint: DockerSocketPath, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		EffectiveUIDKnown: true, EffectiveUID: os.Geteuid(),
	}
	if controller.GOOS != "linux" || controller.GOARCH != "amd64" || controller.EffectiveUID != 0 {
		return nil, ErrSessionUnavailable
	}
	runID, apiKey, ok := generateLocalSessionSecrets(rand.Reader)
	if !ok {
		return nil, ErrSessionUnavailable
	}
	paths := HostPaths{
		SnapshotDir:  filepath.Clean(options.SnapshotDir),
		TemplateFile: filepath.Join(localTemplateRoot, runID, localTemplateName),
		RunDir:       filepath.Join(localRunRoot, runID),
	}
	if !validHostPaths(paths) || paths.SnapshotDir != options.SnapshotDir {
		return nil, ErrSessionUnavailable
	}
	engine, err := newMobyEngine()
	if err != nil {
		return nil, ErrSessionUnavailable
	}
	dependencies := localSessionDependencies(controller, paths, runID, apiKey, engine)
	return startReferenceSession(ctx, dependencies)
}

func localSessionDependencies(controller ControllerInspection, paths HostPaths, runID, apiKey string, engine Engine) sessionDependencies {
	return sessionDependencies{
		engine: engine, controller: controller, paths: paths, runID: runID, apiKey: apiKey,
		prepareHost: func(ctx context.Context, value HostPaths) error {
			return prepareLocalHost(ctx, value, os.Geteuid())
		},
		verifyHostArtifacts: func(ctx context.Context) error {
			return verifyLocalHostArtifacts(ctx, paths)
		},
		verifyContainerArtifacts: func(ctx context.Context, engine Engine, containerID string) error {
			return verifyLocalContainerArtifacts(ctx, engine, containerID, paths)
		},
		readProcess: func(ctx context.Context, pid int) (ProcessInspection, error) {
			return readLocalProcess(ctx, "/proc", pid)
		},
		inspectSocket: inspectLocalSocket,
		probeUnauthorized: func(ctx context.Context, runDir string) error {
			return probeLocalUnauthorized(ctx, filepath.Join(runDir, "vllm.sock"))
		},
		newRecorder: func(runDir, key string) (sessionRecorder, error) {
			return rerank.NewUnixReferenceRecorder(rerank.UnixReferenceRecorderConfig{
				SocketPath: filepath.Join(runDir, "vllm.sock"), APIKey: key,
			})
		},
		probeAuthenticated: func(ctx context.Context, recorder sessionRecorder) error {
			_, err := recorder.Record(ctx, localProbeRequest())
			return err
		},
		cleanupHost: func() error { return cleanupLocalHost(paths) },
	}
}

func generateLocalSessionSecrets(reader io.Reader) (string, string, bool) {
	if reader == nil {
		return "", "", false
	}
	runBytes := make([]byte, 16)
	keyBytes := make([]byte, 32)
	if _, err := io.ReadFull(reader, runBytes); err != nil {
		return "", "", false
	}
	if _, err := io.ReadFull(reader, keyBytes); err != nil {
		return "", "", false
	}
	runID := hex.EncodeToString(runBytes)
	key := hex.EncodeToString(keyBytes)
	return runID, key, validLowerHex(runID, 32) && validSecret(key)
}

func prepareLocalHost(ctx context.Context, paths HostPaths, expectedUID int) error {
	if ctx == nil || ctx.Err() != nil || !validHostPaths(paths) || expectedUID < 0 ||
		!verifyTrustedHostSnapshot(paths.SnapshotDir, localTrustedSnapshotRoot, localTrustedSnapshotUID) {
		return ErrSessionUnavailable
	}
	if err := ensurePrivateBase(filepath.Dir(paths.RunDir), expectedUID); err != nil {
		return ErrSessionUnavailable
	}
	templateDirectory := filepath.Dir(paths.TemplateFile)
	if filepath.Dir(templateDirectory) != localTemplateRoot || filepath.Base(paths.TemplateFile) != localTemplateName ||
		filepath.Dir(paths.RunDir) != localRunRoot || filepath.Base(templateDirectory) != filepath.Base(paths.RunDir) {
		return ErrSessionUnavailable
	}
	if err := ensurePrivateBase(localTemplateRoot, expectedUID); err != nil {
		return ErrSessionUnavailable
	}
	createdRun := false
	createdTemplateDirectory := false
	createdTemplate := false
	rollback := func() {
		if createdTemplate {
			_ = os.Remove(paths.TemplateFile)
		}
		if createdTemplateDirectory {
			_ = os.Remove(templateDirectory)
		}
		if createdRun {
			_ = os.Remove(paths.RunDir)
		}
	}
	if err := os.Mkdir(paths.RunDir, 0o700); err != nil {
		return ErrSessionUnavailable
	}
	createdRun = true
	if err := os.Mkdir(templateDirectory, 0o700); err != nil {
		rollback()
		return ErrSessionUnavailable
	}
	createdTemplateDirectory = true
	file, err := os.OpenFile(paths.TemplateFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
	if err != nil {
		rollback()
		return ErrSessionUnavailable
	}
	createdTemplate = true
	template := deploy.ReferenceTemplate()
	writeError := writeAndSync(file, template)
	closeError := file.Close()
	if writeError != nil || closeError != nil || ctx.Err() != nil ||
		!validPrivateDirectory(paths.RunDir, expectedUID, true) ||
		!validPrivateDirectory(templateDirectory, expectedUID, false) ||
		!validTemplateFile(paths.TemplateFile, expectedUID, template) {
		rollback()
		return ErrSessionUnavailable
	}
	return nil
}

func ensurePrivateBase(path string, expectedUID int) error {
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if !validPrivateDirectory(path, expectedUID, false) {
		return ErrSessionUnavailable
	}
	return nil
}

func validPrivateDirectory(path string, expectedUID int, empty bool) bool {
	info, err := os.Lstat(path)
	if err != nil || info.Mode() != os.ModeDir|0o700 || fileUID(info) != expectedUID {
		return false
	}
	if !empty {
		return true
	}
	entries, err := os.ReadDir(path)
	return err == nil && len(entries) == 0
}

func validTemplateFile(path string, expectedUID int, expected []byte) bool {
	beforePath, err := os.Lstat(path)
	if err != nil || !validTemplateInfo(beforePath, expectedUID, len(expected)) {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	openedBefore, statError := file.Stat()
	if statError != nil || !os.SameFile(beforePath, openedBefore) || !validTemplateInfo(openedBefore, expectedUID, len(expected)) {
		_ = file.Close()
		return false
	}
	hash := sha256.New()
	written, copyError := io.Copy(hash, io.LimitReader(file, int64(len(expected))+1))
	openedAfter, statAfterError := file.Stat()
	closeError := file.Close()
	afterPath, pathAfterError := os.Lstat(path)
	want := sha256.Sum256(expected)
	return copyError == nil && statAfterError == nil && closeError == nil && pathAfterError == nil &&
		os.SameFile(openedBefore, openedAfter) && os.SameFile(openedAfter, afterPath) &&
		validTemplateInfo(openedAfter, expectedUID, len(expected)) && validTemplateInfo(afterPath, expectedUID, len(expected)) &&
		written == int64(len(expected)) && bytes.Equal(hash.Sum(nil), want[:])
}

func validTemplateInfo(info os.FileInfo, expectedUID, expectedBytes int) bool {
	return info != nil && info.Mode().IsRegular() && info.Mode().Perm() == 0o400 && info.Mode()&os.ModeSymlink == 0 &&
		fileUID(info) == expectedUID && info.Size() == int64(expectedBytes) && fileLinkCount(info) == 1
}

func writeAndSync(file *os.File, value []byte) error {
	if file == nil {
		return ErrSessionUnavailable
	}
	written, err := file.Write(value)
	if err != nil || written != len(value) {
		return ErrSessionUnavailable
	}
	return file.Sync()
}

func cleanupLocalHost(paths HostPaths) error {
	var first error
	socketPath := filepath.Join(paths.RunDir, "vllm.sock")
	if info, err := os.Lstat(socketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 || os.Remove(socketPath) != nil {
			first = ErrSessionUnavailable
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		first = ErrSessionUnavailable
	}
	for _, path := range []string{paths.TemplateFile, filepath.Dir(paths.TemplateFile), paths.RunDir} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) && first == nil {
			first = ErrSessionUnavailable
		}
	}
	return first
}

func verifyLocalContainerArtifacts(ctx context.Context, engine Engine, containerID string, paths HostPaths) error {
	if ctx == nil || ctx.Err() != nil || engine == nil || !validLowerHex(containerID, 64) ||
		verifyLocalHostArtifacts(ctx, paths) != nil {
		return ErrSessionUnavailable
	}
	snapshot, err := engine.ReadReferenceArchive(ctx, containerID, ReferenceSnapshotPath)
	if err != nil {
		return ErrSessionUnavailable
	}
	verifyError := runtimeverify.VerifyContainerArchive(snapshot, deploy.ReferenceSnapshotManifest(), ReferenceSnapshotPath)
	closeError := snapshot.Close()
	if verifyError != nil || closeError != nil || ctx.Err() != nil {
		return ErrSessionUnavailable
	}
	template, err := engine.ReadReferenceArchive(ctx, containerID, ReferenceTemplatePath)
	if err != nil {
		return ErrSessionUnavailable
	}
	verifyError = runtimeverify.VerifyContainerFileArchive(template, ReferenceTemplatePath, deploy.ReferenceTemplate())
	closeError = template.Close()
	if verifyError != nil || closeError != nil || ctx.Err() != nil {
		return ErrSessionUnavailable
	}
	return nil
}

func verifyLocalHostArtifacts(ctx context.Context, paths HostPaths) error {
	if ctx == nil || ctx.Err() != nil ||
		!verifyTrustedHostSnapshot(paths.SnapshotDir, localTrustedSnapshotRoot, localTrustedSnapshotUID) ||
		!validTemplateFile(paths.TemplateFile, os.Geteuid(), deploy.ReferenceTemplate()) || ctx.Err() != nil {
		return ErrSessionUnavailable
	}
	return nil
}

// verifyTrustedHostSnapshot binds content verification to an immutable host
// trust boundary. The production caller admits only root-owned paths from the
// snapshot through the filesystem root and a root-owned snapshot tree with no
// group/other write permission. Rechecking the trust boundary after hashing
// prevents an artifact from being admitted after its ownership or mode was
// changed during verification.
func verifyTrustedHostSnapshot(snapshotRoot, trustedRoot string, expectedUID int) bool {
	if !validSnapshotTrustBoundary(snapshotRoot, trustedRoot, expectedUID) ||
		runtimeverify.VerifyHostSnapshot(snapshotRoot, deploy.ReferenceSnapshotManifest()) != nil {
		return false
	}
	return validSnapshotTrustBoundary(snapshotRoot, trustedRoot, expectedUID)
}

func validSnapshotTrustBoundary(snapshotRoot, trustedRoot string, expectedUID int) bool {
	if expectedUID < 0 || snapshotRoot == "" || trustedRoot == "" ||
		!filepath.IsAbs(snapshotRoot) || !filepath.IsAbs(trustedRoot) ||
		filepath.Clean(snapshotRoot) != snapshotRoot || filepath.Clean(trustedRoot) != trustedRoot {
		return false
	}
	relative, err := filepath.Rel(trustedRoot, snapshotRoot)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	for current := snapshotRoot; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || !validTrustedSnapshotInfo(info, expectedUID, true) {
			return false
		}
		if current == trustedRoot {
			break
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
	}
	before, err := os.Lstat(snapshotRoot)
	if err != nil || !validTrustedSnapshotInfo(before, expectedUID, true) {
		return false
	}
	err = filepath.WalkDir(snapshotRoot, func(path string, entry os.DirEntry, walkError error) error {
		if walkError != nil {
			return walkError
		}
		info, err := entry.Info()
		if err != nil || !validTrustedSnapshotInfo(info, expectedUID, entry.IsDir()) {
			return ErrSessionUnavailable
		}
		return nil
	})
	after, statError := os.Lstat(snapshotRoot)
	return err == nil && statError == nil && os.SameFile(before, after) &&
		validTrustedSnapshotInfo(after, expectedUID, true)
}

func validTrustedSnapshotInfo(info os.FileInfo, expectedUID int, requireDirectory bool) bool {
	if info == nil || fileUID(info) != expectedUID || info.Mode().Perm()&0o022 != 0 || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	return !requireDirectory || info.IsDir()
}

func readLocalProcess(ctx context.Context, procRoot string, pid int) (ProcessInspection, error) {
	if ctx == nil || ctx.Err() != nil || pid <= 0 || procRoot == "" || !filepath.IsAbs(procRoot) || filepath.Clean(procRoot) != procRoot {
		return ProcessInspection{}, ErrSessionUnavailable
	}
	file, err := os.Open(filepath.Join(procRoot, strconv.Itoa(pid), "environ"))
	if err != nil {
		return ProcessInspection{}, ErrSessionUnavailable
	}
	raw, readError := io.ReadAll(io.LimitReader(file, maximumProcessEnvBytes+1))
	closeError := file.Close()
	if readError != nil || closeError != nil || len(raw) == 0 || len(raw) > maximumProcessEnvBytes || raw[len(raw)-1] != 0 || ctx.Err() != nil {
		return ProcessInspection{}, ErrSessionUnavailable
	}
	parts := bytes.Split(raw[:len(raw)-1], []byte{0})
	if len(parts) == 0 || len(parts) > maximumProcessEnvEntries {
		return ProcessInspection{}, ErrSessionUnavailable
	}
	environment := make([]string, len(parts))
	for index, part := range parts {
		entry := string(part)
		name, _, present := strings.Cut(entry, "=")
		if len(part) == 0 || len(part) > maximumSecretBytes || !utf8.Valid(part) || !present || name == "" {
			return ProcessInspection{}, ErrSessionUnavailable
		}
		environment[index] = entry
	}
	return ProcessInspection{PID: pid, Environment: environment}, nil
}

func inspectLocalSocket(ctx context.Context, runDir string) (SocketInspection, error) {
	if ctx == nil || ctx.Err() != nil || runDir == "" || !filepath.IsAbs(runDir) || filepath.Clean(runDir) != runDir {
		return SocketInspection{}, ErrSessionUnavailable
	}
	entries, err := os.ReadDir(runDir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "vllm.sock" {
		return SocketInspection{}, ErrSessionUnavailable
	}
	path := filepath.Join(runDir, "vllm.sock")
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 || ctx.Err() != nil {
		return SocketInspection{}, ErrSessionUnavailable
	}
	return SocketInspection{Path: path, IsSocket: true, Fresh: true}, nil
}

func probeLocalUnauthorized(ctx context.Context, socketPath string) error {
	if ctx == nil || ctx.Err() != nil || !filepath.IsAbs(socketPath) || filepath.Clean(socketPath) != socketPath {
		return ErrSessionUnavailable
	}
	encoded, _, err := rerank.EncodeReferenceRequest(localProbeRequest())
	if err != nil {
		return ErrSessionUnavailable
	}
	dialer := net.Dialer{Timeout: localProbeTimeout}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if (network != "tcp" && network != "tcp4" && network != "tcp6") || address != "localhost:80" {
				return nil, ErrSessionUnavailable
			}
			return dialer.DialContext(ctx, "unix", socketPath)
		},
		MaxConnsPerHost: 1, MaxIdleConnsPerHost: 1, IdleConnTimeout: time.Second,
		ResponseHeaderTimeout: localProbeTimeout, MaxResponseHeaderBytes: 64 << 10,
	}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost/v1/rerank", bytes.NewReader(encoded))
	if err != nil {
		return ErrSessionUnavailable
	}
	request.Header.Set("Content-Type", "application/json")
	client := http.Client{
		Transport: transport, Timeout: localProbeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return ErrSessionUnavailable },
	}
	response, err := client.Do(request)
	if err != nil {
		return ErrSessionUnavailable
	}
	read, readError := io.Copy(io.Discard, io.LimitReader(response.Body, maximumProbeBodyBytes+1))
	closeError := response.Body.Close()
	if readError != nil || closeError != nil || read > maximumProbeBodyBytes || response.StatusCode != http.StatusUnauthorized || ctx.Err() != nil {
		return ErrSessionUnavailable
	}
	return nil
}

func localProbeRequest() rerank.ScoreRequest {
	id, _ := rerank.CandidateID("https://reference.invalid/readiness")
	return rerank.ScoreRequest{Query: "readiness", Candidates: []rerank.ScoringCandidate{{
		StableID: id, ProviderRank: 1, Text: "readiness\nprobe",
	}}}
}

func fileUID(info os.FileInfo) int {
	if info == nil {
		return -1
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1
	}
	return int(stat.Uid)
}

func fileLinkCount(info os.FileInfo) uint64 {
	if info == nil {
		return 0
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return uint64(stat.Nlink)
}
