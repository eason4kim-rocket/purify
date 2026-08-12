package deploy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"

	parent "github.com/use-agent/purify/search/rerank"
)

func TestReferenceEnvironmentPolicyBuildsRedactedCanonicalEnvironment(t *testing.T) {
	const secret = "sidecar-process-secret"
	environment, attestation, err := BuildReferenceEnvironment([]EnvironmentValue{
		{Name: "VLLM_NO_USAGE_STATS", Value: "1"},
		{Name: "VLLM_API_KEY", Value: secret, Secret: true},
		{Name: "HF_HUB_OFFLINE", Value: "1"},
	}, SecretBinding{PurifyValue: secret, SidecarValue: secret})
	if err != nil {
		t.Fatal(err)
	}
	wantEnvironment := []string{
		"HF_HUB_OFFLINE=1",
		"VLLM_API_KEY=" + secret,
		"VLLM_NO_USAGE_STATS=1",
	}
	if strings.Join(environment, "\n") != strings.Join(wantEnvironment, "\n") {
		t.Fatalf("environment = %#v, want %#v", environment, wantEnvironment)
	}
	if !attestation.APIKeyPresent || attestation.APIAuth != "VLLM_API_KEY" ||
		strings.Join(attestation.RedactedEnvironment, "\n") != "HF_HUB_OFFLINE=1\nVLLM_API_KEY=<present>\nVLLM_NO_USAGE_STATS=1" {
		t.Fatalf("attestation = %#v", attestation)
	}
	if len(attestation.EnvironmentDigest) != 64 {
		t.Fatalf("environment digest = %q", attestation.EnvironmentDigest)
	}
	if attestation.EnvironmentDigest != "7929a1e8af70048f2386bbd4de49690cda2cdfa0a398c242808a1a761fa7f931" {
		t.Fatalf("environment digest = %q, want locked vector", attestation.EnvironmentDigest)
	}
	encoded := strings.Join(attestation.RedactedEnvironment, "\n") + attestation.EnvironmentDigest
	if strings.Contains(encoded, secret) {
		t.Fatal("attestation leaked secret")
	}

	reversed, reversedAttestation, err := BuildReferenceEnvironment([]EnvironmentValue{
		{Name: "HF_HUB_OFFLINE", Value: "1"},
		{Name: "VLLM_NO_USAGE_STATS", Value: "1"},
		{Name: "VLLM_API_KEY", Value: secret, Secret: true},
	}, SecretBinding{PurifyValue: secret, SidecarValue: secret})
	if err != nil || strings.Join(reversed, "\n") != strings.Join(environment, "\n") || !reflect.DeepEqual(reversedAttestation, attestation) {
		t.Fatalf("permuted environment = %#v/%#v, %v", reversed, reversedAttestation, err)
	}
}

