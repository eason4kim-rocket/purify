package discovery

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDiscoverCombinesCanonicalSourcesInStableOrder(t *testing.T) {
	client := newRouteDoer(map[string]routeResponse{
		"https://example.test/sitemap.xml": xmlRoute(`<sitemapindex>
			<sitemap><loc>/nested.xml</loc></sitemap>
			<sitemap><loc>https://EXAMPLE.test:443/nested.xml#duplicate</loc></sitemap>
		</sitemapindex>`),
		"https://example.test/nested.xml": xmlRoute(`<urlset>
			<url><loc>/b#fragment</loc></url>
			<url><loc>https://EXAMPLE.test:443/a?x=1</loc></url>
			<url><loc>https://outside.invalid/ignored</loc></url>
		</urlset>`),
		"https://example.test/robots.txt":      textRoute("Sitemap: maps/robots.xml\r\nSITEMAP: /nested.xml\n"),
		"https://example.test/maps/robots.xml": xmlRoute(`<urlset><url><loc>/c</loc></url></urlset>`),
		"https://example.test/start": htmlRoute(`<html><body>
			<a href="/d#section">D</a>
			<a href="https://docs.example.test/e">E</a>
			<a href="https://outside.invalid/no">outside</a>
		</body></html>`),
	})
	service := mustService(t, client, Config{AllowPrivateNetworks: true})
	result, err := service.Discover(context.Background(), "https://EXAMPLE.test:443/start#top")
	if err != nil {
		t.Fatalf("Discover() error = %v, warnings = %#v", err, result.Warnings)
	}
	want := []string{
		"https://docs.example.test/e",
		"https://example.test/a?x=1",
		"https://example.test/b",
		"https://example.test/c",
		"https://example.test/d",
		"https://example.test/start",
	}
	if !reflect.DeepEqual(result.URLs, want) {
		t.Fatalf("URLs = %#v, want %#v", result.URLs, want)
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("warnings = %#v", result.Warnings)
	}
	if result.Sources != (SourceStats{SitemapFiles: 3, SitemapURLs: 3, RobotsSitemaps: 2, HomeLinks: 2}) {
		t.Fatalf("source stats = %#v", result.Sources)
	}
	if got := client.callURLs(); !sort.StringsAreSorted(append([]string(nil), result.URLs...)) || len(got) != 5 {
		t.Fatalf("calls = %#v", got)
	}
}

func TestDiscoverReturnsPartialResultsAndWarnings(t *testing.T) {
	client := newRouteDoer(map[string]routeResponse{
		"https://example.test/sitemap.xml": {status: http.StatusBadGateway, contentType: "text/plain"},
		"https://example.test/robots.txt":  textRoute("User-agent: *\n"),
		"https://example.test/":            htmlRoute(`<a href="/found#fragment">found</a>`),
	})
	service := mustService(t, client, Config{AllowPrivateNetworks: true})
	result, err := service.Discover(context.Background(), "https://example.test")
	if err != nil {
		t.Fatalf("Discover() error = %v", err)
	}
	if want := []string{"https://example.test/", "https://example.test/found"}; !reflect.DeepEqual(result.URLs, want) {
		t.Fatalf("URLs = %#v, want %#v", result.URLs, want)
	}
	assertWarning(t, result.Warnings, "sitemap", "http_status")
	if result.Sources.HomeLinks != 1 || result.Sources.RobotsSitemaps != 0 {
		t.Fatalf("stats = %#v", result.Sources)
	}
}

