package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/use-agent/purify/ledger"
)

func TestStorageConfigDefaults(t *testing.T) {
	t.Setenv("PURIFY_DATA_DIR", "")
	t.Setenv("PURIFY_SNAPSHOT_ENABLED", "")
	t.Setenv("PURIFY_SIGNING_KEY", "")
	cfg := Load()
	if cfg.Storage.DataDir != "./data" {
		t.Fatalf("DataDir = %q, want ./data", cfg.Storage.DataDir)
	}
	if !cfg.Storage.SnapshotEnabled {
		t.Fatal("SnapshotEnabled = false, want true")
	}
	if cfg.Storage.SigningKey != "" {
		t.Fatalf("SigningKey = %q, want empty", cfg.Storage.SigningKey)
	}
}

func TestStorageConfigEnvironment(t *testing.T) {
	t.Setenv("PURIFY_DATA_DIR", "/tmp/purify-test-data")
	t.Setenv("PURIFY_SNAPSHOT_ENABLED", "false")
	t.Setenv("PURIFY_SIGNING_KEY", "0011")
	cfg := Load()
	if cfg.Storage.DataDir != "/tmp/purify-test-data" || cfg.Storage.SnapshotEnabled || cfg.Storage.SigningKey != "0011" {
		t.Fatalf("Storage = %#v", cfg.Storage)
	}
}

func TestCompilerConfigDefaults(t *testing.T) {
	t.Setenv("PURIFY_COMPILER_ENABLED", "")
	t.Setenv("PURIFY_COMPILER_API_KEY", "")
	t.Setenv("PURIFY_COMPILER_MODEL", "")
	t.Setenv("PURIFY_COMPILER_BASE_URL", "")

	cfg := Load()
	if cfg.Compiler.Enabled || cfg.Compiler.APIKey != "" ||
		cfg.Compiler.Model != "gpt-4o-mini" || cfg.Compiler.BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("Compiler = %#v", cfg.Compiler)
	}
}

func TestCompilerConfigEnvironment(t *testing.T) {
	t.Setenv("PURIFY_COMPILER_ENABLED", "true")
	t.Setenv("PURIFY_COMPILER_API_KEY", " process-secret ")
	t.Setenv("PURIFY_COMPILER_MODEL", "provider/model-v2")
	t.Setenv("PURIFY_COMPILER_BASE_URL", "https://llm.example.test/v1/")

	cfg := Load()
	if !cfg.Compiler.Enabled || cfg.Compiler.APIKey != " process-secret " ||
		cfg.Compiler.Model != "provider/model-v2" || cfg.Compiler.BaseURL != "https://llm.example.test/v1/" {
		t.Fatalf("Compiler = %#v", cfg.Compiler)
	}
	if err := ValidateCompilerConfig(cfg.Compiler, true); err != nil {
		t.Fatalf("ValidateCompilerConfig() error = %v", err)
	}
}

func TestEAVAllowPrivateConfigDefaultsAndEnvironment(t *testing.T) {
	t.Setenv("PURIFY_EAV_LLM_ALLOW_PRIVATE", "")
	if cfg := Load(); cfg.EAV.AllowPrivate {
		t.Fatalf("default EAV AllowPrivate = true, want false")
	}

	t.Setenv("PURIFY_EAV_LLM_ALLOW_PRIVATE", "true")
	if cfg := Load(); !cfg.EAV.AllowPrivate {
		t.Fatalf("configured EAV AllowPrivate = false, want true")
	}

	t.Setenv("PURIFY_EAV_LLM_ALLOW_PRIVATE", "false")
	if cfg := Load(); cfg.EAV.AllowPrivate {
		t.Fatalf("explicit-false EAV AllowPrivate = true, want false")
	}

	// Load has no error return. Invalid boolean environment values must retain
	// the secure zero-value default rather than enabling private networking.
	t.Setenv("PURIFY_EAV_LLM_ALLOW_PRIVATE", "not-a-boolean")
	if cfg := Load(); cfg.EAV.AllowPrivate {
		t.Fatalf("invalid EAV AllowPrivate = true, want fail-closed false")
	}
}

