package dockerengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/mount"
	networktypes "github.com/moby/moby/api/types/network"
	mobyclient "github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/use-agent/purify/search/rerank/deploy"
)

const mobyHost = "unix://" + DockerSocketPath

// newMobyEngine constructs the only production Docker adapter. It deliberately does not
// use FromEnv: Docker host, context, TLS, proxy, and API-version environment
// variables cannot redirect this client away from the local Unix socket.
func newMobyEngine() (Engine, error) {
	client, err := mobyclient.New(
		mobyclient.WithHost(mobyHost),
		mobyclient.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, operationError("new client", err)
	}
	return &mobyEngine{client: client}, nil
}

type mobyEngine struct {
	client           *mobyclient.Client
	eventMu          sync.Mutex
	expectedArchives map[string]*archiveExpectation
}

type archiveExpectation struct {
	remaining int
	labels    map[string]string
	done      chan struct{}
}

var _ Engine = (*mobyEngine)(nil)

func (engine *mobyEngine) Close() error {
	if engine == nil || engine.client == nil {
		return nil
	}
	if err := engine.client.Close(); err != nil {
		return operationError("close client", err)
	}
	return nil
}

func (engine *mobyEngine) InspectDaemon(ctx context.Context) (DaemonInspection, error) {
	version, err := engine.client.ServerVersion(ctx, mobyclient.ServerVersionOptions{})
	if err != nil {
		return DaemonInspection{}, operationError("inspect daemon version", err)
	}
	info, err := engine.client.Info(ctx, mobyclient.InfoOptions{})
	if err != nil {
		return DaemonInspection{}, operationError("inspect daemon info", err)
	}
	architecture, ok := normalizeDaemonArchitecture(version.Arch, info.Info.Architecture)
	if version.Os != info.Info.OSType || !ok {
		return DaemonInspection{}, operationError("inspect daemon identity", errors.New("inconsistent response"))
	}
	rootful := true
	userNamespaceMode := UserNamespaceDisabled
	for _, option := range info.Info.SecurityOptions {
		name, _, _ := strings.Cut(option, ",")
		switch name {
		case "name=rootless":
			rootful = false
		case "name=userns":
			userNamespaceMode = "remapped"
		}
	}
	return DaemonInspection{
		APIVersion:        version.APIVersion,
		OSType:            version.Os,
		Architecture:      architecture,
		Rootful:           rootful,
		UserNamespaceMode: userNamespaceMode,
	}, nil
}

func normalizeDaemonArchitecture(versionArchitecture, infoArchitecture string) (string, bool) {
	if versionArchitecture == "amd64" && infoArchitecture == "x86_64" {
		return "amd64", true
	}
	return "", false
}

func (engine *mobyEngine) InspectImage(ctx context.Context, reference string) (ImageInspection, error) {
	if reference != ReferenceImageReference {
		return ImageInspection{}, ErrAdmissionRejected
	}
	var raw bytes.Buffer
	result, err := engine.client.ImageInspect(
		ctx,
		reference,
		mobyclient.ImageInspectWithPlatform(&ocispec.Platform{OS: "linux", Architecture: "amd64"}),
		mobyclient.ImageInspectWithRawResponse(&raw),
	)
	if err != nil {
		return ImageInspection{}, operationError("inspect image", err)
	}
	inspection, err := projectImageInspection(reference, result, raw.Bytes())
	if err != nil {
		return ImageInspection{}, operationError("project image", err)
	}
	return inspection, nil
}

func (engine *mobyEngine) Create(ctx context.Context, spec CreateSpec) (CreateResult, error) {
	if err := validateAdapterCreateSpec(spec); err != nil {
		return CreateResult{}, err
	}
	options, err := mobyCreateOptions(spec)
	if err != nil {
		return CreateResult{}, err
	}
	result, err := engine.client.ContainerCreate(ctx, options)
	projected := CreateResult{ContainerID: result.ID, Warnings: append([]string(nil), result.Warnings...)}
	if err != nil {
		// The daemon can create the child and return its ID before a trailing
		// transport/decode failure. Preserve a syntactically valid ID so the
		// supervisor can inspect ownership and conservatively clean it up.
		if !validLowerHex(projected.ContainerID, 64) {
			projected = CreateResult{}
		}
		return projected, operationError("create container", err)
	}
	return projected, nil
}

