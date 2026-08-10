package eav

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func readHarvestFixture(t testing.TB, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "harvest", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return string(raw)
}

type harvestCase struct {
	name string
	doc  Document
	want []Candidate
}

func harvestCases(t testing.TB) []harvestCase {
	return []harvestCase{
		{
			name: "ecommerce product page",
			doc: Document{
				URL:   "https://www.apple.com/shop/buy-iphone/iphone-15-pro",
				Title: "Buy iPhone 15 Pro - Apple",
				Cleaned: "iPhone 15 Pro. Titanium. So strong. So light. So Pro. " +
					"From $999 or $41.62/mo. for 24 mo. Add to Bag",
				RawHTML: readHarvestFixture(t, "ecommerce.html"),
			},
			want: []Candidate{
				{Surface: "Buy iPhone 15 Pro", Signal: SignalTitle, Quote: "Buy iPhone 15 Pro"},
				{Surface: "Apple", Signal: SignalTitle, Quote: "Apple"},
				{Surface: "iPhone 15 Pro", Signal: SignalH1, Quote: "iPhone 15 Pro"},
			},
		},
		{
			name: "news article",
			doc: Document{
				URL:   "https://exampletimes.example/business/nvidia-earnings-q2.html",
				Title: "Nvidia tops quarterly earnings expectations | Example Times",
				Cleaned: "Nvidia tops quarterly earnings expectations " +
					"Nvidia reported quarterly revenue well above analyst estimates, " +
					"driven by sustained demand for its Blackwell accelerators. " +
					"Nvidia said data-center sales more than doubled as cloud providers " +
					"raced to deploy Blackwell clusters. Analysts had expected slower " +
					"Blackwell shipments. Shares of Nvidia rose in extended trading.",
				RawHTML: readHarvestFixture(t, "news.html"),
			},
			want: []Candidate{
				{
					Surface: "Nvidia tops quarterly earnings expectations",
					Signal:  SignalTitle,
					Quote:   "Nvidia tops quarterly earnings expectations",
				},
				{Surface: "Example Times", Signal: SignalTitle, Quote: "Example Times"},
				{Surface: "Nvidia", Signal: SignalJSONLD, Quote: "Nvidia"},
				{Surface: "Blackwell", Signal: SignalFrequency, Quote: "Blackwell"},
			},
		},
		{
			name: "corporate homepage",
			doc: Document{
				URL:   "https://www.catl-ex.example/cn/",
				Title: "宁德时代新能源科技股份有限公司",
				Cleaned: "宁德时代新能源科技股份有限公司成立于2011年，总部位于福建宁德。" +
					"宁德时代专注于新能源汽车动力电池系统与储能系统的研发、生产和销售。" +
					"截至2026年，宁德时代动力电池使用量连续多年位居全球第一。",
				RawHTML: readHarvestFixture(t, "corporate.html"),
			},
			want: []Candidate{
				{
					Surface: "宁德时代新能源科技股份有限公司",
					Signal:  SignalTitle,
					Quote:   "宁德时代新能源科技股份有限公司",
				},
				{Surface: "宁德时代", Signal: SignalH1, Quote: "宁德时代"},
			},
		},
		{
			name: "wikipedia biography",
			doc: Document{
				URL:   "https://en.wikipedia.example/wiki/Tim_Cook",
				Title: "Tim Cook - Wikipedia",
				Cleaned: "Timothy Donald Cook (born November 1, 1960) is an American " +
					"business executive who has been the chief executive officer of " +
					"Apple Inc. since 2011. Cook joined Apple in March 1998 as a senior " +
					"vice president for worldwide operations.",
				RawHTML: readHarvestFixture(t, "wikipedia.html"),
			},
			want: []Candidate{
				{Surface: "Tim Cook", Signal: SignalTitle, Quote: "Tim Cook"},
				{Surface: "Wikipedia", Signal: SignalTitle, Quote: "Wikipedia"},
				{
					Surface: "Timothy Donald Cook",
					Signal:  SignalJSONLD,
					Quote:   "Timothy Donald Cook",
				},
			},
		},
		{
			// A list page must not yield any single dominant product entity:
			// ItemList subtrees are never traversed and one mention is below
			// every frequency threshold, which keeps Primary=nil reachable.
			name: "list page",
			doc: Document{
				URL:   "https://techreviewhub.example/best-smartphones-2026",
				Title: "Best smartphones of 2026 | Tech Review Hub",
				Cleaned: "Best smartphones of 2026 Our testing team compared this " +
					"year's flagships across cameras, battery life, and value. " +
					"Galaxy S26 Ultra: the most complete camera system we tested. " +
					"iPhone 17: the strongest video pipeline and ecosystem. " +
					"Pixel 11 Pro: the best computational photography for the price.",
				RawHTML: readHarvestFixture(t, "list.html"),
			},
			want: []Candidate{
				{
					Surface: "Best smartphones of 2026",
					Signal:  SignalTitle,
					Quote:   "Best smartphones of 2026",
				},
				{Surface: "Tech Review Hub", Signal: SignalTitle, Quote: "Tech Review Hub"},
			},
		},
	}
}

