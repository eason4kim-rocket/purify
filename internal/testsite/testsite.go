// Package testsite provides a deterministic local website for scraper tests.
// It deliberately exercises transport, rendering, metadata, and content cases
// without depending on public websites or credentials.
package testsite

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"time"
)

const (
	StaticTitle       = "Purify Static Fixture"
	StaticMarker      = "deterministic static fixture content"
	DelayedBodyMarker = "delayed body fixture content"
	SPAMarker         = "spa fixture content loaded"
	DocumentMarker    = "deterministic API reference fixture"
	NavigationMarker  = "navigation dense fixture main content"
	HeaderName        = "X-Purify-Fixture"
	HeaderValue       = "header-ok"
	CookieName        = "purify_fixture"
	CookieValue       = "cookie-ok"
	RelativeLinkPath  = "/relative-target"
	RelativeImagePath = "/assets/fixture.png"
	LargeMarker       = "purify-large-fixture-row"

	// The SPA delay intentionally exceeds the old 300ms DOM-stable window.
	// This keeps the browser regression deterministic once its skip is removed.
	SPADelay = 750 * time.Millisecond

	// DelayedBodyDelay simulates a response whose body is not available with
	// the initial head bytes.
	DelayedBodyDelay = 350 * time.Millisecond

	LargeMinimumBytes = 1 << 20
)

// Site owns an httptest server serving the deterministic fixture routes.
type Site struct {
	server *httptest.Server
}

// New starts a deterministic local fixture website.
func New() *Site {
	mux := http.NewServeMux()
	site := &Site{}

	mux.HandleFunc("/static", staticHandler)
	mux.HandleFunc("/delayed-body", delayedBodyHandler)
	mux.HandleFunc("/spa", spaHandler)
	mux.HandleFunc("/redirect/start", redirectStartHandler)
	mux.HandleFunc("/redirect/middle", redirectMiddleHandler)
	mux.HandleFunc("/status/404", statusHandler(http.StatusNotFound))
	mux.HandleFunc("/status/429", statusHandler(http.StatusTooManyRequests))
	mux.HandleFunc("/status/500", statusHandler(http.StatusInternalServerError))
	mux.HandleFunc("/requires-header", requiresHeaderHandler)
	mux.HandleFunc("/requires-cookie", requiresCookieHandler)
	mux.HandleFunc("/content/en", englishHandler)
	mux.HandleFunc("/content/zh", chineseHandler)
	mux.HandleFunc("/content/document", documentHandler)
	mux.HandleFunc("/content/navigation", navigationHandler)
	mux.HandleFunc("/content/table-code", tableCodeHandler)
	mux.HandleFunc("/large", largeHandler)
	mux.HandleFunc("/timeout", timeoutHandler)
	mux.HandleFunc(RelativeLinkPath, relativeTargetHandler)
	mux.HandleFunc(RelativeImagePath, imageHandler)

	site.server = httptest.NewServer(mux)
	return site
}

// Close stops the fixture website.
func (s *Site) Close() {
	if s != nil && s.server != nil {
		s.server.Close()
	}
}

// URL resolves a fixture path against the local server origin.
func (s *Site) URL(path string) string {
	return s.server.URL + path
}

// Client returns the client configured by httptest for this site.
func (s *Site) Client() *http.Client {
	return s.server.Client()
}

func staticHandler(w http.ResponseWriter, _ *http.Request) {
	writeHTML(w, http.StatusOK, `<!doctype html>
<html lang="en">
<head>
  <title>`+StaticTitle+`</title>
  <meta name="description" content="A deterministic static fixture">
</head>
<body>
  <main>
    <article>
      <h1>Static fixture</h1>
      <p>`+StaticMarker+` for local scraper tests.</p>
      <a href="`+RelativeLinkPath+`">Relative target</a>
      <img src="`+RelativeImagePath+`" alt="Fixture image">
    </article>
  </main>
</body>
</html>`)
}

func delayedBodyHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, `<!doctype html><html><head><title>Delayed Body Fixture</title><script>window.fixtureHeadSeen=true;</script></head>`)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	select {
	case <-time.After(DelayedBodyDelay):
	case <-r.Context().Done():
		return
	}

	_, _ = fmt.Fprint(w, `<body><main><h1>Delayed body</h1><p>`+DelayedBodyMarker+`</p></main></body></html>`)
}

func spaHandler(w http.ResponseWriter, _ *http.Request) {
	delayMS := SPADelay.Milliseconds()
	writeHTML(w, http.StatusOK, fmt.Sprintf(`<!doctype html>
<html lang="en">
<head><title>SPA Fixture</title></head>
<body>
  <main id="app" data-state="loading"></main>
  <script>
    setTimeout(() => {
      const app = document.getElementById('app');
      app.dataset.state = 'ready';
      app.innerHTML = '<h1>SPA fixture</h1><p>%s</p>';
    }, %d);
  </script>
</body>
</html>`, SPAMarker, delayMS))
}

func redirectStartHandler(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/redirect/middle?from=start", http.StatusFound)
}

func redirectMiddleHandler(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/static?redirected=1", http.StatusMovedPermanently)
}

func statusHandler(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeHTML(w, code, fmt.Sprintf(`<!doctype html><html><head><title>Status %d</title></head><body><main><p>fixture status %d</p></main></body></html>`, code, code))
	}
}

