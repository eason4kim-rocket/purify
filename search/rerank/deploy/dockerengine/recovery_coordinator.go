package dockerengine

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"time"

	"github.com/use-agent/purify/search/rerank/deploy"
)

const recoveryRetryInterval = 10 * time.Millisecond

type recoveryDependencies struct {
	journal     *recoveryJournal
	engine      Engine
	random      io.Reader
	cleanupHost func(HostPaths) error
	// prepareCreateHost verifies an intact recovery host view or safely
	// reconstructs the derived /run directories/template after reboot.
	prepareCreateHost func(context.Context, HostPaths) error
}

// recoverReferenceSessions serially drains every durable recovery record.
// It never starts a container: a same-name Create is only a name-reservation
// fence used to regain exact cleanup authority after an ambiguous response.
func recoverReferenceSessions(ctx context.Context, dependencies recoveryDependencies) error {
	if ctx == nil || ctx.Err() != nil || dependencies.journal == nil || dependencies.engine == nil ||
		dependencies.random == nil || dependencies.cleanupHost == nil || dependencies.prepareCreateHost == nil {
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
	_, apiKey, ok := generateLocalSessionSecrets(dependencies.random)
	if !ok {
		return ErrSessionUnavailable
	}
	plan, err := buildReferencePlan(deploy.ReferenceDescriptor(), paths, generatedInputs{RunID: record.RunID, APIKey: apiKey})
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
	hostPrepared := false
	for {
		inspection, err := dependencies.engine.ResolveCreate(ctx, plan.cloneCreateSpec())
		if err == nil {
			return persistResolvedOwner(dependencies.journal, plan, record, inspection)
		}
		if errors.Is(err, ErrOwnershipLost) {
			return recoveryRecord{}, ErrOwnershipLost
		}
		if !errors.Is(err, errContainerNotFound) {
			if waitErr := waitForRecoveryRetry(ctx); waitErr != nil {
				return recoveryRecord{}, waitErr
			}
			continue
		}

		// Resolve 404 is not terminal: the original daemon handler may not have
		// reserved its name yet. Reissuing the exact same-name Create is the
		// fence: it either creates/reserves the name itself or conflicts with the
		// original request. Neither outcome grants authority without Resolve.
		if !hostPrepared {
			if err := dependencies.prepareCreateHost(ctx, recoveryHostPaths(record)); err != nil || ctx.Err() != nil {
				return recoveryRecord{}, recoveryCoordinatorError(ctx)
			}
			hostPrepared = true
		}
		result, createErr := dependencies.engine.Create(ctx, plan.cloneCreateSpec())
		if validLowerHex(result.ContainerID, 64) {
			inspection, resolveErr := dependencies.engine.ResolveCreate(ctx, plan.cloneCreateSpec())
			if resolveErr == nil {
				if inspection.ID != result.ContainerID {
					return recoveryRecord{}, ErrOwnershipLost
				}
				return persistResolvedOwner(dependencies.journal, plan, record, inspection)
			}
			if errors.Is(resolveErr, ErrOwnershipLost) {
				return recoveryRecord{}, ErrOwnershipLost
			}
		}
		if createErr == nil && !validLowerHex(result.ContainerID, 64) {
			return recoveryRecord{}, ErrSessionUnavailable
		}
		if createErr != nil && !errors.Is(createErr, errContainerConflict) && !errors.Is(createErr, ErrEngineOperation) {
			return recoveryRecord{}, ErrSessionUnavailable
		}
		if waitErr := waitForRecoveryRetry(ctx); waitErr != nil {
			return recoveryRecord{}, waitErr
		}
	}
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

func waitForRecoveryRetry(ctx context.Context) error {
	if ctx == nil {
		return ErrSessionUnavailable
	}
	timer := time.NewTimer(recoveryRetryInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func recoveryCoordinatorError(ctx context.Context) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return ErrSessionUnavailable
}
