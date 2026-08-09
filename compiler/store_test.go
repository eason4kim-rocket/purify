package compiler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/use-agent/purify/ledger"
)

var storeTestTime = time.Date(2026, 8, 9, 8, 7, 6, 123456789, time.UTC)

func TestBuildPageKeyCanonicalEquivalence(t *testing.T) {
	html := `<html><body><main><h1>Purify</h1></main></body></html>`
	shorthand := json.RawMessage(`{"name":"string"}`)
	standard := json.RawMessage(`{
		"required":["name"],
		"properties":{"name":{"type":["string","null"]}},
		"additionalProperties":false,
		"type":"object"
	}`)

	first, err := BuildPageKey("HTTPS://WWW.Example.COM:443/items#ignored", shorthand, html)
	if err != nil {
		t.Fatalf("BuildPageKey(shorthand) error = %v", err)
	}
	second, err := BuildPageKey("https://www.example.com/items", standard, html)
	if err != nil {
		t.Fatalf("BuildPageKey(standard) error = %v", err)
	}
	if first.URL != "https://www.example.com/items" || first.Host != "example.com" {
		t.Fatalf("canonical URL/host = %q/%q", first.URL, first.Host)
	}
	if first.PageHash != second.PageHash || first.SchemaHash != second.SchemaHash ||
		!bytes.Equal(first.Schema, second.Schema) {
		t.Fatalf("equivalent keys differ:\nfirst=%+v\nsecond=%+v", first, second)
	}

	idn, err := BuildPageKey("https://WWW.食狮.公司.CN:443/path", standard, html)
	if err != nil {
		t.Fatalf("BuildPageKey(IDN) error = %v", err)
	}
	if idn.URL != "https://www.xn--85x722f.xn--55qx5d.cn/path" ||
		idn.Host != "xn--85x722f.xn--55qx5d.cn" {
		t.Fatalf("IDN URL/host = %q/%q", idn.URL, idn.Host)
	}
	ip, err := BuildPageKey("https://8.8.8.8:443/", standard, html)
	if err != nil {
		t.Fatalf("BuildPageKey(IP) error = %v", err)
	}
	if ip.URL != "https://8.8.8.8/" || ip.Host != "8.8.8.8" {
		t.Fatalf("IP URL/host = %q/%q", ip.URL, ip.Host)
	}
}

func TestBuildPageKeyRejectsInvalidInputs(t *testing.T) {
	html := `<html><body><p>x</p></body></html>`
	cases := []struct {
		name   string
		url    string
		schema json.RawMessage
		html   string
	}{
		{"private IP", "http://127.0.0.1/", json.RawMessage(`{"x":"string"}`), html},
		{"duplicate schema key", "https://example.com/", json.RawMessage(`{"x":"string","x":"number"}`), html},
		{"non-object schema", "https://example.com/", json.RawMessage(`[]`), html},
		{"empty DOM", "https://example.com/", json.RawMessage(`{"x":"string"}`), "plain text"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := BuildPageKey(test.url, test.schema, test.html); err == nil {
				t.Fatal("BuildPageKey() error = nil")
			}
		})
	}
}

func TestStoreRejectsHandBuiltLegacyKeyAndLegacyStoredSchema(t *testing.T) {
	store, durable := openExtractorStore(t)
	key := extractorTestKey(t, "https://example.com/item", json.RawMessage(`{"name":"string"}`))
	key.TemplateSimHash = 0x8000000000000001
	legacy := json.RawMessage(`{"name":"string"}`)
	key.Schema = legacy
	key.SchemaHash = sha256Hex(legacy)
	if _, _, err := store.Lookup(context.Background(), key); !errors.Is(err, ErrInvalidStoreInput) {
		t.Fatalf("Lookup(hand-built legacy key) error = %v", err)
	}

	ir, report := extractorFixture("h1")
	irJSON, _ := json.Marshal(ir)
	reportJSON, _ := json.Marshal(report)
	id := "00000000-0000-4000-8000-000000000099"
	if err := durable.Update(context.Background(), func(tx ledger.WriteTx) error {
		_, err := tx.ExecContext(context.Background(), `INSERT INTO extractors (
			id, host, schema_json, schema_hash, template_cluster_id,
			template_simhash, ir, ir_hash, ir_format_version, version,
			validation_report, validation, state, empty_window, stale_reason,
			created_at, last_used_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, 'active', '[]', '', ?, NULL, ?)`,
			id, "example.com", string(legacy), sha256Hex(legacy), templateClusterID(key.TemplateSimHash),
			encodeSimHash(key.TemplateSimHash), string(irJSON), sha256Hex(irJSON), ir.Version,
			string(reportJSON), report.Overall, formatExtractorTime(storeTestTime), formatExtractorTime(storeTestTime))
		return err
	}); err != nil {
		t.Fatalf("insert legacy schema row: %v", err)
	}
	if _, err := store.Get(context.Background(), id); !errors.Is(err, ErrExtractorStore) {
		t.Fatalf("Get(legacy stored schema) error = %v", err)
	}
}

