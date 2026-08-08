package scraper

import (
	"reflect"
	"testing"
	"time"

	"github.com/use-agent/purify/models"
)

func TestFetchRequestAdapterRoundTrip(t *testing.T) {
	wait := true
	request := &models.ScrapeRequest{
		URL:                "https://example.test/page",
		WaitForNetworkIdle: &wait,
		Timeout:            17,
		Stealth:            true,
		ProxyURL:           "http://proxy.test:8080",
		Headers:            map[string]string{"X-Test": "header"},
		Cookies:            []models.Cookie{{Name: "session", Value: "cookie", Domain: "example.test", Path: "/app"}},
		Actions: []models.Action{{
			Type: "execute_js", Selector: "#target", Milliseconds: 50,
			Direction: "down", Amount: 3, Code: "() => document.title",
		}},
		RemoveOverlays: true,
		BlockAds:       true,
		CDPURL:         "ws://browser.test/devtools/browser/id",
	}

	fetchRequest := FetchRequestFromScrapeRequest(request, 17500*time.Millisecond)
	if fetchRequest.Timeout != 17500*time.Millisecond {
		t.Fatalf("fetch timeout = %v", fetchRequest.Timeout)
	}
	got := ScrapeRequestFromFetchRequest(fetchRequest)
	want := *request
	want.Timeout = 18 // duration is rounded up; the parent context stays exact.
	if !reflect.DeepEqual(got, &want) {
		t.Fatalf("adapter round trip\n got: %#v\nwant: %#v", got, &want)
	}

	fetchRequest.Headers["X-Test"] = "mutated"
	fetchRequest.Cookies[0].Value = "mutated"
	fetchRequest.Actions[0].Code = "mutated"
	*fetchRequest.WaitForNetworkIdle = false
	if request.Headers["X-Test"] != "header" || request.Cookies[0].Value != "cookie" ||
		request.Actions[0].Code != "() => document.title" || !*request.WaitForNetworkIdle {
		t.Fatal("FetchRequest adapter retained caller-owned mutable state")
	}
}
