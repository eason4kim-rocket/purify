package eav

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type fakeExtractor struct {
	calls  int
	doc    Document
	slate  []Candidate
	result DocumentEntities
	err    error
}

func (fake *fakeExtractor) ExtractEntities(
	_ context.Context,
	doc Document,
	slate []Candidate,
) (DocumentEntities, error) {
	fake.calls++
	fake.doc = doc
	fake.slate = slate
	return fake.result, fake.err
}

func extractionTestDocument() (Document, []Candidate) {
	doc := Document{
		URL:   "https://example.com/products/iphone-15-pro",
		Title: "Buy iPhone 15 Pro - Apple",
		Cleaned: "iPhone 15 Pro. Titanium design. The ticker AAPL rose today. " +
			"The device iPhone 15 Pro ships Friday.",
	}
	slate := []Candidate{
		{Surface: "iPhone 15 Pro", Signal: SignalTitle, Quote: "iPhone 15 Pro"},
		{Surface: "Apple", Signal: SignalTitle, Quote: "Apple"},
	}
	return doc, slate
}

func TestAnchoredExtractionAdoptsAnchoredForms(t *testing.T) {
	doc, slate := extractionTestDocument()
	fake := &fakeExtractor{result: DocumentEntities{Primary: &Entity{
		Name:    "iphone 15 pro",
		Kind:    KindProduct,
		Aliases: []string{"aapl", "Ghost Alias", "iPhone 15 Pro"},
		Quote:   "The device iPhone 15 Pro ships Friday.",
	}}}
	got, err := AnchoredExtraction(context.Background(), fake, doc, slate)
	if err != nil {
		t.Fatalf("AnchoredExtraction returned error: %v", err)
	}
	want := DocumentEntities{Primary: &Entity{
		Name:    "iPhone 15 Pro",
		Kind:    KindProduct,
		Aliases: []string{"AAPL"},
		Quote:   "The device iPhone 15 Pro ships Friday.",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gated extraction mismatch\n got: %#v\nwant: %#v", got.Primary, want.Primary)
	}
}

func TestAnchoredExtractionGatesDiscardPrimary(t *testing.T) {
	doc, slate := extractionTestDocument()
	cases := []struct {
		name   string
		result DocumentEntities
	}{
		{"nil primary", DocumentEntities{}},
		{"hallucinated name", DocumentEntities{Primary: &Entity{
			Name: "Galaxy S26 Ultra", Kind: KindProduct, Quote: "iPhone 15 Pro",
		}}},
		{"hallucinated quote", DocumentEntities{Primary: &Entity{
			Name: "iPhone 15 Pro", Kind: KindProduct,
			Quote: "totally unrelated fabricated sentence xyzzy",
		}}},
		{"empty name", DocumentEntities{Primary: &Entity{
			Name: "   ", Kind: KindProduct, Quote: "iPhone 15 Pro",
		}}},
		{"oversized name", DocumentEntities{Primary: &Entity{
			Name: strings.Repeat("a", MaxEntityBytes+1), Kind: KindProduct,
			Quote: "iPhone 15 Pro",
		}}},
		{"oversized quote", DocumentEntities{Primary: &Entity{
			Name: "iPhone 15 Pro", Kind: KindProduct,
			Quote: strings.Repeat("q", MaxQuoteBytes+1),
		}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake := &fakeExtractor{result: testCase.result}
			got, err := AnchoredExtraction(context.Background(), fake, doc, slate)
			if err != nil {
				t.Fatalf("gate must degrade, not error: %v", err)
			}
			if got.Primary != nil || got.Secondary != nil {
				t.Fatalf("gate must discard, got %#v", got)
			}
		})
	}
}

func TestAnchoredExtractionQuoteFallsBackToName(t *testing.T) {
	doc, slate := extractionTestDocument()
	fake := &fakeExtractor{result: DocumentEntities{Primary: &Entity{
		Name: "iPhone 15 Pro", Kind: KindProduct,
	}}}
	got, err := AnchoredExtraction(context.Background(), fake, doc, slate)
	if err != nil {
		t.Fatalf("AnchoredExtraction returned error: %v", err)
	}
	if got.Primary == nil || got.Primary.Quote != "iPhone 15 Pro" {
		t.Fatalf("missing quote must fall back to the anchored name, got %#v", got.Primary)
	}
}

func TestAnchoredExtractionCapsAliases(t *testing.T) {
	aliases := make([]string, 0, MaxAliases+4)
	mentions := make([]string, 0, MaxAliases+4)
	for index := 1; index <= MaxAliases+4; index++ {
		name := fmt.Sprintf("Alias%02d", index)
		aliases = append(aliases, name)
		mentions = append(mentions, name)
	}
	doc := Document{
		Title:   "Widget Pro",
		Cleaned: "Widget Pro is here. " + strings.Join(mentions, " "),
	}
	slate := []Candidate{{Surface: "Widget Pro", Signal: SignalTitle, Quote: "Widget Pro"}}
	fake := &fakeExtractor{result: DocumentEntities{Primary: &Entity{
		Name: "Widget Pro", Kind: KindProduct, Aliases: aliases, Quote: "Widget Pro is here.",
	}}}
	got, err := AnchoredExtraction(context.Background(), fake, doc, slate)
	if err != nil {
		t.Fatalf("AnchoredExtraction returned error: %v", err)
	}
	if got.Primary == nil || len(got.Primary.Aliases) != MaxAliases {
		t.Fatalf("aliases must cap at %d, got %#v", MaxAliases, got.Primary)
	}
	if got.Primary.Aliases[0] != "Alias01" || got.Primary.Aliases[MaxAliases-1] != fmt.Sprintf("Alias%02d", MaxAliases) {
		t.Fatalf("alias cap must keep the first entries, got %v", got.Primary.Aliases)
	}
}

func TestAnchoredExtractionCoercesUnknownKind(t *testing.T) {
	doc, slate := extractionTestDocument()
	fake := &fakeExtractor{result: DocumentEntities{Primary: &Entity{
		Name: "iPhone 15 Pro", Kind: Kind("banana"), Quote: "iPhone 15 Pro",
	}}}
	got, err := AnchoredExtraction(context.Background(), fake, doc, slate)
	if err != nil {
		t.Fatalf("AnchoredExtraction returned error: %v", err)
	}
	if got.Primary == nil || got.Primary.Kind != KindOther {
		t.Fatalf("unknown kind must coerce to %q, got %#v", KindOther, got.Primary)
	}
}

func TestAnchoredExtractionSwallowsProviderErrors(t *testing.T) {
	doc, slate := extractionTestDocument()
	fake := &fakeExtractor{err: errors.New("provider returned HTTP 500")}
	got, err := AnchoredExtraction(context.Background(), fake, doc, slate)
	if err != nil {
		t.Fatalf("provider failure must degrade to uncertain, got error %v", err)
	}
	if got.Primary != nil {
		t.Fatalf("provider failure must yield no primary, got %#v", got.Primary)
	}
}

func TestAnchoredExtractionPropagatesNotConfigured(t *testing.T) {
	doc, slate := extractionTestDocument()
	if _, err := AnchoredExtraction(context.Background(), nil, doc, slate); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("nil extractor must return ErrNotConfigured, got %v", err)
	}
	var typedNil EntityExtractorFunc
	if _, err := AnchoredExtraction(context.Background(), typedNil, doc, slate); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("typed-nil extractor must return ErrNotConfigured, got %v", err)
	}
	fake := &fakeExtractor{err: fmt.Errorf("adapter: %w", ErrNotConfigured)}
	if _, err := AnchoredExtraction(context.Background(), fake, doc, slate); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("configuration errors must escalate, got %v", err)
	}
}

