package runtimeverify

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testInventoryFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type testInventory struct {
	SchemaVersion string              `json:"schema_version"`
	Repository    string              `json:"repository"`
	Revision      string              `json:"revision"`
	Files         []testInventoryFile `json:"files"`
}

var testSnapshot = map[string][]byte{
	"config.json":           []byte(`{"model":"qwen"}`),
	"nested/tokenizer.json": []byte(`{"tokenizer":"locked"}`),
}

func TestVerifyHostSnapshotAcceptsExactMaterializedTree(t *testing.T) {
	root := writeHostSnapshot(t, testSnapshot)
	if err := VerifyHostSnapshot(root, inventoryFor(t, testSnapshot)); err != nil {
		t.Fatalf("VerifyHostSnapshot(exact) error = %v", err)
	}
}

func TestVerifyHostSnapshotRejectsSymlinksHardlinksAndSpecialPaths(t *testing.T) {
	inventory := inventoryFor(t, testSnapshot)

	t.Run("root symlink", func(t *testing.T) {
		realRoot := writeHostSnapshot(t, testSnapshot)
		linkedRoot := filepath.Join(t.TempDir(), "snapshot")
		if err := os.Symlink(realRoot, linkedRoot); err != nil {
			t.Fatal(err)
		}
		requireVerificationRejected(t, VerifyHostSnapshot(linkedRoot, inventory))
	})

	t.Run("file symlink", func(t *testing.T) {
		root := writeHostSnapshot(t, testSnapshot)
		target := filepath.Join(t.TempDir(), "outside")
		if err := os.WriteFile(target, testSnapshot["config.json"], 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(root, "config.json")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, "config.json")); err != nil {
			t.Fatal(err)
		}
		requireVerificationRejected(t, VerifyHostSnapshot(root, inventory))
	})

	t.Run("directory symlink", func(t *testing.T) {
		root := writeHostSnapshot(t, map[string][]byte{"config.json": testSnapshot["config.json"]})
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "tokenizer.json"), testSnapshot["nested/tokenizer.json"], 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "nested")); err != nil {
			t.Fatal(err)
		}
		requireVerificationRejected(t, VerifyHostSnapshot(root, inventory))
	})

	t.Run("expected file has another hardlink", func(t *testing.T) {
		root := writeHostSnapshot(t, testSnapshot)
		if err := os.Link(filepath.Join(root, "config.json"), filepath.Join(t.TempDir(), "other-link")); err != nil {
			t.Skipf("filesystem does not support hardlinks: %v", err)
		}
		requireVerificationRejected(t, VerifyHostSnapshot(root, inventory))
	})

	t.Run("root is regular file", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "snapshot")
		if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		requireVerificationRejected(t, VerifyHostSnapshot(root, inventory))
	})
}