func TestReferenceEnvironmentPolicyRejectsBeforeSerializingUnknownOrSecrets(t *testing.T) {
	const secret = "must-not-leak"
	valid := []EnvironmentValue{
		{Name: "HF_HUB_OFFLINE", Value: "1"},
		{Name: "VLLM_NO_USAGE_STATS", Value: "1"},
		{Name: "VLLM_API_KEY", Value: secret, Secret: true},
	}
	tests := []struct {
		name    string
		values  []EnvironmentValue
		binding SecretBinding
	}{
		{name: "unknown AWS secret", values: append(append([]EnvironmentValue(nil), valid...), EnvironmentValue{Name: "AWS_SECRET_ACCESS_KEY", Value: secret, Secret: true}), binding: SecretBinding{PurifyValue: secret, SidecarValue: secret}},
		{name: "unknown VLLM override", values: append(append([]EnvironmentValue(nil), valid...), EnvironmentValue{Name: "VLLM_USE_FASTOKENS", Value: "1"}), binding: SecretBinding{PurifyValue: secret, SidecarValue: secret}},
		{name: "duplicate", values: append(append([]EnvironmentValue(nil), valid...), EnvironmentValue{Name: "HF_HUB_OFFLINE", Value: "1"}), binding: SecretBinding{PurifyValue: secret, SidecarValue: secret}},
		{name: "missing telemetry optout", values: valid[1:], binding: SecretBinding{PurifyValue: secret, SidecarValue: secret}},
		{name: "wrong telemetry optout", values: []EnvironmentValue{{Name: "HF_HUB_OFFLINE", Value: "1"}, {Name: "VLLM_NO_USAGE_STATS", Value: "0"}, {Name: "VLLM_API_KEY", Value: secret, Secret: true}}, binding: SecretBinding{PurifyValue: secret, SidecarValue: secret}},
		{name: "empty key", values: []EnvironmentValue{{Name: "HF_HUB_OFFLINE", Value: "1"}, {Name: "VLLM_NO_USAGE_STATS", Value: "1"}, {Name: "VLLM_API_KEY", Value: "", Secret: true}}, binding: SecretBinding{}},
		{name: "key not marked secret", values: []EnvironmentValue{{Name: "HF_HUB_OFFLINE", Value: "1"}, {Name: "VLLM_NO_USAGE_STATS", Value: "1"}, {Name: "VLLM_API_KEY", Value: secret}}, binding: SecretBinding{PurifyValue: secret, SidecarValue: secret}},
		{name: "secret mismatch", values: valid, binding: SecretBinding{PurifyValue: secret, SidecarValue: "different"}},
		{name: "reference mismatch", values: valid, binding: SecretBinding{PurifyReference: "secret://purify", SidecarReference: "secret://sidecar", PurifyValue: secret, SidecarValue: secret}},
		{name: "secret leading whitespace", values: []EnvironmentValue{{Name: "HF_HUB_OFFLINE", Value: "1"}, {Name: "VLLM_NO_USAGE_STATS", Value: "1"}, {Name: "VLLM_API_KEY", Value: " " + secret, Secret: true}}, binding: SecretBinding{PurifyValue: " " + secret, SidecarValue: " " + secret}},
		{name: "secret control", values: []EnvironmentValue{{Name: "HF_HUB_OFFLINE", Value: "1"}, {Name: "VLLM_NO_USAGE_STATS", Value: "1"}, {Name: "VLLM_API_KEY", Value: secret + "\n", Secret: true}}, binding: SecretBinding{PurifyValue: secret + "\n", SidecarValue: secret + "\n"}},
		{name: "secret N plus 1", values: []EnvironmentValue{{Name: "HF_HUB_OFFLINE", Value: "1"}, {Name: "VLLM_NO_USAGE_STATS", Value: "1"}, {Name: "VLLM_API_KEY", Value: strings.Repeat("k", maximumSecretBytes+1), Secret: true}}, binding: SecretBinding{PurifyValue: strings.Repeat("k", maximumSecretBytes+1), SidecarValue: strings.Repeat("k", maximumSecretBytes+1)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			environment, attestation, err := BuildReferenceEnvironment(test.values, test.binding)
			if !errors.Is(err, ErrAdmissionRejected) || environment != nil || !reflect.DeepEqual(attestation, EnvironmentAttestation{}) {
				t.Fatalf("BuildReferenceEnvironment() = %#v, %#v, %v", environment, attestation, err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error leaked secret: %v", err)
			}
		})
	}
}

func TestReferenceEnvironmentNameAdmissionRunsWithoutValues(t *testing.T) {
	if err := ValidateReferenceEnvironmentNames([]string{"VLLM_API_KEY", "HF_HUB_OFFLINE", "VLLM_NO_USAGE_STATS"}); err != nil {
		t.Fatalf("ValidateReferenceEnvironmentNames(valid) = %v", err)
	}
	for _, names := range [][]string{
		{"HF_HUB_OFFLINE", "VLLM_NO_USAGE_STATS"},
		{"HF_HUB_OFFLINE", "VLLM_NO_USAGE_STATS", "VLLM_USE_FASTOKENS"},
		{"HF_HUB_OFFLINE", "VLLM_NO_USAGE_STATS", "HF_HUB_OFFLINE"},
	} {
		if err := ValidateReferenceEnvironmentNames(names); !errors.Is(err, ErrAdmissionRejected) {
			t.Fatalf("ValidateReferenceEnvironmentNames(%q) = %v", names, err)
		}
	}
}

func TestReferenceDescriptorAndRegistryRemainUncertified(t *testing.T) {
	descriptor := ReferenceDescriptor()
	wantArgv := []string{
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
	if descriptor.ProfileID != parent.ReferenceProfileID || descriptor.ServedModel != parent.ReferenceServedModel ||
		descriptor.Instruction != parent.ReferenceInstruction || descriptor.InstructionVersion != parent.ReferenceInstructionVersion ||
		descriptor.Platform != "linux/amd64" ||
		descriptor.VLLMVersion != "v0.23.0" ||
		descriptor.ImageDigest != "sha256:3a1e7f5904e1a1192a02aa0086ceaffc33985d7044c7bb25b3a43d61bdbe3ac0" ||
		descriptor.ImageIndexDigest != "sha256:6d8429e38e3747723ca07ee1b17972e09bb9c51c4032b266f24fb1cc3b22ed8f" ||
		descriptor.ModelRevision != "e61197ed45024b0ed8a2d74b80b4d909f1255473" ||
		descriptor.TokenizerRevision != "e61197ed45024b0ed8a2d74b80b4d909f1255473" ||
		descriptor.TemplateSHA256 != "e1ee98e69aab7b2da366edf1c50efcef37e34b4a0c50fb816336213e68d9047a" ||
		descriptor.TemplatePath != "/run/purify/qwen3_reranker.jinja" ||
		descriptor.SnapshotManifestSHA256 != ReferenceSnapshotManifestSHA256 || descriptor.Runner != "pooling" ||
		descriptor.MaxModelLen != 8192 ||
		descriptor.HFOverrides != `{"architectures":["Qwen3ForSequenceClassification"],"classifier_from_token":["no","yes"],"is_original_qwen3_reranker":true}` ||
		descriptor.ScoreMinimum != 0 || descriptor.ScoreMaximum != 1 || descriptor.PrefixCaching ||
		descriptor.Route != "/v1/rerank" || descriptor.EnvironmentPolicyVersion != ReferenceEnvironmentPolicyVersion ||
		descriptor.EnvironmentDigest != ReferenceEnvironmentDigest || descriptor.APIAuth != "VLLM_API_KEY" || !descriptor.APIKeyRequired ||
		!reflect.DeepEqual(descriptor.Argv, wantArgv) ||
		!descriptor.RequiresPrivateIngress || !descriptor.RequiresNoHostPublish || !descriptor.RequiresDefaultDenyEgress {
		t.Fatalf("reference descriptor = %#v", descriptor)
	}
	if bytes.Contains([]byte(strings.Join(descriptor.Argv, "\x00")), []byte("api-key")) {
		t.Fatalf("reference argv = %#v", descriptor.Argv)
	}
	if descriptor.Certified || descriptor.ManifestID != "" {
		t.Fatalf("R-3 descriptor unexpectedly certified = %#v", descriptor)
	}
	if err := RequireCertifiedDeployment(descriptor.ProfileID); !errors.Is(err, ErrProfileUnavailable) {
		t.Fatalf("RequireCertifiedDeployment() = %v, want ErrProfileUnavailable", err)
	}
}

func TestReferenceAssetsMatchDescriptorPins(t *testing.T) {
	descriptor := ReferenceDescriptor()
	for _, test := range []struct {
		name string
		raw  []byte
		want string
	}{
		{name: "template", raw: ReferenceTemplate(), want: descriptor.TemplateSHA256},
		{name: "snapshot manifest", raw: ReferenceSnapshotManifest(), want: descriptor.SnapshotManifestSHA256},
	} {
		t.Run(test.name, func(t *testing.T) {
			digest := sha256.Sum256(test.raw)
			if got := hex.EncodeToString(digest[:]); got != test.want {
				t.Fatalf("asset digest = %s, want %s", got, test.want)
			}
			if len(test.raw) == 0 {
				t.Fatal("reference asset is empty")
			}
		})
	}
	template := ReferenceTemplate()
	template[0] ^= 0xff
	if bytes.Equal(template, ReferenceTemplate()) {
		t.Fatal("ReferenceTemplate shares mutable backing storage")
	}
	snapshot := ReferenceSnapshotManifest()
	snapshot[0] ^= 0xff
	if bytes.Equal(snapshot, ReferenceSnapshotManifest()) {
		t.Fatal("ReferenceSnapshotManifest shares mutable backing storage")
	}
}
