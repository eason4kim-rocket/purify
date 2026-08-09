// Package snapshot provides a content-addressed, compressed store for raw page
// captures. Content bytes are immutable; fetch observations live in a compact
// identity sidecar plus an append-only log, so identical HTML fetched at
// different URLs or times still occupies one blob without losing provenance.
package snapshot

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

const (
	idPrefix                  = "sha256:"
	maximumStoredContentBytes = 64 << 20
)

var (
	// ErrContentTooLarge reports that decompression crossed the caller's
	// explicit output limit. No oversized content bytes are returned.
	ErrContentTooLarge = errors.New("snapshot content exceeds maximum bytes")
	// ErrInvalidContentLimit reports a non-positive or excessively broad bound.
	ErrInvalidContentLimit = errors.New("snapshot content limit is invalid")
)

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
	Observations []Meta `json:"observations,omitempty"`
}

type scannedSidecar struct {
	ID               ID
	SHA256           string
	Meta             Meta
	ObservationCount int
}

type observationVisitor func(Meta) (bool, error)

// Store keeps zstd-compressed blobs beneath <dataDir>/snapshots.
type Store struct {
	root string
	mu   sync.RWMutex
	enc  *zstd.Encoder
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
	return &Store{root: root, enc: enc}, nil
}

// Close releases compressor resources. It does not remove stored snapshots.
func (s *Store) Close() {
	if s == nil {
		return
	}
	s.enc.Close()
}

// Put atomically stores html and appends its fetch observation. Repeated
// content returns the same ID and never creates a second compressed blob.
// Observation history is kept in an append-only JSON-lines log so adding one
// observation does not read, allocate, and rewrite the complete history.
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
	observationsPath := observationLogPath(metaPath)
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

	if err := ensureObservationLog(metaPath, observationsPath, id, hexDigest); err != nil {
		return "", err
	}
	if err := appendObservation(observationsPath, meta); err != nil {
		return "", fmt.Errorf("append snapshot observation: %w", err)
	}

	record := sidecar{ID: id, SHA256: hexDigest, Meta: meta}
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

// Content returns at most maximumBytes immutable bytes for id without opening
// or materializing its metadata sidecar. Decompression is streamed and stops at
// maximumBytes+1, preventing a high-ratio blob from allocating its full output.
// A successful result has also passed the content-address hash check.
func (s *Store) Content(id ID, maximumBytes int) ([]byte, error) {
	if s == nil {
		return nil, errors.New("snapshot store is nil")
	}
	if maximumBytes < 1 || maximumBytes > maximumStoredContentBytes {
		return nil, fmt.Errorf("%w: must be between 1 and %d", ErrInvalidContentLimit, maximumStoredContentBytes)
	}
	_, blobPath, _, err := s.paths(id)
	if err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.content(id, blobPath, maximumBytes)
}

