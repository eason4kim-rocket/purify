package verify

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/use-agent/purify/evidence"
	"github.com/use-agent/purify/ledger"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/receipts"
	"github.com/use-agent/purify/simhash"
	"github.com/use-agent/purify/snapshot"
)

const (
	testURL       = "https://example.com/pricing"
	testFinalURL  = "https://www.example.com/pricing"
	oldSnapshotID = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	newSnapshotID = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

var (
	oldFetchedAt = time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	newFetchedAt = time.Date(2026, 8, 9, 11, 12, 13, 456, time.FixedZone("fixture", 8*60*60))
)

func TestVerifyDecisionTable(t *testing.T) {
	tests := []struct {
		name          string
		fixture       string
		statusCode    int
		claim         models.Claim
		wantStatus    models.VerifyStatus
		wantGoneScope models.VerifyGoneScope
		wantNewValue  json.RawMessage
		wantMethod    evidence.Method
		wantChange    bool
	}{
		{
			name:       "selector value unchanged",
			fixture:    "same.html",
			statusCode: 200,
			claim:      testClaim("plan", `"Pro Plan"`, "Pro Plan", "#plan .title"),
			wantStatus: models.VerifyStatusConfirmed,
			wantMethod: evidence.MethodExact,
		},
		{
			name:         "selector value changed",
			fixture:      "changed.html",
			statusCode:   200,
			claim:        testClaim("plan", `"Pro Plan"`, "Pro Plan", "#plan .title"),
			wantStatus:   models.VerifyStatusChanged,
			wantNewValue: json.RawMessage(`"Enterprise Plan"`),
			wantMethod:   evidence.MethodExact,
			wantChange:   true,
		},
		{
			name:          "field disappeared",
			fixture:       "missing.html",
			statusCode:    200,
			claim:         testClaim("plan", `"Pro Plan"`, "Pro Plan", "#plan .title"),
			wantStatus:    models.VerifyStatusGone,
			wantGoneScope: models.VerifyGoneScopeField,
		},
		{
			name:          "page returned 404",
			fixture:       "missing.html",
			statusCode:    404,
			claim:         testClaim("plan", `"Pro Plan"`, "Pro Plan", "#plan .title"),
			wantStatus:    models.VerifyStatusGone,
			wantGoneScope: models.VerifyGoneScopePage,
		},
		{
			name:          "page returned 410",
			fixture:       "missing.html",
			statusCode:    410,
			claim:         testClaim("plan", `"Pro Plan"`, "Pro Plan", "#plan .title"),
			wantStatus:    models.VerifyStatusGone,
			wantGoneScope: models.VerifyGoneScopePage,
		},
		{
			name:       "selector lost but exact quote remains",
			fixture:    "changed.html",
			statusCode: 200,
			claim: testClaim(
				"description",
				`"Durable fact quote remains exactly here"`,
				"Durable fact quote remains exactly here",
				"#legacy-location",
			),
			wantStatus: models.VerifyStatusConfirmed,
			wantMethod: evidence.MethodExact,
		},
		{
			name:       "selector lost after canonical whitespace collapse",
			fixture:    "quote-moved.html",
			statusCode: 200,
			claim: testClaim(
				"description",
				`"Durable fact quote remains exactly here"`,
				"Durable fact quote remains exactly here",
				"#legacy-location",
			),
			wantStatus: models.VerifyStatusConfirmed,
			wantMethod: evidence.MethodExact,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			oldHTML := fixture(t, "old.html")
			currentHTML := fixture(t, test.fixture)
			recorder := &fakeRecorder{}
			signer := testSigner(t)
			service := testService(t, oldHTML, observation(currentHTML, test.statusCode), signer, recorder, nil)

			response, err := service.Verify(context.Background(), models.VerifyRequest{
				URL:           testURL,
				Claims:        []models.Claim{test.claim},
				WebhookURL:    "https://hooks.example.test/facts#ignored",
				WebhookSecret: "fixture-secret",
			})
			if err != nil {
				t.Fatalf("Verify() error = %v", err)
			}
			if response == nil || len(response.Results) != 1 {
				t.Fatalf("Verify() response = %#v, want one result", response)
			}
			result := response.Results[0]
			if result.Status != test.wantStatus {
				t.Fatalf("status = %q, want %q", result.Status, test.wantStatus)
			}
			if result.GoneScope != test.wantGoneScope {
				t.Fatalf("gone scope = %q, want %q", result.GoneScope, test.wantGoneScope)
			}
			assertJSONEqual(t, result.NewValue, test.wantNewValue, "new value")
			wantSimilarity := 1 - float64(simhash.Distance(
				simhash.FingerprintDOM(oldHTML),
				simhash.FingerprintDOM(currentHTML),
			))/64
			if test.statusCode == 404 || test.statusCode == 410 {
				if response.PageSimilarity != nil {
					t.Fatalf("page similarity = %v for gone page, want omitted", *response.PageSimilarity)
				}
			} else if response.PageSimilarity == nil || *response.PageSimilarity != wantSimilarity {
				t.Fatalf("page similarity = %v, want %v", response.PageSimilarity, wantSimilarity)
			}
			if response.URL != testURL || response.FinalURL != testFinalURL || response.StatusCode != test.statusCode {
				t.Fatalf("response observation = (%q, %q, %d)", response.URL, response.FinalURL, response.StatusCode)
			}
			if response.SnapshotID != newSnapshotID || !response.VerifiedAt.Equal(newFetchedAt.UTC()) {
				t.Fatalf("current provenance = (%q, %v), want (%q, %v)", response.SnapshotID, response.VerifiedAt, newSnapshotID, newFetchedAt.UTC())
			}

			batches := recorder.committedBatches()
			if len(batches) != 1 || len(batches[0]) != 1 {
				t.Fatalf("recorded batches = %#v, want one transaction with one row", batches)
			}
			row := batches[0][0]
			if row.VerificationID != response.VerificationID || row.ClaimIndex != 0 || row.OldSnapshotID != oldSnapshotID {
				t.Fatalf("recorded row identity/provenance = %#v", row)
			}
			if row.Outcome != ledger.Outcome(test.wantStatus) {
				t.Fatalf("recorded outcome = %q, want %q", row.Outcome, test.wantStatus)
			}
			if ledger.GoneScope(test.wantGoneScope) != row.GoneScope {
				t.Fatalf("recorded gone scope = %q, want %q", row.GoneScope, test.wantGoneScope)
			}
			if test.statusCode == 404 || test.statusCode == 410 {
				if row.PageSimilarity != nil {
					t.Fatalf("gone row page similarity = %v, want nil", *row.PageSimilarity)
				}
			} else if row.PageSimilarity == nil || *row.PageSimilarity != wantSimilarity {
				t.Fatalf("recorded page similarity = %v, want %v", row.PageSimilarity, wantSimilarity)
			}

			if test.wantStatus == models.VerifyStatusGone {
				if result.Evidence != nil || result.Receipt != "" || row.Receipt != "" {
					t.Fatalf("gone result unexpectedly refreshed evidence/receipt: result=%#v row=%#v", result, row)
				}
			} else {
				if result.Evidence == nil || result.Evidence.SnapshotID != newSnapshotID || !result.Evidence.FetchedAt.Equal(newFetchedAt.UTC()) {
					t.Fatalf("refreshed evidence = %#v", result.Evidence)
				}
				if result.Evidence.Method != test.wantMethod {
					t.Fatalf("refreshed method = %q, want %q", result.Evidence.Method, test.wantMethod)
				}
				payload, err := signer.Verify(result.Receipt)
				if err != nil {
					t.Fatalf("Verify(refreshed receipt) error = %v", err)
				}
				wantReceiptValue := test.claim.Value
				if test.wantStatus == models.VerifyStatusChanged {
					wantReceiptValue = test.wantNewValue
				}
				assertJSONEqual(t, payload.Value, wantReceiptValue, "refreshed receipt value")
				if payload.Anchor.SnapshotID != newSnapshotID || row.Receipt != result.Receipt {
					t.Fatalf("refreshed receipt provenance mismatch: payload=%#v row=%#v", payload, row)
				}
			}

			events := recorder.committedOutboxEvents()
			if len(events) != 1 {
				t.Fatalf("outbox batches = %#v, want one transaction outcome", events)
			}
			if test.wantChange {
				changed := decodeChangedOutbox(t, events[0])
				if len(changed.Changes) != 1 || changed.Changes[0].Path != test.claim.Path {
					t.Fatalf("changed event = %#v, want one change for %q", changed, test.claim.Path)
				}
			} else if events[0] != nil {
				t.Fatalf("outbox event = %#v, want nil", events[0])
			}
		})
	}
}

func TestVerifyReceiptRestoresClaimAndRefreshesReceipt(t *testing.T) {
	oldHTML := fixture(t, "old.html")
	currentHTML := fixture(t, "same.html")
	signer := testSigner(t)
	oldAnchor := testAnchor("Pro Plan", "#plan .title")
	oldReceipt, err := signer.Sign(receipts.Payload{
		URL:              testURL,
		Path:             "plan",
		Value:            json.RawMessage(`"Pro Plan"`),
		Anchor:           oldAnchor,
		ExtractorVersion: "extractor-7",
		IssuedAt:         oldFetchedAt,
	})
	if err != nil {
		t.Fatalf("Sign(old receipt) error = %v", err)
	}
	recorder := &fakeRecorder{}
	service := testService(t, oldHTML, observation(currentHTML, 200), signer, recorder, nil)

	response, err := service.Verify(context.Background(), models.VerifyRequest{Receipt: oldReceipt})
	if err != nil {
		t.Fatalf("Verify(receipt) error = %v", err)
	}
	if got := response.Results[0].Status; got != models.VerifyStatusConfirmed {
		t.Fatalf("status = %q, want confirmed", got)
	}
	rows := recorder.committedBatches()[0]
	if rows[0].OldReceipt != oldReceipt {
		t.Fatalf("old receipt was not retained in ledger row")
	}
	refreshed, err := signer.Verify(response.Results[0].Receipt)
	if err != nil {
		t.Fatalf("Verify(refreshed receipt) error = %v", err)
	}
	if refreshed.URL != testFinalURL || refreshed.Path != "plan" || refreshed.ExtractorVersion != "extractor-7" || refreshed.Anchor.SnapshotID != newSnapshotID {
		t.Fatalf("refreshed payload = %#v", refreshed)
	}
}

