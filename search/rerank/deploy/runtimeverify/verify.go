// Package runtimeverify verifies materialized reranker artifacts before they
// are admitted to the host or a container runtime.
package runtimeverify

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	inventorySchemaVersion = "hf-snapshot-lock-v1"

	maxInventoryBytes = 256 << 10
	maxInventoryFiles = 4096
	maxInventoryDirs  = 8192
	maxInventoryDepth = 8
	maxInventoryNodes = 1 + 4 + maxInventoryFiles*4

	maxInventoryPathBytes = 4 << 10
	maxPathComponents     = 64
	maxRepositoryBytes    = 256

	maxSnapshotFileBytes = int64(8) << 30
	maxSnapshotBytes     = int64(16) << 30
	maxArchiveOverhead   = int64(64) << 20
	directoryReadBatch   = 64
)

// ErrVerificationFailed is returned for every admission failure. Callers can
// use errors.Is without receiving host paths or archive details in the error.
var ErrVerificationFailed = errors.New("reranker runtime artifact verification failed")

type inventoryFile struct {
	path   string
	size   int64
	digest [sha256.Size]byte
}

type inventorySpec struct {
	files    map[string]inventoryFile
	dirs     map[string]struct{}
	children map[string]map[string]expectedEntry
	total    int64
}

type expectedEntry struct {
	directory bool
	file      inventoryFile
}

// VerifyHostSnapshot verifies that root is an exact, materialized copy of the
// regular-file set described by inventory. Symbolic links, hard-linked files,
// special files, missing entries, and extra entries are rejected.
func VerifyHostSnapshot(root string, inventory []byte) error {
	spec, err := parseInventory(inventory)
	if err != nil || !validHostRoot(root) {
		return ErrVerificationFailed
	}
	if err := verifyHostDirectory(filepath.Clean(root), "", spec); err != nil {
		return ErrVerificationFailed
	}
	return nil
}

// VerifyContainerArchive verifies an uncompressed tar stream returned for a
// container path. expectedRootBase may be the complete requested container
// path or its basename; archive members must be rooted at that basename.
func VerifyContainerArchive(reader io.Reader, inventory []byte, expectedRootBase string) error {
	spec, err := parseInventory(inventory)
	if err != nil || nilReader(reader) {
		return ErrVerificationFailed
	}
	archiveRoot, ok := archiveRootName(expectedRootBase)
	if !ok {
		return ErrVerificationFailed
	}
	if err := verifyArchive(reader, archiveRoot, spec); err != nil {
		return ErrVerificationFailed
	}
	return nil
}