func (engine *mobyEngine) Start(ctx context.Context, containerID string) error {
	if !validLowerHex(containerID, 64) {
		return ErrAdmissionRejected
	}
	_, err := engine.client.ContainerStart(ctx, containerID, mobyclient.ContainerStartOptions{})
	if err != nil {
		return operationError("start container", err)
	}
	return nil
}

func (engine *mobyEngine) Inspect(ctx context.Context, containerID string) (ContainerInspection, error) {
	if !validLowerHex(containerID, 64) {
		return ContainerInspection{}, ErrAdmissionRejected
	}
	result, err := engine.client.ContainerInspect(ctx, containerID, mobyclient.ContainerInspectOptions{Size: false})
	if err != nil {
		return ContainerInspection{}, operationError("inspect container", err)
	}
	inspection, err := projectContainerInspection(result)
	if err != nil {
		return ContainerInspection{}, operationError("project container", err)
	}
	return inspection, nil
}

func (engine *mobyEngine) InspectOwnership(ctx context.Context, containerID string) (OwnershipInspection, error) {
	if !validLowerHex(containerID, 64) {
		return OwnershipInspection{}, ErrAdmissionRejected
	}
	result, err := engine.client.ContainerInspect(ctx, containerID, mobyclient.ContainerInspectOptions{Size: false})
	if err != nil {
		return OwnershipInspection{}, operationError("inspect container ownership", err)
	}
	container := result.Container
	inspection, err := projectOwnershipInspection(container)
	if err != nil {
		return OwnershipInspection{}, operationError("project container ownership", err)
	}
	return inspection, nil
}

func (engine *mobyEngine) ReadReferenceArchive(ctx context.Context, containerID, path string) (io.ReadCloser, error) {
	if !validLowerHex(containerID, 64) || validateReferenceArchivePath(path) != nil {
		return nil, ErrAdmissionRejected
	}
	result, err := engine.client.CopyFromContainer(ctx, containerID, mobyclient.CopyFromContainerOptions{SourcePath: path})
	if err != nil {
		return nil, operationError("read reference archive", err)
	}
	if result.Content == nil {
		return nil, operationError("read reference archive", errors.New("empty content"))
	}
	return result.Content, nil
}

func (engine *mobyEngine) ExpectReferenceArchiveEvents(_ context.Context, containerID string, labels map[string]string, count int) (<-chan struct{}, error) {
	owner := ownership{containerID: containerID, labels: cloneMap(labels)}
	if !validLowerHex(containerID, 64) || count < 1 || count > 2 ||
		validateOwnership(owner, containerID, labels) != nil || len(labels) != 4 {
		return nil, ErrAdmissionRejected
	}
	engine.eventMu.Lock()
	defer engine.eventMu.Unlock()
	if engine.expectedArchives == nil {
		engine.expectedArchives = make(map[string]*archiveExpectation)
	}
	if engine.expectedArchives[containerID] != nil {
		return nil, ErrAdmissionRejected
	}
	expectation := &archiveExpectation{remaining: count, labels: cloneMap(labels), done: make(chan struct{})}
	engine.expectedArchives[containerID] = expectation
	return expectation.done, nil
}

func (engine *mobyEngine) consumeExpectedArchive(containerID string, event LifecycleEvent) bool {
	engine.eventMu.Lock()
	defer engine.eventMu.Unlock()
	expectation := engine.expectedArchives[containerID]
	if expectation == nil || event.Action != LifecycleActionArchive || event.ContainerID != containerID ||
		!reflect.DeepEqual(event.Labels, expectation.labels) {
		return false
	}
	expectation.remaining--
	if expectation.remaining == 0 {
		delete(engine.expectedArchives, containerID)
		close(expectation.done)
	} else {
		engine.expectedArchives[containerID] = expectation
	}
	return true
}

