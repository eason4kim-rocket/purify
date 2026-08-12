package dockerengine

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/use-agent/purify/search/rerank/deploy"
)

// recoveryPlanKey is non-secret placeholder material used only to rebuild the
// secret-redacted create-spec digest. Recovery never sends this spec to Create
// or Start.
const recoveryPlanKey = "0000000000000000000000000000000000000000000000000000000000000000"

type recoveryDependencies struct {
	journal     *recoveryJournal
	engine      Engine
	cleanupHost func(HostPaths) error
}

// recoverReferenceSessions serially drains every durable recovery record.
// It never resolves, creates, or starts a container whose durable record is
// still creating. Docker's public API supplies no completion boundary that can
// safely promote an ambiguous Create response after controller restart.
func recoverReferenceSessions(ctx context.Context, dependencies recoveryDependencies) error {
	if ctx == nil || ctx.Err() != nil || dependencies.journal == nil || dependencies.engine == nil ||
		dependencies.cleanupHost == nil {
		return recoveryCoordinatorError(ctx)
	}
	records, err := dependencies.journal.records()
	if err != nil {
		return ErrSessionUnavailable
	}
	for _, record := range records {
		if err := recoverReferenceRecord(ctx, dependencies, record); err != nil {
			return err
		}
	}
	return nil
}

func recoverReferenceRecord(ctx context.Context, dependencies recoveryDependencies, record recoveryRecord) error {
	if ctx == nil || ctx.Err() != nil || !validRecoveryRecord(record) {
		return recoveryCoordinatorError(ctx)
	}
	paths := recoveryHostPaths(record)
	if !validHostPaths(paths) || paths.SnapshotDir != record.SnapshotDir {
		return ErrSessionUnavailable
	}
	if record.State == recoveryStatePrepared {
		if err := dependencies.cleanupHost(paths); err != nil || dependencies.journal.remove(record) != nil {
			return ErrSessionUnavailable
		}
		return nil
	}
	plan, err := buildReferencePlan(deploy.ReferenceDescriptor(), paths, generatedInputs{RunID: record.RunID, APIKey: recoveryPlanKey})
	if err != nil || plan.specDigest != record.SpecDigest || plan.create.Name != record.ContainerName ||
		plan.create.Hostname != record.ContainerName {
		return ErrSessionUnavailable
	}
	current := record
	if current.State == recoveryStateCreating {
		return ErrSessionUnavailable
	}
	if current.State != recoveryStateOwned {
		return ErrSessionUnavailable
	}
	owner := ownership{
		containerID: current.ContainerID, containerName: current.ContainerName,
		labels: ownershipLabels(plan.create.Labels),
	}
	if err := cleanupOwnedUntilSettled(ctx, dependencies.engine, plan, owner); err != nil {
		if errors.Is(err, ErrOwnershipLost) {
			return ErrOwnershipLost
		}
		return recoveryCoordinatorError(ctx)
	}
	if err := dependencies.cleanupHost(paths); err != nil || dependencies.journal.remove(current) != nil {
		return ErrSessionUnavailable
	}
	return nil
}

func recoveryHostPaths(record recoveryRecord) HostPaths {
	return HostPaths{
		SnapshotDir:  record.SnapshotDir,
		TemplateFile: filepath.Join(localTemplateRoot, record.RunID, localTemplateName),
		RunDir:       filepath.Join(localRunRoot, record.RunID),
	}
}

func recoveryCoordinatorError(ctx context.Context) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return ErrSessionUnavailable
}
