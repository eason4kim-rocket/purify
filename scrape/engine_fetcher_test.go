package scrape

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/use-agent/purify/engine"
	"github.com/use-agent/purify/models"
)

type engineFixture struct {
	name      string
	supported bool
	seen      *engine.FetchRequest
	result    *engine.FetchResult
	err       error
}

func (fixture *engineFixture) Name() string { return fixture.name }

func (fixture *engineFixture) Supports(request *engine.FetchRequest) bool {
	fixture.seen = request
	return fixture.supported
}

func (fixture *engineFixture) Fetch(_ context.Context, request *engine.FetchRequest) (*engine.FetchResult, error) {
	fixture.seen = request
	return fixture.result, fixture.err
}

func TestEngineFetcherMapsCompleteRequestWithoutRetainingCallerState(t *testing.T) {
	wait := true
	request := &models.ScrapeRequest{
		URL:                "https://example.test/page",
		WaitForNetworkIdle: &wait,
		Timeout:            17,
		Stealth:            true,
		ProxyURL:           "socks5://proxy.test:1080",
		Headers:            map[string]string{"X-Test": "header"},
		Cookies:            []models.Cookie{{Name: "session", Value: "cookie", Domain: "example.test", Path: "/"}},
		Actions:            []models.Action{{Type: "execute_js", Code: "() => true"}},
		RemoveOverlays:     true,
		BlockAds:           true,
		CDPURL:             "ws://browser.test/devtools/browser/id",
	}
	backend := &engineFixture{name: "rod-stealth", supported: true}
	fetcher, err := NewEngineFetcher(backend)
	if err != nil {
		t.Fatal(err)
	}
	if !fetcher.Supports(request) {
		t.Fatal("Supports() = false")
	}
	want := scraperFetchRequestForTest(request)
	if !reflect.DeepEqual(backend.seen, want) {
		t.Fatalf("mapped request\n got: %#v\nwant: %#v", backend.seen, want)
	}

	backend.seen.Headers["X-Test"] = "mutated"
	backend.seen.Cookies[0].Value = "mutated"
	backend.seen.Actions[0].Code = "mutated"
	*backend.seen.WaitForNetworkIdle = false
	if request.Headers["X-Test"] != "header" || request.Cookies[0].Value != "cookie" ||
		request.Actions[0].Code != "() => true" || !*request.WaitForNetworkIdle {
		t.Fatal("engine adapter retained caller-owned request state")
	}
}

func TestEngineFetcherMapsHTTPAndBrowserResults(t *testing.T) {
	for _, test := range []struct {
		name        string
		backendName string
		resultName  string
		wantMethod  string
	}{
		{name: "http", backendName: "http", wantMethod: "http"},
		{name: "ordinary browser", backendName: "rod", wantMethod: "browser"},
		{name: "stealth attribution", backendName: "rod", resultName: "rod-stealth", wantMethod: "browser"},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &engineFixture{name: test.backendName, supported: true, result: &engine.FetchResult{
				HTML: "<html><body>content</body></html>", Title: "title", StatusCode: 200,
				FinalURL: "https://final.example/page", EngineName: test.resultName, ContentType: "text/html",
			}}
			fetcher, err := NewEngineFetcher(backend)
			if err != nil {
				t.Fatal(err)
			}
			result, err := fetcher.Fetch(context.Background(), &models.ScrapeRequest{URL: "https://example.test", Timeout: 3})
			if err != nil {
				t.Fatal(err)
			}
			wantEngine := test.resultName
			if wantEngine == "" {
				wantEngine = test.backendName
			}
			if result.EngineUsed != wantEngine || result.FetchMethod != test.wantMethod ||
				result.FinalURL != backend.result.FinalURL || result.StatusCode != 200 {
				t.Fatalf("mapped result = %#v", result)
			}
		})
	}
}

func TestEngineFetcherRejectsInvalidConfigurationAndNilResults(t *testing.T) {
	if _, err := NewEngineFetcher(nil); err == nil {
		t.Fatal("NewEngineFetcher(nil) error = nil")
	}
	backend := &engineFixture{name: "http", supported: true}
	fetcher, err := NewEngineFetcher(backend)
	if err != nil {
		t.Fatal(err)
	}
	if fetcher.Supports(nil) {
		t.Fatal("Supports(nil) = true")
	}
	if _, err := fetcher.Fetch(context.Background(), nil); err == nil {
		t.Fatal("Fetch(nil) error = nil")
	}
	if _, err := fetcher.Fetch(context.Background(), &models.ScrapeRequest{URL: "https://example.test"}); err == nil {
		t.Fatal("Fetch() accepted nil engine result")
	}

	backend.err = errors.New("network failed")
	if _, err := fetcher.Fetch(context.Background(), &models.ScrapeRequest{URL: "https://example.test"}); !errors.Is(err, backend.err) {
		t.Fatalf("Fetch() error = %v", err)
	}
}

func scraperFetchRequestForTest(request *models.ScrapeRequest) *engine.FetchRequest {
	wait := *request.WaitForNetworkIdle
	return &engine.FetchRequest{
		URL:                request.URL,
		Headers:            map[string]string{"X-Test": "header"},
		Cookies:            []http.Cookie{{Name: "session", Value: "cookie", Domain: "example.test", Path: "/"}},
		Timeout:            17 * time.Second,
		ProxyURL:           request.ProxyURL,
		Stealth:            true,
		WaitForNetworkIdle: &wait,
		RemoveOverlays:     true,
		BlockAds:           true,
		Actions:            []engine.Action{{Type: "execute_js", Code: "() => true"}},
		CDPURL:             request.CDPURL,
	}
}
