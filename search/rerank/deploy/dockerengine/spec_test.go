package dockerengine

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/use-agent/purify/search/rerank/deploy"
)

const testAPIKey = "r6a-test-secret-must-never-be-attested"

var testPaths = HostPaths{
	SnapshotDir:  "/srv/purify/artifacts/qwen3-reranker/snapshot",
	TemplateFile: "/srv/purify/artifacts/qwen3-reranker/qwen3_reranker.jinja",
	RunDir:       "/run/purify-r6a/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
}

var testGenerated = generatedInputs{
	RunID:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	APIKey: testAPIKey,
}

func TestDaemonAdmissionIsPinnedToLocalRootLinuxAMD64(t *testing.T) {
	controller := ControllerInspection{
		Endpoint:          DockerSocketPath,
		GOOS:              "linux",
		GOARCH:            "amd64",
		EffectiveUIDKnown: true,
		EffectiveUID:      0,
	}
	daemon := DaemonInspection{
		APIVersion:        "1.51",
		OSType:            "linux",
		Architecture:      "amd64",
		Rootful:           true,
		UserNamespaceMode: UserNamespaceDisabled,
	}
	if err := ValidateAdmission(controller, daemon); err != nil {
		t.Fatalf("ValidateAdmission(valid) = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*ControllerInspection, *DaemonInspection)
	}{
		{name: "remote socket", mutate: func(c *ControllerInspection, _ *DaemonInspection) { c.Endpoint = "tcp://127.0.0.1:2375" }},
		{name: "alternate unix socket", mutate: func(c *ControllerInspection, _ *DaemonInspection) { c.Endpoint = "unix:///tmp/docker.sock" }},
		{name: "controller os", mutate: func(c *ControllerInspection, _ *DaemonInspection) { c.GOOS = "darwin" }},
		{name: "controller architecture", mutate: func(c *ControllerInspection, _ *DaemonInspection) { c.GOARCH = "arm64" }},
		{name: "unknown euid", mutate: func(c *ControllerInspection, _ *DaemonInspection) { c.EffectiveUIDKnown = false }},
		{name: "non-root euid", mutate: func(c *ControllerInspection, _ *DaemonInspection) { c.EffectiveUID = 1000 }},
		{name: "old api", mutate: func(_ *ControllerInspection, d *DaemonInspection) { d.APIVersion = "1.48" }},
		{name: "malformed api", mutate: func(_ *ControllerInspection, d *DaemonInspection) { d.APIVersion = "v1.49" }},
		{name: "api suffix", mutate: func(_ *ControllerInspection, d *DaemonInspection) { d.APIVersion = "1.49-beta" }},
		{name: "daemon os", mutate: func(_ *ControllerInspection, d *DaemonInspection) { d.OSType = "windows" }},
		{name: "daemon architecture", mutate: func(_ *ControllerInspection, d *DaemonInspection) { d.Architecture = "arm64" }},
		{name: "rootless", mutate: func(_ *ControllerInspection, d *DaemonInspection) { d.Rootful = false }},
		{name: "userns remap", mutate: func(_ *ControllerInspection, d *DaemonInspection) { d.UserNamespaceMode = "remapped" }},
		{name: "unknown userns", mutate: func(_ *ControllerInspection, d *DaemonInspection) { d.UserNamespaceMode = "" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotController, gotDaemon := controller, daemon
			test.mutate(&gotController, &gotDaemon)
			if err := ValidateAdmission(gotController, gotDaemon); !errors.Is(err, ErrAdmissionRejected) {
				t.Fatalf("ValidateAdmission() = %v", err)
			}
		})
	}
}

func TestReferenceImagePreinspectionRejectsEveryPinnedFieldDrift(t *testing.T) {
	valid := validImageInspection()
	if err := ValidateReferenceImage(valid); err != nil {
		t.Fatalf("ValidateReferenceImage(valid) = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*ImageInspection)
	}{
		{name: "requested tag", mutate: func(i *ImageInspection) { i.RequestedReference = "vllm/vllm-openai:v0.23.0" }},
		{name: "config id", mutate: func(i *ImageInspection) { i.ID = "sha256:" + strings.Repeat("0", 64) }},
		{name: "os", mutate: func(i *ImageInspection) { i.OS = "windows" }},
		{name: "architecture", mutate: func(i *ImageInspection) { i.Architecture = "arm64" }},
		{name: "missing descriptor", mutate: func(i *ImageInspection) { i.ManifestDescriptor = nil }},
		{name: "manifest digest", mutate: func(i *ImageInspection) { i.ManifestDescriptor.Digest = "sha256:" + strings.Repeat("1", 64) }},
		{name: "empty descriptor media type", mutate: func(i *ImageInspection) { i.ManifestDescriptor.MediaType = "" }},
		{name: "entrypoint", mutate: func(i *ImageInspection) { i.Config.Entrypoint = []string{"python"} }},
		{name: "cmd present", mutate: func(i *ImageInspection) { i.Config.CmdPresent = true; i.Config.Cmd = []string{"serve"} }},
		{name: "environment missing", mutate: func(i *ImageInspection) { i.Config.Environment = i.Config.Environment[:len(i.Config.Environment)-1] }},
		{name: "environment extra", mutate: func(i *ImageInspection) {
			i.Config.Environment = append(i.Config.Environment, "AWS_SECRET_ACCESS_KEY=bad")
		}},
		{name: "environment reordered", mutate: func(i *ImageInspection) {
			i.Config.Environment[0], i.Config.Environment[1] = i.Config.Environment[1], i.Config.Environment[0]
		}},
		{name: "user", mutate: func(i *ImageInspection) { i.Config.User = "1000" }},
		{name: "working directory", mutate: func(i *ImageInspection) { i.Config.WorkingDir = "/tmp" }},
		{name: "exposed port", mutate: func(i *ImageInspection) { i.Config.ExposedPorts = []string{"8000/tcp"} }},
		{name: "volume", mutate: func(i *ImageInspection) { i.Config.Volumes = []string{"/data"} }},
		{name: "image label missing", mutate: func(i *ImageInspection) { delete(i.Config.Labels, "ai.vllm.build.commit") }},
		{name: "image label extra", mutate: func(i *ImageInspection) { i.Config.Labels["operator"] = "true" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inspection := cloneImageInspection(valid)
			test.mutate(&inspection)
			if err := ValidateReferenceImage(inspection); !errors.Is(err, ErrImageRejected) {
				t.Fatalf("ValidateReferenceImage() = %v", err)
			}
		})
	}
}

func TestReferenceArchivePathAdmissionIsExact(t *testing.T) {
	for _, path := range []string{ReferenceSnapshotPath, ReferenceTemplatePath} {
		if err := validateReferenceArchivePath(path); err != nil {
			t.Fatalf("validateReferenceArchivePath(%q) = %v", path, err)
		}
	}
	for _, path := range []string{"", "/", ReferenceSnapshotPath + "/config.json", filepath.Dir(ReferenceTemplatePath), "../snapshot"} {
		if err := validateReferenceArchivePath(path); !errors.Is(err, ErrAdmissionRejected) {
			t.Fatalf("validateReferenceArchivePath(%q) = %v", path, err)
		}
	}
}

func TestReferencePlanBuildsFixedCreateSpecWithoutAttestingSecret(t *testing.T) {
	plan, err := buildReferencePlan(deploy.ReferenceDescriptor(), testPaths, testGenerated)
	if err != nil {
		t.Fatal(err)
	}
	spec := plan.cloneCreateSpec()
	wantArgv := append(append([]string(nil), deploy.ReferenceDescriptor().Argv...), "--uds", ReferenceSocketPath)
	if strings.HasSuffix(strings.Join(deploy.ReferenceDescriptor().Argv, "\x00"), "--uds\x00"+ReferenceSocketPath) {
		wantArgv = deploy.ReferenceDescriptor().Argv
	}
	if got := append(append([]string(nil), spec.Entrypoint...), spec.Command...); !reflect.DeepEqual(got, wantArgv) {
		t.Fatalf("effective argv = %#v, want %#v", got, wantArgv)
	}
	if strings.Contains(strings.Join(wantArgv, "\x00"), testAPIKey) {
		t.Fatal("argv contains API key")
	}
	if spec.ImageID != ReferenceImageReference || spec.Name != "purify-r6a-"+testGenerated.RunID ||
		spec.Hostname != spec.Name ||
		spec.NetworkMode != NetworkModeNone || !spec.ReadOnlyRootFS || spec.RestartPolicy != RestartPolicyNo ||
		spec.Privileged || spec.TTY || spec.IPCMode != IPCModePrivate || spec.ShmSizeBytes != 1<<30 ||
		len(spec.ExposedPorts) != 0 || len(spec.PortBindings) != 0 || len(spec.CapAdd) != 0 || len(spec.Links) != 0 {
		t.Fatalf("create spec security boundary drifted: %#v", spec)
	}
	if !reflect.DeepEqual(spec.Mounts, []Mount{
		{Type: MountTypeBind, Source: testPaths.SnapshotDir, Destination: ReferenceSnapshotPath, ReadOnly: true},
		{Type: MountTypeBind, Source: testPaths.TemplateFile, Destination: ReferenceTemplatePath, ReadOnly: true},
		{Type: MountTypeBind, Source: testPaths.RunDir, Destination: ReferenceRuntimePath},
	}) {
		t.Fatalf("mounts = %#v", spec.Mounts)
	}
	if !reflect.DeepEqual(spec.Tmpfs, referenceTmpfs()) || !reflect.DeepEqual(spec.DeviceRequests, referenceDeviceRequests()) {
		t.Fatalf("tmpfs/device request = %#v / %#v", spec.Tmpfs, spec.DeviceRequests)
	}
	if spec.Labels[LabelManaged] != "true" || spec.Labels[LabelComponent] != ReferenceComponent ||
		spec.Labels[LabelRunID] != testGenerated.RunID || spec.Labels[LabelSpecDigest] != plan.specDigest {
		t.Fatalf("labels = %#v", spec.Labels)
	}
	if len(spec.Labels) != len(referenceImageLabels())+4 {
		t.Fatalf("complete image/controller label set = %#v", spec.Labels)
	}
	if countEnvironment(spec.Environment, "VLLM_API_KEY", testAPIKey) != 1 ||
		countEnvironment(spec.Environment, "HF_HUB_OFFLINE", "1") != 1 ||
		countEnvironment(spec.Environment, "VLLM_NO_USAGE_STATS", "1") != 1 ||
		!stringsAreSorted(spec.Environment) {
		t.Fatalf("environment is not the exact deterministic merge: %#v", spec.Environment)
	}

	other, err := buildReferencePlan(deploy.ReferenceDescriptor(), testPaths, generatedInputs{RunID: testGenerated.RunID, APIKey: "different-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.specDigest != other.specDigest {
		t.Fatalf("spec digest depends on secret: %q != %q", plan.specDigest, other.specDigest)
	}
	driftedEnvironment := plan.cloneCreateSpec()
	replaceEnvironment(driftedEnvironment.Environment, "HF_HUB_OFFLINE", "0")
	driftedEnvironmentDigest, err := digestCreateSpec(driftedEnvironment)
	if err != nil || driftedEnvironmentDigest == plan.specDigest {
		t.Fatalf("spec digest did not bind environment: %q, %v", driftedEnvironmentDigest, err)
	}
	driftedNetwork := plan.cloneCreateSpec()
	driftedNetwork.NetworkMode = "bridge"
	driftedNetworkDigest, err := digestCreateSpec(driftedNetwork)
	if err != nil || driftedNetworkDigest == plan.specDigest {
		t.Fatalf("spec digest did not bind network mode: %q, %v", driftedNetworkDigest, err)
	}
	driftedName := plan.cloneCreateSpec()
	driftedName.Name += "-other"
	driftedNameDigest, err := digestCreateSpec(driftedName)
	if err != nil || driftedNameDigest == plan.specDigest {
		t.Fatalf("spec digest did not bind container name: %q, %v", driftedNameDigest, err)
	}
	for _, value := range []string{plan.specDigest, spec.Labels[LabelSpecDigest]} {
		if strings.Contains(value, testAPIKey) {
			t.Fatal("digest/label leaked API key")
		}
	}
	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), testAPIKey) || strings.Contains(string(encoded), "VLLM_API_KEY") {
		t.Fatalf("CreateSpec JSON exposed secret environment: %s", encoded)
	}
}

func TestReferencePlanRejectsInputsWithoutLeakingSecret(t *testing.T) {
	tests := []struct {
		name   string
		paths  HostPaths
		inputs generatedInputs
	}{
		{name: "relative snapshot", paths: HostPaths{SnapshotDir: "snapshot", TemplateFile: testPaths.TemplateFile, RunDir: testPaths.RunDir}, inputs: testGenerated},
		{name: "unclean template", paths: HostPaths{SnapshotDir: testPaths.SnapshotDir, TemplateFile: "/srv/purify/../template", RunDir: testPaths.RunDir}, inputs: testGenerated},
		{name: "root run dir", paths: HostPaths{SnapshotDir: testPaths.SnapshotDir, TemplateFile: testPaths.TemplateFile, RunDir: "/"}, inputs: testGenerated},
		{name: "unbound run dir", paths: HostPaths{SnapshotDir: testPaths.SnapshotDir, TemplateFile: testPaths.TemplateFile, RunDir: "/run/purify-r6a/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}, inputs: testGenerated},
		{name: "overlap", paths: HostPaths{SnapshotDir: testPaths.SnapshotDir, TemplateFile: testPaths.SnapshotDir + "/template", RunDir: testPaths.RunDir}, inputs: testGenerated},
		{name: "bad run id", paths: testPaths, inputs: generatedInputs{RunID: "not-hex", APIKey: testAPIKey}},
		{name: "empty key", paths: testPaths, inputs: generatedInputs{RunID: testGenerated.RunID}},
		{name: "control key", paths: testPaths, inputs: generatedInputs{RunID: testGenerated.RunID, APIKey: testAPIKey + "\n"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := buildReferencePlan(deploy.ReferenceDescriptor(), test.paths, test.inputs)
			if !errors.Is(err, ErrAdmissionRejected) {
				t.Fatalf("buildReferencePlan() error = %v", err)
			}
			if strings.Contains(err.Error(), testAPIKey) {
				t.Fatalf("error leaked secret: %v", err)
			}
		})
	}
}

func TestCreatedInspectionRejectsCreateAndOwnershipDrift(t *testing.T) {
	plan := mustPlan(t)
	owner, err := plan.establishOwnership(CreateResult{ContainerID: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	valid := validCreatedInspection(plan, owner)
	if err := validateCreated(plan, owner, valid); err != nil {
		t.Fatalf("validateCreated(valid) = %v", err)
	}

	tests := []struct {
		name      string
		ownership bool
		mutate    func(*ContainerInspection)
	}{
		{name: "container id", ownership: true, mutate: func(i *ContainerInspection) { i.ID = strings.Repeat("c", 64) }},
		{name: "container name", mutate: func(i *ContainerInspection) { i.Name += "-renamed" }},
		{name: "managed label", ownership: true, mutate: func(i *ContainerInspection) { i.Labels[LabelManaged] = "false" }},
		{name: "run label", ownership: true, mutate: func(i *ContainerInspection) { i.Labels[LabelRunID] = strings.Repeat("d", 32) }},
		{name: "spec label", ownership: true, mutate: func(i *ContainerInspection) { i.Labels[LabelSpecDigest] = strings.Repeat("e", 64) }},
		{name: "extra label", mutate: func(i *ContainerInspection) { i.Labels["operator"] = "true" }},
		{name: "image id", mutate: func(i *ContainerInspection) { i.ImageID = "sha256:" + strings.Repeat("0", 64) }},
		{name: "manifest missing", mutate: func(i *ContainerInspection) { i.ImageManifestDescriptor = nil }},
		{name: "manifest digest", mutate: func(i *ContainerInspection) { i.ImageManifestDescriptor.Digest = "sha256:" + strings.Repeat("0", 64) }},
		{name: "path", mutate: func(i *ContainerInspection) { i.Path = "python" }},
		{name: "args", mutate: func(i *ContainerInspection) { i.Args = append(i.Args, "--api-key", testAPIKey) }},
		{name: "hostname", mutate: func(i *ContainerInspection) { i.Hostname = "operator" }},
		{name: "environment", mutate: func(i *ContainerInspection) {
			i.Environment = append(i.Environment, "AWS_SECRET_ACCESS_KEY="+testAPIKey)
		}},
		{name: "mount readwrite", mutate: func(i *ContainerInspection) { i.Mounts[0].ReadOnly = false }},
		{name: "mount target", mutate: func(i *ContainerInspection) { i.Mounts[1].Destination = "/tmp/template" }},
		{name: "network", mutate: func(i *ContainerInspection) { i.NetworkMode = "bridge" }},
		{name: "network enabled", mutate: func(i *ContainerInspection) { i.NetworkDisabled = false }},
		{name: "exposed port", mutate: func(i *ContainerInspection) { i.ExposedPorts = []string{"8000/tcp"} }},
		{name: "published port", mutate: func(i *ContainerInspection) {
			i.PortBindings = []PortBinding{{ContainerPort: "8000/tcp", HostPort: "8000"}}
		}},
		{name: "writable root", mutate: func(i *ContainerInspection) { i.ReadOnlyRootFS = false }},
		{name: "tmpfs", mutate: func(i *ContainerInspection) { i.Tmpfs[0].SizeBytes-- }},
		{name: "shm", mutate: func(i *ContainerInspection) { i.ShmSizeBytes-- }},
		{name: "ipc host", mutate: func(i *ContainerInspection) { i.IPCMode = "host" }},
		{name: "gpu", mutate: func(i *ContainerInspection) { i.DeviceRequests[0].Count = -1 }},
		{name: "privileged", mutate: func(i *ContainerInspection) { i.Privileged = true }},
		{name: "capability", mutate: func(i *ContainerInspection) { i.CapAdd = []string{"SYS_ADMIN"} }},
		{name: "restart", mutate: func(i *ContainerInspection) { i.RestartPolicy = "always" }},
		{name: "tty", mutate: func(i *ContainerInspection) { i.TTY = true }},
		{name: "link", mutate: func(i *ContainerInspection) { i.Links = []string{"db"} }},
		{name: "running early", mutate: func(i *ContainerInspection) {
			i.State.Status = ContainerStatusRunning
			i.State.Running = true
			i.State.PID = 42
		}},
		{name: "restart count", mutate: func(i *ContainerInspection) { i.RestartCount = 1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inspection := cloneContainerInspection(valid)
			test.mutate(&inspection)
			err := validateCreated(plan, owner, inspection)
			if test.ownership {
				if !errors.Is(err, ErrOwnershipLost) {
					t.Fatalf("validateCreated() = %v", err)
				}
				return
			}
			if !errors.Is(err, ErrContainerRejected) {
				t.Fatalf("validateCreated() = %v", err)
			}
			if strings.Contains(err.Error(), testAPIKey) {
				t.Fatalf("error leaked secret: %v", err)
			}
		})
	}
}

func TestOwnershipInspectionBindsExactCanonicalContainerName(t *testing.T) {
	plan := mustPlan(t)
	owner, err := plan.establishOwnership(CreateResult{ContainerID: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if owner.containerName != plan.create.Name {
		t.Fatalf("owner container name = %q, want %q", owner.containerName, plan.create.Name)
	}
	valid := OwnershipInspection{
		ID: owner.containerID, Name: "/" + owner.containerName,
		Labels: cloneMap(owner.labels), State: ContainerState{Status: ContainerStatusCreated},
	}
	if err := validateOwnershipInspection(owner, valid); err != nil {
		t.Fatalf("validateOwnershipInspection(valid) = %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*OwnershipInspection)
	}{
		{name: "bare name", mutate: func(value *OwnershipInspection) { value.Name = owner.containerName }},
		{name: "renamed", mutate: func(value *OwnershipInspection) { value.Name += "-renamed" }},
		{name: "id", mutate: func(value *OwnershipInspection) { value.ID = strings.Repeat("c", 64) }},
		{name: "labels", mutate: func(value *OwnershipInspection) { value.Labels[LabelManaged] = "false" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			inspection := valid
			inspection.Labels = cloneMap(valid.Labels)
			test.mutate(&inspection)
			if err := validateOwnershipInspection(owner, inspection); !errors.Is(err, ErrOwnershipLost) {
				t.Fatalf("validateOwnershipInspection() = %v", err)
			}
		})
	}
}

func TestRunningInspectionRequiresStablePIDProcessEnvironmentAndFreshSocket(t *testing.T) {
	plan := mustPlan(t)
	owner, err := plan.establishOwnership(CreateResult{ContainerID: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	valid := validRunningInspection(plan, owner)
	if err := validateRunning(plan, owner, valid); err != nil {
		t.Fatalf("validateRunning(valid) = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*RunningInspection)
	}{
		{name: "before not running", mutate: func(i *RunningInspection) { i.Before.State.Running = false }},
		{name: "after pid changed", mutate: func(i *RunningInspection) { i.After.State.PID++ }},
		{name: "process pid changed", mutate: func(i *RunningInspection) { i.Process.PID++ }},
		{name: "missing process env", mutate: func(i *RunningInspection) {
			i.Process.Environment = i.Process.Environment[:len(i.Process.Environment)-1]
		}},
		{name: "extra process env", mutate: func(i *RunningInspection) { i.Process.Environment = append(i.Process.Environment, "HOME=/root") }},
		{name: "wrong process key", mutate: func(i *RunningInspection) { replaceEnvironment(i.Process.Environment, "VLLM_API_KEY", "wrong") }},
		{name: "wrong selected gpu", mutate: func(i *RunningInspection) {
			replaceExactEnvironment(i.Process.Environment, "NVIDIA_VISIBLE_DEVICES=0", "NVIDIA_VISIBLE_DEVICES=1")
		}},
		{name: "wrong gpu capabilities", mutate: func(i *RunningInspection) {
			replaceExactEnvironment(i.Process.Environment, "NVIDIA_DRIVER_CAPABILITIES=compute", "NVIDIA_DRIVER_CAPABILITIES=utility")
		}},
		{name: "process env order", mutate: func(i *RunningInspection) {
			i.Process.Environment[0], i.Process.Environment[1] = i.Process.Environment[1], i.Process.Environment[0]
		}},
		{name: "socket path", mutate: func(i *RunningInspection) { i.Socket.Path += ".other" }},
		{name: "not socket", mutate: func(i *RunningInspection) { i.Socket.IsSocket = false }},
		{name: "socket symlink", mutate: func(i *RunningInspection) { i.Socket.IsSymlink = true }},
		{name: "socket existed", mutate: func(i *RunningInspection) { i.Socket.Fresh = false }},
		{name: "after config drift", mutate: func(i *RunningInspection) { i.After.ReadOnlyRootFS = false }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inspection := cloneRunningInspection(valid)
			test.mutate(&inspection)
			if err := validateRunning(plan, owner, inspection); !errors.Is(err, ErrContainerRejected) {
				t.Fatalf("validateRunning() = %v", err)
			}
		})
	}
}

func TestFinalInspectionRequiresSameOwnedExitedContainer(t *testing.T) {
	plan := mustPlan(t)
	owner, err := plan.establishOwnership(CreateResult{ContainerID: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	final := validCreatedInspection(plan, owner)
	final.State = ContainerState{Status: ContainerStatusExited, ExitCode: 137}
	if err := validateFinal(plan, owner, final); err != nil {
		t.Fatalf("validateFinal(valid) = %v", err)
	}

	configDrift := cloneContainerInspection(final)
	configDrift.NetworkMode = "bridge"
	if err := validateFinal(plan, owner, configDrift); !errors.Is(err, ErrContainerRejected) {
		t.Fatalf("validateFinal(config drift) = %v", err)
	}
	identityDrift := cloneContainerInspection(final)
	identityDrift.ID = strings.Repeat("c", 64)
	if err := validateFinal(plan, owner, identityDrift); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("validateFinal(identity drift) = %v", err)
	}
	restarted := cloneContainerInspection(final)
	restarted.RestartCount = 1
	if err := validateFinal(plan, owner, restarted); !errors.Is(err, ErrContainerRejected) {
		t.Fatalf("validateFinal(restart) = %v", err)
	}
	running := cloneContainerInspection(final)
	running.State = ContainerState{Status: ContainerStatusRunning, Running: true, PID: 42}
	if err := validateFinal(plan, owner, running); !errors.Is(err, ErrContainerRejected) {
		t.Fatalf("validateFinal(running) = %v", err)
	}
}

func TestLifecycleOnlyAdvancesForOwnedMonotonicEvents(t *testing.T) {
	plan := mustPlan(t)
	owner, err := plan.establishOwnership(CreateResult{ContainerID: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	state := LifecycleNew
	for _, step := range []struct {
		action string
		want   LifecycleState
	}{
		{action: LifecycleActionCreate, want: LifecycleCreated},
		{action: LifecycleActionStart, want: LifecycleRunning},
		{action: LifecycleActionKill, want: LifecycleStopping},
		{action: LifecycleActionDie, want: LifecycleExited},
		{action: LifecycleActionDestroy, want: LifecycleRemoved},
	} {
		state, err = advanceLifecycle(state, owner, LifecycleEvent{ContainerID: owner.containerID, Action: step.action, Labels: cloneMap(plan.create.Labels)})
		if err != nil || state != step.want {
			t.Fatalf("advanceLifecycle(%q) = %q, %v", step.action, state, err)
		}
	}

	for _, test := range []struct {
		name  string
		state LifecycleState
		event LifecycleEvent
		want  error
	}{
		{name: "restart", state: LifecycleRunning, event: LifecycleEvent{ContainerID: owner.containerID, Action: "restart", Labels: cloneMap(plan.create.Labels)}, want: ErrLifecycleDrift},
		{name: "start twice", state: LifecycleRunning, event: LifecycleEvent{ContainerID: owner.containerID, Action: LifecycleActionStart, Labels: cloneMap(plan.create.Labels)}, want: ErrLifecycleDrift},
		{name: "other id", state: LifecycleCreated, event: LifecycleEvent{ContainerID: strings.Repeat("c", 64), Action: LifecycleActionStart, Labels: cloneMap(plan.create.Labels)}, want: ErrOwnershipLost},
		{name: "other labels", state: LifecycleCreated, event: LifecycleEvent{ContainerID: owner.containerID, Action: LifecycleActionStart, Labels: map[string]string{}}, want: ErrOwnershipLost},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := advanceLifecycle(test.state, owner, test.event); !errors.Is(err, test.want) {
				t.Fatalf("advanceLifecycle() = %v", err)
			}
		})
	}

	state = LifecycleRunning
	state, err = advanceLifecycle(state, owner, LifecycleEvent{ContainerID: owner.containerID, Action: LifecycleActionDie, Labels: cloneMap(plan.create.Labels)})
	if err != nil || state != LifecycleExited {
		t.Fatalf("natural exit = %q, %v", state, err)
	}
}

func TestEvidenceIsRedactedOutputOnlyAndCertificationStaysClosed(t *testing.T) {
	plan := mustPlan(t)
	owner, err := plan.establishOwnership(CreateResult{ContainerID: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	running := validRunningInspection(plan, owner)
	evidence, err := newRunningEvidence(plan, owner, validImageInspection(), running)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	var output struct {
		RecordingOnly bool     `json:"recording_only"`
		Environment   []string `json:"redacted_environment"`
	}
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), testAPIKey) || !output.RecordingOnly ||
		countEnvironment(output.Environment, "VLLM_API_KEY", "<present>") != 1 ||
		!strings.Contains(string(encoded), plan.specDigest) || !strings.Contains(string(encoded), owner.containerID) {
		t.Fatalf("evidence is not safely bound/redacted: %s", encoded)
	}
	var decoded Evidence
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, Evidence{}) {
		t.Fatalf("Evidence unexpectedly accepted serialized authority: %#v", decoded)
	}
	if err := deploy.RequireCertifiedDeployment(plan.descriptor.ProfileID); !errors.Is(err, deploy.ErrProfileUnavailable) {
		t.Fatalf("RequireCertifiedDeployment() = %v", err)
	}
}

func mustPlan(t *testing.T) *referencePlan {
	t.Helper()
	plan, err := buildReferencePlan(deploy.ReferenceDescriptor(), testPaths, testGenerated)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func validImageInspection() ImageInspection {
	return ImageInspection{
		RequestedReference: ReferenceImageReference,
		ID:                 ReferenceImageManifestDigest,
		OS:                 "linux",
		Architecture:       "amd64",
		ManifestDescriptor: &ManifestDescriptor{Digest: deploy.ReferenceDescriptor().ImageDigest, MediaType: ReferenceManifestMediaType},
		Config: ImageConfigInspection{
			Entrypoint:  []string{"vllm", "serve"},
			Environment: referenceImageEnvironment(),
			WorkingDir:  "/vllm-workspace",
			Labels:      referenceImageLabels(),
		},
	}
}

func validCreatedInspection(plan *referencePlan, owner ownership) ContainerInspection {
	spec := plan.cloneCreateSpec()
	return ContainerInspection{
		ID:                      owner.containerID,
		Name:                    "/" + spec.Name,
		ImageID:                 ReferenceImageManifestDigest,
		ConfiguredImage:         ReferenceImageReference,
		ImageManifestDescriptor: &ManifestDescriptor{Digest: plan.descriptor.ImageDigest, MediaType: ReferenceManifestMediaType},
		Platform:                plan.descriptor.Platform,
		Path:                    spec.Entrypoint[0],
		Args:                    append(append([]string(nil), spec.Entrypoint[1:]...), spec.Command...),
		Hostname:                spec.Hostname,
		Entrypoint:              append([]string(nil), spec.Entrypoint...),
		Command:                 append([]string(nil), spec.Command...),
		Environment:             append([]string(nil), spec.Environment...),
		User:                    spec.User,
		WorkingDir:              spec.WorkingDir,
		Labels:                  cloneMap(spec.Labels),
		Mounts:                  append([]Mount(nil), spec.Mounts...),
		Tmpfs:                   append([]TmpfsMount(nil), spec.Tmpfs...),
		DeviceRequests:          cloneDeviceRequests(spec.DeviceRequests),
		NetworkMode:             spec.NetworkMode,
		NetworkDisabled:         true,
		ReadOnlyRootFS:          spec.ReadOnlyRootFS,
		RestartPolicy:           spec.RestartPolicy,
		LogDriver:               spec.LogDriver,
		Privileged:              spec.Privileged,
		TTY:                     spec.TTY,
		IPCMode:                 spec.IPCMode,
		ShmSizeBytes:            spec.ShmSizeBytes,
		State:                   ContainerState{Status: ContainerStatusCreated},
	}
}

func validRunningInspection(plan *referencePlan, owner ownership) RunningInspection {
	before := validCreatedInspection(plan, owner)
	before.State = ContainerState{Status: ContainerStatusRunning, Running: true, PID: 4242}
	after := cloneContainerInspection(before)
	processEnvironment := make([]string, 0, len(plan.create.Environment)+3)
	for _, entry := range plan.create.Environment {
		if strings.HasPrefix(entry, "PATH=") {
			processEnvironment = append(processEnvironment, entry)
		}
	}
	processEnvironment = append(processEnvironment, "HOSTNAME="+plan.create.Hostname)
	for _, entry := range plan.create.Environment {
		if !strings.HasPrefix(entry, "PATH=") {
			processEnvironment = append(processEnvironment, entry)
		}
	}
	processEnvironment = append(processEnvironment,
		"NVIDIA_VISIBLE_DEVICES=0",
		"NVIDIA_DRIVER_CAPABILITIES=compute",
	)
	return RunningInspection{
		Before:  before,
		Process: ProcessInspection{PID: 4242, Environment: processEnvironment},
		After:   after,
		Socket:  SocketInspection{Path: testPaths.RunDir + "/vllm.sock", IsSocket: true, Fresh: true},
	}
}

func cloneImageInspection(value ImageInspection) ImageInspection {
	clone := value
	if value.ManifestDescriptor != nil {
		descriptor := *value.ManifestDescriptor
		clone.ManifestDescriptor = &descriptor
	}
	clone.Config.Entrypoint = append([]string(nil), value.Config.Entrypoint...)
	clone.Config.Cmd = append([]string(nil), value.Config.Cmd...)
	clone.Config.Environment = append([]string(nil), value.Config.Environment...)
	clone.Config.ExposedPorts = append([]string(nil), value.Config.ExposedPorts...)
	clone.Config.Volumes = append([]string(nil), value.Config.Volumes...)
	clone.Config.Labels = cloneMap(value.Config.Labels)
	return clone
}

func cloneContainerInspection(value ContainerInspection) ContainerInspection {
	clone := value
	if value.ImageManifestDescriptor != nil {
		descriptor := *value.ImageManifestDescriptor
		clone.ImageManifestDescriptor = &descriptor
	}
	clone.Args = append([]string(nil), value.Args...)
	clone.Entrypoint = append([]string(nil), value.Entrypoint...)
	clone.Command = append([]string(nil), value.Command...)
	clone.Environment = append([]string(nil), value.Environment...)
	clone.Labels = cloneMap(value.Labels)
	clone.Mounts = append([]Mount(nil), value.Mounts...)
	clone.Tmpfs = append([]TmpfsMount(nil), value.Tmpfs...)
	clone.DeviceRequests = cloneDeviceRequests(value.DeviceRequests)
	clone.ExposedPorts = append([]string(nil), value.ExposedPorts...)
	clone.PortBindings = append([]PortBinding(nil), value.PortBindings...)
	clone.CapAdd = append([]string(nil), value.CapAdd...)
	clone.Links = append([]string(nil), value.Links...)
	return clone
}

func cloneRunningInspection(value RunningInspection) RunningInspection {
	return RunningInspection{
		Before: cloneContainerInspection(value.Before),
		Process: ProcessInspection{
			PID:         value.Process.PID,
			Environment: append([]string(nil), value.Process.Environment...),
		},
		After:  cloneContainerInspection(value.After),
		Socket: value.Socket,
	}
}

func countEnvironment(environment []string, name, value string) int {
	count := 0
	for _, entry := range environment {
		if entry == name+"="+value {
			count++
		}
	}
	return count
}

func replaceEnvironment(environment []string, name, value string) {
	for index, entry := range environment {
		if strings.HasPrefix(entry, name+"=") {
			environment[index] = name + "=" + value
			return
		}
	}
}

func replaceExactEnvironment(environment []string, old, value string) {
	for index := range environment {
		if environment[index] == old {
			environment[index] = value
			return
		}
	}
}

func stringsAreSorted(values []string) bool {
	for index := 1; index < len(values); index++ {
		if values[index-1] >= values[index] {
			return false
		}
	}
	return true
}

func sortStrings(values []string) {
	for index := 1; index < len(values); index++ {
		for inner := index; inner > 0 && values[inner] < values[inner-1]; inner-- {
			values[inner], values[inner-1] = values[inner-1], values[inner]
		}
	}
}
