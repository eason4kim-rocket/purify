package receipts

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/use-agent/purify/config"
)

// SigningKeyFilename is the durable seed file created beneath DataDir when no
// explicit PURIFY_SIGNING_KEY seed is configured.
const SigningKeyFilename = "signing.key"

// LoadOrCreateKey loads the configured hex seed or atomically publishes a new
// 0600 seed file beneath cfg.DataDir. Concurrent callers and processes all
// converge on the first successfully published key.
func LoadOrCreateKey(cfg config.StorageConfig) (ed25519.PrivateKey, string, error) {
	if strings.TrimSpace(cfg.SigningKey) != "" {
		priv, err := privateKeyFromHex(cfg.SigningKey)
		if err != nil {
			return nil, "", fmt.Errorf("PURIFY_SIGNING_KEY: %w", err)
		}
		return keyAndID(priv)
	}

	if strings.TrimSpace(cfg.DataDir) == "" {
		return nil, "", errors.New("receipts: data directory is required when no signing seed is configured")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, "", fmt.Errorf("receipts: create data directory: %w", err)
	}
	keyPath := filepath.Join(cfg.DataDir, SigningKeyFilename)
	if priv, err := loadKeyFile(keyPath); err == nil {
		return keyAndID(priv)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, "", err
	}

	seed := make([]byte, ed25519.SeedSize)
	if _, err := io.ReadFull(rand.Reader, seed); err != nil {
		return nil, "", fmt.Errorf("receipts: generate signing seed: %w", err)
	}
	created, err := publishSeed(keyPath, seed)
	if err != nil {
		return nil, "", err
	}
	if created {
		priv := ed25519.NewKeyFromSeed(seed)
		priv, kid, err := keyAndID(priv)
		if err != nil {
			return nil, "", err
		}
		slog.Info("generated receipt signing key", "path", keyPath, "kid", kid)
		return priv, kid, nil
	}

	// Another caller won the atomic publish race. The final path only becomes
	// visible after its complete contents have been flushed and closed.
	priv, err := loadKeyFile(keyPath)
	if err != nil {
		return nil, "", err
	}
	return keyAndID(priv)
}

func keyAndID(priv ed25519.PrivateKey) (ed25519.PrivateKey, string, error) {
	pub, err := PublicKey(priv)
	if err != nil {
		return nil, "", err
	}
	kid, err := KeyID(pub)
	if err != nil {
		return nil, "", err
	}
	return priv, kid, nil
}

func privateKeyFromHex(encoded string) (ed25519.PrivateKey, error) {
	seed, err := hex.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("decode Ed25519 seed hex: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%w: seed length %d, want %d", ErrInvalidKey, len(seed), ed25519.SeedSize)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func loadKeyFile(path string) (ed25519.PrivateKey, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("receipts: signing key %q is not a regular file", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("receipts: signing key %q has permissions %04o, want 0600", path, info.Mode().Perm())
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("receipts: open signing key: %w", err)
	}
	defer file.Close()
	encoded, err := io.ReadAll(io.LimitReader(file, 1025))
	if err != nil {
		return nil, fmt.Errorf("receipts: read signing key: %w", err)
	}
	if len(encoded) > 1024 {
		return nil, errors.New("receipts: signing key file is too large")
	}
	priv, err := privateKeyFromHex(string(encoded))
	if err != nil {
		return nil, fmt.Errorf("receipts: parse signing key file: %w", err)
	}
	return priv, nil
}

// publishSeed writes a fully formed temporary file and atomically hard-links
// it into place. Link's no-replace semantics prevent concurrent first starts
// from ever overwriting an already published key.
func publishSeed(path string, seed []byte) (bool, error) {
	if len(seed) != ed25519.SeedSize {
		return false, fmt.Errorf("%w: seed length %d, want %d", ErrInvalidKey, len(seed), ed25519.SeedSize)
	}
	dir := filepath.Dir(path)
	temporary, err := os.CreateTemp(dir, ".signing.key-*")
	if err != nil {
		return false, fmt.Errorf("receipts: create temporary signing key: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	closed := false
	defer func() {
		if !closed {
			_ = temporary.Close()
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return false, fmt.Errorf("receipts: chmod temporary signing key: %w", err)
	}
	encoded := make([]byte, hex.EncodedLen(len(seed))+1)
	hex.Encode(encoded, seed)
	encoded[len(encoded)-1] = '\n'
	if _, err := io.Copy(temporary, bytes.NewReader(encoded)); err != nil {
		return false, fmt.Errorf("receipts: write temporary signing key: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return false, fmt.Errorf("receipts: sync temporary signing key: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return false, fmt.Errorf("receipts: close temporary signing key: %w", err)
	}
	closed = true

	if err := os.Link(temporaryPath, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, fmt.Errorf("receipts: atomically publish signing key: %w", err)
	}
	bestEffortSyncDir(dir)
	return true, nil
}

func bestEffortSyncDir(dir string) {
	directory, err := os.Open(dir)
	if err != nil {
		return
	}
	defer directory.Close()
	_ = directory.Sync()
}
