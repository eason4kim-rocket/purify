package revisit

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/use-agent/purify/engine"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/publicnet"
	"github.com/use-agent/purify/scraper"
	"github.com/use-agent/purify/snapshot"
	"github.com/use-agent/purify/verify"
)

const (
	testProxyURL = "socks5://127.0.0.1:19080"
	testSnapshot = snapshot.ID("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	testMaximum  = int64(4 << 20)
)

var testFetchedAt = time.Date(2026, 8, 9, 7, 30, 0, 123, time.UTC)

func TestNewValidatesConfiguration(t *testing.T) {
	validEngine := &fakeEngine{name: "http"}
	validFinalizer := &fakeFinalizer{}
	validPolicy := testPolicy(nil)
	valid := Config{
		Engines:          []engine.Engine{validEngine},
		Finalizer:        validFinalizer,
		Policy:           validPolicy,
		SafeProxyURL:     testProxyURL,
		Timeout:          time.Second,
		MaximumBodyBytes: testMaximum,
	}
	var nilEngine *fakeEngine
	var nilFinalizer *fakeFinalizer
	for _, test := range []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "engines", mutate: func(config *Config) { config.Engines = nil }},
		{name: "nil engine", mutate: func(config *Config) { config.Engines = []engine.Engine{nilEngine} }},
		{name: "unknown engine", mutate: func(config *Config) { config.Engines = []engine.Engine{&fakeEngine{name: "other"}} }},
		{name: "duplicate engine", mutate: func(config *Config) { config.Engines = []engine.Engine{validEngine, &fakeEngine{name: "http"}} }},
		{name: "finalizer", mutate: func(config *Config) { config.Finalizer = nil }},
		{name: "typed nil finalizer", mutate: func(config *Config) { config.Finalizer = nilFinalizer }},
		{name: "policy", mutate: func(config *Config) { config.Policy = nil }},
		{name: "timeout", mutate: func(config *Config) { config.Timeout = 0 }},
		{name: "negative timeout", mutate: func(config *Config) { config.Timeout = -1 }},
		{name: "timeout over hard limit", mutate: func(config *Config) { config.Timeout = hardMaximumTimeout + 1 }},
		{name: "body zero", mutate: func(config *Config) { config.MaximumBodyBytes = 0 }},
		{name: "body over hard limit", mutate: func(config *Config) { config.MaximumBodyBytes = hardMaximumResponseBodyBytes + 1 }},
		{name: "proxy empty", mutate: func(config *Config) { config.SafeProxyURL = "" }},
		{name: "proxy scheme", mutate: func(config *Config) { config.SafeProxyURL = "http://127.0.0.1:8080" }},
		{name: "proxy domain", mutate: func(config *Config) { config.SafeProxyURL = "socks5://localhost:8080" }},
		{name: "proxy private", mutate: func(config *Config) { config.SafeProxyURL = "socks5://10.0.0.1:8080" }},
		{name: "proxy port", mutate: func(config *Config) { config.SafeProxyURL = "socks5://127.0.0.1:0" }},
		{name: "proxy auth", mutate: func(config *Config) { config.SafeProxyURL = "socks5://user@127.0.0.1:8080" }},
		{name: "proxy path", mutate: func(config *Config) { config.SafeProxyURL = "socks5://127.0.0.1:8080/path" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			config.Engines = append([]engine.Engine(nil), valid.Engines...)
			test.mutate(&config)
			service, err := New(config)
			if service != nil || !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("New() = (%#v, %v), want nil + ErrInvalidConfig", service, err)
			}
		})
	}

	config := valid
	config.SafeProxyURL = "SOCKS5://[::1]:019080"
	service, err := New(config)
	if err != nil {
		t.Fatalf("New(valid IPv6 proxy) error = %v", err)
	}
	if service.safeProxyURL != "socks5://127.0.0.1:19080" && service.safeProxyURL != "socks5://[::1]:19080" {
		t.Fatalf("canonical safe proxy = %q", service.safeProxyURL)
	}
}

func TestRevisitRejectsInvalidOrPrivateInitialTargetBeforeEngine(t *testing.T) {
	for _, test := range []struct {
		name     string
		target   string
		resolver resolverFunc
	}{
		{name: "empty", target: ""},
		{name: "file", target: "file:///private/etc/passwd"},
		{name: "literal loopback", target: "http://127.0.0.1/"},
		{name: "localhost", target: "http://localhost/"},
		{
			name:   "DNS private",
			target: "https://private.test/",
			resolver: func(context.Context, string, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			},
		},
		{
			name:   "DNS mixed",
			target: "https://mixed.test/",
			resolver: func(context.Context, string, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("10.0.0.1")}, nil
			},
		},
		{
			name:   "misconfigured permissive policy remains fail closed",
			target: "https://private.test/",
			resolver: func(context.Context, string, string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("10.0.0.1")}, nil
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &fakeEngine{name: "http", fetch: unexpectedFetch(t)}
			finalizer := &fakeFinalizer{fn: unexpectedFinalize(t)}
			allowPrivate := test.name == "misconfigured permissive policy remains fail closed"
			policy := publicnet.NewPolicy(publicnet.Options{
				AllowPrivateNetworks: allowPrivate,
				Resolver:             defaultResolver(test.resolver),
				DialContext:          noDial,
			})
			service := mustService(t, []engine.Engine{backend}, finalizer, policy, time.Second, testMaximum)
			result, err := service.Revisit(context.Background(), test.target)
			if result != (verify.RevisitResult{}) || !errors.Is(err, verify.ErrInvalidRequest) {
				t.Fatalf("Revisit() = (%#v, %v), want zero + ErrInvalidRequest", result, err)
			}
			if backend.callCount() != 0 || finalizer.callCount() != 0 {
				t.Fatalf("invalid target reached dependencies: engine=%d finalizer=%d", backend.callCount(), finalizer.callCount())
			}
		})
	}
}