func parseInventory(raw []byte) (*inventorySpec, error) {
	if len(raw) == 0 || len(raw) > maxInventoryBytes || !utf8.Valid(raw) || bytes.HasPrefix(raw, []byte{0xef, 0xbb, 0xbf}) {
		return nil, ErrVerificationFailed
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	budget := jsonBudget{}
	if err := consumeJSONValue(decoder, 1, &budget); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, ErrVerificationFailed
	}

	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil || !exactFields(root, "schema_version", "repository", "revision", "files") {
		return nil, ErrVerificationFailed
	}

	var schemaVersion, repository, revision string
	if decodeRequired(root["schema_version"], &schemaVersion) != nil || schemaVersion != inventorySchemaVersion {
		return nil, ErrVerificationFailed
	}
	if decodeRequired(root["repository"], &repository) != nil || !validRepository(repository) {
		return nil, ErrVerificationFailed
	}
	if decodeRequired(root["revision"], &revision) != nil || !lowerHex(revision, 40) {
		return nil, ErrVerificationFailed
	}
	if isJSONNull(root["files"]) {
		return nil, ErrVerificationFailed
	}

	var fileObjects []json.RawMessage
	if err := json.Unmarshal(root["files"], &fileObjects); err != nil || len(fileObjects) == 0 || len(fileObjects) > maxInventoryFiles {
		return nil, ErrVerificationFailed
	}

	spec := &inventorySpec{
		files: make(map[string]inventoryFile, len(fileObjects)),
		dirs:  map[string]struct{}{"": {}},
	}
	for _, encodedFile := range fileObjects {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encodedFile, &fields); err != nil || !exactFields(fields, "path", "size", "sha256") {
			return nil, ErrVerificationFailed
		}

		var name, digestText string
		if decodeRequired(fields["path"], &name) != nil || !validRelativePath(name) {
			return nil, ErrVerificationFailed
		}
		if _, exists := spec.files[name]; exists {
			return nil, ErrVerificationFailed
		}
		size, err := decodeCanonicalSize(fields["size"])
		if err != nil || size > maxSnapshotFileBytes || spec.total > maxSnapshotBytes-size {
			return nil, ErrVerificationFailed
		}
		if decodeRequired(fields["sha256"], &digestText) != nil || !lowerHex(digestText, sha256.Size*2) {
			return nil, ErrVerificationFailed
		}
		digestBytes, err := hex.DecodeString(digestText)
		if err != nil || len(digestBytes) != sha256.Size {
			return nil, ErrVerificationFailed
		}
		var digest [sha256.Size]byte
		copy(digest[:], digestBytes)
		spec.files[name] = inventoryFile{path: name, size: size, digest: digest}
		spec.total += size

		parts := strings.Split(name, "/")
		current := ""
		for _, component := range parts[:len(parts)-1] {
			if current == "" {
				current = component
			} else {
				current += "/" + component
			}
			spec.dirs[current] = struct{}{}
			if len(spec.dirs) > maxInventoryDirs {
				return nil, ErrVerificationFailed
			}
		}
	}

	for directory := range spec.dirs {
		if _, collision := spec.files[directory]; collision {
			return nil, ErrVerificationFailed
		}
	}
	spec.children = make(map[string]map[string]expectedEntry, len(spec.dirs))
	for directory := range spec.dirs {
		spec.children[directory] = make(map[string]expectedEntry)
	}
	for directory := range spec.dirs {
		if directory == "" {
			continue
		}
		parent, base := splitRelative(directory)
		if !addExpected(spec.children[parent], base, expectedEntry{directory: true}) {
			return nil, ErrVerificationFailed
		}
	}
	for name, file := range spec.files {
		parent, base := splitRelative(name)
		if !addExpected(spec.children[parent], base, expectedEntry{file: file}) {
			return nil, ErrVerificationFailed
		}
	}
	return spec, nil
}

type jsonBudget struct {
	nodes int
}

func consumeJSONValue(decoder *json.Decoder, depth int, budget *jsonBudget) error {
	if depth > maxInventoryDepth {
		return ErrVerificationFailed
	}
	token, err := decoder.Token()
	if err != nil {
		return ErrVerificationFailed
	}
	budget.nodes++
	if budget.nodes > maxInventoryNodes {
		return ErrVerificationFailed
	}
	delim, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return ErrVerificationFailed
			}
			key, ok := keyToken.(string)
			if !ok || invalidText(key, maxInventoryPathBytes) {
				return ErrVerificationFailed
			}
			if _, duplicate := seen[key]; duplicate {
				return ErrVerificationFailed
			}
			seen[key] = struct{}{}
			if len(seen) > maxInventoryFiles+4 || consumeJSONValue(decoder, depth+1, budget) != nil {
				return ErrVerificationFailed
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return ErrVerificationFailed
		}
	case '[':
		items := 0
		for decoder.More() {
			items++
			if items > maxInventoryFiles || consumeJSONValue(decoder, depth+1, budget) != nil {
				return ErrVerificationFailed
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return ErrVerificationFailed
		}
	default:
		return ErrVerificationFailed
	}
	return nil
}

func exactFields(fields map[string]json.RawMessage, names ...string) bool {
	if fields == nil || len(fields) != len(names) {
		return false
	}
	for _, name := range names {
		value, exists := fields[name]
		if !exists || len(value) == 0 {
			return false
		}
	}
	return true
}

func decodeRequired[T any](raw json.RawMessage, target *T) error {
	if len(raw) == 0 || isJSONNull(raw) {
		return ErrVerificationFailed
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return ErrVerificationFailed
	}
	return nil
}