func TestVerifyReceiptURLCanonicalizationAndOptionalGuard(t *testing.T) {
	signer := testSigner(t)
	token, err := signer.Sign(receipts.Payload{
		URL:      "HTTPS://EXAMPLE.COM.:00443/pricing#old-fragment",
		Path:     "plan",
		Value:    json.RawMessage(`"Pro Plan"`),
		Anchor:   testAnchor("Pro Plan", "#plan .title"),
		IssuedAt: oldFetchedAt,
	})
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	recorder := &fakeRecorder{}
	service := testService(
		t,
		fixture(t, "old.html"),
		observation(fixture(t, "same.html"), 200),
		signer,
		recorder,
		nil,
	)

	response, err := service.Verify(context.Background(), models.VerifyRequest{
		URL:     "https://example.com:443/pricing#caller-fragment",
		Receipt: token,
	})
	if err != nil || response == nil {
		t.Fatalf("Verify(canonically equal receipt URL) = (%#v, %v), want success", response, err)
	}
	row := recorder.committedBatches()[0][0]
	if row.URL != testURL || row.FinalURL != testFinalURL {
		t.Fatalf("recorded URLs = (%q, %q), want (%q, %q)", row.URL, row.FinalURL, testURL, testFinalURL)
	}

	before := recorder.callCount()
	response, err = service.Verify(context.Background(), models.VerifyRequest{
		URL:     "https://other.example/pricing",
		Receipt: token,
	})
	if response != nil || !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("Verify(mismatched URL guard) = (%#v, %v), want nil + ErrInvalidReceipt", response, err)
	}
	if recorder.callCount() != before {
		t.Fatal("mismatched receipt URL reached ledger")
	}
}

func TestVerifyRejectsSignedNonHTTPURLBeforeRevisit(t *testing.T) {
	signer := testSigner(t)
	token, err := signer.Sign(receipts.Payload{
		URL:      "file:///private/etc/passwd",
		Path:     "plan",
		Value:    json.RawMessage(`"Pro Plan"`),
		Anchor:   testAnchor("Pro Plan", "#plan .title"),
		IssuedAt: oldFetchedAt,
	})
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	var revisits atomic.Int32
	service := mustService(t, Config{
		Revisitor: revisitorFunc(func(context.Context, string) (RevisitResult, error) {
			revisits.Add(1)
			return RevisitResult{}, nil
		}),
		Snapshots: snapshotReaderFunc(func(snapshot.ID) ([]byte, snapshot.Meta, error) {
			return nil, snapshot.Meta{}, errors.New("unexpected snapshot call")
		}),
		Receipts: signer,
		Recorder: &fakeRecorder{},
	})

	response, err := service.Verify(context.Background(), models.VerifyRequest{Receipt: token})
	if response != nil || !errors.Is(err, ErrInvalidReceipt) {
		t.Fatalf("Verify(non-HTTP receipt URL) = (%#v, %v), want nil + ErrInvalidReceipt", response, err)
	}
	if revisits.Load() != 0 {
		t.Fatalf("invalid signed URL caused %d revisits", revisits.Load())
	}
}

func TestVerifyReceiptOnlyRequestJSONOmitsURL(t *testing.T) {
	encoded, err := json.Marshal(models.VerifyRequest{Receipt: "header.payload.signature"})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if strings.Contains(string(encoded), `"url"`) || string(encoded) != `{"receipt":"header.payload.signature"}` {
		t.Fatalf("receipt-only request JSON = %s", encoded)
	}
}

func TestVerifyRejectsBadReceiptBeforeRevisit(t *testing.T) {
	signer := testSigner(t)
	token, err := signer.Sign(receipts.Payload{
		URL:      testURL,
		Path:     "plan",
		Value:    json.RawMessage(`"Pro Plan"`),
		Anchor:   testAnchor("Pro Plan", "#plan .title"),
		IssuedAt: oldFetchedAt,
	})
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	token = token[:len(token)-1] + differentLastByte(token[len(token)-1])
	var revisitCalls atomic.Int32
	recorder := &fakeRecorder{}
	service := mustService(t, Config{
		Revisitor: revisitorFunc(func(context.Context, string) (RevisitResult, error) {
			revisitCalls.Add(1)
			return observation(fixture(t, "same.html"), 200), nil
		}),
		Snapshots: snapshotReaderFunc(func(snapshot.ID) ([]byte, snapshot.Meta, error) {
			return []byte(fixture(t, "old.html")), snapshot.Meta{}, nil
		}),
		Receipts: signer,
		Recorder: recorder,
	})

	response, err := service.Verify(context.Background(), models.VerifyRequest{URL: testURL, Receipt: token})
	if !errors.Is(err, ErrInvalidReceipt) || response != nil {
		t.Fatalf("Verify(bad receipt) = (%#v, %v), want nil + ErrInvalidReceipt", response, err)
	}
	if revisitCalls.Load() != 0 || recorder.callCount() != 0 {
		t.Fatalf("bad receipt reached side effects: revisits=%d records=%d", revisitCalls.Load(), recorder.callCount())
	}
}

func TestVerifyClassifiesSignedButUnverifiableFactsAsInvalidReceipts(t *testing.T) {
	signer := testSigner(t)
	tests := []struct {
		name   string
		value  json.RawMessage
		anchor evidence.Anchor
	}{
		{
			name:   "null scalar",
			value:  json.RawMessage(`null`),
			anchor: testAnchor("null", "#value"),
		},
		{
			name:  "unlocated anchor",
			value: json.RawMessage(`"value"`),
			anchor: evidence.Anchor{
				Method:     evidence.MethodUnlocated,
				SnapshotID: oldSnapshotID,
				FetchedAt:  oldFetchedAt,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			token, err := signer.Sign(receipts.Payload{
				URL:      testURL,
				Path:     "value",
				Value:    test.value,
				Anchor:   test.anchor,
				IssuedAt: oldFetchedAt,
			})
			if err != nil {
				t.Fatalf("Sign() error = %v", err)
			}
			var revisits atomic.Int32
			recorder := &fakeRecorder{}
			service := mustService(t, Config{
				Revisitor: revisitorFunc(func(context.Context, string) (RevisitResult, error) {
					revisits.Add(1)
					return RevisitResult{}, errors.New("unexpected revisit")
				}),
				Snapshots: snapshotReaderFunc(func(snapshot.ID) ([]byte, snapshot.Meta, error) {
					return nil, snapshot.Meta{}, errors.New("unexpected snapshot read")
				}),
				Receipts: signer,
				Recorder: recorder,
			})

			response, err := service.Verify(context.Background(), models.VerifyRequest{Receipt: token})
			if response != nil || !errors.Is(err, ErrInvalidReceipt) {
				t.Fatalf("Verify() = (%#v, %v), want nil + ErrInvalidReceipt", response, err)
			}
			if revisits.Load() != 0 || recorder.callCount() != 0 {
				t.Fatalf("unverifiable receipt reached side effects: revisits=%d records=%d", revisits.Load(), recorder.callCount())
			}
		})
	}
}

func TestVerifyEnforcesStrictInputXOR(t *testing.T) {
	var revisitCalls atomic.Int32
	service := mustService(t, Config{
		Revisitor: revisitorFunc(func(context.Context, string) (RevisitResult, error) {
			revisitCalls.Add(1)
			return RevisitResult{}, nil
		}),
		Snapshots: snapshotReaderFunc(func(snapshot.ID) ([]byte, snapshot.Meta, error) {
			return nil, snapshot.Meta{}, errors.New("unexpected snapshot call")
		}),
		Receipts: testSigner(t),
		Recorder: &fakeRecorder{},
	})
	claim := testClaim("plan", `"Pro Plan"`, "Pro Plan", "#plan .title")
	tests := []models.VerifyRequest{
		{URL: testURL},
		{URL: testURL, Claims: []models.Claim{}, Receipt: " \t"},
		{URL: testURL, Claims: []models.Claim{claim}, Receipt: "also-present"},
	}
	for index, request := range tests {
		if response, err := service.Verify(context.Background(), request); !errors.Is(err, ErrInvalidRequest) || response != nil {
			t.Fatalf("case %d: Verify() = (%#v, %v), want nil + ErrInvalidRequest", index, response, err)
		}
	}
	if revisitCalls.Load() != 0 {
		t.Fatalf("invalid XOR caused %d revisits", revisitCalls.Load())
	}
}

func TestVerifyRejectsNonScalarAndUnlocatedClaims(t *testing.T) {
	base := testClaim("field", `"value"`, "value", "#field")
	tests := []struct {
		name  string
		value json.RawMessage
		alter func(*models.Claim)
	}{
		{name: "null", value: json.RawMessage(`null`)},
		{name: "array", value: json.RawMessage(`[1]`)},
		{name: "object", value: json.RawMessage(`{"value":1}`)},
		{name: "multiple JSON values", value: json.RawMessage(`1 2`)},
		{
			name:  "unlocated anchor",
			value: json.RawMessage(`"value"`),
			alter: func(claim *models.Claim) { claim.Anchor.Method = evidence.MethodUnlocated },
		},
		{
			name:  "anchor without snapshot",
			value: json.RawMessage(`"value"`),
			alter: func(claim *models.Claim) { claim.Anchor.SnapshotID = "" },
		},
		{
			name:  "invalid selector",
			value: json.RawMessage(`"value"`),
			alter: func(claim *models.Claim) { claim.Anchor.Selector = "[" },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claim := base
			claim.Value = test.value
			if test.alter != nil {
				test.alter(&claim)
			}
			var revisitCalls atomic.Int32
			service := mustService(t, Config{
				Revisitor: revisitorFunc(func(context.Context, string) (RevisitResult, error) {
					revisitCalls.Add(1)
					return RevisitResult{}, nil
				}),
				Snapshots: snapshotReaderFunc(func(snapshot.ID) ([]byte, snapshot.Meta, error) {
					return nil, snapshot.Meta{}, errors.New("unexpected snapshot call")
				}),
				Receipts: testSigner(t),
				Recorder: &fakeRecorder{},
			})
			response, err := service.Verify(context.Background(), models.VerifyRequest{URL: testURL, Claims: []models.Claim{claim}})
			if !errors.Is(err, ErrInvalidClaim) || response != nil {
				t.Fatalf("Verify() = (%#v, %v), want nil + ErrInvalidClaim", response, err)
			}
			if revisitCalls.Load() != 0 {
				t.Fatalf("invalid claim caused %d revisits", revisitCalls.Load())
			}
		})
	}
}

