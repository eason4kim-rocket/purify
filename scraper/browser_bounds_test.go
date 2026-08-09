package scraper

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/use-agent/purify/config"
	"github.com/use-agent/purify/engine"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/proxy"
)

func TestBoundedBrowserEnforcesMainDocumentAndRenderedUTF8Limits(t *testing.T) {
	browserBin := boundedBrowserTestBinary(t)
	exactHTML := `<html><head><title>bounded</title></head><body>é界🙂</body></html>`
	renderedExpansion := `<html><head></head><body><script>document.body.textContent = "z".repeat(4096)</script></body></html>`
	gzipExact := gzipFixture(t, exactHTML)
	gzipOver := gzipFixture(t, exactHTML+"x")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch request.URL.Path {
		case "/exact":
			_, _ = writer.Write([]byte(exactHTML))
		case "/over":
			_, _ = writer.Write([]byte(exactHTML + "x"))
		case "/rendered-expansion":
			_, _ = writer.Write([]byte(renderedExpansion))
		case "/gzip-exact":
			writer.Header().Set("Content-Encoding", "gzip")
			_, _ = writer.Write(gzipExact)
		case "/gzip-over":
			writer.Header().Set("Content-Encoding", "gzip")
			_, _ = writer.Write(gzipOver)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)

	sc := newBoundedBrowserTestScraper(t, browserBin, 1)
	limit := int64(len(exactHTML))
	result, err := sc.DoScrapeRodBounded(context.Background(), &models.ScrapeRequest{
		URL:     server.URL + "/exact",
		Timeout: 5,
	}, limit)
	if err != nil {
		t.Fatalf("exact N-byte browser response failed: %v", err)
	}
	if result.RawHTML != exactHTML || int64(len(result.RawHTML)) != limit {
		t.Fatalf("exact response = %q (%d bytes), want %q (%d bytes)", result.RawHTML, len(result.RawHTML), exactHTML, limit)
	}

	_, err = sc.DoScrapeRodBounded(context.Background(), &models.ScrapeRequest{
		URL:     server.URL + "/over",
		Timeout: 5,
	}, limit)
	if !errors.Is(err, engine.ErrResponseBodyTooLarge) {
		t.Fatalf("N+1 browser response error = %v", err)
	}

	gzipResult, err := sc.DoScrapeRodBounded(context.Background(), &models.ScrapeRequest{
		URL:     server.URL + "/gzip-exact",
		Timeout: 5,
	}, limit)
	if err != nil || gzipResult.RawHTML != exactHTML {
		t.Fatalf("gzip exact response = (%#v, %v)", gzipResult, err)
	}
	_, err = sc.DoScrapeRodBounded(context.Background(), &models.ScrapeRequest{
		URL:     server.URL + "/gzip-over",
		Timeout: 5,
	}, limit)
	if !errors.Is(err, engine.ErrResponseBodyTooLarge) {
		t.Fatalf("gzip decoded N+1 response error = %v", err)
	}

	// The wire body is below this limit, but JavaScript expands the rendered
	// DOM above it. The renderer returns only the oversized flag over CDP.
	renderedLimit := int64(len(renderedExpansion) + 128)
	_, err = sc.DoScrapeRodBounded(context.Background(), &models.ScrapeRequest{
		URL:     server.URL + "/rendered-expansion",
		Timeout: 5,
	}, renderedLimit)
	if !errors.Is(err, engine.ErrResponseBodyTooLarge) {
		t.Fatalf("oversized rendered DOM error = %v", err)
	}
}