func TestRevisitDNSFailureIsRevisitErrorBeforeEngine(t *testing.T) {
	backend := &fakeEngine{name: "http", fetch: unexpectedFetch(t)}
	finalizer := &fakeFinalizer{fn: unexpectedFinalize(t)}
	dnsErr := errors.New("DNS unavailable")
	policy := testPolicy(func(context.Context, string, string) ([]netip.Addr, error) { return nil, dnsErr })
	service := mustService(t, []engine.Engine{backend}, finalizer, policy, time.Second, testMaximum)
	_, err := service.Revisit(context.Background(), "https://public.test/")
	if !errors.Is(err, verify.ErrRevisit) || !errors.Is(err, dnsErr) || errors.Is(err, verify.ErrInvalidRequest) {
		t.Fatalf("Revisit() error = %v", err)
	}
	if backend.callCount() != 0 || finalizer.callCount() != 0 {
		t.Fatal("DNS failure reached an engine or finalizer")
	}
}

func TestRevisitRejectsNilContextBeforeEngine(t *testing.T) {
	backend := &fakeEngine{name: "http", fetch: unexpectedFetch(t)}
	finalizer := &fakeFinalizer{fn: unexpectedFinalize(t)}
	service := mustService(t, []engine.Engine{backend}, finalizer, testPolicy(nil), time.Second, testMaximum)
	_, err := service.Revisit(nil, "https://example.test/")
	if !errors.Is(err, verify.ErrInvalidRequest) || backend.callCount() != 0 || finalizer.callCount() != 0 {
		t.Fatalf("Revisit(nil) error=%v engine=%d finalizer=%d", err, backend.callCount(), finalizer.callCount())
	}
}

func TestRevisitUsesDeterministicOrderIndependentRequestsAndOneDeadline(t *testing.T) {
	var orderMu sync.Mutex
	var order []string
	var requestPointers []*engine.FetchRequest
	var deadlines []time.Time
	record := func(name string, check func(*testing.T, *engine.FetchRequest)) func(context.Context, *engine.FetchRequest) (*engine.FetchResult, error) {
		return func(ctx context.Context, request *engine.FetchRequest) (*engine.FetchResult, error) {
			orderMu.Lock()
			defer orderMu.Unlock()
			order = append(order, name)
			requestPointers = append(requestPointers, request)
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Errorf("%s context has no deadline", name)
			}
			deadlines = append(deadlines, deadline)
			check(t, request)
			switch name {
			case "http":
				request.URL = "mutated-by-http"
				return validFetch(http.StatusOK, loadingHTML(), "text/html"), nil
			case "rod":
				return validFetch(http.StatusOK, missingBodyHTML(), "text/html"), nil
			default:
				return validFetch(http.StatusOK, shortHTML(), "text/html; charset=utf-8"), nil
			}
		}
	}
	checkCommon := func(t *testing.T, request *engine.FetchRequest) {
		t.Helper()
		if request.URL != "https://example.test/start" || request.Timeout != 750*time.Millisecond ||
			request.ProxyURL != testProxyURL || request.Mode != engine.FetchModeObservation || request.MaximumBodyBytes != testMaximum {
			t.Fatalf("fetch request = %#v", request)
		}
	}
	httpEngine := &fakeEngine{name: "http", fetch: record("http", func(t *testing.T, request *engine.FetchRequest) {
		checkCommon(t, request)
		if request.CheckRedirect == nil || request.WaitForNetworkIdle != nil || request.Stealth {
			t.Fatalf("HTTP-only fields = %#v", request)
		}
	})}
	rodEngine := &fakeEngine{name: "rod", fetch: record("rod", func(t *testing.T, request *engine.FetchRequest) {
		checkCommon(t, request)
		if request.CheckRedirect != nil || request.WaitForNetworkIdle == nil || !*request.WaitForNetworkIdle || request.Stealth {
			t.Fatalf("Rod-only fields = %#v", request)
		}
	})}
	stealthEngine := &fakeEngine{name: "rod-stealth", fetch: record("rod-stealth", func(t *testing.T, request *engine.FetchRequest) {
		checkCommon(t, request)
		if request.CheckRedirect != nil || request.WaitForNetworkIdle == nil || !*request.WaitForNetworkIdle || !request.Stealth {
			t.Fatalf("Rod-stealth fields = %#v", request)
		}
	})}
	finalizer := &fakeFinalizer{}
	service := mustService(
		t,
		[]engine.Engine{stealthEngine, httpEngine, rodEngine},
		finalizer,
		testPolicy(nil),
		750*time.Millisecond,
		testMaximum,
	)
	result, err := service.Revisit(context.Background(), " HTTPS://EXAMPLE.TEST.:443/start#fragment ")
	if err != nil {
		t.Fatalf("Revisit() error = %v", err)
	}
	if fmt.Sprint(order) != fmt.Sprint([]string{"http", "rod", "rod-stealth"}) {
		t.Fatalf("engine order = %v", order)
	}
	if len(requestPointers) != 3 || requestPointers[0] == requestPointers[1] || requestPointers[1] == requestPointers[2] || requestPointers[0] == requestPointers[2] {
		t.Fatalf("requests are not independent: %p %p %p", requestPointers[0], requestPointers[1], requestPointers[2])
	}
	if len(deadlines) != 3 || !deadlines[0].Equal(deadlines[1]) || !deadlines[1].Equal(deadlines[2]) {
		t.Fatalf("engine deadlines = %v, want one parent deadline", deadlines)
	}
	if finalizer.callCount() != 1 {
		t.Fatalf("finalizer calls = %d, want 1", finalizer.callCount())
	}
	request, source := finalizer.lastInput(t)
	if request.URL != "https://example.test/start" || request.ProxyURL != testProxyURL || request.Timeout != 1 || !request.Stealth {
		t.Fatalf("finalizer request = %#v", request)
	}
	if source.EngineUsed != "rod-stealth" || source.FetchMethod != "browser" || source.RawHTML != shortHTML() || source.FinalURL != "https://final.example/path" {
		t.Fatalf("finalizer source = %#v", source)
	}
	if result.StatusCode != http.StatusOK || result.FinalURL != "https://final.example/path" || result.RawHTML != shortHTML() ||
		result.SnapshotID != string(testSnapshot) || !result.FetchedAt.Equal(testFetchedAt) {
		t.Fatalf("Revisit() result = %#v", result)
	}
}

