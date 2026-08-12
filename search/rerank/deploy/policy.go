// Package deploy defines the pure, pre-exec descriptor and environment policy
// for the pinned reranker sidecar. It does not claim to authenticate Docker or
// OCI runtime state: the positive supervisor/backend remains unavailable until
// a concrete runtime evidence channel and R-6 manifest are committed.
package deploy

import (
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maximumSecretBytes = 16 << 10

const (
	ReferenceEnvironmentPolicyVersion         = "rerank-env-v2"
	ReferenceOverlayEnvironmentDigest         = "21b4e2eaf503da54e9db0ef6f99ebf71d12471d37cc994c59f819b42dd2ab5bd"
	ReferenceImageBaselineEnvironmentDigest   = "1bdf7468993a8748565059dc00ea30b28a1cf67b01327be3675aee4d77c1abf1"
	ReferenceEnvironmentDigest                = "90d6a698c89f10d716c874648f7b316b7899ce6089a4e39936fd290cd9de4617"
	ReferenceSnapshotManifestSHA256           = "f4769df1fce7a8bff3d1e5f1f913e8e5e1ddedf9906db423b27be7be6a906c65"
	ReferenceImageConfigID                    = "sha256:f37691f675bb82f734f606de8af90e777d3f80a20b120e699fd43fd10e60b8d7"
	ReferenceSnapshotPath                     = "/root/.cache/huggingface/hub/models--Qwen--Qwen3-Reranker-0.6B/snapshots/e61197ed45024b0ed8a2d74b80b4d909f1255473"
	ReferenceUDSPath                          = "/run/purify/vllm.sock"
	referenceOverlayEnvironmentDigestDomain   = "rerank-overlay-env-v2\x00"
	referenceImageEnvironmentDigestDomain     = "rerank-image-env-v1\x00"
	referenceEffectiveEnvironmentDigestDomain = "rerank-effective-env-v2\x00"
)

// referenceTemplate is copied verbatim from vLLM v0.23.0
// examples/pooling/score/template/qwen3_reranker.jinja (Apache-2.0); its
// upstream digest is part of the recording tuple.
//
//go:embed assets/qwen3_reranker.jinja
var referenceTemplate []byte

//go:embed assets/qwen3-reranker-0.6b.snapshot.json
var referenceSnapshotManifest []byte

var (
	ErrAdmissionRejected  = errors.New("rerank deploy: admission rejected")
	ErrProfileUnavailable = errors.New("rerank deploy: certified profile is unavailable")
)

type EnvironmentValue struct {
	Name   string
	Value  string
	Secret bool
}

type SecretBinding struct {
	PurifyReference  string
	SidecarReference string
	PurifyValue      string
	SidecarValue     string
}

type EnvironmentAttestation struct {
	RedactedEnvironment []string
	EnvironmentDigest   string
	APIAuth             string
	APIKeyPresent       bool
}

// EnvironmentOverlayAttestation describes only the three entries supplied by
// the supervisor. It must not be mistaken for the effective child environment,
// which also contains the authenticated image baseline.
type EnvironmentOverlayAttestation struct {
	RedactedOverlay []string
	OverlayDigest   string
	APIAuth         string
	APIKeyPresent   bool
}

type Descriptor struct {
	ProfileID                      string
	Platform                       string
	VLLMVersion                    string
	ImageDigest                    string
	ImageIndexDigest               string
	ImageConfigID                  string
	ImageEntrypoint                []string
	ModelRevision                  string
	TokenizerRevision              string
	ServedModel                    string
	Instruction                    string
	InstructionVersion             string
	TemplateSHA256                 string
	TemplatePath                   string
	SnapshotPath                   string
	SnapshotManifestSHA256         string
	UDSPath                        string
	Runner                         string
	MaxModelLen                    int
	HFOverrides                    string
	ScoreMinimum                   float64
	ScoreMaximum                   float64
	PrefixCaching                  bool
	Route                          string
	EnvironmentPolicyVersion       string
	ImageBaselineEnvironmentDigest string
	EnvironmentDigest              string
	APIAuth                        string
	APIKeyRequired                 bool
	Argv                           []string
	RequiresPrivateIngress         bool
	RequiresNoHostPublish          bool
	RequiresDefaultDenyEgress      bool
	Certified                      bool
	ManifestID                     string
}

var referenceArgv = []string{
	"vllm", "serve", "Qwen/Qwen3-Reranker-0.6B",
	"--revision", "e61197ed45024b0ed8a2d74b80b4d909f1255473",
	"--tokenizer-revision", "e61197ed45024b0ed8a2d74b80b4d909f1255473",
	"--served-model-name", "Qwen/Qwen3-Reranker-0.6B",
	"--runner", "pooling",
	"--max-model-len", "8192",
	"--no-enable-prefix-caching",
	"--hf-overrides", `{"architectures":["Qwen3ForSequenceClassification"],"classifier_from_token":["no","yes"],"is_original_qwen3_reranker":true}`,
	"--chat-template", "/run/purify/qwen3_reranker.jinja",
	"--uds", ReferenceUDSPath,
}

var referenceImageEntrypoint = []string{"vllm", "serve"}

var referenceImageBaselineEnvironment = []string{
	"PATH=/usr/local/nvidia/bin:/usr/local/cuda/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	"NVARCH=x86_64",
	"NVIDIA_REQUIRE_CUDA=cuda>=13.0 brand=unknown,driver>=535,driver<536 brand=grid,driver>=535,driver<536 brand=tesla,driver>=535,driver<536 brand=nvidia,driver>=535,driver<536 brand=quadro,driver>=535,driver<536 brand=quadrortx,driver>=535,driver<536 brand=nvidiartx,driver>=535,driver<536 brand=vapps,driver>=535,driver<536 brand=vpc,driver>=535,driver<536 brand=vcs,driver>=535,driver<536 brand=vws,driver>=535,driver<536 brand=cloudgaming,driver>=535,driver<536 brand=unknown,driver>=550,driver<551 brand=grid,driver>=550,driver<551 brand=tesla,driver>=550,driver<551 brand=nvidia,driver>=550,driver<551 brand=quadro,driver>=550,driver<551 brand=quadrortx,driver>=550,driver<551 brand=nvidiartx,driver>=550,driver<551 brand=vapps,driver>=550,driver<551 brand=vpc,driver>=550,driver<551 brand=vcs,driver>=550,driver<551 brand=vws,driver>=550,driver<551 brand=cloudgaming,driver>=550,driver<551 brand=unknown,driver>=565,driver<566 brand=grid,driver>=565,driver<566 brand=tesla,driver>=565,driver<566 brand=nvidia,driver>=565,driver<566 brand=quadro,driver>=565,driver<566 brand=quadrortx,driver>=565,driver<566 brand=nvidiartx,driver>=565,driver<566 brand=vapps,driver>=565,driver<566 brand=vpc,driver>=565,driver<566 brand=vcs,driver>=565,driver<566 brand=vws,driver>=565,driver<566 brand=cloudgaming,driver>=565,driver<566 brand=unknown,driver>=570,driver<571 brand=grid,driver>=570,driver<571 brand=tesla,driver>=570,driver<571 brand=nvidia,driver>=570,driver<571 brand=quadro,driver>=570,driver<571 brand=quadrortx,driver>=570,driver<571 brand=nvidiartx,driver>=570,driver<571 brand=vapps,driver>=570,driver<571 brand=vpc,driver>=570,driver<571 brand=vcs,driver>=570,driver<571 brand=vws,driver>=570,driver<571 brand=cloudgaming,driver>=570,driver<571 brand=unknown,driver>=575,driver<576 brand=grid,driver>=575,driver<576 brand=tesla,driver>=575,driver<576 brand=nvidia,driver>=575,driver<576 brand=quadro,driver>=575,driver<576 brand=quadrortx,driver>=575,driver<576 brand=nvidiartx,driver>=575,driver<576 brand=vapps,driver>=575,driver<576 brand=vpc,driver>=575,driver<576 brand=vcs,driver>=575,driver<576 brand=vws,driver>=575,driver<576 brand=cloudgaming,driver>=575,driver<576",
	"NV_CUDA_CUDART_VERSION=13.0.96-1",
	"CUDA_VERSION=13.0.2",
	"LD_LIBRARY_PATH=/usr/local/nvidia/lib64:/usr/local/cuda/lib64:/usr/local/nvidia/lib:/usr/local/nvidia/lib64:/usr/local/cuda/lib64",
	"NVIDIA_VISIBLE_DEVICES=all",
	"NVIDIA_DRIVER_CAPABILITIES=compute,utility",
	"DEBIAN_FRONTEND=noninteractive",
	"UV_HTTP_TIMEOUT=500",
	"UV_INDEX_STRATEGY=unsafe-best-match",
	"UV_LINK_MODE=copy",
	"UV_PYTHON_INSTALL_DIR=/opt/uv/python",
	"UV_CACHE_DIR=/opt/uv/cache",
	"VLLM_ENABLE_CUDA_COMPATIBILITY=0",
	"TORCH_CUDA_ARCH_LIST=7.5 8.0 8.6 8.9 9.0 10.0 12.0+PTX",
	"VLLM_USAGE_SOURCE=production-docker-image",
	"VLLM_BUILD_COMMIT=91df0fad4dc98a67c7659d9dbd915245d5c43d96",
	"VLLM_BUILD_PIPELINE=019d130e-464e-4ff7-b84b-492992c0c06b",
	"VLLM_BUILD_URL=https://buildkite.com/vllm/release-v2/builds/2657",
	"VLLM_IMAGE_TAG=vllm/vllm-openai:v0.23.0",
}

func ReferenceDescriptor() Descriptor {
	return Descriptor{
		ProfileID:                      "qwen3-reranker-0.6b-v1",
		Platform:                       "linux/amd64",
		VLLMVersion:                    "v0.23.0",
		ImageDigest:                    "sha256:3a1e7f5904e1a1192a02aa0086ceaffc33985d7044c7bb25b3a43d61bdbe3ac0",
		ImageIndexDigest:               "sha256:6d8429e38e3747723ca07ee1b17972e09bb9c51c4032b266f24fb1cc3b22ed8f",
		ImageConfigID:                  ReferenceImageConfigID,
		ImageEntrypoint:                append([]string(nil), referenceImageEntrypoint...),
		ModelRevision:                  "e61197ed45024b0ed8a2d74b80b4d909f1255473",
		TokenizerRevision:              "e61197ed45024b0ed8a2d74b80b4d909f1255473",
		ServedModel:                    "Qwen/Qwen3-Reranker-0.6B",
		Instruction:                    "Given a web search query, retrieve relevant passages that answer the query",
		InstructionVersion:             "qwen3-reranker-instruction-v1",
		TemplateSHA256:                 "e1ee98e69aab7b2da366edf1c50efcef37e34b4a0c50fb816336213e68d9047a",
		TemplatePath:                   "/run/purify/qwen3_reranker.jinja",
		SnapshotPath:                   ReferenceSnapshotPath,
		SnapshotManifestSHA256:         ReferenceSnapshotManifestSHA256,
		UDSPath:                        ReferenceUDSPath,
		Runner:                         "pooling",
		MaxModelLen:                    8192,
		HFOverrides:                    `{"architectures":["Qwen3ForSequenceClassification"],"classifier_from_token":["no","yes"],"is_original_qwen3_reranker":true}`,
		ScoreMinimum:                   0,
		ScoreMaximum:                   1,
		PrefixCaching:                  false,
		Route:                          "/v1/rerank",
		EnvironmentPolicyVersion:       ReferenceEnvironmentPolicyVersion,
		ImageBaselineEnvironmentDigest: ReferenceImageBaselineEnvironmentDigest,
		EnvironmentDigest:              ReferenceEnvironmentDigest,
		APIAuth:                        "VLLM_API_KEY",
		APIKeyRequired:                 true,
		Argv:                           append([]string(nil), referenceArgv...),
		RequiresPrivateIngress:         true,
		RequiresNoHostPublish:          true,
		RequiresDefaultDenyEgress:      true,
	}
}

// ReferenceTemplate returns a detached copy of the exact vLLM template whose
// digest is pinned by ReferenceDescriptor.
func ReferenceTemplate() []byte { return append([]byte(nil), referenceTemplate...) }

// ReferenceSnapshotManifest returns a detached, canonical inventory of every
// file in the pinned Hugging Face snapshot. R-6a must verify the mounted tree
// against this inventory; the inventory itself is not runtime attestation.
func ReferenceSnapshotManifest() []byte {
	return append([]byte(nil), referenceSnapshotManifest...)
}

// ReferenceImageBaselineEnvironment returns a detached copy of the exact Env
// array from the pinned linux/amd64 image config. It contains no supervisor or
// operator-provided entries.
func ReferenceImageBaselineEnvironment() []string {
	return append([]string(nil), referenceImageBaselineEnvironment...)
}

// RequireCertifiedDeployment is deliberately fail-closed in R-3. R-6a and
// R-6 must jointly add an authenticated, manifest-backed registry entry before
// this can succeed.
func RequireCertifiedDeployment(string) error { return ErrProfileUnavailable }

// ValidateReferenceEnvironmentNames is the first phase for a future runtime
// supervisor. It can validate spec names before that supervisor reads any
// corresponding value from an environment or secret store.
func ValidateReferenceEnvironmentNames(names []string) error {
	if len(names) != 3 {
		return ErrAdmissionRejected
	}
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if _, duplicate := seen[name]; duplicate {
			return ErrAdmissionRejected
		}
		switch name {
		case "HF_HUB_OFFLINE", "VLLM_NO_USAGE_STATS", "VLLM_API_KEY":
		default:
			return ErrAdmissionRejected
		}
		seen[name] = struct{}{}
	}
	return nil
}

