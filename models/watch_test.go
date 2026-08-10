package models

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestWatchWireModelsHaveStableCredentialFreeShapes(t *testing.T) {
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	view := WatchView{
		ID: "00000000-0000-4000-8000-000000000001",
		Spec: FactSpec{Subject: "example", Predicate: "price", Freshness: "day",
			MinIndependentSources: 2, OnConflict: FactConflictExpose},
		State: "active", NextCheckAt: &now, EWMAIntervalSeconds: 3600,
		CreatedAt: now, UpdatedAt: now,
	}
	encoded, err := json.Marshal(CreateWatchResponse{Watch: view, Created: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"lease_id", "lease_until", "webhook", "secret", "credential",
		"last_verification_id", "last_verification_claim_index",
	} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("public watch leaked %q: %s", forbidden, encoded)
		}
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	if len(document) != 2 || string(document["created"]) != "true" || document["watch"] == nil {
		t.Fatalf("create response = %s", encoded)
	}
}

func TestFactLookupGapAndListsEncodeNonNilCollections(t *testing.T) {
	now := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	gap, err := json.Marshal(FactLookupResponse{Fact: nil, AsOf: now})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gap), `"fact":null`) {
		t.Fatalf("gap response = %s", gap)
	}
	page, err := json.Marshal(WatchListResponse{Watches: []WatchView{}})
	if err != nil {
		t.Fatal(err)
	}
	if string(page) != `{"watches":[]}` {
		t.Fatalf("empty page = %s", page)
	}
}

func TestWatchStableErrorCodes(t *testing.T) {
	for name, value := range map[string]string{
		"unavailable": ErrCodeWatchUnavailable,
		"not found":   ErrCodeWatchNotFound,
		"limit":       ErrCodeWatchLimitReached,
		"fact":        ErrCodeFactUnavailable,
	} {
		if value == "" || strings.ToUpper(value) != value {
			t.Fatalf("%s code = %q", name, value)
		}
	}
}
