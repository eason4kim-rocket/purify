package dockerengine

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/use-agent/purify/search/rerank/deploy"
)

// recoveryPlanKey is non-secret placeholder material used only to rebuild the
// secret-redacted create-spec digest accepted by ResolveCreate. Recovery never
// sends this spec to Create or Start.
const recoveryPlanKey = "0000000000000000000000000000000000000000000000000000000000000000"

type recoveryDependencies struct {
	journal     *recoveryJournal
	engine      Engine
	cleanupHost func(HostPaths) error
}

// recoverReferenceSessions serially drains every durable recovery record.
// It never creates or starts a container. In particular, a missing
// deterministic name is not treated as proof that an earlier daemon Create
// handler cannot still reserve it later.
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
		current, err = recoverCreatingRecord(ctx, dependencies, plan, current)
		if err != nil {
			return err
		}
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

func recoverCreatingRecord(ctx context.Context, dependencies recoveryDependencies, plan *referencePlan, record recoveryRecord) (recoveryRecord, error) {
	inspection, err := dependencies.engine.ResolveCreate(ctx, plan.cloneCreateSpec())
	if err == nil {
		// The pinned Moby inspect path locks the registered container; Create's
		// register path holds that same lock through its durable checkpoint.
		// Therefore a successful exact inspection has crossed the only available
		// completion boundary. Absence has no symmetric meaning and stays pending.
		return persistResolvedOwner(dependencies.journal, plan, record, inspection)
	}
	if errors.Is(err, ErrOwnershipLost) {
		return recoveryRecord{}, ErrOwnershipLost
	}
	return recoveryRecord{}, recoveryCoordinatorError(ctx)
}

func persistResolvedOwner(journal *recoveryJournal, plan *referencePlan, current recoveryRecord, inspection OwnershipInspection) (recoveryRecord, error) {
	if journal == nil || plan == nil || !validLowerHex(inspection.ID, 64) {
		return recoveryRecord{}, ErrOwnershipLost
	}
	owner := ownership{
		containerID: inspection.ID, containerName: plan.create.Name,
		labels: ownershipLabels(plan.create.Labels),
	}
	if validateOwnershipInspection(owner, inspection) != nil {
		return recoveryRecord{}, ErrOwnershipLost
	}
	owned, err := journal.markOwned(current, inspection.ID)
	if err != nil {
		return recoveryRecord{}, ErrSessionUnavailable
	}
	return owned, nil
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