// Get returns the original bytes and the most recent observation. It preserves
// the original API while reading the sidecar as a token stream, so legacy
// observation arrays are not materialized merely to obtain the latest Meta.
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

	html, err := s.content(id, blobPath, maximumStoredContentBytes)
	if err != nil {
		return nil, Meta{}, err
	}
	digest, _ := digestFromID(id)
	record, _, err := scanSidecar(metaPath, id, digest, nil)
	if err != nil {
		return nil, Meta{}, err
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
	observationsPath := observationLogPath(metaPath)
	s.mu.RLock()
	defer s.mu.RUnlock()
	digest, _ := digestFromID(id)
	if _, err := os.Stat(observationsPath); err == nil {
		if _, _, err := scanSidecar(metaPath, id, digest, nil); err != nil {
			return nil, err
		}
		observations := make([]Meta, 0)
		_, err := visitObservationLog(observationsPath, func(meta Meta) (bool, error) {
			observations = append(observations, meta)
			return false, nil
		})
		if err != nil {
			return nil, err
		}
		return observations, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("inspect snapshot observations: %w", err)
	}

	observations := make([]Meta, 0)
	_, _, err = scanSidecar(metaPath, id, digest, func(meta Meta) (bool, error) {
		observations = append(observations, meta)
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return observations, nil
}

// HasObservation streams observations for id until match returns true. It
// validates the sidecar identity before reading the append-only log and never
// materializes the complete history. The callback runs synchronously while the
// Store read lock is held; it should be fast and must not re-enter this Store.
func (s *Store) HasObservation(id ID, match func(Meta) bool) (bool, error) {
	if s == nil {
		return false, errors.New("snapshot store is nil")
	}
	if match == nil {
		return false, errors.New("snapshot observation matcher is nil")
	}
	_, _, metaPath, err := s.paths(id)
	if err != nil {
		return false, err
	}
	observationsPath := observationLogPath(metaPath)

	s.mu.RLock()
	defer s.mu.RUnlock()
	digest, _ := digestFromID(id)
	if _, err := os.Stat(observationsPath); err == nil {
		if _, _, err := scanSidecar(metaPath, id, digest, nil); err != nil {
			return false, err
		}
		return visitObservationLog(observationsPath, func(meta Meta) (bool, error) {
			return match(meta), nil
		})
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("inspect snapshot observations: %w", err)
	}

	_, found, err := scanSidecar(metaPath, id, digest, func(meta Meta) (bool, error) {
		return match(meta), nil
	})
	return found, err
}

func (s *Store) content(id ID, blobPath string, maximumBytes int) ([]byte, error) {
	compressed, err := os.Open(blobPath)
	if err != nil {
		return nil, fmt.Errorf("read snapshot blob: %w", err)
	}
	defer compressed.Close()
	decoderMemoryLimit := uint64(maximumBytes) + 1
	if decoderMemoryLimit < zstd.MinWindowSize {
		decoderMemoryLimit = zstd.MinWindowSize
	}
	decoder, err := zstd.NewReader(
		compressed,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxMemory(decoderMemoryLimit),
		zstd.WithDecoderMaxWindow(decoderMemoryLimit),
	)
	if err != nil {
		return nil, fmt.Errorf("open snapshot blob decoder: %w", err)
	}
	defer decoder.Close()
	html, err := io.ReadAll(io.LimitReader(decoder, int64(maximumBytes)+1))
	if err != nil {
		if errors.Is(err, zstd.ErrDecoderSizeExceeded) || errors.Is(err, zstd.ErrWindowSizeExceeded) {
			return nil, fmt.Errorf("%w: limit %d: %v", ErrContentTooLarge, maximumBytes, err)
		}
		return nil, fmt.Errorf("decode snapshot blob: %w", err)
	}
	if len(html) > maximumBytes {
		return nil, fmt.Errorf("%w: limit %d", ErrContentTooLarge, maximumBytes)
	}
	digest, _ := digestFromID(id)
	actual := sha256.Sum256(html)
	if !strings.EqualFold(hex.EncodeToString(actual[:]), digest) {
		return nil, errors.New("snapshot content hash mismatch")
	}
	return html, nil
}

func observationLogPath(metaPath string) string {
	return strings.TrimSuffix(metaPath, filepath.Ext(metaPath)) + ".observations.jsonl"
}

// ensureObservationLog migrates a legacy sidecar exactly once. The log is
// atomically installed before the compact sidecar is written, so a crash leaves
// either the untouched legacy array or a complete authoritative log.
func ensureObservationLog(metaPath, observationsPath string, id ID, digest string) error {
	if _, err := os.Stat(observationsPath); err == nil {
		if _, metaErr := os.Stat(metaPath); metaErr == nil {
			if _, _, scanErr := scanSidecar(metaPath, id, digest, nil); scanErr != nil {
				return scanErr
			} else {
				return nil
			}
		} else if errors.Is(metaErr, fs.ErrNotExist) {
			return nil
		} else {
			return fmt.Errorf("inspect snapshot metadata: %w", metaErr)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inspect snapshot observations: %w", err)
	}

	if _, err := os.Stat(metaPath); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect snapshot metadata: %w", err)
	}
	if err := migrateLegacyObservations(metaPath, observationsPath, id, digest); err != nil {
		return fmt.Errorf("migrate snapshot observations: %w", err)
	}
	return nil
}

func migrateLegacyObservations(metaPath, observationsPath string, id ID, digest string) error {
	dir := filepath.Dir(observationsPath)
	tmp, err := os.CreateTemp(dir, ".snapshot-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	encoder := json.NewEncoder(tmp)
	record, _, err := scanSidecar(metaPath, id, digest, func(meta Meta) (bool, error) {
		return false, encoder.Encode(meta)
	})
	if err != nil {
		return err
	}
	if record.ObservationCount == 0 && record.Meta != (Meta{}) {
		if err := encoder.Encode(record.Meta); err != nil {
			return err
		}
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, observationsPath); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func appendObservation(path string, meta Meta) (retErr error) {
	encoded, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err := file.Close(); retErr == nil {
			retErr = err
		}
	}()
	completeSize, actualSize, err := completeObservationLogSize(file)
	if err != nil {
		return err
	}
	if completeSize != actualSize {
		if err := file.Truncate(completeSize); err != nil {
			return err
		}
		if err := file.Sync(); err != nil {
			return err
		}
	}
	written, err := file.Write(encoded)
	if err != nil {
		return err
	}
	if written != len(encoded) {
		return io.ErrShortWrite
	}
	return file.Sync()
}

func visitObservationLog(path string, visit observationVisitor) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("read snapshot observations: %w", err)
	}
	defer file.Close()
	completeSize, _, err := completeObservationLogSize(file)
	if err != nil {
		return false, fmt.Errorf("inspect snapshot observations: %w", err)
	}
	decoder := json.NewDecoder(io.LimitReader(file, completeSize))
	for {
		var meta Meta
		if err := decoder.Decode(&meta); errors.Is(err, io.EOF) {
			return false, nil
		} else if err != nil {
			return false, fmt.Errorf("decode snapshot observations: %w", err)
		}
		stop, err := visit(meta)
		if err != nil {
			return false, err
		}
		if stop {
			return true, nil
		}
	}
}

// completeObservationLogSize returns the prefix ending at the last durable
// newline-delimited record. A torn final append is uncommitted and is ignored by
// readers, then truncated by the next writer without scanning older history.
func completeObservationLogSize(file *os.File) (complete, actual int64, err error) {
	info, err := file.Stat()
	if err != nil {
		return 0, 0, err
	}
	actual = info.Size()
	if actual == 0 {
		return 0, 0, nil
	}
	var last [1]byte
	if _, err := file.ReadAt(last[:], actual-1); err != nil {
		return 0, actual, err
	}
	if last[0] == '\n' {
		return actual, actual, nil
	}

	const searchBlockBytes = 32 << 10
	buffer := make([]byte, searchBlockBytes)
	for end := actual; end > 0; {
		start := end - int64(len(buffer))
		if start < 0 {
			start = 0
		}
		length := int(end - start)
		n, readErr := file.ReadAt(buffer[:length], start)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return 0, actual, readErr
		}
		if index := bytes.LastIndexByte(buffer[:n], '\n'); index >= 0 {
			return start + int64(index) + 1, actual, nil
		}
		end = start
	}
	return 0, actual, nil
}