func TestValidateEAVConfigDisabledAllowPrivateIsInert(t *testing.T) {
	err := ValidateEAVConfig(EAVConfig{
		AllowPrivate: true,
		APIKey:       "   ",
		Model:        "invalid model",
		BaseURL:      "://invalid",
		CacheEntries: -1,
	})
	if err != nil {
		t.Fatalf("ValidateEAVConfig(disabled) error = %v", err)
	}
}

func TestHealConfigDefaultsAndEnvironment(t *testing.T) {
	t.Setenv("PURIFY_HEAL_WEBHOOK_URL", "")
	t.Setenv("PURIFY_HEAL_WEBHOOK_SECRET", "")
	cfg := Load()
	if cfg.Heal != (HealConfig{}) {
		t.Fatalf("Heal defaults = %#v", cfg.Heal)
	}

	t.Setenv("PURIFY_HEAL_WEBHOOK_URL", "HTTPS://Hooks.Example.COM:443/heal")
	t.Setenv("PURIFY_HEAL_WEBHOOK_SECRET", "process-secret")
	cfg = Load()
	if cfg.Heal.WebhookURL != "HTTPS://Hooks.Example.COM:443/heal" ||
		cfg.Heal.WebhookSecret != "process-secret" {
		t.Fatalf("Heal environment = %#v", cfg.Heal)
	}
	if err := ValidateHealConfig(cfg.Heal, true); err != nil {
		t.Fatalf("ValidateHealConfig(environment) = %v", err)
	}
}

func TestSearchConfigDefaultsAndEnvironment(t *testing.T) {
	t.Setenv("PURIFY_SEARCH_INDEX_PATH", "")
	cfg := Load()
	if cfg.Search != (SearchConfig{}) {
		t.Fatalf("Search defaults = %#v", cfg.Search)
	}

	t.Setenv("PURIFY_SEARCH_INDEX_PATH", " ./data/index.db ")
	cfg = Load()
	if cfg.Search.IndexPath != " ./data/index.db " {
		t.Fatalf("Search environment = %#v", cfg.Search)
	}
}

func TestRerankConfigDefaultsAndEnvironment(t *testing.T) {
	for _, key := range []string{
		"PURIFY_RERANK_ENABLED",
		"PURIFY_RERANK_ENDPOINT",
		"PURIFY_RERANK_API_KEY",
		"PURIFY_RERANK_PROFILE",
		"PURIFY_RERANK_ALLOW_PRIVATE",
		"PURIFY_RERANK_TIMEOUT_SECONDS",
	} {
		t.Setenv(key, "")
	}

	cfg := Load()
	wantDefault := RerankConfig{
		Profile:        DefaultRerankProfile,
		TimeoutSeconds: 5,
	}
	if cfg.Rerank != wantDefault {
		t.Fatalf("Rerank defaults = %#v, want %#v", cfg.Rerank, wantDefault)
	}
	if err := ValidateRerankConfig(cfg.Rerank); err != nil {
		t.Fatalf("ValidateRerankConfig(default disabled) error = %v", err)
	}

	t.Setenv("PURIFY_RERANK_ENABLED", "true")
	t.Setenv("PURIFY_RERANK_ENDPOINT", "http://reranker.internal:8000/v1/rerank")
	t.Setenv("PURIFY_RERANK_API_KEY", "process-rerank-key")
	t.Setenv("PURIFY_RERANK_PROFILE", "test-profile-v1")
	t.Setenv("PURIFY_RERANK_ALLOW_PRIVATE", "true")
	t.Setenv("PURIFY_RERANK_TIMEOUT_SECONDS", "10")
	cfg = Load()
	wantConfigured := RerankConfig{
		Enabled:        true,
		Endpoint:       "http://reranker.internal:8000/v1/rerank",
		APIKey:         "process-rerank-key",
		Profile:        "test-profile-v1",
		AllowPrivate:   true,
		TimeoutSeconds: 10,
	}
	if cfg.Rerank != wantConfigured {
		t.Fatalf("Rerank environment = %#v, want %#v", cfg.Rerank, wantConfigured)
	}
	if err := ValidateRerankConfig(cfg.Rerank); err != nil {
		t.Fatalf("ValidateRerankConfig(environment) error = %v", err)
	}
}

