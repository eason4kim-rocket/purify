package replay

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/use-agent/purify/search/rerank"
	"github.com/use-agent/purify/search/rerank/deploy"
)

func TestReferenceManifestHasExactCanonicalIdentity(t *testing.T) {
	manifest, err := ReferenceManifest()
	if err != nil {
		t.Fatalf("ReferenceManifest() error = %v", err)
	}
	if manifest.SchemaVersion != "rerank-recording-manifest-v2" ||
		manifest.ProfileID != "qwen3-reranker-0.6b-v1" ||
		manifest.InstructionVersion != "qwen3-reranker-instruction-v1" ||
		manifest.Instruction != "Given a web search query, retrieve relevant passages that answer the query" ||
		manifest.VLLMVersion != "v0.23.0" ||
		manifest.Platform != "linux/amd64" ||
		manifest.ImageDigest != "sha256:3a1e7f5904e1a1192a02aa0086ceaffc33985d7044c7bb25b3a43d61bdbe3ac0" ||
		manifest.ImageIndexDigest != "sha256:6d8429e38e3747723ca07ee1b17972e09bb9c51c4032b266f24fb1cc3b22ed8f" ||
		manifest.ImageConfigID != "sha256:f37691f675bb82f734f606de8af90e777d3f80a20b120e699fd43fd10e60b8d7" ||
		!reflect.DeepEqual(manifest.ImageEntrypoint, []string{"vllm", "serve"}) ||
		manifest.ServedModelID != "Qwen/Qwen3-Reranker-0.6B" ||
		manifest.ModelRevision != "e61197ed45024b0ed8a2d74b80b4d909f1255473" ||
		manifest.TokenizerRevision != "e61197ed45024b0ed8a2d74b80b4d909f1255473" ||
		manifest.SnapshotManifestSHA256 != "f4769df1fce7a8bff3d1e5f1f913e8e5e1ddedf9906db423b27be7be6a906c65" ||
		manifest.SnapshotPath != "/root/.cache/huggingface/hub/models--Qwen--Qwen3-Reranker-0.6B/snapshots/e61197ed45024b0ed8a2d74b80b4d909f1255473" ||
		manifest.TemplateSHA256 != "e1ee98e69aab7b2da366edf1c50efcef37e34b4a0c50fb816336213e68d9047a" ||
		manifest.TemplatePath != "/run/purify/qwen3_reranker.jinja" || manifest.Runner != "pooling" ||
		manifest.UDSPath != "/run/purify/vllm.sock" ||
		manifest.MaxModelLen != 8192 ||
		manifest.HFOverrides != `{"architectures":["Qwen3ForSequenceClassification"],"classifier_from_token":["no","yes"],"is_original_qwen3_reranker":true}` ||
		manifest.ScoreMinimum != 0 || manifest.ScoreMaximum != 1 || manifest.PrefixCaching ||
		manifest.Route != "/v1/rerank" || manifest.EnvironmentPolicyVersion != deploy.ReferenceEnvironmentPolicyVersion ||
		manifest.ImageBaselineEnvironmentDigest != "1bdf7468993a8748565059dc00ea30b28a1cf67b01327be3675aee4d77c1abf1" ||
		manifest.EnvironmentDigest != "90d6a698c89f10d716c874648f7b316b7899ce6089a4e39936fd290cd9de4617" ||
		manifest.APIAuth != "VLLM_API_KEY" || !manifest.APIKeyRequired ||
		!manifest.RequiresPrivateIngress || !manifest.RequiresNoHostPublish || !manifest.RequiresDefaultDenyEgress {
		t.Fatalf("reference manifest drifted: %#v", manifest)
	}
	if manifest.SchemaVersion != ManifestSchemaVersion {
		t.Fatalf("schema = %q, package pin = %q", manifest.SchemaVersion, ManifestSchemaVersion)
	}
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
	if !reflect.DeepEqual(manifest.Argv, wantArgv) {
		t.Fatalf("reference argv = %#v, want %#v", manifest.Argv, wantArgv)
	}
	if manifest.ManifestID != "2c40e264225c52f43431d0593c772b80f9e97b4e87cf757aaefa941c7e880e35" || manifest.ManifestID != ManifestID(manifest) {
		t.Fatalf("manifest id = %q, recomputed = %q", manifest.ManifestID, ManifestID(manifest))
	}
	if err := ValidateManifest(manifest); err != nil {
		t.Fatalf("ValidateManifest(reference) error = %v", err)
	}
}

