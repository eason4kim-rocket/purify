// Package snapshot provides a content-addressed, compressed store for raw page
// captures. Content bytes are immutable; fetch observations are appended to a
// sidecar so identical HTML fetched at different URLs or times still occupies
// one blob without losing provenance.
package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

const idPrefix = "sha256:"

// ID is the stable SHA-256 identity of uncompressed snapshot bytes.
type ID string

// Meta records one observation of a content blob.
type Meta struct {
	URL         string    `json:"url"`
	FetchedAt   time.Time `json:"fetched_at"`
	Engine      string    `json:"engine"`
	StatusCode  int       `json:"status_code"`
	ContentType string    `json:"content_type"`
}

type sidecar struct {
	ID     ID     `json:"id"`
	SHA256 string `json:"sha256"`
	Meta
	Observations []Meta `json:"observations"`
}

// Store keeps zstd-compressed blobs beneath <dataDir>/snapshots.
type Store struct {
	root string
	mu   sync.RWMutex
	enc  *zstd.Encoder
	dec  *zstd.Decoder
}

// NewStore opens a content-addressed store rooted at dataDir. The snapshots
// directory is created with owner-only permissions.
func NewStore(dataDir string) (*Store, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("snapshot data directory is required")
	}
	root := filepath.Join(dataDir, "snapshots")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create snapshot root: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("secure snapshot root: %w", err)
	}

	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return nil, fmt.Errorf("create zstd encoder: %w", err)
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(64<<20))
	if err != nil {
		enc.Close()
		return nil, fmt.Errorf("create zstd decoder: %w", err)
	}
	return &Store{root: root, enc: enc, dec: dec}, nil
}

// Close releases compressor resources. It does not remove stored snapshots.
func (s *Store) Close() {
	if s == nil {
		return
	}
	s.enc.Close()
	s.dec.Close()
}

// Put atomically stores html and appends its fetch observation. Repeated
// content returns the same ID and never creates a second compressed blob.
func (s *Store) Put(html []byte, meta Meta) (ID, error) {
	if s == nil {
		return "", errors.New("snapshot store is nil")
	}
	sum := sha256.Sum256(html)
	hexDigest := hex.EncodeToString(sum[:])
	id := ID(idPrefix + hexDigest)
	dir, blobPath, metaPath, err := s.paths(id)
	if err != nil {
		return "", err
	}
	compressed := s.enc.EncodeAll(html, nil)

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create snapshot shard: %w", err)
	}
	if _, err := os.Stat(blobPath); errors.Is(err, fs.ErrNotExist) {
		if err := writeAtomic(blobPath, compressed, 0o600); err != nil {
			return "", fmt.Errorf("write snapshot blob: %w", err)
		}
	} else if err != nil {
		return "", fmt.Errorf("inspect snapshot blob: %w", err)
	}

	record := sidecar{ID: id, SHA256: hexDigest, Meta: meta, Observations: []Meta{meta}}
	if existing, err := os.ReadFile(metaPath); err == nil {
		if err := json.Unmarshal(existing, &record); err != nil {
			return "", fmt.Errorf("decode existing snapshot metadata: %w", err)
		}
		if record.ID != id || record.SHA256 != hexDigest {
			return "", errors.New("existing snapshot metadata identity mismatch")
		}
		record.Meta = meta
		record.Observations = append(record.Observations, meta)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("read snapshot metadata: %w", err)
	}

	encodedMeta, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode snapshot metadata: %w", err)
	}
	encodedMeta = append(encodedMeta, '\n')
	if err := writeAtomic(metaPath, encodedMeta, 0o600); err != nil {
		return "", fmt.Errorf("write snapshot metadata: %w", err)
	}
	return id, nil
}

// Get returns the original bytes and the most recent observation. It verifies
// the decompressed content hash so truncated or substituted blobs fail closed.
func (s *Store) Get(id ID) ([]byte, Meta, error) {
	if s == nil {
		return nil, Meta{}, errors.New("snapshot store is nil")
	}
	_, blobPath, metaPath, err := s.paths(id)
	if err != nil {
		return nil, Meta{}, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	compressed, err := os.ReadFile(blobPath)
	if err != nil {
		return nil, Meta{}, fmt.Errorf("read snapshot blob: %w", err)
	}
	html, err := s.dec.DecodeAll(compressed, nil)
	if err != nil {
		return nil, Meta{}, fmt.Errorf("decode snapshot blob: %w", err)
	}
	digest, _ := digestFromID(id)
	actual := sha256.Sum256(html)
	if !strings.EqualFold(hex.EncodeToString(actual[:]), digest) {
		return nil, Meta{}, errors.New("snapshot content hash mismatch")
	}

	encodedMeta, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, Meta{}, fmt.Errorf("read snapshot metadata: %w", err)
	}
	var record sidecar
	if err := json.Unmarshal(encodedMeta, &record); err != nil {
		return nil, Meta{}, fmt.Errorf("decode snapshot metadata: %w", err)
	}
	if record.ID != id || !strings.EqualFold(record.SHA256, digest) {
		return nil, Meta{}, errors.New("snapshot metadata identity mismatch")
	}
	return html, record.Meta, nil
}

// Has reports whether both the content blob and sidecar exist for id.
func (s *Store) Has(id ID) bool {
	if s == nil {
		return false
	}
	_, blobPath, metaPath, err := s.paths(id)
	if err != nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, err := os.Stat(blobPath); err != nil {
		return false
	}
	_, err = os.Stat(metaPath)
	return err == nil
}

// Observations returns all known fetch observations for a content ID. This is
// additive to the MASTERPLAN API and prevents CAS deduplication from erasing
// URL/time provenance needed by verification and future watch history.
func (s *Store) Observations(id ID) ([]Meta, error) {
	if s == nil {
		return nil, errors.New("snapshot store is nil")
	}
	_, _, metaPath, err := s.paths(id)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	encoded, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, fmt.Errorf("read snapshot metadata: %w", err)
	}
	var record sidecar
	if err := json.Unmarshal(encoded, &record); err != nil {
		return nil, fmt.Errorf("decode snapshot metadata: %w", err)
	}
	digest, _ := digestFromID(id)
	if record.ID != id || !strings.EqualFold(record.SHA256, digest) {
		return nil, errors.New("snapshot metadata identity mismatch")
	}
	return append([]Meta(nil), record.Observations...), nil
}

func (s *Store) paths(id ID) (dir, blobPath, metaPath string, err error) {
	digest, err := digestFromID(id)
	if err != nil {
		return "", "", "", err
	}
	dir = filepath.Join(s.root, digest[:2], digest[2:4])
	blobPath = filepath.Join(dir, digest+".html.zst")
	metaPath = filepath.Join(dir, digest+".json")
	return dir, blobPath, metaPath, nil
}

func digestFromID(id ID) (string, error) {
	raw := string(id)
	if !strings.HasPrefix(raw, idPrefix) {
		return "", errors.New("snapshot ID must start with sha256:")
	}
	digest := strings.TrimPrefix(raw, idPrefix)
	if len(digest) != sha256.Size*2 {
		return "", errors.New("snapshot ID must contain a 64-character SHA-256 digest")
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != sha256.Size {
		return "", errors.New("snapshot ID contains invalid SHA-256 hex")
	}
	return strings.ToLower(digest), nil
}

func writeAtomic(path string, data []byte, mode fs.FileMode) (retErr error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".snapshot-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	if dirHandle, err := os.Open(dir); err == nil {
		_ = dirHandle.Sync()
		_ = dirHandle.Close()
	}
	return nil
}
