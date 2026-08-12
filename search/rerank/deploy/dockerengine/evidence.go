package dockerengine

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/use-agent/purify/search/rerank/deploy"
)

const (
	referenceEvidenceSchema             = "rerank-docker-evidence-v1"
	ReferenceEffectiveEnvironmentDigest = deploy.ReferenceEnvironmentDigest
)

func newRunningEvidence(plan *referencePlan, owner ownership, image ImageInspection, running RunningInspection) (Evidence, error) {
	if err := ValidateReferenceImage(image); err != nil {
		return Evidence{}, err
	}
	if err := validateRunning(plan, owner, running); err != nil {
		return Evidence{}, err
	}
	redacted, ok := redactedEnvironment(plan.create.Environment)
	if !ok {
		return Evidence{}, ErrContainerRejected
	}
	digest := digestEnvironment(redacted)
	if digest != ReferenceEffectiveEnvironmentDigest {
		return Evidence{}, ErrContainerRejected
	}
	return Evidence{
		containerID:         owner.containerID,
		runID:               plan.create.Labels[LabelRunID],
		specDigest:          plan.specDigest,
		imageDigest:         plan.descriptor.ImageDigest,
		imageConfigID:       plan.descriptor.ImageConfigID,
		platform:            plan.descriptor.Platform,
		environmentDigest:   digest,
		redactedEnvironment: redacted,
		state:               ContainerStatusRunning,
	}, nil
}

func (evidence Evidence) MarshalJSON() ([]byte, error) {
	if !validLowerHex(evidence.containerID, 64) || !validLowerHex(evidence.runID, 32) ||
		!validLowerHex(evidence.specDigest, 64) || !validLowerHex(evidence.environmentDigest, 64) ||
		evidence.imageDigest != deployImageDigest() || evidence.imageConfigID != ReferenceImageConfigID ||
		evidence.platform != "linux/amd64" || evidence.state != ContainerStatusRunning ||
		digestEnvironment(evidence.redactedEnvironment) != evidence.environmentDigest {
		return nil, errors.New("rerank docker engine: invalid evidence")
	}
	type output struct {
		SchemaVersion     string   `json:"schema_version"`
		RecordingOnly     bool     `json:"recording_only"`
		ContainerID       string   `json:"container_id"`
		RunID             string   `json:"run_id"`
		SpecDigest        string   `json:"spec_digest"`
		ImageDigest       string   `json:"image_digest"`
		ImageConfigID     string   `json:"image_config_id"`
		Platform          string   `json:"platform"`
		EnvironmentDigest string   `json:"environment_digest"`
		Environment       []string `json:"redacted_environment"`
		NetworkMode       string   `json:"network_mode"`
		SocketPath        string   `json:"socket_path"`
		State             string   `json:"state"`
	}
	return json.Marshal(output{
		SchemaVersion:     referenceEvidenceSchema,
		RecordingOnly:     true,
		ContainerID:       evidence.containerID,
		RunID:             evidence.runID,
		SpecDigest:        evidence.specDigest,
		ImageDigest:       evidence.imageDigest,
		ImageConfigID:     evidence.imageConfigID,
		Platform:          evidence.platform,
		EnvironmentDigest: evidence.environmentDigest,
		Environment:       append([]string(nil), evidence.redactedEnvironment...),
		NetworkMode:       NetworkModeNone,
		SocketPath:        ReferenceSocketPath,
		State:             evidence.state,
	})
}

func redactedEnvironment(environment []string) ([]string, bool) {
	redacted := append([]string(nil), environment...)
	seenAPIKey := false
	for index, entry := range redacted {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			return nil, false
		}
		if name == "VLLM_API_KEY" {
			if seenAPIKey {
				return nil, false
			}
			seenAPIKey = true
			redacted[index] = "VLLM_API_KEY=<present>"
		}
	}
	if !seenAPIKey {
		return nil, false
	}
	sort.Strings(redacted)
	for index := 1; index < len(redacted); index++ {
		previous, _, _ := strings.Cut(redacted[index-1], "=")
		current, _, _ := strings.Cut(redacted[index], "=")
		if previous == current {
			return nil, false
		}
	}
	return redacted, true
}

func digestEnvironment(redacted []string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(referenceEvidenceEnvVersion + "\x00"))
	var length [binary.MaxVarintLen64]byte
	for _, entry := range redacted {
		encoded := []byte(entry)
		count := binary.PutUvarint(length[:], uint64(len(encoded)))
		_, _ = hash.Write(length[:count])
		_, _ = hash.Write(encoded)
	}
	return hex.EncodeToString(hash.Sum(nil))
}