func (engine *mobyEngine) Events(ctx context.Context, containerID string, sinceUnixNano int64) (<-chan LifecycleEvent, <-chan error) {
	output := make(chan LifecycleEvent)
	errorsOutput := make(chan error, 1)
	options, optionError := referenceEventOptions(containerID, sinceUnixNano)
	if optionError != nil {
		close(output)
		errorsOutput <- optionError
		close(errorsOutput)
		return output, errorsOutput
	}
	stream := engine.client.Events(ctx, options)
	go func() {
		defer close(output)
		defer close(errorsOutput)
		for {
			select {
			case <-ctx.Done():
				errorsOutput <- operationError("watch container events", ctx.Err())
				return
			case err, ok := <-stream.Err:
				if !ok {
					errorsOutput <- operationError("watch container events", io.ErrUnexpectedEOF)
					return
				}
				errorsOutput <- operationError("watch container events", err)
				return
			case message, ok := <-stream.Messages:
				if !ok {
					errorsOutput <- operationError("watch container events", io.ErrUnexpectedEOF)
					return
				}
				projected := projectLifecycleEvent(message)
				if engine.consumeExpectedArchive(containerID, projected) {
					continue
				}
				select {
				case output <- projected:
				case <-ctx.Done():
					errorsOutput <- operationError("watch container events", ctx.Err())
					return
				}
			}
		}
	}()
	return output, errorsOutput
}

func referenceEventOptions(containerID string, sinceUnixNano int64) (mobyclient.EventsListOptions, error) {
	if !validLowerHex(containerID, 64) || sinceUnixNano <= 0 {
		return mobyclient.EventsListOptions{}, ErrAdmissionRejected
	}
	filters := make(mobyclient.Filters).
		Add("type", string(events.ContainerEventType)).
		Add("container", containerID)
	seconds := sinceUnixNano / int64(time.Second)
	nanoseconds := sinceUnixNano % int64(time.Second)
	since := strconv.FormatInt(seconds, 10) + "." + strconv.FormatInt(nanoseconds+int64(time.Second), 10)[1:]
	return mobyclient.EventsListOptions{Filters: filters, Since: since}, nil
}

func (engine *mobyEngine) Kill(ctx context.Context, containerID string) error {
	if !validLowerHex(containerID, 64) {
		return ErrAdmissionRejected
	}
	_, err := engine.client.ContainerKill(ctx, containerID, mobyclient.ContainerKillOptions{Signal: "SIGKILL"})
	if err != nil {
		return operationError("kill container", err)
	}
	return nil
}

func (engine *mobyEngine) Wait(ctx context.Context, containerID string) (WaitResult, error) {
	if !validLowerHex(containerID, 64) {
		return WaitResult{}, ErrAdmissionRejected
	}
	result := engine.client.ContainerWait(ctx, containerID, mobyclient.ContainerWaitOptions{Condition: containertypes.WaitConditionNotRunning})
	select {
	case <-ctx.Done():
		return WaitResult{}, operationError("wait container", ctx.Err())
	case err, ok := <-result.Error:
		if !ok || err == nil {
			return WaitResult{}, operationError("wait container", io.ErrUnexpectedEOF)
		}
		return WaitResult{}, operationError("wait container", err)
	case response, ok := <-result.Result:
		if !ok {
			return WaitResult{}, operationError("wait container", io.ErrUnexpectedEOF)
		}
		if response.Error != nil {
			return WaitResult{}, operationError("wait container", errors.New("daemon wait failure"))
		}
		return WaitResult{ContainerID: containerID, ExitCode: response.StatusCode}, nil
	}
}

func (engine *mobyEngine) Remove(ctx context.Context, containerID string) error {
	if !validLowerHex(containerID, 64) {
		return ErrAdmissionRejected
	}
	_, err := engine.client.ContainerRemove(ctx, containerID, mobyclient.ContainerRemoveOptions{
		RemoveVolumes: false,
		RemoveLinks:   false,
		Force:         false,
	})
	if err != nil {
		return operationError("remove container", err)
	}
	return nil
}

