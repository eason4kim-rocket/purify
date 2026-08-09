package engine

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestRodEnginePassesCompleteClonedRequest(t *testing.T) {
	wait := true
	request := &FetchRequest{
		URL:                "https://example.test/page",
		Headers:            map[string]string{"X-Test": "value"},
		Cookies:            []http.Cookie{{Name: "session", Value: "cookie", Domain: "example.test", Path: "/"}},
		Timeout:            2 * time.Second,
		ProxyURL:           "socks5://proxy.test:1080",
		WaitForNetworkIdle: &wait,
		RemoveOverlays:     true,
		BlockAds:           true,
		Actions: []Action{{
			Type: "execute_js", Selector: "#target", Milliseconds: 25,
			Direction: "down", Amount: 2, Code: "() => true",
		}},
		CDPURL: "ws://browser.test/devtools/browser/id",
	}
	want := cloneFetchRequest(request)
	var captured *FetchRequest
	rod := NewRodEngine(func(ctx context.Context, req *FetchRequest) (*FetchResult, error) {
		captured = req
		if _, ok := ctx.Deadline(); !ok {
			t.Error("Rod callback context has no request deadline")
		}
		return &FetchResult{HTML: "<html></html>"}, nil
	}, true)

	result, err := rod.Fetch(context.Background(), request)
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	want.Stealth = true
	if !reflect.DeepEqual(captured, want) {
		t.Fatalf("Rod callback request\n got: %#v\nwant: %#v", captured, want)
	}
	if result.EngineName != "rod-stealth" {
		t.Fatalf("EngineName = %q", result.EngineName)
	}

	captured.Headers["X-Test"] = "mutated"
	captured.Cookies[0].Value = "mutated"
	captured.Actions[0].Code = "mutated"
	*captured.WaitForNetworkIdle = false
	if request.Headers["X-Test"] != "value" || request.Cookies[0].Value != "cookie" ||
		request.Actions[0].Code != "() => true" || !*request.WaitForNetworkIdle {
		t.Fatal("Rod engine callback mutated caller-owned request state")
	}
}

func TestExplicitStealthSkipsOrdinaryRodAndUsesRodStealth(t *testing.T) {
	var ordinaryCalls atomic.Int32
	ordinary := NewRodEngine(func(context.Context, *FetchRequest) (*FetchResult, error) {
		ordinaryCalls.Add(1)
		return &FetchResult{HTML: "ordinary"}, nil
	}, false)
	var stealthCalls atomic.Int32
	stealth := NewRodEngine(func(_ context.Context, req *FetchRequest) (*FetchResult, error) {
		stealthCalls.Add(1)
		if !req.Stealth {
			t.Error("rod-stealth callback received Stealth=false")
		}
		return &FetchResult{HTML: "stealth"}, nil
	}, true)

	if ordinary.Supports(&FetchRequest{Stealth: true}) {
		t.Fatal("ordinary Rod Supports(explicit stealth) = true")
	}
	if !stealth.Supports(&FetchRequest{Stealth: true}) {
		t.Fatal("rod-stealth Supports(explicit stealth) = false")
	}
	if !ordinary.Supports(&FetchRequest{}) || !stealth.Supports(&FetchRequest{}) {
		t.Fatal("default request must remain eligible for rod then rod-stealth escalation")
	}

	dispatcher := NewDispatcher(
		[]Engine{ordinary, stealth},
		[]time.Duration{2 * time.Second, 5 * time.Second},
		nil,
	)
	result, err := dispatcher.Dispatch(context.Background(), &FetchRequest{
		URL:     "https://example.test",
		Stealth: true,
	})
	if err != nil {
		t.Fatalf("Dispatch() error = %v", err)
	}
	if result.EngineName != "rod-stealth" {
		t.Fatalf("EngineName = %q, want rod-stealth", result.EngineName)
	}
	if ordinaryCalls.Load() != 0 || stealthCalls.Load() != 1 {
		t.Fatalf("ordinary calls=%d stealth calls=%d", ordinaryCalls.Load(), stealthCalls.Load())
	}
}

func TestRodEngineEnforcesRequestedBodyLimit(t *testing.T) {
	rod := NewRodEngine(func(context.Context, *FetchRequest) (*FetchResult, error) {
		return &FetchResult{HTML: "12345"}, nil
	}, false)
	_, err := rod.Fetch(context.Background(), &FetchRequest{MaximumBodyBytes: 4})
	if !errors.Is(err, ErrResponseBodyTooLarge) {
		t.Fatalf("Fetch() error = %v, want ErrResponseBodyTooLarge", err)
	}
}

func TestRodEngineRejectsHTTPOnlyRedirectPolicy(t *testing.T) {
	rod := NewRodEngine(func(context.Context, *FetchRequest) (*FetchResult, error) {
		t.Fatal("fetch callback must not run for an unsupported redirect policy")
		return nil, nil
	}, false)
	request := &FetchRequest{CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
	if rod.Supports(request) {
		t.Fatal("Supports() accepted an HTTP-only redirect callback")
	}
	if _, err := rod.Fetch(context.Background(), request); !errors.Is(err, ErrUnsupportedRequest) {
		t.Fatalf("Fetch() error = %v, want ErrUnsupportedRequest", err)
	}
}