// BuildReferenceEnvironment constructs only the three-entry supervisor
// overlay. Callers must pass its first result to MergeReferenceEnvironment
// together with an authenticated image baseline before creating a child.
func BuildReferenceEnvironment(values []EnvironmentValue, binding SecretBinding) ([]string, EnvironmentOverlayAttestation, error) {
	names := make([]string, len(values))
	for index, value := range values {
		names[index] = value.Name
	}
	if err := ValidateReferenceEnvironmentNames(names); err != nil {
		return nil, EnvironmentOverlayAttestation{}, err
	}
	seen := make(map[string]EnvironmentValue, len(values))
	for _, value := range values {
		seen[value.Name] = value
	}
	if seen["HF_HUB_OFFLINE"].Value != "1" || seen["HF_HUB_OFFLINE"].Secret ||
		seen["VLLM_NO_USAGE_STATS"].Value != "1" || seen["VLLM_NO_USAGE_STATS"].Secret ||
		!seen["VLLM_API_KEY"].Secret || !validSecret(seen["VLLM_API_KEY"].Value) {
		return nil, EnvironmentOverlayAttestation{}, ErrAdmissionRejected
	}
	if binding.PurifyReference != "" || binding.SidecarReference != "" {
		if binding.PurifyReference == "" || binding.SidecarReference == "" || binding.PurifyReference != binding.SidecarReference {
			return nil, EnvironmentOverlayAttestation{}, ErrAdmissionRejected
		}
	}
	if !validSecret(binding.PurifyValue) || !validSecret(binding.SidecarValue) ||
		len(binding.PurifyValue) != len(binding.SidecarValue) || subtle.ConstantTimeCompare([]byte(binding.PurifyValue), []byte(binding.SidecarValue)) != 1 ||
		len(binding.SidecarValue) != len(seen["VLLM_API_KEY"].Value) || subtle.ConstantTimeCompare([]byte(binding.SidecarValue), []byte(seen["VLLM_API_KEY"].Value)) != 1 {
		return nil, EnvironmentOverlayAttestation{}, ErrAdmissionRejected
	}

	orderedNames := []string{"HF_HUB_OFFLINE", "VLLM_API_KEY", "VLLM_NO_USAGE_STATS"}
	sort.Strings(orderedNames)
	environment := make([]string, len(orderedNames))
	redacted := make([]string, len(orderedNames))
	for index, name := range orderedNames {
		value := seen[name].Value
		environment[index] = name + "=" + value
		if name == "VLLM_API_KEY" {
			value = "<present>"
		}
		redacted[index] = name + "=" + value
	}
	digest := environmentDigest(referenceOverlayEnvironmentDigestDomain, redacted)
	if digest != ReferenceOverlayEnvironmentDigest {
		return nil, EnvironmentOverlayAttestation{}, ErrAdmissionRejected
	}
	return environment, EnvironmentOverlayAttestation{
		RedactedOverlay: append([]string(nil), redacted...),
		OverlayDigest:   digest,
		APIAuth:         "VLLM_API_KEY",
		APIKeyPresent:   true,
	}, nil
}