func TestBoundedBrowserStopsChunkedMainDocumentAtNPlusOne(t *testing.T) {
	browserBin := boundedBrowserTestBinary(t)
	const limit = int64(1024)
	extraSent := make(chan struct{})
	requestCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		flusher, ok := writer.(http.Flusher)
		if !ok {
			t.Error("test response writer does not implement http.Flusher")
			return
		}
		_, _ = writer.Write([]byte(strings.Repeat("x", int(limit))))
		flusher.Flush()
		_, _ = writer.Write([]byte("y"))
		flusher.Flush()
		close(extraSent)
		select {
		case <-request.Context().Done():
			close(requestCanceled)
		case <-time.After(5 * time.Second):
			t.Error("browser did not abort the chunked response after N+1 bytes")
		}
	}))
	t.Cleanup(server.Close)

	sc := newBoundedBrowserTestScraper(t, browserBin, 1)
	_, err := sc.DoScrapeRodBounded(context.Background(), &models.ScrapeRequest{
		URL:     server.URL,
		Timeout: 5,
	}, limit)
	if !errors.Is(err, engine.ErrResponseBodyTooLarge) {
		t.Fatalf("chunked N+1 browser response error = %v", err)
	}
	select {
	case <-extraSent:
	case <-time.After(time.Second):
		t.Fatal("chunked fixture never sent byte N+1")
	}
	select {
	case <-requestCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("browser left the oversized chunked response open")
	}
}

func TestBoundedBrowserEnforcesOneDecodedBudgetAcrossSubresources(t *testing.T) {
	browserBin := boundedBrowserTestBinary(t)
	const mainLimit = int64(1024)
	const totalLimit = mainLimit * 4
	exactPage := `<html><head><script src="/resource-exact.js"></script></head><body>total-exact</body></html>`
	overPage := `<html><head><script src="/resource-over.js"></script></head><body>total-over</body></html>`
	exactResourceSize := int(totalLimit) - len(exactPage)
	overResourceSize := int(totalLimit) - len(overPage)
	if exactResourceSize <= 4 || overResourceSize <= 4 {
		t.Fatal("invalid total-resource fixture size")
	}
	var exactHits atomic.Int32
	var overHits atomic.Int32
	overCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/exact-page":
			writer.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = writer.Write([]byte(exactPage))
		case "/over-page":
			writer.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = writer.Write([]byte(overPage))
		case "/resource-exact.js":
			exactHits.Add(1)
			writer.Header().Set("Content-Type", "application/javascript")
			_, _ = writer.Write(scriptFixture(exactResourceSize))
		case "/resource-over.js":
			overHits.Add(1)
			writer.Header().Set("Content-Type", "application/javascript")
			flusher, ok := writer.(http.Flusher)
			if !ok {
				t.Error("test response writer does not implement http.Flusher")
				return
			}
			_, _ = writer.Write(scriptFixture(overResourceSize))
			flusher.Flush()
			_, _ = writer.Write([]byte("x"))
			flusher.Flush()
			select {
			case <-request.Context().Done():
				close(overCanceled)
			case <-time.After(5 * time.Second):
				t.Error("browser did not abort total-resource byte N+1")
			}
		default:
			writer.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(server.Close)

	sc := newBoundedBrowserTestScraper(t, browserBin, 1)
	result, err := sc.DoScrapeRodBounded(context.Background(), &models.ScrapeRequest{
		URL:     server.URL + "/exact-page",
		Timeout: 5,
	}, mainLimit)
	if err != nil || !strings.Contains(result.RawHTML, "total-exact") || exactHits.Load() != 1 {
		t.Fatalf("exact total-resource budget = (%#v, %v), resource hits=%d", result, err, exactHits.Load())
	}

	_, err = sc.DoScrapeRodBounded(context.Background(), &models.ScrapeRequest{
		URL:     server.URL + "/over-page",
		Timeout: 5,
	}, mainLimit)
	if !errors.Is(err, engine.ErrResponseBodyTooLarge) || overHits.Load() != 1 {
		t.Fatalf("total-resource N+1 error = %v, resource hits=%d", err, overHits.Load())
	}
	select {
	case <-overCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("total-resource N+1 response remained open")
	}
}