func TestRerankTimeoutEnvironmentIsStrictAndDisabledInert(t *testing.T) {
	setValidRerankEnvironment(t)
	for _, raw := range []string{"5s", "1.5", "+5", " 5", "five"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("PURIFY_RERANK_ENABLED", "true")
			t.Setenv("PURIFY_RERANK_TIMEOUT_SECONDS", raw)
			cfg := Load()
			if cfg.Rerank.TimeoutSeconds != 0 {
				t.Fatalf("malformed timeout %q loaded as %d, want invalid sentinel 0", raw, cfg.Rerank.TimeoutSeconds)
			}
			if err := ValidateRerankConfig(cfg.Rerank); !errors.Is(err, ErrInvalidRerankConfig) {
				t.Fatalf("ValidateRerankConfig(malformed timeout %q) = %v", raw, err)
			}

			t.Setenv("PURIFY_RERANK_ENABLED", "false")
			cfg = Load()
			if err := ValidateRerankConfig(cfg.Rerank); err != nil {
				t.Fatalf("ValidateRerankConfig(disabled malformed timeout %q) = %v", raw, err)
			}
		})
	}
}

func TestValidateRerankConfigMatrixAndRedaction(t *testing.T) {
	valid := RerankConfig{
		Enabled:        true,
		Endpoint:       "https://rerank.example.test/v1/rerank",
		APIKey:         "rerank-process-secret",
		Profile:        DefaultRerankProfile,
		TimeoutSeconds: 5,
	}
	tests := []struct {
		name    string
		config  RerankConfig
		invalid bool
	}{
		{name: "valid HTTPS", config: valid},
		{name: "valid private HTTP", config: withRerankEndpoint(valid, "http://reranker.internal:8000/v1/rerank", true)},
		{name: "disabled stale values are inert", config: RerankConfig{Endpoint: "://bad", APIKey: "\xff", Profile: " bad ", AllowPrivate: true}, invalid: false},
		{name: "missing endpoint", config: withRerankEndpoint(valid, "", false), invalid: true},
		{name: "endpoint leading space", config: withRerankEndpoint(valid, " "+valid.Endpoint, false), invalid: true},
		{name: "endpoint trailing space", config: withRerankEndpoint(valid, valid.Endpoint+" ", false), invalid: true},
		{name: "endpoint invalid UTF-8", config: withRerankEndpoint(valid, "https://rerank.example.test/\xff", false), invalid: true},
		{name: "endpoint control", config: withRerankEndpoint(valid, "https://rerank.example.test/v1/rerank\n", false), invalid: true},
		{name: "relative endpoint", config: withRerankEndpoint(valid, "/v1/rerank", false), invalid: true},
		{name: "unsupported scheme", config: withRerankEndpoint(valid, "ftp://rerank.example.test/v1/rerank", false), invalid: true},
		{name: "uppercase scheme", config: withRerankEndpoint(valid, "HTTPS://rerank.example.test/v1/rerank", false), invalid: true},
		{name: "missing host", config: withRerankEndpoint(valid, "https:///v1/rerank", false), invalid: true},
		{name: "opaque endpoint", config: withRerankEndpoint(valid, "https:rerank.example.test/v1/rerank", false), invalid: true},
		{name: "userinfo", config: withRerankEndpoint(valid, "https://user:secret@rerank.example.test/v1/rerank", false), invalid: true},
		{name: "query", config: withRerankEndpoint(valid, "https://rerank.example.test/v1/rerank?token=secret", false), invalid: true},
		{name: "empty query", config: withRerankEndpoint(valid, "https://rerank.example.test/v1/rerank?", false), invalid: true},
		{name: "fragment", config: withRerankEndpoint(valid, "https://rerank.example.test/v1/rerank#secret", false), invalid: true},
		{name: "empty fragment", config: withRerankEndpoint(valid, "https://rerank.example.test/v1/rerank#", false), invalid: true},
		{name: "wrong path", config: withRerankEndpoint(valid, "https://rerank.example.test/rerank", false), invalid: true},
		{name: "trailing slash", config: withRerankEndpoint(valid, "https://rerank.example.test/v1/rerank/", false), invalid: true},
		{name: "escaped path", config: withRerankEndpoint(valid, "https://rerank.example.test/v1/%72erank", false), invalid: true},
		{name: "empty port", config: withRerankEndpoint(valid, "https://rerank.example.test:/v1/rerank", false), invalid: true},
		{name: "zero port", config: withRerankEndpoint(valid, "https://rerank.example.test:0/v1/rerank", false), invalid: true},
		{name: "port too large", config: withRerankEndpoint(valid, "https://rerank.example.test:65536/v1/rerank", false), invalid: true},
		{name: "maximum port", config: withRerankEndpoint(valid, "https://rerank.example.test:65535/v1/rerank", false)},
		{name: "zoned IPv6", config: withRerankEndpoint(valid, "https://[fe80::1%25eth0]/v1/rerank", true), invalid: true},
		{name: "public HTTP without opt-in", config: withRerankEndpoint(valid, "http://rerank.example.test/v1/rerank", false), invalid: true},
		{name: "empty key", config: withRerankKey(valid, ""), invalid: true},
		{name: "key leading space", config: withRerankKey(valid, " "+valid.APIKey), invalid: true},
		{name: "key trailing space", config: withRerankKey(valid, valid.APIKey+" "), invalid: true},
		{name: "key invalid UTF-8", config: withRerankKey(valid, "\xff"), invalid: true},
		{name: "key control", config: withRerankKey(valid, "secret\nvalue"), invalid: true},
		{name: "empty profile", config: withRerankProfile(valid, ""), invalid: true},
		{name: "profile leading space", config: withRerankProfile(valid, " "+valid.Profile), invalid: true},
		{name: "profile trailing space", config: withRerankProfile(valid, valid.Profile+" "), invalid: true},
		{name: "profile invalid UTF-8", config: withRerankProfile(valid, "\xff"), invalid: true},
		{name: "profile control", config: withRerankProfile(valid, "profile\nname"), invalid: true},
		{name: "timeout zero", config: withRerankTimeout(valid, 0), invalid: true},
		{name: "timeout over maximum", config: withRerankTimeout(valid, 11), invalid: true},
		{name: "timeout minimum", config: withRerankTimeout(valid, 1)},
		{name: "timeout maximum", config: withRerankTimeout(valid, 10)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateRerankConfig(test.config)
			if got := errors.Is(err, ErrInvalidRerankConfig); got != test.invalid {
				t.Fatalf("ValidateRerankConfig() = %v, invalid=%v want=%v", err, got, test.invalid)
			}
			if err == nil {
				return
			}
			for _, secret := range []string{test.config.Endpoint, test.config.APIKey, test.config.Profile} {
				if secret != "" && strings.Contains(err.Error(), secret) {
					t.Fatalf("validation error leaked configuration: %v", err)
				}
			}
		})
	}
}