func TestAnchoredExtractionContextDiscipline(t *testing.T) {
	doc, slate := extractionTestDocument()
	fake := &fakeExtractor{}
	if _, err := AnchoredExtraction(nil, fake, doc, slate); !errors.Is(err, ErrInvalidInput) { //nolint:staticcheck
		t.Fatalf("nil context must return ErrInvalidInput, got %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := AnchoredExtraction(canceled, fake, doc, slate); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled context must return its error, got %v", err)
	}
	if fake.calls != 0 {
		t.Fatal("a canceled context must not reach the extractor")
	}

	midflight, cancelMidflight := context.WithCancel(context.Background())
	interrupted := EntityExtractorFunc(func(context.Context, Document, []Candidate) (DocumentEntities, error) {
		cancelMidflight()
		return DocumentEntities{}, errors.New("interrupted")
	})
	if _, err := AnchoredExtraction(midflight, interrupted, doc, slate); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation during extraction must surface, got %v", err)
	}
}

func TestAnchoredExtractionShortCircuits(t *testing.T) {
	doc, slate := extractionTestDocument()
	cases := []struct {
		name  string
		doc   Document
		slate []Candidate
	}{
		{"empty slate", doc, nil},
		{"empty cleaned", Document{Title: doc.Title}, slate},
		{
			"oversized cleaned",
			Document{Title: doc.Title, Cleaned: strings.Repeat("a", MaxDocumentBytes+1)},
			slate,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake := &fakeExtractor{}
			got, err := AnchoredExtraction(context.Background(), fake, testCase.doc, testCase.slate)
			if err != nil {
				t.Fatalf("short circuit must not error: %v", err)
			}
			if fake.calls != 0 {
				t.Fatal("short circuit must not call the extractor")
			}
			if got.Primary != nil {
				t.Fatalf("short circuit must yield no primary, got %#v", got.Primary)
			}
		})
	}
}