func TestVerifyTreatsTransientHTTPAndNetworkFailuresAsRequestErrors(t *testing.T) {
	oldHTML := fixture(t, "old.html")
	for _, statusCode := range []int{403, 429, 500, 503} {
		t.Run(fmt.Sprintf("status_%d", statusCode), func(t *testing.T) {
			recorder := &fakeRecorder{}
			service := mustService(t, Config{
				Revisitor: revisitorFunc(func(context.Context, string) (RevisitResult, error) {
					return observation("denied", statusCode), nil
				}),
				Snapshots: snapshotReaderFunc(func(snapshot.ID) ([]byte, snapshot.Meta, error) {
					return []byte(oldHTML), snapshot.Meta{
						URL:        testURL,
						FetchedAt:  oldFetchedAt,
						StatusCode: 200,
					}, nil
				}),
				Receipts: testSigner(t),
				Recorder: recorder,
			})
			response, err := service.Verify(context.Background(), models.VerifyRequest{
				URL:    testURL,
				Claims: []models.Claim{testClaim("plan", `"Pro Plan"`, "Pro Plan", "#plan .title")},
			})
			var statusErr *HTTPStatusError
			if response != nil || !errors.Is(err, ErrRevisitStatus) || !errors.As(err, &statusErr) || statusErr.StatusCode != statusCode {
				t.Fatalf("Verify(status %d) = (%#v, %v), want matching HTTPStatusError", statusCode, response, err)
			}
			if recorder.callCount() != 0 {
				t.Fatalf("status error reached ledger: records=%d", recorder.callCount())
			}
		})
	}

	networkErr := errors.New("dial failed")
	recorder := &fakeRecorder{}
	service := mustService(t, Config{
		Revisitor: revisitorFunc(func(context.Context, string) (RevisitResult, error) {
			return RevisitResult{}, networkErr
		}),
		Snapshots: snapshotReaderFunc(func(snapshot.ID) ([]byte, snapshot.Meta, error) {
			return []byte(oldHTML), snapshot.Meta{
				URL:        testURL,
				FetchedAt:  oldFetchedAt,
				StatusCode: 200,
			}, nil
		}),
		Receipts: testSigner(t),
		Recorder: recorder,
	})
	response, err := service.Verify(context.Background(), models.VerifyRequest{
		URL:    testURL,
		Claims: []models.Claim{testClaim("plan", `"Pro Plan"`, "Pro Plan", "#plan .title")},
	})
	if response != nil || !errors.Is(err, ErrRevisit) || !errors.Is(err, networkErr) {
		t.Fatalf("Verify(network failure) = (%#v, %v), want wrapped network request error", response, err)
	}
	if recorder.callCount() != 0 {
		t.Fatal("network failure reached ledger")
	}
}

func TestVerifyOldSnapshotProvenanceUsesObservationHistory(t *testing.T) {
	store, err := snapshot.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	t.Cleanup(store.Close)

	oldHTML := fixture(t, "old.html")
	oldID, err := store.Put([]byte(oldHTML), snapshot.Meta{
		URL:        "HTTPS://EXAMPLE.COM.:443/pricing#capture-fragment",
		FetchedAt:  oldFetchedAt,
		StatusCode: 200,
	})
	if err != nil {
		t.Fatalf("Put(old snapshot) error = %v", err)
	}
	if _, err := store.Put([]byte(oldHTML), snapshot.Meta{
		URL:        "https://mirror.example/copy",
		FetchedAt:  oldFetchedAt.Add(time.Hour),
		StatusCode: 200,
	}); err != nil {
		t.Fatalf("Put(deduplicated observation) error = %v", err)
	}
	currentHTML := oldHTML
	currentID, err := store.Put([]byte(currentHTML), snapshot.Meta{
		URL:        testFinalURL,
		FetchedAt:  newFetchedAt,
		StatusCode: 200,
	})
	if err != nil {
		t.Fatalf("Put(current snapshot) error = %v", err)
	}
	if currentID != oldID {
		t.Fatalf("CAS IDs = (%q, %q), want one shared content ID", oldID, currentID)
	}
	current := observation(currentHTML, 200)
	current.SnapshotID = string(currentID)
	claim := testClaim("plan", `"Pro Plan"`, "Pro Plan", "#plan .title")
	claim.Anchor.SnapshotID = string(oldID)
	recorder := &fakeRecorder{}
	service := mustService(t, Config{
		Revisitor: revisitorFunc(func(_ context.Context, target string) (RevisitResult, error) {
			if target != testURL {
				return RevisitResult{}, fmt.Errorf("target = %q, want %q", target, testURL)
			}
			return current, nil
		}),
		Snapshots: store,
		Receipts:  testSigner(t),
		Recorder:  recorder,
	})

	response, err := service.Verify(context.Background(), models.VerifyRequest{
		URL:    "https://example.com:443/pricing#request-fragment",
		Claims: []models.Claim{claim},
	})
	if err != nil || response == nil {
		t.Fatalf("Verify() = (%#v, %v), want success from historical matching observation", response, err)
	}
	if response.URL != testURL || response.SnapshotID != string(currentID) || recorder.callCount() != 1 {
		t.Fatalf("response/ledger = (%#v, %d)", response, recorder.callCount())
	}
}

func TestVerifyRejectsOldSnapshotWithoutMatchingObservation(t *testing.T) {
	tests := []struct {
		name string
		meta snapshot.Meta
	}{
		{
			name: "different URL",
			meta: snapshot.Meta{URL: "https://other.example/pricing", FetchedAt: oldFetchedAt, StatusCode: 200},
		},
		{
			name: "different fetch time",
			meta: snapshot.Meta{URL: testURL, FetchedAt: oldFetchedAt.Add(time.Second), StatusCode: 200},
		},
		{
			name: "non-successful observation",
			meta: snapshot.Meta{URL: testURL, FetchedAt: oldFetchedAt, StatusCode: 404},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, err := snapshot.NewStore(t.TempDir())
			if err != nil {
				t.Fatalf("NewStore() error = %v", err)
			}
			t.Cleanup(store.Close)
			oldID, err := store.Put([]byte(fixture(t, "old.html")), test.meta)
			if err != nil {
				t.Fatalf("Put(old snapshot) error = %v", err)
			}
			currentHTML := fixture(t, "same.html")
			currentID, err := store.Put([]byte(currentHTML), snapshot.Meta{
				URL:        testFinalURL,
				FetchedAt:  newFetchedAt,
				StatusCode: 200,
			})
			if err != nil {
				t.Fatalf("Put(current snapshot) error = %v", err)
			}
			current := observation(currentHTML, 200)
			current.SnapshotID = string(currentID)
			claim := testClaim("plan", `"Pro Plan"`, "Pro Plan", "#plan .title")
			claim.Anchor.SnapshotID = string(oldID)
			codec := &countingReceiptCodec{delegate: testSigner(t)}
			recorder := &fakeRecorder{}
			var revisits atomic.Int32
			service := mustService(t, Config{
				Revisitor: revisitorFunc(func(context.Context, string) (RevisitResult, error) {
					revisits.Add(1)
					return current, nil
				}),
				Snapshots: store,
				Receipts:  codec,
				Recorder:  recorder,
			})

			response, err := service.Verify(context.Background(), models.VerifyRequest{URL: testURL, Claims: []models.Claim{claim}})
			if response != nil || !errors.Is(err, ErrSnapshot) {
				t.Fatalf("Verify() = (%#v, %v), want nil + ErrSnapshot", response, err)
			}
			if revisits.Load() != 0 || codec.signCalls.Load() != 0 || recorder.callCount() != 0 {
				t.Fatalf("provenance failure reached side effects: revisits=%d signs=%d records=%d", revisits.Load(), codec.signCalls.Load(), recorder.callCount())
			}
		})
	}
}

func TestVerifyRejectsInconsistentCurrentSnapshotProvenance(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*RevisitResult)
		wantErr error
	}{
		{
			name:    "body does not match snapshot",
			mutate:  func(result *RevisitResult) { result.RawHTML += "<!-- tampered -->" },
			wantErr: ErrSnapshot,
		},
		{
			name:    "final URL has no snapshot observation",
			mutate:  func(result *RevisitResult) { result.FinalURL = "https://redirected.example/pricing" },
			wantErr: ErrSnapshot,
		},
		{
			name:    "fetch time has no snapshot observation",
			mutate:  func(result *RevisitResult) { result.FetchedAt = result.FetchedAt.Add(time.Second) },
			wantErr: ErrSnapshot,
		},
		{
			name:    "status has no snapshot observation",
			mutate:  func(result *RevisitResult) { result.StatusCode = 201 },
			wantErr: ErrSnapshot,
		},
		{
			name: "snapshot does not exist",
			mutate: func(result *RevisitResult) {
				result.SnapshotID = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
			},
			wantErr: ErrSnapshot,
		},
		{
			name:    "successful response has no body",
			mutate:  func(result *RevisitResult) { result.RawHTML = "" },
			wantErr: ErrRevisit,
		},
		{
			name:    "successful response has no snapshot",
			mutate:  func(result *RevisitResult) { result.SnapshotID = "" },
			wantErr: ErrRevisit,
		},
		{
			name:    "successful response has malformed snapshot",
			mutate:  func(result *RevisitResult) { result.SnapshotID = "sha256:not-a-digest" },
			wantErr: ErrRevisit,
		},
		{
			name:    "unsafe final URL",
			mutate:  func(result *RevisitResult) { result.FinalURL = "file:///private/tmp/page" },
			wantErr: ErrRevisit,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, err := snapshot.NewStore(t.TempDir())
			if err != nil {
				t.Fatalf("NewStore() error = %v", err)
			}
			t.Cleanup(store.Close)
			oldHTML := fixture(t, "old.html")
			oldID, err := store.Put([]byte(oldHTML), snapshot.Meta{URL: testURL, FetchedAt: oldFetchedAt, StatusCode: 200})
			if err != nil {
				t.Fatalf("Put(old snapshot) error = %v", err)
			}
			currentHTML := fixture(t, "same.html")
			currentID, err := store.Put([]byte(currentHTML), snapshot.Meta{
				URL:        testFinalURL,
				FetchedAt:  newFetchedAt,
				StatusCode: 200,
			})
			if err != nil {
				t.Fatalf("Put(current snapshot) error = %v", err)
			}
			current := observation(currentHTML, 200)
			current.SnapshotID = string(currentID)
			test.mutate(&current)
			claim := testClaim("plan", `"Pro Plan"`, "Pro Plan", "#plan .title")
			claim.Anchor.SnapshotID = string(oldID)
			codec := &countingReceiptCodec{delegate: testSigner(t)}
			recorder := &fakeRecorder{}
			service := mustService(t, Config{
				Revisitor: revisitorFunc(func(context.Context, string) (RevisitResult, error) { return current, nil }),
				Snapshots: store,
				Receipts:  codec,
				Recorder:  recorder,
			})

			response, err := service.Verify(context.Background(), models.VerifyRequest{URL: testURL, Claims: []models.Claim{claim}})
			if response != nil || !errors.Is(err, test.wantErr) {
				t.Fatalf("Verify() = (%#v, %v), want nil + %v", response, err, test.wantErr)
			}
			if codec.signCalls.Load() != 0 || recorder.callCount() != 0 {
				t.Fatalf("invalid current snapshot reached signer/ledger: signs=%d records=%d", codec.signCalls.Load(), recorder.callCount())
			}
		})
	}
}