func TestRevisitPersistsEmptyGoneObservationAndStopsEscalation(t *testing.T) {
	for _, statusCode := range []int{http.StatusNotFound, http.StatusGone} {
		t.Run(fmt.Sprint(statusCode), func(t *testing.T) {
			store, err := snapshot.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(store.Close)
			finalizer := &scraper.Scraper{}
			finalizer.SetSnapshotStore(store)
			httpEngine := &fakeEngine{name: "http", fetch: func(context.Context, *engine.FetchRequest) (*engine.FetchResult, error) {
				return validFetch(statusCode, "", "text/plain"), nil
			}}
			rodEngine := &fakeEngine{name: "rod", fetch: unexpectedFetch(t)}
			service := mustService(t, []engine.Engine{rodEngine, httpEngine}, finalizer, testPolicy(nil), time.Second, testMaximum)
			result, err := service.Revisit(context.Background(), "https://example.test/gone")
			if err != nil {
				t.Fatalf("Revisit() error = %v", err)
			}
			if result.StatusCode != statusCode || result.RawHTML != "" || result.SnapshotID == "" || result.FetchedAt.IsZero() {
				t.Fatalf("Revisit() result = %#v", result)
			}
			if rodEngine.callCount() != 0 {
				t.Fatalf("gone response escalated to Rod %d times", rodEngine.callCount())
			}
			content, meta, err := store.Get(snapshot.ID(result.SnapshotID))
			if err != nil {
				t.Fatalf("Get(snapshot) error = %v", err)
			}
			if len(content) != 0 || meta.StatusCode != statusCode || meta.URL != result.FinalURL || !meta.FetchedAt.Equal(result.FetchedAt) {
				t.Fatalf("stored observation = (%q, %#v)", content, meta)
			}
		})
	}
}

func TestRevisitSelectsShortHTMLWithoutCleanedContentThreshold(t *testing.T) {
	for _, contentType := range []string{"text/html", "application/xhtml+xml; charset=utf-8"} {
		t.Run(contentType, func(t *testing.T) {
			finalizer := &fakeFinalizer{}
			httpEngine := &fakeEngine{name: "http", fetch: func(context.Context, *engine.FetchRequest) (*engine.FetchResult, error) {
				return validFetch(http.StatusOK, shortHTML(), contentType), nil
			}}
			rodEngine := &fakeEngine{name: "rod", fetch: unexpectedFetch(t)}
			service := mustService(t, []engine.Engine{rodEngine, httpEngine}, finalizer, testPolicy(nil), time.Second, testMaximum)
			result, err := service.Revisit(context.Background(), "https://example.test/")
			if err != nil || result.RawHTML != shortHTML() {
				t.Fatalf("Revisit() = (%#v, %v)", result, err)
			}
			if rodEngine.callCount() != 0 || finalizer.callCount() != 1 {
				t.Fatalf("calls: rod=%d finalizer=%d", rodEngine.callCount(), finalizer.callCount())
			}
		})
	}
}

func TestRevisitDefaultsEmptyFinalURLToCanonicalRequestURL(t *testing.T) {
	finalizer := &fakeFinalizer{}
	backend := &fakeEngine{name: "http", fetch: func(context.Context, *engine.FetchRequest) (*engine.FetchResult, error) {
		result := validFetch(http.StatusOK, shortHTML(), "text/html")
		result.FinalURL = ""
		result.EngineName = "spoofed-engine"
		return result, nil
	}}
	service := mustService(t, []engine.Engine{backend}, finalizer, testPolicy(nil), time.Second, testMaximum)
	result, err := service.Revisit(context.Background(), "HTTPS://EXAMPLE.TEST.:443/current#fragment")
	if err != nil || result.FinalURL != "https://example.test/current" {
		t.Fatalf("Revisit() = (%#v, %v)", result, err)
	}
	_, source := finalizer.lastInput(t)
	if source.FinalURL != "https://example.test/current" || source.EngineUsed != "http" || source.FetchMethod != "http" {
		t.Fatalf("finalizer source = %#v", source)
	}
}