func mobyCreateOptions(spec CreateSpec) (mobyclient.ContainerCreateOptions, error) {
	initDisabled := false
	oomKillDisabled := false
	mounts := make([]mount.Mount, 0, len(spec.Mounts)+len(spec.Tmpfs))
	for _, item := range spec.Mounts {
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   item.Source,
			Target:   item.Destination,
			ReadOnly: item.ReadOnly,
			BindOptions: &mount.BindOptions{
				Propagation:            mount.PropagationRPrivate,
				ReadOnlyForceRecursive: item.ReadOnly && item.Destination == ReferenceSnapshotPath,
			},
		})
	}
	for _, item := range spec.Tmpfs {
		mounts = append(mounts, mount.Mount{
			Type:   mount.TypeTmpfs,
			Target: item.Destination,
			TmpfsOptions: &mount.TmpfsOptions{
				SizeBytes: item.SizeBytes,
				Mode:      os.FileMode(item.Mode),
			},
		})
	}
	deviceRequests := make([]containertypes.DeviceRequest, len(spec.DeviceRequests))
	for index, request := range spec.DeviceRequests {
		if request.Count > int64(^uint(0)>>1) {
			return mobyclient.ContainerCreateOptions{}, ErrAdmissionRejected
		}
		deviceRequests[index] = containertypes.DeviceRequest{
			Driver:       request.Driver,
			Count:        int(request.Count),
			DeviceIDs:    append([]string(nil), request.DeviceIDs...),
			Capabilities: cloneStringMatrix(request.Capabilities),
			Options:      maps.Clone(request.Options),
		}
	}
	return mobyclient.ContainerCreateOptions{
		Config: &containertypes.Config{
			Hostname:        spec.Hostname,
			User:            spec.User,
			ExposedPorts:    make(networktypes.PortSet),
			Tty:             spec.TTY,
			Env:             append([]string(nil), spec.Environment...),
			Cmd:             append([]string(nil), spec.Command...),
			Image:           spec.ImageID,
			Volumes:         make(map[string]struct{}),
			WorkingDir:      spec.WorkingDir,
			Entrypoint:      append([]string(nil), spec.Entrypoint...),
			NetworkDisabled: spec.NetworkMode == NetworkModeNone,
			Labels:          maps.Clone(spec.Labels),
		},
		HostConfig: &containertypes.HostConfig{
			LogConfig:       containertypes.LogConfig{Type: spec.LogDriver},
			NetworkMode:     containertypes.NetworkMode(spec.NetworkMode),
			PortBindings:    make(networktypes.PortMap),
			RestartPolicy:   containertypes.RestartPolicy{Name: containertypes.RestartPolicyMode(spec.RestartPolicy)},
			CapAdd:          append([]string(nil), spec.CapAdd...),
			IpcMode:         containertypes.IpcMode(spec.IPCMode),
			Links:           append([]string(nil), spec.Links...),
			Privileged:      spec.Privileged,
			PublishAllPorts: false,
			ReadonlyRootfs:  spec.ReadOnlyRootFS,
			ShmSize:         spec.ShmSizeBytes,
			Runtime:         "runc",
			Init:            &initDisabled,
			CgroupnsMode:    containertypes.CgroupnsModePrivate,
			Mounts:          mounts,
			Resources: containertypes.Resources{
				OomKillDisable: &oomKillDisabled,
				DeviceRequests: deviceRequests,
			},
		},
		NetworkingConfig: nil,
		Platform:         &ocispec.Platform{OS: "linux", Architecture: "amd64"},
		Name:             "",
	}, nil
}

func validateAdapterCreateSpec(spec CreateSpec) error {
	key, ok := findUniqueEnvironmentValue(spec.Environment, "VLLM_API_KEY")
	if !ok || !validSecret(key) || !strings.HasPrefix(spec.Hostname, "purify-r6a-") {
		return ErrAdmissionRejected
	}
	runID := strings.TrimPrefix(spec.Hostname, "purify-r6a-")
	if !validLowerHex(runID, 32) || len(spec.Mounts) != 3 {
		return ErrAdmissionRejected
	}
	plan, err := buildReferencePlan(deployDescriptor(), HostPaths{
		SnapshotDir:  spec.Mounts[0].Source,
		TemplateFile: spec.Mounts[1].Source,
		RunDir:       spec.Mounts[2].Source,
	}, generatedInputs{RunID: runID, APIKey: key})
	if err != nil || !equalCreateSpecs(plan.create, spec) {
		return ErrAdmissionRejected
	}
	return nil
}

