// Package replay owns the offline, pinned relevance recording contract. It is
// intentionally separate from production capability admission: a canonical
// manifest identity proves deterministic tuple integrity, not runtime
// authenticity. R-6a must later bind this tuple to authenticated deployment
// evidence before any production registry entry can exist.
package replay

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"reflect"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/use-agent/purify/search/rerank/deploy"
)

const (
	ManifestSchemaVersion  = "rerank-recording-manifest-v1"
	manifestIdentityDomain = ManifestSchemaVersion + "\x00"
)

var ErrInvalidManifest = errors.New("rerank replay: invalid recording manifest")

const MaxManifestBytes = 256 << 10

// Manifest is the complete non-secret tuple under which relevance recordings
// are produced. ManifestID is a canonical content identity, never an
// attestation or certification claim.
type Manifest struct {
	SchemaVersion             string   `json:"schema_version"`
	ManifestID                string   `json:"manifest_id"`
	ProfileID                 string   `json:"profile_id"`
	InstructionVersion        string   `json:"instruction_version"`
	Instruction               string   `json:"instruction"`
	VLLMVersion               string   `json:"vllm_version"`
	ImageDigest               string   `json:"image_digest"`
	ImageIndexDigest          string   `json:"image_index_digest"`
	Platform                  string   `json:"platform"`
	ModelRevision             string   `json:"model_revision"`
	TokenizerRevision         string   `json:"tokenizer_revision"`
	SnapshotManifestSHA256    string   `json:"snapshot_manifest_sha256"`
	ServedModelID             string   `json:"served_model_id"`
	Runner                    string   `json:"runner"`
	MaxModelLen               int      `json:"max_model_len"`
	HFOverrides               string   `json:"hf_overrides"`
	ScoreMinimum              float64  `json:"score_minimum"`
	ScoreMaximum              float64  `json:"score_maximum"`
	PrefixCaching             bool     `json:"prefix_caching"`
	Route                     string   `json:"route"`
	TemplatePath              string   `json:"template_path"`
	TemplateSHA256            string   `json:"template_sha256"`
	EnvironmentPolicyVersion  string   `json:"environment_policy_version"`
	EnvironmentDigest         string   `json:"environment_digest"`
	APIAuth                   string   `json:"api_auth"`
	APIKeyRequired            bool     `json:"api_key_required"`
	RequiresPrivateIngress    bool     `json:"requires_private_ingress"`
	RequiresNoHostPublish     bool     `json:"requires_no_host_publish"`
	RequiresDefaultDenyEgress bool     `json:"requires_default_deny_egress"`
	Argv                      []string `json:"argv"`
}

// ReferenceManifest derives the recording tuple from the single R-3 deploy
// descriptor, then binds it to the repository-owned template and snapshot
// inventory assets. It does not change either production admission gate.
func ReferenceManifest() (Manifest, error) {
	descriptor := deploy.ReferenceDescriptor()
	if !assetDigestMatches(deploy.ReferenceTemplate(), descriptor.TemplateSHA256) ||
		!assetDigestMatches(deploy.ReferenceSnapshotManifest(), descriptor.SnapshotManifestSHA256) {
		return Manifest{}, ErrInvalidManifest
	}
	manifest := Manifest{
		SchemaVersion:             ManifestSchemaVersion,
		ProfileID:                 descriptor.ProfileID,
		InstructionVersion:        descriptor.InstructionVersion,
		Instruction:               descriptor.Instruction,
		VLLMVersion:               descriptor.VLLMVersion,
		ImageDigest:               descriptor.ImageDigest,
		ImageIndexDigest:          descriptor.ImageIndexDigest,
		Platform:                  descriptor.Platform,
		ModelRevision:             descriptor.ModelRevision,
		TokenizerRevision:         descriptor.TokenizerRevision,
		SnapshotManifestSHA256:    descriptor.SnapshotManifestSHA256,
		ServedModelID:             descriptor.ServedModel,
		Runner:                    descriptor.Runner,
		MaxModelLen:               descriptor.MaxModelLen,
		HFOverrides:               descriptor.HFOverrides,
		ScoreMinimum:              descriptor.ScoreMinimum,
		ScoreMaximum:              descriptor.ScoreMaximum,
		PrefixCaching:             descriptor.PrefixCaching,
		Route:                     descriptor.Route,
		TemplatePath:              descriptor.TemplatePath,
		TemplateSHA256:            descriptor.TemplateSHA256,
		EnvironmentPolicyVersion:  descriptor.EnvironmentPolicyVersion,
		EnvironmentDigest:         descriptor.EnvironmentDigest,
		APIAuth:                   descriptor.APIAuth,
		APIKeyRequired:            descriptor.APIKeyRequired,
		RequiresPrivateIngress:    descriptor.RequiresPrivateIngress,
		RequiresNoHostPublish:     descriptor.RequiresNoHostPublish,
		RequiresDefaultDenyEgress: descriptor.RequiresDefaultDenyEgress,
		Argv:                      append([]string(nil), descriptor.Argv...),
	}
	manifest.ManifestID = ManifestID(manifest)
	return manifest, nil
}

