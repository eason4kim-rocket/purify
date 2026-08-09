package cache

import (
	"encoding/hex"
	"reflect"
	"testing"

	"github.com/use-agent/purify/models"
)

func TestKeyForRequestStableCanonicalCollections(t *testing.T) {
	wait := false
	onlyMain := false
	request := &models.ScrapeRequest{
		URL:                "https://example.com/products?q=go",
		WaitForNetworkIdle: &wait,
		Timeout:            45,
		Stealth:            true,
		ProxyURL:           "socks5://proxy.example:1080",
		OutputFormat:       "text",
		ExtractMode:        "pruning",
		CSSSelector:        "main.product",
		Headers: map[string]string{
			"X-Zeta":          "last",
			"Accept-Language": "en-US",
		},
		Cookies: []models.Cookie{
			{Name: "session", Value: "abc", Domain: "example.com", Path: "/"},
			{Name: "locale", Value: "en", Domain: "example.com", Path: "/products"},
		},
		Actions: []models.Action{
			{Type: "click", Selector: "#load"},
			{Type: "wait", Milliseconds: 250},
		},
		IncludeTags:     []string{"article", "main"},
		ExcludeTags:     []string{".ad", "nav"},
		OnlyMainContent: &onlyMain,
		RemoveOverlays:  true,
		BlockAds:        true,
		CDPURL:          "ws://browser.example/devtools/browser/id",
		MaxAge:          1_000,
	}
	originalCookies := cloneSlice(request.Cookies)
	originalActions := cloneSlice(request.Actions)
	originalInclude := cloneSlice(request.IncludeTags)
	originalExclude := cloneSlice(request.ExcludeTags)

	first := KeyForRequest(request)
	second := KeyForRequest(request)
	if first != second {
		t.Fatalf("KeyForRequest() unstable: %q != %q", first, second)
	}
	decoded, err := hex.DecodeString(first)
	if err != nil || len(decoded) != 32 {
		t.Fatalf("key %q is not a SHA-256 hex digest: %v", first, err)
	}
	if !reflect.DeepEqual(request.Cookies, originalCookies) ||
		!reflect.DeepEqual(request.Actions, originalActions) ||
		!reflect.DeepEqual(request.IncludeTags, originalInclude) ||
		!reflect.DeepEqual(request.ExcludeTags, originalExclude) {
		t.Fatal("KeyForRequest mutated caller collections")
	}

	reordered := *request
	reordered.Headers = map[string]string{
		"Accept-Language": "en-US",
		"X-Zeta":          "last",
	}
	if got := KeyForRequest(&reordered); got != first {
		t.Fatalf("header map insertion order changed key: got %q, want %q", got, first)
	}

	reordered.Cookies = []models.Cookie{request.Cookies[1], request.Cookies[0]}
	if got := KeyForRequest(&reordered); got == first {
		t.Fatal("ordered cookie sequence did not change key")
	}
	reordered.Cookies = cloneSlice(request.Cookies)
	reordered.Actions = []models.Action{request.Actions[1], request.Actions[0]}
	if got := KeyForRequest(&reordered); got == first {
		t.Fatal("ordered action sequence did not change key")
	}
	reordered.Actions = cloneSlice(request.Actions)
	reordered.IncludeTags = []string{"main", "article"}
	if got := KeyForRequest(&reordered); got == first {
		t.Fatal("ordered include selector sequence did not change key")
	}
	reordered.IncludeTags = cloneSlice(request.IncludeTags)
	reordered.ExcludeTags = []string{"nav", ".ad"}
	if got := KeyForRequest(&reordered); got == first {
		t.Fatal("ordered exclude selector sequence did not change key")
	}
	if RequestKey(request) != first {
		t.Fatal("RequestKey alias differs from KeyForRequest")
	}
}

func TestKeyForRequestSemanticDefaultsAndPolicyFields(t *testing.T) {
	defaultRequest := &models.ScrapeRequest{URL: "https://example.com"}
	wait := true
	explicitDefaults := &models.ScrapeRequest{
		URL:                "https://example.com",
		WaitForNetworkIdle: &wait,
		Timeout:            30,
		OutputFormat:       "markdown",
		ExtractMode:        "readability",
	}
	if KeyForRequest(defaultRequest) != KeyForRequest(explicitDefaults) {
		t.Fatal("implicit and explicit defaults produced different keys")
	}

	withMaxAge := *explicitDefaults
	withMaxAge.MaxAge = 987_654
	if KeyForRequest(&withMaxAge) != KeyForRequest(explicitDefaults) {
		t.Fatal("cache policy MaxAge unexpectedly changed output key")
	}

	falseValue := false
	legacyAlias := &models.ScrapeRequest{URL: "https://example.com", OnlyMainContent: &falseValue}
	rawMode := &models.ScrapeRequest{URL: "https://example.com", ExtractMode: "raw"}
	if KeyForRequest(legacyAlias) != KeyForRequest(rawMode) {
		t.Fatal("OnlyMainContent=false was not normalized into raw mode")
	}
	trueValue := true
	legacyDefault := &models.ScrapeRequest{URL: "https://example.com", OnlyMainContent: &trueValue}
	if KeyForRequest(legacyDefault) != KeyForRequest(defaultRequest) {
		t.Fatal("OnlyMainContent=true should be equivalent to the default mode")
	}

	if KeyForRequest(nil) != KeyForRequest(&models.ScrapeRequest{}) {
		t.Fatal("nil request key is not stable with an empty request")
	}
}