func equalCreateSpecs(expected, actual CreateSpec) bool {
	expectedEnvironment := expected.Environment
	actualEnvironment := actual.Environment
	expected.Environment = nil
	actual.Environment = nil
	return reflect.DeepEqual(expected, actual) && equalConfigEnvironment(expectedEnvironment, actualEnvironment)
}

func projectImageInspection(reference string, result mobyclient.ImageInspectResult, raw []byte) (ImageInspection, error) {
	if result.Config == nil {
		return ImageInspection{}, errors.New("missing image config")
	}
	cmdPresent, err := imageCommandPresent(raw)
	if err != nil {
		return ImageInspection{}, err
	}
	inspection := ImageInspection{
		RequestedReference: reference,
		ID:                 result.ID,
		OS:                 result.Os,
		Architecture:       result.Architecture,
		ManifestDescriptor: projectManifestDescriptor(result.Descriptor),
		Config: ImageConfigInspection{
			Entrypoint:   append([]string(nil), result.Config.Entrypoint...),
			Cmd:          append([]string(nil), result.Config.Cmd...),
			CmdPresent:   cmdPresent,
			Environment:  append([]string(nil), result.Config.Env...),
			User:         result.Config.User,
			WorkingDir:   result.Config.WorkingDir,
			ExposedPorts: sortedSetKeys(result.Config.ExposedPorts),
			Volumes:      sortedSetKeys(result.Config.Volumes),
			Labels:       maps.Clone(result.Config.Labels),
		},
	}
	return inspection, nil
}

func projectContainerInspection(result mobyclient.ContainerInspectResult) (ContainerInspection, error) {
	container := result.Container
	if container.Config == nil || container.HostConfig == nil || container.State == nil ||
		container.NetworkSettings == nil || container.NetworkSettings.SandboxID != "" ||
		container.NetworkSettings.SandboxKey != "" || len(container.NetworkSettings.Ports) != 0 ||
		len(container.NetworkSettings.Networks) != 0 {
		return ContainerInspection{}, errors.New("incomplete container inspection")
	}
	if err := validateMobyContainerConfig(container.Config, container.HostConfig); err != nil {
		return ContainerInspection{}, err
	}
	mounts, tmpfs, err := projectMounts(container.Mounts, container.HostConfig.Mounts)
	if err != nil {
		return ContainerInspection{}, err
	}
	deviceRequests := make([]DeviceRequest, len(container.HostConfig.DeviceRequests))
	for index, request := range container.HostConfig.DeviceRequests {
		deviceRequests[index] = DeviceRequest{
			Driver:       request.Driver,
			Count:        int64(request.Count),
			DeviceIDs:    append([]string(nil), request.DeviceIDs...),
			Capabilities: cloneStringMatrix(request.Capabilities),
			Options:      maps.Clone(request.Options),
		}
	}
	platform := container.Platform
	if platform == "linux" {
		// Docker container inspect reports only the OS here. The exact amd64
		// child remains bound by its config ID and manifest descriptor.
		platform = "linux/amd64"
	}
	return ContainerInspection{
		ID:                      container.ID,
		ImageID:                 container.Image,
		ConfiguredImage:         container.Config.Image,
		ImageManifestDescriptor: projectManifestDescriptor(container.ImageManifestDescriptor),
		Platform:                platform,
		Path:                    container.Path,
		Args:                    append([]string(nil), container.Args...),
		Hostname:                container.Config.Hostname,
		Entrypoint:              append([]string(nil), container.Config.Entrypoint...),
		Command:                 append([]string(nil), container.Config.Cmd...),
		Environment:             append([]string(nil), container.Config.Env...),
		User:                    container.Config.User,
		WorkingDir:              container.Config.WorkingDir,
		Labels:                  maps.Clone(container.Config.Labels),
		Mounts:                  mounts,
		Tmpfs:                   tmpfs,
		DeviceRequests:          deviceRequests,
		NetworkMode:             string(container.HostConfig.NetworkMode),
		NetworkDisabled:         container.Config.NetworkDisabled,
		IPCMode:                 string(container.HostConfig.IpcMode),
		ShmSizeBytes:            container.HostConfig.ShmSize,
		ReadOnlyRootFS:          container.HostConfig.ReadonlyRootfs,
		RestartPolicy:           string(container.HostConfig.RestartPolicy.Name),
		LogDriver:               container.HostConfig.LogConfig.Type,
		Privileged:              container.HostConfig.Privileged,
		TTY:                     container.Config.Tty,
		ExposedPorts:            sortedNetworkPortSet(container.Config.ExposedPorts),
		PortBindings:            projectPortBindings(container.HostConfig.PortBindings),
		CapAdd:                  append([]string(nil), container.HostConfig.CapAdd...),
		Links:                   append([]string(nil), container.HostConfig.Links...),
		RestartCount:            container.RestartCount,
		State:                   projectContainerState(container.State),
	}, nil
}

