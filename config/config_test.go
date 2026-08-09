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