func scanSidecar(path string, expectedID ID, expectedDigest string, visit observationVisitor) (scannedSidecar, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return scannedSidecar{}, false, fmt.Errorf("read snapshot metadata: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	opening, err := decoder.Token()
	if err != nil {
		return scannedSidecar{}, false, fmt.Errorf("decode snapshot metadata: %w", err)
	}
	if opening != json.Delim('{') {
		return scannedSidecar{}, false, errors.New("decode snapshot metadata: top-level value must be an object")
	}

	var record scannedSidecar
	found := false
	seenID := false
	seenDigest := false
	for decoder.More() {
		rawKey, err := decoder.Token()
		if err != nil {
			return scannedSidecar{}, false, fmt.Errorf("decode snapshot metadata: %w", err)
		}
		key, ok := rawKey.(string)
		if !ok {
			return scannedSidecar{}, false, errors.New("decode snapshot metadata: object key is not a string")
		}
		switch key {
		case "id":
			if seenID {
				return scannedSidecar{}, false, errors.New("decode snapshot metadata: duplicate id")
			}
			seenID = true
			if err := decoder.Decode(&record.ID); err != nil {
				return scannedSidecar{}, false, fmt.Errorf("decode snapshot metadata id: %w", err)
			}
		case "sha256":
			if seenDigest {
				return scannedSidecar{}, false, errors.New("decode snapshot metadata: duplicate sha256")
			}
			seenDigest = true
			if err := decoder.Decode(&record.SHA256); err != nil {
				return scannedSidecar{}, false, fmt.Errorf("decode snapshot metadata sha256: %w", err)
			}
		case "url":
			if err := decoder.Decode(&record.Meta.URL); err != nil {
				return scannedSidecar{}, false, fmt.Errorf("decode snapshot metadata URL: %w", err)
			}
		case "fetched_at":
			if err := decoder.Decode(&record.Meta.FetchedAt); err != nil {
				return scannedSidecar{}, false, fmt.Errorf("decode snapshot metadata fetched_at: %w", err)
			}
		case "engine":
			if err := decoder.Decode(&record.Meta.Engine); err != nil {
				return scannedSidecar{}, false, fmt.Errorf("decode snapshot metadata engine: %w", err)
			}
		case "status_code":
			if err := decoder.Decode(&record.Meta.StatusCode); err != nil {
				return scannedSidecar{}, false, fmt.Errorf("decode snapshot metadata status_code: %w", err)
			}
		case "content_type":
			if err := decoder.Decode(&record.Meta.ContentType); err != nil {
				return scannedSidecar{}, false, fmt.Errorf("decode snapshot metadata content_type: %w", err)
			}
		case "observations":
			arrayOpening, err := decoder.Token()
			if err != nil {
				return scannedSidecar{}, false, fmt.Errorf("decode snapshot observations: %w", err)
			}
			if arrayOpening != json.Delim('[') {
				return scannedSidecar{}, false, errors.New("decode snapshot observations: value must be an array")
			}
			for decoder.More() {
				record.ObservationCount++
				var meta Meta
				if err := decoder.Decode(&meta); err != nil {
					return scannedSidecar{}, false, fmt.Errorf("decode snapshot observations: %w", err)
				}
				if visit == nil || found {
					continue
				}
				stop, err := visit(meta)
				if err != nil {
					return scannedSidecar{}, false, err
				}
				found = stop
			}
			arrayClosing, err := decoder.Token()
			if err != nil {
				return scannedSidecar{}, false, fmt.Errorf("decode snapshot observations: %w", err)
			}
			if arrayClosing != json.Delim(']') {
				return scannedSidecar{}, false, errors.New("decode snapshot observations: unterminated array")
			}
		default:
			if err := skipJSONValue(decoder); err != nil {
				return scannedSidecar{}, false, fmt.Errorf("decode snapshot metadata field %q: %w", key, err)
			}
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return scannedSidecar{}, false, fmt.Errorf("decode snapshot metadata: %w", err)
	}
	if closing != json.Delim('}') {
		return scannedSidecar{}, false, errors.New("decode snapshot metadata: unterminated object")
	}
	if trailing, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return scannedSidecar{}, false, fmt.Errorf("decode snapshot metadata: %w", err)
		}
		return scannedSidecar{}, false, fmt.Errorf("decode snapshot metadata: unexpected trailing token %v", trailing)
	}
	if record.ID != expectedID || !strings.EqualFold(record.SHA256, expectedDigest) {
		return scannedSidecar{}, false, errors.New("snapshot metadata identity mismatch")
	}
	return record, found, nil
}

func skipJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok || delimiter != '{' && delimiter != '[' {
		return nil
	}
	depth := 1
	for depth > 0 {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			continue
		}
		switch delimiter {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		}
	}
	return nil
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
	return syncDirectory(dir)
}

func syncDirectory(dir string) error {
	dirHandle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer dirHandle.Close()
	return dirHandle.Sync()
}
