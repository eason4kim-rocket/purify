package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/use-agent/purify/config"
	"github.com/use-agent/purify/search/rerank"
)

func TestValidateManagedRerankConfigDisabledIsInert(t *testing.T) {
	if config.DefaultRerankProfile != rerank.ReferenceProfileID {
		t.Fatalf("default profile drift: config=%q rerank=%q", config.DefaultRerankProfile, rerank.ReferenceProfileID)
	}
	if err := validateManagedRerankConfig(config.RerankConfig{
		Endpoint:       "://invalid",
		APIKey:         " private-key ",
		Profile:        "unknown",
		AllowPrivate:   true,
		TimeoutSeconds: -1,
	}); err != nil {
		t.Fatalf("disabled validation error = %v", err)
	}
}

func TestValidateManagedRerankConfigRejectsUncertifiedProfileWithoutSecrets(t *testing.T) {
	const secret = "must-not-leak"
	err := validateManagedRerankConfig(config.RerankConfig{
		Enabled:        true,
		Endpoint:       "https://rerank.example.test/v1/rerank",
		APIKey:         secret,
		Profile:        config.DefaultRerankProfile,
		TimeoutSeconds: 5,
	})
	if !errors.Is(err, rerank.ErrProfileUnavailable) {
		t.Fatalf("validation error = %v, want ErrProfileUnavailable", err)
	}
	for _, forbidden := range []string{secret, "rerank.example.test", config.DefaultRerankProfile} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("validation error leaked %q: %v", forbidden, err)
		}
	}
}

func TestValidateManagedRerankConfigRejectsUncertifiedProfileBeforeParsingConfiguration(t *testing.T) {
	err := validateManagedRerankConfig(config.RerankConfig{
		Enabled:        true,
		Endpoint:       "://not-an-endpoint",
		APIKey:         " invalid key ",
		Profile:        config.DefaultRerankProfile,
		TimeoutSeconds: -1,
	})
	if !errors.Is(err, rerank.ErrProfileUnavailable) || errors.Is(err, config.ErrInvalidRerankConfig) {
		t.Fatalf("validation error = %v, want only ErrProfileUnavailable", err)
	}
}

func TestRunRejectsUncertifiedRerankerBeforeDurableOrBrowserState(t *testing.T) {
	dataDirectory := filepath.Join(t.TempDir(), "must-not-exist")
	t.Setenv("PURIFY_DATA_DIR", dataDirectory)
	t.Setenv("PURIFY_RERANK_ENABLED", "true")
	t.Setenv("PURIFY_RERANK_ENDPOINT", "https://rerank.example.test/v1/rerank")
	t.Setenv("PURIFY_RERANK_API_KEY", "process-key")
	t.Setenv("PURIFY_RERANK_PROFILE", config.DefaultRerankProfile)
	t.Setenv("PURIFY_RERANK_TIMEOUT_SECONDS", "5")

	err := run()
	if !errors.Is(err, rerank.ErrProfileUnavailable) {
		t.Fatalf("run() error = %v, want ErrProfileUnavailable", err)
	}
	if _, statErr := os.Stat(dataDirectory); !os.IsNotExist(statErr) {
		t.Fatalf("early reranker admission touched durable state: stat error = %v", statErr)
	}
}