func projectContainerState(state *containertypes.State) ContainerState {
	if state == nil {
		return ContainerState{}
	}
	return ContainerState{
		Status: string(state.Status), Running: state.Running, Paused: state.Paused,
		Restarting: state.Restarting, Dead: state.Dead, OOMKilled: state.OOMKilled,
		PID: state.Pid, ExitCode: state.ExitCode,
	}
}

func projectOwnershipInspection(container containertypes.InspectResponse) (OwnershipInspection, error) {
	if container.Config == nil || container.State == nil || !validLowerHex(container.ID, 64) {
		return OwnershipInspection{}, errors.New("incomplete ownership response")
	}
	return OwnershipInspection{
		ID: container.ID, Labels: ownershipLabels(container.Config.Labels), State: projectContainerState(container.State),
	}, nil
}

func validateMobyContainerConfig(config *containertypes.Config, host *containertypes.HostConfig) error {
	if config.Domainname != "" || config.AttachStdin || config.AttachStdout || config.AttachStderr ||
		config.OpenStdin || config.StdinOnce || config.Healthcheck != nil || config.ArgsEscaped ||
		len(config.OnBuild) != 0 || config.StopSignal != "" || config.StopTimeout != nil || len(config.Shell) != 0 ||
		len(config.Volumes) != 0 ||
		len(host.Binds) != 0 || host.ContainerIDFile != "" || host.AutoRemove || host.VolumeDriver != "" ||
		len(host.VolumesFrom) != 0 || host.PublishAllPorts || len(host.CapDrop) != 0 ||
		host.LogConfig.Type != LogDriverNone || len(host.LogConfig.Config) != 0 || host.ConsoleSize != [2]uint{} ||
		len(host.DNS) != 0 || len(host.DNSOptions) != 0 || len(host.DNSSearch) != 0 ||
		len(host.ExtraHosts) != 0 || len(host.GroupAdd) != 0 || host.PidMode != "" || host.UsernsMode != "" ||
		host.UTSMode != "" || len(host.SecurityOpt) != 0 || len(host.StorageOpt) != 0 || len(host.Tmpfs) != 0 ||
		len(host.Sysctls) != 0 || host.Runtime != "runc" || host.Init == nil || *host.Init || len(host.Devices) != 0 ||
		len(host.DeviceCgroupRules) != 0 || len(host.Annotations) != 0 ||
		host.RestartPolicy.MaximumRetryCount != 0 || !host.CgroupnsMode.IsPrivate() || !zeroMobyResources(host.Resources) ||
		host.OomScoreAdj != 0 || host.Cgroup != "" || host.Isolation != "" || !validMobyMaskedPaths(host.MaskedPaths) ||
		!reflect.DeepEqual(host.ReadonlyPaths, referenceReadonlyPaths()) {
		return errors.New("unexpected container configuration")
	}
	return nil
}