func TestRerankConfigResourceLimitsAreInclusive(t *testing.T) {
	endpointPrefix := "https://"
	endpointSuffix := "/v1/rerank"
	value := RerankConfig{
		Enabled:        true,
		Endpoint:       endpointPrefix + strings.Repeat("a", maximumRerankEndpointBytes-len(endpointPrefix)-len(endpointSuffix)) + endpointSuffix,
		APIKey:         strings.Repeat("k", maximumRerankAPIKeyBytes),
		Profile:        strings.Repeat("p", maximumRerankProfileBytes),
		TimeoutSeconds: 5,
	}
	if err := ValidateRerankConfig(value); err != nil {
		t.Fatalf("ValidateRerankConfig(at limits) = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*RerankConfig)
	}{
		{name: "endpoint N plus 1", mutate: func(config *RerankConfig) { config.Endpoint += "a" }},
		{name: "key N plus 1", mutate: func(config *RerankConfig) { config.APIKey += "k" }},
		{name: "profile N plus 1", mutate: func(config *RerankConfig) { config.Profile += "p" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := value
			test.mutate(&candidate)
			if err := ValidateRerankConfig(candidate); !errors.Is(err, ErrInvalidRerankConfig) {
				t.Fatalf("ValidateRerankConfig() = %v, want ErrInvalidRerankConfig", err)
			}
		})
	}
}

func setValidRerankEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("PURIFY_RERANK_ENDPOINT", "https://rerank.example.test/v1/rerank")
	t.Setenv("PURIFY_RERANK_API_KEY", "process-rerank-key")
	t.Setenv("PURIFY_RERANK_PROFILE", DefaultRerankProfile)
	t.Setenv("PURIFY_RERANK_ALLOW_PRIVATE", "false")
}

