package receipts

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/use-agent/purify/config"
)

func TestLoadOrCreateKeyFromConfiguredSeed(t *testing.T) {
	seed := bytes.Repeat([]byte{0xa1}, ed25519.SeedSize)
	unusedDataDir := filepath.Join(t.TempDir(), "must-not-exist")
	priv, kid, err := LoadOrCreateKey(config.StorageConfig{
		DataDir:    unusedDataDir,
		SigningKey: "  " + hex.EncodeToString(seed) + "  ",
	})
	if err != nil {
		t.Fatalf("LoadOrCreateKey() error = %v", err)
	}
	want := ed25519.NewKeyFromSeed(seed)
	if !bytes.Equal(priv, want) {
		t.Fatal("configured seed produced the wrong private key")
	}
	wantKID, _ := KeyID(want.Public().(ed25519.PublicKey))
	if kid != wantKID {
		t.Fatalf("kid = %q, want %q", kid, wantKID)
	}
	if _, err := os.Stat(unusedDataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("configured seed unexpectedly wrote data directory: %v", err)
	}
}

func TestLoadOrCreateKeyRejectsInvalidConfiguredSeed(t *testing.T) {
	tests := []string{"zz", "00", strings.Repeat("00", ed25519.SeedSize+1)}
	for _, encoded := range tests {
		_, _, err := LoadOrCreateKey(config.StorageConfig{SigningKey: encoded})
		if err == nil {
			t.Errorf("LoadOrCreateKey(SigningKey=%q) unexpectedly succeeded", encoded)
		}
	}
}

func TestLoadOrCreateKeyGenerates0600AndReloads(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	cfg := config.StorageConfig{DataDir: dataDir}
	first, firstKID, err := LoadOrCreateKey(cfg)
	if err != nil {
		t.Fatalf("first LoadOrCreateKey() error = %v", err)
	}
	keyPath := filepath.Join(dataDir, SigningKeyFilename)
	info, err := os.Lstat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("key mode = %v, want regular file", info.Mode())
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("key permissions = %04o, want 0600", info.Mode().Perm())
	}
	encoded, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil || len(seed) != ed25519.SeedSize {
		t.Fatalf("stored seed = %q, decode error = %v", encoded, err)
	}

	second, secondKID, err := LoadOrCreateKey(cfg)
	if err != nil {
		t.Fatalf("second LoadOrCreateKey() error = %v", err)
	}
	if !bytes.Equal(first, second) || firstKID != secondKID {
		t.Fatal("reloading generated key returned different key material")
	}
	leftovers, err := filepath.Glob(filepath.Join(dataDir, ".signing.key-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary key files = %v, glob error = %v", leftovers, err)
	}
}

func TestLoadOrCreateKeyConcurrentFirstStart(t *testing.T) {
	const callers = 64
	cfg := config.StorageConfig{DataDir: filepath.Join(t.TempDir(), "data")}
	type result struct {
		key ed25519.PrivateKey
		kid string
		err error
	}
	results := make(chan result, callers)
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(callers)
	for range callers {
		go func() {
			ready.Done()
			<-start
			key, kid, err := LoadOrCreateKey(cfg)
			results <- result{key: key, kid: kid, err: err}
		}()
	}
	ready.Wait()
	close(start)

	var first result
	for i := 0; i < callers; i++ {
		got := <-results
		if got.err != nil {
			t.Fatalf("concurrent caller %d error = %v", i, got.err)
		}
		if i == 0 {
			first = got
			continue
		}
		if !bytes.Equal(got.key, first.key) || got.kid != first.kid {
			t.Fatalf("concurrent caller %d returned a different key", i)
		}
	}
	info, err := os.Stat(filepath.Join(cfg.DataDir, SigningKeyFilename))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("concurrent key permissions = %04o, want 0600", info.Mode().Perm())
	}
}

func TestPublishSeedDoesNotOverwriteExistingKey(t *testing.T) {
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, SigningKeyFilename)
	firstSeed := bytes.Repeat([]byte{0xb1}, ed25519.SeedSize)
	secondSeed := bytes.Repeat([]byte{0xb2}, ed25519.SeedSize)
	if err := os.WriteFile(path, []byte(hex.EncodeToString(firstSeed)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	created, err := publishSeed(path, secondSeed)
	if err != nil {
		t.Fatalf("publishSeed() error = %v", err)
	}
	if created {
		t.Fatal("publishSeed() reported overwriting an existing key")
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(stored)) != hex.EncodeToString(firstSeed) {
		t.Fatal("existing signing key was overwritten")
	}
}

func TestLoadOrCreateKeyRejectsUnsafeOrCorruptFiles(t *testing.T) {
	t.Run("corrupt", func(t *testing.T) {
		dataDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dataDir, SigningKeyFilename), []byte("not-a-key\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := LoadOrCreateKey(config.StorageConfig{DataDir: dataDir}); err == nil {
			t.Fatal("LoadOrCreateKey() accepted corrupt key file")
		}
	})

	if runtime.GOOS != "windows" {
		t.Run("permissive mode", func(t *testing.T) {
			dataDir := t.TempDir()
			seed := bytes.Repeat([]byte{0xc1}, ed25519.SeedSize)
			path := filepath.Join(dataDir, SigningKeyFilename)
			if err := os.WriteFile(path, []byte(hex.EncodeToString(seed)+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, _, err := LoadOrCreateKey(config.StorageConfig{DataDir: dataDir}); err == nil {
				t.Fatal("LoadOrCreateKey() accepted permissive key permissions")
			}
		})

		t.Run("symlink", func(t *testing.T) {
			dataDir := t.TempDir()
			seed := bytes.Repeat([]byte{0xc2}, ed25519.SeedSize)
			target := filepath.Join(dataDir, "target")
			if err := os.WriteFile(target, []byte(hex.EncodeToString(seed)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(dataDir, SigningKeyFilename)); err != nil {
				t.Fatal(err)
			}
			if _, _, err := LoadOrCreateKey(config.StorageConfig{DataDir: dataDir}); err == nil {
				t.Fatal("LoadOrCreateKey() accepted symlink key file")
			}
		})
	}
}

func TestLoadOrCreateKeyRequiresDataDir(t *testing.T) {
	if _, _, err := LoadOrCreateKey(config.StorageConfig{}); err == nil {
		t.Fatal("LoadOrCreateKey() accepted empty data directory without configured seed")
	}
}