func TestValidateManifestRejectsEveryRecordedTupleDrift(t *testing.T) {
	reference, err := ReferenceManifest()
	if err != nil {
		t.Fatalf("ReferenceManifest() error = %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{name: "schema", mutate: func(value *Manifest) { value.SchemaVersion += "-next" }},
		{name: "manifest id", mutate: func(value *Manifest) { value.ManifestID = strings.Repeat("0", 64) }},
		{name: "profile", mutate: func(value *Manifest) { value.ProfileID += "-next" }},
		{name: "instruction version", mutate: func(value *Manifest) { value.InstructionVersion += "-next" }},
		{name: "instruction", mutate: func(value *Manifest) { value.Instruction += "!" }},
		{name: "template", mutate: func(value *Manifest) { value.TemplateSHA256 = strings.Repeat("0", 64) }},
		{name: "vllm", mutate: func(value *Manifest) { value.VLLMVersion = "v0.23.1" }},
		{name: "image", mutate: func(value *Manifest) { value.ImageDigest = "sha256:" + strings.Repeat("0", 64) }},
		{name: "image index", mutate: func(value *Manifest) { value.ImageIndexDigest = "sha256:" + strings.Repeat("0", 64) }},
		{name: "image config", mutate: func(value *Manifest) { value.ImageConfigID = "sha256:" + strings.Repeat("0", 64) }},
		{name: "image entrypoint", mutate: func(value *Manifest) { value.ImageEntrypoint[0] = "other" }},
		{name: "platform", mutate: func(value *Manifest) { value.Platform = "linux/arm64" }},
		{name: "revision", mutate: func(value *Manifest) { value.ModelRevision = strings.Repeat("0", 40) }},
		{name: "tokenizer revision", mutate: func(value *Manifest) { value.TokenizerRevision = strings.Repeat("0", 40) }},
		{name: "snapshot", mutate: func(value *Manifest) { value.SnapshotManifestSHA256 = strings.Repeat("0", 64) }},
		{name: "snapshot path", mutate: func(value *Manifest) { value.SnapshotPath += ".other" }},
		{name: "served model", mutate: func(value *Manifest) { value.ServedModelID += "-alias" }},
		{name: "runner", mutate: func(value *Manifest) { value.Runner = "generate" }},
		{name: "hf overrides", mutate: func(value *Manifest) { value.HFOverrides = `{}` }},
		{name: "model len", mutate: func(value *Manifest) { value.MaxModelLen++ }},
		{name: "score minimum", mutate: func(value *Manifest) { value.ScoreMinimum = -1 }},
		{name: "score maximum", mutate: func(value *Manifest) { value.ScoreMaximum = 2 }},
		{name: "prefix cache", mutate: func(value *Manifest) { value.PrefixCaching = true }},
		{name: "route", mutate: func(value *Manifest) { value.Route = "/rerank" }},
		{name: "uds path", mutate: func(value *Manifest) { value.UDSPath += ".other" }},
		{name: "template path", mutate: func(value *Manifest) { value.TemplatePath += ".other" }},
		{name: "environment policy", mutate: func(value *Manifest) { value.EnvironmentPolicyVersion += "-next" }},
		{name: "image baseline environment", mutate: func(value *Manifest) { value.ImageBaselineEnvironmentDigest = strings.Repeat("0", 64) }},
		{name: "environment", mutate: func(value *Manifest) { value.EnvironmentDigest = strings.Repeat("0", 64) }},
		{name: "api auth", mutate: func(value *Manifest) { value.APIAuth = "argv" }},
		{name: "api key required", mutate: func(value *Manifest) { value.APIKeyRequired = false }},
		{name: "private ingress", mutate: func(value *Manifest) { value.RequiresPrivateIngress = false }},
		{name: "host publish", mutate: func(value *Manifest) { value.RequiresNoHostPublish = false }},
		{name: "egress", mutate: func(value *Manifest) { value.RequiresDefaultDenyEgress = false }},
		{name: "argv", mutate: func(value *Manifest) { value.Argv[0] = "other" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := cloneManifest(reference)
			test.mutate(&candidate)
			if err := ValidateManifest(candidate); err == nil {
				t.Fatalf("ValidateManifest(%s drift) succeeded: %#v", test.name, candidate)
			}
		})
	}
}

func TestManifestIdentityIsDomainSeparatedAndDoesNotMutateInput(t *testing.T) {
	manifest, err := ReferenceManifest()
	if err != nil {
		t.Fatal(err)
	}
	original := cloneManifest(manifest)
	first := ManifestID(manifest)
	manifest.ManifestID = strings.Repeat("f", 64)
	if second := ManifestID(manifest); second != first {
		t.Fatalf("ManifestID depends on its own field: %q != %q", second, first)
	}
	manifest.ManifestID = original.ManifestID
	if !reflect.DeepEqual(manifest, original) {
		t.Fatalf("ManifestID mutated input: got %#v want %#v", manifest, original)
	}
	if first == rerankManifestWithoutDomainForTest(original) {
		t.Fatal("manifest identity is not domain separated")
	}
}

func TestLegacyManifestSchemaCannotBeReidentifiedAsCurrent(t *testing.T) {
	manifest, err := ReferenceManifest()
	if err != nil {
		t.Fatal(err)
	}
	manifest.SchemaVersion = "rerank-recording-manifest-v1"
	manifest.ManifestID = ManifestID(manifest)
	if err := ValidateManifest(manifest); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("ValidateManifest(legacy schema) = %v", err)
	}
}

