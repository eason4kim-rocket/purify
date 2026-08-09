package llm

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

func TestValidateAgainstSchemaViolationMatrix(t *testing.T) {
	schema := json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["name", "status", "items"],
  "properties": {
    "name": {"type": "string"},
    "status": {"enum": ["active", "paused"]},
    "items": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["quantity"],
        "properties": {"quantity": {"type": "number"}},
        "additionalProperties": false
      }
    }
  }
}`)

	tests := []struct {
		name         string
		data         string
		pathContains string
		messagePart  string
	}{
		{name: "wrong type", data: `{"name":12,"status":"active","items":[]}`, pathContains: "/name", messagePart: "string"},
		{name: "missing required", data: `{"status":"active","items":[]}`, pathContains: "$", messagePart: "name"},
		{name: "enum out of range", data: `{"name":"x","status":"deleted","items":[]}`, pathContains: "/status", messagePart: "one of"},
		{name: "nested array error", data: `{"name":"x","status":"active","items":[{"quantity":"many"}]}`, pathContains: "/items/0/quantity", messagePart: "number"},
		{name: "additional property", data: `{"name":"x","status":"active","items":[],"secret":true}`, pathContains: "$", messagePart: "additional"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			violations, err := ValidateAgainstSchema(schema, json.RawMessage(tt.data))
			if err != nil {
				t.Fatalf("ValidateAgainstSchema() error = %v", err)
			}
			if len(violations) == 0 {
				t.Fatal("expected at least one violation")
			}
			var matched bool
			for _, violation := range violations {
				if strings.Contains(violation.Path, tt.pathContains) && strings.Contains(strings.ToLower(violation.Message), strings.ToLower(tt.messagePart)) {
					matched = true
					break
				}
			}
			if !matched {
				t.Fatalf("violations = %#v, want path containing %q and message containing %q", violations, tt.pathContains, tt.messagePart)
			}
		})
	}
}

func TestValidateAgainstSchemaValid(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"],"additionalProperties":false}`)
	violations, err := ValidateAgainstSchema(schema, json.RawMessage(`{"count":3}`))
	if err != nil {
		t.Fatalf("ValidateAgainstSchema() error = %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %#v, want none", violations)
	}
}

func TestValidateSchemaRejectsExternalReference(t *testing.T) {
	err := ValidateSchema(json.RawMessage(`{"$ref":"https://example.com/schema.json"}`))
	if err == nil || !strings.Contains(err.Error(), "external schema reference") {
		t.Fatalf("ValidateSchema() error = %v, want external-reference rejection", err)
	}
}

func TestNormalizeSchemaPreservesLegacyShorthand(t *testing.T) {
	normalized, err := NormalizeSchema(json.RawMessage(`{"name":"string","price":"number","features":["string"]}`))
	if err != nil {
		t.Fatalf("NormalizeSchema() error = %v", err)
	}
	valid := json.RawMessage(`{"name":"Ada","price":29.99,"features":["fast"]}`)
	violations, err := ValidateAgainstSchema(normalized, valid)
	if err != nil || len(violations) != 0 {
		t.Fatalf("legacy normalized validation: violations=%#v err=%v schema=%s", violations, err, normalized)
	}
	nullable := json.RawMessage(`{"name":null,"price":null,"features":null}`)
	violations, err = ValidateAgainstSchema(normalized, nullable)
	if err != nil || len(violations) != 0 {
		t.Fatalf("legacy nullable validation: violations=%#v err=%v schema=%s", violations, err, normalized)
	}
}

func TestNormalizeSchemaDoesNotMistakeFieldNamedTypeForJSONSchema(t *testing.T) {
	normalized, err := NormalizeSchema(json.RawMessage(`{"type":"string","name":"string"}`))
	if err != nil {
		t.Fatalf("NormalizeSchema() error = %v", err)
	}
	violations, err := ValidateAgainstSchema(normalized, json.RawMessage(`{"type":"widget","name":"A"}`))
	if err != nil || len(violations) != 0 {
		t.Fatalf("field named type lost: violations=%#v err=%v schema=%s", violations, err, normalized)
	}
}

