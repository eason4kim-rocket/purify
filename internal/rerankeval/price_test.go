package rerankeval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testBravePriceHTML = `<!doctype html><html><body><div class="card plan"><h2>Search</h2><p>Complete search results.</p><div class="price-item"><span>$</span><span>5.00</span><span> per <b>1,000</b> requests</span></div></div><div class="card plan"><h2>Answers</h2><div class="price-item"><span>$4.00 per 1,000 queries</span></div></div></body></html>`

func TestLoadPriceEvidenceBindsOfficialBytesAndDerivesUnitCost(t *testing.T) {
	directory, manifest := writeTestPriceEvidence(t, testBravePriceHTML)
	evidence, err := LoadPriceEvidence(directory)
	if err != nil {
		t.Fatalf("LoadPriceEvidence() error = %v", err)
	}
	if evidence.UnitCostNanos != 5_000_000 || evidence.Manifest.ArtifactID != manifest.ArtifactID {
		t.Fatalf("evidence = %#v", evidence)
	}
	manifest.Sources[0].Path = "changed.html"
	if evidence.Manifest.Sources[0].Path == manifest.Sources[0].Path {
		t.Fatal("returned evidence aliases caller state")
	}
}

func TestPriceEvidenceIdentityIsCanonicalAndDomainSeparated(t *testing.T) {
	manifest := validTestPriceManifest(t, []byte(testBravePriceHTML))
	first, err := PriceEvidenceID(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifest.ArtifactID = strings.Repeat("f", 64)
	second, err := PriceEvidenceID(manifest)
	if err != nil || first != second {
		t.Fatalf("identity depends on identity field: %q/%q %v", first, second, err)
	}
	manifest.Basis += "-drift"
	if _, err := PriceEvidenceID(manifest); err == nil {
		t.Fatal("PriceEvidenceID accepted a non-reference manifest")
	}
	manifest = validTestPriceManifest(t, []byte(testBravePriceHTML))
	manifest.Sources[0].ResponseDate = "Tue, 11 Aug 2026 14:42:29 GMT"
	if _, err := PriceEvidenceID(manifest); err == nil {
		t.Fatal("PriceEvidenceID accepted a source captured on another price date")
	}
}

func TestDecodePriceManifestRejectsStrictJSONAndProfileDrift(t *testing.T) {
	manifest := validTestPriceManifest(t, []byte(testBravePriceHTML))
	raw := marshalPriceManifest(t, manifest)
	tests := []struct {
		name string
		raw  []byte
	}{
		{name: "BOM", raw: append([]byte{0xef, 0xbb, 0xbf}, raw...)},
		{name: "embedded BOM", raw: []byte(strings.Replace(string(raw), `"etag":"`, `"etag":"\ufeff`, 1))},
		{name: "invalid UTF-8", raw: append(append([]byte(nil), raw[:len(raw)-1]...), 0xff, '}')},
		{name: "trailing space", raw: append(append([]byte(nil), raw...), ' ')},
		{name: "unknown", raw: []byte(strings.Replace(string(raw), `"sources":`, `"extra":false,"sources":`, 1))},
		{name: "case smuggle", raw: []byte(strings.Replace(string(raw), `"kind"`, `"Kind"`, 1))},
		{name: "unicode duplicate", raw: []byte(strings.Replace(string(raw), `"kind"`, `"k\u0069nd":"provider_call","kind"`, 1))},
		{name: "null", raw: []byte(strings.Replace(string(raw), `"publisher":"brave"`, `"publisher":null`, 1))},
		{name: "fractional quantity", raw: []byte(strings.Replace(string(raw), `"unit_quantity":1000`, `"unit_quantity":1000.0`, 1))},
		{name: "wrong amount", raw: []byte(strings.Replace(string(raw), `"units":5`, `"units":4`, 1))},
		{name: "wrong URL", raw: []byte(strings.Replace(string(raw), "api-dashboard.search.brave.com", "example.test", 1))},
		{name: "stale identity", raw: []byte(strings.Replace(string(raw), manifest.ArtifactID, strings.Repeat("f", 64), 1))},
		{name: "trailing value", raw: append(append([]byte(nil), raw...), []byte(`{}`)...)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if decoded, err := DecodePriceManifest(test.raw); err == nil || decoded.ArtifactID != "" {
				t.Fatalf("DecodePriceManifest() = %#v, %v", decoded, err)
			}
		})
	}
}