func TestRevisitRejectsIncompleteOrSubstitutedPagesWithoutFinalize(t *testing.T) {
	for _, test := range []struct {
		name string
		html string
	}{
		{name: "missing body", html: missingBodyHTML()},
		{name: "empty body", html: `<html><body><script>late()</script></body></html>`},
		{name: "loading", html: loadingHTML()},
		{name: "browser error", html: `<html><body><main id="main-frame-error">This site can't be reached ERR_NAME_NOT_RESOLVED</main></body></html>`},
		{name: "challenge", html: `<html><body><div id="cf-chl-widget">Checking your browser before accessing the site</div></body></html>`},
	} {
		t.Run(test.name, func(t *testing.T) {
			finalizer := &fakeFinalizer{fn: unexpectedFinalize(t)}
			backend := &fakeEngine{name: "http", fetch: func(context.Context, *engine.FetchRequest) (*engine.FetchResult, error) {
				return validFetch(http.StatusOK, test.html, "text/html"), nil
			}}
			service := mustService(t, []engine.Engine{backend}, finalizer, testPolicy(nil), time.Second, testMaximum)
			result, err := service.Revisit(context.Background(), "https://example.test/")
			if result != (verify.RevisitResult{}) || !errors.Is(err, verify.ErrRevisit) {
				t.Fatalf("Revisit() = (%#v, %v), want fail-closed", result, err)
			}
			if finalizer.callCount() != 0 {
				t.Fatalf("unusable page finalized %d times", finalizer.callCount())
			}
		})
	}
}

func TestRevisitRejectsNonHTMLSuccessAndEscalates(t *testing.T) {
	finalizer := &fakeFinalizer{}
	httpEngine := &fakeEngine{name: "http", fetch: func(context.Context, *engine.FetchRequest) (*engine.FetchResult, error) {
		return validFetch(http.StatusOK, `{"price":10}`, "application/json"), nil
	}}
	rodEngine := &fakeEngine{name: "rod", fetch: func(context.Context, *engine.FetchRequest) (*engine.FetchResult, error) {
		return validFetch(http.StatusOK, shortHTML(), "text/html"), nil
	}}
	service := mustService(t, []engine.Engine{rodEngine, httpEngine}, finalizer, testPolicy(nil), time.Second, testMaximum)
	result, err := service.Revisit(context.Background(), "https://example.test/")
	if err != nil || result.RawHTML != shortHTML() || finalizer.callCount() != 1 {
		t.Fatalf("Revisit() = (%#v, %v), finalizer=%d", result, err, finalizer.callCount())
	}
}

func TestRevisitProtectedStatusesEscalateThenReturnHTTPStatusError(t *testing.T) {
	for _, statusCode := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests} {
		t.Run(fmt.Sprint(statusCode), func(t *testing.T) {
			finalizer := &fakeFinalizer{fn: unexpectedFinalize(t)}
			httpEngine := &fakeEngine{name: "http", fetch: func(context.Context, *engine.FetchRequest) (*engine.FetchResult, error) {
				return validFetch(statusCode, `<html><body>denied</body></html>`, "text/html"), nil
			}}
			rodEngine := &fakeEngine{name: "rod", fetch: func(context.Context, *engine.FetchRequest) (*engine.FetchResult, error) {
				return nil, errors.New("browser unavailable")
			}}
			service := mustService(t, []engine.Engine{rodEngine, httpEngine}, finalizer, testPolicy(nil), time.Second, testMaximum)
			_, err := service.Revisit(context.Background(), "https://example.test/")
			var statusErr *verify.HTTPStatusError
			if !errors.As(err, &statusErr) || statusErr.StatusCode != statusCode || !errors.Is(err, verify.ErrRevisitStatus) {
				t.Fatalf("Revisit() error = %v", err)
			}
			if httpEngine.callCount() != 1 || rodEngine.callCount() != 1 || finalizer.callCount() != 0 {
				t.Fatalf("calls: http=%d rod=%d finalizer=%d", httpEngine.callCount(), rodEngine.callCount(), finalizer.callCount())
			}
		})
	}
}

func TestRevisitProtectedStatusCanEscalateToUsableHTML(t *testing.T) {
	finalizer := &fakeFinalizer{}
	httpEngine := &fakeEngine{name: "http", fetch: func(context.Context, *engine.FetchRequest) (*engine.FetchResult, error) {
		return validFetch(http.StatusForbidden, "denied", "text/html"), nil
	}}
	rodEngine := &fakeEngine{name: "rod", fetch: func(context.Context, *engine.FetchRequest) (*engine.FetchResult, error) {
		return validFetch(http.StatusOK, shortHTML(), "text/html"), nil
	}}
	service := mustService(t, []engine.Engine{rodEngine, httpEngine}, finalizer, testPolicy(nil), time.Second, testMaximum)
	result, err := service.Revisit(context.Background(), "https://example.test/")
	if err != nil || result.StatusCode != http.StatusOK || finalizer.callCount() != 1 {
		t.Fatalf("Revisit() = (%#v, %v), finalizer=%d", result, err, finalizer.callCount())
	}
}

func TestRevisitOtherStatusesAreRequestErrors(t *testing.T) {
	for _, statusCode := range []int{0, http.StatusMovedPermanently, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(statusCode), func(t *testing.T) {
			finalizer := &fakeFinalizer{fn: unexpectedFinalize(t)}
			backend := &fakeEngine{name: "http", fetch: func(context.Context, *engine.FetchRequest) (*engine.FetchResult, error) {
				return validFetch(statusCode, shortHTML(), "text/html"), nil
			}}
			service := mustService(t, []engine.Engine{backend}, finalizer, testPolicy(nil), time.Second, testMaximum)
			_, err := service.Revisit(context.Background(), "https://example.test/")
			var statusErr *verify.HTTPStatusError
			if !errors.Is(err, verify.ErrRevisit) || errors.As(err, &statusErr) {
				t.Fatalf("Revisit() error = %v", err)
			}
			if finalizer.callCount() != 0 {
				t.Fatal("unusable status was finalized")
			}
		})
	}
}