// MergeReferenceEnvironment authenticates the exact pinned image baseline,
// rejects every overlay collision or extra entry, and returns the complete
// child environment in bytewise name order. Its attestation redacts only the
// generated API key and therefore has a secret-independent locked digest.
func MergeReferenceEnvironment(imageBaseline, overlay []string) ([]string, EnvironmentAttestation, error) {
	baselineByName, ok := uniqueEnvironmentByName(imageBaseline)
	if !ok || len(baselineByName) != len(referenceImageBaselineEnvironment) {
		return nil, EnvironmentAttestation{}, ErrAdmissionRejected
	}
	referenceByName, ok := uniqueEnvironmentByName(referenceImageBaselineEnvironment)
	if !ok || !reflectEnvironmentMapsEqual(baselineByName, referenceByName) ||
		environmentDigest(referenceImageEnvironmentDigestDomain, imageBaseline) != ReferenceImageBaselineEnvironmentDigest {
		return nil, EnvironmentAttestation{}, ErrAdmissionRejected
	}

	overlayByName, ok := uniqueEnvironmentByName(overlay)
	if !ok || len(overlayByName) != 3 || overlayByName["HF_HUB_OFFLINE"] != "HF_HUB_OFFLINE=1" ||
		overlayByName["VLLM_NO_USAGE_STATS"] != "VLLM_NO_USAGE_STATS=1" {
		return nil, EnvironmentAttestation{}, ErrAdmissionRejected
	}
	apiKeyEntry, present := overlayByName["VLLM_API_KEY"]
	if !present {
		return nil, EnvironmentAttestation{}, ErrAdmissionRejected
	}
	_, apiKey, ok := splitEnvironmentEntry(apiKeyEntry)
	if !ok || !validSecret(apiKey) {
		return nil, EnvironmentAttestation{}, ErrAdmissionRejected
	}
	for name := range overlayByName {
		switch name {
		case "HF_HUB_OFFLINE", "VLLM_NO_USAGE_STATS", "VLLM_API_KEY":
		default:
			return nil, EnvironmentAttestation{}, ErrAdmissionRejected
		}
		if _, collision := baselineByName[name]; collision {
			return nil, EnvironmentAttestation{}, ErrAdmissionRejected
		}
	}

	effective := make([]string, 0, len(imageBaseline)+len(overlay))
	effective = append(effective, imageBaseline...)
	effective = append(effective, overlay...)
	sortEnvironmentByName(effective)
	redacted := append([]string(nil), effective...)
	for index, entry := range redacted {
		name, _, _ := splitEnvironmentEntry(entry)
		if name == "VLLM_API_KEY" {
			redacted[index] = "VLLM_API_KEY=<present>"
		}
	}
	digest := environmentDigest(referenceEffectiveEnvironmentDigestDomain, redacted)
	if digest != ReferenceEnvironmentDigest {
		return nil, EnvironmentAttestation{}, ErrAdmissionRejected
	}
	return effective, EnvironmentAttestation{
		RedactedEnvironment: redacted,
		EnvironmentDigest:   digest,
		APIAuth:             "VLLM_API_KEY",
		APIKeyPresent:       true,
	}, nil
}