func withRerankEndpoint(source RerankConfig, endpoint string, allowPrivate bool) RerankConfig {
	source.Endpoint = endpoint
	source.AllowPrivate = allowPrivate
	return source
}

func withRerankKey(source RerankConfig, key string) RerankConfig {
	source.APIKey = key
	return source
}

func withRerankProfile(source RerankConfig, profile string) RerankConfig {
	source.Profile = profile
	return source
}

func withRerankTimeout(source RerankConfig, timeout int) RerankConfig {
	source.TimeoutSeconds = timeout
	return source
}

func TestValidateHealConfigMatrixAndRedaction(t *testing.T) {
	valid := HealConfig{WebhookURL: "https://hooks.example.com/heal", WebhookSecret: "process-secret"}
	tests := []struct {
		name            string
		config          HealConfig
		snapshots       bool
		wantInvalid     bool
		forbiddenDetail string
	}{
		{name: "snapshots on empty", snapshots: true},
		{name: "snapshots off empty inert"},
		{name: "valid", config: valid, snapshots: true},
		{name: "URL without secret", config: HealConfig{WebhookURL: valid.WebhookURL}, snapshots: true},
		{name: "canonicalizable URL", config: HealConfig{WebhookURL: " HTTPS://Hooks.Example.COM:443/heal "}, snapshots: true},
		{name: "configured URL snapshots off", config: HealConfig{WebhookURL: valid.WebhookURL}, wantInvalid: true},
		{name: "secret without URL snapshots on", config: HealConfig{WebhookSecret: valid.WebhookSecret}, snapshots: true, wantInvalid: true, forbiddenDetail: valid.WebhookSecret},
		{name: "secret without URL snapshots off", config: HealConfig{WebhookSecret: valid.WebhookSecret}, wantInvalid: true, forbiddenDetail: valid.WebhookSecret},
		{name: "private literal", config: HealConfig{WebhookURL: "http://127.0.0.1/heal"}, snapshots: true, wantInvalid: true, forbiddenDetail: "127.0.0.1"},
		{name: "localhost", config: HealConfig{WebhookURL: "http://localhost/heal"}, snapshots: true, wantInvalid: true, forbiddenDetail: "localhost"},
		{name: "userinfo", config: HealConfig{WebhookURL: "https://user:secret@hooks.example.com/heal"}, snapshots: true, wantInvalid: true, forbiddenDetail: "user:secret"},
		{name: "unsupported scheme", config: HealConfig{WebhookURL: "ftp://hooks.example.com/heal"}, snapshots: true, wantInvalid: true},
		{name: "empty port", config: HealConfig{WebhookURL: "https://hooks.example.com:/heal"}, snapshots: true, wantInvalid: true},
		{name: "zero port", config: HealConfig{WebhookURL: "https://hooks.example.com:0/heal"}, snapshots: true, wantInvalid: true},
		{name: "invalid URL UTF-8", config: HealConfig{WebhookURL: "https://hooks.example.com/\xff"}, snapshots: true, wantInvalid: true},
		{name: "invalid secret UTF-8", config: HealConfig{WebhookURL: valid.WebhookURL, WebhookSecret: "\xff"}, snapshots: true, wantInvalid: true},
		{name: "URL N plus 1", config: HealConfig{WebhookURL: "https://hooks.example.com/" + strings.Repeat("p", ledger.MaxOutboxURLBytes)}, snapshots: true, wantInvalid: true},
		{name: "secret N plus 1", config: HealConfig{WebhookURL: valid.WebhookURL, WebhookSecret: strings.Repeat("s", ledger.MaxOutboxSecretBytes+1)}, snapshots: true, wantInvalid: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateHealConfig(test.config, test.snapshots)
			if got := errors.Is(err, ErrInvalidHealConfig); got != test.wantInvalid {
				t.Fatalf("ValidateHealConfig() = %v, invalid=%v want=%v", err, got, test.wantInvalid)
			}
			for _, forbidden := range []string{test.forbiddenDetail, test.config.WebhookSecret} {
				if err != nil && forbidden != "" && strings.Contains(err.Error(), forbidden) {
					t.Fatalf("ValidateHealConfig leaked configuration: %v", err)
				}
			}
		})
	}
}