func TestBoundedBrowserBlocksWebSocketConstruction(t *testing.T) {
	browserBin := boundedBrowserTestBinary(t)
	pageHTML := `<html><head></head><body><div id="state"></div><iframe src="/child-frame"></iframe><script>
		const state = document.getElementById("state");
		try {
			new WebSocket("ws://127.0.0.1:1/unbounded");
			state.textContent += "websocket-not-blocked ";
		} catch (error) {
			state.textContent += "websocket-blocked ";
		}
		try {
			new Worker("/worker.js");
			state.textContent += "worker-not-blocked";
		} catch (error) {
			state.textContent += "worker-blocked";
		}
	</script></body></html>`
	var childFrameHits atomic.Int32
	var workerHits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/child-frame":
			childFrameHits.Add(1)
		case "/worker.js":
			workerHits.Add(1)
		default:
			writer.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = writer.Write([]byte(pageHTML))
		}
	}))
	t.Cleanup(server.Close)

	sc := newBoundedBrowserTestScraper(t, browserBin, 1)
	result, err := sc.DoScrapeRodBounded(context.Background(), &models.ScrapeRequest{
		URL:     server.URL,
		Timeout: 5,
	}, 1<<10)
	if err != nil {
		t.Fatalf("bounded WebSocket page error = %v", err)
	}
	if !strings.Contains(result.RawHTML, `<div id="state">websocket-blocked worker-blocked</div>`) {
		t.Fatalf("bounded WebSocket result = %s", result.RawHTML)
	}
	if childFrameHits.Load() != 0 || workerHits.Load() != 0 {
		t.Fatalf("unscoped child targets reached network: iframe=%d worker=%d", childFrameHits.Load(), workerHits.Load())
	}
}

func TestBoundedNetworkScriptBlocksDirectRTCTransportConstructors(t *testing.T) {
	for _, constructor := range []string{
		"RTCPeerConnection",
		"webkitRTCPeerConnection",
		"mozRTCPeerConnection",
		"RTCIceGatherer",
		"RTCIceTransport",
		"RTCDtlsTransport",
		"RTCSctpTransport",
		"RTCQuicTransport",
	} {
		if !strings.Contains(blockUnscopedNetworkScript, `"`+constructor+`"`) {
			t.Errorf("bounded document-start script does not disable %s", constructor)
		}
	}
	for _, hardening := range []string{
		"configurable: false",
		"writable: false",
		"failed to disable ",
	} {
		if !strings.Contains(blockUnscopedNetworkScript, hardening) {
			t.Errorf("bounded document-start script is missing %q", hardening)
		}
	}
}

func TestBoundedRequestProxyBlocksWebRTCUDPBypass(t *testing.T) {
	browserBin := boundedBrowserTestBinary(t)
	pageServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(writer, `<html><head></head><body><div id="rtc-state"></div><script>
			const state = document.getElementById("rtc-state");
			try {
				const peer = new RTCPeerConnection({iceServers: [{urls: "stun:127.0.0.1:%s"}]});
				globalThis.__purifyRTCProbe = peer;
				peer.createDataChannel("purify-probe");
				peer.createOffer().then((offer) => peer.setLocalDescription(offer)).catch(() => {});
				state.textContent = "rtc-constructor-open";
			} catch (_) {
				state.textContent = "rtc-constructor-blocked";
			}
		</script></body></html>`, request.URL.Query().Get("stun_port"))
	}))
	t.Cleanup(pageServer.Close)

	var relayDials atomic.Int32
	dialer := &net.Dialer{}
	relay, err := proxy.StartDirectRelay(func(ctx context.Context, network, address string) (net.Conn, error) {
		relayDials.Add(1)
		return dialer.DialContext(ctx, network, address)
	})
	if err != nil {
		t.Fatalf("StartDirectRelay() error = %v", err)
	}
	t.Cleanup(func() { _ = relay.Close() })
	proxyURL := "socks5://" + relay.Addr()

	sc := newBoundedBrowserTestScraper(t, browserBin, 1)
	legacyProbe, legacyPort := listenWebRTCUDPProbe(t)
	legacyResult, err := sc.DoScrapeRodBounded(context.Background(), &models.ScrapeRequest{
		URL:      fmt.Sprintf("%s?stun_port=%d", pageServer.URL, legacyPort),
		Timeout:  5,
		ProxyURL: proxyURL,
		Actions:  []models.Action{{Type: "wait", Milliseconds: 1200}},
	}, 0)
	if err != nil {
		t.Fatalf("legacy WebRTC probe scrape error = %v", err)
	}
	if !strings.Contains(legacyResult.RawHTML, `id="rtc-state">rtc-constructor-open</div>`) {
		t.Fatalf("legacy WebRTC constructor was not available: %s", legacyResult.RawHTML)
	}
	if hit, readErr := readWebRTCUDPProbe(legacyProbe, 3*time.Second); readErr != nil {
		t.Fatalf("read legacy WebRTC UDP probe: %v", readErr)
	} else if !hit {
		t.Fatal("legacy request-level SOCKS BrowserContext did not reproduce the WebRTC UDP bypass")
	}
	_ = legacyProbe.Close()

	boundedProbe, boundedPort := listenWebRTCUDPProbe(t)
	boundedResult, err := sc.DoScrapeRodBounded(context.Background(), &models.ScrapeRequest{
		URL:      fmt.Sprintf("%s?stun_port=%d", pageServer.URL, boundedPort),
		Timeout:  5,
		ProxyURL: proxyURL,
		Actions:  []models.Action{{Type: "wait", Milliseconds: 1200}},
	}, 1<<20)
	if err != nil {
		t.Fatalf("bounded WebRTC probe scrape error = %v", err)
	}
	if !strings.Contains(boundedResult.RawHTML, `id="rtc-state">rtc-constructor-blocked</div>`) {
		t.Fatalf("bounded WebRTC constructor was not blocked: %s", boundedResult.RawHTML)
	}
	noPacketStarted := time.Now()
	if hit, readErr := readWebRTCUDPProbe(boundedProbe, 1100*time.Millisecond); readErr != nil {
		t.Fatalf("read bounded WebRTC UDP probe: %v", readErr)
	} else if hit {
		t.Fatal("bounded request leaked WebRTC/ICE UDP outside its request-level SOCKS proxy")
	}
	if elapsed := time.Since(noPacketStarted); elapsed < time.Second {
		t.Fatalf("bounded UDP no-packet observation lasted %v, want at least 1s", elapsed)
	}
	_ = boundedProbe.Close()
	if relayDials.Load() < 2 {
		t.Fatalf("request BrowserContexts did not use their SOCKS relay: dials=%d", relayDials.Load())
	}
}

