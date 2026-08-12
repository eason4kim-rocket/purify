package deploy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	parent "github.com/use-agent/purify/search/rerank"
)

func TestReferenceEnvironmentPolicyBuildsRedactedCanonicalOverlay(t *testing.T) {
	const secret = "sidecar-process-secret"
	overlay, attestation, err := BuildReferenceEnvironment([]EnvironmentValue{
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
	if strings.Join(overlay, "\n") != strings.Join(wantEnvironment, "\n") {
		t.Fatalf("overlay = %#v, want %#v", overlay, wantEnvironment)
	}
	if !attestation.APIKeyPresent || attestation.APIAuth != "VLLM_API_KEY" ||
		strings.Join(attestation.RedactedOverlay, "\n") != "HF_HUB_OFFLINE=1\nVLLM_API_KEY=<present>\nVLLM_NO_USAGE_STATS=1" {
		t.Fatalf("attestation = %#v", attestation)
	}
	if len(attestation.OverlayDigest) != 64 {
		t.Fatalf("overlay digest = %q", attestation.OverlayDigest)
	}
	if attestation.OverlayDigest != "21b4e2eaf503da54e9db0ef6f99ebf71d12471d37cc994c59f819b42dd2ab5bd" {
		t.Fatalf("overlay digest = %q, want locked vector", attestation.OverlayDigest)
	}
	encoded := strings.Join(attestation.RedactedOverlay, "\n") + attestation.OverlayDigest
	if strings.Contains(encoded, secret) {
		t.Fatal("attestation leaked secret")
	}

	reversed, reversedAttestation, err := BuildReferenceEnvironment([]EnvironmentValue{
		{Name: "HF_HUB_OFFLINE", Value: "1"},
		{Name: "VLLM_NO_USAGE_STATS", Value: "1"},
		{Name: "VLLM_API_KEY", Value: secret, Secret: true},
	}, SecretBinding{PurifyValue: secret, SidecarValue: secret})
	if err != nil || strings.Join(reversed, "\n") != strings.Join(overlay, "\n") || !reflect.DeepEqual(reversedAttestation, attestation) {
		t.Fatalf("permuted environment = %#v/%#v, %v", reversed, reversedAttestation, err)
	}
}

func TestReferenceImageBaselineEnvironmentIsExactAndDetached(t *testing.T) {
	want := []string{
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
	got := ReferenceImageBaselineEnvironment()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("image baseline = %#v, want %#v", got, want)
	}
	got[0] = "PATH=mutated"
	if reflect.DeepEqual(got, ReferenceImageBaselineEnvironment()) {
		t.Fatal("ReferenceImageBaselineEnvironment shares mutable backing storage")
	}
	if ReferenceImageBaselineEnvironmentDigest != "1bdf7468993a8748565059dc00ea30b28a1cf67b01327be3675aee4d77c1abf1" {
		t.Fatalf("image baseline digest = %q", ReferenceImageBaselineEnvironmentDigest)
	}
}

func TestMergeReferenceEnvironmentAuthenticatesBaselineAndRedactsOnlyAPIKey(t *testing.T) {
	const secret = "sidecar-process-secret"
	overlay, _, err := BuildReferenceEnvironment([]EnvironmentValue{
		{Name: "VLLM_NO_USAGE_STATS", Value: "1"},
		{Name: "VLLM_API_KEY", Value: secret, Secret: true},
		{Name: "HF_HUB_OFFLINE", Value: "1"},
	}, SecretBinding{PurifyValue: secret, SidecarValue: secret})
	if err != nil {
		t.Fatal(err)
	}
	baseline := ReferenceImageBaselineEnvironment()
	for left, right := 0, len(baseline)-1; left < right; left, right = left+1, right-1 {
		baseline[left], baseline[right] = baseline[right], baseline[left]
	}
	effective, attestation, err := MergeReferenceEnvironment(baseline, overlay)
	if err != nil {
		t.Fatal(err)
	}
	if len(effective) != 24 || !sort.StringsAreSorted(effective) {
		t.Fatalf("effective environment = %#v", effective)
	}
	if len(attestation.RedactedEnvironment) != 24 || !sort.StringsAreSorted(attestation.RedactedEnvironment) ||
		attestation.EnvironmentDigest != "90d6a698c89f10d716c874648f7b316b7899ce6089a4e39936fd290cd9de4617" ||
		!attestation.APIKeyPresent || attestation.APIAuth != "VLLM_API_KEY" {
		t.Fatalf("effective attestation = %#v", attestation)
	}
	if attestation.EnvironmentDigest != ReferenceEnvironmentDigest {
		t.Fatalf("effective digest = %q, descriptor pin = %q", attestation.EnvironmentDigest, ReferenceEnvironmentDigest)
	}
	if count := countExact(attestation.RedactedEnvironment, "VLLM_API_KEY=<present>"); count != 1 {
		t.Fatalf("redacted API key count = %d: %#v", count, attestation.RedactedEnvironment)
	}
	if strings.Contains(strings.Join(attestation.RedactedEnvironment, "\n")+attestation.EnvironmentDigest, secret) {
		t.Fatal("effective attestation leaked secret")
	}
	if count := countExact(effective, "VLLM_API_KEY="+secret); count != 1 {
		t.Fatalf("effective API key count = %d: %#v", count, effective)
	}

	secondOverlay, _, err := BuildReferenceEnvironment([]EnvironmentValue{
		{Name: "VLLM_NO_USAGE_STATS", Value: "1"},
		{Name: "VLLM_API_KEY", Value: "another-secret", Secret: true},
		{Name: "HF_HUB_OFFLINE", Value: "1"},
	}, SecretBinding{PurifyValue: "another-secret", SidecarValue: "another-secret"})
	if err != nil {
		t.Fatal(err)
	}
	secondEffective, secondAttestation, err := MergeReferenceEnvironment(ReferenceImageBaselineEnvironment(), secondOverlay)
	if err != nil || reflect.DeepEqual(secondEffective, effective) || !reflect.DeepEqual(secondAttestation, attestation) {
		t.Fatalf("secret-independent attestation = %#v/%#v, %v", secondEffective, secondAttestation, err)
	}
}

func TestMergeReferenceEnvironmentRejectsBaselineOrOverlayDrift(t *testing.T) {
	validBaseline := ReferenceImageBaselineEnvironment()
	validOverlay := []string{"HF_HUB_OFFLINE=1", "VLLM_API_KEY=secret", "VLLM_NO_USAGE_STATS=1"}
	tests := []struct {
		name     string
		baseline []string
		overlay  []string
	}{
		{name: "missing baseline", baseline: validBaseline[:len(validBaseline)-1], overlay: validOverlay},
		{name: "extra baseline", baseline: append(append([]string(nil), validBaseline...), "EXTRA=value"), overlay: validOverlay},
		{name: "duplicate baseline", baseline: append(append([]string(nil), validBaseline...), validBaseline[0]), overlay: validOverlay},
		{name: "baseline value drift", baseline: append([]string{"PATH=/other"}, validBaseline[1:]...), overlay: validOverlay},
		{name: "malformed baseline", baseline: append([]string{"PATH"}, validBaseline[1:]...), overlay: validOverlay},
		{name: "missing overlay", baseline: validBaseline, overlay: validOverlay[:2]},
		{name: "unknown overlay", baseline: validBaseline, overlay: []string{"HF_HUB_OFFLINE=1", "VLLM_API_KEY=secret", "EXTRA=value"}},
		{name: "duplicate overlay", baseline: validBaseline, overlay: []string{"HF_HUB_OFFLINE=1", "VLLM_API_KEY=secret", "VLLM_API_KEY=other"}},
		{name: "baseline override", baseline: validBaseline, overlay: []string{"HF_HUB_OFFLINE=1", "VLLM_API_KEY=secret", "VLLM_USAGE_SOURCE=other"}},
		{name: "wrong offline", baseline: validBaseline, overlay: []string{"HF_HUB_OFFLINE=0", "VLLM_API_KEY=secret", "VLLM_NO_USAGE_STATS=1"}},
		{name: "empty key", baseline: validBaseline, overlay: []string{"HF_HUB_OFFLINE=1", "VLLM_API_KEY=", "VLLM_NO_USAGE_STATS=1"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			effective, attestation, err := MergeReferenceEnvironment(test.baseline, test.overlay)
			if !errors.Is(err, ErrAdmissionRejected) || effective != nil || !reflect.DeepEqual(attestation, EnvironmentAttestation{}) {
				t.Fatalf("MergeReferenceEnvironment() = %#v, %#v, %v", effective, attestation, err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("error leaked secret: %v", err)
			}
		})
	}
}

func countExact(values []string, want string) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
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
			if !errors.Is(err, ErrAdmissionRejected) || environment != nil || !reflect.DeepEqual(attestation, EnvironmentOverlayAttestation{}) {
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
		"--uds", "/run/purify/vllm.sock",
	}
	if descriptor.ProfileID != parent.ReferenceProfileID || descriptor.ServedModel != parent.ReferenceServedModel ||
		descriptor.Instruction != parent.ReferenceInstruction || descriptor.InstructionVersion != parent.ReferenceInstructionVersion ||
		descriptor.Platform != "linux/amd64" ||
		descriptor.VLLMVersion != "v0.23.0" ||
		descriptor.ImageDigest != "sha256:3a1e7f5904e1a1192a02aa0086ceaffc33985d7044c7bb25b3a43d61bdbe3ac0" ||
		descriptor.ImageIndexDigest != "sha256:6d8429e38e3747723ca07ee1b17972e09bb9c51c4032b266f24fb1cc3b22ed8f" ||
		descriptor.ImageConfigID != "sha256:f37691f675bb82f734f606de8af90e777d3f80a20b120e699fd43fd10e60b8d7" ||
		!reflect.DeepEqual(descriptor.ImageEntrypoint, []string{"vllm", "serve"}) ||
		descriptor.ModelRevision != "e61197ed45024b0ed8a2d74b80b4d909f1255473" ||
		descriptor.TokenizerRevision != "e61197ed45024b0ed8a2d74b80b4d909f1255473" ||
		descriptor.TemplateSHA256 != "e1ee98e69aab7b2da366edf1c50efcef37e34b4a0c50fb816336213e68d9047a" ||
		descriptor.TemplatePath != "/run/purify/qwen3_reranker.jinja" ||
		descriptor.SnapshotPath != "/root/.cache/huggingface/hub/models--Qwen--Qwen3-Reranker-0.6B/snapshots/e61197ed45024b0ed8a2d74b80b4d909f1255473" ||
		descriptor.UDSPath != "/run/purify/vllm.sock" ||
		descriptor.SnapshotManifestSHA256 != ReferenceSnapshotManifestSHA256 || descriptor.Runner != "pooling" ||
		descriptor.MaxModelLen != 8192 ||
		descriptor.HFOverrides != `{"architectures":["Qwen3ForSequenceClassification"],"classifier_from_token":["no","yes"],"is_original_qwen3_reranker":true}` ||
		descriptor.ScoreMinimum != 0 || descriptor.ScoreMaximum != 1 || descriptor.PrefixCaching ||
		descriptor.Route != "/v1/rerank" || descriptor.EnvironmentPolicyVersion != ReferenceEnvironmentPolicyVersion ||
		descriptor.ImageBaselineEnvironmentDigest != ReferenceImageBaselineEnvironmentDigest ||
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
	descriptor.ImageEntrypoint[0] = "mutated"
	if ReferenceDescriptor().ImageEntrypoint[0] != "vllm" {
		t.Fatal("reference image entrypoint shares mutable backing storage")
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