func TestDiscoverReportsAllSourcesFailed(t *testing.T) {
	client := newRouteDoer(map[string]routeResponse{
		"https://example.test/sitemap.xml": {status: http.StatusNotFound, contentType: "text/plain"},
		"https://example.test/robots.txt":  {status: http.StatusInternalServerError, contentType: "text/plain"},
		"https://example.test/":            {status: http.StatusServiceUnavailable, contentType: "text/html"},
	})
	service := mustService(t, client, Config{AllowPrivateNetworks: true})
	result, err := service.Discover(context.Background(), "https://example.test")
	if !errors.Is(err, ErrAllSourcesFailed) {
		t.Fatalf("Discover() error = %v, want ErrAllSourcesFailed", err)
	}
	if want := []string{"https://example.test/"}; !reflect.DeepEqual(result.URLs, want) {
		t.Fatalf("URLs = %#v, want root-only %#v", result.URLs, want)
	}
	if len(result.Warnings) != 3 {
		t.Fatalf("warnings = %#v, want 3", result.Warnings)
	}
	if got := []string{result.Warnings[0].Source, result.Warnings[1].Source, result.Warnings[2].Source}; !reflect.DeepEqual(got, []string{"homepage", "robots", "sitemap"}) {
		t.Fatalf("warning order = %#v", got)
	}
}

func TestDiscoverEnforcesDepthFileAndCycleBounds(t *testing.T) {
	t.Run("depth", func(t *testing.T) {
		client := newRouteDoer(map[string]routeResponse{
			"https://example.test/sitemap.xml": xmlRoute(`<sitemapindex><sitemap><loc>/one.xml</loc></sitemap></sitemapindex>`),
			"https://example.test/one.xml":     xmlRoute(`<sitemapindex><sitemap><loc>/two.xml</loc></sitemap></sitemapindex>`),
			"https://example.test/robots.txt":  textRoute(""),
			"https://example.test/":            htmlRoute("<html></html>"),
		})
		service := mustService(t, client, Config{AllowPrivateNetworks: true, MaxSitemapDepth: 1})
		result, err := service.Discover(context.Background(), "https://example.test")
		if err != nil {
			t.Fatal(err)
		}
		assertWarning(t, result.Warnings, "sitemap", "depth_limit")
		if client.called("https://example.test/two.xml") || result.Sources.SitemapFiles != 2 || !result.Sources.Truncated {
			t.Fatalf("depth calls=%#v stats=%#v", client.callURLs(), result.Sources)
		}
	})

	t.Run("files", func(t *testing.T) {
		client := newRouteDoer(map[string]routeResponse{
			"https://example.test/sitemap.xml": xmlRoute(`<sitemapindex><sitemap><loc>/one.xml</loc></sitemap></sitemapindex>`),
			"https://example.test/robots.txt":  textRoute(""),
			"https://example.test/":            htmlRoute("<html></html>"),
		})
		service := mustService(t, client, Config{AllowPrivateNetworks: true, MaxSitemapFiles: 1})
		result, err := service.Discover(context.Background(), "https://example.test")
		if err != nil {
			t.Fatal(err)
		}
		assertWarning(t, result.Warnings, "sitemap", "file_limit")
		if client.called("https://example.test/one.xml") || result.Sources.SitemapFiles != 1 || !result.Sources.Truncated {
			t.Fatalf("file calls=%#v stats=%#v", client.callURLs(), result.Sources)
		}
	})

	t.Run("cycle", func(t *testing.T) {
		client := newRouteDoer(map[string]routeResponse{
			"https://example.test/sitemap.xml": xmlRoute(`<sitemapindex><sitemap><loc>/sitemap.xml#again</loc></sitemap></sitemapindex>`),
			"https://example.test/robots.txt":  textRoute("Sitemap: /sitemap.xml"),
			"https://example.test/":            htmlRoute("<html></html>"),
		})
		service := mustService(t, client, Config{AllowPrivateNetworks: true})
		result, err := service.Discover(context.Background(), "https://example.test")
		if err != nil {
			t.Fatal(err)
		}
		if count := client.callCount("https://example.test/sitemap.xml"); count != 1 {
			t.Fatalf("cycle sitemap calls = %d, want 1", count)
		}
		if result.Sources.SitemapFiles != 1 {
			t.Fatalf("stats = %#v", result.Sources)
		}
	})
}