func TestCompiledSchemaCacheHitUsesCanonicalSchemaKey(t *testing.T) {
	var compilations atomic.Int32
	cache := newCompiledSchemaCache(8, 16<<10, 8<<10, func(schema json.RawMessage) (*jsonschema.Schema, error) {
		compilations.Add(1)
		return compileNormalizedSchema(schema)
	})

	first := json.RawMessage(`{
  "type": "object",
  "properties": {"name": {"type": "string"}},
  "required": ["name"]
}`)
	second := json.RawMessage(`{"required":["name"],"properties":{"name":{"type":"string"}},"type":"object"}`)
	compiledFirst := cachedCompileForTest(t, cache, first)
	compiledSecond := cachedCompileForTest(t, cache, second)

	if got := compilations.Load(); got != 1 {
		t.Fatalf("compilations = %d, want 1", got)
	}
	if compiledFirst != compiledSecond {
		t.Fatal("equivalent normalized schemas did not share one cache entry")
	}
}

func TestValidationEntrypointsShareCompiledSchemaCache(t *testing.T) {
	var compilations atomic.Int32
	cache := newCompiledSchemaCache(8, 16<<10, 8<<10, func(schema json.RawMessage) (*jsonschema.Schema, error) {
		compilations.Add(1)
		return compileNormalizedSchema(schema)
	})
	previous := schemaCache
	schemaCache = cache
	t.Cleanup(func() { schemaCache = previous })

	raw := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)
	if err := ValidateSchema(raw); err != nil {
		t.Fatalf("ValidateSchema() error = %v", err)
	}
	violations, err := ValidateAgainstSchema(raw, json.RawMessage(`{"ok":true}`))
	if err != nil || len(violations) != 0 {
		t.Fatalf("ValidateAgainstSchema() violations=%v error=%v", violations, err)
	}
	if got := compilations.Load(); got != 1 {
		t.Fatalf("compilations = %d, want both entrypoints to share 1", got)
	}
}

func TestCompiledSchemaCacheConcurrentSingleflight(t *testing.T) {
	var compilations atomic.Int32
	compileStarted := make(chan struct{})
	releaseCompile := make(chan struct{})
	cache := newCompiledSchemaCache(8, 16<<10, 8<<10, func(schema json.RawMessage) (*jsonschema.Schema, error) {
		if compilations.Add(1) == 1 {
			close(compileStarted)
		}
		<-releaseCompile
		return compileNormalizedSchema(schema)
	})

	raw := json.RawMessage(`{"type":"object","properties":{"value":{"type":"integer"}}}`)
	normalized, canonical := normalizedSchemaForTest(t, raw)
	const workers = 32
	start := make(chan struct{})
	results := make(chan *jsonschema.Schema, workers)
	errs := make(chan error, workers)
	var ready sync.WaitGroup
	ready.Add(workers)
	for range workers {
		go func() {
			ready.Done()
			<-start
			compiled, err := cache.get(canonical, normalized)
			results <- compiled
			errs <- err
		}()
	}
	ready.Wait()
	close(start)
	<-compileStarted

	deadline := time.Now().Add(2 * time.Second)
	for {
		cache.mu.Lock()
		inflight := len(cache.inflight)
		cache.mu.Unlock()
		if inflight == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	// Give all released workers a scheduling turn while the leader remains
	// blocked inside the compiler.
	time.Sleep(20 * time.Millisecond)
	close(releaseCompile)

	var first *jsonschema.Schema
	for range workers {
		if err := <-errs; err != nil {
			t.Fatalf("cache.get() error = %v", err)
		}
		compiled := <-results
		if first == nil {
			first = compiled
		} else if compiled != first {
			t.Fatal("singleflight callers received different compiled schemas")
		}
	}
	if got := compilations.Load(); got != 1 {
		t.Fatalf("compilations = %d, want 1", got)
	}
}

func TestCompiledSchemaCacheLRUEntryEviction(t *testing.T) {
	cache := newCompiledSchemaCache(2, 64<<10, 16<<10, compileNormalizedSchema)
	first := json.RawMessage(`{"type":"string","title":"first"}`)
	second := json.RawMessage(`{"type":"string","title":"second"}`)
	third := json.RawMessage(`{"type":"string","title":"third"}`)

	cachedCompileForTest(t, cache, first)
	cachedCompileForTest(t, cache, second)
	cachedCompileForTest(t, cache, first) // first becomes most recently used
	cachedCompileForTest(t, cache, third)

	firstKey := schemaKeyForTest(t, first)
	secondKey := schemaKeyForTest(t, second)
	thirdKey := schemaKeyForTest(t, third)
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(cache.entries))
	}
	if _, ok := cache.entries[firstKey]; !ok {
		t.Fatal("recently used first schema was evicted")
	}
	if _, ok := cache.entries[secondKey]; ok {
		t.Fatal("least recently used second schema was not evicted")
	}
	if _, ok := cache.entries[thirdKey]; !ok {
		t.Fatal("new third schema missing from cache")
	}
}