func TestHarvestCandidatesFixtures(t *testing.T) {
	for _, testCase := range harvestCases(t) {
		t.Run(testCase.name, func(t *testing.T) {
			got := HarvestCandidates(testCase.doc)
			if !reflect.DeepEqual(got, testCase.want) {
				t.Fatalf("slate mismatch\n got: %#v\nwant: %#v", got, testCase.want)
			}
		})
	}
}

// Every candidate must anchor verbatim, normalize uniquely, use a known
// signal, and respect the slate cap — across all fixtures, twice, so a
// nondeterministic map ordering cannot hide.
func TestHarvestCandidatesInvariants(t *testing.T) {
	signals := map[string]struct{}{
		SignalTitle: {}, SignalH1: {}, SignalOGTitle: {}, SignalOGSiteName: {},
		SignalJSONLD: {}, SignalSlug: {}, SignalFrequency: {},
	}
	for _, testCase := range harvestCases(t) {
		first := HarvestCandidates(testCase.doc)
		second := HarvestCandidates(testCase.doc)
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("%s: harvest is not deterministic", testCase.name)
		}
		if len(first) > MaxCandidates {
			t.Fatalf("%s: slate exceeds MaxCandidates", testCase.name)
		}
		seen := make(map[string]struct{}, len(first))
		for _, candidate := range first {
			if !strings.Contains(testCase.doc.Title, candidate.Quote) &&
				!strings.Contains(testCase.doc.Cleaned, candidate.Quote) {
				t.Fatalf("%s: quote %q is not anchored", testCase.name, candidate.Quote)
			}
			if _, ok := signals[candidate.Signal]; !ok {
				t.Fatalf("%s: unknown signal %q", testCase.name, candidate.Signal)
			}
			key := Normalize(candidate.Surface)
			if key == "" {
				t.Fatalf("%s: surface %q normalizes to nothing", testCase.name, candidate.Surface)
			}
			if _, duplicate := seen[key]; duplicate {
				t.Fatalf("%s: duplicate normalized surface %q", testCase.name, key)
			}
			seen[key] = struct{}{}
		}
	}
}

func TestHarvestDropsUnanchoredCandidates(t *testing.T) {
	doc := Document{
		Title:   "Daily bulletin",
		Cleaned: "General news about markets.",
		RawHTML: `<html><head><title>ignored</title>` +
			`<meta property="og:title" content="Ghost Entity Prime"></head>` +
			`<body><h1>Phantom Heading</h1></body></html>`,
	}
	want := []Candidate{{Surface: "Daily bulletin", Signal: SignalTitle, Quote: "Daily bulletin"}}
	if got := HarvestCandidates(doc); !reflect.DeepEqual(got, want) {
		t.Fatalf("unanchored candidates must be dropped\n got: %#v\nwant: %#v", got, want)
	}
}

func TestHarvestSlugAdoptsDocumentCasing(t *testing.T) {
	doc := Document{
		URL:     "https://frame.work/products/framework-laptop-16",
		Cleaned: "The Framework Laptop 16 is a modular, repairable laptop.",
	}
	want := []Candidate{
		{Surface: "Framework Laptop 16", Signal: SignalSlug, Quote: "Framework Laptop 16"},
	}
	if got := HarvestCandidates(doc); !reflect.DeepEqual(got, want) {
		t.Fatalf("slug must adopt document casing\n got: %#v\nwant: %#v", got, want)
	}
}