func TestBrowserSlotIsolatesPooledProxyAndCDPPathsWithCancellation(t *testing.T) {
	browserBin := boundedBrowserTestBinary(t)
	started := make(chan struct{})
	releaseFirst := make(chan struct{})
	site := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		if request.URL.Path == "/hold" {
			close(started)
			<-releaseFirst
		}
		_, _ = writer.Write([]byte("<html><head></head><body>slot-ok</body></html>"))
	}))
	t.Cleanup(site.Close)

	var proxyHits atomic.Int32
	proxyServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		proxyHits.Add(1)
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = writer.Write([]byte("<html><head></head><body>proxy-ok</body></html>"))
	}))
	t.Cleanup(proxyServer.Close)

	sc := newBoundedBrowserTestScraper(t, browserBin, 1)
	firstDone := make(chan error, 1)
	go func() {
		_, err := sc.DoScrapeRodBounded(context.Background(), &models.ScrapeRequest{
			URL:     site.URL + "/hold",
			Timeout: 5,
		}, 1<<20)
		firstDone <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("pooled browser request did not start")
	}

	proxyCtx, proxyCancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer proxyCancel()
	_, err := sc.DoScrapeRodBounded(proxyCtx, &models.ScrapeRequest{
		URL:      "http://purify-slot-proxy.invalid/page",
		Timeout:  5,
		ProxyURL: proxyServer.URL,
	}, 1<<20)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("isolated proxy waiter error = %v", err)
	}
	if hits := proxyHits.Load(); hits != 0 {
		t.Fatalf("isolated proxy path bypassed global browser slot: hits=%d", hits)
	}

	cdpCtx, cdpCancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cdpCancel()
	_, err = sc.DoScrapeRodBounded(cdpCtx, &models.ScrapeRequest{
		URL:     site.URL + "/cdp-waiter",
		Timeout: 5,
		CDPURL:  sc.controlURL,
	}, 1<<20)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CDP waiter error = %v", err)
	}

	close(releaseFirst)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("pooled request error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pooled request did not release its browser slot")
	}

	proxyResult, err := sc.DoScrapeRodBounded(context.Background(), &models.ScrapeRequest{
		URL:      "http://purify-slot-proxy.invalid/page",
		Timeout:  5,
		ProxyURL: proxyServer.URL,
	}, 1<<20)
	if err != nil || !strings.Contains(proxyResult.RawHTML, "proxy-ok") {
		t.Fatalf("isolated proxy request after release = (%#v, %v)", proxyResult, err)
	}
	cdpResult, err := sc.DoScrapeRodBounded(context.Background(), &models.ScrapeRequest{
		URL:     site.URL + "/cdp",
		Timeout: 5,
		CDPURL:  sc.controlURL,
	}, 1<<20)
	if err != nil || !strings.Contains(cdpResult.RawHTML, "slot-ok") {
		t.Fatalf("CDP request after release = (%#v, %v)", cdpResult, err)
	}
	if active := sc.Stats().ActivePages; active != 0 {
		t.Fatalf("active browser slots = %d, want 0", active)
	}
}