func TestAnchoredExtractionTrimsBlindInput(t *testing.T) {
	slate := make([]Candidate, 0, MaxCandidates+6)
	for index := 0; index < MaxCandidates+6; index++ {
		name := fmt.Sprintf("Cand%02d", index)
		slate = append(slate, Candidate{Surface: name, Signal: SignalFrequency, Quote: name})
	}
	doc := Document{
		URL:     "https://example.com/deep/page",
		Title:   "Buy iPhone 15 Pro - Apple",
		Cleaned: strings.Repeat("a", MaxHeadWindowBytes) + " TAILMARKER appears beyond the window.",
		RawHTML: "<html><body>raw</body></html>",
	}
	fake := &fakeExtractor{result: DocumentEntities{Primary: &Entity{
		Name: "iPhone 15 Pro", Kind: KindProduct, Quote: "TAILMARKER",
	}}}
	got, err := AnchoredExtraction(context.Background(), fake, doc, slate)
	if err != nil {
		t.Fatalf("AnchoredExtraction returned error: %v", err)
	}
	if fake.doc.Cleaned != doc.Cleaned[:MaxHeadWindowBytes] {
		t.Fatal("the extractor must only see the head window")
	}
	if fake.doc.RawHTML != "" {
		t.Fatal("the extractor must never see raw HTML")
	}
	if fake.doc.Title != doc.Title || fake.doc.URL != doc.URL {
		t.Fatal("title and URL must pass through unchanged")
	}
	if len(fake.slate) != MaxCandidates {
		t.Fatalf("slate must cap at %d, got %d", MaxCandidates, len(fake.slate))
	}
	// The quote sits beyond the head window: gating runs on the full cleaned
	// text, so the primary survives.
	if got.Primary == nil || got.Primary.Quote != "TAILMARKER" {
		t.Fatalf("gates must anchor against the full cleaned text, got %#v", got.Primary)
	}
}

func TestAnchoredExtractionGatesSecondaries(t *testing.T) {
	mentions := make([]string, 0, MaxSecondary+4)
	secondary := make([]Entity, 0, MaxSecondary+4)
	for index := 1; index <= MaxSecondary+2; index++ {
		name := fmt.Sprintf("Second%02d", index)
		mentions = append(mentions, name)
		secondary = append(secondary, Entity{Name: name, Kind: KindOrganization})
	}
	secondary = append(secondary,
		Entity{Name: "Phantom Entity", Kind: KindOrganization},
		Entity{Name: "widget pro", Kind: KindProduct},
	)
	doc := Document{
		Title:   "Widget Pro",
		Cleaned: "Widget Pro overview. " + strings.Join(mentions, " "),
	}
	slate := []Candidate{{Surface: "Widget Pro", Signal: SignalTitle, Quote: "Widget Pro"}}
	fake := &fakeExtractor{result: DocumentEntities{
		Primary:   &Entity{Name: "Widget Pro", Kind: KindProduct},
		Secondary: secondary,
	}}
	got, err := AnchoredExtraction(context.Background(), fake, doc, slate)
	if err != nil {
		t.Fatalf("AnchoredExtraction returned error: %v", err)
	}
	if got.Primary == nil {
		t.Fatal("primary must survive")
	}
	if len(got.Secondary) != MaxSecondary {
		t.Fatalf("secondaries must cap at %d, got %d", MaxSecondary, len(got.Secondary))
	}
	for _, entity := range got.Secondary {
		if entity.Name == "Phantom Entity" {
			t.Fatal("unanchored secondary must be dropped")
		}
		if Normalize(entity.Name) == Normalize(got.Primary.Name) {
			t.Fatal("a secondary duplicating the primary must be dropped")
		}
	}
}