func TestDiscoverSitemapFileBudgetHasDeterministicSourcePriority(t *testing.T) {
	for iteration := range 50 {
		client := newRouteDoer(map[string]routeResponse{
			"https://example.test/sitemap.xml": xmlRoute(`<sitemapindex><sitemap><loc>/default.xml</loc></sitemap></sitemapindex>`),
			"https://example.test/default.xml": xmlRoute(`<urlset><url><loc>/from-default</loc></url></urlset>`),
			"https://example.test/robots.txt":  textRoute("Sitemap: /robots.xml\n"),
			"https://example.test/robots.xml":  xmlRoute(`<urlset><url><loc>/from-robots</loc></url></urlset>`),
			"https://example.test/":            htmlRoute("<html></html>"),
		})
		service := mustService(t, client, Config{AllowPrivateNetworks: true, MaxSitemapFiles: 2})
		result, err := service.Discover(context.Background(), "https://example.test")
		if err != nil {
			t.Fatalf("iteration %d: %v", iteration, err)
		}
		if !contains(result.URLs, "https://example.test/from-default") || contains(result.URLs, "https://example.test/from-robots") {
			t.Fatalf("iteration %d URLs = %#v", iteration, result.URLs)
		}
		if client.called("https://example.test/robots.xml") {
			t.Fatalf("iteration %d: lower-priority robots sitemap consumed the file budget", iteration)
		}
	}
}

func TestDiscoverEnforcesURLLimitWithLimitPlusOneDetection(t *testing.T) {
	client := newRouteDoer(map[string]routeResponse{
		"https://example.test/sitemap.xml": xmlRoute(`<urlset>
			<url><loc>/z</loc></url><url><loc>/a</loc></url><url><loc>/b</loc></url>
		</urlset>`),
		"https://example.test/robots.txt": textRoute(""),
		"https://example.test/":           htmlRoute(`<a href="/home">home</a>`),
	})
	service := mustService(t, client, Config{AllowPrivateNetworks: true, MaxURLs: 2})
	result, err := service.Discover(context.Background(), "https://example.test")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://example.test/", "https://example.test/a"}
	if !reflect.DeepEqual(result.URLs, want) {
		t.Fatalf("URLs = %#v, want %#v", result.URLs, want)
	}
	assertWarning(t, result.Warnings, "sitemap", "url_limit")
	if !result.Sources.Truncated {
		t.Fatalf("stats = %#v, want truncated", result.Sources)
	}
}

func TestDiscoverEnforcesBodyAndContentTypeLimits(t *testing.T) {
	tests := []struct {
		name        string
		route       routeResponse
		maximum     int64
		warningCode string
	}{
		{name: "plain body", route: xmlRoute(strings.Repeat("x", 1024)), maximum: 64, warningCode: "body_limit"},
		{name: "gzip bomb", route: gzipRoute(strings.Repeat("x", 1024)), maximum: 64, warningCode: "body_limit"},
		{name: "wrong content type", route: routeResponse{status: 200, body: []byte(`<urlset/>`), contentType: "text/html"}, maximum: 1024, warningCode: "content_type"},
		{name: "invalid XML", route: xmlRoute(`<urlset>`), maximum: 1024, warningCode: "invalid_xml"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newRouteDoer(map[string]routeResponse{
				"https://example.test/sitemap.xml": test.route,
				"https://example.test/robots.txt":  textRoute(""),
				"https://example.test/":            htmlRoute("<html></html>"),
			})
			service := mustService(t, client, Config{AllowPrivateNetworks: true, MaxSitemapBytes: test.maximum})
			result, err := service.Discover(context.Background(), "https://example.test")
			if err != nil {
				t.Fatalf("Discover() error = %v", err)
			}
			assertWarning(t, result.Warnings, "sitemap", test.warningCode)
		})
	}

	t.Run("valid gzip", func(t *testing.T) {
		client := newRouteDoer(map[string]routeResponse{
			"https://example.test/sitemap.xml": gzipRoute(`<urlset><url><loc>/gzip</loc></url></urlset>`),
			"https://example.test/robots.txt":  textRoute(""),
			"https://example.test/":            htmlRoute("<html></html>"),
		})
		service := mustService(t, client, Config{AllowPrivateNetworks: true})
		result, err := service.Discover(context.Background(), "https://example.test")
		if err != nil || !contains(result.URLs, "https://example.test/gzip") {
			t.Fatalf("Discover() = %#v, %v", result, err)
		}
	})
}

