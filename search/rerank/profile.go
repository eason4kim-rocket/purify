package rerank

import (
	"errors"
)

const (
	// ReferenceProfileID names the pinned Qwen descriptor used by the strict
	// adapter and recorder. R-3 has no production constructor; R-6a must first
	// define an authenticated deployment handoff and R-6 must admit its manifest.
	ReferenceProfileID = "qwen3-reranker-0.6b-v1"

	ReferenceServedModel = "Qwen/Qwen3-Reranker-0.6B"
	ReferenceInstruction = "Given a web search query, retrieve relevant passages that answer the query"
)

var ErrProfileUnavailable = errors.New("rerank: certified profile is unavailable")

type profileDescriptor struct {
	id          string
	servedModel string
}

var referenceProfile = profileDescriptor{
	id:          ReferenceProfileID,
	servedModel: ReferenceServedModel,
}

// RequireCertifiedProfile is the process-start admission gate. R-3 ships a
// descriptor and transport for testing/recording, but production remains
// unavailable until R-6a defines an authenticated deployment handoff and R-6
// commits the matching recording manifest. R-3 intentionally has no positive
// production runtime factory: adding one is part of that later security card,
// not a latent profile-map branch that operator configuration could unlock.
func RequireCertifiedProfile(string) error { return ErrProfileUnavailable }