func zeroMobyResources(resources containertypes.Resources) bool {
	requests := resources.DeviceRequests
	resources.DeviceRequests = nil
	// Dockerd normalizes an unspecified/false OOM-killer request to either a
	// non-nil false pointer or nil when the host kernel lacks that controller.
	// Both encode the exact requested semantic; true is never accepted.
	if resources.OomKillDisable != nil && *resources.OomKillDisable {
		return false
	}
	resources.OomKillDisable = nil
	return reflect.DeepEqual(resources, containertypes.Resources{}) && len(requests) == 1 &&
		requests[0].Driver == "nvidia" && requests[0].Count == 1 && len(requests[0].DeviceIDs) == 0 &&
		reflect.DeepEqual(requests[0].Capabilities, [][]string{{"gpu", "nvidia", "compute"}}) && len(requests[0].Options) == 0
}

func referenceReadonlyPaths() []string {
	return []string{"/proc/bus", "/proc/fs", "/proc/irq", "/proc/sys", "/proc/sysrq-trigger"}
}

func validMobyMaskedPaths(paths []string) bool {
	baseValues := []string{
		"/proc/asound", "/proc/acpi", "/proc/interrupts", "/proc/kcore", "/proc/keys",
		"/proc/latency_stats", "/proc/timer_list", "/proc/timer_stats", "/proc/sched_debug",
		"/proc/scsi", "/sys/firmware", "/sys/devices/virtual/powercap",
	}
	base := make(map[string]struct{}, len(baseValues))
	for _, path := range baseValues {
		base[path] = struct{}{}
	}
	if len(paths) < len(base) {
		return false
	}
	seen := make(map[string]struct{}, len(paths))
	baseSeen := 0
	for _, path := range paths {
		if _, duplicate := seen[path]; duplicate {
			return false
		}
		seen[path] = struct{}{}
		if _, required := base[path]; required {
			baseSeen++
			continue
		}
		const prefix = "/sys/devices/system/cpu/cpu"
		const suffix = "/thermal_throttle"
		if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
			return false
		}
		cpu := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
		if !canonicalDecimal(cpu) {
			return false
		}
	}
	return baseSeen == len(base)
}

func projectMounts(actual []containertypes.MountPoint, configured []mount.Mount) ([]Mount, []TmpfsMount, error) {
	binds := make([]Mount, 0, len(configured))
	tmpfs := make([]TmpfsMount, 0, len(configured))
	actualByDestination := make(map[string]containertypes.MountPoint, len(actual))
	for _, item := range actual {
		if item.Destination == "" {
			return nil, nil, errors.New("mount without destination")
		}
		if _, duplicate := actualByDestination[item.Destination]; duplicate {
			return nil, nil, errors.New("duplicate mount destination")
		}
		actualByDestination[item.Destination] = item
	}
	if len(actualByDestination) != len(configured) {
		return nil, nil, errors.New("mount count mismatch")
	}
	for _, item := range configured {
		actualItem, present := actualByDestination[item.Target]
		if !present || actualItem.Type != item.Type || actualItem.RW == item.ReadOnly {
			return nil, nil, errors.New("mount inspection mismatch")
		}
		switch item.Type {
		case mount.TypeBind:
			if item.Source == "" || item.BindOptions == nil || item.BindOptions.Propagation != mount.PropagationRPrivate ||
				item.BindOptions.NonRecursive || item.BindOptions.CreateMountpoint || item.BindOptions.ReadOnlyNonRecursive ||
				item.BindOptions.ReadOnlyForceRecursive != (item.ReadOnly && item.Target == ReferenceSnapshotPath) || item.VolumeOptions != nil ||
				item.ImageOptions != nil || item.TmpfsOptions != nil || item.ClusterOptions != nil ||
				actualItem.Source != item.Source || actualItem.Name != "" || actualItem.Driver != "" || actualItem.Propagation != mount.PropagationRPrivate {
				return nil, nil, errors.New("bind mount inspection mismatch")
			}
			binds = append(binds, Mount{Type: MountTypeBind, Source: item.Source, Destination: item.Target, ReadOnly: item.ReadOnly})
		case mount.TypeTmpfs:
			if item.Source != "" || item.ReadOnly || item.TmpfsOptions == nil || item.TmpfsOptions.SizeBytes <= 0 ||
				len(item.TmpfsOptions.Options) != 0 || item.BindOptions != nil || item.VolumeOptions != nil ||
				item.ImageOptions != nil || item.ClusterOptions != nil || actualItem.Source != "" || actualItem.Name != "" || actualItem.Driver != "" {
				return nil, nil, errors.New("tmpfs mount inspection mismatch")
			}
			tmpfs = append(tmpfs, TmpfsMount{Destination: item.Target, SizeBytes: item.TmpfsOptions.SizeBytes, Mode: uint32(item.TmpfsOptions.Mode)})
		default:
			return nil, nil, errors.New("unexpected mount type")
		}
	}
	return binds, tmpfs, nil
}

