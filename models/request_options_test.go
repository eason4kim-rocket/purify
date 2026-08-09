package models

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin/binding"
)

func TestScrapeOptionsFieldContractMatchesScrapeRequest(t *testing.T) {
	requestType := reflect.TypeOf(ScrapeRequest{})
	optionsType := reflect.TypeOf(ScrapeOptions{})

	if requestType.NumField() != optionsType.NumField()+1 {
		t.Fatalf("ScrapeRequest fields = %d, ScrapeOptions fields = %d; want URL plus exactly the shared options", requestType.NumField(), optionsType.NumField())
	}
	urlField, ok := requestType.FieldByName("URL")
	if !ok || urlField.Tag.Get("json") != "url" {
		t.Fatalf("ScrapeRequest.URL contract = %#v", urlField)
	}
	if _, ok := optionsType.FieldByName("URL"); ok {
		t.Fatal("ScrapeOptions must not contain URL")
	}

	for index := 0; index < optionsType.NumField(); index++ {
		optionField := optionsType.Field(index)
		if requestType.Field(index+1).Name != optionField.Name {
			t.Fatalf("option order differs at %d: request=%s options=%s", index, requestType.Field(index+1).Name, optionField.Name)
		}
		requestField, ok := requestType.FieldByName(optionField.Name)
		if !ok {
			t.Fatalf("ScrapeOptions.%s has no ScrapeRequest counterpart", optionField.Name)
		}
		if requestField.Type != optionField.Type {
			t.Fatalf("%s type differs: request=%v options=%v", optionField.Name, requestField.Type, optionField.Type)
		}
		for _, tag := range []string{"json", "binding"} {
			if requestField.Tag.Get(tag) != optionField.Tag.Get(tag) {
				t.Fatalf("%s %s tag differs: request=%q options=%q", optionField.Name, tag, requestField.Tag.Get(tag), optionField.Tag.Get(tag))
			}
		}
		if strings.Split(optionField.Tag.Get("json"), ",")[0] == "url" {
			t.Fatalf("ScrapeOptions.%s exposes the reserved URL field", optionField.Name)
		}
	}

	if reflect.TypeOf(BatchOptions{}) != optionsType {
		t.Fatal("BatchOptions is no longer an alias of ScrapeOptions")
	}
	if reflect.TypeOf(CrawlOptions{}) != optionsType {
		t.Fatal("CrawlOptions is no longer an alias of ScrapeOptions")
	}

	// Compile-time source-compatibility guard for existing direct-field callers.
	request := ScrapeRequest{URL: "https://example.test", Timeout: 17, Headers: map[string]string{"X-Test": "one"}}
	if request.Timeout != 17 || request.Headers["X-Test"] != "one" {
		t.Fatalf("direct ScrapeRequest literal = %#v", request)
	}
}

func TestScrapeOptionsKeepSingleFlatAndBatchCrawlNestedJSON(t *testing.T) {
	options := populatedScrapeOptions()
	tests := []struct {
		name       string
		value      any
		wantURL    bool
		wantNested bool
	}{
		{
			name: "single remains flat",
			value: func() ScrapeRequest {
				request := ScrapeRequest{URL: "https://single.example/page"}
				ApplyScrapeOptions(&request, options)
				return request
			}(),
			wantURL: true,
		},
		{
			name:       "batch remains nested",
			value:      BatchRequest{URLs: []string{"https://batch.example/page"}, Options: options},
			wantNested: true,
		},
		{
			name:       "crawl remains nested",
			value:      CrawlRequest{URL: "https://crawl.example/page", Options: options},
			wantURL:    true,
			wantNested: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := json.Marshal(test.value)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			var document map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &document); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			_, hasURL := document["url"]
			if hasURL != test.wantURL {
				t.Fatalf("top-level url presence = %v, want %v; JSON=%s", hasURL, test.wantURL, encoded)
			}
			_, hasTopLevelTimeout := document["timeout"]
			if test.wantNested {
				if hasTopLevelTimeout {
					t.Fatalf("nested API leaked timeout to top level: %s", encoded)
				}
				var nested map[string]json.RawMessage
				if err := json.Unmarshal(document["options"], &nested); err != nil {
					t.Fatalf("decode options: %v; JSON=%s", err, encoded)
				}
				if _, hasNestedURL := nested["url"]; hasNestedURL {
					t.Fatalf("nested options leaked URL: %s", encoded)
				}
				for _, field := range []string{"timeout", "headers", "cookies", "actions", "include_tags", "exclude_tags", "max_age"} {
					if _, ok := nested[field]; !ok {
						t.Fatalf("nested options missing %q: %s", field, encoded)
					}
				}
				return
			}
			if !hasTopLevelTimeout {
				t.Fatalf("single API no longer has flat options: %s", encoded)
			}
			if _, hasOptions := document["options"]; hasOptions {
				t.Fatalf("single API unexpectedly gained nested options: %s", encoded)
			}
		})
	}
}

