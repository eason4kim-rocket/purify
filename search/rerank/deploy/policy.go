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
	ReferenceEnvironmentPolicyVersion = "rerank-env-v1"
	ReferenceEnvironmentDigest        = "7929a1e8af70048f2386bbd4de49690cda2cdfa0a398c242808a1a761fa7f931"
	ReferenceSnapshotManifestSHA256   = "f4769df1fce7a8bff3d1e5f1f913e8e5e1ddedf9906db423b27be7be6a906c65"
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

type Descriptor struct {
	ProfileID                 string
	Platform                  string
	VLLMVersion               string
	ImageDigest               string
	ImageIndexDigest          string
	ModelRevision             string
	TokenizerRevision         string
	ServedModel               string
	Instruction               string
	InstructionVersion        string
	TemplateSHA256            string
	TemplatePath              string
	SnapshotManifestSHA256    string
	Runner                    string
	MaxModelLen               int
	HFOverrides               string
	ScoreMinimum              float64
	ScoreMaximum              float64
	PrefixCaching             bool
	Route                     string
	EnvironmentPolicyVersion  string
	EnvironmentDigest         string
	APIAuth                   string
	APIKeyRequired            bool
	Argv                      []string
	RequiresPrivateIngress    bool
	RequiresNoHostPublish     bool
	RequiresDefaultDenyEgress bool
	Certified                 bool
	ManifestID                string
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
}

func ReferenceDescriptor() Descriptor {
	return Descriptor{
		ProfileID:                 "qwen3-reranker-0.6b-v1",
		Platform:                  "linux/amd64",
		VLLMVersion:               "v0.23.0",
		ImageDigest:               "sha256:3a1e7f5904e1a1192a02aa0086ceaffc33985d7044c7bb25b3a43d61bdbe3ac0",
		ImageIndexDigest:          "sha256:6d8429e38e3747723ca07ee1b17972e09bb9c51c4032b266f24fb1cc3b22ed8f",
		ModelRevision:             "e61197ed45024b0ed8a2d74b80b4d909f1255473",
		TokenizerRevision:         "e61197ed45024b0ed8a2d74b80b4d909f1255473",
		ServedModel:               "Qwen/Qwen3-Reranker-0.6B",
		Instruction:               "Given a web search query, retrieve relevant passages that answer the query",
		InstructionVersion:        "qwen3-reranker-instruction-v1",
		TemplateSHA256:            "e1ee98e69aab7b2da366edf1c50efcef37e34b4a0c50fb816336213e68d9047a",
		TemplatePath:              "/run/purify/qwen3_reranker.jinja",
		SnapshotManifestSHA256:    ReferenceSnapshotManifestSHA256,
		Runner:                    "pooling",
		MaxModelLen:               8192,
		HFOverrides:               `{"architectures":["Qwen3ForSequenceClassification"],"classifier_from_token":["no","yes"],"is_original_qwen3_reranker":true}`,
		ScoreMinimum:              0,
		ScoreMaximum:              1,
		PrefixCaching:             false,
		Route:                     "/v1/rerank",
		EnvironmentPolicyVersion:  ReferenceEnvironmentPolicyVersion,
		EnvironmentDigest:         ReferenceEnvironmentDigest,
		APIAuth:                   "VLLM_API_KEY",
		APIKeyRequired:            true,
		Argv:                      append([]string(nil), referenceArgv...),
		RequiresPrivateIngress:    true,
		RequiresNoHostPublish:     true,
		RequiresDefaultDenyEgress: true,
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

func BuildReferenceEnvironment(values []EnvironmentValue, binding SecretBinding) ([]string, EnvironmentAttestation, error) {
	names := make([]string, len(values))
	for index, value := range values {
		names[index] = value.Name
	}
	if err := ValidateReferenceEnvironmentNames(names); err != nil {
		return nil, EnvironmentAttestation{}, err
	}
	seen := make(map[string]EnvironmentValue, len(values))
	for _, value := range values {
		seen[value.Name] = value
	}
	if seen["HF_HUB_OFFLINE"].Value != "1" || seen["HF_HUB_OFFLINE"].Secret ||
		seen["VLLM_NO_USAGE_STATS"].Value != "1" || seen["VLLM_NO_USAGE_STATS"].Secret ||
		!seen["VLLM_API_KEY"].Secret || !validSecret(seen["VLLM_API_KEY"].Value) {
		return nil, EnvironmentAttestation{}, ErrAdmissionRejected
	}
	if binding.PurifyReference != "" || binding.SidecarReference != "" {
		if binding.PurifyReference == "" || binding.SidecarReference == "" || binding.PurifyReference != binding.SidecarReference {
			return nil, EnvironmentAttestation{}, ErrAdmissionRejected
		}
	}
	if !validSecret(binding.PurifyValue) || !validSecret(binding.SidecarValue) ||
		len(binding.PurifyValue) != len(binding.SidecarValue) || subtle.ConstantTimeCompare([]byte(binding.PurifyValue), []byte(binding.SidecarValue)) != 1 ||
		len(binding.SidecarValue) != len(seen["VLLM_API_KEY"].Value) || subtle.ConstantTimeCompare([]byte(binding.SidecarValue), []byte(seen["VLLM_API_KEY"].Value)) != 1 {
		return nil, EnvironmentAttestation{}, ErrAdmissionRejected
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
	digest := sha256.New()
	_, _ = digest.Write([]byte(ReferenceEnvironmentPolicyVersion + "\x00"))
	var length [binary.MaxVarintLen64]byte
	for _, entry := range redacted {
		encoded := []byte(entry)
		count := binary.PutUvarint(length[:], uint64(len(encoded)))
		_, _ = digest.Write(length[:count])
		_, _ = digest.Write(encoded)
	}
	return environment, EnvironmentAttestation{
		RedactedEnvironment: append([]string(nil), redacted...),
		EnvironmentDigest:   hex.EncodeToString(digest.Sum(nil)),
		APIAuth:             "VLLM_API_KEY",
		APIKeyPresent:       true,
	}, nil
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
