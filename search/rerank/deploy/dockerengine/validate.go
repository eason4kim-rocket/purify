package dockerengine

import (
	"crypto/subtle"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
)

func ValidateAdmission(controller ControllerInspection, daemon DaemonInspection) error {
	if controller.Endpoint != DockerSocketPath || controller.GOOS != "linux" || controller.GOARCH != "amd64" ||
		!controller.EffectiveUIDKnown || controller.EffectiveUID != 0 ||
		daemon.OSType != "linux" || daemon.Architecture != "amd64" || !daemon.Rootful ||
		daemon.UserNamespaceMode != UserNamespaceDisabled || !atLeastAPIVersion(daemon.APIVersion, MinimumEngineAPIVersion) {
		return ErrAdmissionRejected
	}
	return nil
}

func ValidateReferenceImage(inspection ImageInspection) error {
	if inspection.RequestedReference != ReferenceImageReference || inspection.ID != ReferenceImageManifestDigest ||
		inspection.OS != "linux" || inspection.Architecture != "amd64" ||
		!validManifestDescriptor(inspection.ManifestDescriptor) ||
		!reflect.DeepEqual(inspection.Config.Entrypoint, []string{"vllm", "serve"}) ||
		inspection.Config.CmdPresent || len(inspection.Config.Cmd) != 0 ||
		!reflect.DeepEqual(inspection.Config.Environment, deployReferenceImageEnvironment()) ||
		inspection.Config.User != "" || inspection.Config.WorkingDir != "/vllm-workspace" ||
		len(inspection.Config.ExposedPorts) != 0 || len(inspection.Config.Volumes) != 0 ||
		!reflect.DeepEqual(inspection.Config.Labels, referenceImageLabels()) {
		return ErrImageRejected
	}
	return nil
}

func validateReferenceArchivePath(path string) error {
	if path != ReferenceSnapshotPath && path != ReferenceTemplatePath {
		return ErrAdmissionRejected
	}
	return nil
}

type ownership struct {
	containerID   string
	containerName string
	labels        map[string]string
}

func (plan *referencePlan) establishOwnership(result CreateResult) (ownership, error) {
	if !validLowerHex(result.ContainerID, 64) || len(result.Warnings) != 0 {
		return ownership{}, ErrContainerRejected
	}
	return ownership{
		containerID:   result.ContainerID,
		containerName: plan.create.Name,
		labels:        ownershipLabels(plan.create.Labels),
	}, nil
}

func validateCreated(plan *referencePlan, owner ownership, inspection ContainerInspection) error {
	if err := validateOwnership(owner, inspection.ID, inspection.Labels); err != nil {
		return err
	}
	if !validateContainerStatic(plan, inspection) || inspection.RestartCount != 0 || !createdState(inspection.State) {
		return ErrContainerRejected
	}
	return nil
}

func validateRunning(plan *referencePlan, owner ownership, inspection RunningInspection) error {
	if err := validateOwnership(owner, inspection.Before.ID, inspection.Before.Labels); err != nil {
		return err
	}
	if err := validateOwnership(owner, inspection.After.ID, inspection.After.Labels); err != nil {
		return err
	}
	if !validateContainerStatic(plan, inspection.Before) || !validateContainerStatic(plan, inspection.After) ||
		inspection.Before.RestartCount != 0 || inspection.After.RestartCount != 0 ||
		!runningState(inspection.Before.State) || !runningState(inspection.After.State) ||
		inspection.Before.State.PID != inspection.After.State.PID || inspection.Before.State.PID != inspection.Process.PID ||
		!equalProcessEnvironment(plan, inspection.Process.Environment) ||
		inspection.Socket.Path != filepath.Join(plan.create.Mounts[2].Source, "vllm.sock") ||
		!inspection.Socket.IsSocket || inspection.Socket.IsSymlink || !inspection.Socket.Fresh {
		return ErrContainerRejected
	}
	return nil
}