func projectLifecycleEvent(message events.Message) LifecycleEvent {
	labels := make(map[string]string, 4)
	for _, name := range []string{LabelManaged, LabelComponent, LabelRunID, LabelSpecDigest} {
		if value, present := message.Actor.Attributes[name]; present {
			labels[name] = value
		}
	}
	if message.Type != events.ContainerEventType {
		labels = nil
	}
	return LifecycleEvent{ContainerID: message.Actor.ID, Action: string(message.Action), Labels: labels}
}

func projectManifestDescriptor(descriptor *ocispec.Descriptor) *ManifestDescriptor {
	if descriptor == nil {
		return nil
	}
	return &ManifestDescriptor{Digest: descriptor.Digest.String(), MediaType: descriptor.MediaType}
}

func imageCommandPresent(raw []byte) (bool, error) {
	var envelope struct {
		Config *struct {
			Command json.RawMessage `json:"Cmd"`
		} `json:"Config"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Config == nil {
		return false, errors.New("invalid raw image inspection")
	}
	command := bytes.TrimSpace(envelope.Config.Command)
	return len(command) != 0 && !bytes.Equal(command, []byte("null")), nil
}

func findUniqueEnvironmentValue(environment []string, wanted string) (string, bool) {
	var value string
	found := false
	for _, entry := range environment {
		name, item, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			return "", false
		}
		if name == wanted {
			if found {
				return "", false
			}
			found = true
			value = item
		}
	}
	return value, found
}

func sortedSetKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedNetworkPortSet(values networktypes.PortSet) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key.String())
	}
	sort.Strings(keys)
	return keys
}

func projectPortBindings(values networktypes.PortMap) []PortBinding {
	bindings := make([]PortBinding, 0, len(values))
	for port, items := range values {
		for _, item := range items {
			hostIP := ""
			if item.HostIP.IsValid() {
				hostIP = item.HostIP.String()
			}
			bindings = append(bindings, PortBinding{ContainerPort: port.String(), HostIP: hostIP, HostPort: item.HostPort})
		}
	}
	sort.Slice(bindings, func(left, right int) bool {
		leftKey := bindings[left].ContainerPort + "\x00" + bindings[left].HostIP + "\x00" + bindings[left].HostPort
		rightKey := bindings[right].ContainerPort + "\x00" + bindings[right].HostIP + "\x00" + bindings[right].HostPort
		return leftKey < rightKey
	})
	return bindings
}

func cloneStringMatrix(values [][]string) [][]string {
	clone := make([][]string, len(values))
	for index, value := range values {
		clone[index] = append([]string(nil), value...)
	}
	return clone
}

func deployDescriptor() deploy.Descriptor { return deploy.ReferenceDescriptor() }

func operationError(operation string, err error) error {
	if err == nil {
		return nil
	}
	if cerrdefs.IsNotFound(err) {
		return errors.Join(ErrEngineOperation, errContainerNotFound)
	}
	if errors.Is(err, context.Canceled) {
		return errors.Join(ErrEngineOperation, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.Join(ErrEngineOperation, context.DeadlineExceeded)
	}
	return fmt.Errorf("%w: %s", ErrEngineOperation, operation)
}