func TestIsolatedTimeoutKeepsSlotUntilBrowserContextIsDisposed(t *testing.T) {
	browserBin := boundedBrowserTestBinary(t)
	var firstOnce sync.Once
	var secondOnce sync.Once
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	releaseSecond := make(chan struct{})
	proxyServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Hostname() {
		case "purify-cleanup-first.invalid":
			firstOnce.Do(func() { close(firstStarted) })
			<-request.Context().Done()
		case "purify-cleanup-second.invalid":
			secondOnce.Do(func() { close(secondStarted) })
			<-releaseSecond
			writer.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = writer.Write([]byte("<html><head></head><body>second-context</body></html>"))
		default:
			writer.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(proxyServer.Close)

	sc := newBoundedBrowserTestScraper(t, browserBin, 1)
	baseline := browserContextCount(t, sc.browser)
	firstCtx, firstCancel := context.WithTimeout(context.Background(), time.Second)
	defer firstCancel()
	_, err := sc.DoScrapeRodBounded(firstCtx, &models.ScrapeRequest{
		URL:      "http://purify-cleanup-first.invalid/page",
		Timeout:  5,
		ProxyURL: proxyServer.URL,
	}, 1<<20)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first isolated timeout error = %v", err)
	}
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first isolated request never reached its proxy")
	}

	var maximumContexts atomic.Int32
	maximumContexts.Store(int32(browserContextCount(t, sc.browser)))
	stopMonitor := make(chan struct{})
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopMonitor:
				return
			case <-ticker.C:
				contexts, contextsErr := (proto.TargetGetBrowserContexts{}).Call(sc.browser)
				if contextsErr != nil {
					continue
				}
				count := int32(len(contexts.BrowserContextIDs))
				for current := maximumContexts.Load(); count > current && !maximumContexts.CompareAndSwap(current, count); current = maximumContexts.Load() {
				}
			}
		}
	}()

	secondDone := make(chan error, 1)
	go func() {
		_, secondErr := sc.DoScrapeRodBounded(context.Background(), &models.ScrapeRequest{
			URL:      "http://purify-cleanup-second.invalid/page",
			Timeout:  5,
			ProxyURL: proxyServer.URL,
		}, 1<<20)
		secondDone <- secondErr
	}()
	select {
	case <-secondStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("N+1 isolated request did not start after cleanup")
	}
	if contexts := browserContextCount(t, sc.browser); contexts != baseline+1 {
		t.Fatalf("contexts while N+1 request active = %d, want %d", contexts, baseline+1)
	}
	close(releaseSecond)
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("N+1 isolated request error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("N+1 isolated request did not finish")
	}
	close(stopMonitor)
	<-monitorDone
	if got := int(maximumContexts.Load()); got > baseline+1 {
		t.Fatalf("overlapping isolated BrowserContexts = %d, maximum allowed %d", got, baseline+1)
	}
}

