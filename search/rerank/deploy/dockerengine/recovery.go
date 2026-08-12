package dockerengine

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unicode"
	"unicode/utf8"
)

const (
	localRecoveryRoot             = "/var/lib/purify-r6a-recovery"
	recoveryJournalVersion        = "purify-r6a-recovery-v1"
	recoveryStatePrepared         = "prepared"
	recoveryStateCreating         = "creating"
	recoveryStateOwned            = "owned"
	recoveryJournalSuffix         = ".json"
	recoveryLeaseFilename         = ".controller.lock"
	maximumRecoveryJournalBytes   = 8 << 10
	maximumRecoveryJournalRecords = 128
	maximumRecoveryTemporaries    = 128
	maximumRecoveryRootBytes      = 4096
)

var errRecoveryJournal = errors.New("rerank docker engine: recovery journal rejected")

// recoveryRecord contains only the deterministic identity needed to recover
// one controller-owned container. SnapshotDir is the only operator input: it
// binds the exact host-cleanup paths and secret-redacted spec identity. The
// record intentionally cannot contain the API key, environment, argv, or
// session credential.
type recoveryRecord struct {
	Version       string `json:"version"`
	State         string `json:"state"`
	RunID         string `json:"run_id"`
	ContainerName string `json:"container_name"`
	SnapshotDir   string `json:"snapshot_dir"`
	ContainerID   string `json:"container_id"`
	SpecDigest    string `json:"spec_digest"`
}

// recoveryLease is an advisory, process-wide controller lease. The flock is
// held for the complete lifetime of recoveryJournal, so a second supervisor
// cannot classify a still-active session as stale merely because it can read
// the journal directory.
type recoveryLease struct {
	mu          sync.Mutex
	root        string
	expectedUID int
	file        *os.File
	active      bool
}

type recoveryJournal struct {
	mu          sync.Mutex
	root        string
	expectedUID int
	lease       *recoveryLease
}

// prepareRecoveryJournal creates or opens the exact private journal root and
// acquires its non-blocking exclusive controller lease. The returned handle
// must remain open for the full active-session and recovery interval.
func prepareRecoveryJournal(rootPath string, expectedUID int) (*recoveryJournal, error) {
	lease, err := acquireRecoveryLease(rootPath, expectedUID)
	if err != nil {
		return nil, errRecoveryJournal
	}
	return &recoveryJournal{root: rootPath, expectedUID: expectedUID, lease: lease}, nil
}

func acquireRecoveryLease(rootPath string, expectedUID int) (*recoveryLease, error) {
	root, created, err := openRecoveryRoot(rootPath, expectedUID, true)
	if err != nil {
		return nil, errRecoveryJournal
	}
	defer root.Close()

	before, beforeErr := root.Lstat(recoveryLeaseFilename)
	if beforeErr != nil && !errors.Is(beforeErr, os.ErrNotExist) {
		return nil, errRecoveryJournal
	}
	if beforeErr == nil && !validRecoveryFileInfo(before, expectedUID, 0) {
		return nil, errRecoveryJournal
	}
	file, err := root.OpenFile(recoveryLeaseFilename, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, errRecoveryJournal
	}
	closeOnFailure := true
	defer func() {
		if closeOnFailure {
			_ = file.Close()
		}
	}()
	chmodErr := file.Chmod(0o600)
	opened, openedErr := file.Stat()
	after, afterErr := root.Lstat(recoveryLeaseFilename)
	if chmodErr != nil || openedErr != nil || afterErr != nil || !validRecoveryFileInfo(opened, expectedUID, 0) ||
		!validRecoveryFileInfo(after, expectedUID, 0) || !os.SameFile(opened, after) ||
		beforeErr == nil && !os.SameFile(before, opened) || file.Sync() != nil {
		return nil, errRecoveryJournal
	}
	if (created || beforeErr != nil) && syncRecoveryDirectory(root) != nil {
		return nil, errRecoveryJournal
	}
	if syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return nil, errRecoveryJournal
	}
	if !validRecoveryRootHandle(root, rootPath, expectedUID) ||
		!validRecoveryAncestors(filepath.Dir(rootPath), expectedUID) {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		return nil, errRecoveryJournal
	}
	// A crash may leave only an unpublished atomic-write temporary. Remove
	// precisely those private, bounded remnants while holding the controller
	// lease; malformed or unsafe lookalikes remain untouched and fail closed.
	if cleanupRecoveryTemporaries(root, expectedUID) != nil ||
		!validRecoveryRootHandle(root, rootPath, expectedUID) ||
		!validRecoveryAncestors(filepath.Dir(rootPath), expectedUID) {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		return nil, errRecoveryJournal
	}
	locked, lockedErr := file.Stat()
	lockPath, lockPathErr := root.Lstat(recoveryLeaseFilename)
	if lockedErr != nil || lockPathErr != nil || !validRecoveryFileInfo(locked, expectedUID, 0) ||
		!validRecoveryFileInfo(lockPath, expectedUID, 0) || !os.SameFile(locked, lockPath) {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		return nil, errRecoveryJournal
	}
	closeOnFailure = false
	return &recoveryLease{root: rootPath, expectedUID: expectedUID, file: file, active: true}, nil
}