func TestDiscoverUsesOneTotalDeadline(t *testing.T) {
	client := &blockingDoer{}
	service := mustService(t, client, Config{AllowPrivateNetworks: true, Timeout: 25 * time.Millisecond})
	started := time.Now()
	result, err := service.Discover(context.Background(), "https://example.test")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Discover() error = %v, want context deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 400*time.Millisecond {
		t.Fatalf("Discover exceeded total deadline: %v", elapsed)
	}
	if client.calls.Load() != 3 {
		t.Fatalf("HTTP calls = %d, want three bounded source attempts", client.calls.Load())
	}
	if len(result.Warnings) < 3 {
		t.Fatalf("warnings = %#v", result.Warnings)
	}
}

func TestDiscoverDoesNotStarveFastSourcesBehindSlowSitemap(t *testing.T) {
	service := mustService(t, &slowSitemapDoer{}, Config{AllowPrivateNetworks: true, Timeout: 30 * time.Millisecond})
	started := time.Now()
	result, err := service.Discover(context.Background(), "https://example.test")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Discover() error = %v, want context deadline", err)
	}
	if !contains(result.URLs, "https://example.test/fast") {
		t.Fatalf("URLs = %#v, fast homepage result was starved", result.URLs)
	}
	if elapsed := time.Since(started); elapsed > 400*time.Millisecond {
		t.Fatalf("Discover() elapsed = %v", elapsed)
	}
}

func TestDiscoverBoundsWarnings(t *testing.T) {
	var entries strings.Builder
	entries.WriteString("<urlset>")
	for index := range 100 {
		fmt.Fprintf(&entries, "<url><loc>ftp://bad.invalid/%d</loc></url>", index)
	}
	entries.WriteString("</urlset>")
	client := newRouteDoer(map[string]routeResponse{
		"https://example.test/sitemap.xml": xmlRoute(entries.String()),
		"https://example.test/robots.txt":  textRoute(""),
		"https://example.test/":            htmlRoute("<html></html>"),
	})
	service := mustService(t, client, Config{AllowPrivateNetworks: true, MaxWarnings: 4})
	result, err := service.Discover(context.Background(), "https://example.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Warnings) != 4 {
		t.Fatalf("warning count = %d, want hard bound 4: %#v", len(result.Warnings), result.Warnings)
	}
	assertWarning(t, result.Warnings, "discovery", warningLimitCode)
}

func TestDiscoverEnforcesRobotsAndHomepageBodyLimits(t *testing.T) {
	tests := []struct {
		name       string
		robotsBody string
		homeBody   string
		wantSource string
		wantCode   string
	}{
		{name: "robots exact", robotsBody: strings.Repeat("x", 16), homeBody: "", wantSource: "", wantCode: ""},
		{name: "robots limit plus one", robotsBody: strings.Repeat("x", 17), homeBody: "", wantSource: "robots", wantCode: "body_limit"},
		{name: "homepage exact", robotsBody: "", homeBody: strings.Repeat("x", 16), wantSource: "", wantCode: ""},
		{name: "homepage limit plus one", robotsBody: "", homeBody: strings.Repeat("x", 17), wantSource: "homepage", wantCode: "body_limit"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newRouteDoer(map[string]routeResponse{
				"https://example.test/sitemap.xml": xmlRoute("<urlset></urlset>"),
				"https://example.test/robots.txt":  textRoute(test.robotsBody),
				"https://example.test/":            htmlRoute(test.homeBody),
			})
			service := mustService(t, client, Config{AllowPrivateNetworks: true, MaxRobotsBytes: 16, MaxHomeBytes: 16})
			result, err := service.Discover(context.Background(), "https://example.test")
			if err != nil {
				t.Fatal(err)
			}
			if test.wantCode != "" {
				assertWarning(t, result.Warnings, test.wantSource, test.wantCode)
			} else if len(result.Warnings) != 0 {
				t.Fatalf("warnings = %#v", result.Warnings)
			}
		})
	}
}

