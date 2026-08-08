package testsite

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestStaticFixtureIncludesRelativeAssets(t *testing.T) {
	site := New()
	t.Cleanup(site.Close)

	resp := get(t, site.Client(), site.URL("/static"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := readAndClose(t, resp)
	for _, want := range []string{StaticTitle, StaticMarker, `href="` + RelativeLinkPath + `"`, `src="` + RelativeImagePath + `"`} {
		if !strings.Contains(body, want) {
			t.Errorf("static body missing %q", want)
		}
	}
}

func TestDelayedBodyFixtureEventuallyCompletes(t *testing.T) {
	site := New()
	t.Cleanup(site.Close)

	started := time.Now()
	resp := get(t, site.Client(), site.URL("/delayed-body"))
	body := readAndClose(t, resp)
	if !strings.Contains(body, DelayedBodyMarker) {
		t.Fatalf("delayed body missing marker: %s", body)
	}
	if elapsed := time.Since(started); elapsed < DelayedBodyDelay {
		t.Fatalf("response completed in %v, want at least %v", elapsed, DelayedBodyDelay)
	}
}

func TestSPAFixtureDeclaresDelayedContent(t *testing.T) {
	site := New()
	t.Cleanup(site.Close)

	resp := get(t, site.Client(), site.URL("/spa"))
	body := readAndClose(t, resp)
	for _, want := range []string{`data-state="loading"`, SPAMarker, strconv.FormatInt(SPADelay.Milliseconds(), 10)} {
		if !strings.Contains(body, want) {
			t.Errorf("SPA body missing %q", want)
		}
	}
}

func TestRedirectFixtureHasStableFinalURL(t *testing.T) {
	site := New()
	t.Cleanup(site.Close)

	resp := get(t, site.Client(), site.URL("/redirect/start"))
	body := readAndClose(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Request.URL.Path; got != "/static" {
		t.Fatalf("final path = %q, want /static", got)
	}
	if got := resp.Request.URL.Query().Get("redirected"); got != "1" {
		t.Fatalf("redirected query = %q, want 1", got)
	}
	if !strings.Contains(body, StaticMarker) {
		t.Fatalf("redirect body missing static marker")
	}
}

func TestStatusFixtures(t *testing.T) {
	site := New()
	t.Cleanup(site.Close)

	for _, code := range []int{http.StatusNotFound, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			resp := get(t, site.Client(), site.URL("/status/"+httpStatusString(code)))
			body := readAndClose(t, resp)
			if resp.StatusCode != code {
				t.Fatalf("status = %d, want %d", resp.StatusCode, code)
			}
			if !strings.Contains(body, "fixture status "+httpStatusString(code)) {
				t.Fatalf("body = %q, want status marker", body)
			}
		})
	}
}

func TestHeaderAndCookieFixtures(t *testing.T) {
	site := New()
	t.Cleanup(site.Close)

	t.Run("header required", func(t *testing.T) {
		resp := get(t, site.Client(), site.URL("/requires-header"))
		_ = readAndClose(t, resp)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status without header = %d, want 403", resp.StatusCode)
		}

		req, err := http.NewRequest(http.MethodGet, site.URL("/requires-header"), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(HeaderName, HeaderValue)
		resp = do(t, site.Client(), req)
		body := readAndClose(t, resp)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, "header protected fixture content") {
			t.Fatalf("authorized header response = status %d body %q", resp.StatusCode, body)
		}
	})

	t.Run("cookie required", func(t *testing.T) {
		resp := get(t, site.Client(), site.URL("/requires-cookie"))
		_ = readAndClose(t, resp)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status without cookie = %d, want 403", resp.StatusCode)
		}

		req, err := http.NewRequest(http.MethodGet, site.URL("/requires-cookie"), nil)
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(&http.Cookie{Name: CookieName, Value: CookieValue})
		resp = do(t, site.Client(), req)
		body := readAndClose(t, resp)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, "cookie protected fixture content") {
			t.Fatalf("authorized cookie response = status %d body %q", resp.StatusCode, body)
		}
	})
}

func TestContentFixtures(t *testing.T) {
	site := New()
	t.Cleanup(site.Close)

	for _, test := range []struct {
		path  string
		wants []string
	}{
		{path: "/content/en", wants: []string{"Deterministic English fixture", "second paragraph"}},
		{path: "/content/zh", wants: []string{"中文内容", "完全本地", "第二段文字"}},
		{path: "/content/document", wants: []string{DocumentMarker, "Fixture API Reference", `<section id="request">`, "<dl>"}},
		{path: "/content/navigation", wants: []string{NavigationMarker, "Navigation item 1", "Navigation item 40", `aria-label="primary"`}},
		{path: "/content/table-code", wants: []string{"<table>", "Feature", "<pre><code>", "func main()"}},
	} {
		t.Run(test.path, func(t *testing.T) {
			resp := get(t, site.Client(), site.URL(test.path))
			body := readAndClose(t, resp)
			for _, want := range test.wants {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %q", want)
				}
			}
		})
	}
}

func TestNavigationFixtureIsNavigationDense(t *testing.T) {
	site := New()
	t.Cleanup(site.Close)

	resp := get(t, site.Client(), site.URL("/content/navigation"))
	body := readAndClose(t, resp)
	if got := strings.Count(body, `<a href="/navigation/item/`); got != 40 {
		t.Fatalf("navigation item count = %d, want 40", got)
	}
	if !strings.Contains(body, NavigationMarker) {
		t.Fatal("navigation-dense body missing main-content marker")
	}
}

func TestLargeFixtureExceedsOneMiB(t *testing.T) {
	site := New()
	t.Cleanup(site.Close)

	resp := get(t, site.Client(), site.URL("/large"))
	body := readAndClose(t, resp)
	if len(body) < LargeMinimumBytes {
		t.Fatalf("large body length = %d, want >= %d", len(body), LargeMinimumBytes)
	}
	if !strings.Contains(body, LargeMarker) {
		t.Fatal("large body missing marker")
	}
}

func TestTimeoutFixtureHonorsClientCancellation(t *testing.T) {
	site := New()
	t.Cleanup(site.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, site.URL("/timeout?delay_ms=500"), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = site.Client().Do(req)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline exceeded", err)
	}
}

func TestURLReturnsSameOrigin(t *testing.T) {
	site := New()
	t.Cleanup(site.Close)

	parsed, err := url.Parse(site.URL("/static"))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme != "http" || parsed.Host == "" || parsed.Path != "/static" {
		t.Fatalf("fixture URL = %q", parsed.String())
	}
}

func get(t *testing.T, client *http.Client, target string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	return do(t, client, req)
}

func do(t *testing.T, client *http.Client, req *http.Request) *http.Response {
	t.Helper()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func readAndClose(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func httpStatusString(code int) string {
	return strconv.Itoa(code)
}
