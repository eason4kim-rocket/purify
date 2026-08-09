package scraper

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
	"github.com/use-agent/purify/engine"
)

const browserInterceptionCleanupTimeout = 250 * time.Millisecond

const hardMaximumBrowserResourceBytes int64 = 64 << 20

// pageInterception owns either the legacy request-blocking router or the
// bounded Fetch-domain response stream. The latter is used only when a caller
// supplies MaximumBodyBytes, preserving ordinary scrape behavior.
type pageInterception struct {
	stopOnce sync.Once
	stopFunc func()
	errMu    sync.RWMutex
	lastErr  error
	budget   *decodedResourceBudget
}

type decodedResourceBudget struct {
	mu      sync.Mutex
	maximum int64
	used    int64
}

func newDecodedResourceBudget(maximum int64) *decodedResourceBudget {
	return &decodedResourceBudget{maximum: maximum}
}

func (budget *decodedResourceBudget) remaining() int64 {
	budget.mu.Lock()
	defer budget.mu.Unlock()
	return budget.maximum - budget.used
}

func (budget *decodedResourceBudget) consume(bytes int64) bool {
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if bytes < 0 || bytes > budget.maximum-budget.used {
		return false
	}
	budget.used += bytes
	return true
}

func (interception *pageInterception) stop() {
	if interception == nil {
		return
	}
	interception.stopOnce.Do(func() {
		if interception.stopFunc != nil {
			interception.stopFunc()
		}
	})
}

func (interception *pageInterception) err() error {
	if interception == nil {
		return nil
	}
	interception.errMu.RLock()
	defer interception.errMu.RUnlock()
	return interception.lastErr
}

func (interception *pageInterception) setErr(err error) {
	if interception == nil || err == nil {
		return
	}
	interception.errMu.Lock()
	if interception.lastErr == nil {
		interception.lastErr = err
	}
	interception.errMu.Unlock()
}

func setupPageInterception(page *rod.Page, blockedTypes []string, blockAds bool, maximumBodyBytes int64) (*pageInterception, error) {
	if maximumBodyBytes <= 0 {
		router := setupHijack(page, blockedTypes, blockAds)
		interception := &pageInterception{}
		if router != nil {
			interception.stopFunc = func() { _ = router.Stop() }
		}
		return interception, nil
	}
	return setupBoundedPageInterception(page, blockedTypes, blockAds, maximumBodyBytes)
}

func setupBoundedPageInterception(page *rod.Page, blockedTypes []string, blockAds bool, maximumBodyBytes int64) (*pageInterception, error) {
	blocked := make(map[proto.NetworkResourceType]struct{}, len(blockedTypes))
	for _, name := range blockedTypes {
		if resourceType, ok := configToProto[name]; ok {
			blocked[resourceType] = struct{}{}
		}

	}

	// The request-stage pattern preserves configured blocking and rejects
	// WebSockets. The response-stage pattern streams every HTTP(S) resource
	// through one decoded-byte budget before Chromium receives it.
	patterns := []*proto.FetchRequestPattern{
		{URLPattern: "*", RequestStage: proto.FetchRequestStageRequest},
		{URLPattern: "*", RequestStage: proto.FetchRequestStageResponse},
	}

	eventPage, cancelEvents := page.WithCancel()
	messages := eventPage.Event()
	interception := &pageInterception{budget: newDecodedResourceBudget(totalBrowserResourceLimit(maximumBodyBytes))}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for message := range messages {
			if message.Method != (proto.FetchRequestPaused{}).ProtoEvent() {
				continue
			}
			var event proto.FetchRequestPaused
			message.Load(&event)
			handlePausedRequest(eventPage, page.FrameID, &event, blocked, blockAds, maximumBodyBytes, interception)
		}
	}()

	blockedWebSocket, err := (proto.PageAddScriptToEvaluateOnNewDocument{Source: blockUnscopedNetworkScript}).Call(eventPage)
	if err != nil {
		cancelEvents()
		<-done
		return nil, err
	}
	if err := (proto.FetchEnable{Patterns: patterns}).Call(eventPage); err != nil {
		cancelEvents()
		<-done
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), browserInterceptionCleanupTimeout)
		defer cleanupCancel()
		_ = (proto.PageRemoveScriptToEvaluateOnNewDocument{Identifier: blockedWebSocket.Identifier}).Call(page.Context(cleanupCtx))
		return nil, err
	}
	interception.stopFunc = func() {
		cancelEvents()
		<-done
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), browserInterceptionCleanupTimeout)
		defer cleanupCancel()
		cleanupPage := page.Context(cleanupCtx)
		_ = (proto.FetchDisable{}).Call(cleanupPage)
		_ = (proto.PageRemoveScriptToEvaluateOnNewDocument{Identifier: blockedWebSocket.Identifier}).Call(cleanupPage)
	}
	return interception, nil
}