func TestCompiledSchemaCacheByteEviction(t *testing.T) {
	first := json.RawMessage(`{"type":"string","title":"first-byte-entry"}`)
	second := json.RawMessage(`{"type":"string","title":"second-byte-entry"}`)
	_, firstCanonical := normalizedSchemaForTest(t, first)
	_, secondCanonical := normalizedSchemaForTest(t, second)
	maxBytes := (sha256Size + len(firstCanonical)) + (sha256Size + len(secondCanonical)) - 1
	cache := newCompiledSchemaCache(10, maxBytes, maxBytes, compileNormalizedSchema)

	cachedCompileForTest(t, cache, first)
	cachedCompileForTest(t, cache, second)

	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.bytes > maxBytes {
		t.Fatalf("cache bytes = %d, exceeds max %d", cache.bytes, maxBytes)
	}
	if len(cache.entries) != 1 {
		t.Fatalf("entries = %d, want byte limit to evict down to 1", len(cache.entries))
	}
	if _, ok := cache.entries[schemaKeyForTest(t, second)]; !ok {
		t.Fatal("newest schema was evicted instead of oldest")
	}
}

func TestCompiledSchemaCacheOversizeBypassesStorage(t *testing.T) {
	var compilations atomic.Int32
	raw := json.RawMessage(`{"type":"string","description":"larger than this test cache permits"}`)
	_, canonical := normalizedSchemaForTest(t, raw)
	cache := newCompiledSchemaCache(8, 16<<10, len(canonical)-1, func(schema json.RawMessage) (*jsonschema.Schema, error) {
		compilations.Add(1)
		return compileNormalizedSchema(schema)
	})

	cachedCompileForTest(t, cache, raw)
	cachedCompileForTest(t, cache, raw)
	if got := compilations.Load(); got != 2 {
		t.Fatalf("compilations = %d, want 2 for oversize bypass", got)
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.entries) != 0 || cache.bytes != 0 {
		t.Fatalf("oversize schema retained: entries=%d bytes=%d", len(cache.entries), cache.bytes)
	}
}