func TestVerifyDoesNotUseAmbiguousSelectorOrFuzzyQuoteAsProof(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		oldHTML string
		claim   models.Claim
		assert  func(*testing.T, string, string)
	}{
		{
			name:    "ambiguous selector",
			fixture: "ambiguous.html",
			claim:   testClaim("price", `1299`, "The archived amount was exactly 1,299 dollars.", ".price"),
		},
		{
			name:    "fuzzy quote only",
			fixture: "fuzzy-only.html",
			oldHTML: `<html><body><span id="removed">alpha</span></body></html>`,
			claim: testClaim(
				"summary",
				`"alpha"`,
				"alpha beta gamma delta epsilon zeta eta theta iota kappa",
				"#removed",
			),
			assert: func(t *testing.T, html, text string) {
				t.Helper()
				anchor := evidence.AlignValue(
					"alpha beta gamma delta epsilon zeta eta theta iota kappa",
					text,
					html,
				)
				if anchor.Method != evidence.MethodFuzzy {
					t.Fatalf("fixture precondition method = %q, want fuzzy", anchor.Method)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			html := fixture(t, test.fixture)
			text := htmlText(t, html)
			if test.assert != nil {
				test.assert(t, html, text)
			}
			recorder := &fakeRecorder{}
			oldHTML := test.oldHTML
			if oldHTML == "" {
				oldHTML = fixture(t, "old.html")
			}
			service := testService(t, oldHTML, RevisitResult{
				StatusCode: 200,
				FinalURL:   testFinalURL,
				RawHTML:    html,
				SnapshotID: newSnapshotID,
				FetchedAt:  newFetchedAt,
			}, testSigner(t), recorder, nil)
			response, err := service.Verify(context.Background(), models.VerifyRequest{URL: testURL, Claims: []models.Claim{test.claim}})
			if err != nil {
				t.Fatalf("Verify() error = %v", err)
			}
			result := response.Results[0]
			if result.Status != models.VerifyStatusGone || result.GoneScope != models.VerifyGoneScopeField {
				t.Fatalf("result = %#v, want field gone", result)
			}
			if recorder.committedBatches()[0][0].Outcome != ledger.OutcomeGone {
				t.Fatal("field gone was not recorded")
			}
		})
	}
}

func TestVerifyUsesOldJSONTypeForSelectorExtraction(t *testing.T) {
	tests := []struct {
		name         string
		fixture      string
		claim        models.Claim
		wantStatus   models.VerifyStatus
		wantNewValue json.RawMessage
	}{
		{
			name:       "thousands formatted number is equal",
			fixture:    "same.html",
			claim:      testClaim("price", `1299`, "$1,299.00", "#plan .price"),
			wantStatus: models.VerifyStatusConfirmed,
		},
		{
			name:       "exponent number is equal",
			fixture:    "same.html",
			claim:      testClaim("price", `1.299e3`, "$1,299.00", "#plan .price"),
			wantStatus: models.VerifyStatusConfirmed,
		},
		{
			name:         "number changed",
			fixture:      "changed.html",
			claim:        testClaim("price", `1299`, "$1,299.00", "#plan .price"),
			wantStatus:   models.VerifyStatusChanged,
			wantNewValue: json.RawMessage(`1499.50`),
		},
		{
			name:       "boolean is case insensitive",
			fixture:    "same.html",
			claim:      testClaim("available", `true`, "true", "#plan .available"),
			wantStatus: models.VerifyStatusConfirmed,
		},
		{
			name:         "boolean changed",
			fixture:      "changed.html",
			claim:        testClaim("available", `true`, "true", "#plan .available"),
			wantStatus:   models.VerifyStatusChanged,
			wantNewValue: json.RawMessage(`false`),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			html := fixture(t, test.fixture)
			service := testService(t, fixture(t, "old.html"), observation(html, 200), testSigner(t), &fakeRecorder{}, nil)
			response, err := service.Verify(context.Background(), models.VerifyRequest{URL: testURL, Claims: []models.Claim{test.claim}})
			if err != nil {
				t.Fatalf("Verify() error = %v", err)
			}
			result := response.Results[0]
			if result.Status != test.wantStatus {
				t.Fatalf("status = %q, want %q", result.Status, test.wantStatus)
			}
			assertJSONEqual(t, result.NewValue, test.wantNewValue, "typed new value")
		})
	}
}

func TestVerifyStringNormalizationPreservesCurrencyAndPunctuation(t *testing.T) {
	tests := []struct {
		name        string
		oldValue    string
		oldQuote    string
		currentText string
	}{
		{name: "currency changed", oldValue: `"$100"`, oldQuote: "$100", currentText: "€100"},
		{name: "currency removed", oldValue: `"$100"`, oldQuote: "$100", currentText: "100"},
		{name: "comma removed from string", oldValue: `"1,000"`, oldQuote: "1,000", currentText: "1000"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			html := `<html><body><span id="value">` + test.currentText + `</span></body></html>`
			oldHTML := `<html><body><span id="value">` + test.oldQuote + `</span></body></html>`
			service := testService(t, oldHTML, observation(html, 200), testSigner(t), &fakeRecorder{}, nil)
			response, err := service.Verify(context.Background(), models.VerifyRequest{
				URL: testURL,
				Claims: []models.Claim{
					testClaim("value", test.oldValue, test.oldQuote, "#value"),
				},
			})
			if err != nil {
				t.Fatalf("Verify() error = %v", err)
			}
			result := response.Results[0]
			if result.Status != models.VerifyStatusChanged {
				t.Fatalf("status = %q, want changed for %s -> %s", result.Status, test.oldValue, test.currentText)
			}
			want, _ := json.Marshal(test.currentText)
			assertJSONEqual(t, result.NewValue, want, "changed string value")
		})
	}
}

func TestVerifyRejectsClaimWhoseOldAnchorDoesNotSupportItsValue(t *testing.T) {
	oldHTML := `<html><body><footer>Copyright 2026 Example</footer></body></html>`
	currentHTML := `<html><body><footer>Copyright 2026 Example</footer></body></html>`
	recorder := &fakeRecorder{}
	codec := &countingReceiptCodec{delegate: testSigner(t)}
	service := testService(t, oldHTML, observation(currentHTML, 200), codec, recorder, nil)

	response, err := service.Verify(context.Background(), models.VerifyRequest{
		URL: testURL,
		Claims: []models.Claim{testClaim(
			"price",
			`999`,
			"Copyright 2026 Example",
			"#removed-price",
		)},
	})
	if response != nil || !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("Verify(unbacked claim) = (%#v, %v), want nil + ErrInvalidClaim", response, err)
	}
	if codec.signCalls.Load() != 0 || recorder.callCount() != 0 {
		t.Fatalf("unbacked claim reached signer/ledger: signs=%d records=%d", codec.signCalls.Load(), recorder.callCount())
	}
}

func TestVerifyRejectsNumericSubstringInOldSnapshotBeforeRevisit(t *testing.T) {
	var revisits atomic.Int32
	codec := &countingReceiptCodec{delegate: testSigner(t)}
	recorder := &fakeRecorder{}
	service := mustService(t, Config{
		Revisitor: revisitorFunc(func(context.Context, string) (RevisitResult, error) {
			revisits.Add(1)
			return observation(`<span id="new-price">1000</span>`, 200), nil
		}),
		Snapshots: snapshotReaderFunc(func(snapshot.ID) ([]byte, snapshot.Meta, error) {
			return []byte(`<span id="old-price">1000</span>`), snapshot.Meta{
				URL:        testURL,
				FetchedAt:  oldFetchedAt,
				StatusCode: 200,
			}, nil
		}),
		Receipts: codec,
		Recorder: recorder,
	})
	claim := testClaim("price", `100`, "100", "#removed-price")

	response, err := service.Verify(context.Background(), models.VerifyRequest{URL: testURL, Claims: []models.Claim{claim}})
	if response != nil || !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("Verify(numeric substring) = (%#v, %v), want nil + ErrInvalidClaim", response, err)
	}
	if revisits.Load() != 0 || codec.signCalls.Load() != 0 || recorder.callCount() != 0 {
		t.Fatalf("invalid old evidence reached side effects: revisits=%d signs=%d records=%d", revisits.Load(), codec.signCalls.Load(), recorder.callCount())
	}
}