func handlePausedRequest(
	page *rod.Page,
	mainFrameID proto.PageFrameID,
	event *proto.FetchRequestPaused,
	blocked map[proto.NetworkResourceType]struct{},
	blockAds bool,
	maximumBodyBytes int64,
	interception *pageInterception,
) {
	if event == nil || event.Request == nil {
		return
	}
	if interception.err() != nil {
		_ = (proto.FetchFailRequest{RequestID: event.RequestID, ErrorReason: proto.NetworkErrorReasonAborted}).Call(page)
		return
	}
	if event.ResponseStatusCode == nil && event.ResponseErrorReason == "" {
		isChildDocument := event.ResourceType == proto.NetworkResourceTypeDocument &&
			event.FrameID != "" && event.FrameID != mainFrameID
		if shouldBlockBrowserRequest(event, blocked, blockAds) ||
			isChildDocument ||
			event.ResourceType == proto.NetworkResourceTypeWebSocket ||
			isWebSocketURL(event.Request.URL) {
			_ = (proto.FetchFailRequest{
				RequestID:   event.RequestID,
				ErrorReason: proto.NetworkErrorReasonBlockedByClient,
			}).Call(page)
			return
		}
		_ = (proto.FetchContinueRequest{RequestID: event.RequestID}).Call(page)
		return
	}

	if !isHTTPURL(event.Request.URL) {
		_ = (proto.FetchContinueRequest{RequestID: event.RequestID}).Call(page)
		return
	}
	if event.ResponseStatusCode == nil || responseHasNoBody(*event.ResponseStatusCode) {
		_ = (proto.FetchContinueRequest{RequestID: event.RequestID}).Call(page)
		return
	}
	// Every decoded HTTP(S) response gets the caller's main-document limit;
	// the separate shared budget still caps the whole page session at 4x.
	perResourceLimit := maximumBodyBytes
	if length, known := decodedContentLength(event.ResponseHeaders); known &&
		(perResourceLimit > 0 && length > perResourceLimit || length > interception.budget.remaining()) {
		if perResourceLimit > 0 && length > perResourceLimit {
			interception.setErr(bodyTooLargeError(maximumBodyBytes))
		} else {
			interception.setErr(totalResourcesTooLargeError(interception.budget.maximum))
		}
		_ = (proto.FetchFailRequest{
			RequestID:   event.RequestID,
			ErrorReason: proto.NetworkErrorReasonAborted,
		}).Call(page)
		return
	}

	body, err := readPausedResponseBody(page, event.RequestID, perResourceLimit, interception.budget)
	if err != nil {
		interception.setErr(err)
		_ = (proto.FetchFailRequest{
			RequestID:   event.RequestID,
			ErrorReason: proto.NetworkErrorReasonAborted,
		}).Call(page)
		return
	}
	if err := (proto.FetchFulfillRequest{
		RequestID:      event.RequestID,
		ResponseCode:   *event.ResponseStatusCode,
		ResponsePhrase: event.ResponseStatusText,
		ResponseHeaders: boundedResponseHeaders(
			event.ResponseHeaders,
			len(body),
		),
		Body: body,
	}).Call(page); err != nil {
		interception.setErr(fmt.Errorf("fulfill bounded browser response: %w", err))
	}
}

func shouldBlockBrowserRequest(event *proto.FetchRequestPaused, blocked map[proto.NetworkResourceType]struct{}, blockAds bool) bool {
	if _, ok := blocked[event.ResourceType]; ok {
		return true
	}
	if !blockAds || event.Request == nil {
		return false
	}
	parsed, err := url.Parse(event.Request.URL)
	return err == nil && isAdDomain(parsed.Hostname())
}

func isHTTPURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	return err == nil && (strings.EqualFold(parsed.Scheme, "http") || strings.EqualFold(parsed.Scheme, "https"))
}

func isWebSocketURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	return err == nil && (strings.EqualFold(parsed.Scheme, "ws") || strings.EqualFold(parsed.Scheme, "wss"))
}