func (journal *recoveryJournal) close() error {
	if journal == nil {
		return errRecoveryJournal
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	if journal.lease == nil {
		return errRecoveryJournal
	}
	err := journal.lease.close()
	journal.lease = nil
	return err
}

func (lease *recoveryLease) close() error {
	if lease == nil {
		return errRecoveryJournal
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if !lease.active || lease.file == nil {
		return errRecoveryJournal
	}
	unlockErr := syscall.Flock(int(lease.file.Fd()), syscall.LOCK_UN)
	closeErr := lease.file.Close()
	lease.active = false
	lease.file = nil
	if unlockErr != nil || closeErr != nil {
		return errRecoveryJournal
	}
	return nil
}

func (journal *recoveryJournal) createPrepared(runID, snapshotDir, specDigest string) (recoveryRecord, error) {
	record := recoveryRecord{
		Version:       recoveryJournalVersion,
		State:         recoveryStatePrepared,
		RunID:         runID,
		ContainerName: recoveryContainerName(runID),
		SnapshotDir:   snapshotDir,
		SpecDigest:    specDigest,
	}
	if !validRecoveryRecord(record) || journal == nil {
		return recoveryRecord{}, errRecoveryJournal
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	if err := journal.replace(nil, record); err != nil {
		return recoveryRecord{}, errRecoveryJournal
	}
	return record, nil
}

// markCreating is the durable permission boundary for Docker Create. Callers
// must not issue Create until this transition and its directory fsync return.
func (journal *recoveryJournal) markCreating(current recoveryRecord) (recoveryRecord, error) {
	if journal == nil || !validRecoveryRecord(current) || current.State != recoveryStatePrepared || current.ContainerID != "" {
		return recoveryRecord{}, errRecoveryJournal
	}
	next := current
	next.State = recoveryStateCreating
	journal.mu.Lock()
	defer journal.mu.Unlock()
	if err := journal.replace(&current, next); err != nil {
		return recoveryRecord{}, errRecoveryJournal
	}
	return next, nil
}

func (journal *recoveryJournal) markOwned(current recoveryRecord, containerID string) (recoveryRecord, error) {
	if journal == nil || !validRecoveryRecord(current) || current.State != recoveryStateCreating || current.ContainerID != "" ||
		!validLowerHex(containerID, 64) {
		return recoveryRecord{}, errRecoveryJournal
	}
	next := current
	next.State = recoveryStateOwned
	next.ContainerID = containerID
	journal.mu.Lock()
	defer journal.mu.Unlock()
	if err := journal.replace(&current, next); err != nil {
		return recoveryRecord{}, errRecoveryJournal
	}
	return next, nil
}

func (journal *recoveryJournal) records() ([]recoveryRecord, error) {
	if journal == nil {
		return nil, errRecoveryJournal
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	root, err := journal.openLeasedRoot()
	if err != nil {
		return nil, errRecoveryJournal
	}
	defer root.Close()
	records, err := readRecoveryRecords(root, journal.expectedUID)
	if err != nil || !validRecoveryRootHandle(root, journal.root, journal.expectedUID) ||
		!validRecoveryAncestors(filepath.Dir(journal.root), journal.expectedUID) {
		return nil, errRecoveryJournal
	}
	return records, nil
}

func (journal *recoveryJournal) remove(current recoveryRecord) error {
	// A creating record is deliberately not removable. Docker's public API has
	// no cross-implementation completion barrier for an ambiguous Create, so
	// automatic recovery quarantines it until a future authenticated recovery
	// protocol can persist owned authority.
	if journal == nil || !validRecoveryRecord(current) || current.State == recoveryStateCreating {
		return errRecoveryJournal
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	root, err := journal.openLeasedRoot()
	if err != nil {
		return errRecoveryJournal
	}
	defer root.Close()
	records, err := readRecoveryRecords(root, journal.expectedUID)
	if err != nil {
		return errRecoveryJournal
	}
	stored, present := findRecoveryRecord(records, current.RunID)
	if !present || stored != current || root.Remove(recoveryFilename(current.RunID)) != nil ||
		syncRecoveryDirectory(root) != nil {
		return errRecoveryJournal
	}
	if _, err := root.Lstat(recoveryFilename(current.RunID)); !errors.Is(err, os.ErrNotExist) {
		return errRecoveryJournal
	}
	if _, err := readRecoveryRecords(root, journal.expectedUID); err != nil ||
		!validRecoveryRootHandle(root, journal.root, journal.expectedUID) ||
		!validRecoveryAncestors(filepath.Dir(journal.root), journal.expectedUID) {
		return errRecoveryJournal
	}
	return nil
}

func (journal *recoveryJournal) replace(current *recoveryRecord, next recoveryRecord) error {
	if journal == nil || !validRecoveryRecord(next) || current != nil && (!validRecoveryRecord(*current) || current.RunID != next.RunID) {
		return errRecoveryJournal
	}
	root, err := journal.openLeasedRoot()
	if err != nil {
		return errRecoveryJournal
	}
	defer root.Close()
	records, err := readRecoveryRecords(root, journal.expectedUID)
	if err != nil {
		return errRecoveryJournal
	}
	stored, present := findRecoveryRecord(records, next.RunID)
	if current == nil && present || current != nil && (!present || stored != *current) {
		return errRecoveryJournal
	}
	if current == nil && len(records) >= maximumRecoveryJournalRecords {
		return errRecoveryJournal
	}
	encoded, err := marshalRecoveryRecord(next)
	if err != nil {
		return errRecoveryJournal
	}
	temporary, err := recoveryTemporaryFilename(next.RunID)
	if err != nil {
		return errRecoveryJournal
	}
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errRecoveryJournal
	}
	temporaryPresent := true
	defer func() {
		if temporaryPresent {
			_ = root.Remove(temporary)
		}
	}()
	chmodErr := file.Chmod(0o600)
	written, writeErr := file.Write(encoded)
	syncErr := file.Sync()
	opened, statErr := file.Stat()
	pathInfo, pathErr := root.Lstat(temporary)
	closeErr := file.Close()
	if chmodErr != nil || writeErr != nil || written != len(encoded) || syncErr != nil || statErr != nil || pathErr != nil || closeErr != nil ||
		!validRecoveryFileInfo(opened, journal.expectedUID, int64(len(encoded))) ||
		!validRecoveryFileInfo(pathInfo, journal.expectedUID, int64(len(encoded))) || !os.SameFile(opened, pathInfo) {
		return errRecoveryJournal
	}
	if root.Rename(temporary, recoveryFilename(next.RunID)) != nil {
		return errRecoveryJournal
	}
	temporaryPresent = false
	if syncRecoveryDirectory(root) != nil {
		return errRecoveryJournal
	}
	stored, err = readRecoveryRecord(root, recoveryFilename(next.RunID), journal.expectedUID)
	if err != nil || stored != next {
		return errRecoveryJournal
	}
	if _, err := readRecoveryRecords(root, journal.expectedUID); err != nil ||
		!validRecoveryRootHandle(root, journal.root, journal.expectedUID) ||
		!validRecoveryAncestors(filepath.Dir(journal.root), journal.expectedUID) {
		return errRecoveryJournal
	}
	return nil
}

func (journal *recoveryJournal) openLeasedRoot() (*os.Root, error) {
	if journal == nil || journal.lease == nil {
		return nil, errRecoveryJournal
	}
	root, _, err := openRecoveryRoot(journal.root, journal.expectedUID, false)
	if err != nil {
		return nil, errRecoveryJournal
	}
	if !journal.lease.valid(root) {
		_ = root.Close()
		return nil, errRecoveryJournal
	}
	return root, nil
}

func (lease *recoveryLease) valid(root *os.Root) bool {
	if lease == nil || root == nil {
		return false
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if !lease.active || lease.file == nil {
		return false
	}
	opened, openedErr := lease.file.Stat()
	pathInfo, pathErr := root.Lstat(recoveryLeaseFilename)
	return openedErr == nil && pathErr == nil && validRecoveryFileInfo(opened, lease.expectedUID, 0) &&
		validRecoveryFileInfo(pathInfo, lease.expectedUID, 0) && os.SameFile(opened, pathInfo)
}

func openRecoveryRoot(rootPath string, expectedUID int, create bool) (*os.Root, bool, error) {
	if !validRecoveryRootPath(rootPath) || expectedUID < 0 || !validRecoveryAncestors(filepath.Dir(rootPath), expectedUID) {
		return nil, false, errRecoveryJournal
	}
	created := false
	if create {
		if err := os.Mkdir(rootPath, 0o700); err == nil {
			created = true
		} else if !errors.Is(err, os.ErrExist) {
			return nil, false, errRecoveryJournal
		}
	}
	before, err := os.Lstat(rootPath)
	if err != nil || !validRecoveryRootInfo(before, expectedUID) {
		return nil, false, errRecoveryJournal
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, false, errRecoveryJournal
	}
	if !validRecoveryRootHandle(root, rootPath, expectedUID) || !validRecoveryAncestors(filepath.Dir(rootPath), expectedUID) {
		_ = root.Close()
		return nil, false, errRecoveryJournal
	}
	// Every acquisition that may authorize a later Create re-durably anchors
	// the journal root in its parent. This also closes a prior process crash
	// between Mkdir and the first parent-directory fsync.
	if create && syncRecoveryParent(rootPath) != nil {
		_ = root.Close()
		return nil, false, errRecoveryJournal
	}
	return root, created, nil
}

func validRecoveryRootHandle(root *os.Root, rootPath string, expectedUID int) bool {
	if root == nil {
		return false
	}
	opened, openedErr := root.Stat(".")
	pathInfo, pathErr := os.Lstat(rootPath)
	return openedErr == nil && pathErr == nil && validRecoveryRootInfo(opened, expectedUID) &&
		validRecoveryRootInfo(pathInfo, expectedUID) && os.SameFile(opened, pathInfo)
}

func validRecoveryRootPath(path string) bool {
	if path == "" || len(path) > maximumRecoveryRootBytes || !utf8.ValidString(path) ||
		!filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) {
		return false
	}
	for _, character := range path {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validRecoveryRootInfo(info os.FileInfo, expectedUID int) bool {
	return info != nil && info.Mode() == os.ModeDir|0o700 && fileUID(info) == expectedUID && info.Mode()&os.ModeSymlink == 0
}

// validRecoveryAncestors rejects redirected or replaceable path components.
// A root-owned sticky directory such as /tmp is accepted because its sticky
// bit prevents another uid from replacing the expected-uid private child.
func validRecoveryAncestors(path string, expectedUID int) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || expectedUID < 0 {
		return false
	}
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 ||
			fileUID(info) != 0 && fileUID(info) != expectedUID {
			return false
		}
		if info.Mode().Perm()&0o022 != 0 &&
			(info.Mode()&os.ModeSticky == 0 || fileUID(info) != 0) {
			return false
		}
		parent := filepath.Dir(current)
		if parent == current {
			return true
		}
	}
}

func validRecoveryFileInfo(info os.FileInfo, expectedUID int, exactSize int64) bool {
	return info != nil && info.Mode().IsRegular() && info.Mode().Perm() == 0o600 && info.Mode()&os.ModeSymlink == 0 &&
		fileUID(info) == expectedUID && fileLinkCount(info) == 1 && info.Size() == exactSize
}

func cleanupRecoveryTemporaries(root *os.Root, expectedUID int) error {
	if root == nil {
		return errRecoveryJournal
	}
	directory, err := root.Open(".")
	if err != nil {
		return errRecoveryJournal
	}
	entries, readErr := directory.ReadDir(maximumRecoveryJournalRecords + maximumRecoveryTemporaries + 2)
	closeErr := directory.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) || closeErr != nil ||
		len(entries) > maximumRecoveryJournalRecords+maximumRecoveryTemporaries+1 {
		return errRecoveryJournal
	}
	type recoveryTemporary struct {
		name string
		info os.FileInfo
	}
	temporaries := make([]recoveryTemporary, 0)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".tmp") {
			continue
		}
		if !validRecoveryTemporaryFilename(name) {
			return errRecoveryJournal
		}
		if len(temporaries) == maximumRecoveryTemporaries {
			return errRecoveryJournal
		}
		before, err := root.Lstat(name)
		if err != nil || before.Size() < 0 || before.Size() > maximumRecoveryJournalBytes ||
			!validRecoveryFileInfo(before, expectedUID, before.Size()) {
			return errRecoveryJournal
		}
		temporaries = append(temporaries, recoveryTemporary{name: name, info: before})
	}
	for _, temporary := range temporaries {
		before, err := root.Lstat(temporary.name)
		if err != nil || !os.SameFile(temporary.info, before) || before.Size() < 0 || before.Size() > maximumRecoveryJournalBytes ||
			!validRecoveryFileInfo(before, expectedUID, before.Size()) {
			return errRecoveryJournal
		}
		file, err := root.Open(temporary.name)
		if err != nil {
			return errRecoveryJournal
		}
		opened, statErr := file.Stat()
		closeErr := file.Close()
		after, pathErr := root.Lstat(temporary.name)
		if statErr != nil || closeErr != nil || pathErr != nil ||
			!validRecoveryFileInfo(opened, expectedUID, before.Size()) ||
			!validRecoveryFileInfo(after, expectedUID, before.Size()) ||
			!os.SameFile(temporary.info, before) || !os.SameFile(before, opened) || !os.SameFile(opened, after) ||
			root.Remove(temporary.name) != nil {
			return errRecoveryJournal
		}
	}
	if len(temporaries) > 0 && syncRecoveryDirectory(root) != nil {
		return errRecoveryJournal
	}
	return nil
}

func readRecoveryRecords(root *os.Root, expectedUID int) ([]recoveryRecord, error) {
	if root == nil {
		return nil, errRecoveryJournal
	}
	directory, err := root.Open(".")
	if err != nil {
		return nil, errRecoveryJournal
	}
	entries, readErr := directory.ReadDir(maximumRecoveryJournalRecords + 2)
	closeErr := directory.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) || closeErr != nil || len(entries) > maximumRecoveryJournalRecords+1 {
		return nil, errRecoveryJournal
	}
	records := make([]recoveryRecord, 0, len(entries))
	leaseSeen := false
	for _, entry := range entries {
		name := entry.Name()
		if name == recoveryLeaseFilename {
			if leaseSeen {
				return nil, errRecoveryJournal
			}
			leaseSeen = true
			info, err := root.Lstat(name)
			if err != nil || !validRecoveryFileInfo(info, expectedUID, 0) {
				return nil, errRecoveryJournal
			}
			continue
		}
		if len(records) == maximumRecoveryJournalRecords || !validRecoveryFilename(name) {
			return nil, errRecoveryJournal
		}
		record, err := readRecoveryRecord(root, name, expectedUID)
		if err != nil || recoveryFilename(record.RunID) != name {
			return nil, errRecoveryJournal
		}
		records = append(records, record)
	}
	if !leaseSeen {
		return nil, errRecoveryJournal
	}
	sort.Slice(records, func(left, right int) bool { return records[left].RunID < records[right].RunID })
	for index := 1; index < len(records); index++ {
		if records[index-1].RunID == records[index].RunID {
			return nil, errRecoveryJournal
		}
	}
	return records, nil
}