func TestRevisitAllEngineFailuresDoNotFinalize(t *testing.T) {
	firstErr := errors.New("HTTP transport failed")
	httpEngine := &fakeEngine{name: "http", fetch: func(context.Context, *engine.FetchRequest) (*engine.FetchResult, error) {
		return nil, firstErr
	}}
	rodEngine := &fakeEngine{name: "rod", fetch: func(context.Context, *engine.FetchRequest) (*engine.FetchResult, error) {
		return nil, nil
	}}
	stealthEngine := &fakeEngine{
		name:     "rod-stealth",
		supports: func(*engine.FetchRequest) bool { return false },
		fetch:    unexpectedFetch(t),
	}
	finalizer := &fakeFinalizer{fn: unexpectedFinalize(t)}
	service := mustService(
		t,
		[]engine.Engine{stealthEngine, rodEngine, httpEngine},
		finalizer,
		testPolicy(nil),
		time.Second,
		testMaximum,
	)
	result, err := service.Revisit(context.Background(), "https://example.test/")
	if result != (verify.RevisitResult{}) || !errors.Is(err, verify.ErrRevisit) {
		t.Fatalf("Revisit() = (%#v, %v)", result, err)
	}
	if httpEngine.callCount() != 1 || rodEngine.callCount() != 1 || stealthEngine.callCount() != 0 || finalizer.callCount() != 0 {
		t.Fatalf(
			"calls: http=%d rod=%d stealth=%d finalizer=%d",
			httpEngine.callCount(),
			rodEngine.callCount(),
			stealthEngine.callCount(),
			finalizer.callCount(),
		)
	}
}

func TestRevisitEnforcesBodyLimitEvenWhenEngineDoesNot(t *testing.T) {
	finalizer := &fakeFinalizer{fn: unexpectedFinalize(t)}
	backend := &fakeEngine{name: "http", fetch: func(_ context.Context, request *engine.FetchRequest) (*engine.FetchResult, error) {
		if request.MaximumBodyBytes != 32 {
			t.Fatalf("MaximumBodyBytes = %d", request.MaximumBodyBytes)
		}
		return validFetch(http.StatusOK, strings.Repeat("x", 33), "text/html"), nil
	}}
	service := mustService(t, []engine.Engine{backend}, finalizer, testPolicy(nil), time.Second, 32)
	_, err := service.Revisit(context.Background(), "https://example.test/")
	if !errors.Is(err, verify.ErrRevisit) || !errors.Is(err, engine.ErrResponseBodyTooLarge) {
		t.Fatalf("Revisit() error = %v", err)
	}
	if finalizer.callCount() != 0 {
		t.Fatal("oversized body was finalized")
	}
}

func TestRevisitBodyLimitFailureIsTerminalAcrossBrowserTiers(t *testing.T) {
	for _, test := range []struct {
		name      string
		httpFetch func(context.Context, *engine.FetchRequest) (*engine.FetchResult, error)
	}{
		{
			name: "HTTP engine returns sentinel",
			httpFetch: func(context.Context, *engine.FetchRequest) (*engine.FetchResult, error) {
				return nil, fmt.Errorf("http: %w", engine.ErrResponseBodyTooLarge)
			},
		},
		{
			name: "defense in depth catches oversized result",
			httpFetch: func(context.Context, *engine.FetchRequest) (*engine.FetchResult, error) {
				return validFetch(http.StatusOK, strings.Repeat("x", 33), "text/html"), nil
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			httpEngine := &fakeEngine{name: "http", fetch: test.httpFetch}
			rodEngine := &fakeEngine{name: "rod", fetch: unexpectedFetch(t)}
			stealthEngine := &fakeEngine{name: "rod-stealth", fetch: unexpectedFetch(t)}
			finalizer := &fakeFinalizer{fn: unexpectedFinalize(t)}
			service := mustService(
				t,
				[]engine.Engine{stealthEngine, rodEngine, httpEngine},
				finalizer,
				testPolicy(nil),
				time.Second,
				32,
			)

			_, err := service.Revisit(context.Background(), "https://example.test/")
			if !errors.Is(err, verify.ErrRevisit) || !errors.Is(err, engine.ErrResponseBodyTooLarge) {
				t.Fatalf("Revisit() error = %v", err)
			}
			if httpEngine.callCount() != 1 || rodEngine.callCount() != 0 || stealthEngine.callCount() != 0 || finalizer.callCount() != 0 {
				t.Fatalf(
					"calls: http=%d rod=%d stealth=%d finalizer=%d",
					httpEngine.callCount(),
					rodEngine.callCount(),
					stealthEngine.callCount(),
					finalizer.callCount(),
				)
			}
		})
	}
}