func validateFinal(plan *referencePlan, owner ownership, inspection ContainerInspection) error {
	if err := validateOwnership(owner, inspection.ID, inspection.Labels); err != nil {
		return err
	}
	if !validateContainerStatic(plan, inspection) || inspection.RestartCount != 0 || !exitedState(inspection.State) {
		return ErrContainerRejected
	}
	return nil
}

func validateContainerStatic(plan *referencePlan, inspection ContainerInspection) bool {
	spec := plan.create
	argv := append(append([]string(nil), spec.Entrypoint...), spec.Command...)
	return inspection.Name == "/"+spec.Name &&
		inspection.ImageID == ReferenceImageManifestDigest && inspection.ConfiguredImage == ReferenceImageReference &&
		validManifestDescriptor(inspection.ImageManifestDescriptor) && inspection.Platform == plan.descriptor.Platform &&
		inspection.Path == argv[0] && reflect.DeepEqual(inspection.Args, argv[1:]) &&
		inspection.Hostname == spec.Hostname && reflect.DeepEqual(inspection.Entrypoint, spec.Entrypoint) &&
		reflect.DeepEqual(inspection.Command, spec.Command) && equalConfigEnvironment(spec.Environment, inspection.Environment) &&
		inspection.User == spec.User && inspection.WorkingDir == spec.WorkingDir &&
		reflect.DeepEqual(inspection.Labels, spec.Labels) && reflect.DeepEqual(inspection.Mounts, spec.Mounts) &&
		reflect.DeepEqual(inspection.Tmpfs, spec.Tmpfs) && reflect.DeepEqual(inspection.DeviceRequests, spec.DeviceRequests) &&
		inspection.NetworkMode == NetworkModeNone && inspection.NetworkDisabled && inspection.IPCMode == IPCModePrivate &&
		inspection.ShmSizeBytes == spec.ShmSizeBytes && inspection.ReadOnlyRootFS &&
		inspection.RestartPolicy == RestartPolicyNo && inspection.LogDriver == LogDriverNone && !inspection.Privileged && !inspection.TTY &&
		len(inspection.ExposedPorts) == 0 && len(inspection.PortBindings) == 0 &&
		len(inspection.CapAdd) == 0 && len(inspection.Links) == 0
}

func validateOwnership(owner ownership, containerID string, labels map[string]string) error {
	if owner.containerID == "" || containerID != owner.containerID || !reflect.DeepEqual(ownershipLabels(labels), owner.labels) {
		return ErrOwnershipLost
	}
	return nil
}

func validateOwnershipInspection(owner ownership, inspection OwnershipInspection) error {
	if owner.containerName == "" || inspection.Name != "/"+owner.containerName {
		return ErrOwnershipLost
	}
	return validateOwnership(owner, inspection.ID, inspection.Labels)
}

func ownershipLabels(labels map[string]string) map[string]string {
	projected := make(map[string]string, 4)
	for _, name := range []string{LabelManaged, LabelComponent, LabelRunID, LabelSpecDigest} {
		if value, present := labels[name]; present {
			projected[name] = value
		}
	}
	return projected
}

func advanceLifecycle(state LifecycleState, owner ownership, event LifecycleEvent) (LifecycleState, error) {
	if err := validateOwnership(owner, event.ContainerID, event.Labels); err != nil {
		return state, err
	}
	var next LifecycleState
	switch {
	case state == LifecycleNew && event.Action == LifecycleActionCreate:
		next = LifecycleCreated
	case state == LifecycleCreated && event.Action == LifecycleActionStart:
		next = LifecycleRunning
	case state == LifecycleCreated && event.Action == LifecycleActionDestroy:
		next = LifecycleRemoved
	case state == LifecycleRunning && event.Action == LifecycleActionKill:
		next = LifecycleStopping
	case state == LifecycleRunning && event.Action == LifecycleActionDie:
		next = LifecycleExited
	case state == LifecycleStopping && event.Action == LifecycleActionDie:
		next = LifecycleExited
	case state == LifecycleExited && event.Action == LifecycleActionDestroy:
		next = LifecycleRemoved
	default:
		return state, ErrLifecycleDrift
	}
	return next, nil
}