func TestHarvestCapsSlate(t *testing.T) {
	segments := []string{
		"Alpha One", "Alpha Two", "Alpha Three", "Alpha Four", "Alpha Five",
		"Alpha Six", "Alpha Seven",
	}
	graph := make([]string, 0, maxJSONLDNames)
	names := make([]string, 0, 32)
	for index := 0; index < maxJSONLDNames; index++ {
		name := "Entity Number " + string(rune('A'+index))
		graph = append(graph, `{"@type":"Organization","name":"`+name+`"}`)
		names = append(names, name)
	}
	rawHTML := `<html><head>` +
		`<meta property="og:title" content="Og Title Alpha">` +
		`<meta property="og:site_name" content="Og Site Beta">` +
		`<script type="application/ld+json">{"@graph":[` + strings.Join(graph, ",") + `]}</script>` +
		`</head><body><h1>Heading One</h1><h1>Heading Two</h1></body></html>`
	doc := Document{
		Title: strings.Join(segments, " | "),
		Cleaned: "Heading One Heading Two Og Title Alpha Og Site Beta " +
			strings.Join(names, " "),
		RawHTML: rawHTML,
	}
	got := HarvestCandidates(doc)
	if len(got) != MaxCandidates {
		t.Fatalf("slate length = %d, want the MaxCandidates cap %d", len(got), MaxCandidates)
	}
}

func TestTitleSegments(t *testing.T) {
	cases := []struct {
		name  string
		title string
		want  []string
	}{
		{"spaced hyphen", "Buy iPhone 15 Pro - Apple", []string{"Buy iPhone 15 Pro", "Apple"}},
		{"wikipedia style", "Tim Cook - Wikipedia", []string{"Tim Cook", "Wikipedia"}},
		{
			"pipe", "AMD Ryzen 9 7950X Review | Tom's Hardware",
			[]string{"AMD Ryzen 9 7950X Review", "Tom's Hardware"},
		},
		{"underscore portal", "小米14 Pro_小米商城", []string{"小米14 Pro", "小米商城"}},
		{"fullwidth pipe", "深度｜阿里巴巴的下一个十年", []string{"深度", "阿里巴巴的下一个十年"}},
		{
			"spaced en dashes", "Breaking – markets rally – live",
			[]string{"Breaking", "markets rally", "live"},
		},
		{
			"bare hyphen kept", "Mercedes-Benz International: news",
			[]string{"Mercedes-Benz International: news"},
		},
		{"empty", "", nil},
		{"blank", "   ", nil},
		{
			"segment cap", "A | B | C | D | E | F | G | H",
			[]string{"A", "B", "C", "D", "E", "F"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := titleSegments(testCase.title)
			if !reflect.DeepEqual(got, testCase.want) {
				t.Fatalf("titleSegments(%q) = %#v, want %#v", testCase.title, got, testCase.want)
			}
		})
	}
}

func TestSlugPhrase(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"product slug", "https://frame.work/products/framework-laptop-16", "framework laptop 16"},
		{"extension cut", "https://example.com/news/nvidia-earnings-q2.html", "nvidia earnings q2"},
		{"trailing slash", "https://example.com/iphone-15-pro/", "iphone 15 pro"},
		{"percent decoded", "https://example.com/people/tim%20cook", "tim cook"},
		{"numeric id", "https://example.com/p/12345", ""},
		{"hex id", "https://example.com/a/9f8e7d6c5b4a3210deadbeefcafe1234", ""},
		{"structural index", "https://example.com/docs/index.html", ""},
		{"root", "https://example.com/", ""},
		{"unparsable", "://bad url", ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := slugPhrase(testCase.url); got != testCase.want {
				t.Fatalf("slugPhrase(%q) = %q, want %q", testCase.url, got, testCase.want)
			}
		})
	}
}

func TestFrequencyPhrases(t *testing.T) {
	t.Run("latin repeated run", func(t *testing.T) {
		head := "Framework Laptop 16 review. Framework Laptop 16 pricing. " +
			"We tested the Framework Laptop 16."
		got := frequencyPhrases(head)
		if !reflect.DeepEqual(got, []string{"Framework Laptop 16"}) {
			t.Fatalf("frequencyPhrases = %#v, want the repeated run", got)
		}
	})
	t.Run("stopwords filtered", func(t *testing.T) {
		head := "However, prices fell. However, demand rose. However, supply lagged."
		if got := frequencyPhrases(head); len(got) != 0 {
			t.Fatalf("frequencyPhrases = %#v, want none", got)
		}
	})
	t.Run("cjk gram", func(t *testing.T) {
		head := "宁德时代发布新品。宁德时代股价上涨。市场看好宁德时代。"
		got := frequencyPhrases(head)
		if !reflect.DeepEqual(got, []string{"宁德时代"}) {
			t.Fatalf("frequencyPhrases = %#v, want the longest recurring gram", got)
		}
	})
}

func BenchmarkHarvestCandidates(b *testing.B) {
	var doc Document
	for _, testCase := range harvestCases(b) {
		if testCase.name == "news article" {
			doc = testCase.doc
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		HarvestCandidates(doc)
	}
}