func responseHasNoBody(status int) bool {
	return status >= 100 && status < 200 ||
		status == 204 || status == 205 || status == 304 ||
		status == 301 || status == 302 || status == 303 || status == 307 || status == 308
}

func decodedContentLength(headers []*proto.FetchHeaderEntry) (int64, bool) {
	for _, header := range headers {
		if header != nil && strings.EqualFold(header.Name, "Content-Encoding") {
			encoding := strings.TrimSpace(header.Value)
			if encoding != "" && !strings.EqualFold(encoding, "identity") {
				// Fetch's response stream is decoded. The encoded Content-Length
				// cannot prove that the decoded observation is oversized.
				return 0, false
			}
		}
	}
	for _, header := range headers {
		if header == nil || !strings.EqualFold(header.Name, "Content-Length") {
			continue
		}
		length, err := strconv.ParseInt(strings.TrimSpace(header.Value), 10, 64)
		return length, err == nil && length >= 0
	}
	return 0, false
}

func readPausedResponseBody(page *rod.Page, requestID proto.FetchRequestID, maximumBodyBytes int64, budget *decodedResourceBudget) ([]byte, error) {
	stream, err := (proto.FetchTakeResponseBodyAsStream{RequestID: requestID}).Call(page)
	if err != nil {
		return nil, fmt.Errorf("%w: cannot open bounded browser response stream: %v", engine.ErrResponseBodyTooLarge, err)
	}
	defer func() { _ = (proto.IOClose{Handle: stream.Stream}).Call(page) }()

	var body bytes.Buffer
	grow := budget.remaining()
	if maximumBodyBytes > 0 && maximumBodyBytes < grow {
		grow = maximumBodyBytes
	}
	if grow < 1<<20 {
		body.Grow(int(grow))
	} else {
		body.Grow(1 << 20)
	}
	for {
		bodyRemaining := int64(hardMaximumBrowserResourceBytes)
		if maximumBodyBytes > 0 {
			bodyRemaining = maximumBodyBytes - int64(body.Len())
		}
		if bodyRemaining < 0 {
			return nil, bodyTooLargeError(maximumBodyBytes)
		}
		budgetRemaining := budget.remaining()
		remaining := bodyRemaining
		if budgetRemaining < remaining {
			remaining = budgetRemaining
		}
		readSize := int64(32 << 10)
		if remaining+1 < readSize {
			readSize = remaining + 1
		}
		size := int(readSize)
		chunk, err := (proto.IORead{Handle: stream.Stream, Size: &size}).Call(page)
		if err != nil {
			return nil, fmt.Errorf("%w: cannot read bounded browser response stream: %v", engine.ErrResponseBodyTooLarge, err)
		}
		data := []byte(chunk.Data)
		if chunk.Base64Encoded {
			decoded := make([]byte, base64.StdEncoding.DecodedLen(len(chunk.Data)))
			decodedLength, decodeErr := base64.StdEncoding.Decode(decoded, []byte(chunk.Data))
			if decodeErr != nil {
				return nil, fmt.Errorf("%w: cannot decode bounded browser response stream: %v", engine.ErrResponseBodyTooLarge, decodeErr)
			}
			data = decoded[:decodedLength]
		}
		if maximumBodyBytes > 0 && int64(body.Len()+len(data)) > maximumBodyBytes {
			return nil, bodyTooLargeError(maximumBodyBytes)
		}
		if !budget.consume(int64(len(data))) {
			return nil, totalResourcesTooLargeError(budget.maximum)
		}
		_, _ = body.Write(data)
		if chunk.EOF {
			return body.Bytes(), nil
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("%w: bounded browser response stream made no progress", engine.ErrResponseBodyTooLarge)
		}
	}
}

func boundedResponseHeaders(headers []*proto.FetchHeaderEntry, bodyLength int) []*proto.FetchHeaderEntry {
	bounded := make([]*proto.FetchHeaderEntry, 0, len(headers)+1)
	for _, header := range headers {
		if header == nil ||
			strings.EqualFold(header.Name, "Content-Length") ||
			strings.EqualFold(header.Name, "Content-Encoding") ||
			strings.EqualFold(header.Name, "Transfer-Encoding") {
			continue
		}
		copy := *header
		bounded = append(bounded, &copy)
	}
	bounded = append(bounded, &proto.FetchHeaderEntry{Name: "Content-Length", Value: strconv.Itoa(bodyLength)})
	return bounded
}

