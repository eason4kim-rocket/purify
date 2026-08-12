package dockerengine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/use-agent/purify/search/rerank/deploy"
)

const (
	referenceSpecDigestVersion  = "rerank-docker-create-spec-v2"
	referenceEvidenceEnvVersion = "rerank-effective-env-v2"
	maximumHostPathBytes        = 4096
	maximumSecretBytes          = 16 << 10
)

// ReferenceImageReference is the only reference accepted for pre-inspection
// and create. The containerd image store resolves this named child-manifest
// digest; the manifest itself binds the independently pinned config digest.
const ReferenceImageReference = ReferenceImageRepository + "@" + ReferenceImageManifestDigest

type generatedInputs struct {
	RunID  string
	APIKey string
}

type referencePlan struct {
	descriptor deploy.Descriptor
	create     CreateSpec
	specDigest string
	apiKey     string
}

func buildReferencePlan(descriptor deploy.Descriptor, paths HostPaths, generated generatedInputs) (*referencePlan, error) {
	if !reflect.DeepEqual(descriptor, deploy.ReferenceDescriptor()) ||
		!validHostPaths(paths) || !validLowerHex(generated.RunID, 32) ||
		paths.RunDir != filepath.Join("/run/purify-r6a", generated.RunID) || !validSecret(generated.APIKey) {
		return nil, ErrAdmissionRejected
	}

	argv := referenceArgv(descriptor)
	if len(argv) < 3 || !reflect.DeepEqual(argv[:2], []string{"vllm", "serve"}) {
		return nil, ErrAdmissionRejected
	}
	environment, ok := referenceEffectiveEnvironment(generated.APIKey)
	if !ok {
		return nil, ErrAdmissionRejected
	}
	containerName := "purify-r6a-" + generated.RunID
	create := CreateSpec{
		ImageID:     ReferenceImageReference,
		Name:        containerName,
		Hostname:    containerName,
		Entrypoint:  append([]string(nil), argv[:2]...),
		Command:     append([]string(nil), argv[2:]...),
		Environment: environment,
		WorkingDir:  "/vllm-workspace",
		Labels:      referenceImageLabels(),
		Mounts: []Mount{
			{Type: MountTypeBind, Source: paths.SnapshotDir, Destination: ReferenceSnapshotPath, ReadOnly: true},
			{Type: MountTypeBind, Source: paths.TemplateFile, Destination: ReferenceTemplatePath, ReadOnly: true},
			{Type: MountTypeBind, Source: paths.RunDir, Destination: ReferenceRuntimePath},
		},
		Tmpfs:          referenceTmpfs(),
		DeviceRequests: referenceDeviceRequests(),
		NetworkMode:    NetworkModeNone,
		IPCMode:        IPCModePrivate,
		ShmSizeBytes:   1 << 30,
		ReadOnlyRootFS: true,
		RestartPolicy:  RestartPolicyNo,
		LogDriver:      LogDriverNone,
	}
	create.Labels[LabelManaged] = "true"
	create.Labels[LabelComponent] = ReferenceComponent
	create.Labels[LabelRunID] = generated.RunID
	digest, err := digestCreateSpec(create)
	if err != nil {
		return nil, ErrAdmissionRejected
	}
	create.Labels[LabelSpecDigest] = digest
	return &referencePlan{
		descriptor: descriptor,
		create:     create,
		specDigest: digest,
		apiKey:     generated.APIKey,
	}, nil
}

func referenceImageLabels() map[string]string {
	return map[string]string{
		"ai.vllm.build.commit":              "91df0fad4dc98a67c7659d9dbd915245d5c43d96",
		"ai.vllm.build.pipeline":            "019d130e-464e-4ff7-b84b-492992c0c06b",
		"ai.vllm.build.url":                 "https://buildkite.com/vllm/release-v2/builds/2657",
		"ai.vllm.image.tag":                 "vllm/vllm-openai:v0.23.0",
		"maintainer":                        "NVIDIA CORPORATION <cudatools@nvidia.com>",
		"org.opencontainers.image.ref.name": "ubuntu",
		"org.opencontainers.image.revision": "91df0fad4dc98a67c7659d9dbd915245d5c43d96",
		"org.opencontainers.image.source":   "https://github.com/vllm-project/vllm",
		"org.opencontainers.image.url":      "https://buildkite.com/vllm/release-v2/builds/2657",
		"org.opencontainers.image.version":  "vllm/vllm-openai:v0.23.0",
	}
}

func (plan *referencePlan) cloneCreateSpec() CreateSpec {
	return cloneCreateSpec(plan.create)
}