func TestLoadPriceEvidenceRejectsFilesystemAndContentDrift(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{name: "body digest", mutate: func(t *testing.T, directory string) {
			writeTestFile(t, filepath.Join(directory, "brave-search-pricing.html"), []byte(testBravePriceHTML+" "))
		}},
		{name: "price text", mutate: func(t *testing.T, directory string) {
			body := strings.Replace(testBravePriceHTML, "5.00", "4.00", 1)
			manifest := validTestPriceManifest(t, []byte(body))
			writeTestFile(t, filepath.Join(directory, "brave-search-pricing.html"), []byte(body))
			writeTestFile(t, filepath.Join(directory, priceManifestName), marshalPriceManifest(t, manifest))
		}},
		{name: "ambiguous price text", mutate: func(t *testing.T, directory string) {
			body := strings.Replace(testBravePriceHTML, `</div></div><div class="card plan"><h2>Answers`, `<div class="price-item"><span>$</span><span>4.00</span><span> per 1,000 requests</span></div></div></div><div class="card plan"><h2>Answers`, 1)
			manifest := validTestPriceManifest(t, []byte(body))
			writeTestFile(t, filepath.Join(directory, "brave-search-pricing.html"), []byte(body))
			writeTestFile(t, filepath.Join(directory, priceManifestName), marshalPriceManifest(t, manifest))
		}},
		{name: "hidden stale price", mutate: func(t *testing.T, directory string) {
			body := strings.Replace(testBravePriceHTML, `<h2>Search</h2>`, `<h2>Search</h2><script>Search $ 5.00 per 1,000 requests</script>`, 1)
			body = strings.Replace(body, `<span>5.00</span>`, `<span>4.00</span>`, 1)
			manifest := validTestPriceManifest(t, []byte(body))
			writeTestFile(t, filepath.Join(directory, "brave-search-pricing.html"), []byte(body))
			writeTestFile(t, filepath.Join(directory, priceManifestName), marshalPriceManifest(t, manifest))
		}},
		{name: "hidden DOM stale price", mutate: func(t *testing.T, directory string) {
			body := strings.Replace(testBravePriceHTML, `<span>5.00</span><span> per <b>1,000</b> requests</span>`,
				`<span>4.00</span><span> per <b>2,000</b> requests</span><span hidden>$ 5.00 per 1,000 requests</span>`, 1)
			manifest := validTestPriceManifest(t, []byte(body))
			writeTestFile(t, filepath.Join(directory, "brave-search-pricing.html"), []byte(body))
			writeTestFile(t, filepath.Join(directory, priceManifestName), marshalPriceManifest(t, manifest))
		}},
		{name: "hidden ancestor stale card", mutate: func(t *testing.T, directory string) {
			body := strings.Replace(testBravePriceHTML, `<div class="card plan"><h2>Search</h2>`,
				`<section hidden><div class="card plan"><h2>Search</h2>`, 1)
			body = strings.Replace(body, `</div><div class="card plan"><h2>Answers`,
				`</div></section><div class="card plan"><h2>Search Plus</h2></div><div class="card plan"><h2>Answers`, 1)
			manifest := validTestPriceManifest(t, []byte(body))
			writeTestFile(t, filepath.Join(directory, "brave-search-pricing.html"), []byte(body))
			writeTestFile(t, filepath.Join(directory, priceManifestName), marshalPriceManifest(t, manifest))
		}},
		{name: "source invalid UTF-8", mutate: func(t *testing.T, directory string) {
			body := append([]byte(testBravePriceHTML), 0xff)
			manifest := validTestPriceManifest(t, body)
			writeTestFile(t, filepath.Join(directory, "brave-search-pricing.html"), body)
			writeTestFile(t, filepath.Join(directory, priceManifestName), marshalPriceManifest(t, manifest))
		}},
		{name: "extra file", mutate: func(t *testing.T, directory string) {
			writeTestFile(t, filepath.Join(directory, "unused"), []byte("x"))
		}},
		{name: "source symlink", mutate: func(t *testing.T, directory string) {
			path := filepath.Join(directory, "brave-search-pricing.html")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(priceManifestName, path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "source hardlink", mutate: func(t *testing.T, directory string) {
			path := filepath.Join(directory, "brave-search-pricing.html")
			if err := os.Link(path, directory+"-hardlink"); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory, _ := writeTestPriceEvidence(t, testBravePriceHTML)
			test.mutate(t, directory)
			if evidence, err := LoadPriceEvidence(directory); err == nil || !errors.Is(err, ErrInvalidPriceEvidence) || evidence.UnitCostNanos != 0 {
				t.Fatalf("LoadPriceEvidence() = %#v, %v", evidence, err)
			}
		})
	}
}

func TestLoadPriceEvidenceRejectsRelativeDirectory(t *testing.T) {
	if _, err := LoadPriceEvidence("relative"); !errors.Is(err, ErrInvalidPriceEvidence) {
		t.Fatalf("error = %v", err)
	}
}

func TestPriceFileSnapshotRejectsMutationAfterInitialRead(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "source.html")
	writeTestFile(t, path, []byte("original"))
	snapshot, err := openPriceFileSnapshot(directory, "source.html", 64)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.close()
	if err := os.WriteFile(path, []byte("mutated!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.verify(); !errors.Is(err, ErrInvalidPriceEvidence) {
		t.Fatalf("verify() error = %v", err)
	}
}

func writeTestPriceEvidence(t *testing.T, body string) (string, PriceEvidenceManifest) {
	t.Helper()
	directory := t.TempDir()
	manifest := validTestPriceManifest(t, []byte(body))
	writeTestFile(t, filepath.Join(directory, "brave-search-pricing.html"), []byte(body))
	writeTestFile(t, filepath.Join(directory, priceManifestName), marshalPriceManifest(t, manifest))
	return directory, manifest
}

func validTestPriceManifest(t *testing.T, body []byte) PriceEvidenceManifest {
	t.Helper()
	digest := sha256.Sum256(body)
	manifest := PriceEvidenceManifest{
		SchemaVersion: PriceEvidenceSchemaVersion,
		Kind:          PriceKindProviderCall,
		CapturedAt:    "2026-08-12T14:42:29Z",
		PriceDate:     "2026-08-12",
		Currency:      "USD",
		Publisher:     pricePublisherBrave,
		Product:       priceProductBraveWebSearch,
		PlanOrSKU:     pricePlanBraveSearch,
		Offering:      "public_list",
		Region:        "global",
		BillingUnit:   priceUnitRequest,
		UnitQuantity:  1_000,
		Amount:        PriceMoney{Units: 5},
		Basis:         "public_list_excluding_credits_discounts_tax",
		SourceFormat:  priceSourceBraveHTML,
		Sources: []PriceEvidenceSource{{
			Role: "pricing_page", Path: "brave-search-pricing.html",
			CanonicalHTTPSURL: "https://api-dashboard.search.brave.com/documentation/pricing",
			MediaType:         "text/html; charset=utf-8", BodySHA256: hex.EncodeToString(digest[:]),
			ResponseDate: "Wed, 12 Aug 2026 14:42:29 GMT", ETag: `"g9pjb6"`,
		}},
	}
	identity, err := PriceEvidenceID(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifest.ArtifactID = identity
	return manifest
}

func marshalPriceManifest(t *testing.T, manifest PriceEvidenceManifest) []byte {
	t.Helper()
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func writeTestFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}