func TestCompiledSchemaCacheDoesNotRetainFailures(t *testing.T) {
	tests := []struct {
		name string
		raw  json.RawMessage
		want string
	}{
		{name: "invalid", raw: json.RawMessage(`{"type":"not-a-json-type","required":[]}`), want: "compile JSON schema"},
		{name: "external ref", raw: json.RawMessage(`{"$ref":"https://example.com/schema.json"}`), want: "external schema reference"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var compilations atomic.Int32
			cache := newCompiledSchemaCache(8, 16<<10, 8<<10, func(schema json.RawMessage) (*jsonschema.Schema, error) {
				compilations.Add(1)
				return compileNormalizedSchema(schema)
			})
			normalized, canonical := normalizedSchemaForTest(t, tt.raw)
			for range 2 {
				_, err := cache.get(canonical, normalized)
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("cache.get() error = %v, want %q", err, tt.want)
				}
			}
			if got := compilations.Load(); got != 2 {
				t.Fatalf("compilations = %d, want failures not cached", got)
			}
			cache.mu.Lock()
			defer cache.mu.Unlock()
			if len(cache.entries) != 0 || cache.bytes != 0 {
				t.Fatalf("failed schema retained: entries=%d bytes=%d", len(cache.entries), cache.bytes)
			}
		})
	}
}

func TestCompiledSchemaCacheConcurrentValidation(t *testing.T) {
	cache := newCompiledSchemaCache(8, 16<<10, 8<<10, compileNormalizedSchema)
	raw := json.RawMessage(`{
  "type":"object",
  "required":["value"],
  "properties":{"value":{"type":"integer"}},
  "additionalProperties":false
}`)
	normalized, canonical := normalizedSchemaForTest(t, raw)
	compiled, err := cache.get(canonical, normalized)
	if err != nil {
		t.Fatalf("cache.get() error = %v", err)
	}

	const workers = 64
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			fromCache, err := cache.get(canonical, normalized)
			if err != nil {
				errs <- err
				return
			}
			if fromCache != compiled {
				errs <- fmt.Errorf("worker %d received a different compiled schema", i)
				return
			}
			instance, err := jsonschema.UnmarshalJSON(strings.NewReader(fmt.Sprintf(`{"value":%d}`, i)))
			if err != nil {
				errs <- err
				return
			}
			if err := fromCache.Validate(instance); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent validation: %v", err)
	}
}

func TestCompiledSchemaCacheIsolatesCallerBuffer(t *testing.T) {
	raw := json.RawMessage(`{"type":"string","title":"caller-owned"}`)
	normalized, canonical := normalizedSchemaForTest(t, raw)
	var compilerInput json.RawMessage
	cache := newCompiledSchemaCache(8, 16<<10, 8<<10, func(schema json.RawMessage) (*jsonschema.Schema, error) {
		compilerInput = schema
		return compileNormalizedSchema(schema)
	})
	compiled, err := cache.get(canonical, normalized)
	if err != nil {
		t.Fatalf("cache.get() error = %v", err)
	}
	for i := range normalized {
		normalized[i] = 'x'
	}
	if strings.Contains(string(compilerInput), "xxxx") || !json.Valid(compilerInput) {
		t.Fatalf("compiler input changed with caller buffer: %q", compilerInput)
	}
	instance, err := jsonschema.UnmarshalJSON(strings.NewReader(`"still valid"`))
	if err != nil {
		t.Fatal(err)
	}
	if err := compiled.Validate(instance); err != nil {
		t.Fatalf("cached schema changed with caller buffer: %v", err)
	}
}

const sha256Size = 32

func cachedCompileForTest(t *testing.T, cache *compiledSchemaCache, raw json.RawMessage) *jsonschema.Schema {
	t.Helper()
	normalized, canonical := normalizedSchemaForTest(t, raw)
	compiled, err := cache.get(canonical, normalized)
	if err != nil {
		t.Fatalf("cache.get() error = %v", err)
	}
	return compiled
}

func normalizedSchemaForTest(t *testing.T, raw json.RawMessage) (json.RawMessage, json.RawMessage) {
	t.Helper()
	normalized, err := NormalizeSchema(raw)
	if err != nil {
		t.Fatalf("NormalizeSchema() error = %v", err)
	}
	canonical, err := canonicalSchema(normalized)
	if err != nil {
		t.Fatalf("canonicalSchema() error = %v", err)
	}
	return normalized, canonical
}

func schemaKeyForTest(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	_, canonical := normalizedSchemaForTest(t, raw)
	digest := sha256.Sum256(canonical)
	return string(digest[:])
}
