package evidence

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func TestAlignValueMatchingMatrix(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		cleaned string
		html    string
		method  Method
		quote   string
	}{
		{
			name:    "exact",
			value:   "Pro Plan",
			cleaned: "Choose the Pro Plan today",
			html:    `<main><h2 id="pro">Pro Plan</h2></main>`,
			method:  MethodExact,
			quote:   "Pro Plan",
		},
		{
			name:    "whitespace normalized",
			value:   "Fast reliable search",
			cleaned: "Fast\n\t reliable   search",
			html:    `<p class="tagline">Fast reliable search</p>`,
			method:  MethodNormalized,
			quote:   "Fast\n\t reliable   search",
		},
		{
			name:    "thousands price",
			value:   "1299",
			cleaned: "List price: $1,299 today",
			html:    `<span id="price">$1,299</span>`,
			method:  MethodNormalized,
			quote:   "1,299",
		},
		{
			name:    "full width",
			value:   "ABC 123",
			cleaned: "型号：ＡＢＣ　１２３",
			html:    `<div id="model">型号：ＡＢＣ　１２３</div>`,
			method:  MethodNormalized,
			quote:   "ＡＢＣ　１２３",
		},
		{
			name:    "fuzzy word order",
			value:   "Acme new model launches today",
			cleaned: "Breaking: Acme launches new model today worldwide.",
			html:    `<article><p class="lead">Acme launches new model today worldwide.</p></article>`,
			method:  MethodFuzzy,
			quote:   "Acme launches new model today",
		},
		{
			name:    "unlocated",
			value:   "invented fact",
			cleaned: "Only sourced facts live here",
			html:    `<p>Only sourced facts live here</p>`,
			method:  MethodUnlocated,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			anchor := AlignValue(tt.value, tt.cleaned, tt.html)
			if anchor.Method != tt.method {
				t.Fatalf("Method = %q, want %q; anchor=%#v", anchor.Method, tt.method, anchor)
			}
			if tt.quote != "" && anchor.Quote != tt.quote {
				t.Fatalf("Quote = %q, want %q", anchor.Quote, tt.quote)
			}
			if anchor.Method != MethodUnlocated {
				if got := tt.cleaned[anchor.TextRange[0]:anchor.TextRange[1]]; got != anchor.Quote {
					t.Fatalf("range resolves to %q, want quote %q", got, anchor.Quote)
				}
				if anchor.Selector == "" {
					t.Fatal("Selector is empty for located value")
				}
			}
		})
	}
}

func TestAlignAllNestedArrayPathsAndRate(t *testing.T) {
	data := json.RawMessage(`{"items":[{"name":"Alpha","price":10},{"name":"Missing","price":20}],"active":true}`)
	cleaned := "Alpha costs 10. Active: true. Another price is 20."
	html := `<main><div class="item"><span>Alpha</span><b>10</b></div><p>Active: true.</p><p>Another price is 20.</p></main>`
	fetchedAt := time.Unix(123, 0).UTC()
	basis, rate := AlignAll(data, cleaned, html, "sha256:abc", fetchedAt)
	for _, path := range []string{"items.0.name", "items.0.price", "items.1.name", "items.1.price", "active"} {
		if _, ok := basis[path]; !ok {
			t.Fatalf("basis missing path %q: %#v", path, basis)
		}
	}
	if basis["items.1.name"].Method != MethodUnlocated {
		t.Fatalf("Missing method = %q", basis["items.1.name"].Method)
	}
	if math.Abs(rate-0.2) > 0.0001 {
		t.Fatalf("unlocated rate = %f, want 0.2", rate)
	}
	for _, anchor := range basis {
		if anchor.SnapshotID != "sha256:abc" || !anchor.FetchedAt.Equal(fetchedAt) {
			t.Fatalf("provenance missing: %#v", anchor)
		}
	}
}

func TestFindUniqueSelectorUsesDeepestUniqueElement(t *testing.T) {
	html := `<main><section class="card"><span class="value">Needle</span></section><section class="card"><span class="value">Other</span></section></main>`
	selector := FindUniqueSelector(html, "Needle")
	if selector == "" {
		t.Fatal("selector is empty")
	}
	if selector != "section.card:nth-of-type(1) > span.value" && selector != "main > section.card:nth-of-type(1) > span.value" {
		t.Fatalf("selector = %q, want a unique selector for deepest span", selector)
	}
}

func BenchmarkAlignAll(b *testing.B) {
	data := json.RawMessage(`{"title":"Pro Plan","price":"1299","features":["Fast reliable search","Evidence receipts"]}`)
	cleaned := "Pro Plan costs $1,299. Fast reliable search with Evidence receipts."
	html := `<main><article id="pro"><h1>Pro Plan</h1><p>Pro Plan costs $1,299.</p><ul><li>Fast reliable search</li><li>Evidence receipts</li></ul></article></main>`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		AlignAll(data, cleaned, html, "sha256:test")
	}
}