func decodeCanonicalSize(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 || raw[0] < '0' || raw[0] > '9' {
		return 0, ErrVerificationFailed
	}
	if len(raw) > 1 && raw[0] == '0' {
		return 0, ErrVerificationFailed
	}
	for _, character := range raw {
		if character < '0' || character > '9' {
			return 0, ErrVerificationFailed
		}
	}
	value, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || value < 0 {
		return 0, ErrVerificationFailed
	}
	return value, nil
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func validRepository(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && !invalidText(value, maxRepositoryBytes)
}

func lowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, character := range []byte(value) {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func validRelativePath(value string) bool {
	if value == "" || invalidText(value, maxInventoryPathBytes) || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") || path.Clean(value) != value {
		return false
	}
	parts := strings.Split(value, "/")
	if len(parts) == 0 || len(parts) > maxPathComponents {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func invalidText(value string, byteLimit int) bool {
	if len(value) == 0 || len(value) > byteLimit || !utf8.ValidString(value) || strings.ContainsRune(value, utf8.RuneError) {
		return true
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}

func splitRelative(value string) (string, string) {
	index := strings.LastIndexByte(value, '/')
	if index < 0 {
		return "", value
	}
	return value[:index], value[index+1:]
}

func addExpected(children map[string]expectedEntry, name string, entry expectedEntry) bool {
	if children == nil {
		return false
	}
	if _, exists := children[name]; exists {
		return false
	}
	children[name] = entry
	return true
}

func validHostRoot(root string) bool {
	if root == "" || invalidText(root, maxInventoryPathBytes) || strings.IndexByte(root, 0) >= 0 {
		return false
	}
	cleaned := filepath.Clean(root)
	return cleaned == root && cleaned != "." && cleaned != string(filepath.Separator)
}

func verifyHostDirectory(fullPath, relativePath string, spec *inventorySpec) error {
	before, err := os.Lstat(fullPath)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return ErrVerificationFailed
	}

	directory, err := os.Open(fullPath)
	if err != nil {
		return ErrVerificationFailed
	}
	defer directory.Close()
	opened, err := directory.Stat()
	if err != nil || !opened.IsDir() || !os.SameFile(before, opened) {
		return ErrVerificationFailed
	}

	expected := spec.children[relativePath]
	if expected == nil {
		return ErrVerificationFailed
	}
	seen := make(map[string]struct{}, len(expected))
	for {
		entries, readErr := directory.ReadDir(directoryReadBatch)
		for _, entry := range entries {
			name := entry.Name()
			expectation, exists := expected[name]
			if !exists {
				return ErrVerificationFailed
			}
			if _, duplicate := seen[name]; duplicate {
				return ErrVerificationFailed
			}
			seen[name] = struct{}{}
			childFull := filepath.Join(fullPath, name)
			childRelative := name
			if relativePath != "" {
				childRelative = relativePath + "/" + name
			}
			if expectation.directory {
				if err := verifyHostDirectory(childFull, childRelative, spec); err != nil {
					return err
				}
				continue
			}
			if err := verifyHostFile(childFull, expectation.file); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return ErrVerificationFailed
		}
	}
	if len(seen) != len(expected) {
		return ErrVerificationFailed
	}
	after, err := os.Lstat(fullPath)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !after.IsDir() || !os.SameFile(before, after) {
		return ErrVerificationFailed
	}
	return nil
}

func verifyHostFile(fullPath string, expected inventoryFile) error {
	before, err := os.Lstat(fullPath)
	if err != nil || !before.Mode().IsRegular() || before.Size() != expected.size || !singleLink(before) {
		return ErrVerificationFailed
	}

	file, err := os.Open(fullPath)
	if err != nil {
		return ErrVerificationFailed
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || opened.Size() != expected.size || !singleLink(opened) || !os.SameFile(before, opened) {
		return ErrVerificationFailed
	}

	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, expected.size+1))
	if err != nil || written != expected.size || !bytes.Equal(hash.Sum(nil), expected.digest[:]) {
		return ErrVerificationFailed
	}
	openedAfter, err := file.Stat()
	if err != nil || !openedAfter.Mode().IsRegular() || openedAfter.Size() != expected.size || !singleLink(openedAfter) || !os.SameFile(opened, openedAfter) {
		return ErrVerificationFailed
	}
	pathAfter, err := os.Lstat(fullPath)
	if err != nil || !pathAfter.Mode().IsRegular() || pathAfter.Size() != expected.size || !singleLink(pathAfter) || !os.SameFile(opened, pathAfter) {
		return ErrVerificationFailed
	}
	return nil
}

func singleLink(info fs.FileInfo) bool {
	value := reflect.ValueOf(info.Sys())
	if !value.IsValid() {
		return false
	}
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return false
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return false
	}
	links := value.FieldByName("Nlink")
	if !links.IsValid() {
		return false
	}
	switch links.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return links.Uint() == 1
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return links.Int() == 1
	default:
		return false
	}
}