func referenceArgv(descriptor deploy.Descriptor) []string {
	argv := append([]string(nil), descriptor.Argv...)
	for index, argument := range argv {
		if argument == "--uds" || strings.HasPrefix(argument, "--uds=") {
			if index+1 == len(argv) || argument != "--uds" || argv[index+1] != ReferenceSocketPath || index+2 != len(argv) {
				return nil
			}
			return argv
		}
	}
	return append(argv, "--uds", ReferenceSocketPath)
}

func referenceImageEnvironment() []string {
	return deploy.ReferenceImageBaselineEnvironment()
}

func referenceEffectiveEnvironment(apiKey string) ([]string, bool) {
	overlay, _, err := deploy.BuildReferenceEnvironment([]deploy.EnvironmentValue{
		{Name: "HF_HUB_OFFLINE", Value: "1"},
		{Name: "VLLM_API_KEY", Value: apiKey, Secret: true},
		{Name: "VLLM_NO_USAGE_STATS", Value: "1"},
	}, deploy.SecretBinding{PurifyValue: apiKey, SidecarValue: apiKey})
	if err != nil {
		return nil, false
	}
	environment, attestation, err := deploy.MergeReferenceEnvironment(deploy.ReferenceImageBaselineEnvironment(), overlay)
	if err != nil || attestation.EnvironmentDigest != deploy.ReferenceEnvironmentDigest {
		return nil, false
	}
	return environment, true
}

func referenceTmpfs() []TmpfsMount {
	return []TmpfsMount{
		{Destination: "/tmp", SizeBytes: 1 << 30, Mode: 0o1777},
		{Destination: "/root/.cache/vllm", SizeBytes: 1 << 30, Mode: 0o700},
		{Destination: "/root/.triton", SizeBytes: 512 << 20, Mode: 0o700},
	}
}

func referenceDeviceRequests() []DeviceRequest {
	return []DeviceRequest{{
		Driver:       "nvidia",
		Count:        1,
		Capabilities: [][]string{{"gpu", "nvidia", "compute"}},
	}}
}

func digestCreateSpec(spec CreateSpec) (string, error) {
	redacted := cloneCreateSpec(spec)
	for index, entry := range redacted.Environment {
		if strings.HasPrefix(entry, "VLLM_API_KEY=") {
			redacted.Environment[index] = "VLLM_API_KEY=<present>"
		}
	}
	delete(redacted.Labels, LabelSpecDigest)
	encoded, err := json.Marshal(struct {
		Spec        CreateSpec `json:"spec"`
		Environment []string   `json:"environment"`
	}{
		Spec:        redacted,
		Environment: redacted.Environment,
	})
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(referenceSpecDigestVersion + "\x00"))
	_, _ = hash.Write(encoded)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func validHostPaths(paths HostPaths) bool {
	values := []string{paths.SnapshotDir, paths.TemplateFile, paths.RunDir}
	for _, value := range values {
		if len(value) == 0 || len(value) > maximumHostPathBytes || !utf8.ValidString(value) ||
			!filepath.IsAbs(value) || filepath.Clean(value) != value || value == string(filepath.Separator) {
			return false
		}
		for _, character := range value {
			if unicode.IsControl(character) {
				return false
			}
		}
	}
	for left := range values {
		for right := left + 1; right < len(values); right++ {
			if pathsOverlap(values[left], values[right]) {
				return false
			}
		}
	}
	return true
}

func pathsOverlap(left, right string) bool {
	for _, pair := range [][2]string{{left, right}, {right, left}} {
		relative, err := filepath.Rel(pair[0], pair[1])
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func validSecret(value string) bool {
	if len(value) == 0 || len(value) > maximumSecretBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validLowerHex(value string, size int) bool {
	if len(value) != size {
		return false
	}
	for _, character := range value {
		if !(character >= '0' && character <= '9') && !(character >= 'a' && character <= 'f') {
			return false
		}
	}
	return true
}

func cloneCreateSpec(value CreateSpec) CreateSpec {
	clone := value
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

func cloneDeviceRequests(values []DeviceRequest) []DeviceRequest {
	clones := make([]DeviceRequest, len(values))
	for index, value := range values {
		clones[index] = value
		clones[index].DeviceIDs = append([]string(nil), value.DeviceIDs...)
		clones[index].Capabilities = make([][]string, len(value.Capabilities))
		for capabilityIndex, capabilities := range value.Capabilities {
			clones[index].Capabilities[capabilityIndex] = append([]string(nil), capabilities...)
		}
		clones[index].Options = cloneMap(value.Options)
	}
	return clones
}

func cloneMap(value map[string]string) map[string]string {
	if value == nil {
		return nil
	}
	clone := make(map[string]string, len(value))
	for key, item := range value {
		clone[key] = item
	}
	return clone
}
