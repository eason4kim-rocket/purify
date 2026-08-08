package config

import "testing"

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
