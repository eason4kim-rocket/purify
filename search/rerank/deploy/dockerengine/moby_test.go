package dockerengine

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	units "github.com/docker/go-units"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/mount"
	networktypes "github.com/moby/moby/api/types/network"
	mobyclient "github.com/moby/moby/client"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestNewMobyEngineUsesOnlyFixedLocalSocket(t *testing.T) {
	for _, setting := range []struct{ name, value string }{
		{name: "DOCKER_HOST", value: "tcp://attacker.invalid:2375"},
		{name: "DOCKER_API_VERSION", value: "1.40"},
		{name: "DOCKER_CONTEXT", value: "attacker"},
		{name: "HTTP_PROXY", value: "http://attacker.invalid"},
		{name: "HTTPS_PROXY", value: "http://attacker.invalid"},
	} {
		t.Setenv(setting.name, setting.value)
	}

	engine, err := newMobyEngine()
	if err != nil {
		t.Fatal(err)
	}
	moby, ok := engine.(*mobyEngine)
	if !ok {
		t.Fatalf("New() returned %T", engine)
	}
	if got := moby.client.DaemonHost(); got != "unix://"+DockerSocketPath {
		t.Fatalf("daemon host = %q", got)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMobyCreateTranslationIsExactAndAdapterRejectsArbitrarySpec(t *testing.T) {
	plan := mustPlan(t)
	spec := plan.cloneCreateSpec()
	options, err := mobyCreateOptions(spec)
	if err != nil {
		t.Fatal(err)
	}
	if options.Config == nil || options.HostConfig == nil || options.Platform == nil || options.NetworkingConfig != nil || options.Name != "" {
		t.Fatalf("create options missing fixed parts: %#v", options)
	}
	if options.Config.Image != ReferenceImageReference || options.Config.Hostname != spec.Hostname ||
		!reflect.DeepEqual(options.Config.Entrypoint, spec.Entrypoint) || !reflect.DeepEqual(options.Config.Cmd, spec.Command) ||
		!reflect.DeepEqual(options.Config.Env, spec.Environment) || !options.Config.NetworkDisabled || options.Config.Tty ||
		options.Platform.OS != "linux" || options.Platform.Architecture != "amd64" {
		t.Fatalf("portable create config = %#v / %#v", options.Config, options.Platform)
	}
	host := options.HostConfig
	if string(host.NetworkMode) != NetworkModeNone || !host.ReadonlyRootfs || host.Privileged || host.PublishAllPorts ||
		host.IpcMode != containertypes.IpcMode(IPCModePrivate) || host.ShmSize != 1<<30 ||
		host.RestartPolicy.Name != containertypes.RestartPolicyDisabled || host.LogConfig.Type != LogDriverNone ||
		host.Runtime != "runc" || host.Init == nil || *host.Init || !host.CgroupnsMode.IsPrivate() ||
		host.OomKillDisable == nil || *host.OomKillDisable || len(host.PortBindings) != 0 ||
		len(host.CapAdd) != 0 || len(host.Links) != 0 || len(host.Mounts) != 6 || len(host.DeviceRequests) != 1 {
		t.Fatalf("host create config = %#v", host)
	}
	for index, item := range host.Mounts[:3] {
		want := spec.Mounts[index]
		if item.Type != mount.TypeBind || item.Source != want.Source || item.Target != want.Destination || item.ReadOnly != want.ReadOnly ||
			item.BindOptions == nil || item.BindOptions.Propagation != mount.PropagationRPrivate ||
			item.BindOptions.ReadOnlyForceRecursive != (want.ReadOnly && want.Destination == ReferenceSnapshotPath) {
			t.Fatalf("bind mount[%d] = %#v", index, item)
		}
	}
	for index, item := range host.Mounts[3:] {
		want := spec.Tmpfs[index]
		if item.Type != mount.TypeTmpfs || item.Target != want.Destination || item.TmpfsOptions == nil ||
			item.TmpfsOptions.SizeBytes != want.SizeBytes || uint32(item.TmpfsOptions.Mode) != want.Mode {
			t.Fatalf("tmpfs[%d] = %#v", index, item)
		}
	}
	device := host.DeviceRequests[0]
	if device.Driver != "nvidia" || device.Count != 1 || !reflect.DeepEqual(device.Capabilities, [][]string{{"gpu", "nvidia", "compute"}}) {
		t.Fatalf("device request = %#v", device)
	}
	joinedNonEnvironment := strings.Join(append(append(append([]string(nil), options.Config.Entrypoint...), options.Config.Cmd...), mapEntries(options.Config.Labels)...), "\x00")
	if strings.Contains(joinedNonEnvironment, testAPIKey) || countEnvironment(options.Config.Env, "VLLM_API_KEY", testAPIKey) != 1 {
		t.Fatal("secret escaped the environment-only create field")
	}
	if err := validateAdapterCreateSpec(spec); err != nil {
		t.Fatalf("validateAdapterCreateSpec(valid) = %v", err)
	}
	for _, mutate := range []func(*CreateSpec){
		func(value *CreateSpec) { value.ImageID = "alpine:latest" },
		func(value *CreateSpec) { value.Command = []string{"sh"} },
		func(value *CreateSpec) { value.NetworkMode = "host" },
		func(value *CreateSpec) { value.Mounts[0].Source = "/" },
		func(value *CreateSpec) { value.Labels[LabelSpecDigest] = strings.Repeat("0", 64) },
	} {
		drift := plan.cloneCreateSpec()
		mutate(&drift)
		if err := validateAdapterCreateSpec(drift); !errors.Is(err, ErrAdmissionRejected) {
			t.Fatalf("validateAdapterCreateSpec(drift) = %v", err)
		}
	}
}

func TestMobyImageProjectionPreservesDescriptorPlatformAndNullCommand(t *testing.T) {
	config := &dockerspec.DockerOCIImageConfig{}
	config.Entrypoint = []string{"vllm", "serve"}
	config.Env = referenceImageEnvironment()
	config.User = ""
	config.WorkingDir = "/vllm-workspace"
	config.Labels = referenceImageLabels()
	result := mobyclient.ImageInspectResult{InspectResponse: image.InspectResponse{
		ID:           ReferenceImageManifestDigest,
		Config:       config,
		Architecture: "amd64",
		Os:           "linux",
		Descriptor: &ocispec.Descriptor{
			Digest:    digest.Digest(ReferenceImageManifestDigest),
			MediaType: ReferenceManifestMediaType,
		},
	}}
	inspection, err := projectImageInspection(ReferenceImageReference, result, []byte(`{"Config":{"Cmd":null}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateReferenceImage(inspection); err != nil {
		t.Fatalf("ValidateReferenceImage(projected) = %v: %#v", err, inspection)
	}
	present, err := projectImageInspection(ReferenceImageReference, result, []byte(`{"Config":{"Cmd":[]}}`))
	if err != nil || !present.Config.CmdPresent {
		t.Fatalf("present empty command = %#v, %v", present.Config, err)
	}
	if err := ValidateReferenceImage(present); !errors.Is(err, ErrImageRejected) {
		t.Fatalf("ValidateReferenceImage(present command) = %v", err)
	}
}

func TestMobyContainerProjectionFeedsExactStateValidator(t *testing.T) {
	plan := mustPlan(t)
	owner, err := plan.establishOwnership(CreateResult{ContainerID: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	options, err := mobyCreateOptions(plan.cloneCreateSpec())
	if err != nil {
		t.Fatal(err)
	}
	actualMounts := make([]containertypes.MountPoint, len(options.HostConfig.Mounts))
	for index, item := range options.HostConfig.Mounts {
		actualMounts[index] = containertypes.MountPoint{
			Type:        item.Type,
			Source:      item.Source,
			Destination: item.Target,
			RW:          !item.ReadOnly,
		}
		if item.Type == mount.TypeBind {
			actualMounts[index].Propagation = mount.PropagationRPrivate
		}
	}
	result := mobyclient.ContainerInspectResult{Container: containertypes.InspectResponse{
		ID:           owner.containerID,
		Path:         options.Config.Entrypoint[0],
		Args:         append(append([]string(nil), options.Config.Entrypoint[1:]...), options.Config.Cmd...),
		State:        &containertypes.State{Status: containertypes.StateCreated},
		Image:        ReferenceImageManifestDigest,
		Platform:     "linux",
		Mounts:       actualMounts,
		Config:       options.Config,
		HostConfig:   options.HostConfig,
		RestartCount: 0,
		ImageManifestDescriptor: &ocispec.Descriptor{
			Digest:    digest.Digest(ReferenceImageManifestDigest),
			MediaType: ReferenceManifestMediaType,
		},
		NetworkSettings: &containertypes.NetworkSettings{
			Ports:    make(networktypes.PortMap),
			Networks: make(map[string]*networktypes.EndpointSettings),
		},
	}}
	result.Container.HostConfig.MaskedPaths = append([]string{
		"/proc/asound", "/proc/acpi", "/proc/interrupts", "/proc/kcore", "/proc/keys",
		"/proc/latency_stats", "/proc/timer_list", "/proc/timer_stats", "/proc/sched_debug",
		"/proc/scsi", "/sys/firmware", "/sys/devices/virtual/powercap",
	}, "/sys/devices/system/cpu/cpu0/thermal_throttle")
	result.Container.HostConfig.ReadonlyPaths = referenceReadonlyPaths()
	inspection, err := projectContainerInspection(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCreated(plan, owner, inspection); err != nil {
		t.Fatalf("validateCreated(projected) = %v: %#v", err, inspection)
	}

	drift := result
	drift.Container.HostConfig = cloneMobyHostConfig(options.HostConfig)
	drift.Container.HostConfig.PublishAllPorts = true
	if _, err := projectContainerInspection(drift); err == nil {
		t.Fatal("projectContainerInspection accepted publish-all drift")
	}
	minimal, err := projectOwnershipInspection(drift.Container)
	if err != nil || minimal.ID != owner.containerID || !reflect.DeepEqual(minimal.Labels, owner.labels) {
		t.Fatalf("minimal ownership projection lost cleanup authority: %#v, %v", minimal, err)
	}

	t.Run("daemon normalized host and network state is exact", func(t *testing.T) {
		base := result.Container
		mutations := []struct {
			name string
			edit func(*containertypes.InspectResponse)
		}{
			{name: "init missing", edit: func(v *containertypes.InspectResponse) { v.HostConfig.Init = nil }},
			{name: "init enabled", edit: func(v *containertypes.InspectResponse) { yes := true; v.HostConfig.Init = &yes }},
			{name: "runtime empty", edit: func(v *containertypes.InspectResponse) { v.HostConfig.Runtime = "" }},
			{name: "runtime nvidia", edit: func(v *containertypes.InspectResponse) { v.HostConfig.Runtime = "nvidia" }},
			{name: "cgroup empty", edit: func(v *containertypes.InspectResponse) { v.HostConfig.CgroupnsMode = "" }},
			{name: "cgroup host", edit: func(v *containertypes.InspectResponse) { v.HostConfig.CgroupnsMode = containertypes.CgroupnsModeHost }},
			{name: "oom true", edit: func(v *containertypes.InspectResponse) { yes := true; v.HostConfig.OomKillDisable = &yes }},
			{name: "log default", edit: func(v *containertypes.InspectResponse) { v.HostConfig.LogConfig.Type = "json-file" }},
			{name: "log option", edit: func(v *containertypes.InspectResponse) {
				v.HostConfig.LogConfig.Config = map[string]string{"max-size": "1m"}
			}},
			{name: "daemon ulimit", edit: func(v *containertypes.InspectResponse) {
				v.HostConfig.Ulimits = []*units.Ulimit{{Name: "nofile", Soft: 1024, Hard: 1024}}
			}},
			{name: "masked missing", edit: func(v *containertypes.InspectResponse) { v.HostConfig.MaskedPaths = v.HostConfig.MaskedPaths[1:] }},
			{name: "masked duplicate", edit: func(v *containertypes.InspectResponse) {
				v.HostConfig.MaskedPaths = append(v.HostConfig.MaskedPaths, v.HostConfig.MaskedPaths[0])
			}},
			{name: "masked foreign", edit: func(v *containertypes.InspectResponse) {
				v.HostConfig.MaskedPaths = append(v.HostConfig.MaskedPaths, "/etc/shadow")
			}},
			{name: "readonly missing", edit: func(v *containertypes.InspectResponse) { v.HostConfig.ReadonlyPaths = v.HostConfig.ReadonlyPaths[1:] }},
			{name: "windows isolation", edit: func(v *containertypes.InspectResponse) { v.HostConfig.Isolation = "hyperv" }},
			{name: "network settings missing", edit: func(v *containertypes.InspectResponse) { v.NetworkSettings = nil }},
			{name: "network sandbox", edit: func(v *containertypes.InspectResponse) { v.NetworkSettings.SandboxID = "sandbox" }},
			{name: "network attachment", edit: func(v *containertypes.InspectResponse) {
				v.NetworkSettings.Networks["bridge"] = &networktypes.EndpointSettings{}
			}},
		}
		for _, mutation := range mutations {
			t.Run(mutation.name, func(t *testing.T) {
				candidate := cloneMobyInspectResponse(base)
				mutation.edit(&candidate)
				if _, err := projectContainerInspection(mobyclient.ContainerInspectResult{Container: candidate}); err == nil {
					t.Fatal("projectContainerInspection accepted daemon drift")
				}
			})
		}
	})
}

func TestMobyEventProjectionKeepsOnlyOwnershipLabels(t *testing.T) {
	labels := map[string]string{
		LabelManaged: "true", LabelComponent: ReferenceComponent,
		LabelRunID: testGenerated.RunID, LabelSpecDigest: strings.Repeat("a", 64),
		"image": "contains-operator-data", "name": "daemon-name",
	}
	event := projectLifecycleEvent(events.Message{
		Type: events.ContainerEventType, Action: events.ActionStart,
		Actor: events.Actor{ID: strings.Repeat("b", 64), Attributes: labels},
	})
	if event.Action != LifecycleActionStart || event.ContainerID != strings.Repeat("b", 64) || len(event.Labels) != 4 || event.Labels["image"] != "" {
		t.Fatalf("event = %#v", event)
	}
	nonContainer := projectLifecycleEvent(events.Message{Type: events.ImageEventType, Actor: events.Actor{ID: event.ContainerID, Attributes: labels}})
	if nonContainer.Labels != nil {
		t.Fatalf("non-container event retained ownership labels: %#v", nonContainer)
	}
}

func TestNormalizeDaemonArchitectureLocksMobyLinuxSpellings(t *testing.T) {
	if got, ok := normalizeDaemonArchitecture("amd64", "x86_64"); !ok || got != "amd64" {
		t.Fatalf("normalizeDaemonArchitecture() = %q, %v", got, ok)
	}
	for _, pair := range [][2]string{{"amd64", "amd64"}, {"arm64", "aarch64"}, {"", "x86_64"}, {"amd64", ""}} {
		if got, ok := normalizeDaemonArchitecture(pair[0], pair[1]); ok || got != "" {
			t.Fatalf("normalizeDaemonArchitecture(%q,%q) = %q,%v", pair[0], pair[1], got, ok)
		}
	}
}

func TestMobyArchiveAllowanceIsOwnerBoundAndAcknowledgedExactly(t *testing.T) {
	engine := &mobyEngine{}
	containerID := strings.Repeat("b", 64)
	labels := map[string]string{
		LabelManaged: "true", LabelComponent: ReferenceComponent,
		LabelRunID: testGenerated.RunID, LabelSpecDigest: strings.Repeat("a", 64),
	}
	done, err := engine.ExpectReferenceArchiveEvents(context.Background(), containerID, labels, 2)
	if err != nil || done == nil {
		t.Fatalf("ExpectReferenceArchiveEvents() = %#v, %v", done, err)
	}
	foreign := LifecycleEvent{ContainerID: containerID, Action: LifecycleActionArchive, Labels: cloneMap(labels)}
	foreign.Labels[LabelRunID] = strings.Repeat("c", 32)
	if engine.consumeExpectedArchive(containerID, foreign) {
		t.Fatal("foreign archive consumed allowance")
	}
	ownerEvent := LifecycleEvent{ContainerID: containerID, Action: LifecycleActionArchive, Labels: cloneMap(labels)}
	if !engine.consumeExpectedArchive(containerID, ownerEvent) {
		t.Fatal("first owned archive was not consumed")
	}
	select {
	case <-done:
		t.Fatal("archive allowance acknowledged too early")
	default:
	}
	if !engine.consumeExpectedArchive(containerID, ownerEvent) {
		t.Fatal("second owned archive was not consumed")
	}
	select {
	case <-done:
	default:
		t.Fatal("archive allowance was not acknowledged")
	}
	if engine.consumeExpectedArchive(containerID, ownerEvent) {
		t.Fatal("unregistered archive was consumed")
	}
}

func TestMobyReferenceEventOptionsRequireReplayCursorAndExactFilters(t *testing.T) {
	containerID := strings.Repeat("b", 64)
	options, err := referenceEventOptions(containerID, 1_725_000_000_123_456_789)
	if err != nil {
		t.Fatal(err)
	}
	if options.Since != "1725000000.123456789" ||
		!options.Filters["type"][string(events.ContainerEventType)] ||
		!options.Filters["container"][containerID] || len(options.Filters) != 2 {
		t.Fatalf("event options = %#v", options)
	}
	for _, invalid := range []struct {
		id     string
		cursor int64
	}{{containerID, 0}, {containerID, -1}, {"short", 1}} {
		if _, err := referenceEventOptions(invalid.id, invalid.cursor); !errors.Is(err, ErrAdmissionRejected) {
			t.Fatalf("referenceEventOptions(%q,%d) = %v", invalid.id, invalid.cursor, err)
		}
	}
}

func TestMobyOperationErrorsAreStableAndSecretFree(t *testing.T) {
	err := operationError("create container", errors.New("daemon echoed "+testAPIKey))
	if !errors.Is(err, ErrEngineOperation) || strings.Contains(err.Error(), testAPIKey) {
		t.Fatalf("operationError() = %v", err)
	}
	canceled := operationError("wait container", context.Canceled)
	if !errors.Is(canceled, ErrEngineOperation) || !errors.Is(canceled, context.Canceled) {
		t.Fatalf("canceled error = %v", canceled)
	}
	notFound := operationError("inspect container", cerrdefs.ErrNotFound)
	if !errors.Is(notFound, ErrEngineOperation) || !errors.Is(notFound, errContainerNotFound) {
		t.Fatalf("not-found error = %v", notFound)
	}
}

func mapEntries(values map[string]string) []string {
	entries := make([]string, 0, len(values))
	for key, value := range values {
		entries = append(entries, key+"="+value)
	}
	return entries
}

func cloneMobyHostConfig(value *containertypes.HostConfig) *containertypes.HostConfig {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	var clone containertypes.HostConfig
	if err := json.Unmarshal(encoded, &clone); err != nil {
		panic(err)
	}
	return &clone
}

func cloneMobyInspectResponse(value containertypes.InspectResponse) containertypes.InspectResponse {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	var clone containertypes.InspectResponse
	if err := json.Unmarshal(encoded, &clone); err != nil {
		panic(err)
	}
	return clone
}