func uniqueEnvironmentByName(environment []string) (map[string]string, bool) {
	byName := make(map[string]string, len(environment))
	for _, entry := range environment {
		name, _, ok := splitEnvironmentEntry(entry)
		if !ok {
			return nil, false
		}
		if _, duplicate := byName[name]; duplicate {
			return nil, false
		}
		byName[name] = entry
	}
	return byName, true
}

func splitEnvironmentEntry(entry string) (string, string, bool) {
	separator := strings.IndexByte(entry, '=')
	if separator <= 0 {
		return "", "", false
	}
	return entry[:separator], entry[separator+1:], true
}

func reflectEnvironmentMapsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for name, entry := range left {
		if right[name] != entry {
			return false
		}
	}
	return true
}

func sortEnvironmentByName(environment []string) {
	sort.Slice(environment, func(left, right int) bool {
		leftName, _, _ := splitEnvironmentEntry(environment[left])
		rightName, _, _ := splitEnvironmentEntry(environment[right])
		return leftName < rightName
	})
}

func environmentDigest(domain string, environment []string) string {
	canonical := append([]string(nil), environment...)
	sortEnvironmentByName(canonical)
	digest := sha256.New()
	_, _ = digest.Write([]byte(domain))
	var length [binary.MaxVarintLen64]byte
	for _, entry := range canonical {
		encoded := []byte(entry)
		count := binary.PutUvarint(length[:], uint64(len(encoded)))
		_, _ = digest.Write(length[:count])
		_, _ = digest.Write(encoded)
	}
	return hex.EncodeToString(digest.Sum(nil))
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
