package dockerengine

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

const testRecoverySpecDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
const testRecoverySnapshotDir = "/srv/purify/snapshots/qwen3-reranker"

func TestRecoveryJournalPersistsSecretFreeCanonicalPreparedCreatingOwnedState(t *testing.T) {
	journal, root := newTestRecoveryJournal(t)
	runID := strings.Repeat("1a", 16)
	prepared, err := journal.createPrepared(runID, testRecoverySnapshotDir, testRecoverySpecDigest)
	if err != nil {
		t.Fatalf("createPrepared() = %v", err)
	}
	wantPrepared := recoveryRecord{
		Version:       recoveryJournalVersion,
		State:         recoveryStatePrepared,
		RunID:         runID,
		ContainerName: recoveryContainerName(runID),
		SnapshotDir:   testRecoverySnapshotDir,
		SpecDigest:    testRecoverySpecDigest,
	}
	if prepared != wantPrepared {
		t.Fatalf("prepared = %#v, want %#v", prepared, wantPrepared)
	}
	assertRecoveryRootMode(t, root)
	path := filepath.Join(root, runID+recoveryJournalSuffix)
	assertRecoveryFile(t, path, prepared)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("secret-value-must-never-be-journaled", 2)
	if bytes.Contains(raw, []byte(secret)) || bytes.Contains(raw, []byte("api_key")) || bytes.Contains(raw, []byte("environment")) {
		t.Fatalf("journal contains a credential-bearing field: %q", raw)
	}

	creating, err := journal.markCreating(prepared)
	if err != nil {
		t.Fatalf("markCreating() = %v", err)
	}
	wantCreating := wantPrepared
	wantCreating.State = recoveryStateCreating
	if creating != wantCreating {
		t.Fatalf("creating = %#v, want %#v", creating, wantCreating)
	}
	assertRecoveryFile(t, path, creating)

	containerID := strings.Repeat("b7", 32)
	owned, err := journal.markOwned(creating, containerID)
	if err != nil {
		t.Fatalf("markOwned() = %v", err)
	}
	wantOwned := wantCreating
	wantOwned.State = recoveryStateOwned
	wantOwned.ContainerID = containerID
	if owned != wantOwned {
		t.Fatalf("owned = %#v, want %#v", owned, wantOwned)
	}
	assertRecoveryFile(t, path, owned)
	records, err := journal.records()
	if err != nil || !reflect.DeepEqual(records, []recoveryRecord{owned}) {
		t.Fatalf("records() = %#v, %v", records, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 2 || entries[0].Name() != recoveryLeaseFilename || entries[1].Name() != runID+recoveryJournalSuffix {
		t.Fatalf("directory entries = %#v, %v", entries, err)
	}
}

func TestRecoveryJournalHoldsExclusiveLeaseForItsLifetime(t *testing.T) {
	root := filepath.Join(canonicalTempDir(t), "recovery")
	first, err := prepareRecoveryJournal(root, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(root, recoveryLeaseFilename)
	info, err := os.Lstat(lockPath)
	if err != nil || !validRecoveryFileInfo(info, os.Geteuid(), 0) {
		t.Fatalf("lease info = %#v, %v", info, err)
	}
	if second, err := prepareRecoveryJournal(root, os.Geteuid()); !errors.Is(err, errRecoveryJournal) || second != nil {
		t.Fatalf("second prepareRecoveryJournal() = %#v, %v", second, err)
	}
	if err := first.close(); err != nil {
		t.Fatalf("first.close() = %v", err)
	}
	second, err := prepareRecoveryJournal(root, os.Geteuid())
	if err != nil {
		t.Fatalf("prepare after release = %v", err)
	}
	if err := second.close(); err != nil {
		t.Fatalf("second.close() = %v", err)
	}
}

func TestRecoveryJournalLeaseRemovesOnlySafeCanonicalCrashTemporaries(t *testing.T) {
	for name, size := range map[string]int{
		"zero":    0,
		"partial": 37,
		"maximum": maximumRecoveryJournalBytes,
	} {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(canonicalTempDir(t), "recovery")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			runID := strings.Repeat("7a", 16)
			temporary := "." + runID + "." + strings.Repeat("b", 32) + ".tmp"
			path := filepath.Join(root, temporary)
			if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, size), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
			journal, err := prepareRecoveryJournal(root, os.Geteuid())
			if err != nil {
				t.Fatalf("prepareRecoveryJournal() = %v", err)
			}
			t.Cleanup(func() { _ = journal.close() })
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("temporary remains: %v", err)
			}
			if records, err := journal.records(); err != nil || len(records) != 0 {
				t.Fatalf("records() = %#v, %v", records, err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 1 || entries[0].Name() != recoveryLeaseFilename {
				t.Fatalf("entries after cleanup = %#v, %v", entries, err)
			}
		})
	}
}

func TestRecoveryJournalLeaseRejectsUnsafeTemporaryWithoutRemovingIt(t *testing.T) {
	for name, setup := range map[string]func(*testing.T, string) string{
		"bad name": func(t *testing.T, root string) string {
			path := filepath.Join(root, ".bad.tmp")
			if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		},
		"uppercase nonce": func(t *testing.T, root string) string {
			path := filepath.Join(root, "."+strings.Repeat("8", 32)+"."+strings.Repeat("A", 32)+".tmp")
			if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		},
		"unsafe mode": func(t *testing.T, root string) string {
			path := validRecoveryTemporaryPath(root, "8b", "c")
			if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o640); err != nil {
				t.Fatal(err)
			}
			return path
		},
		"symlink": func(t *testing.T, root string) string {
			outside := filepath.Join(filepath.Dir(root), "outside")
			if err := os.WriteFile(outside, []byte("partial"), 0o600); err != nil {
				t.Fatal(err)
			}
			path := validRecoveryTemporaryPath(root, "8c", "d")
			if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}
			return path
		},
		"hardlink": func(t *testing.T, root string) string {
			path := validRecoveryTemporaryPath(root, "8d", "e")
			if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(path, filepath.Join(filepath.Dir(root), "outside-link")); err != nil {
				t.Fatal(err)
			}
			return path
		},
		"oversize": func(t *testing.T, root string) string {
			path := validRecoveryTemporaryPath(root, "8e", "f")
			if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, maximumRecoveryJournalBytes+1), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		},
		"directory": func(t *testing.T, root string) string {
			path := validRecoveryTemporaryPath(root, "8f", "0")
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			return path
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(canonicalTempDir(t), "recovery")
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			path := setup(t, root)
			if journal, err := prepareRecoveryJournal(root, os.Geteuid()); !errors.Is(err, errRecoveryJournal) || journal != nil {
				t.Fatalf("prepareRecoveryJournal() = %#v, %v", journal, err)
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatalf("unsafe temporary was removed: %v", err)
			}
		})
	}
}

