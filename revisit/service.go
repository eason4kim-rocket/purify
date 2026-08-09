// Package revisit implements the production page-observation adapter used by
// verify. It owns deterministic engine escalation and persists only the one
// candidate selected by its fail-closed status and raw-document policy.
package revisit

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/use-agent/purify/engine"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/publicnet"
	"github.com/use-agent/purify/quality"
	"github.com/use-agent/purify/scraper"
	"github.com/use-agent/purify/verify"
)

const (
	maximumRedirects             = 5
	hardMaximumTimeout           = 120 * time.Second
	hardMaximumResponseBodyBytes = 64 << 20
)

// ErrInvalidConfig reports a construction-time configuration error.
var ErrInvalidConfig = errors.New("revisit: invalid configuration")

// Finalizer persists the one engine result selected by Service.
type Finalizer interface {
	FinalizeSelected(*models.ScrapeRequest, *scraper.ScrapeResult) (*scraper.ScrapeResult, error)
}

// Config contains the complete production dependencies and work bounds.
type Config struct {
	Engines          []engine.Engine
	Finalizer        Finalizer
	Policy           *publicnet.Policy
	SafeProxyURL     string
	Timeout          time.Duration
	MaximumBodyBytes int64
}

// Service obtains one durable, public-only page observation.
type Service struct {
	engines          []engine.Engine
	finalizer        Finalizer
	policy           *publicnet.Policy
	safeProxyURL     string
	timeout          time.Duration
	maximumBodyBytes int64
}

var _ verify.Revisitor = (*Service)(nil)