func TestManifestRejectsSelfConsistentNegativeZeroIdentity(t *testing.T) {
	manifest, err := ReferenceManifest()
	if err != nil {
		t.Fatal(err)
	}
	manifest.ScoreMinimum = math.Copysign(0, -1)
	manifest.ManifestID = ManifestID(manifest)
	if manifest.ManifestID == "" {
		t.Fatal("negative-zero manifest did not produce the expected distinct JSON identity")
	}
	if err := ValidateManifest(manifest); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("ValidateManifest(negative zero) = %v", err)
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := DecodeManifest(raw); !errors.Is(err, ErrInvalidManifest) || !reflect.DeepEqual(decoded, Manifest{}) {
		t.Fatalf("DecodeManifest(negative zero) = %#v, %v", decoded, err)
	}
}

func TestRecordingManifestNeverOpensProductionAdmission(t *testing.T) {
	manifest, err := ReferenceManifest()
	if err != nil || ValidateManifest(manifest) != nil {
		t.Fatalf("reference recording manifest = %#v, %v", manifest, err)
	}
	descriptor := deploy.ReferenceDescriptor()
	if descriptor.Certified || descriptor.ManifestID != "" {
		t.Fatalf("recording changed deployment certification: %#v", descriptor)
	}
	if err := rerank.RequireCertifiedProfile(manifest.ProfileID); !errors.Is(err, rerank.ErrProfileUnavailable) {
		t.Fatalf("RequireCertifiedProfile() = %v", err)
	}
	if err := deploy.RequireCertifiedDeployment(manifest.ProfileID); !errors.Is(err, deploy.ErrProfileUnavailable) {
		t.Fatalf("RequireCertifiedDeployment() = %v", err)
	}
}

func TestCommittedManifestDecodesToReference(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeManifest(raw)
	if err != nil {
		t.Fatalf("DecodeManifest() error = %v", err)
	}
	want, err := ReferenceManifest()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("committed manifest = %#v, want %#v, err=%v", got, want, err)
	}
	canonical, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	canonical = append(canonical, '\n')
	if !bytes.Equal(raw, canonical) {
		t.Fatalf("committed manifest is not the exact canonical encoding\ngot:  %q\nwant: %q", raw, canonical)
	}
	got.Argv[0] = "mutated"
	got.ImageEntrypoint[0] = "mutated"
	again, err := DecodeManifest(raw)
	if err != nil || again.Argv[0] != "vllm" || again.ImageEntrypoint[0] != "vllm" {
		t.Fatalf("decoded manifest shares mutable state: %#v, %v", again, err)
	}
}

func TestDecodeManifestRejectsStrictJSONSmugglingAndBounds(t *testing.T) {
	reference, err := ReferenceManifest()
	if err != nil {
		t.Fatal(err)
	}
	valid, err := json.Marshal(reference)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		raw  []byte
	}{
		{name: "empty"},
		{name: "BOM", raw: append([]byte{0xef, 0xbb, 0xbf}, valid...)},
		{name: "invalid UTF-8", raw: append(append([]byte(nil), valid...), 0xff)},
		{name: "trailing", raw: append(append([]byte(nil), valid...), []byte(` {}`)...)},
		{name: "unknown", raw: bytes.Replace(valid, []byte(`"profile_id"`), []byte(`"unknown"`), 1)},
		{name: "case alias", raw: bytes.Replace(valid, []byte(`"profile_id"`), []byte(`"ProfileID"`), 1)},
		{name: "null", raw: bytes.Replace(valid, []byte(`"profile_id":"qwen3-reranker-0.6b-v1"`), []byte(`"profile_id":null`), 1)},
		{name: "duplicate", raw: bytes.Replace(valid, []byte(`"profile_id":"qwen3-reranker-0.6b-v1"`), []byte(`"profile_id":"qwen3-reranker-0.6b-v1","profile_id":"qwen3-reranker-0.6b-v1"`), 1)},
		{name: "Unicode duplicate", raw: bytes.Replace(valid, []byte(`"profile_id":"qwen3-reranker-0.6b-v1"`), []byte(`"profile_id":"qwen3-reranker-0.6b-v1","profile\u005fid":"qwen3-reranker-0.6b-v1"`), 1)},
		{name: "lone high surrogate", raw: bytes.Replace(valid, []byte(`Given a web search query`), []byte(`Given a web search query \ud800`), 1)},
		{name: "lone low surrogate", raw: bytes.Replace(valid, []byte(`Given a web search query`), []byte(`Given a web search query \udfff`), 1)},
		{name: "N plus 1", raw: bytes.Repeat([]byte{' '}, MaxManifestBytes+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got, err := DecodeManifest(test.raw); !errors.Is(err, ErrInvalidManifest) || !reflect.DeepEqual(got, Manifest{}) {
				t.Fatalf("DecodeManifest() = %#v, %v", got, err)
			}
		})
	}
	if err := rejectUnpairedSurrogates([]byte(`{"value":"\ud83d\ude00"}`)); err != nil {
		t.Fatalf("valid surrogate pair rejected = %v", err)
	}
}

func rerankManifestWithoutDomainForTest(manifest Manifest) string {
	manifest.ManifestID = ""
	encoded, _ := json.Marshal(manifest)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}