func TestHealWebhookResourceLimitsAreInclusive(t *testing.T) {
	prefix := "https://hooks.example.com/"
	value := HealConfig{
		WebhookURL:    prefix + strings.Repeat("p", ledger.MaxOutboxURLBytes-len(prefix)),
		WebhookSecret: strings.Repeat("s", ledger.MaxOutboxSecretBytes),
	}
	if err := ValidateHealConfig(value, true); err != nil {
		t.Fatalf("ValidateHealConfig(at limits) = %v", err)
	}
	value.WebhookSecret += "s"
	if err := ValidateHealConfig(value, true); !errors.Is(err, ErrInvalidHealConfig) {
		t.Fatalf("ValidateHealConfig(secret N+1) = %v", err)
	}
}

func TestValidateCompilerConfigMatrix(t *testing.T) {
	valid := CompilerConfig{
		Enabled: true,
		APIKey:  "process-secret",
		Model:   "gpt-4o-mini",
		BaseURL: "https://api.openai.com/v1",
	}
	tests := []struct {
		name             string
		config           CompilerConfig
		snapshotEnabled  bool
		wantInvalidError bool
	}{
		{name: "valid", config: valid, snapshotEnabled: true},
		{name: "disabled is inert", config: CompilerConfig{
			APIKey:  strings.Repeat("k", maximumCompilerCredentialBytes+1),
			Model:   "invalid model",
			BaseURL: "://invalid",
		}, wantInvalidError: false},
		{name: "trim-empty key", config: withCompilerKey(valid, " \t\n "), snapshotEnabled: true, wantInvalidError: true},
		{name: "key too long", config: withCompilerKey(valid, strings.Repeat("k", maximumCompilerCredentialBytes+1)), snapshotEnabled: true, wantInvalidError: true},
		{name: "snapshots disabled", config: valid, snapshotEnabled: false, wantInvalidError: true},
		{name: "empty model", config: withCompilerModel(valid, "  "), snapshotEnabled: true, wantInvalidError: true},
		{name: "model whitespace", config: withCompilerModel(valid, "model name"), snapshotEnabled: true, wantInvalidError: true},
		{name: "model control", config: withCompilerModel(valid, "model\nname"), snapshotEnabled: true, wantInvalidError: true},
		{name: "model too long", config: withCompilerModel(valid, strings.Repeat("m", maximumCompilerModelBytes+1)), snapshotEnabled: true, wantInvalidError: true},
		{name: "relative base URL", config: withCompilerBaseURL(valid, "/v1"), snapshotEnabled: true, wantInvalidError: true},
		{name: "unsupported base URL scheme", config: withCompilerBaseURL(valid, "ftp://llm.example.test/v1"), snapshotEnabled: true, wantInvalidError: true},
		{name: "base URL userinfo", config: withCompilerBaseURL(valid, "https://user:pass@llm.example.test/v1"), snapshotEnabled: true, wantInvalidError: true},
		{name: "base URL query", config: withCompilerBaseURL(valid, "https://llm.example.test/v1?token=secret"), snapshotEnabled: true, wantInvalidError: true},
		{name: "empty base URL query", config: withCompilerBaseURL(valid, "https://llm.example.test/v1?"), snapshotEnabled: true, wantInvalidError: true},
		{name: "base URL fragment", config: withCompilerBaseURL(valid, "https://llm.example.test/v1#fragment"), snapshotEnabled: true, wantInvalidError: true},
		{name: "base URL too long", config: withCompilerBaseURL(valid, "https://llm.example.test/"+strings.Repeat("p", maximumCompilerBaseURLBytes)), snapshotEnabled: true, wantInvalidError: true},
		{name: "empty explicit port", config: withCompilerBaseURL(valid, "https://llm.example.test:/v1"), snapshotEnabled: true, wantInvalidError: true},
		{name: "zero port", config: withCompilerBaseURL(valid, "https://llm.example.test:0/v1"), snapshotEnabled: true, wantInvalidError: true},
		{name: "port too large", config: withCompilerBaseURL(valid, "https://llm.example.test:65536/v1"), snapshotEnabled: true, wantInvalidError: true},
		{name: "maximum port", config: withCompilerBaseURL(valid, "https://llm.example.test:65535/v1"), snapshotEnabled: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateCompilerConfig(test.config, test.snapshotEnabled)
			if got := errors.Is(err, ErrInvalidCompilerConfig); got != test.wantInvalidError {
				t.Fatalf("ValidateCompilerConfig() error = %v, invalid = %v, want %v", err, got, test.wantInvalidError)
			}
			if err != nil && strings.Contains(err.Error(), test.config.APIKey) && strings.TrimSpace(test.config.APIKey) != "" {
				t.Fatalf("validation error leaked API key: %v", err)
			}
		})
	}
}