func TestRecoveryJournalValidatesAllTemporariesBeforeRemovingAny(t *testing.T) {
	root := filepath.Join(canonicalTempDir(t), "recovery")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	valid := validRecoveryTemporaryPath(root, "91", "a")
	unsafe := validRecoveryTemporaryPath(root, "92", "b")
	if err := os.WriteFile(valid, []byte("valid partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unsafe, []byte("unsafe partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafe, 0o640); err != nil {
		t.Fatal(err)
	}
	if journal, err := prepareRecoveryJournal(root, os.Geteuid()); !errors.Is(err, errRecoveryJournal) || journal != nil {
		t.Fatalf("prepareRecoveryJournal() = %#v, %v", journal, err)
	}
	for _, path := range []string{valid, unsafe} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("temporary removed before batch validation: %s: %v", path, err)
		}
	}
}

func TestRecoveryJournalCleansTemporaryAtMaximumRecordBoundary(t *testing.T) {
	root := filepath.Join(canonicalTempDir(t), "recovery")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < maximumRecoveryJournalRecords; index++ {
		runID := fmt.Sprintf("%032x", index+1)
		record := recoveryRecord{
			Version:       recoveryJournalVersion,
			State:         recoveryStatePrepared,
			RunID:         runID,
			ContainerName: recoveryContainerName(runID),
			SnapshotDir:   testRecoverySnapshotDir,
			SpecDigest:    testRecoverySpecDigest,
		}
		raw, err := marshalRecoveryRecord(record)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, recoveryFilename(runID))
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	temporary := validRecoveryTemporaryPath(root, "aa", "c")
	if err := os.WriteFile(temporary, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	journal, err := prepareRecoveryJournal(root, os.Geteuid())
	if err != nil {
		t.Fatalf("prepareRecoveryJournal() = %v", err)
	}
	t.Cleanup(func() { _ = journal.close() })
	if _, err := os.Lstat(temporary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary remains: %v", err)
	}
	records, err := journal.records()
	if err != nil || len(records) != maximumRecoveryJournalRecords {
		t.Fatalf("records() count = %d, %v", len(records), err)
	}
}

func TestRecoveryJournalUnknownNonTemporaryStillFailsInventory(t *testing.T) {
	root := filepath.Join(canonicalTempDir(t), "recovery")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(root, "operator-note")
	if err := os.WriteFile(unknown, []byte("do not delete"), 0o600); err != nil {
		t.Fatal(err)
	}
	journal, err := prepareRecoveryJournal(root, os.Geteuid())
	if err != nil {
		t.Fatalf("prepareRecoveryJournal() = %v", err)
	}
	t.Cleanup(func() { _ = journal.close() })
	if records, err := journal.records(); !errors.Is(err, errRecoveryJournal) || records != nil {
		t.Fatalf("records() = %#v, %v", records, err)
	}
	if raw, err := os.ReadFile(unknown); err != nil || string(raw) != "do not delete" {
		t.Fatalf("unknown entry changed: %q, %v", raw, err)
	}
}

func validRecoveryTemporaryPath(root, runPair, nonceDigit string) string {
	return filepath.Join(root, "."+strings.Repeat(runPair, 16)+"."+strings.Repeat(nonceDigit, 32)+".tmp")
}

func TestRecoveryJournalEnforcesStateMachineAndExpectedRecord(t *testing.T) {
	journal, root := newTestRecoveryJournal(t)
	runID := strings.Repeat("2b", 16)
	prepared, err := journal.createPrepared(runID, testRecoverySnapshotDir, testRecoverySpecDigest)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, runID+recoveryJournalSuffix)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := journal.createPrepared(runID, testRecoverySnapshotDir, testRecoverySpecDigest); !errors.Is(err, errRecoveryJournal) {
		t.Fatalf("duplicate createPrepared() = %v", err)
	}
	mismatch := prepared
	mismatch.SpecDigest = strings.Repeat("f", 64)
	if _, err := journal.markOwned(mismatch, strings.Repeat("c", 64)); !errors.Is(err, errRecoveryJournal) {
		t.Fatalf("markOwned(mismatch) = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("failed transition changed journal: %v", err)
	}
	if _, err := journal.markOwned(prepared, strings.Repeat("c", 64)); !errors.Is(err, errRecoveryJournal) {
		t.Fatalf("prepared -> owned transition = %v", err)
	}
	creating, err := journal.markCreating(prepared)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.markCreating(creating); !errors.Is(err, errRecoveryJournal) {
		t.Fatalf("creating -> creating transition = %v", err)
	}
	if err := journal.remove(creating); !errors.Is(err, errRecoveryJournal) {
		t.Fatalf("remove(creating) = %v", err)
	}
	owned, err := journal.markOwned(creating, strings.Repeat("c", 64))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.markOwned(owned, strings.Repeat("d", 64)); !errors.Is(err, errRecoveryJournal) {
		t.Fatalf("owned -> owned transition = %v", err)
	}
	if err := journal.remove(mismatch); !errors.Is(err, errRecoveryJournal) {
		t.Fatalf("remove(mismatch) = %v", err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("mismatched remove deleted journal: %v", err)
	}
	if err := journal.remove(owned); err != nil {
		t.Fatalf("remove(owned) = %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal remains after remove: %v", err)
	}
	if err := journal.remove(owned); !errors.Is(err, errRecoveryJournal) {
		t.Fatalf("second remove = %v", err)
	}

	other, err := journal.createPrepared(strings.Repeat("3c", 16), testRecoverySnapshotDir, testRecoverySpecDigest)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.remove(other); err != nil {
		t.Fatalf("remove(intent) = %v", err)
	}
}

func TestRecoveryJournalRejectsNonCanonicalCorruptAndOversizeJSON(t *testing.T) {
	runID := strings.Repeat("4d", 16)
	valid := recoveryRecord{
		Version:       recoveryJournalVersion,
		State:         recoveryStatePrepared,
		RunID:         runID,
		ContainerName: recoveryContainerName(runID),
		SnapshotDir:   testRecoverySnapshotDir,
		SpecDigest:    testRecoverySpecDigest,
	}
	canonical, err := marshalRecoveryRecord(valid)
	if err != nil {
		t.Fatal(err)
	}
	withoutNewline := bytes.TrimSuffix(canonical, []byte{'\n'})
	unknownField := append(append([]byte(nil), canonical[:len(canonical)-2]...), []byte(",\"api_key\":\"do-not-store\"}\n")...)
	for name, raw := range map[string][]byte{
		"missing newline": withoutNewline,
		"trailing space":  append(append([]byte(nil), canonical...), ' '),
		"leading space":   append([]byte{' '}, canonical...),
		"unknown field":   unknownField,
		"duplicate field": bytes.Replace(canonical, []byte("\"version\":"), []byte("\"version\":\"purify-r6a-recovery-v1\",\"version\":"), 1),
		"wrong ordering":  []byte(`{"state":"prepared","version":"purify-r6a-recovery-v1","run_id":"` + runID + `","container_name":"` + recoveryContainerName(runID) + `","snapshot_dir":"` + testRecoverySnapshotDir + `","container_id":"","spec_digest":"` + testRecoverySpecDigest + `"}` + "\n"),
		"invalid state":   bytes.Replace(canonical, []byte(`"state":"prepared"`), []byte(`"state":"complete"`), 1),
		"oversize":        bytes.Repeat([]byte{'x'}, maximumRecoveryJournalBytes+1),
		"truncated":       canonical[:len(canonical)/2],
	} {
		t.Run(name, func(t *testing.T) {
			journal, root := newTestRecoveryJournal(t)
			path := filepath.Join(root, runID+recoveryJournalSuffix)
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if records, err := journal.records(); !errors.Is(err, errRecoveryJournal) || records != nil {
				t.Fatalf("records() = %#v, %v", records, err)
			}
			if _, err := journal.markCreating(valid); !errors.Is(err, errRecoveryJournal) {
				t.Fatalf("markCreating(corrupt) = %v", err)
			}
			if err := journal.remove(valid); !errors.Is(err, errRecoveryJournal) {
				t.Fatalf("remove(corrupt) = %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, raw) {
				t.Fatalf("corrupt record was mutated: %v", err)
			}
		})
	}
}

func TestRecoveryRecordRejectsNonCanonicalSnapshotPathsAndInvalidStateFields(t *testing.T) {
	runID := strings.Repeat("6f", 16)
	for name, snapshotDir := range map[string]string{
		"empty":        "",
		"relative":     "snapshots/model",
		"root":         "/",
		"unclean":      "/srv/purify/../model",
		"control":      "/srv/purify/model\nother",
		"over maximum": "/" + strings.Repeat("x", maximumHostPathBytes),
	} {
		t.Run(name, func(t *testing.T) {
			journal, _ := newTestRecoveryJournal(t)
			if record, err := journal.createPrepared(runID, snapshotDir, testRecoverySpecDigest); !errors.Is(err, errRecoveryJournal) || record != (recoveryRecord{}) {
				t.Fatalf("createPrepared() = %#v, %v", record, err)
			}
		})
	}
	valid := recoveryRecord{
		Version:       recoveryJournalVersion,
		State:         recoveryStatePrepared,
		RunID:         runID,
		ContainerName: recoveryContainerName(runID),
		SnapshotDir:   testRecoverySnapshotDir,
		SpecDigest:    testRecoverySpecDigest,
	}
	for name, mutate := range map[string]func(*recoveryRecord){
		"version":        func(record *recoveryRecord) { record.Version += "-unknown" },
		"container name": func(record *recoveryRecord) { record.ContainerName += "-other" },
		"prepared id":    func(record *recoveryRecord) { record.ContainerID = strings.Repeat("a", 64) },
		"owned empty id": func(record *recoveryRecord) { record.State = recoveryStateOwned },
		"creating id": func(record *recoveryRecord) {
			record.State = recoveryStateCreating
			record.ContainerID = strings.Repeat("b", 64)
		},
	} {
		t.Run(name, func(t *testing.T) {
			record := valid
			mutate(&record)
			if validRecoveryRecord(record) {
				t.Fatalf("invalid record accepted: %#v", record)
			}
		})
	}
}

func TestRecoveryJournalRejectsUnsafeDirectoryAndFileObjects(t *testing.T) {
	t.Run("directory mode", func(t *testing.T) {
		parent := canonicalTempDir(t)
		root := filepath.Join(parent, "journal")
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if journal, err := prepareRecoveryJournal(root, os.Geteuid()); !errors.Is(err, errRecoveryJournal) || journal != nil {
			t.Fatalf("prepareRecoveryJournal() = %#v, %v", journal, err)
		}
	})
	t.Run("directory symlink", func(t *testing.T) {
		parent := canonicalTempDir(t)
		real := filepath.Join(parent, "real")
		alias := filepath.Join(parent, "alias")
		if err := os.Mkdir(real, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, alias); err != nil {
			t.Fatal(err)
		}
		if journal, err := prepareRecoveryJournal(alias, os.Geteuid()); !errors.Is(err, errRecoveryJournal) || journal != nil {
			t.Fatalf("prepareRecoveryJournal() = %#v, %v", journal, err)
		}
	})
	t.Run("ancestor symlink", func(t *testing.T) {
		parent := canonicalTempDir(t)
		real := filepath.Join(parent, "real")
		alias := filepath.Join(parent, "alias")
		if err := os.Mkdir(real, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, alias); err != nil {
			t.Fatal(err)
		}
		if journal, err := prepareRecoveryJournal(filepath.Join(alias, "journal"), os.Geteuid()); !errors.Is(err, errRecoveryJournal) || journal != nil {
			t.Fatalf("prepareRecoveryJournal() = %#v, %v", journal, err)
		}
	})
	t.Run("writable ancestor", func(t *testing.T) {
		parent := canonicalTempDir(t)
		writable := filepath.Join(parent, "writable")
		if err := os.Mkdir(writable, 0o770); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(writable, 0o770); err != nil {
			t.Fatal(err)
		}
		if journal, err := prepareRecoveryJournal(filepath.Join(writable, "journal"), os.Geteuid()); !errors.Is(err, errRecoveryJournal) || journal != nil {
			t.Fatalf("prepareRecoveryJournal() = %#v, %v", journal, err)
		}
	})
	t.Run("owner mismatch", func(t *testing.T) {
		journal, _ := newTestRecoveryJournal(t)
		journal.expectedUID++
		if records, err := journal.records(); !errors.Is(err, errRecoveryJournal) || records != nil {
			t.Fatalf("records() = %#v, %v", records, err)
		}
	})

	for name, mutate := range map[string]func(t *testing.T, path, root string){
		"file mode": func(t *testing.T, path, _ string) {
			if err := os.Chmod(path, 0o640); err != nil {
				t.Fatal(err)
			}
		},
		"file symlink": func(t *testing.T, path, root string) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(filepath.Dir(root), "outside")
			if err := os.WriteFile(outside, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}
		},
		"file hardlink": func(t *testing.T, path, root string) {
			if err := os.Link(path, filepath.Join(root, "second-link")); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			journal, root := newTestRecoveryJournal(t)
			record, err := journal.createPrepared(strings.Repeat("5e", 16), testRecoverySnapshotDir, testRecoverySpecDigest)
			if err != nil {
				t.Fatal(err)
			}
			mutate(t, filepath.Join(root, record.RunID+recoveryJournalSuffix), root)
			if records, err := journal.records(); !errors.Is(err, errRecoveryJournal) || records != nil {
				t.Fatalf("records() = %#v, %v", records, err)
			}
		})
	}
}

func TestRecoveryJournalInventoryIsExactBoundedAndSorted(t *testing.T) {
	journal, root := newTestRecoveryJournal(t)
	second, err := journal.createPrepared(strings.Repeat("b", 32), testRecoverySnapshotDir, testRecoverySpecDigest)
	if err != nil {
		t.Fatal(err)
	}
	first, err := journal.createPrepared(strings.Repeat("a", 32), testRecoverySnapshotDir, testRecoverySpecDigest)
	if err != nil {
		t.Fatal(err)
	}
	records, err := journal.records()
	if err != nil || !reflect.DeepEqual(records, []recoveryRecord{first, second}) {
		t.Fatalf("records() = %#v, %v", records, err)
	}
	if err := os.WriteFile(filepath.Join(root, ".leftover.tmp"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if records, err := journal.records(); !errors.Is(err, errRecoveryJournal) || records != nil {
		t.Fatalf("records(with extra) = %#v, %v", records, err)
	}
}

func TestRecoveryJournalRejectsRecordLimitBeforeWriting(t *testing.T) {
	journal, root := newTestRecoveryJournal(t)
	for index := 0; index < maximumRecoveryJournalRecords; index++ {
		runID := fmt.Sprintf("%032x", index+1)
		if _, err := journal.createPrepared(runID, testRecoverySnapshotDir, testRecoverySpecDigest); err != nil {
			t.Fatalf("createPrepared(%d) = %v", index, err)
		}
	}
	before, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	overflowID := strings.Repeat("f", 32)
	if _, err := journal.createPrepared(overflowID, testRecoverySnapshotDir, testRecoverySpecDigest); !errors.Is(err, errRecoveryJournal) {
		t.Fatalf("overflow createPrepared() = %v", err)
	}
	after, err := os.ReadDir(root)
	if err != nil || len(after) != len(before) {
		t.Fatalf("entries after overflow = %d, want %d, err %v", len(after), len(before), err)
	}
	if _, err := os.Lstat(filepath.Join(root, recoveryFilename(overflowID))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("overflow record reached disk: %v", err)
	}
	records, err := journal.records()
	if err != nil || len(records) != maximumRecoveryJournalRecords {
		t.Fatalf("records() = %d, %v", len(records), err)
	}
}

func TestRecoveryJournalSerializesConcurrentDistinctIntents(t *testing.T) {
	journal, _ := newTestRecoveryJournal(t)
	const count = 8
	var wait sync.WaitGroup
	errorsSeen := make(chan error, count)
	for index := 0; index < count; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			runID := strings.Repeat(string("01234567"[index]), 32)
			_, err := journal.createPrepared(runID, testRecoverySnapshotDir, testRecoverySpecDigest)
			errorsSeen <- err
		}(index)
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("createPrepared() = %v", err)
		}
	}
	records, err := journal.records()
	if err != nil || len(records) != count {
		t.Fatalf("records() count = %d, %v", len(records), err)
	}
}

func newTestRecoveryJournal(t *testing.T) (*recoveryJournal, string) {
	t.Helper()
	root := filepath.Join(canonicalTempDir(t), "recovery")
	journal, err := prepareRecoveryJournal(root, os.Geteuid())
	if err != nil {
		t.Fatalf("prepareRecoveryJournal() = %v", err)
	}
	t.Cleanup(func() {
		if journal.lease != nil {
			_ = journal.close()
		}
	})
	return journal, root
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func assertRecoveryRootMode(t *testing.T, root string) {
	t.Helper()
	info, err := os.Lstat(root)
	if err != nil || info.Mode() != os.ModeDir|0o700 || fileUID(info) != os.Geteuid() {
		t.Fatalf("root info = %#v, %v", info, err)
	}
}

func assertRecoveryFile(t *testing.T, path string, record recoveryRecord) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 ||
		fileUID(info) != os.Geteuid() || fileLinkCount(info) != 1 {
		t.Fatalf("journal info = %#v, %v", info, err)
	}
	want, err := marshalRecoveryRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("journal bytes = %q, want %q, err %v", got, want, err)
	}
}
