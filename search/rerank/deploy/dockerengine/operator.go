package dockerengine

import (
	"sort"
	"strings"
)

const (
	// OperatorAdoptConfirmation is the exact typed confirmation for adopt.
	OperatorAdoptConfirmation = "ADOPT"
	// OperatorAbandonConfirmation is the exact typed confirmation for abandon.
	OperatorAbandonConfirmation = "ABANDON"
)

// OperatorInventoryRecord is the redacted view of one quarantined creating
// record. It never includes argv, environment, keys, or snapshot contents.
type OperatorInventoryRecord struct {
	RunID         string
	ContainerName string
	SpecDigest    string
	State         string
}

// OperatorJournal is the explicit human recovery surface. It never talks to
// Docker. Adopt only persists an operator-supplied container ID. Abandon only
// deletes the journal record after the operator has already handled the host.
type OperatorJournal struct {
	journal *recoveryJournal
}

// OpenOperatorJournal acquires the exclusive recovery lease on root.
func OpenOperatorJournal(root string, expectedUID int) (*OperatorJournal, error) {
	journal, err := prepareRecoveryJournal(root, expectedUID)
	if err != nil {
		return nil, errRecoveryJournal
	}
	return &OperatorJournal{journal: journal}, nil
}

// Close releases the exclusive recovery lease.
func (operator *OperatorJournal) Close() error {
	if operator == nil || operator.journal == nil {
		return errRecoveryJournal
	}
	err := operator.journal.close()
	operator.journal = nil
	return err
}

// List returns redacted creating records in run-id order.
func (operator *OperatorJournal) List() ([]OperatorInventoryRecord, error) {
	if operator == nil || operator.journal == nil {
		return nil, errRecoveryJournal
	}
	records, err := operator.journal.records()
	if err != nil {
		return nil, errRecoveryJournal
	}
	inventory := make([]OperatorInventoryRecord, 0, len(records))
	for _, record := range records {
		if record.State != recoveryStateCreating {
			continue
		}
		inventory = append(inventory, OperatorInventoryRecord{
			RunID:         strings.Clone(record.RunID),
			ContainerName: strings.Clone(record.ContainerName),
			SpecDigest:    strings.Clone(record.SpecDigest),
			State:         recoveryStateCreating,
		})
	}
	sort.Slice(inventory, func(left, right int) bool { return inventory[left].RunID < inventory[right].RunID })
	return inventory, nil
}

// Adopt persists operator-attested ownership of one creating record. The
// container ID must come from the operator's own inspect, not from name
// resolution inside this package.
func (operator *OperatorJournal) Adopt(runID, containerID, confirmation string) error {
	if operator == nil || operator.journal == nil || confirmation != OperatorAdoptConfirmation ||
		!validLowerHex(runID, 32) || !validLowerHex(containerID, 64) {
		return errRecoveryJournal
	}
	current, err := operator.creatingRecord(runID)
	if err != nil {
		return err
	}
	if _, err := operator.journal.markOwned(current, containerID); err != nil {
		return errRecoveryJournal
	}
	return nil
}

// Abandon deletes one creating journal record after the operator confirms the
// host child is already gone. This command never issues Docker APIs.
func (operator *OperatorJournal) Abandon(runID, confirmation string) error {
	if operator == nil || operator.journal == nil || confirmation != OperatorAbandonConfirmation ||
		!validLowerHex(runID, 32) {
		return errRecoveryJournal
	}
	current, err := operator.creatingRecord(runID)
	if err != nil {
		return err
	}
	if err := operator.journal.removeCreating(current); err != nil {
		return errRecoveryJournal
	}
	return nil
}

func (operator *OperatorJournal) creatingRecord(runID string) (recoveryRecord, error) {
	records, err := operator.journal.records()
	if err != nil {
		return recoveryRecord{}, errRecoveryJournal
	}
	stored, present := findRecoveryRecord(records, runID)
	if !present || stored.State != recoveryStateCreating || stored.ContainerID != "" {
		return recoveryRecord{}, errRecoveryJournal
	}
	return stored, nil
}