func TestSharedScrapeOptionsUseSingleBindingContract(t *testing.T) {
	valid := populatedScrapeOptions()
	valid.OutputFormat = "markdown_citations"
	valid.ExtractMode = "auto"

	tests := []struct {
		name    string
		request any
		wantErr bool
	}{
		{name: "single valid", request: func() ScrapeRequest {
			request := ScrapeRequest{URL: "https://single.example/page"}
			ApplyScrapeOptions(&request, valid)
			return request
		}()},
		{name: "batch valid", request: BatchRequest{URLs: []string{"https://batch.example/page"}, Options: valid}},
		{name: "crawl valid", request: CrawlRequest{URL: "https://crawl.example/page", Options: valid}},
		{name: "batch invalid output", request: BatchRequest{URLs: []string{"https://batch.example/page"}, Options: BatchOptions{OutputFormat: "pdf"}}, wantErr: true},
		{name: "crawl invalid mode", request: CrawlRequest{URL: "https://crawl.example/page", Options: CrawlOptions{ExtractMode: "unknown"}}, wantErr: true},
		{name: "batch invalid timeout", request: BatchRequest{URLs: []string{"https://batch.example/page"}, Options: BatchOptions{Timeout: 121}}, wantErr: true},
		{name: "crawl invalid action", request: CrawlRequest{URL: "https://crawl.example/page", Options: CrawlOptions{Actions: []Action{{Type: "unknown"}}}}, wantErr: true},
		{name: "single invalid max age", request: ScrapeRequest{URL: "https://single.example/page", MaxAge: -1}, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := binding.Validator.ValidateStruct(test.request)
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateStruct() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestCloneScrapeOptionsDeepCopiesMutableFieldsAndPreservesOrder(t *testing.T) {
	source := populatedScrapeOptions()
	want := populatedScrapeOptions()
	cloned := CloneScrapeOptions(source)
	if !reflect.DeepEqual(cloned, want) {
		t.Fatalf("CloneScrapeOptions() = %#v, want %#v", cloned, want)
	}

	*source.WaitForNetworkIdle = true
	*source.OnlyMainContent = true
	source.Headers["X-First"] = "mutated-source"
	source.Cookies[0].Value = "mutated-source"
	source.Actions[0].Selector = "#mutated-source"
	source.IncludeTags[0] = ".mutated-source"
	source.ExcludeTags[0] = ".mutated-source"
	if !reflect.DeepEqual(cloned, want) {
		t.Fatalf("source mutation changed clone: got %#v, want %#v", cloned, want)
	}

	cloned.Headers["X-Second"] = "mutated-clone"
	cloned.Cookies[1].Value = "mutated-clone"
	cloned.Actions[1].Code = "mutated-clone"
	cloned.IncludeTags[1] = ".mutated-clone"
	cloned.ExcludeTags[1] = ".mutated-clone"
	if source.Headers["X-Second"] == "mutated-clone" || source.Cookies[1].Value == "mutated-clone" ||
		source.Actions[1].Code == "mutated-clone" || source.IncludeTags[1] == ".mutated-clone" ||
		source.ExcludeTags[1] == ".mutated-clone" {
		t.Fatal("clone mutation changed source")
	}

	if got := []string{want.Actions[0].Type, want.Actions[1].Type}; !reflect.DeepEqual(got, []string{"click", "execute_js"}) {
		t.Fatalf("action order = %v", got)
	}
	if got := want.IncludeTags; !reflect.DeepEqual(got, []string{"main", "article"}) {
		t.Fatalf("include order = %v", got)
	}
	if got := want.ExcludeTags; !reflect.DeepEqual(got, []string{"nav", ".ad"}) {
		t.Fatalf("exclude order = %v", got)
	}
}

func TestScrapeOptionConversionsAreDetachedAndLeaveURLAlone(t *testing.T) {
	options := populatedScrapeOptions()
	want := populatedScrapeOptions()
	fixtureValue := reflect.ValueOf(options)
	fixtureType := fixtureValue.Type()
	for index := 0; index < fixtureValue.NumField(); index++ {
		if fixtureValue.Field(index).IsZero() {
			t.Fatalf("populatedScrapeOptions.%s is zero; every shared field needs a round-trip sentinel", fixtureType.Field(index).Name)
		}
	}
	request := ScrapeRequest{
		URL:         "https://example.test/page",
		Timeout:     99,
		Headers:     map[string]string{"stale": "value"},
		MaxAge:      999,
		BlockAds:    false,
		ExtractMode: "raw",
	}
	ApplyScrapeOptions(&request, options)
	if request.URL != "https://example.test/page" {
		t.Fatalf("ApplyScrapeOptions changed URL to %q", request.URL)
	}
	if got := ScrapeOptionsFromRequest(&request); !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %#v, want %#v", got, want)
	}

	options.Headers["X-First"] = "mutated"
	options.Cookies[0].Value = "mutated"
	options.Actions[0].Selector = "#mutated"
	options.IncludeTags[0] = ".mutated"
	options.ExcludeTags[0] = ".mutated"
	*options.WaitForNetworkIdle = true
	*options.OnlyMainContent = true
	if got := ScrapeOptionsFromRequest(&request); !reflect.DeepEqual(got, want) {
		t.Fatalf("input mutation changed applied request: got %#v, want %#v", got, want)
	}

	extracted := ScrapeOptionsFromRequest(&request)
	extracted.Headers["X-First"] = "changed-extracted"
	extracted.Cookies[0].Value = "changed-extracted"
	extracted.Actions[0].Selector = "#changed-extracted"
	extracted.IncludeTags[0] = ".changed-extracted"
	extracted.ExcludeTags[0] = ".changed-extracted"
	*extracted.WaitForNetworkIdle = true
	*extracted.OnlyMainContent = true
	if got := ScrapeOptionsFromRequest(&request); !reflect.DeepEqual(got, want) {
		t.Fatalf("extracted mutation changed request: got %#v, want %#v", got, want)
	}

	ApplyScrapeOptions(&request, ScrapeOptions{})
	if request.URL != "https://example.test/page" || !reflect.DeepEqual(ScrapeOptionsFromRequest(&request), ScrapeOptions{}) {
		t.Fatalf("zero options did not replace prior values: %#v", request)
	}
	ApplyScrapeOptions(nil, options)
	if got := ScrapeOptionsFromRequest(nil); !reflect.DeepEqual(got, ScrapeOptions{}) {
		t.Fatalf("ScrapeOptionsFromRequest(nil) = %#v", got)
	}
}

func populatedScrapeOptions() ScrapeOptions {
	waitForNetworkIdle := false
	onlyMainContent := false
	return ScrapeOptions{
		WaitForNetworkIdle: &waitForNetworkIdle,
		Timeout:            47,
		Stealth:            true,
		ProxyURL:           "https://proxy.example:8443",
		OutputFormat:       "markdown_citations",
		ExtractMode:        "pruning",
		CSSSelector:        "main.content",
		Headers: map[string]string{
			"X-First":  "one",
			"X-Second": "two",
		},
		Cookies: []Cookie{
			{Name: "first", Value: "one", Domain: "example.test", Path: "/"},
			{Name: "second", Value: "two", Domain: "example.test", Path: "/docs"},
		},
		Actions: []Action{
			{Type: "click", Selector: "#load-more"},
			{Type: "execute_js", Code: "() => document.title"},
		},
		IncludeTags:     []string{"main", "article"},
		ExcludeTags:     []string{"nav", ".ad"},
		OnlyMainContent: &onlyMainContent,
		RemoveOverlays:  true,
		BlockAds:        true,
		CDPURL:          "wss://browser.example/devtools/browser/id",
		MaxAge:          12_345,
	}
}