func TestCompilerCredentialAndBaseURLLimitsAreInclusive(t *testing.T) {
	basePrefix := "https://llm.example.test/"
	value := CompilerConfig{
		Enabled: true,
		APIKey:  strings.Repeat("k", maximumCompilerCredentialBytes),
		Model:   "gpt-4o-mini",
		BaseURL: basePrefix + strings.Repeat("p", maximumCompilerBaseURLBytes-len(basePrefix)),
	}
	if err := ValidateCompilerConfig(value, true); err != nil {
		t.Fatalf("ValidateCompilerConfig(at limits) error = %v", err)
	}
	value.APIKey += "k"
	if err := ValidateCompilerConfig(value, true); !errors.Is(err, ErrInvalidCompilerConfig) {
		t.Fatalf("ValidateCompilerConfig(key N+1) error = %v", err)
	}
	value.APIKey = "process-secret"
	value.BaseURL += "p"
	if err := ValidateCompilerConfig(value, true); !errors.Is(err, ErrInvalidCompilerConfig) {
		t.Fatalf("ValidateCompilerConfig(base URL N+1) error = %v", err)
	}
}

func withCompilerKey(value CompilerConfig, key string) CompilerConfig {
	value.APIKey = key
	return value
}

func withCompilerModel(value CompilerConfig, model string) CompilerConfig {
	value.Model = model
	return value
}

func withCompilerBaseURL(value CompilerConfig, baseURL string) CompilerConfig {
	value.BaseURL = baseURL
	return value
}