func TestDiscoverHandlesHTTPContentEncodingGzip(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/sitemap.xml":
			writer.Header().Set("Content-Type", "application/xml")
			writer.Header().Set("Content-Encoding", "gzip")
			gzipWriter := gzip.NewWriter(writer)
			_, _ = fmt.Fprintf(gzipWriter, "<urlset><url><loc>%s/gzip-http</loc></url></urlset>", server.URL)
			_ = gzipWriter.Close()
		case "/robots.txt":
			writer.Header().Set("Content-Type", "text/plain")
		case "/":
			writer.Header().Set("Content-Type", "text/html")
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	service := mustService(t, server.Client(), Config{AllowPrivateNetworks: true})
	result, err := service.Discover(context.Background(), server.URL)
	if err != nil || !contains(result.URLs, server.URL+"/gzip-http") {
		t.Fatalf("Discover() = %#v, %v", result, err)
	}
}

func TestDiscoverRejectsMalformedGzip(t *testing.T) {
	client := newRouteDoer(map[string]routeResponse{
		"https://example.test/sitemap.xml": {status: 200, contentType: "application/gzip", body: []byte{0x1f, 0x8b, 0x00}},
		"https://example.test/robots.txt":  textRoute(""),
		"https://example.test/":            htmlRoute(""),
	})
	service := mustService(t, client, Config{AllowPrivateNetworks: true})
	result, err := service.Discover(context.Background(), "https://example.test")
	if err != nil {
		t.Fatal(err)
	}
	assertWarning(t, result.Warnings, "sitemap", "read_failed")
}

func TestParseSitemapStreamsWithinEntryCaps(t *testing.T) {
	var body strings.Builder
	body.WriteString("<urlset>")
	for index := range 1000 {
		fmt.Fprintf(&body, "<url><loc>/item-%04d</loc></url>", index)
	}
	body.WriteString("</urlset>")
	document, err := parseSitemap([]byte(body.String()), 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(document.URLs) != 3 || !document.Truncated {
		t.Fatalf("document URLs=%d truncated=%v", len(document.URLs), document.Truncated)
	}
	if _, err := parseSitemap([]byte(`<urlset></urlset><urlset></urlset>`), 2, 2); err == nil {
		t.Fatal("multiple XML roots were accepted")
	}
}

func TestDiscoverPropagatesCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	service := mustService(t, &blockingDoer{}, Config{AllowPrivateNetworks: true})
	result, err := service.Discover(ctx, "https://example.test")
	if !errors.Is(err, context.Canceled) || result == nil {
		t.Fatalf("Discover() = %#v, %v", result, err)
	}
}

func TestDiscoverWithHTTPTestServerAndRelativeRobotsSitemap(t *testing.T) {
	var server *httptest.Server
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/sitemap.xml":
			writer.Header().Set("Content-Type", "application/xml")
			fmt.Fprint(writer, `<urlset><url><loc>`+server.URL+`/from-default</loc></url></urlset>`)
		case "/robots.txt":
			writer.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(writer, "Sitemap: maps/site.xml\n")
		case "/maps/site.xml":
			writer.Header().Set("Content-Type", "application/xml")
			fmt.Fprint(writer, `<urlset><url><loc>/from-robots</loc></url></urlset>`)
		case "/":
			writer.Header().Set("Content-Type", "text/html")
			fmt.Fprint(writer, `<a href="/from-home">home</a>`)
		default:
			http.NotFound(writer, request)
		}
	})
	server = httptest.NewServer(handler)
	defer server.Close()
	service := mustService(t, server.Client(), Config{AllowPrivateNetworks: true})
	result, err := service.Discover(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("Discover() error = %v, warnings=%#v", err, result.Warnings)
	}
	for _, suffix := range []string{"/from-default", "/from-robots", "/from-home"} {
		if !contains(result.URLs, server.URL+suffix) {
			t.Fatalf("URLs = %#v, missing %s", result.URLs, suffix)
		}
	}
}