// ManifestID returns the domain-separated canonical identity of a manifest.
// The ManifestID field is canonicalized to the empty string before hashing;
// it therefore cannot authenticate or recursively influence its own identity.
func ManifestID(manifest Manifest) string {
	identity := cloneManifest(manifest)
	identity.ManifestID = ""
	encoded, err := json.Marshal(identity)
	if err != nil {
		return ""
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte(manifestIdentityDomain))
	_, _ = digest.Write(encoded)
	return hex.EncodeToString(digest.Sum(nil))
}

// ValidateManifest accepts only the exact current reference tuple and its
// canonical identity. This is recording admission, not production admission.
func ValidateManifest(manifest Manifest) error {
	if manifest.ManifestID == "" || manifest.ManifestID != ManifestID(manifest) {
		return ErrInvalidManifest
	}
	reference, err := ReferenceManifest()
	if err != nil || math.Float64bits(manifest.ScoreMinimum) != math.Float64bits(reference.ScoreMinimum) ||
		math.Float64bits(manifest.ScoreMaximum) != math.Float64bits(reference.ScoreMaximum) ||
		!reflect.DeepEqual(manifest, reference) {
		return ErrInvalidManifest
	}
	return nil
}

// DecodeManifest strictly decodes a committed manifest. Unlike the standard
// Go decoder alone, it rejects duplicate decoded keys, wrong-case aliases,
// null required fields, trailing values, and lone UTF-16 surrogate escapes.
func DecodeManifest(raw []byte) (Manifest, error) {
	if len(raw) == 0 || len(raw) > MaxManifestBytes || !utf8.Valid(raw) ||
		bytes.HasPrefix(raw, []byte{0xef, 0xbb, 0xbf}) || rejectUnpairedSurrogates(raw) != nil || rejectDuplicateFields(raw) != nil {
		return Manifest{}, ErrInvalidManifest
	}
	fields := []string{
		"schema_version", "manifest_id", "profile_id", "instruction_version", "instruction", "vllm_version",
		"image_digest", "image_index_digest", "platform", "model_revision", "tokenizer_revision",
		"snapshot_manifest_sha256", "served_model_id", "runner", "max_model_len", "hf_overrides",
		"score_minimum", "score_maximum", "prefix_caching", "route", "template_path", "template_sha256",
		"environment_policy_version", "environment_digest", "api_auth", "api_key_required",
		"requires_private_ingress", "requires_no_host_publish", "requires_default_deny_egress", "argv",
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil || len(root) != len(fields) {
		return Manifest{}, ErrInvalidManifest
	}
	for _, field := range fields {
		value, present := root[field]
		if !present || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return Manifest{}, ErrInvalidManifest
		}
	}
	allowed := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		allowed[field] = struct{}{}
	}
	for field := range root {
		if _, present := allowed[field]; !present {
			return Manifest{}, ErrInvalidManifest
		}
	}
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, ErrInvalidManifest
	}
	if err := ValidateManifest(manifest); err != nil {
		return Manifest{}, err
	}
	return cloneManifest(manifest), nil
}

func assetDigestMatches(raw []byte, want string) bool {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]) == want
}

func cloneManifest(source Manifest) Manifest {
	cloned := source
	cloned.Argv = append([]string(nil), source.Argv...)
	return cloned
}

func rejectDuplicateFields(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var consume func() error
	consume = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		switch delimiter {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				key, ok := keyToken.(string)
				if err != nil || !ok {
					return ErrInvalidManifest
				}
				if _, duplicate := seen[key]; duplicate {
					return ErrInvalidManifest
				}
				seen[key] = struct{}{}
				if err := consume(); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim('}') {
				return ErrInvalidManifest
			}
		case '[':
			for decoder.More() {
				if err := consume(); err != nil {
					return err
				}
			}
			closing, err := decoder.Token()
			if err != nil || closing != json.Delim(']') {
				return ErrInvalidManifest
			}
		default:
			return ErrInvalidManifest
		}
		return nil
	}
	if err := consume(); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return ErrInvalidManifest
	}
	return nil
}

func rejectUnpairedSurrogates(raw []byte) error {
	inString := false
	for index := 0; index < len(raw); index++ {
		switch raw[index] {
		case '"':
			inString = !inString
		case '\\':
			if !inString || index+1 >= len(raw) {
				continue
			}
			if raw[index+1] != 'u' {
				index++
				continue
			}
			unit, ok := hexQuad(raw, index+2)
			if !ok {
				return ErrInvalidManifest
			}
			index += 5
			if unit >= 0xd800 && unit <= 0xdbff {
				if index+6 >= len(raw) || raw[index+1] != '\\' || raw[index+2] != 'u' {
					return ErrInvalidManifest
				}
				low, ok := hexQuad(raw, index+3)
				if !ok || low < 0xdc00 || low > 0xdfff || !utf16.IsSurrogate(rune(unit)) || !utf16.IsSurrogate(rune(low)) {
					return ErrInvalidManifest
				}
				index += 6
			} else if unit >= 0xdc00 && unit <= 0xdfff {
				return ErrInvalidManifest
			}
		}
	}
	return nil
}

func hexQuad(raw []byte, start int) (uint16, bool) {
	if start < 0 || start+4 > len(raw) {
		return 0, false
	}
	var value uint16
	for _, character := range raw[start : start+4] {
		value <<= 4
		switch {
		case character >= '0' && character <= '9':
			value += uint16(character - '0')
		case character >= 'a' && character <= 'f':
			value += uint16(character-'a') + 10
		case character >= 'A' && character <= 'F':
			value += uint16(character-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}