func TestVerifyQuoteFallbackFailsClosedForSubstringAndAmbiguity(t *testing.T) {
	tests := []struct {
		name        string
		currentHTML string
	}{
		{name: "numeric substring", currentHTML: `<span id="new-price">1000</span>`},
		{name: "ambiguous occurrences", currentHTML: `<span>100</span><span>100</span>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			codec := &countingReceiptCodec{delegate: testSigner(t)}
			recorder := &fakeRecorder{}
			service := testService(
				t,
				`<span id="price">100</span>`,
				observation(test.currentHTML, 200),
				codec,
				recorder,
				nil,
			)
			claim := testClaim("price", `100`, "100", "#price")

			response, err := service.Verify(context.Background(), models.VerifyRequest{URL: testURL, Claims: []models.Claim{claim}})
			if err != nil {
				t.Fatalf("Verify() error = %v", err)
			}
			result := response.Results[0]
			if result.Status != models.VerifyStatusGone || result.GoneScope != models.VerifyGoneScopeField || result.Receipt != "" {
				t.Fatalf("result = %#v, want unsigned field-gone", result)
			}
			if codec.signCalls.Load() != 0 || recorder.committedBatches()[0][0].Outcome != ledger.OutcomeGone {
				t.Fatalf("field-gone side effects: signs=%d rows=%#v", codec.signCalls.Load(), recorder.committedBatches())
			}
		})
	}
}

func TestVerifyQuoteFallbackDoesNotConfirmLongerStringToken(t *testing.T) {
	codec := &countingReceiptCodec{delegate: testSigner(t)}
	recorder := &fakeRecorder{}
	service := testService(
		t,
		`<span id="plan">Pro</span>`,
		observation(`<span id="renamed-plan">Pro-Max</span>`, 200),
		codec,
		recorder,
		nil,
	)
	claim := testClaim("plan", `"Pro"`, "Pro", "#plan")

	response, err := service.Verify(context.Background(), models.VerifyRequest{URL: testURL, Claims: []models.Claim{claim}})
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	result := response.Results[0]
	if result.Status != models.VerifyStatusGone || result.GoneScope != models.VerifyGoneScopeField || result.Receipt != "" {
		t.Fatalf("result = %#v, want unsigned field-gone", result)
	}
	if codec.signCalls.Load() != 0 || recorder.committedBatches()[0][0].Outcome != ledger.OutcomeGone {
		t.Fatalf("longer token side effects: signs=%d rows=%#v", codec.signCalls.Load(), recorder.committedBatches())
	}
}

func TestVerifyDoesNotFallbackFromAmbiguousTypedSelector(t *testing.T) {
	tests := []struct {
		name        string
		oldHTML     string
		currentHTML string
		claim       models.Claim
	}{
		{
			name:        "stale and current number",
			oldHTML:     `<span id="price">$100</span>`,
			currentHTML: `<span id="price"><del>$100</del><ins>$200</ins></span>`,
			claim:       testClaim("price", `100`, "$100", "#price"),
		},
		{
			name:        "stale and current boolean",
			oldHTML:     `<span id="available">true</span>`,
			currentHTML: `<span id="available"><del>true</del><ins>false</ins></span>`,
			claim:       testClaim("available", `true`, "true", "#available"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			codec := &countingReceiptCodec{delegate: testSigner(t)}
			recorder := &fakeRecorder{}
			service := testService(t, test.oldHTML, observation(test.currentHTML, 200), codec, recorder, nil)

			response, err := service.Verify(context.Background(), models.VerifyRequest{
				URL:    testURL,
				Claims: []models.Claim{test.claim},
			})
			if err != nil {
				t.Fatalf("Verify() error = %v", err)
			}
			result := response.Results[0]
			if result.Status != models.VerifyStatusGone || result.GoneScope != models.VerifyGoneScopeField || result.Receipt != "" {
				t.Fatalf("result = %#v, want unsigned field-gone", result)
			}
			if codec.signCalls.Load() != 0 || recorder.committedBatches()[0][0].Outcome != ledger.OutcomeGone {
				t.Fatalf("ambiguous selector side effects: signs=%d rows=%#v", codec.signCalls.Load(), recorder.committedBatches())
			}
		})
	}
}

func TestVerifyDoesNotUseHiddenOrSyntheticInlineTextAsProof(t *testing.T) {
	tests := []struct {
		name        string
		oldHTML     string
		currentHTML string
		claim       models.Claim
	}{
		{
			name:        "hidden stale string",
			oldHTML:     `<span id="plan">Pro Plan</span>`,
			currentHTML: `<span id="plan" hidden>Pro Plan</span><span id="new-plan">Max Plan</span>`,
			claim:       testClaim("plan", `"Pro Plan"`, "Pro Plan", "#plan"),
		},
		{
			name:        "aria-hidden stale string",
			oldHTML:     `<span id="plan">Pro Plan</span>`,
			currentHTML: `<span id="plan" aria-hidden="true">Pro Plan</span><span id="new-plan">Max Plan</span>`,
			claim:       testClaim("plan", `"Pro Plan"`, "Pro Plan", "#plan"),
		},
		{
			name:        "inline word continuation",
			oldHTML:     `<span id="plan">Pro</span>`,
			currentHTML: `<span id="new-plan"><b>Pro</b><i>fessional</i></span>`,
			claim:       testClaim("plan", `"Pro"`, "Pro", "#plan"),
		},
		{
			name:        "inline numeric continuation",
			oldHTML:     `<span id="price">10</span>`,
			currentHTML: `<span id="new-price"><b>10</b><i>0</i></span>`,
			claim:       testClaim("price", `10`, "10", "#price"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			codec := &countingReceiptCodec{delegate: testSigner(t)}
			recorder := &fakeRecorder{}
			service := testService(t, test.oldHTML, observation(test.currentHTML, 200), codec, recorder, nil)

			response, err := service.Verify(context.Background(), models.VerifyRequest{URL: testURL, Claims: []models.Claim{test.claim}})
			if err != nil {
				t.Fatalf("Verify() error = %v", err)
			}
			result := response.Results[0]
			if result.Status != models.VerifyStatusGone || result.GoneScope != models.VerifyGoneScopeField || result.Receipt != "" {
				t.Fatalf("result = %#v, want unsigned field-gone", result)
			}
			if codec.signCalls.Load() != 0 || recorder.committedBatches()[0][0].Outcome != ledger.OutcomeGone {
				t.Fatalf("non-renderable/continued text side effects: signs=%d rows=%#v", codec.signCalls.Load(), recorder.committedBatches())
			}
		})
	}
}

func TestVerifySelectorAnchorUsesSelectedCanonicalSpan(t *testing.T) {
	currentHTML := `<span id="unrelated">Pro</span> <span id="plan">Pro</span>`
	service := testService(
		t,
		`<span id="plan">Pro</span>`,
		observation(currentHTML, 200),
		testSigner(t),
		&fakeRecorder{},
		nil,
	)
	response, err := service.Verify(context.Background(), models.VerifyRequest{
		URL:    testURL,
		Claims: []models.Claim{testClaim("plan", `"Pro"`, "Pro", "#plan")},
	})
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	result := response.Results[0]
	if result.Status != models.VerifyStatusConfirmed || result.Evidence == nil {
		t.Fatalf("result = %#v, want confirmed evidence", result)
	}
	page, err := parseCanonicalPage(currentHTML)
	if err != nil {
		t.Fatalf("parseCanonicalPage() error = %v", err)
	}
	wantRange := [2]int{4, 7}
	if result.Evidence.TextRange != wantRange || page.text[wantRange[0]:wantRange[1]] != result.Evidence.Quote {
		t.Fatalf("anchor = %#v, canonical text=%q, want selected range %v", result.Evidence, page.text, wantRange)
	}
}

func TestVerifyDirectQuoteAnchorCanUseCanonicalOldRange(t *testing.T) {
	oldHTML := `<span>Pro</span> <span>Pro</span>`
	claim := testClaim("plan", `"Pro"`, "Pro", "")
	claim.Anchor.TextRange = [2]int{4, 7}
	service := testService(t, oldHTML, observation(`<span>Pro</span>`, 200), testSigner(t), &fakeRecorder{}, nil)

	response, err := service.Verify(context.Background(), models.VerifyRequest{URL: testURL, Claims: []models.Claim{claim}})
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if response.Results[0].Status != models.VerifyStatusConfirmed {
		t.Fatalf("result = %#v, want confirmed from range-disambiguated old anchor", response.Results[0])
	}
}

func TestVerifyDerivesCurrentTextFromVerifiedHTML(t *testing.T) {
	codec := &countingReceiptCodec{delegate: testSigner(t)}
	current := observation(`<span id="other">absent</span>`, 200)
	service := testService(
		t,
		`<span id="value">secret</span>`,
		current,
		codec,
		&fakeRecorder{},
		nil,
	)
	claim := testClaim("value", `"secret"`, "secret", "#value")

	response, err := service.Verify(context.Background(), models.VerifyRequest{URL: testURL, Claims: []models.Claim{claim}})
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	result := response.Results[0]
	if result.Status != models.VerifyStatusGone || result.GoneScope != models.VerifyGoneScopeField || result.Receipt != "" {
		t.Fatalf("result = %#v, want field-gone from RawHTML", result)
	}
	if codec.signCalls.Load() != 0 {
		t.Fatalf("absent verified HTML value caused %d receipt signatures", codec.signCalls.Load())
	}
}

func TestVerifyInvalidOldSnapshotDoesNotTriggerRevisit(t *testing.T) {
	var revisits atomic.Int32
	codec := &countingReceiptCodec{delegate: testSigner(t)}
	recorder := &fakeRecorder{}
	service := mustService(t, Config{
		Revisitor: revisitorFunc(func(context.Context, string) (RevisitResult, error) {
			revisits.Add(1)
			return RevisitResult{}, errors.New("unexpected revisit")
		}),
		Snapshots: snapshotReaderFunc(func(snapshot.ID) ([]byte, snapshot.Meta, error) {
			return nil, snapshot.Meta{}, errors.New("snapshot does not exist")
		}),
		Receipts: codec,
		Recorder: recorder,
	})

	response, err := service.Verify(context.Background(), models.VerifyRequest{
		URL:    testURL,
		Claims: []models.Claim{testClaim("plan", `"Pro Plan"`, "Pro Plan", "#plan .title")},
	})
	if response != nil || !errors.Is(err, ErrSnapshot) {
		t.Fatalf("Verify(missing old snapshot) = (%#v, %v), want nil + ErrSnapshot", response, err)
	}
	if revisits.Load() != 0 || codec.signCalls.Load() != 0 || recorder.callCount() != 0 {
		t.Fatalf("invalid snapshot reached side effects: revisits=%d signs=%d records=%d", revisits.Load(), codec.signCalls.Load(), recorder.callCount())
	}
}

func TestVerifyRejectsOversizedInputsBeforeExternalWork(t *testing.T) {
	baseClaim := testClaim("plan", `"Pro Plan"`, "Pro Plan", "#plan .title")
	tests := []struct {
		name    string
		request models.VerifyRequest
		wantErr error
	}{
		{
			name: "too many claims",
			request: models.VerifyRequest{
				URL:    testURL,
				Claims: make([]models.Claim, maximumClaims+1),
			},
			wantErr: ErrInvalidRequest,
		},
		{
			name:    "URL",
			request: models.VerifyRequest{URL: strings.Repeat("u", maximumURLBytes+1), Claims: []models.Claim{baseClaim}},
			wantErr: ErrInvalidRequest,
		},
		{
			name:    "receipt",
			request: models.VerifyRequest{Receipt: strings.Repeat("r", maximumReceiptBytes+1)},
			wantErr: ErrInvalidReceipt,
		},
		{
			name: "webhook URL",
			request: models.VerifyRequest{
				URL:        testURL,
				Claims:     []models.Claim{baseClaim},
				WebhookURL: "https://hooks.example/" + strings.Repeat("u", ledger.MaxOutboxURLBytes),
			},
			wantErr: ErrInvalidRequest,
		},
		{
			name: "webhook secret",
			request: models.VerifyRequest{
				URL:           testURL,
				Claims:        []models.Claim{baseClaim},
				WebhookURL:    "https://hooks.example/facts",
				WebhookSecret: strings.Repeat("s", maximumWebhookSecretBytes+1),
			},
			wantErr: ErrInvalidRequest,
		},
		{
			name: "webhook secret without URL",
			request: models.VerifyRequest{
				URL:           testURL,
				Claims:        []models.Claim{baseClaim},
				WebhookSecret: "secret",
			},
			wantErr: ErrInvalidRequest,
		},
		{
			name: "webhook non-HTTP URL",
			request: models.VerifyRequest{
				URL:        testURL,
				Claims:     []models.Claim{baseClaim},
				WebhookURL: "file:///tmp/fact",
			},
			wantErr: ErrInvalidRequest,
		},
		{
			name: "path",
			request: models.VerifyRequest{URL: testURL, Claims: []models.Claim{func() models.Claim {
				claim := baseClaim
				claim.Path = strings.Repeat("p", maximumPathBytes+1)
				return claim
			}()}},
			wantErr: ErrInvalidClaim,
		},
		{
			name: "value",
			request: models.VerifyRequest{URL: testURL, Claims: []models.Claim{func() models.Claim {
				claim := baseClaim
				claim.Value = json.RawMessage(strings.Repeat("v", maximumScalarBytes+1))
				return claim
			}()}},
			wantErr: ErrInvalidClaim,
		},
		{
			name: "quote",
			request: models.VerifyRequest{URL: testURL, Claims: []models.Claim{func() models.Claim {
				claim := baseClaim
				claim.Anchor.Quote = strings.Repeat("q", maximumQuoteBytes+1)
				return claim
			}()}},
			wantErr: ErrInvalidClaim,
		},
		{
			name: "selector",
			request: models.VerifyRequest{URL: testURL, Claims: []models.Claim{func() models.Claim {
				claim := baseClaim
				claim.Anchor.Selector = strings.Repeat("s", maximumSelectorBytes+1)
				return claim
			}()}},
			wantErr: ErrInvalidClaim,
		},
		{
			name: "aggregate claims",
			request: models.VerifyRequest{URL: testURL, Claims: func() []models.Claim {
				claims := make([]models.Claim, maximumClaims)
				for index := range claims {
					claims[index] = baseClaim
					claims[index].Path = fmt.Sprintf("path-%03d", index)
					claims[index].Value = json.RawMessage(strconv.Quote(strings.Repeat("v", 5300)))
				}
				return claims
			}()},
			wantErr: ErrInvalidRequest,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var revisits atomic.Int32
			codec := &countingReceiptCodec{delegate: testSigner(t)}
			recorder := &fakeRecorder{}
			service := mustService(t, Config{
				Revisitor: revisitorFunc(func(context.Context, string) (RevisitResult, error) {
					revisits.Add(1)
					return RevisitResult{}, errors.New("unexpected revisit")
				}),
				Snapshots: snapshotReaderFunc(func(snapshot.ID) ([]byte, snapshot.Meta, error) {
					return nil, snapshot.Meta{}, errors.New("unexpected snapshot read")
				}),
				Receipts: codec,
				Recorder: recorder,
			})

			response, err := service.Verify(context.Background(), test.request)
			if response != nil || !errors.Is(err, test.wantErr) {
				t.Fatalf("Verify() = (%#v, %v), want nil + %v", response, err, test.wantErr)
			}
			if revisits.Load() != 0 || codec.verifyCalls.Load() != 0 || recorder.callCount() != 0 {
				t.Fatalf("oversized input reached dependencies: revisits=%d verifies=%d records=%d", revisits.Load(), codec.verifyCalls.Load(), recorder.callCount())
			}
		})
	}
}

func TestVerifyRejectsOversizedSnapshotBodies(t *testing.T) {
	t.Run("old snapshot before revisit", func(t *testing.T) {
		var revisits atomic.Int32
		service := mustService(t, Config{
			Revisitor: revisitorFunc(func(context.Context, string) (RevisitResult, error) {
				revisits.Add(1)
				return RevisitResult{}, errors.New("unexpected revisit")
			}),
			Snapshots: snapshotReaderFunc(func(snapshot.ID) ([]byte, snapshot.Meta, error) {
				return []byte(strings.Repeat("x", maximumPageBytes+1)), snapshot.Meta{
					URL:        testURL,
					FetchedAt:  oldFetchedAt,
					StatusCode: 200,
				}, nil
			}),
			Receipts: testSigner(t),
			Recorder: &fakeRecorder{},
		})
		response, err := service.Verify(context.Background(), models.VerifyRequest{
			URL:    testURL,
			Claims: []models.Claim{testClaim("plan", `"Pro Plan"`, "Pro Plan", "#plan .title")},
		})
		if response != nil || !errors.Is(err, ErrSnapshot) || revisits.Load() != 0 {
			t.Fatalf("Verify(oversized old) = (%#v, %v), revisits=%d", response, err, revisits.Load())
		}
	})

	t.Run("current snapshot after revisit", func(t *testing.T) {
		current := observation(strings.Repeat("x", maximumPageBytes+1), 200)
		service := testService(t, fixture(t, "old.html"), current, testSigner(t), &fakeRecorder{}, nil)
		response, err := service.Verify(context.Background(), models.VerifyRequest{
			URL:    testURL,
			Claims: []models.Claim{testClaim("plan", `"Pro Plan"`, "Pro Plan", "#plan .title")},
		})
		if response != nil || !errors.Is(err, ErrRevisit) {
			t.Fatalf("Verify(oversized current) = (%#v, %v), want nil + ErrRevisit", response, err)
		}
	})
}

func TestVerifyRecordsAllClaimsAndChangedEventAtomically(t *testing.T) {
	var orderMu sync.Mutex
	order := make([]string, 0, 1)
	recorder := &fakeRecorder{before: func() {
		orderMu.Lock()
		order = append(order, "record")
		orderMu.Unlock()
	}}
	service := testService(
		t,
		fixture(t, "old.html"),
		observation(fixture(t, "changed.html"), 200),
		testSigner(t),
		recorder,
		nil,
	)
	request := models.VerifyRequest{
		URL:           testURL,
		WebhookURL:    "HTTPS://Hooks.Example.Test:443/facts#fragment",
		WebhookSecret: "top-secret",
		Claims: []models.Claim{
			testClaim("plan", `"Pro Plan"`, "Pro Plan", "#plan .title"),
			testClaim("description", `"Durable fact quote remains exactly here"`, "Durable fact quote remains exactly here", ".durable"),
		},
	}

	response, err := service.Verify(context.Background(), request)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	orderMu.Lock()
	gotOrder := append([]string(nil), order...)
	orderMu.Unlock()
	if !reflect.DeepEqual(gotOrder, []string{"record"}) {
		t.Fatalf("side-effect order = %v, want one atomic record call", gotOrder)
	}
	batches := recorder.committedBatches()
	if len(batches) != 1 || len(batches[0]) != 2 {
		t.Fatalf("ledger batches = %#v, want one two-row transaction", batches)
	}
	if batches[0][0].VerificationID != response.VerificationID || batches[0][1].VerificationID != response.VerificationID {
		t.Fatal("claims were not grouped under one verification ID")
	}
	events := recorder.committedOutboxEvents()
	if len(events) != 1 || events[0] == nil {
		t.Fatalf("outbox events = %#v, want one durable event", events)
	}
	event := events[0]
	if event.ID != response.VerificationID || event.VerificationID != response.VerificationID ||
		event.Type != factChangedEventType || event.URL != "https://hooks.example.test/facts" || event.Secret != "top-secret" ||
		!event.CreatedAt.Equal(response.VerifiedAt) || !event.NextAttemptAt.Equal(response.VerifiedAt) {
		t.Fatalf("outbox event metadata = %#v", event)
	}
	changed := decodeChangedOutbox(t, event)
	if len(changed.Changes) != 1 || changed.Changes[0].Path != "plan" {
		t.Fatalf("changed event = %#v, want only changed plan", changed)
	}
}

func TestVerifyLedgerFailureRollsBackVerdictsAndOutbox(t *testing.T) {
	ledgerErr := errors.New("transaction rolled back")
	var orderMu sync.Mutex
	order := make([]string, 0, 1)
	recorder := &fakeRecorder{
		err: ledgerErr,
		before: func() {
			orderMu.Lock()
			order = append(order, "record")
			orderMu.Unlock()
		},
	}
	service := testService(t, fixture(t, "old.html"), observation(fixture(t, "changed.html"), 200), testSigner(t), recorder, nil)
	request := models.VerifyRequest{
		URL:        testURL,
		WebhookURL: "https://hooks.example.test/facts",
		Claims: []models.Claim{
			testClaim("plan", `"Pro Plan"`, "Pro Plan", "#plan .title"),
			testClaim("price", `1299`, "$1,299.00", "#plan .price"),
		},
	}

	response, err := service.Verify(context.Background(), request)
	if response != nil || !errors.Is(err, ErrRecord) || !errors.Is(err, ledgerErr) {
		t.Fatalf("Verify(ledger failure) = (%#v, %v), want nil + wrapped ledger error", response, err)
	}
	if recorder.callCount() != 1 || len(recorder.lastAttempt()) != 2 || len(recorder.committedBatches()) != 0 {
		t.Fatalf("recorder state calls=%d attempted=%d committed=%d", recorder.callCount(), len(recorder.lastAttempt()), len(recorder.committedBatches()))
	}
	if recorder.lastAttemptedOutboxEvent() == nil || len(recorder.committedOutboxEvents()) != 0 {
		t.Fatalf("outbox transaction state attempted=%#v committed=%#v", recorder.lastAttemptedOutboxEvent(), recorder.committedOutboxEvents())
	}
	orderMu.Lock()
	gotOrder := append([]string(nil), order...)
	orderMu.Unlock()
	if !reflect.DeepEqual(gotOrder, []string{"record"}) {
		t.Fatalf("side-effect order = %v, want only record", gotOrder)
	}
}

func TestVerifyChangedWithoutWebhookDoesNotCreateOutbox(t *testing.T) {
	recorder := &fakeRecorder{}
	service := testService(
		t,
		fixture(t, "old.html"),
		observation(fixture(t, "changed.html"), 200),
		testSigner(t),
		recorder,
		nil,
	)
	response, err := service.Verify(context.Background(), models.VerifyRequest{
		URL:    testURL,
		Claims: []models.Claim{testClaim("plan", `"Pro Plan"`, "Pro Plan", "#plan .title")},
	})
	if err != nil || response == nil || response.Results[0].Status != models.VerifyStatusChanged {
		t.Fatalf("Verify() = (%#v, %v), want changed success", response, err)
	}
	events := recorder.committedOutboxEvents()
	if len(events) != 1 || events[0] != nil {
		t.Fatalf("outbox events = %#v, want one nil transaction event", events)
	}
}

func TestChangedEventJSONUsesStableSnakeCaseFields(t *testing.T) {
	event := ChangedEvent{
		VerificationID: "verification-json",
		URL:            testURL,
		FinalURL:       testFinalURL,
		VerifiedAt:     newFetchedAt.UTC(),
		Changes: []FactChange{{
			Path:     "plan",
			OldValue: json.RawMessage(`"Pro Plan"`),
			NewValue: json.RawMessage(`"Enterprise Plan"`),
			Evidence: testAnchor("Enterprise Plan", "#plan .title"),
			Receipt:  "header.payload.signature",
		}},
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &outer); err != nil {
		t.Fatalf("Unmarshal(event) error = %v", err)
	}
	wantOuter := []string{"verification_id", "url", "final_url", "verified_at", "changes"}
	if len(outer) != len(wantOuter) {
		t.Fatalf("event JSON = %s", encoded)
	}
	for _, key := range wantOuter {
		if _, ok := outer[key]; !ok {
			t.Fatalf("event JSON missing %q: %s", key, encoded)
		}
	}
	var changes []map[string]json.RawMessage
	if err := json.Unmarshal(outer["changes"], &changes); err != nil || len(changes) != 1 {
		t.Fatalf("decode changes = (%#v, %v)", changes, err)
	}
	wantChange := []string{"path", "old_value", "new_value", "evidence", "receipt"}
	if len(changes[0]) != len(wantChange) {
		t.Fatalf("change JSON = %s", outer["changes"])
	}
	for _, key := range wantChange {
		if _, ok := changes[0][key]; !ok {
			t.Fatalf("change JSON missing %q: %s", key, outer["changes"])
		}
	}
}

func TestVerifyPageGoneResponseOmitsSimilarityAndCarriesObservation(t *testing.T) {
	recorder := &fakeRecorder{}
	service := testService(t, fixture(t, "old.html"), RevisitResult{
		StatusCode: 404,
		FinalURL:   testFinalURL,
		SnapshotID: newSnapshotID,
		FetchedAt:  newFetchedAt,
	}, testSigner(t), recorder, nil)
	response, err := service.Verify(context.Background(), models.VerifyRequest{
		URL:    testURL,
		Claims: []models.Claim{testClaim("plan", `"Pro Plan"`, "Pro Plan", "#plan .title")},
	})
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if response.URL != testURL || response.FinalURL != testFinalURL || response.StatusCode != 404 || response.PageSimilarity != nil || response.SnapshotID != newSnapshotID {
		t.Fatalf("page-gone response = %#v", response)
	}
	row := recorder.committedBatches()[0][0]
	if row.Outcome != ledger.OutcomeGone || row.GoneScope != ledger.GoneScopePage || row.PageSimilarity != nil || row.NewSnapshotID != newSnapshotID {
		t.Fatalf("page-gone ledger row = %#v", row)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if _, exists := document["page_similarity"]; exists {
		t.Fatalf("page_similarity unexpectedly present: %s", encoded)
	}
	if _, exists := document["snapshot_id"]; !exists {
		t.Fatalf("snapshot_id missing from definitive page-gone observation: %s", encoded)
	}
	for _, key := range []string{"verification_id", "url", "final_url", "status_code", "results", "verified_at"} {
		if _, exists := document[key]; !exists {
			t.Fatalf("response missing %q: %s", key, encoded)
		}
	}
}

func TestVerifyRejectsPageGoneWithoutSnapshotProvenance(t *testing.T) {
	recorder := &fakeRecorder{}
	service := testService(t, fixture(t, "old.html"), RevisitResult{
		StatusCode: 404,
		FinalURL:   testFinalURL,
		FetchedAt:  newFetchedAt,
	}, testSigner(t), recorder, nil)
	response, err := service.Verify(context.Background(), models.VerifyRequest{
		URL:    testURL,
		Claims: []models.Claim{testClaim("plan", `"Pro Plan"`, "Pro Plan", "#plan .title")},
	})
	if response != nil || !errors.Is(err, ErrRevisit) {
		t.Fatalf("Verify(page gone without snapshot) = (%#v, %v), want nil + ErrRevisit", response, err)
	}
	if recorder.callCount() != 0 {
		t.Fatal("page-gone observation without snapshot reached ledger")
	}
}

func TestVerifyHonorsContextCancellation(t *testing.T) {
	t.Run("already canceled", func(t *testing.T) {
		var revisits atomic.Int32
		service := mustService(t, Config{
			Revisitor: revisitorFunc(func(context.Context, string) (RevisitResult, error) {
				revisits.Add(1)
				return RevisitResult{}, nil
			}),
			Snapshots: snapshotReaderFunc(func(snapshot.ID) ([]byte, snapshot.Meta, error) {
				return []byte(fixture(t, "old.html")), snapshot.Meta{
					URL:        testURL,
					FetchedAt:  oldFetchedAt,
					StatusCode: 200,
				}, nil
			}),
			Receipts: testSigner(t),
			Recorder: &fakeRecorder{},
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		response, err := service.Verify(ctx, models.VerifyRequest{
			URL:    testURL,
			Claims: []models.Claim{testClaim("plan", `"Pro Plan"`, "Pro Plan", "#plan .title")},
		})
		if response != nil || !errors.Is(err, context.Canceled) || revisits.Load() != 0 {
			t.Fatalf("Verify(canceled) = (%#v, %v), revisits=%d", response, err, revisits.Load())
		}
	})

	t.Run("canceled during revisit", func(t *testing.T) {
		started := make(chan struct{})
		recorder := &fakeRecorder{}
		oldHTML := fixture(t, "old.html")
		service := mustService(t, Config{
			Revisitor: revisitorFunc(func(ctx context.Context, _ string) (RevisitResult, error) {
				close(started)
				<-ctx.Done()
				return RevisitResult{}, ctx.Err()
			}),
			Snapshots: snapshotReaderFunc(func(snapshot.ID) ([]byte, snapshot.Meta, error) {
				return []byte(oldHTML), snapshot.Meta{
					URL:        testURL,
					FetchedAt:  oldFetchedAt,
					StatusCode: 200,
				}, nil
			}),
			Receipts: testSigner(t),
			Recorder: recorder,
		})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := service.Verify(ctx, models.VerifyRequest{
				URL:    testURL,
				Claims: []models.Claim{testClaim("plan", `"Pro Plan"`, "Pro Plan", "#plan .title")},
			})
			done <- err
		}()
		<-started
		cancel()
		err := <-done
		if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrRevisit) {
			t.Fatalf("Verify() error = %v, want context.Canceled and ErrRevisit", err)
		}
		if recorder.callCount() != 0 {
			t.Fatal("canceled revisit reached ledger")
		}
	})
}

func TestServiceConcurrentVerify(t *testing.T) {
	const calls = 32
	oldHTML := fixture(t, "old.html")
	current := observation(fixture(t, "same.html"), 200)
	recorder := &fakeRecorder{}
	var nextID atomic.Int32
	service := mustService(t, Config{
		Revisitor: revisitorFunc(func(context.Context, string) (RevisitResult, error) {
			return current, nil
		}),
		Snapshots: snapshotReaderFunc(func(id snapshot.ID) ([]byte, snapshot.Meta, error) {
			switch id {
			case snapshot.ID(oldSnapshotID):
				return []byte(oldHTML), snapshot.Meta{URL: testURL, FetchedAt: oldFetchedAt, StatusCode: 200}, nil
			case snapshot.ID(newSnapshotID):
				return []byte(current.RawHTML), snapshot.Meta{
					URL:        current.FinalURL,
					FetchedAt:  current.FetchedAt,
					StatusCode: current.StatusCode,
				}, nil
			default:
				return nil, snapshot.Meta{}, fmt.Errorf("unexpected snapshot %q", id)
			}
		}),
		Receipts: testSigner(t),
		Recorder: recorder,
		IDs: func() (string, error) {
			return fmt.Sprintf("verify-%d", nextID.Add(1)), nil
		},
	})

	start := make(chan struct{})
	errorsSeen := make(chan error, calls)
	var workers sync.WaitGroup
	for range calls {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, err := service.Verify(context.Background(), models.VerifyRequest{
				URL:    testURL,
				Claims: []models.Claim{testClaim("plan", `"Pro Plan"`, "Pro Plan", "#plan .title")},
			})
			if err != nil {
				errorsSeen <- err
			}
		}()
	}
	close(start)
	workers.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Errorf("Verify() error = %v", err)
	}
	if got := len(recorder.committedBatches()); got != calls {
		t.Fatalf("committed transactions = %d, want %d", got, calls)
	}
}

func TestNewServiceRequiresCoreDependencies(t *testing.T) {
	valid := Config{
		Revisitor: revisitorFunc(func(context.Context, string) (RevisitResult, error) { return RevisitResult{}, nil }),
		Snapshots: snapshotReaderFunc(func(snapshot.ID) ([]byte, snapshot.Meta, error) { return nil, snapshot.Meta{}, nil }),
		Receipts:  testSigner(t),
		Recorder:  &fakeRecorder{},
	}
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "revisitor", mutate: func(config *Config) { config.Revisitor = nil }},
		{name: "snapshots", mutate: func(config *Config) { config.Snapshots = nil }},
		{name: "receipts", mutate: func(config *Config) { config.Receipts = nil }},
		{name: "recorder", mutate: func(config *Config) { config.Recorder = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			service, err := NewService(config)
			if service != nil || !errors.Is(err, ErrNotConfigured) {
				t.Fatalf("NewService() = (%#v, %v), want nil + ErrNotConfigured", service, err)
			}
		})
	}
}

type revisitorFunc func(context.Context, string) (RevisitResult, error)

func (f revisitorFunc) Revisit(ctx context.Context, target string) (RevisitResult, error) {
	return f(ctx, target)
}

type snapshotReaderFunc func(snapshot.ID) ([]byte, snapshot.Meta, error)

func (f snapshotReaderFunc) Content(id snapshot.ID, _ int) ([]byte, error) {
	content, _, err := f(id)
	return content, err
}

func (f snapshotReaderFunc) HasObservation(id snapshot.ID, match func(snapshot.Meta) bool) (bool, error) {
	_, meta, err := f(id)
	if err != nil {
		return false, err
	}
	return match(meta), nil
}

type countingReceiptCodec struct {
	delegate    ReceiptCodec
	verifyCalls atomic.Int32
	signCalls   atomic.Int32
}

func (codec *countingReceiptCodec) Verify(token string) (*receipts.Payload, error) {
	codec.verifyCalls.Add(1)
	return codec.delegate.Verify(token)
}

func (codec *countingReceiptCodec) Sign(payload receipts.Payload) (string, error) {
	codec.signCalls.Add(1)
	return codec.delegate.Sign(payload)
}

type fakeRecorder struct {
	mu              sync.Mutex
	err             error
	before          func()
	calls           int
	attempted       [][]ledger.Verification
	committed       [][]ledger.Verification
	attemptedEvents []*ledger.OutboxEvent
	committedEvents []*ledger.OutboxEvent
}

func (recorder *fakeRecorder) RecordVerificationBatch(
	ctx context.Context,
	rows []ledger.Verification,
	event *ledger.OutboxEvent,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if recorder.before != nil {
		recorder.before()
	}
	cloned := cloneRows(rows)
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.calls++
	recorder.attempted = append(recorder.attempted, cloned)
	recorder.attemptedEvents = append(recorder.attemptedEvents, cloneOutboxEvent(event))
	if recorder.err != nil {
		return recorder.err
	}
	recorder.committed = append(recorder.committed, cloneRows(cloned))
	recorder.committedEvents = append(recorder.committedEvents, cloneOutboxEvent(event))
	return nil
}

func (recorder *fakeRecorder) callCount() int {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.calls
}

func (recorder *fakeRecorder) committedBatches() [][]ledger.Verification {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	result := make([][]ledger.Verification, len(recorder.committed))
	for index := range recorder.committed {
		result[index] = cloneRows(recorder.committed[index])
	}
	return result
}

func (recorder *fakeRecorder) lastAttempt() []ledger.Verification {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.attempted) == 0 {
		return nil
	}
	return cloneRows(recorder.attempted[len(recorder.attempted)-1])
}

func (recorder *fakeRecorder) committedOutboxEvents() []*ledger.OutboxEvent {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	result := make([]*ledger.OutboxEvent, len(recorder.committedEvents))
	for index := range recorder.committedEvents {
		result[index] = cloneOutboxEvent(recorder.committedEvents[index])
	}
	return result
}

func (recorder *fakeRecorder) lastAttemptedOutboxEvent() *ledger.OutboxEvent {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.attemptedEvents) == 0 {
		return nil
	}
	return cloneOutboxEvent(recorder.attemptedEvents[len(recorder.attemptedEvents)-1])
}

func cloneOutboxEvent(event *ledger.OutboxEvent) *ledger.OutboxEvent {
	if event == nil {
		return nil
	}
	cloned := *event
	cloned.Payload = cloneRaw(event.Payload)
	if event.LastAttemptAt != nil {
		value := *event.LastAttemptAt
		cloned.LastAttemptAt = &value
	}
	if event.DeliveredAt != nil {
		value := *event.DeliveredAt
		cloned.DeliveredAt = &value
	}
	if event.FailedAt != nil {
		value := *event.FailedAt
		cloned.FailedAt = &value
	}
	return &cloned
}

func cloneRows(rows []ledger.Verification) []ledger.Verification {
	cloned := make([]ledger.Verification, len(rows))
	for index := range rows {
		cloned[index] = rows[index]
		cloned[index].OldValue = cloneRaw(rows[index].OldValue)
		cloned[index].NewValue = cloneRaw(rows[index].NewValue)
		if rows[index].PageSimilarity != nil {
			cloned[index].PageSimilarity = floatPointer(*rows[index].PageSimilarity)
		}
	}
	return cloned
}

func decodeChangedOutbox(t *testing.T, event *ledger.OutboxEvent) ChangedEvent {
	t.Helper()
	if event == nil {
		t.Fatal("outbox event is nil")
	}
	var envelope struct {
		Type      string       `json:"type"`
		JobID     string       `json:"job_id"`
		Timestamp int64        `json:"timestamp"`
		Data      ChangedEvent `json:"data"`
	}
	if err := json.Unmarshal(event.Payload, &envelope); err != nil {
		t.Fatalf("Unmarshal(outbox payload) error = %v", err)
	}
	if envelope.Type != factChangedEventType || envelope.JobID != event.VerificationID ||
		envelope.Timestamp != event.CreatedAt.Unix() || envelope.Data.VerificationID != event.VerificationID {
		t.Fatalf("outbox envelope = %#v for event %#v", envelope, event)
	}
	return envelope.Data
}

func testService(
	t *testing.T,
	oldHTML string,
	current RevisitResult,
	codec ReceiptCodec,
	recorder VerificationRecorder,
	_ any,
) *Service {
	t.Helper()
	return mustService(t, Config{
		Revisitor: revisitorFunc(func(ctx context.Context, target string) (RevisitResult, error) {
			if err := ctx.Err(); err != nil {
				return RevisitResult{}, err
			}
			if target != testURL {
				return RevisitResult{}, fmt.Errorf("target = %q, want %q", target, testURL)
			}
			return current, nil
		}),
		Snapshots: snapshotReaderFunc(func(id snapshot.ID) ([]byte, snapshot.Meta, error) {
			switch id {
			case snapshot.ID(oldSnapshotID):
				return []byte(oldHTML), snapshot.Meta{URL: testURL, FetchedAt: oldFetchedAt, StatusCode: 200}, nil
			case snapshot.ID(current.SnapshotID):
				finalURL := current.FinalURL
				if finalURL == "" {
					finalURL = testURL
				}
				return []byte(current.RawHTML), snapshot.Meta{
					URL:        finalURL,
					FetchedAt:  current.FetchedAt,
					StatusCode: current.StatusCode,
				}, nil
			default:
				return nil, snapshot.Meta{}, fmt.Errorf("unexpected snapshot %q", id)
			}
		}),
		Receipts: codec,
		Recorder: recorder,
		IDs: func() (string, error) {
			return "verification-fixture", nil
		},
	})
}

func mustService(t *testing.T, config Config) *Service {
	t.Helper()
	service, err := NewService(config)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return service
}

func testSigner(t *testing.T) *receipts.Signer {
	t.Helper()
	seed := bytes.Repeat([]byte{0x42}, ed25519.SeedSize)
	signer, err := receipts.NewSigner(ed25519.NewKeyFromSeed(seed))
	if err != nil {
		t.Fatalf("NewSigner() error = %v", err)
	}
	return signer
}

func testClaim(path, value, quote, selector string) models.Claim {
	return models.Claim{
		Path:   path,
		Value:  json.RawMessage(value),
		Anchor: testAnchor(quote, selector),
	}
}

func testAnchor(quote, selector string) evidence.Anchor {
	return evidence.Anchor{
		Quote:      quote,
		TextRange:  [2]int{10, 10 + len(quote)},
		Selector:   selector,
		Method:     evidence.MethodExact,
		SnapshotID: oldSnapshotID,
		FetchedAt:  oldFetchedAt,
	}
}

func observation(html string, statusCode int) RevisitResult {
	return RevisitResult{
		StatusCode: statusCode,
		FinalURL:   testFinalURL,
		RawHTML:    html,
		SnapshotID: newSnapshotID,
		FetchedAt:  newFetchedAt,
	}
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	contents, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(contents)
}

func htmlText(t *testing.T, html string) string {
	t.Helper()
	document, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return strings.TrimSpace(document.Text())
}

func assertJSONEqual(t *testing.T, got, want json.RawMessage, description string) {
	t.Helper()
	if len(want) == 0 {
		if len(got) != 0 {
			t.Fatalf("%s = %s, want omitted", description, got)
		}
		return
	}
	gotValue, gotErr := decodeScalar(got)
	wantValue, wantErr := decodeScalar(want)
	if gotErr != nil || wantErr != nil || !gotValue.equal(wantValue) {
		t.Fatalf("%s = %s, want semantically equal to %s (errors %v, %v)", description, got, want, gotErr, wantErr)
	}
}

func differentLastByte(last byte) string {
	if last == 'A' {
		return "B"
	}
	return "A"
}