func TestRevisitRodBodyLimitFailureDoesNotRunStealth(t *testing.T) {
	httpEngine := &fakeEngine{name: "http", fetch: func(context.Context, *engine.FetchRequest) (*engine.FetchResult, error) {
		return nil, errors.New("HTTP transport failed")
	}}
	rodEngine := &fakeEngine{name: "rod", fetch: func(context.Context, *engine.FetchRequest) (*engine.FetchResult, error) {
		return nil, fmt.Errorf("rod: %w", engine.ErrResponseBodyTooLarge)
	}}
	stealthEngine := &fakeEngine{name: "rod-stealth", fetch: unexpectedFetch(t)}
	finalizer := &fakeFinalizer{fn: unexpectedFinalize(t)}
	service := mustService(
		t,
		[]engine.Engine{stealthEngine, rodEngine, httpEngine},
		finalizer,
		testPolicy(nil),
		time.Second,
		32,
	)

	_, err := service.Revisit(context.Background(), "https://example.test/")
	if !errors.Is(err, verify.ErrRevisit) || !errors.Is(err, engine.ErrResponseBodyTooLarge) {
		t.Fatalf("Revisit() error = %v", err)
	}
	if httpEngine.callCount() != 1 || rodEngine.callCount() != 1 || stealthEngine.callCount() != 0 || finalizer.callCount() != 0 {
		t.Fatalf(
			"calls: http=%d rod=%d stealth=%d finalizer=%d",
			httpEngine.callCount(),
			rodEngine.callCount(),
			stealthEngine.callCount(),
			finalizer.callCount(),
		)
	}
}

func TestRevisitCancellationUsesOneTotalBudgetAndDoesNotFinalize(t *testing.T) {
	t.Run("already canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		backend := &fakeEngine{name: "http", fetch: unexpectedFetch(t)}
		finalizer := &fakeFinalizer{fn: unexpectedFinalize(t)}
		service := mustService(t, []engine.Engine{backend}, finalizer, testPolicy(nil), time.Second, testMaximum)
		_, err := service.Revisit(ctx, "https://example.test/")
		if !errors.Is(err, verify.ErrRevisit) || !errors.Is(err, context.Canceled) || backend.callCount() != 0 || finalizer.callCount() != 0 {
			t.Fatalf("Revisit() error=%v engine=%d finalizer=%d", err, backend.callCount(), finalizer.callCount())
		}
	})

	t.Run("deadline stops escalation", func(t *testing.T) {
		first := &fakeEngine{name: "http", fetch: func(ctx context.Context, _ *engine.FetchRequest) (*engine.FetchResult, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		second := &fakeEngine{name: "rod", fetch: unexpectedFetch(t)}
		finalizer := &fakeFinalizer{fn: unexpectedFinalize(t)}
		service := mustService(t, []engine.Engine{second, first}, finalizer, testPolicy(nil), 30*time.Millisecond, testMaximum)
		started := time.Now()
		_, err := service.Revisit(context.Background(), "https://example.test/")
		if !errors.Is(err, verify.ErrRevisit) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Revisit() error = %v", err)
		}
		if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
			t.Fatalf("Revisit() elapsed = %v", elapsed)
		}
		if second.callCount() != 0 || finalizer.callCount() != 0 {
			t.Fatalf("deadline reached later dependencies: rod=%d finalizer=%d", second.callCount(), finalizer.callCount())
		}
	})

	t.Run("canceled by engine before selection", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		backend := &fakeEngine{name: "http", fetch: func(context.Context, *engine.FetchRequest) (*engine.FetchResult, error) {
			cancel()
			return validFetch(http.StatusOK, shortHTML(), "text/html"), nil
		}}
		finalizer := &fakeFinalizer{fn: unexpectedFinalize(t)}
		service := mustService(t, []engine.Engine{backend}, finalizer, testPolicy(nil), time.Second, testMaximum)
		_, err := service.Revisit(ctx, "https://example.test/")
		if !errors.Is(err, verify.ErrRevisit) || !errors.Is(err, context.Canceled) || finalizer.callCount() != 0 {
			t.Fatalf("Revisit() error=%v finalizer=%d", err, finalizer.callCount())
		}
	})
}

func TestRevisitRejectsUnsafeFinalURLWithoutFinalize(t *testing.T) {
	for _, finalURL := range []string{"file:///private/etc/passwd", "http://127.0.0.1/", "https://private.test/"} {
		t.Run(finalURL, func(t *testing.T) {
			finalizer := &fakeFinalizer{fn: unexpectedFinalize(t)}
			backend := &fakeEngine{name: "http", fetch: func(context.Context, *engine.FetchRequest) (*engine.FetchResult, error) {
				result := validFetch(http.StatusOK, shortHTML(), "text/html")
				result.FinalURL = finalURL
				return result, nil
			}}
			resolver := resolverFunc(func(_ context.Context, _, hostname string) ([]netip.Addr, error) {
				if hostname == "private.test" {
					return []netip.Addr{netip.MustParseAddr("10.0.0.1")}, nil
				}
				return []netip.Addr{netip.MustParseAddr("1.1.1.1")}, nil
			})
			service := mustService(t, []engine.Engine{backend}, finalizer, testPolicy(resolver), time.Second, testMaximum)
			_, err := service.Revisit(context.Background(), "https://example.test/")
			if !errors.Is(err, verify.ErrRevisit) || finalizer.callCount() != 0 {
				t.Fatalf("Revisit() error=%v finalizer=%d", err, finalizer.callCount())
			}
		})
	}
}