func requiresHeaderHandler(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(HeaderName) != HeaderValue {
		writeHTML(w, http.StatusForbidden, `<!doctype html><html><body><p>missing fixture header</p></body></html>`)
		return
	}
	writeHTML(w, http.StatusOK, `<!doctype html><html><head><title>Header Fixture</title></head><body><main><p>header protected fixture content</p></main></body></html>`)
}

func requiresCookieHandler(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(CookieName)
	if err != nil || cookie.Value != CookieValue {
		writeHTML(w, http.StatusForbidden, `<!doctype html><html><body><p>missing fixture cookie</p></body></html>`)
		return
	}
	writeHTML(w, http.StatusOK, `<!doctype html><html><head><title>Cookie Fixture</title></head><body><main><p>cookie protected fixture content</p></main></body></html>`)
}

func englishHandler(w http.ResponseWriter, _ *http.Request) {
	writeHTML(w, http.StatusOK, `<!doctype html><html lang="en"><head><title>English Fixture</title></head><body><main><article><h1>English content</h1><p>Deterministic English fixture with enough prose for extraction and formatting checks.</p><p>The second paragraph keeps article extraction stable across runs.</p></article></main></body></html>`)
}

func chineseHandler(w http.ResponseWriter, _ *http.Request) {
	writeHTML(w, http.StatusOK, `<!doctype html><html lang="zh-CN"><head><title>中文测试页面</title></head><body><main><article><h1>中文内容</h1><p>这是一个完全本地、可重复执行的中文抓取测试页面，用于验证字符编码和正文清洗。</p><p>第二段文字用于确保正文提取结果稳定。</p></article></main></body></html>`)
}

func documentHandler(w http.ResponseWriter, _ *http.Request) {
	writeHTML(w, http.StatusOK, `<!doctype html>
<html lang="en">
<head><title>Fixture API Reference</title></head>
<body>
  <aside><a href="#overview">Overview</a><a href="#request">Request</a><a href="#response">Response</a></aside>
  <main>
    <article>
      <h1>Fixture API Reference</h1>
      <p>`+DocumentMarker+` for scraper document-page tests.</p>
      <section id="overview"><h2>Overview</h2><p>The endpoint returns a stable local response.</p></section>
      <section id="request"><h2>Request</h2><dl><dt>URL</dt><dd>A local fixture URL.</dd><dt>Method</dt><dd>GET</dd></dl></section>
      <section id="response"><h2>Response</h2><p>Status 200 with deterministic HTML.</p></section>
    </article>
  </main>
</body>
</html>`)
}

func navigationHandler(w http.ResponseWriter, _ *http.Request) {
	links := make([]string, 0, 40)
	for i := 1; i <= 40; i++ {
		links = append(links, fmt.Sprintf(`<li><a href="/navigation/item/%d">Navigation item %d</a></li>`, i, i))
	}
	writeHTML(w, http.StatusOK, `<!doctype html>
<html lang="en">
<head><title>Navigation Dense Fixture</title></head>
<body>
  <header><nav aria-label="primary"><ul>`+strings.Join(links, "")+`</ul></nav></header>
  <main><article><h1>Navigation result</h1><p>`+NavigationMarker+` remains distinct from forty navigation links.</p></article></main>
  <footer><nav><a href="/about">About</a><a href="/privacy">Privacy</a></nav></footer>
</body>
</html>`)
}

func tableCodeHandler(w http.ResponseWriter, _ *http.Request) {
	writeHTML(w, http.StatusOK, `<!doctype html><html lang="en"><head><title>Table and Code Fixture</title></head><body><main><article><h1>Structured content</h1><table><thead><tr><th>Feature</th><th>Status</th></tr></thead><tbody><tr><td>Fixture server</td><td>Ready</td></tr></tbody></table><pre><code>package main

func main() {
    println("fixture")
}</code></pre></article></main></body></html>`)
}

func largeHandler(w http.ResponseWriter, _ *http.Request) {
	row := `<p>` + LargeMarker + ` contains deterministic payload text for bounded large-response tests.</p>`
	count := LargeMinimumBytes/len(row) + 2
	writeHTML(w, http.StatusOK, `<!doctype html><html><head><title>Large Fixture</title></head><body><main>`+strings.Repeat(row, count)+`</main></body></html>`)
}

func timeoutHandler(w http.ResponseWriter, r *http.Request) {
	delay := 2 * time.Second
	if raw := r.URL.Query().Get("delay_ms"); raw != "" {
		if milliseconds, err := strconv.Atoi(raw); err == nil && milliseconds >= 0 && milliseconds <= 10_000 {
			delay = time.Duration(milliseconds) * time.Millisecond
		}
	}

	select {
	case <-time.After(delay):
		writeHTML(w, http.StatusOK, `<!doctype html><html><head><title>Timeout Fixture</title></head><body><main><p>timeout fixture eventually completed</p></main></body></html>`)
	case <-r.Context().Done():
		return
	}
}

func relativeTargetHandler(w http.ResponseWriter, _ *http.Request) {
	writeHTML(w, http.StatusOK, `<!doctype html><html><head><title>Relative Target</title></head><body><main><p>relative target fixture content</p></main></body></html>`)
}

func imageHandler(w http.ResponseWriter, _ *http.Request) {
	// A valid image is unnecessary for URL extraction tests; a deterministic
	// payload and image content type are sufficient.
	w.Header().Set("Content-Type", "image/png")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("purify-fixture-image"))
}

func writeHTML(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprint(w, body)
}