func TestDecodeExtractionReply(t *testing.T) {
	valid := `{"primary":{"name":"Apple","kind":"organization","aliases":["AAPL"],` +
		`"quote":"Apple Inc"},"reason_if_none":null}`
	got, err := DecodeExtractionReply([]byte(valid))
	if err != nil {
		t.Fatalf("valid reply must decode: %v", err)
	}
	want := DocumentEntities{Primary: &Entity{
		Name: "Apple", Kind: KindOrganization, Aliases: []string{"AAPL"}, Quote: "Apple Inc",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decode mismatch\n got: %#v\nwant: %#v", got.Primary, want.Primary)
	}

	nullPrimary, err := DecodeExtractionReply([]byte(`{"primary":null,"reason_if_none":"list page"}`))
	if err != nil || nullPrimary.Primary != nil {
		t.Fatalf("null primary must decode to an empty result, got (%#v, %v)", nullPrimary, err)
	}

	coerced, err := DecodeExtractionReply([]byte(
		`{"primary":{"name":"X","kind":"banana","aliases":null,"quote":"X"},"reason_if_none":null}`,
	))
	if err != nil || coerced.Primary == nil || coerced.Primary.Kind != KindOther {
		t.Fatalf("unknown kind must coerce to other, got (%#v, %v)", coerced.Primary, err)
	}
	if coerced.Primary.Aliases != nil {
		t.Fatalf("null aliases must decode to nil, got %#v", coerced.Primary.Aliases)
	}

	invalid := [][]byte{
		nil,
		[]byte(`{oops`),
		[]byte(`{"primary":null,"reason_if_none":null} trailing`),
		[]byte(strings.Repeat("x", MaxExtractionReplyBytes+1)),
	}
	for index, raw := range invalid {
		if _, err := DecodeExtractionReply(raw); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid reply %d must return ErrInvalidInput, got %v", index, err)
		}
	}
}

// The wire schema must stay in lockstep with the Go contract: its alias cap
// is MaxAliases and its kind enum is exactly the Kind literals.
func TestExtractionReplySchemaMatchesContract(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(ExtractionReplySchema), &schema); err != nil {
		t.Fatalf("schema constant must be valid JSON: %v", err)
	}
	primary := schema["properties"].(map[string]any)["primary"].(map[string]any)
	properties := primary["properties"].(map[string]any)
	maxItems := properties["aliases"].(map[string]any)["maxItems"].(float64)
	if int(maxItems) != MaxAliases {
		t.Fatalf("schema alias cap %d must equal MaxAliases %d", int(maxItems), MaxAliases)
	}
	enum := properties["kind"].(map[string]any)["enum"].([]any)
	wantKinds := map[string]struct{}{
		string(KindOrganization): {}, string(KindPerson): {}, string(KindProduct): {},
		string(KindPlace): {}, string(KindEvent): {}, string(KindWork): {},
		string(KindSubstance): {}, string(KindOther): {},
	}
	if len(enum) != len(wantKinds) {
		t.Fatalf("schema kind enum has %d entries, want %d", len(enum), len(wantKinds))
	}
	for _, kind := range enum {
		if _, ok := wantKinds[kind.(string)]; !ok {
			t.Fatalf("schema kind %q is not a Kind literal", kind)
		}
	}
}

func TestBuildExtractionInput(t *testing.T) {
	doc, slate := extractionTestDocument()
	first := BuildExtractionInput(doc, slate)
	second := BuildExtractionInput(doc, slate)
	if first != second {
		t.Fatal("extraction input must be deterministic")
	}
	for _, fragment := range []string{
		"URL: https://example.com/products/iphone-15-pro",
		"TITLE: Buy iPhone 15 Pro - Apple",
		"CANDIDATES:\n1. \"iPhone 15 Pro\" (title)\n2. \"Apple\" (title)\n",
		"CONTENT:\n" + doc.Cleaned,
	} {
		if !strings.Contains(first, fragment) {
			t.Fatalf("extraction input is missing %q in:\n%s", fragment, first)
		}
	}

	oversized := make([]Candidate, 0, MaxCandidates+2)
	for index := 0; index < MaxCandidates+2; index++ {
		name := fmt.Sprintf("Cand%02d", index)
		oversized = append(oversized, Candidate{Surface: name, Signal: SignalFrequency, Quote: name})
	}
	capped := BuildExtractionInput(doc, oversized)
	if !strings.Contains(capped, fmt.Sprintf("%d. ", MaxCandidates)) {
		t.Fatalf("input must include candidate %d", MaxCandidates)
	}
	if strings.Contains(capped, fmt.Sprintf("%d. ", MaxCandidates+1)) {
		t.Fatalf("input must cap the slate at %d candidates", MaxCandidates)
	}
}

func TestEntityExtractorFuncNilGuard(t *testing.T) {
	var fn EntityExtractorFunc
	if _, err := fn.ExtractEntities(context.Background(), Document{}, nil); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("nil function must return ErrNotConfigured, got %v", err)
	}
}