func TestKeyForRequestCoversEveryOutputAffectingField(t *testing.T) {
	base := &models.ScrapeRequest{URL: "https://example.com"}
	baseKey := KeyForRequest(base)
	falseValue := false
	tests := []struct {
		name   string
		mutate func(*models.ScrapeRequest)
	}{
		{name: "URL", mutate: func(r *models.ScrapeRequest) { r.URL = "https://other.example.com" }},
		{name: "WaitForNetworkIdle", mutate: func(r *models.ScrapeRequest) { r.WaitForNetworkIdle = &falseValue }},
		{name: "Timeout", mutate: func(r *models.ScrapeRequest) { r.Timeout = 31 }},
		{name: "Stealth", mutate: func(r *models.ScrapeRequest) { r.Stealth = true }},
		{name: "ProxyURL", mutate: func(r *models.ScrapeRequest) { r.ProxyURL = "http://proxy.example" }},
		{name: "OutputFormat", mutate: func(r *models.ScrapeRequest) { r.OutputFormat = "html" }},
		{name: "ExtractMode", mutate: func(r *models.ScrapeRequest) { r.ExtractMode = "raw" }},
		{name: "CSSSelector", mutate: func(r *models.ScrapeRequest) { r.CSSSelector = "main" }},
		{name: "Headers", mutate: func(r *models.ScrapeRequest) { r.Headers = map[string]string{"X-Test": "one"} }},
		{name: "Cookies", mutate: func(r *models.ScrapeRequest) {
			r.Cookies = []models.Cookie{{Name: "session", Value: "one", Domain: "example.com", Path: "/"}}
		}},
		{name: "Actions", mutate: func(r *models.ScrapeRequest) {
			r.Actions = []models.Action{{Type: "wait", Selector: "#ready", Milliseconds: 1, Direction: "down", Amount: 2, Code: "1"}}
		}},
		{name: "IncludeTags", mutate: func(r *models.ScrapeRequest) { r.IncludeTags = []string{"article"} }},
		{name: "ExcludeTags", mutate: func(r *models.ScrapeRequest) { r.ExcludeTags = []string{"nav"} }},
		{name: "OnlyMainContent", mutate: func(r *models.ScrapeRequest) { r.OnlyMainContent = &falseValue }},
		{name: "RemoveOverlays", mutate: func(r *models.ScrapeRequest) { r.RemoveOverlays = true }},
		{name: "BlockAds", mutate: func(r *models.ScrapeRequest) { r.BlockAds = true }},
		{name: "CDPURL", mutate: func(r *models.ScrapeRequest) { r.CDPURL = "ws://browser.example/devtools" }},
		{name: "MaximumBodyBytes", mutate: func(r *models.ScrapeRequest) { r.MaximumBodyBytes = 4 << 20 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := *base
			tt.mutate(&request)
			if got := KeyForRequest(&request); got == baseKey {
				t.Fatalf("changing %s did not change key", tt.name)
			}
		})
	}
}

func TestRequestKeyFieldClassificationStaysCurrent(t *testing.T) {
	assertFields(t, reflect.TypeOf(models.ScrapeRequest{}), []string{
		"URL", "WaitForNetworkIdle", "Timeout", "Stealth", "ProxyURL",
		"OutputFormat", "ExtractMode", "CSSSelector", "Headers", "Cookies",
		"Actions", "IncludeTags", "ExcludeTags", "OnlyMainContent",
		"RemoveOverlays", "BlockAds", "CDPURL", "MaxAge", "MaximumBodyBytes",
	})
	assertFields(t, reflect.TypeOf(models.Cookie{}), []string{"Name", "Value", "Domain", "Path"})
	assertFields(t, reflect.TypeOf(models.Action{}), []string{"Type", "Selector", "Milliseconds", "Direction", "Amount", "Code"})
}

func assertFields(t *testing.T, structure reflect.Type, expected []string) {
	t.Helper()
	actual := make([]string, structure.NumField())
	for index := 0; index < structure.NumField(); index++ {
		actual[index] = structure.Field(index).Name
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("%s fields changed: got %v, classified %v; update cache key policy", structure.Name(), actual, expected)
	}
}