func TestVerifyHostSnapshotRejectsMissingExtraSizeAndHashDrift(t *testing.T) {
	inventory := inventoryFor(t, testSnapshot)
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{name: "missing", mutate: func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, "config.json")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "extra file", mutate: func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, "extra.json"), []byte("extra"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "extra directory", mutate: func(t *testing.T, root string) {
			if err := os.Mkdir(filepath.Join(root, "empty-extra"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "size drift", mutate: func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, "config.json"), []byte("longer"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "hash drift", mutate: func(t *testing.T, root string) {
			original := testSnapshot["config.json"]
			drifted := bytes.Repeat([]byte{'x'}, len(original))
			if err := os.WriteFile(filepath.Join(root, "config.json"), drifted, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "directory where file expected", mutate: func(t *testing.T, root string) {
			if err := os.Remove(filepath.Join(root, "config.json")); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, "config.json"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := writeHostSnapshot(t, testSnapshot)
			test.mutate(t, root)
			requireVerificationRejected(t, VerifyHostSnapshot(root, inventory))
		})
	}
}

func TestVerifyContainerArchiveAcceptsExactRegularFileSet(t *testing.T) {
	inventory := inventoryFor(t, testSnapshot)
	archive := tarSnapshot(t, "revision", testSnapshot, nil)
	if err := VerifyContainerArchive(bytes.NewReader(archive), inventory, "/models/snapshots/revision"); err != nil {
		t.Fatalf("VerifyContainerArchive(exact) error = %v", err)
	}

	// Directory headers are structural and optional; the regular file set is
	// authoritative either way.
	archive = tarSnapshotWithoutDirectories(t, "revision", testSnapshot)
	if err := VerifyContainerArchive(bytes.NewReader(archive), inventory, "revision"); err != nil {
		t.Fatalf("VerifyContainerArchive(no directories) error = %v", err)
	}
}

func TestVerifyContainerArchiveRejectsUnsafeHeadersAndSetDrift(t *testing.T) {
	inventory := inventoryFor(t, testSnapshot)
	regular := func(name string, body []byte) tarEntry {
		return tarEntry{header: tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg}, body: body}
	}
	tests := []struct {
		name    string
		entries []tarEntry
	}{
		{name: "path traversal", entries: []tarEntry{regular("revision/../config.json", testSnapshot["config.json"])}},
		{name: "absolute path", entries: []tarEntry{regular("/revision/config.json", testSnapshot["config.json"])}},
		{name: "sibling root", entries: []tarEntry{regular("other/config.json", testSnapshot["config.json"])}},
		{name: "symlink", entries: []tarEntry{{header: tar.Header{Name: "revision/config.json", Typeflag: tar.TypeSymlink, Linkname: "target"}}}},
		{name: "hardlink", entries: []tarEntry{{header: tar.Header{Name: "revision/config.json", Typeflag: tar.TypeLink, Linkname: "revision/other"}}}},
		{name: "device", entries: []tarEntry{{header: tar.Header{Name: "revision/config.json", Typeflag: tar.TypeChar}}}},
		{name: "fifo", entries: []tarEntry{{header: tar.Header{Name: "revision/config.json", Typeflag: tar.TypeFifo}}}},
		{name: "duplicate", entries: []tarEntry{
			regular("revision/config.json", testSnapshot["config.json"]),
			regular("revision/config.json", testSnapshot["config.json"]),
		}},
		{name: "extra file", entries: []tarEntry{
			regular("revision/config.json", testSnapshot["config.json"]),
			regular("revision/nested/tokenizer.json", testSnapshot["nested/tokenizer.json"]),
			regular("revision/extra", []byte("extra")),
		}},
		{name: "extra directory", entries: []tarEntry{
			{header: tar.Header{Name: "revision/extra/", Typeflag: tar.TypeDir, Mode: 0o700}},
		}},
		{name: "missing", entries: []tarEntry{regular("revision/config.json", testSnapshot["config.json"])}},
		{name: "hash drift", entries: []tarEntry{
			regular("revision/config.json", bytes.Repeat([]byte{'x'}, len(testSnapshot["config.json"]))),
			regular("revision/nested/tokenizer.json", testSnapshot["nested/tokenizer.json"]),
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			archive := writeTar(t, test.entries)
			requireVerificationRejected(t, VerifyContainerArchive(bytes.NewReader(archive), inventory, "/models/snapshots/revision"))
		})
	}
}

func TestVerifyContainerArchiveRejectsDeclaredSizeAndTruncatedBody(t *testing.T) {
	inventory := inventoryFor(t, testSnapshot)

	wrongSize := writeTar(t, []tarEntry{{
		header: tar.Header{Name: "revision/config.json", Mode: 0o600, Size: int64(len(testSnapshot["config.json"]) + 1), Typeflag: tar.TypeReg},
		body:   append(append([]byte(nil), testSnapshot["config.json"]...), '!'),
	}})
	requireVerificationRejected(t, VerifyContainerArchive(bytes.NewReader(wrongSize), inventory, "revision"))

	valid := tarSnapshotWithoutDirectories(t, "revision", testSnapshot)
	truncated := valid[:len(valid)-700]
	requireVerificationRejected(t, VerifyContainerArchive(bytes.NewReader(truncated), inventory, "revision"))
}

func TestVerifyContainerArchiveRejectsUnsafeRootAndTrailingPayload(t *testing.T) {
	inventory := inventoryFor(t, testSnapshot)
	archive := tarSnapshotWithoutDirectories(t, "revision", testSnapshot)
	for _, root := range []string{"", "../revision", "/models/../revision", "revision/"} {
		requireVerificationRejected(t, VerifyContainerArchive(bytes.NewReader(archive), inventory, root))
	}

	withPayloadAfterEnd := append(append([]byte(nil), archive...), byte(1))
	requireVerificationRejected(t, VerifyContainerArchive(bytes.NewReader(withPayloadAfterEnd), inventory, "revision"))

	var typedNil *bytes.Reader
	requireVerificationRejected(t, VerifyContainerArchive(typedNil, inventory, "revision"))
}

func TestReferenceSnapshotInventoryFitsAdmissionLimits(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "assets", "qwen3-reranker-0.6b.snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := parseInventory(raw)
	if err != nil {
		t.Fatalf("parseInventory(reference snapshot) error = %v", err)
	}
	if len(spec.files) != 14 {
		t.Fatalf("reference snapshot files = %d, want 14", len(spec.files))
	}
	if spec.total <= 1<<30 || spec.total > maxSnapshotBytes {
		t.Fatalf("reference snapshot total = %d, want a bounded value above 1 GiB", spec.total)
	}
}

func TestInventoryAdmissionIsStrictAndBounded(t *testing.T) {
	valid := inventoryFor(t, testSnapshot)
	base := string(valid)
	tests := []struct {
		name string
		raw  []byte
	}{
		{name: "nil", raw: nil},
		{name: "BOM", raw: append([]byte{0xef, 0xbb, 0xbf}, valid...)},
		{name: "trailing", raw: append(append([]byte(nil), valid...), []byte(` {}`)...)},
		{name: "unknown root", raw: []byte(strings.Replace(base, `"files":`, `"unknown":true,"files":`, 1))},
		{name: "case smuggled root", raw: []byte(strings.Replace(base, `"revision":`, `"Revision":`, 1))},
		{name: "duplicate root", raw: []byte(strings.Replace(base, `"revision":`, `"revision":"0000000000000000000000000000000000000000","revision":`, 1))},
		{name: "unicode duplicate nested", raw: []byte(strings.Replace(base, `"path":"config.json"`, `"path":"config.json","\u0070ath":"other"`, 1))},
		{name: "null files", raw: []byte(`{"schema_version":"hf-snapshot-lock-v1","repository":"Qwen/test","revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","files":null}`)},
		{name: "bad version", raw: []byte(strings.Replace(base, "hf-snapshot-lock-v1", "hf-snapshot-lock-v2", 1))},
		{name: "traversal path", raw: inventoryWithPath(t, "../escape")},
		{name: "absolute path", raw: inventoryWithPath(t, "/escape")},
		{name: "backslash path", raw: inventoryWithPath(t, `nested\escape`)},
		{name: "noncanonical hash", raw: []byte(strings.Replace(base, testHash(testSnapshot["config.json"]), strings.ToUpper(testHash(testSnapshot["config.json"])), 1))},
		{name: "negative size", raw: []byte(strings.Replace(base, `"size":16`, `"size":-1`, 1))},
		{name: "inventory bytes", raw: bytes.Repeat([]byte{' '}, maxInventoryBytes+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := writeHostSnapshot(t, testSnapshot)
			requireVerificationRejected(t, VerifyHostSnapshot(root, test.raw))
			requireVerificationRejected(t, VerifyContainerArchive(bytes.NewReader(nil), test.raw, "revision"))
		})
	}
}

func TestInventoryRejectsDuplicatePathsAndResourceLimitOverflow(t *testing.T) {
	duplicate := testInventory{
		SchemaVersion: "hf-snapshot-lock-v1",
		Repository:    "Qwen/test",
		Revision:      strings.Repeat("a", 40),
		Files: []testInventoryFile{
			fileRecord("same", []byte("a")),
			fileRecord("same", []byte("a")),
		},
	}
	overTotal := testInventory{
		SchemaVersion: "hf-snapshot-lock-v1",
		Repository:    "Qwen/test",
		Revision:      strings.Repeat("a", 40),
		Files: []testInventoryFile{{
			Path: "model.safetensors", Size: maxSnapshotBytes + 1, SHA256: strings.Repeat("a", 64),
		}},
	}
	for name, inventory := range map[string]testInventory{"duplicate": duplicate, "total": overTotal} {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(inventory)
			if err != nil {
				t.Fatal(err)
			}
			requireVerificationRejected(t, VerifyHostSnapshot(t.TempDir(), raw))
		})
	}
}

