package dockerengine

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestOperatorJournalListsOnlyCreatingAndAdoptsSuppliedID(t *testing.T) {
	journal, root := newTestRecoveryJournal(t)
	if err := journal.close(); err != nil {
		t.Fatal(err)
	}
	operator, err := OpenOperatorJournal(root, os.Geteuid())
	if err != nil {
		t.Fatalf("OpenOperatorJournal() = %v", err)
	}
	t.Cleanup(func() { _ = operator.Close() })

	creating := seedOperatorCreating(t, operator, strings.Repeat("aa", 16))
	preparedRun := strings.Repeat("bb", 16)
	if _, err := operator.journal.createPrepared(preparedRun, testRecoverySnapshotDir, testRecoverySpecDigest); err != nil {
		t.Fatal(err)
	}

	inventory, err := operator.List()
	if err != nil {
		t.Fatalf("List() = %v", err)
	}
	if !reflect.DeepEqual(inventory, []OperatorInventoryRecord{{
		RunID: creating.RunID, ContainerName: creating.ContainerName,
		SpecDigest: creating.SpecDigest, State: recoveryStateCreating,
	}}) {
		t.Fatalf("List() = %#v", inventory)
	}
	for _, record := range inventory {
		if record.RunID == "" || strings.Contains(record.SpecDigest, "secret") || record.State != recoveryStateCreating {
			t.Fatalf("leaked inventory %#v", record)
		}
	}

	containerID := strings.Repeat("cd", 32)
	if err := operator.Adopt(creating.RunID, containerID, OperatorAdoptConfirmation); err != nil {
		t.Fatalf("Adopt() = %v", err)
	}
	records, err := operator.journal.records()
	if err != nil || len(records) != 2 {
		t.Fatalf("records after adopt = %#v, %v", records, err)
	}
	owned, present := findRecoveryRecord(records, creating.RunID)
	if !present || owned.State != recoveryStateOwned || owned.ContainerID != containerID {
		t.Fatalf("adopted record = %#v", owned)
	}
	if inventory, err := operator.List(); err != nil || len(inventory) != 0 {
		t.Fatalf("List after adopt = %#v, %v", inventory, err)
	}
}

func TestOperatorJournalAbandonDeletesCreatingRecordWithoutDocker(t *testing.T) {
	journal, root := newTestRecoveryJournal(t)
	if err := journal.close(); err != nil {
		t.Fatal(err)
	}
	operator, err := OpenOperatorJournal(root, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = operator.Close() })
	creating := seedOperatorCreating(t, operator, strings.Repeat("11", 16))

	if err := operator.Abandon(creating.RunID, OperatorAbandonConfirmation); err != nil {
		t.Fatalf("Abandon() = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, creating.RunID+recoveryJournalSuffix)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("creating record still on disk: %v", err)
	}
	if records, err := operator.journal.records(); err != nil || len(records) != 0 {
		t.Fatalf("records after abandon = %#v, %v", records, err)
	}
}

func TestOperatorJournalRejectsMissingConfirmationAndNonCreatingStates(t *testing.T) {
	journal, root := newTestRecoveryJournal(t)
	if err := journal.close(); err != nil {
		t.Fatal(err)
	}
	operator, err := OpenOperatorJournal(root, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = operator.Close() })
	creating := seedOperatorCreating(t, operator, strings.Repeat("22", 16))
	preparedRun := strings.Repeat("33", 16)
	if _, err := operator.journal.createPrepared(preparedRun, testRecoverySnapshotDir, testRecoverySpecDigest); err != nil {
		t.Fatal(err)
	}

	if err := operator.Adopt(creating.RunID, strings.Repeat("ee", 32), "adopt"); err == nil {
		t.Fatal("Adopt accepted a lowercase confirmation")
	}
	if err := operator.Abandon(creating.RunID, "abandon"); err == nil {
		t.Fatal("Abandon accepted a lowercase confirmation")
	}
	if err := operator.Adopt(preparedRun, strings.Repeat("ee", 32), OperatorAdoptConfirmation); err == nil {
		t.Fatal("Adopt accepted a prepared record")
	}
	if err := operator.Abandon(preparedRun, OperatorAbandonConfirmation); err == nil {
		t.Fatal("Abandon accepted a prepared record")
	}
	if err := operator.Adopt(creating.RunID, "not-a-container-id", OperatorAdoptConfirmation); err == nil {
		t.Fatal("Adopt accepted a non-hex container ID")
	}
	if err := journal.remove(creating); err == nil {
		t.Fatal("automatic remove accepted creating after operator API existed")
	}
}

func seedOperatorCreating(t *testing.T, operator *OperatorJournal, runID string) recoveryRecord {
	t.Helper()
	prepared, err := operator.journal.createPrepared(runID, testRecoverySnapshotDir, testRecoverySpecDigest)
	if err != nil {
		t.Fatal(err)
	}
	creating, err := operator.journal.markCreating(prepared)
	if err != nil {
		t.Fatal(err)
	}
	return creating
}