func readRecoveryRecord(root *os.Root, name string, expectedUID int) (recoveryRecord, error) {
	if root == nil || !validRecoveryFilename(name) {
		return recoveryRecord{}, errRecoveryJournal
	}
	before, err := root.Lstat(name)
	if err != nil || before.Size() <= 0 || before.Size() > maximumRecoveryJournalBytes ||
		!validRecoveryFileInfo(before, expectedUID, before.Size()) {
		return recoveryRecord{}, errRecoveryJournal
	}
	file, err := root.Open(name)
	if err != nil {
		return recoveryRecord{}, errRecoveryJournal
	}
	openedBefore, statErr := file.Stat()
	raw, readErr := io.ReadAll(io.LimitReader(file, maximumRecoveryJournalBytes+1))
	openedAfter, statAfterErr := file.Stat()
	closeErr := file.Close()
	after, pathAfterErr := root.Lstat(name)
	if statErr != nil || readErr != nil || statAfterErr != nil || closeErr != nil || pathAfterErr != nil ||
		len(raw) == 0 || len(raw) > maximumRecoveryJournalBytes || int64(len(raw)) != before.Size() ||
		!validRecoveryFileInfo(openedBefore, expectedUID, int64(len(raw))) ||
		!validRecoveryFileInfo(openedAfter, expectedUID, int64(len(raw))) ||
		!validRecoveryFileInfo(after, expectedUID, int64(len(raw))) ||
		!os.SameFile(before, openedBefore) || !os.SameFile(openedBefore, openedAfter) || !os.SameFile(openedAfter, after) {
		return recoveryRecord{}, errRecoveryJournal
	}
	var record recoveryRecord
	if json.Unmarshal(raw, &record) != nil || !validRecoveryRecord(record) || recoveryFilename(record.RunID) != name {
		return recoveryRecord{}, errRecoveryJournal
	}
	canonical, err := marshalRecoveryRecord(record)
	if err != nil || !bytes.Equal(raw, canonical) {
		return recoveryRecord{}, errRecoveryJournal
	}
	return record, nil
}