func TestDiscoverClosesBodiesAndSendsSharedHeaders(t *testing.T) {
	var closed atomic.Int32
	client := &headerDoer{closed: &closed}
	service := mustService(t, client, Config{AllowPrivateNetworks: true, UserAgent: "test-agent"})
	_, _ = service.Discover(context.Background(), "https://example.test")
	if closed.Load() != 3 {
		t.Fatalf("closed bodies = %d, want 3", closed.Load())
	}
	if client.badHeaders.Load() != 0 {
		t.Fatal("discovery requests did not share Accept/User-Agent headers")
	}
}

func TestServiceSupportsConcurrentDiscovery(t *testing.T) {
	client := newRouteDoer(map[string]routeResponse{
		"https://example.test/sitemap.xml": xmlRoute(`<urlset><url><loc>/one</loc></url></urlset>`),
		"https://example.test/robots.txt":  textRoute(""),
		"https://example.test/":            htmlRoute(`<a href="/two">two</a>`),
	})
	service := mustService(t, client, Config{AllowPrivateNetworks: true})
	const workers = 32
	var group sync.WaitGroup
	errorsByWorker := make(chan error, workers)
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := service.Discover(context.Background(), "https://example.test")
			if err == nil && len(result.URLs) != 3 {
				err = fmt.Errorf("URL count = %d", len(result.URLs))
			}
			errorsByWorker <- err
		}()
	}
	group.Wait()
	close(errorsByWorker)
	for err := range errorsByWorker {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestNewServiceDefaultsAndValidation(t *testing.T) {
	if _, err := NewService(Config{MaxURLs: -1}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("negative config error = %v", err)
	}
	service := mustService(t, nil, Config{})
	if service.cfg.Timeout != defaultTimeout || service.cfg.MaxSitemapDepth != defaultMaxSitemapDepth || service.cfg.MaxSitemapFiles != defaultMaxSitemapFiles || service.cfg.MaxSitemapBytes != defaultMaxSitemapBytes || service.cfg.MaxRobotsBytes != defaultMaxRobotsBytes || service.cfg.MaxHomeBytes != defaultMaxHomeBytes || service.cfg.MaxURLs != defaultMaxURLs || service.cfg.MaxWarnings != defaultMaxWarnings || service.cfg.MaxRedirects != defaultMaxRedirects || service.cfg.UserAgent != defaultUserAgent {
		t.Fatalf("defaults = %#v", service.cfg)
	}
	unsafeConfigs := []Config{
		{Timeout: hardMaxTimeout + time.Nanosecond},
		{MaxSitemapDepth: hardMaxSitemapDepth + 1},
		{MaxSitemapFiles: hardMaxSitemapFiles + 1},
		{MaxSitemapBytes: hardMaxBodyBytes + 1},
		{MaxRobotsBytes: int64(^uint64(0) >> 1)},
		{MaxURLs: hardMaxURLs + 1},
		{MaxWarnings: hardMaxWarnings + 1},
		{MaxRedirects: hardMaxRedirects + 1},
	}
	for _, cfg := range unsafeConfigs {
		if _, err := NewService(cfg); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("hard limit config %#v error = %v", cfg, err)
		}
	}
}

type routeResponse struct {
	status      int
	body        []byte
	contentType string
	finalURL    string
}

func xmlRoute(body string) routeResponse {
	return routeResponse{status: http.StatusOK, body: []byte(body), contentType: "application/xml; charset=utf-8"}
}

func textRoute(body string) routeResponse {
	return routeResponse{status: http.StatusOK, body: []byte(body), contentType: "text/plain; charset=utf-8"}
}