func TestScraperCloseWaitsForTransferredRealBrowserContextCleanup(t *testing.T) {
	browserBin := boundedBrowserTestBinary(t)
	sc := newBoundedBrowserTestScraperWithoutCleanup(t, browserBin, 1)
	release, err := sc.browserSlots.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, cleanup, err := newIsolatedBrowserContext(context.Background(), sc.browser, "")
	if err != nil {
		release()
		sc.Close()
		t.Fatal(err)
	}
	transferredCleanup := make(chan struct{})
	if !releaseBrowserSlotAfter(transferredCleanup, release) {
		t.Fatal("real BrowserContext cleanup did not take ownership of slot")
	}

	closeDone := make(chan struct{})
	go func() {
		sc.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
		t.Fatal("Scraper.Close returned while real BrowserContext cleanup was pending")
	case <-time.After(50 * time.Millisecond):
	}
	cleanupDone := cleanup()
	select {
	case <-cleanupDone:
		close(transferredCleanup)
	case <-time.After(3 * time.Second):
		t.Fatal("real BrowserContext cleanup did not complete")
	}
	select {
	case <-closeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Scraper.Close did not resume after real BrowserContext disposal")
	}
}

func TestBoundedBrowserPreservesBlockedResourceAndAdFiltering(t *testing.T) {
	browserBin := boundedBrowserTestBinary(t)
	var imageHits atomic.Int32
	var adHits atomic.Int32
	proxyServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Hostname() {
		case "purify-block.invalid":
			writer.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = writer.Write([]byte(`<html><head></head><body>block-options<img src="http://assets.invalid/image.png"><script src="http://doubleclick.net/ad.js"></script></body></html>`))
		case "assets.invalid":
			imageHits.Add(1)
			writer.Header().Set("Content-Type", "image/png")
			_, _ = writer.Write([]byte("not-an-image"))
		case "doubleclick.net":
			adHits.Add(1)
			writer.Header().Set("Content-Type", "application/javascript")
			_, _ = writer.Write([]byte("globalThis.adLoaded = true"))
		default:
			writer.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(proxyServer.Close)

	sc, err := NewScraper(config.BrowserConfig{
		Headless:   true,
		MaxPages:   1,
		BrowserBin: browserBin,
		NoSandbox:  os.Getenv("CI") == "true",
	}, config.ScraperConfig{
		DefaultTimeout:       5 * time.Second,
		MaxTimeout:           5 * time.Second,
		BlockedResourceTypes: []string{"Image"},
	})
	if err != nil {
		t.Fatalf("NewScraper() error = %v", err)
	}
	t.Cleanup(sc.Close)
	result, err := sc.DoScrapeRodBounded(context.Background(), &models.ScrapeRequest{
		URL:      "http://purify-block.invalid/page",
		Timeout:  5,
		ProxyURL: proxyServer.URL,
		BlockAds: true,
	}, 1<<20)
	if err != nil || !strings.Contains(result.RawHTML, "block-options") {
		t.Fatalf("bounded block-options scrape = (%#v, %v)", result, err)
	}
	if hits := imageHits.Load(); hits != 0 {
		t.Fatalf("blocked image reached proxy %d times", hits)
	}
	if hits := adHits.Load(); hits != 0 {
		t.Fatalf("blocked ad reached proxy %d times", hits)
	}
}

func TestScraperCloseWaitsForActiveBrowserAndRejectsNewWork(t *testing.T) {
	browserBin := boundedBrowserTestBinary(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		startedOnce.Do(func() { close(started) })
		<-release
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = writer.Write([]byte("<html><head></head><body>close-ok</body></html>"))
	}))
	t.Cleanup(server.Close)

	sc := newBoundedBrowserTestScraperWithoutCleanup(t, browserBin, 1)
	requestDone := make(chan error, 1)
	go func() {
		_, err := sc.DoScrapeRodBounded(context.Background(), &models.ScrapeRequest{URL: server.URL, Timeout: 5}, 1<<20)
		requestDone <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("active browser request did not start")
	}

	closeDone := make(chan struct{})
	go func() {
		sc.Close()
		close(closeDone)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		sc.browserSlots.mu.Lock()
		closing := sc.browserSlots.closing
		sc.browserSlots.mu.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Close did not close the browser slot gate")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-closeDone:
		t.Fatal("Close returned while browser work was active")
	case <-time.After(50 * time.Millisecond):
	}
	_, err := sc.DoScrapeRodBounded(context.Background(), &models.ScrapeRequest{URL: server.URL, Timeout: 5}, 1<<20)
	if err == nil || !strings.Contains(err.Error(), "browser is unavailable") {
		t.Fatalf("new work during Close error = %v", err)
	}

	close(release)
	select {
	case err := <-requestDone:
		if err != nil {
			t.Fatalf("active request failed during graceful Close: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("active request did not finish")
	}
	select {
	case <-closeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return after active request finished")
	}
	sc.Close() // idempotent
}

func boundedBrowserTestBinary(t *testing.T) string {
	t.Helper()
	if os.Getenv("PURIFY_BROWSER_TEST") != "1" {
		t.Skip("set PURIFY_BROWSER_TEST=1 with PURIFY_BROWSER_BIN for the fixed Chromium regression")
	}
	browserBin := os.Getenv("PURIFY_BROWSER_BIN")
	if browserBin == "" {
		t.Fatal("PURIFY_BROWSER_BIN is required for deterministic browser tests")
	}
	return browserBin
}

func newBoundedBrowserTestScraper(t *testing.T, browserBin string, maxPages int) *Scraper {
	t.Helper()
	sc := newBoundedBrowserTestScraperWithoutCleanup(t, browserBin, maxPages)
	t.Cleanup(sc.Close)
	return sc
}

func newBoundedBrowserTestScraperWithoutCleanup(t *testing.T, browserBin string, maxPages int) *Scraper {
	t.Helper()
	sc, err := NewScraper(config.BrowserConfig{
		Headless:   true,
		MaxPages:   maxPages,
		BrowserBin: browserBin,
		NoSandbox:  os.Getenv("CI") == "true",
	}, config.ScraperConfig{
		DefaultTimeout: 5 * time.Second,
		MaxTimeout:     5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewScraper() error = %v", err)
	}
	return sc
}

func browserContextCount(t *testing.T, browser *rod.Browser) int {
	t.Helper()
	contexts, err := (proto.TargetGetBrowserContexts{}).Call(browser)
	if err != nil {
		t.Fatalf("TargetGetBrowserContexts() error = %v", err)
	}
	return len(contexts.BrowserContextIDs)
}

func TestBoundedResponseHelpers(t *testing.T) {
	headers := []*proto.FetchHeaderEntry{
		{Name: "Content-Type", Value: "text/html"},
		{Name: "Content-Encoding", Value: "gzip"},
		{Name: "Transfer-Encoding", Value: "chunked"},
		{Name: "Content-Length", Value: "999"},
	}
	bounded := boundedResponseHeaders(headers, 17)
	if len(bounded) != 2 || bounded[0].Name != "Content-Type" || bounded[1].Name != "Content-Length" || bounded[1].Value != "17" {
		t.Fatalf("bounded headers = %#v", bounded)
	}
	if _, known := decodedContentLength(headers); known {
		t.Fatal("encoded Content-Length was treated as decoded body length")
	}
	identityHeaders := []*proto.FetchHeaderEntry{{Name: "Content-Length", Value: "999"}}
	if length, known := decodedContentLength(identityHeaders); !known || length != 999 {
		t.Fatalf("decodedContentLength() = (%d, %v)", length, known)
	}
	if totalBrowserResourceLimit(1<<20) != 4<<20 || totalBrowserResourceLimit(32<<20) != 64<<20 {
		t.Fatal("total browser resource limit does not apply 4x and 64MiB cap")
	}
}

func gzipFixture(t *testing.T, body string) []byte {
	t.Helper()
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}

func scriptFixture(size int) []byte {
	if size < 4 {
		panic("script fixture is too small")
	}
	return []byte("/*" + strings.Repeat("x", size-4) + "*/")
}

func listenWebRTCUDPProbe(t *testing.T) (*net.UDPConn, int) {
	t.Helper()
	listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen for WebRTC UDP probe: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener, listener.LocalAddr().(*net.UDPAddr).Port
}

func readWebRTCUDPProbe(listener *net.UDPConn, timeout time.Duration) (bool, error) {
	if err := listener.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return false, err
	}
	buffer := make([]byte, 2048)
	_, _, err := listener.ReadFromUDP(buffer)
	if err == nil {
		return true, nil
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) && networkErr.Timeout() {
		return false, nil
	}
	return false, err
}