func requireVerificationRejected(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrVerificationFailed) {
		t.Fatalf("verification error = %v, want ErrVerificationFailed", err)
	}
}

func inventoryFor(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	records := make([]testInventoryFile, 0, len(files))
	for _, name := range []string{"config.json", "nested/tokenizer.json"} {
		body, ok := files[name]
		if ok {
			records = append(records, fileRecord(name, body))
		}
	}
	for name, body := range files {
		if name != "config.json" && name != "nested/tokenizer.json" {
			records = append(records, fileRecord(name, body))
		}
	}
	encoded, err := json.Marshal(testInventory{
		SchemaVersion: "hf-snapshot-lock-v1",
		Repository:    "Qwen/Qwen3-Reranker-0.6B",
		Revision:      strings.Repeat("a", 40),
		Files:         records,
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func inventoryWithPath(t *testing.T, name string) []byte {
	t.Helper()
	encoded, err := json.Marshal(testInventory{
		SchemaVersion: "hf-snapshot-lock-v1",
		Repository:    "Qwen/test",
		Revision:      strings.Repeat("a", 40),
		Files:         []testInventoryFile{fileRecord(name, []byte("body"))},
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func fileRecord(name string, body []byte) testInventoryFile {
	return testInventoryFile{Path: name, Size: int64(len(body)), SHA256: testHash(body)}
}

func testHash(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func writeHostSnapshot(t *testing.T, files map[string][]byte) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "snapshot")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

type tarEntry struct {
	header tar.Header
	body   []byte
}

func tarSnapshot(t *testing.T, root string, files map[string][]byte, extra []tarEntry) []byte {
	t.Helper()
	entries := []tarEntry{
		{header: tar.Header{Name: root + "/", Typeflag: tar.TypeDir, Mode: 0o700}},
		{header: tar.Header{Name: root + "/nested/", Typeflag: tar.TypeDir, Mode: 0o700}},
	}
	for _, name := range []string{"config.json", "nested/tokenizer.json"} {
		body := files[name]
		entries = append(entries, tarEntry{
			header: tar.Header{Name: root + "/" + name, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(body))},
			body:   body,
		})
	}
	return writeTar(t, append(entries, extra...))
}

func tarSnapshotWithoutDirectories(t *testing.T, root string, files map[string][]byte) []byte {
	t.Helper()
	entries := make([]tarEntry, 0, len(files))
	for _, name := range []string{"config.json", "nested/tokenizer.json"} {
		body := files[name]
		entries = append(entries, tarEntry{
			header: tar.Header{Name: root + "/" + name, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(body))},
			body:   body,
		})
	}
	return writeTar(t, entries)
}

func writeTar(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	for _, entry := range entries {
		header := entry.header
		if err := writer.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if len(entry.body) > 0 {
			if _, err := writer.Write(entry.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

var _ io.Reader = (*bytes.Reader)(nil)