func atLeastAPIVersion(value, minimum string) bool {
	major, minor, ok := parseAPIVersion(value)
	if !ok {
		return false
	}
	minimumMajor, minimumMinor, ok := parseAPIVersion(minimum)
	if !ok {
		return false
	}
	return major > minimumMajor || major == minimumMajor && minor >= minimumMinor
}

func parseAPIVersion(value string) (int, int, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 || !canonicalDecimal(parts[0]) || !canonicalDecimal(parts[1]) {
		return 0, 0, false
	}
	major, majorError := strconv.Atoi(parts[0])
	minor, minorError := strconv.Atoi(parts[1])
	if majorError != nil || minorError != nil {
		return 0, 0, false
	}
	return major, minor, true
}

func canonicalDecimal(value string) bool {
	if value == "" || len(value) > 9 || len(value) > 1 && value[0] == '0' {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func validManifestDescriptor(descriptor *ManifestDescriptor) bool {
	return descriptor != nil && descriptor.Digest == deployImageDigest() && descriptor.MediaType == ReferenceManifestMediaType
}

func deployImageDigest() string {
	// Keeping the comparison behind this small function makes it impossible for
	// callers to supply an expected digest through an inspection DTO.
	return ReferenceImageManifestDigest
}

func deployReferenceImageEnvironment() []string { return referenceImageEnvironment() }

func createdState(state ContainerState) bool {
	return state.Status == ContainerStatusCreated && !state.Running && !state.Paused && !state.Restarting &&
		!state.Dead && !state.OOMKilled && state.PID == 0 && state.ExitCode == 0
}

func runningState(state ContainerState) bool {
	return state.Status == ContainerStatusRunning && state.Running && !state.Paused && !state.Restarting &&
		!state.Dead && !state.OOMKilled && state.PID > 0 && state.ExitCode == 0
}

func exitedState(state ContainerState) bool {
	return state.Status == ContainerStatusExited && !state.Running && !state.Paused && !state.Restarting &&
		!state.Dead && !state.OOMKilled && state.PID == 0
}

func equalConfigEnvironment(expected, actual []string) bool {
	if len(expected) != len(actual) {
		return false
	}
	for index := range expected {
		expectedName, expectedValue, expectedOK := strings.Cut(expected[index], "=")
		actualName, actualValue, actualOK := strings.Cut(actual[index], "=")
		if !expectedOK || !actualOK || expectedName != actualName {
			return false
		}
		if expectedName == "VLLM_API_KEY" {
			if len(expectedValue) != len(actualValue) || subtle.ConstantTimeCompare([]byte(expectedValue), []byte(actualValue)) != 1 {
				return false
			}
		} else if expectedValue != actualValue {
			return false
		}
	}
	return true
}

func equalProcessEnvironment(plan *referencePlan, actual []string) bool {
	// Moby starts with PATH and HOSTNAME, then applies Config.Env with
	// ReplaceOrAppendEnvValues. The pinned Config.Env PATH therefore replaces the
	// first slot in place, HOSTNAME remains second, and all other Config.Env entries
	// retain their order after it. The legacy NVIDIA driver then appends two entries.
	// They intentionally duplicate names present in the image baseline. Their
	// envp position is security-relevant because getenv consumers resolve
	// duplicate names by order, so the complete sequence is compared exactly.
	expected := make([]string, 0, len(plan.create.Environment)+3)
	pathIndex := -1
	for index, entry := range plan.create.Environment {
		name, _, ok := strings.Cut(entry, "=")
		if ok && name == "PATH" {
			if pathIndex >= 0 {
				return false
			}
			pathIndex = index
			expected = append(expected, entry)
		}
	}
	if pathIndex < 0 {
		return false
	}
	expected = append(expected, "HOSTNAME="+plan.create.Hostname)
	for index, entry := range plan.create.Environment {
		if index != pathIndex {
			expected = append(expected, entry)
		}
	}
	expected = append(expected,
		"NVIDIA_VISIBLE_DEVICES=0",
		"NVIDIA_DRIVER_CAPABILITIES=compute",
	)
	return equalConfigEnvironment(expected, actual)
}