func TestRevisitRequiresFinalizerProvenanceAndExactSelectedFields(t *testing.T) {
	for _, test := range []struct {
		name string
		fn   finalizeFunc
	}{
		{name: "error", fn: func(*models.ScrapeRequest, *scraper.ScrapeResult) (*scraper.ScrapeResult, error) {
			return nil, errors.New("disk unavailable")
		}},
		{name: "nil result", fn: func(*models.ScrapeRequest, *scraper.ScrapeResult) (*scraper.ScrapeResult, error) { return nil, nil }},
		{name: "snapshot", fn: finalizedMutation(func(result *scraper.ScrapeResult) { result.SnapshotID = "" })},
		{name: "fetched at", fn: finalizedMutation(func(result *scraper.ScrapeResult) { result.FetchedAt = time.Time{} })},
		{name: "status", fn: finalizedMutation(func(result *scraper.ScrapeResult) { result.StatusCode++ })},
		{name: "raw HTML", fn: finalizedMutation(func(result *scraper.ScrapeResult) { result.RawHTML += "tampered" })},
		{name: "final URL", fn: finalizedMutation(func(result *scraper.ScrapeResult) { result.FinalURL = "https://other.example/" })},
	} {
		t.Run(test.name, func(t *testing.T) {
			finalizer := &fakeFinalizer{fn: test.fn}
			backend := &fakeEngine{name: "http", fetch: func(context.Context, *engine.FetchRequest) (*engine.FetchResult, error) {
				return validFetch(http.StatusOK, shortHTML(), "text/html"), nil
			}}
			service := mustService(t, []engine.Engine{backend}, finalizer, testPolicy(nil), time.Second, testMaximum)
			_, err := service.Revisit(context.Background(), "https://example.test/")
			if !errors.Is(err, verify.ErrRevisit) || finalizer.callCount() != 1 {
				t.Fatalf("Revisit() error=%v finalizer=%d", err, finalizer.callCount())
			}
		})
	}
}

func TestHTTPRedirectPolicyIsRequestLocalBoundedAndPublicOnly(t *testing.T) {
	for _, test := range []struct {
		name               string
		redirectURL        string
		viaURLs            []string
		privateHost        string
		wantSuccess        bool
		wantLookupsAtLeast int32
	}{
		{name: "public hop", redirectURL: "HTTPS://REDIRECT.TEST.:443/next#fragment", viaURLs: []string{"https://example.test/start"}, wantSuccess: true, wantLookupsAtLeast: 2},
		{name: "private hop", redirectURL: "https://private.test/secret", viaURLs: []string{"https://example.test/start"}, privateHost: "private.test"},
		{name: "cycle", redirectURL: "https://example.test/start#again", viaURLs: []string{"https://example.test/start"}},
		{name: "five hops allowed", redirectURL: "https://redirect.test/final", viaURLs: []string{"https://one.test/", "https://two.test/", "https://three.test/", "https://four.test/", "https://five.test/"}, wantSuccess: true, wantLookupsAtLeast: 2},
		{name: "sixth hop rejected", redirectURL: "https://redirect.test/final", viaURLs: []string{"https://one.test/", "https://two.test/", "https://three.test/", "https://four.test/", "https://five.test/", "https://six.test/"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var lookups atomic.Int32
			resolver := resolverFunc(func(_ context.Context, _, hostname string) ([]netip.Addr, error) {
				lookups.Add(1)
				if hostname == test.privateHost {
					return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
				}
				return []netip.Addr{netip.MustParseAddr("1.1.1.1")}, nil
			})
			finalizer := &fakeFinalizer{}
			backend := &fakeEngine{name: "http", fetch: func(ctx context.Context, request *engine.FetchRequest) (*engine.FetchResult, error) {
				if request.CheckRedirect == nil {
					t.Fatal("HTTP request has no redirect callback")
				}
				current, err := http.NewRequestWithContext(ctx, http.MethodGet, test.redirectURL, nil)
				if err != nil {
					return nil, err
				}
				via := make([]*http.Request, 0, len(test.viaURLs))
				for _, previousURL := range test.viaURLs {
					previous, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, previousURL, nil)
					if requestErr != nil {
						return nil, requestErr
					}
					via = append(via, previous)
				}
				if err := request.CheckRedirect(current, via); err != nil {
					return nil, err
				}
				result := validFetch(http.StatusOK, shortHTML(), "text/html")
				result.FinalURL = current.URL.String()
				return result, nil
			}}
			service := mustService(t, []engine.Engine{backend}, finalizer, testPolicy(resolver), time.Second, testMaximum)
			result, err := service.Revisit(context.Background(), "https://example.test/start")
			if test.wantSuccess {
				if err != nil || result.SnapshotID == "" || finalizer.callCount() != 1 {
					t.Fatalf("Revisit() = (%#v, %v), finalizer=%d", result, err, finalizer.callCount())
				}
			} else if !errors.Is(err, verify.ErrRevisit) || finalizer.callCount() != 0 {
				t.Fatalf("Revisit() = (%#v, %v), finalizer=%d", result, err, finalizer.callCount())
			}
			if got := lookups.Load(); got < test.wantLookupsAtLeast {
				t.Fatalf("resolver lookups = %d, want at least %d", got, test.wantLookupsAtLeast)
			}
		})
	}
}

func TestServiceIsConcurrentSafe(t *testing.T) {
	backend := &fakeEngine{name: "http", fetch: func(context.Context, *engine.FetchRequest) (*engine.FetchResult, error) {
		return validFetch(http.StatusOK, shortHTML(), "text/html"), nil
	}}
	finalizer := &fakeFinalizer{}
	service := mustService(t, []engine.Engine{backend}, finalizer, testPolicy(nil), time.Second, testMaximum)
	const workers = 32
	errs := make(chan error, workers)
	var waitGroup sync.WaitGroup
	for range workers {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			result, err := service.Revisit(context.Background(), "https://example.test/")
			if err == nil && result.SnapshotID == "" {
				err = errors.New("missing snapshot ID")
			}
			errs <- err
		}()
	}
	waitGroup.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Revisit() error = %v", err)
		}
	}
	if backend.callCount() != workers || finalizer.callCount() != workers {
		t.Fatalf("calls: engine=%d finalizer=%d, want %d", backend.callCount(), finalizer.callCount(), workers)
	}
}

