package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/use-agent/purify/config"
)

func TestOpenSnapshotStoreDisabledDoesNotWrite(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "must-not-exist")
	store, err := openSnapshotStore(config.StorageConfig{DataDir: dataDir, SnapshotEnabled: false})
	if err != nil {
		t.Fatalf("openSnapshotStore() error = %v", err)
	}
	if store != nil {
		t.Fatal("openSnapshotStore() returned a store while disabled")
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("disabled snapshot setup touched disk: stat error = %v", err)
	}
}