// New validates configuration and fixes engine order to HTTP, Rod, then
// Rod-stealth regardless of caller slice order.
func New(config Config) (*Service, error) {
	if isNil(config.Finalizer) {
		return nil, fmt.Errorf("%w: finalizer is required", ErrInvalidConfig)
	}
	if config.Policy == nil {
		return nil, fmt.Errorf("%w: public network policy is required", ErrInvalidConfig)
	}
	if config.Timeout <= 0 || config.Timeout > hardMaximumTimeout {
		return nil, fmt.Errorf("%w: timeout must be between 1ns and %s", ErrInvalidConfig, hardMaximumTimeout)
	}
	if config.MaximumBodyBytes <= 0 || config.MaximumBodyBytes > hardMaximumResponseBodyBytes {
		return nil, fmt.Errorf(
			"%w: maximum body bytes must be between 1 and %d",
			ErrInvalidConfig,
			hardMaximumResponseBodyBytes,
		)
	}
	safeProxyURL, err := normalizeSafeProxyURL(config.SafeProxyURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	engines, err := orderedEngines(config.Engines)
	if err != nil {
		return nil, err
	}
	return &Service{
		engines:          engines,
		finalizer:        config.Finalizer,
		policy:           config.Policy,
		safeProxyURL:     safeProxyURL,
		timeout:          config.Timeout,
		maximumBodyBytes: config.MaximumBodyBytes,
	}, nil
}

// Revisit fetches, classifies, and durably finalizes exactly one observation.
// Invalid/private initial targets are rejected before any engine is invoked.
func (service *Service) Revisit(ctx context.Context, rawURL string) (verify.RevisitResult, error) {
	if service == nil || len(service.engines) == 0 || isNil(service.finalizer) || service.policy == nil {
		return verify.RevisitResult{}, fmt.Errorf("%w: service is not configured", verify.ErrRevisit)
	}
	if ctx == nil {
		return verify.RevisitResult{}, fmt.Errorf("%w: context is nil", verify.ErrInvalidRequest)
	}
	if err := ctx.Err(); err != nil {
		return verify.RevisitResult{}, fmt.Errorf("%w: %w", verify.ErrRevisit, err)
	}

	requestCtx, cancel := context.WithTimeout(ctx, service.timeout)
	defer cancel()
	canonicalURL, parsedURL, err := publicnet.NormalizeHTTPURL(rawURL, nil, false)
	if err != nil {
		return verify.RevisitResult{}, fmt.Errorf("%w: invalid revisit URL: %v", verify.ErrInvalidRequest, err)
	}
	if err := service.resolvePublic(requestCtx, parsedURL.Hostname()); err != nil {
		if errors.Is(err, publicnet.ErrNotPublic) {
			return verify.RevisitResult{}, fmt.Errorf("%w: target is not public", verify.ErrInvalidRequest)
		}
		return verify.RevisitResult{}, fmt.Errorf("%w: resolve initial target: %w", verify.ErrRevisit, err)
	}

	var lastErr error
	var protectedStatus *verify.HTTPStatusError
	for _, backend := range service.engines {
		if err := requestCtx.Err(); err != nil {
			lastErr = err
			break
		}
		name := backend.Name()
		request := service.fetchRequest(canonicalURL, name)
		if !backend.Supports(request) {
			lastErr = fmt.Errorf("engine %q does not support its revisit request", name)
			continue
		}

		fetched, fetchErr := backend.Fetch(requestCtx, request)
		if fetchErr != nil {
			lastErr = fetchErr
			if requestCtx.Err() != nil {
				break
			}
			if errors.Is(fetchErr, engine.ErrResponseBodyTooLarge) {
				return verify.RevisitResult{}, fmt.Errorf("%w: %w", verify.ErrRevisit, fetchErr)
			}
			continue
		}
		if fetched == nil {
			lastErr = fmt.Errorf("engine %q returned no observation", name)
			continue
		}
		if int64(len(fetched.HTML)) > service.maximumBodyBytes {
			lastErr = fmt.Errorf(
				"%w: engine %q returned more than %d bytes",
				engine.ErrResponseBodyTooLarge,
				name,
				service.maximumBodyBytes,
			)
			return verify.RevisitResult{}, fmt.Errorf("%w: %w", verify.ErrRevisit, lastErr)
		}

		candidate := *fetched
		if strings.TrimSpace(candidate.FinalURL) == "" {
			candidate.FinalURL = canonicalURL
		}
		candidate.FinalURL, _, err = service.normalizeResolvedURL(requestCtx, candidate.FinalURL)
		if err != nil {
			lastErr = fmt.Errorf("engine %q returned an unsafe final URL: %w", name, err)
			if requestCtx.Err() != nil {
				break
			}
			continue
		}

		switch candidate.StatusCode {
		case http.StatusNotFound, http.StatusGone:
			return service.finalize(requestCtx, canonicalURL, name, &candidate)
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
			protectedStatus = &verify.HTTPStatusError{StatusCode: candidate.StatusCode}
			continue
		}
		if candidate.StatusCode < 200 || candidate.StatusCode >= 300 {
			lastErr = fmt.Errorf("engine %q returned unusable HTTP status %d", name, candidate.StatusCode)
			continue
		}
		if !isHTMLContentType(candidate.ContentType) {
			lastErr = fmt.Errorf("engine %q returned a non-HTML success response", name)
			continue
		}
		if reason := quality.EvaluateRawDocument(candidate.HTML); reason != "" {
			lastErr = fmt.Errorf("engine %q returned unusable raw document: %s", name, reason)
			continue
		}
		return service.finalize(requestCtx, canonicalURL, name, &candidate)
	}

	if err := requestCtx.Err(); err != nil {
		return verify.RevisitResult{}, fmt.Errorf("%w: %w", verify.ErrRevisit, err)
	}
	if protectedStatus != nil {
		return verify.RevisitResult{}, protectedStatus
	}
	if lastErr == nil {
		lastErr = errors.New("no configured engine produced an observation")
	}
	return verify.RevisitResult{}, fmt.Errorf("%w: no usable observation: %w", verify.ErrRevisit, lastErr)
}

func (service *Service) fetchRequest(targetURL, engineName string) *engine.FetchRequest {
	request := &engine.FetchRequest{
		URL:              targetURL,
		Timeout:          service.timeout,
		ProxyURL:         service.safeProxyURL,
		Mode:             engine.FetchModeObservation,
		MaximumBodyBytes: service.maximumBodyBytes,
	}
	switch engineName {
	case "http":
		request.CheckRedirect = service.redirectPolicy()
	case "rod", "rod-stealth":
		waitForNetworkIdle := true
		request.WaitForNetworkIdle = &waitForNetworkIdle
		request.Stealth = engineName == "rod-stealth"
	}
	return request
}

func (service *Service) redirectPolicy() func(*http.Request, []*http.Request) error {
	return func(request *http.Request, via []*http.Request) error {
		if request == nil || request.URL == nil {
			return errors.New("revisit: redirect target is missing")
		}
		if len(via) > maximumRedirects {
			return fmt.Errorf("revisit: redirect limit %d exceeded", maximumRedirects)
		}
		canonical, _, err := service.normalizeResolvedURL(request.Context(), request.URL.String())
		if err != nil {
			return fmt.Errorf("revisit: reject redirect target: %w", err)
		}
		for _, previous := range via {
			if previous == nil || previous.URL == nil {
				return errors.New("revisit: redirect history is invalid")
			}
			previousCanonical, _, normalizeErr := publicnet.NormalizeHTTPURL(previous.URL.String(), nil, false)
			if normalizeErr != nil {
				return fmt.Errorf("revisit: redirect history is invalid: %w", normalizeErr)
			}
			if previousCanonical == canonical {
				return errors.New("revisit: redirect cycle detected")
			}
		}
		return nil
	}
}

func (service *Service) normalizeResolvedURL(ctx context.Context, rawURL string) (string, *url.URL, error) {
	canonical, parsed, err := publicnet.NormalizeHTTPURL(rawURL, nil, false)
	if err != nil {
		return "", nil, err
	}
	if err := service.resolvePublic(ctx, parsed.Hostname()); err != nil {
		return "", nil, err
	}
	return canonical, parsed, nil
}

func (service *Service) resolvePublic(ctx context.Context, hostname string) error {
	addresses, err := service.policy.Resolve(ctx, hostname)
	if err != nil {
		return err
	}
	for _, address := range addresses {
		if !publicnet.IsPublicAddress(address) {
			return fmt.Errorf("%w: resolved address is private, local, or reserved", publicnet.ErrNotPublic)
		}
	}
	return nil
}

func (service *Service) finalize(ctx context.Context, requestURL, engineName string, selected *engine.FetchResult) (verify.RevisitResult, error) {
	if err := ctx.Err(); err != nil {
		return verify.RevisitResult{}, fmt.Errorf("%w: %w", verify.ErrRevisit, err)
	}
	expectedFinalURL := selected.FinalURL
	expectedHTML := selected.HTML
	expectedStatus := selected.StatusCode
	source := &scraper.ScrapeResult{
		RawHTML:     expectedHTML,
		Title:       selected.Title,
		StatusCode:  expectedStatus,
		FinalURL:    expectedFinalURL,
		EngineUsed:  engineName,
		FetchMethod: fetchMethod(engineName),
		ContentType: selected.ContentType,
	}
	request := &models.ScrapeRequest{
		URL:         requestURL,
		Timeout:     durationSecondsCeil(service.timeout),
		ProxyURL:    service.safeProxyURL,
		Stealth:     engineName == "rod-stealth",
		MaxAge:      0,
		CDPURL:      "",
		Actions:     nil,
		Headers:     nil,
		Cookies:     nil,
		BlockAds:    false,
		ExtractMode: "",
	}
	finalized, err := service.finalizer.FinalizeSelected(request, source)
	if err != nil {
		return verify.RevisitResult{}, fmt.Errorf("%w: finalize selected observation: %w", verify.ErrRevisit, err)
	}
	if finalized == nil {
		return verify.RevisitResult{}, fmt.Errorf("%w: finalizer returned no observation", verify.ErrRevisit)
	}
	if strings.TrimSpace(string(finalized.SnapshotID)) == "" {
		return verify.RevisitResult{}, fmt.Errorf("%w: finalized snapshot ID is required", verify.ErrRevisit)
	}
	if finalized.FetchedAt.IsZero() {
		return verify.RevisitResult{}, fmt.Errorf("%w: finalized fetch time is required", verify.ErrRevisit)
	}
	if finalized.StatusCode != expectedStatus {
		return verify.RevisitResult{}, fmt.Errorf("%w: finalizer changed HTTP status", verify.ErrRevisit)
	}
	if finalized.RawHTML != expectedHTML {
		return verify.RevisitResult{}, fmt.Errorf("%w: finalizer changed raw HTML", verify.ErrRevisit)
	}
	finalCanonical, _, err := publicnet.NormalizeHTTPURL(finalized.FinalURL, nil, false)
	if err != nil || finalCanonical != expectedFinalURL {
		return verify.RevisitResult{}, fmt.Errorf("%w: finalizer changed final URL", verify.ErrRevisit)
	}
	return verify.RevisitResult{
		StatusCode: finalized.StatusCode,
		FinalURL:   finalCanonical,
		RawHTML:    finalized.RawHTML,
		SnapshotID: strings.TrimSpace(string(finalized.SnapshotID)),
		FetchedAt:  finalized.FetchedAt.UTC(),
	}, nil
}

func orderedEngines(configured []engine.Engine) ([]engine.Engine, error) {
	if len(configured) == 0 {
		return nil, fmt.Errorf("%w: at least one engine is required", ErrInvalidConfig)
	}
	ordered := make([]engine.Engine, 3)
	for _, backend := range configured {
		if isNil(backend) {
			return nil, fmt.Errorf("%w: engine is nil", ErrInvalidConfig)
		}
		var index int
		switch backend.Name() {
		case "http":
			index = 0
		case "rod":
			index = 1
		case "rod-stealth":
			index = 2
		default:
			return nil, fmt.Errorf("%w: unsupported engine %q", ErrInvalidConfig, backend.Name())
		}
		if ordered[index] != nil {
			return nil, fmt.Errorf("%w: duplicate engine %q", ErrInvalidConfig, backend.Name())
		}
		ordered[index] = backend
	}
	compacted := make([]engine.Engine, 0, len(configured))
	for _, backend := range ordered {
		if backend != nil {
			compacted = append(compacted, backend)
		}
	}
	return compacted, nil
}

func normalizeSafeProxyURL(rawURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", fmt.Errorf("safe proxy URL is invalid: %w", err)
	}
	if strings.ToLower(parsed.Scheme) != "socks5" || parsed.Opaque != "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" {
		return "", errors.New("safe proxy URL must be an unauthenticated socks5 URL without a path, query, or fragment")
	}
	hostname := parsed.Hostname()
	address, err := netip.ParseAddr(hostname)
	if err != nil || address.Zone() != "" || !address.Unmap().IsLoopback() {
		return "", errors.New("safe proxy URL host must be a loopback IP address")
	}
	rawPort := parsed.Port()
	if rawPort == "" || strings.Trim(rawPort, "0123456789") != "" {
		return "", errors.New("safe proxy URL port must be between 1 and 65535")
	}
	port, err := strconv.ParseUint(rawPort, 10, 16)
	if err != nil || port == 0 {
		return "", errors.New("safe proxy URL port must be between 1 and 65535")
	}
	parsed.Scheme = "socks5"
	parsed.Host = net.JoinHostPort(address.Unmap().String(), strconv.FormatUint(port, 10))
	return parsed.String(), nil
}

func isHTMLContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	switch strings.ToLower(mediaType) {
	case "text/html", "application/xhtml+xml":
		return true
	default:
		return false
	}
}

func fetchMethod(engineName string) string {
	if engineName == "http" {
		return "http"
	}
	return "browser"
}

func durationSecondsCeil(duration time.Duration) int {
	return int((duration + time.Second - 1) / time.Second)
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