func bodyTooLargeError(maximumBodyBytes int64) error {
	return fmt.Errorf("%w: maximum is %d bytes", engine.ErrResponseBodyTooLarge, maximumBodyBytes)
}

func totalResourcesTooLargeError(maximumBodyBytes int64) error {
	return fmt.Errorf("%w: total browser resources exceed %d bytes", engine.ErrResponseBodyTooLarge, maximumBodyBytes)
}

func totalBrowserResourceLimit(maximumBodyBytes int64) int64 {
	if maximumBodyBytes >= hardMaximumBrowserResourceBytes/4 {
		return hardMaximumBrowserResourceBytes
	}
	return maximumBodyBytes * 4
}

const blockUnscopedNetworkScript = `(() => {
	class PurifyBlockedNetworkAPI {
		constructor() { throw new TypeError("unscoped network APIs are disabled for bounded observations"); }
	}
	// BrowserContext proxies cover Chromium's HTTP stack, but WebRTC/ICE and
	// direct-socket APIs can create transports outside it (notably UDP). Define
	// every known alias even when Chromium does not expose it so page scripts
	// cannot install a native-looking fallback later. The non-configurable,
	// non-writable properties are installed before any document script runs.
	for (const name of [
		"WebSocket",
		"WebSocketStream",
		"Worker",
		"SharedWorker",
		"WebTransport",
		"RTCPeerConnection",
		"webkitRTCPeerConnection",
		"mozRTCPeerConnection",
		"RTCIceGatherer",
		"RTCIceTransport",
		"RTCDtlsTransport",
		"RTCSctpTransport",
		"RTCQuicTransport",
		"UDPSocket",
		"TCPSocket",
		"DirectUDPSocket",
		"DirectTCPSocket",
	]) {
		Object.defineProperty(globalThis, name, {
			configurable: false,
			enumerable: false,
			writable: false,
			value: PurifyBlockedNetworkAPI,
		});
		const descriptor = Object.getOwnPropertyDescriptor(globalThis, name);
		if (!descriptor || descriptor.value !== PurifyBlockedNetworkAPI || descriptor.configurable || descriptor.writable) {
			throw new TypeError("failed to disable " + name + " for bounded observations");
		}
	}
	try {
		if (navigator.serviceWorker) {
			Object.defineProperty(navigator.serviceWorker, "register", {
				configurable: false,
				writable: false,
				value: () => Promise.reject(new TypeError("ServiceWorker is disabled for bounded observations")),
			});
		}
	} catch (_) {}
	try {
		Object.defineProperty(globalThis, "open", {
			configurable: false,
			writable: false,
			value: () => null,
		});
	} catch (_) {}
})();`

const boundedHTMLScript = `(maximumBytes) => {
	const html = document.documentElement ? document.documentElement.outerHTML : "";
	let bytes = 0;
	for (let index = 0; index < html.length; index++) {
		const code = html.charCodeAt(index);
		if (code <= 0x7f) {
			bytes += 1;
		} else if (code <= 0x7ff) {
			bytes += 2;
		} else if (code >= 0xd800 && code <= 0xdbff) {
			if (index + 1 < html.length) {
				const next = html.charCodeAt(index + 1);
				if (next >= 0xdc00 && next <= 0xdfff) {
					bytes += 4;
					index++;
				} else {
					bytes += 3;
				}
			} else {
				bytes += 3;
			}
		} else {
			bytes += 3;
		}
		if (bytes > maximumBytes) return { tooLarge: true, html: "" };
	}
	return { tooLarge: false, html };
}`

// extractBoundedHTML performs UTF-8 byte counting inside the renderer. An
// oversized DOM returns only a boolean over CDP; the large HTML string is never
// serialized into the CDP response or allocated in Go.
func extractBoundedHTML(page *rod.Page, maximumBodyBytes int64) (string, error) {
	if maximumBodyBytes <= 0 {
		return page.HTML()
	}
	result, err := page.Eval(boundedHTMLScript, maximumBodyBytes)
	if err != nil {
		return "", err
	}
	if result.Value.Get("tooLarge").Bool() {
		return "", bodyTooLargeError(maximumBodyBytes)
	}
	html := result.Value.Get("html").Str()
	if int64(len(html)) > maximumBodyBytes {
		return "", bodyTooLargeError(maximumBodyBytes)
	}
	return html, nil
}