func htmlRoute(body string) routeResponse {
	return routeResponse{status: http.StatusOK, body: []byte(body), contentType: "text/html; charset=utf-8"}
}

func gzipRoute(body string) routeResponse {
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	_, _ = writer.Write([]byte(body))
	_ = writer.Close()
	return routeResponse{status: http.StatusOK, body: buffer.Bytes(), contentType: "application/gzip"}
}

type routeDoer struct {
	mu     sync.Mutex
	routes map[string]routeResponse
	calls  []string
}

func newRouteDoer(routes map[string]routeResponse) *routeDoer {
	return &routeDoer{routes: routes}
}

func (client *routeDoer) Do(request *http.Request) (*http.Response, error) {
	client.mu.Lock()
	client.calls = append(client.calls, request.URL.String())
	route, ok := client.routes[request.URL.String()]
	client.mu.Unlock()
	if !ok {
		route = routeResponse{status: http.StatusNotFound, contentType: "text/plain"}
	}
	status := route.status
	if status == 0 {
		status = http.StatusOK
	}
	final := request.URL
	if route.finalURL != "" {
		parsed, err := url.Parse(route.finalURL)
		if err != nil {
			return nil, err
		}
		final = parsed
	}
	responseRequest := request.Clone(request.Context())
	responseRequest.URL = final
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{route.contentType}},
		Body:       io.NopCloser(bytes.NewReader(append([]byte(nil), route.body...))),
		Request:    responseRequest,
	}, nil
}

func (client *routeDoer) callURLs() []string {
	client.mu.Lock()
	defer client.mu.Unlock()
	return append([]string(nil), client.calls...)
}

func (client *routeDoer) called(rawURL string) bool { return client.callCount(rawURL) > 0 }

func (client *routeDoer) callCount(rawURL string) int {
	count := 0
	for _, value := range client.callURLs() {
		if value == rawURL {
			count++
		}
	}
	return count
}

type blockingDoer struct{ calls atomic.Int32 }

func (client *blockingDoer) Do(request *http.Request) (*http.Response, error) {
	client.calls.Add(1)
	<-request.Context().Done()
	return nil, request.Context().Err()
}

type slowSitemapDoer struct{}

func (client *slowSitemapDoer) Do(request *http.Request) (*http.Response, error) {
	if request.URL.Path == "/sitemap.xml" {
		<-request.Context().Done()
		return nil, request.Context().Err()
	}
	body := ""
	contentType := "text/plain"
	if request.URL.Path == "/" {
		body = `<a href="/fast">fast</a>`
		contentType = "text/html"
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}, nil
}

type headerDoer struct {
	closed     *atomic.Int32
	badHeaders atomic.Int32
}

func (client *headerDoer) Do(request *http.Request) (*http.Response, error) {
	if request.Header.Get("User-Agent") != "test-agent" || request.Header.Get("Accept") == "" {
		client.badHeaders.Add(1)
	}
	body := &trackingBody{Reader: strings.NewReader(""), closed: client.closed}
	contentType := "text/plain"
	if strings.HasSuffix(request.URL.Path, "/") {
		contentType = "text/html"
	}
	if strings.HasSuffix(request.URL.Path, ".xml") {
		contentType = "application/xml"
		body.Reader = strings.NewReader("<urlset></urlset>")
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{contentType}}, Body: body, Request: request}, nil
}

type trackingBody struct {
	io.Reader
	closed *atomic.Int32
}

func (body *trackingBody) Close() error {
	body.closed.Add(1)
	return nil
}

func mustService(t *testing.T, client httpDoer, cfg Config) *Service {
	t.Helper()
	service, err := newServiceWithClient(client, cfg)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return service
}

func assertWarning(t *testing.T, warnings []Warning, source, code string) {
	t.Helper()
	for _, warning := range warnings {
		if warning.Source == source && warning.Code == code {
			return
		}
	}
	t.Fatalf("warnings = %#v, missing %s/%s", warnings, source, code)
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