func archiveRootName(expected string) (string, bool) {
	if expected == "" || invalidText(expected, maxInventoryPathBytes) || strings.Contains(expected, "\\") || path.Clean(expected) != expected || expected == "/" || expected == "." || expected == ".." {
		return "", false
	}
	components := strings.Split(strings.TrimPrefix(expected, "/"), "/")
	if len(components) == 0 || len(components) > maxPathComponents {
		return "", false
	}
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			return "", false
		}
	}
	base := path.Base(expected)
	if base == "" || base == "." || base == ".." || strings.Contains(base, "/") || invalidText(base, maxInventoryPathBytes) {
		return "", false
	}
	return base, true
}

func verifyArchive(reader io.Reader, archiveRoot string, spec *inventorySpec) error {
	budget := spec.total + maxArchiveOverhead
	if budget < spec.total || budget >= int64(^uint64(0)>>1) {
		return ErrVerificationFailed
	}
	source := &progressReader{reader: reader}
	limited := &io.LimitedReader{R: source, N: budget + 1}
	archive := tar.NewReader(limited)
	seenHeaders := make(map[string]struct{}, len(spec.files)+len(spec.dirs))
	seenFiles := make(map[string]struct{}, len(spec.files))

	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || limited.N == 0 || header == nil || header.Linkname != "" || len(header.Xattrs) != 0 || len(header.PAXRecords) != 0 {
			return ErrVerificationFailed
		}
		if len(seenHeaders) >= len(spec.files)+len(spec.dirs) {
			return ErrVerificationFailed
		}
		relative, ok := archiveRelativeName(header.Name, archiveRoot, header.Typeflag == tar.TypeDir)
		if !ok {
			return ErrVerificationFailed
		}
		key := string([]byte{header.Typeflag}) + ":" + relative
		if header.Typeflag == tar.TypeRegA {
			key = string([]byte{tar.TypeReg}) + ":" + relative
		}
		if _, duplicate := seenHeaders[key]; duplicate {
			return ErrVerificationFailed
		}
		seenHeaders[key] = struct{}{}

		switch header.Typeflag {
		case tar.TypeDir:
			if header.Size != 0 {
				return ErrVerificationFailed
			}
			if _, expected := spec.dirs[relative]; !expected {
				return ErrVerificationFailed
			}
		case tar.TypeReg, tar.TypeRegA:
			if relative == "" {
				return ErrVerificationFailed
			}
			expected, exists := spec.files[relative]
			if !exists || header.Size != expected.size {
				return ErrVerificationFailed
			}
			if _, duplicate := seenFiles[relative]; duplicate {
				return ErrVerificationFailed
			}
			seenFiles[relative] = struct{}{}
			hash := sha256.New()
			written, err := io.Copy(hash, archive)
			if err != nil || written != expected.size || !bytes.Equal(hash.Sum(nil), expected.digest[:]) {
				return ErrVerificationFailed
			}
		default:
			return ErrVerificationFailed
		}
	}
	if len(seenFiles) != len(spec.files) {
		return ErrVerificationFailed
	}
	if err := verifyZeroTrailer(limited); err != nil || limited.N == 0 {
		return ErrVerificationFailed
	}
	return nil
}

func archiveRelativeName(name, root string, directory bool) (string, bool) {
	if name == "" || invalidText(name, maxInventoryPathBytes*2) || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") {
		return "", false
	}
	if directory && strings.HasSuffix(name, "/") {
		name = strings.TrimSuffix(name, "/")
	}
	if name == "" || path.Clean(name) != name {
		return "", false
	}
	if name == root {
		return "", true
	}
	prefix := root + "/"
	if !strings.HasPrefix(name, prefix) {
		return "", false
	}
	relative := strings.TrimPrefix(name, prefix)
	if !validRelativePath(relative) {
		return "", false
	}
	return relative, true
}

func verifyZeroTrailer(reader io.Reader) error {
	buffer := make([]byte, 32<<10)
	for {
		count, err := reader.Read(buffer)
		for _, value := range buffer[:count] {
			if value != 0 {
				return ErrVerificationFailed
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return ErrVerificationFailed
		}
	}
}

type progressReader struct {
	reader io.Reader
	empty  int
}

func (reader *progressReader) Read(buffer []byte) (int, error) {
	count, err := reader.reader.Read(buffer)
	if count == 0 && err == nil {
		reader.empty++
		if reader.empty >= 100 {
			return 0, io.ErrNoProgress
		}
	} else {
		reader.empty = 0
	}
	return count, err
}

func nilReader(reader io.Reader) bool {
	if reader == nil {
		return true
	}
	value := reflect.ValueOf(reader)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
