package config

import (
	"errors"
	"strings"
	"testing"
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