func marshalRecoveryRecord(record recoveryRecord) ([]byte, error) {
	if !validRecoveryRecord(record) {
		return nil, errRecoveryJournal
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, errRecoveryJournal
	}
	encoded = append(encoded, '\n')
	if len(encoded) == 0 || len(encoded) > maximumRecoveryJournalBytes {
		return nil, errRecoveryJournal
	}
	return encoded, nil
}

func validRecoveryRecord(record recoveryRecord) bool {
	if record.Version != recoveryJournalVersion || !validLowerHex(record.RunID, 32) ||
		record.ContainerName != recoveryContainerName(record.RunID) || !validRecoverySnapshotDir(record.SnapshotDir) ||
		!validLowerHex(record.SpecDigest, 64) {
		return false
	}
	switch record.State {
	case recoveryStatePrepared, recoveryStateCreating:
		return record.ContainerID == ""
	case recoveryStateOwned:
		return validLowerHex(record.ContainerID, 64)
	default:
		return false
	}
}

func validRecoverySnapshotDir(path string) bool {
	if path == "" || len(path) > maximumHostPathBytes || !utf8.ValidString(path) ||
		!filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) {
		return false
	}
	for _, character := range path {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func recoveryContainerName(runID string) string {
	if !validLowerHex(runID, 32) {
		return ""
	}
	return "purify-r6a-" + runID
}

func recoveryFilename(runID string) string {
	if !validLowerHex(runID, 32) {
		return ""
	}
	return runID + recoveryJournalSuffix
}

func validRecoveryFilename(name string) bool {
	return strings.HasSuffix(name, recoveryJournalSuffix) &&
		validLowerHex(strings.TrimSuffix(name, recoveryJournalSuffix), 32) &&
		name == recoveryFilename(strings.TrimSuffix(name, recoveryJournalSuffix))
}

func validRecoveryTemporaryFilename(name string) bool {
	if len(name) != 1+32+1+32+len(".tmp") || name[0] != '.' || !strings.HasSuffix(name, ".tmp") {
		return false
	}
	runID := name[1:33]
	nonce := name[34 : len(name)-len(".tmp")]
	return name[33] == '.' && validLowerHex(runID, 32) && validLowerHex(nonce, 32) &&
		name == "."+runID+"."+nonce+".tmp"
}

func recoveryTemporaryFilename(runID string) (string, error) {
	if !validLowerHex(runID, 32) {
		return "", errRecoveryJournal
	}
	nonce := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", errRecoveryJournal
	}
	return "." + runID + "." + hex.EncodeToString(nonce) + ".tmp", nil
}

func findRecoveryRecord(records []recoveryRecord, runID string) (recoveryRecord, bool) {
	index := sort.Search(len(records), func(index int) bool { return records[index].RunID >= runID })
	if index == len(records) || records[index].RunID != runID {
		return recoveryRecord{}, false
	}
	return records[index], true
}

func syncRecoveryDirectory(root *os.Root) error {
	if root == nil {
		return errRecoveryJournal
	}
	directory, err := root.Open(".")
	if err != nil {
		return errRecoveryJournal
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil || closeErr != nil {
		return errRecoveryJournal
	}
	return nil
}

func syncRecoveryParent(path string) error {
	parent, err := os.Open(filepath.Dir(path))
	if err != nil {
		return errRecoveryJournal
	}
	syncErr := parent.Sync()
	closeErr := parent.Close()
	if syncErr != nil || closeErr != nil {
		return errRecoveryJournal
	}
	return nil
}