type fakeEngine struct {
	name     string
	supports func(*engine.FetchRequest) bool
	fetch    func(context.Context, *engine.FetchRequest) (*engine.FetchResult, error)
	calls    atomic.Int32
}

func (backend *fakeEngine) Name() string { return backend.name }

func (backend *fakeEngine) Supports(request *engine.FetchRequest) bool {
	if backend.supports != nil {
		return backend.supports(request)
	}
	return true
}

func (backend *fakeEngine) Fetch(ctx context.Context, request *engine.FetchRequest) (*engine.FetchResult, error) {
	backend.calls.Add(1)
	if backend.fetch == nil {
		return nil, errors.New("fake engine has no result")
	}
	return backend.fetch(ctx, request)
}

func (backend *fakeEngine) callCount() int32 { return backend.calls.Load() }

type finalizeFunc func(*models.ScrapeRequest, *scraper.ScrapeResult) (*scraper.ScrapeResult, error)

type fakeFinalizer struct {
	fn       finalizeFunc
	calls    atomic.Int32
	mu       sync.Mutex
	requests []models.ScrapeRequest
	sources  []scraper.ScrapeResult
}

func (finalizer *fakeFinalizer) FinalizeSelected(request *models.ScrapeRequest, source *scraper.ScrapeResult) (*scraper.ScrapeResult, error) {
	finalizer.calls.Add(1)
	finalizer.mu.Lock()
	if request != nil {
		finalizer.requests = append(finalizer.requests, *request)
	}
	if source != nil {
		finalizer.sources = append(finalizer.sources, *source)
	}
	finalizer.mu.Unlock()
	if finalizer.fn != nil {
		return finalizer.fn(request, source)
	}
	return finalizedMutation(nil)(request, source)
}

func (finalizer *fakeFinalizer) callCount() int32 { return finalizer.calls.Load() }

func (finalizer *fakeFinalizer) lastInput(t *testing.T) (models.ScrapeRequest, scraper.ScrapeResult) {
	t.Helper()
	finalizer.mu.Lock()
	defer finalizer.mu.Unlock()
	if len(finalizer.requests) == 0 || len(finalizer.sources) == 0 {
		t.Fatal("finalizer has no captured input")
	}
	return finalizer.requests[len(finalizer.requests)-1], finalizer.sources[len(finalizer.sources)-1]
}

func finalizedMutation(mutate func(*scraper.ScrapeResult)) finalizeFunc {
	return func(_ *models.ScrapeRequest, source *scraper.ScrapeResult) (*scraper.ScrapeResult, error) {
		if source == nil {
			return nil, errors.New("source is nil")
		}
		result := *source
		result.SnapshotID = testSnapshot
		result.FetchedAt = testFetchedAt
		if mutate != nil {
			mutate(&result)
		}
		return &result, nil
	}
}

type resolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (resolver resolverFunc) LookupNetIP(ctx context.Context, network, hostname string) ([]netip.Addr, error) {
	return resolver(ctx, network, hostname)
}

func defaultResolver(override resolverFunc) resolverFunc {
	if override != nil {
		return override
	}
	return func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("1.1.1.1")}, nil
	}
}

func testPolicy(resolver resolverFunc) *publicnet.Policy {
	return publicnet.NewPolicy(publicnet.Options{
		Resolver:    defaultResolver(resolver),
		DialContext: noDial,
	})
}

func noDial(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("raw dial must not run in revisit unit tests")
}

func mustService(
	t *testing.T,
	engines []engine.Engine,
	finalizer Finalizer,
	policy *publicnet.Policy,
	timeout time.Duration,
	maximumBodyBytes int64,
) *Service {
	t.Helper()
	service, err := New(Config{
		Engines:          engines,
		Finalizer:        finalizer,
		Policy:           policy,
		SafeProxyURL:     testProxyURL,
		Timeout:          timeout,
		MaximumBodyBytes: maximumBodyBytes,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return service
}

func validFetch(statusCode int, html, contentType string) *engine.FetchResult {
	return &engine.FetchResult{
		HTML:        html,
		Title:       "Current page",
		StatusCode:  statusCode,
		FinalURL:    "https://final.example/path",
		ContentType: contentType,
	}
}

func shortHTML() string {
	return `<html><body><main>OK</main></body></html>`
}

func loadingHTML() string {
	return `<html><body><main aria-busy="true">Loading content...</main></body></html>`
}

func missingBodyHTML() string {
	return `<html><head><title>Head only</title></head></html>`
}

func unexpectedFetch(t *testing.T) func(context.Context, *engine.FetchRequest) (*engine.FetchResult, error) {
	t.Helper()
	return func(context.Context, *engine.FetchRequest) (*engine.FetchResult, error) {
		t.Error("unexpected engine fetch")
		return nil, errors.New("unexpected engine fetch")
	}
}

func unexpectedFinalize(t *testing.T) finalizeFunc {
	t.Helper()
	return func(*models.ScrapeRequest, *scraper.ScrapeResult) (*scraper.ScrapeResult, error) {
		t.Error("unexpected finalizer call")
		return nil, errors.New("unexpected finalizer call")
	}
}