func TestTemplateMedoidBoundariesAndOrder(t *testing.T) {
	base := uint64(1) << 63
	values := []uint64{base, base | 1, base | 3}
	first, err := TemplateMedoid(values)
	if err != nil {
		t.Fatalf("TemplateMedoid() error = %v", err)
	}
	second, err := TemplateMedoid([]uint64{values[2], values[0], values[1]})
	if err != nil {
		t.Fatalf("TemplateMedoid(reordered) error = %v", err)
	}
	if first != base|1 || second != first {
		t.Fatalf("medoids = %#x/%#x, want %#x", first, second, base|1)
	}
	if got, err := TemplateMedoid([]uint64{base, base, base | 0x3f}); err != nil || got != base {
		t.Fatalf("distance-6 medoid = %#x, %v", got, err)
	}
	invalid := [][]uint64{
		{base, base},
		{base, base, 0},
		{base, base, base | 0x7f},
	}
	for _, samples := range invalid {
		if _, err := TemplateMedoid(samples); !errors.Is(err, ErrInvalidStoreInput) {
			t.Fatalf("TemplateMedoid(%v) error = %v", samples, err)
		}
	}
}

func TestStoreSaveGetIdempotencyAndBigEndianSimHash(t *testing.T) {
	store, durable := openExtractorStore(t)
	key := extractorTestKey(t, "https://shop.example.com/product/1", json.RawMessage(`{"name":"string"}`))
	key.TemplateSimHash = 0x8000000000000001
	ir, report := extractorFixture("h1")

	first, err := store.Save(context.Background(), key, ir, report)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if !validExtractorID(first.ID) || first.Version != 1 || first.State != StateActive ||
		first.TemplateSimHash != key.TemplateSimHash {
		t.Fatalf("saved extractor = %+v", first)
	}
	var raw []byte
	if err := durable.View(context.Background(), func(tx ledger.ReadTx) error {
		return tx.QueryRowContext(context.Background(),
			"SELECT template_simhash FROM extractors WHERE id = ?", first.ID).Scan(&raw)
	}); err != nil {
		t.Fatalf("read raw simhash: %v", err)
	}
	if want := []byte{0x80, 0, 0, 0, 0, 0, 0, 1}; !bytes.Equal(raw, want) {
		t.Fatalf("template_simhash = %x, want %x", raw, want)
	}

	retry, err := store.Save(context.Background(), key, ir, report)
	if err != nil {
		t.Fatalf("Save(exact retry) error = %v", err)
	}
	if retry.ID != first.ID || retry.Version != 1 {
		t.Fatalf("exact retry = %s v%d, want %s v1", retry.ID, retry.Version, first.ID)
	}
	got, err := store.Get(context.Background(), first.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !reflect.DeepEqual(got, first) {
		t.Fatalf("Get() = %#v, want %#v", got, first)
	}

	weak := report
	weak.CanEnable = false
	if _, err := store.Save(context.Background(), key, ir, weak); !errors.Is(err, ErrInvalidStoreInput) {
		t.Fatalf("Save(disabled report) error = %v", err)
	}
	mismatchedIR := IR{Version: CurrentIRVersion, Fields: []FieldRule{{
		Name: "price", Selector: ".price", Type: TypeString, Required: true,
	}}}
	mismatchedReport := ValidationReport{
		PerField: []FieldValidation{{Name: "price", Matches: 3, Samples: 3, Score: 1}},
		Overall:  1, Samples: 3, ValidSamples: 3,
		Threshold: ValidationThreshold, CanEnable: true,
	}
	if _, err := store.Save(context.Background(), key, mismatchedIR, mismatchedReport); !errors.Is(err, ErrInvalidStoreInput) {
		t.Fatalf("Save(schema-mismatched IR) error = %v", err)
	}
}

func TestStoreMetadataRejectsMismatchedClusterID(t *testing.T) {
	store, durable := openExtractorStore(t)
	key := extractorTestKey(t, "https://example.com/item", json.RawMessage(`{"name":"string"}`))
	key.TemplateSimHash = 0x8000000000000001
	ir, report := extractorFixture("h1")
	irJSON, _ := json.Marshal(ir)
	reportJSON, _ := json.Marshal(report)
	id := "00000000-0000-4000-8000-000000000098"
	if err := durable.Update(context.Background(), func(tx ledger.WriteTx) error {
		_, err := tx.ExecContext(context.Background(), `INSERT INTO extractors (
			id, host, schema_json, schema_hash, template_cluster_id,
			template_simhash, ir, ir_hash, ir_format_version, version,
			validation_report, validation, state, empty_window, stale_reason,
			created_at, last_used_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, 'active', '[]', '', ?, NULL, ?)`,
			id, key.Host, string(key.Schema), key.SchemaHash, strings.Repeat("a", 64),
			encodeSimHash(key.TemplateSimHash), string(irJSON), sha256Hex(irJSON), ir.Version,
			string(reportJSON), report.Overall, formatExtractorTime(storeTestTime), formatExtractorTime(storeTestTime))
		return err
	}); err != nil {
		t.Fatalf("insert mismatched cluster row: %v", err)
	}
	unbound := extractorTestKey(t, "https://example.com/unbound", json.RawMessage(`{"name":"string"}`))
	unbound.TemplateSimHash = key.TemplateSimHash
	if _, _, err := store.Lookup(context.Background(), unbound); !errors.Is(err, ErrExtractorStore) {
		t.Fatalf("Lookup(mismatched cluster) error = %v", err)
	}
}

func TestStoreLookupBindingRejectsSilentReplacementAndDetectsDrift(t *testing.T) {
	store, durable := openExtractorStore(t)
	key := extractorTestKey(t, "https://shop.example.com/product/1", json.RawMessage(`{"name":"string"}`))
	key.TemplateSimHash = 0x8000000000000001
	ir, report := extractorFixture("h1")
	first, err := store.Save(context.Background(), key, ir, report)
	if err != nil {
		t.Fatalf("Save(v1) error = %v", err)
	}

	unbound := extractorTestKey(t, "https://shop.example.com/product/2", json.RawMessage(`{"name":"string"}`))
	unbound.TemplateSimHash = key.TemplateSimHash ^ 0x3f
	matched, ok, err := store.Lookup(context.Background(), unbound)
	if err != nil || !ok || matched.ID != first.ID {
		t.Fatalf("Lookup(distance 6) = %s/%v/%v", matched.ID, ok, err)
	}
	assertBindingCount(t, durable, unbound, 0)
	if _, err := store.RecordUse(context.Background(), unbound, first.ID, UseNotExecuted); err != nil {
		t.Fatalf("RecordUse(not executed) error = %v", err)
	}
	assertBindingCount(t, durable, unbound, 0)
	if _, err := store.Touch(context.Background(), unbound, first.ID); err != nil {
		t.Fatalf("Touch() error = %v", err)
	}
	assertBindingID(t, durable, unbound, first.ID)

	secondIR, secondReport := extractorFixture(".name")
	if _, err := store.Save(context.Background(), key, secondIR, secondReport); !errors.Is(err, ErrHealingRequired) {
		t.Fatalf("Save(replacement) error = %v", err)
	}
	assertBindingID(t, durable, unbound, first.ID)
	stillCurrent, err := store.Get(context.Background(), first.ID)
	if err != nil || stillCurrent.State != StateActive || stillCurrent.Version != 1 {
		t.Fatalf("active revision changed after rejected replacement = %+v, %v", stillCurrent, err)
	}

	far := extractorTestKey(t, "https://shop.example.com/product/new", json.RawMessage(`{"name":"string"}`))
	far.TemplateSimHash = 0x5555555555555555
	if _, ok, err := store.Lookup(context.Background(), far); err != nil || ok {
		t.Fatalf("generic far Lookup() = ok %v, err %v", ok, err)
	}
	stillActive, err := store.Get(context.Background(), first.ID)
	if err != nil || stillActive.State != StateActive {
		t.Fatalf("generic miss degraded active extractor: %+v, %v", stillActive, err)
	}

	boundDrift := unbound
	boundDrift.TemplateSimHash = far.TemplateSimHash
	if _, ok, err := store.Lookup(context.Background(), boundDrift); err != nil || ok {
		t.Fatalf("bound drift Lookup() = ok %v, err %v", ok, err)
	}
	drifted, err := store.Get(context.Background(), first.ID)
	if err != nil || drifted.State != StateStale || drifted.StaleReason != "template_drift" {
		t.Fatalf("bound active after drift = %+v, %v", drifted, err)
	}
}

func TestStoreInitialRegistrationRejectsNearbyExistingCluster(t *testing.T) {
	store, durable := openExtractorStore(t)
	base := uint64(1) << 63
	firstPage := extractorTestKey(t, "https://example.com/a", json.RawMessage(`{"name":"string"}`))
	firstPage.TemplateSimHash = base
	ir, report := extractorFixture("h1")
	first, err := store.Save(context.Background(), firstPage, ir, report)
	if err != nil {
		t.Fatalf("Save(v1) error = %v", err)
	}

	oldPage := extractorTestKey(t, "https://example.com/p", json.RawMessage(`{"name":"string"}`))
	oldPage.TemplateSimHash = base ^ 0x3f
	if _, err := store.Touch(context.Background(), oldPage, first.ID); err != nil {
		t.Fatalf("Touch(old page) error = %v", err)
	}

	promotionPage := extractorTestKey(t, "https://example.com/b", json.RawMessage(`{"name":"string"}`))
	promotionPage.TemplateSimHash = base ^ 0xfc0
	secondIR, secondReport := extractorFixture(".name")
	if _, err := store.Save(context.Background(), promotionPage, secondIR, secondReport); !errors.Is(err, ErrHealingRequired) {
		t.Fatalf("Save(nearby target) error = %v", err)
	}
	assertBindingID(t, durable, oldPage, first.ID)
	matched, ok, err := store.Lookup(context.Background(), oldPage)
	if err != nil || !ok || matched.ID != first.ID {
		t.Fatalf("Lookup(old page after exact registration) = %s/%v/%v", matched.ID, ok, err)
	}
	current, err := store.Get(context.Background(), first.ID)
	if err != nil || current.State != StateActive {
		t.Fatalf("first extractor changed unexpectedly: %+v, %v", current, err)
	}
}

func TestStoreExactRetryDoesNotStealAnotherLineageBinding(t *testing.T) {
	store, durable := openExtractorStore(t)
	schema := json.RawMessage(`{"name":"string"}`)
	firstKey := extractorTestKey(t, "https://example.com/a", schema)
	firstKey.TemplateSimHash = uint64(1) << 63
	secondKey := extractorTestKey(t, "https://example.com/b", schema)
	secondKey.TemplateSimHash = 0x40000000000000ff
	ir, report := extractorFixture("h1")
	first, err := store.Save(context.Background(), firstKey, ir, report)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Save(context.Background(), secondKey, ir, report)
	if err != nil {
		t.Fatal(err)
	}
	if err := durable.Update(context.Background(), func(tx ledger.WriteTx) error {
		_, err := tx.ExecContext(context.Background(), `UPDATE extractor_page_bindings
			SET extractor_id = ? WHERE page_hash = ? AND schema_hash = ?`,
			second.ID, firstKey.PageHash, firstKey.SchemaHash)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(context.Background(), firstKey, ir, report); !errors.Is(err, ErrHealingRequired) {
		t.Fatalf("Save(exact retry with foreign binding) error = %v", err)
	}
	assertBindingID(t, durable, firstKey, second.ID)
	current, err := store.Get(context.Background(), first.ID)
	if err != nil || current.State != StateActive {
		t.Fatalf("first active changed = %+v, %v", current, err)
	}
}

func TestStoreHealthWindowThresholdAndNotExecuted(t *testing.T) {
	store, _ := openExtractorStore(t)
	key := extractorTestKey(t, "https://example.com/item", json.RawMessage(`{"name":"string"}`))
	key.TemplateSimHash = 0x8000000000000001
	ir, report := extractorFixture("h1")
	value, err := store.Save(context.Background(), key, ir, report)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	for index := 0; index < 14; index++ {
		value, err = store.Touch(context.Background(), key, value.ID)
		if err != nil {
			t.Fatalf("Touch(%d) error = %v", index, err)
		}
	}
	for index := 0; index < 6; index++ {
		value, err = store.RecordEmpty(context.Background(), key, value.ID)
		if err != nil {
			t.Fatalf("RecordEmpty(%d) error = %v", index, err)
		}
	}
	if len(value.EmptyWindow) != 20 || value.State != StateActive {
		t.Fatalf("6/20 extractor = state %q window %v", value.State, value.EmptyWindow)
	}
	before := cloneExtractor(value)
	value, err = store.RecordUse(context.Background(), key, value.ID, UseNotExecuted)
	if err != nil {
		t.Fatalf("RecordUse(not executed) error = %v", err)
	}
	if !reflect.DeepEqual(value, before) {
		t.Fatalf("not_executed changed extractor:\nbefore=%+v\nafter=%+v", before, value)
	}
	value, err = store.RecordEmpty(context.Background(), key, value.ID)
	if err != nil {
		t.Fatalf("RecordEmpty(7th) error = %v", err)
	}
	if value.State != StateStale || value.StaleReason != "required_empty_rate" || len(value.EmptyWindow) != 20 {
		t.Fatalf("7/20 extractor = state %q reason %q window %v", value.State, value.StaleReason, value.EmptyWindow)
	}
	retired, err := store.Retire(context.Background(), value.ID)
	if err != nil {
		t.Fatalf("Retire() error = %v", err)
	}
	if retired.State != StateRetired || retired.StaleReason != "required_empty_rate" {
		t.Fatalf("retired extractor lost drift reason: %+v", retired)
	}
}

func TestStoreClampsRegressingClockAcrossLifecycle(t *testing.T) {
	durable, err := ledger.Open(t.TempDir())
	if err != nil {
		t.Fatalf("ledger.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = durable.Close() })
	times := []time.Time{
		storeTestTime,
		storeTestTime.Add(-time.Second),
		storeTestTime.Add(-2 * time.Second),
		storeTestTime.Add(-3 * time.Second),
		storeTestTime.Add(-4 * time.Second),
		storeTestTime.Add(-5 * time.Second),
	}
	clockIndex := 0
	store, err := NewStore(durable, WithStoreClock(func() time.Time {
		if clockIndex >= len(times) {
			return times[len(times)-1]
		}
		value := times[clockIndex]
		clockIndex++
		return value
	}))
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	key := extractorTestKey(t, "https://example.com/item", json.RawMessage(`{"name":"string"}`))
	key.TemplateSimHash = 0x8000000000000001
	ir, report := extractorFixture("h1")
	first, err := store.Save(context.Background(), key, ir, report)
	if err != nil {
		t.Fatalf("Save(v1) error = %v", err)
	}
	if _, err := store.Touch(context.Background(), key, first.ID); err != nil {
		t.Fatalf("Touch(regressing clock) error = %v", err)
	}
	if _, err := store.RecordEmpty(context.Background(), key, first.ID); err != nil {
		t.Fatalf("RecordEmpty(regressing clock) error = %v", err)
	}
	if got, err := store.Get(context.Background(), first.ID); err != nil || !got.UpdatedAt.Equal(storeTestTime) {
		t.Fatalf("Get(v1 after clock rollback) = %+v, %v", got, err)
	}

	secondIR, secondReport := extractorFixture(".name")
	if _, err := store.Save(context.Background(), key, secondIR, secondReport); !errors.Is(err, ErrHealingRequired) {
		t.Fatalf("Save(replacement, regressing clock) error = %v", err)
	}

	drift := key
	drift.TemplateSimHash = 0x5555555555555555
	if _, ok, err := store.Lookup(context.Background(), drift); err != nil || ok {
		t.Fatalf("Lookup(drift, regressing clock) = ok %v, err %v", ok, err)
	}
	drifted, err := store.Get(context.Background(), first.ID)
	if err != nil || drifted.State != StateStale || !drifted.UpdatedAt.Equal(storeTestTime) {
		t.Fatalf("Get(drifted after clock rollback) = %+v, %v", drifted, err)
	}
	retired, err := store.Retire(context.Background(), first.ID)
	if err != nil {
		t.Fatalf("Retire(regressing clock) error = %v", err)
	}
	if retired.State != StateRetired || !retired.UpdatedAt.Equal(storeTestTime) {
		t.Fatalf("retired after clock rollback = %+v", retired)
	}
	if _, err := store.Get(context.Background(), first.ID); err != nil {
		t.Fatalf("Get(retired after clock rollback) error = %v", err)
	}
}

func TestStoreConcurrentExactSaveAndRecordUse(t *testing.T) {
	store, _ := openExtractorStore(t)
	key := extractorTestKey(t, "https://example.com/item", json.RawMessage(`{"name":"string"}`))
	key.TemplateSimHash = 0x8000000000000001
	ir, report := extractorFixture("h1")

	const savers = 24
	start := make(chan struct{})
	results := make(chan Extractor, savers)
	errorsByWorker := make(chan error, savers)
	var group sync.WaitGroup
	for index := 0; index < savers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			value, err := store.Save(context.Background(), key, ir, report)
			if err != nil {
				errorsByWorker <- err
				return
			}
			results <- value
		}()
	}
	close(start)
	group.Wait()
	close(results)
	close(errorsByWorker)
	for err := range errorsByWorker {
		t.Fatalf("concurrent Save() error = %v", err)
	}
	firstID := ""
	for value := range results {
		if firstID == "" {
			firstID = value.ID
		}
		if value.ID != firstID || value.Version != 1 {
			t.Fatalf("concurrent exact Save() = %s v%d, want %s v1", value.ID, value.Version, firstID)
		}
	}

	outcomes := make([]UseOutcome, 20)
	for index := range outcomes {
		outcomes[index] = UseSucceeded
	}
	for index := 0; index < 7; index++ {
		outcomes[index] = UseRequiredEmpty
	}
	errorsByUse := make(chan error, len(outcomes))
	for _, outcome := range outcomes {
		outcome := outcome
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := store.RecordUse(context.Background(), key, firstID, outcome)
			errorsByUse <- err
		}()
	}
	group.Wait()
	close(errorsByUse)
	for err := range errorsByUse {
		if err != nil {
			t.Fatalf("concurrent RecordUse() error = %v", err)
		}
	}
	got, err := store.Get(context.Background(), firstID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.State != StateStale || len(got.EmptyWindow) != 20 {
		t.Fatalf("concurrent health = state %q window %v", got.State, got.EmptyWindow)
	}
}

func TestStoreCandidateScanDecodesOnlySelectedExtractor(t *testing.T) {
	store, durable := openExtractorStore(t)
	key := extractorTestKey(t, "https://example.com/selected", json.RawMessage(`{"name":"string"}`))
	key.TemplateSimHash = uint64(1) << 63
	ir, report := extractorFixture("h1")
	selected, err := store.Save(context.Background(), key, ir, report)
	if err != nil {
		t.Fatalf("Save(selected) error = %v", err)
	}

	largeIR := `{"padding":"` + strings.Repeat("x", MaxRuleDefinitionBytes-64) + `"}`
	if err := durable.Update(context.Background(), func(tx ledger.WriteTx) error {
		for index := 1; index < MaxTemplateClusters; index++ {
			fingerprint := uint64(index)*0x9e3779b97f4a7c15 + 1
			for fingerprint == 0 || fingerprint == key.TemplateSimHash ||
				simhashDistanceForTest(fingerprint, key.TemplateSimHash) <= TemplateDistanceThreshold {
				fingerprint ^= 0xaaaaaaaaaaaaaaaa
			}
			id, err := newExtractorID()
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(context.Background(), `INSERT INTO extractors (
				id, host, schema_json, schema_hash, template_cluster_id,
				template_simhash, ir, ir_hash, ir_format_version, version,
				validation_report, validation, state, empty_window, stale_reason,
				created_at, last_used_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, 1, '{"can_enable":true}',
				1.0, 'active', '[]', '', ?, NULL, ?)`,
				id, key.Host, string(key.Schema), key.SchemaHash, templateClusterID(fingerprint),
				encodeSimHash(fingerprint), largeIR, sha256Hex([]byte(largeIR)),
				formatExtractorTime(storeTestTime), formatExtractorTime(storeTestTime)); err != nil {
				return fmt.Errorf("insert cluster %d: %w", index, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed 256 clusters: %v", err)
	}

	var decodes atomic.Int64
	previousObserver := observeFullExtractorDecode
	observeFullExtractorDecode = func() { decodes.Add(1) }
	t.Cleanup(func() { observeFullExtractorDecode = previousObserver })
	unbound := extractorTestKey(t, "https://example.com/unbound", json.RawMessage(`{"name":"string"}`))
	unbound.TemplateSimHash = key.TemplateSimHash
	got, ok, err := store.Lookup(context.Background(), unbound)
	if err != nil || !ok || got.ID != selected.ID {
		t.Fatalf("Lookup(256 clusters) = %s/%v/%v", got.ID, ok, err)
	}
	if count := decodes.Load(); count != 1 {
		t.Fatalf("full extractor decodes = %d, want only selected row", count)
	}
}

func openExtractorStore(t *testing.T) (*Store, *ledger.Store) {
	t.Helper()
	durable, err := ledger.Open(t.TempDir())
	if err != nil {
		t.Fatalf("ledger.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = durable.Close() })
	store, err := NewStore(durable, WithStoreClock(func() time.Time { return storeTestTime }))
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	return store, durable
}

func extractorTestKey(t *testing.T, rawURL string, schema json.RawMessage) PageKey {
	t.Helper()
	key, err := BuildPageKey(rawURL, schema, `<html><body><main><h1 class="name">Purify</h1></main></body></html>`)
	if err != nil {
		t.Fatalf("BuildPageKey() error = %v", err)
	}
	return key
}

func extractorFixture(selector string) (IR, ValidationReport) {
	ir := IR{Version: CurrentIRVersion, Fields: []FieldRule{{
		Name: "name", Selector: selector, Type: TypeString, Required: true,
	}}}
	report := ValidationReport{
		PerField: []FieldValidation{{Name: "name", Matches: 3, Samples: 3, Score: 1}},
		Overall:  1, Samples: 3, ValidSamples: 3,
		Threshold: ValidationThreshold, CanEnable: true,
	}
	return ir, report
}

func assertBindingCount(t *testing.T, durable *ledger.Store, key PageKey, want int) {
	t.Helper()
	var got int
	if err := durable.View(context.Background(), func(tx ledger.ReadTx) error {
		return tx.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM extractor_page_bindings
			WHERE page_hash = ? AND schema_hash = ?`, key.PageHash, key.SchemaHash).Scan(&got)
	}); err != nil {
		t.Fatalf("count binding: %v", err)
	}
	if got != want {
		t.Fatalf("binding count = %d, want %d", got, want)
	}
}

func assertBindingID(t *testing.T, durable *ledger.Store, key PageKey, want string) {
	t.Helper()
	var got string
	if err := durable.View(context.Background(), func(tx ledger.ReadTx) error {
		return tx.QueryRowContext(context.Background(), `SELECT extractor_id FROM extractor_page_bindings
			WHERE page_hash = ? AND schema_hash = ?`, key.PageHash, key.SchemaHash).Scan(&got)
	}); err != nil {
		t.Fatalf("read binding: %v", err)
	}
	if got != want {
		t.Fatalf("binding extractor = %q, want %q", got, want)
	}
}

func simhashDistanceForTest(first, second uint64) int {
	value := first ^ second
	distance := 0
	for value != 0 {
		value &= value - 1
		distance++
	}
	return distance
}
