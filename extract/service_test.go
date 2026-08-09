package extract

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/use-agent/purify/evidence"
	"github.com/use-agent/purify/llm"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/receipts"
	"github.com/use-agent/purify/scrape"
	"github.com/use-agent/purify/scraper"
	"github.com/use-agent/purify/snapshot"
)

const testPublicArtifactProxyURL = "socks5://127.0.0.1:19080"

func TestServiceExtractUsesCanonicalRunnerAndRepairsOnce(t *testing.T) {
	runner := &recordingRunner{result: successfulScrapeResult()}
	client := &recordingExtractor{
		initial:  &llm.ExtractResult{Data: json.RawMessage(`{"count":"three"}`), Usage: &models.LLMUsage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3}},
		repaired: &llm.ExtractResult{Data: json.RawMessage(`{"count":3}`), Usage: &models.LLMUsage{PromptTokens: 4, CompletionTokens: 2, TotalTokens: 6}},
	}
	service := newTestService(t, runner, client, nil)
	waitForNetwork := false
	request := &models.ExtractRequest{
		URL:                "https://example.test/product",
		Schema:             json.RawMessage(`{"count":"number"}`),
		LLMAPIKey:          "secret",
		LLMModel:           "model-a",
		LLMBaseURL:         "https://llm.example/v1",
		CSSSelector:        "main",
		OutputFormat:       "text",
		ExtractMode:        "pruning",
		WaitForNetworkIdle: &waitForNetwork,
		Timeout:            17,
		Stealth:            true,
		ProxyURL:           "https://proxy.example:8443",
	}
	wantSchema := append(json.RawMessage(nil), request.Schema...)

	response, err := service.Extract(context.Background(), request)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if !response.Success || response.Partial || string(response.Data) != `{"count":3}` {
		t.Fatalf("response = %#v", response)
	}
	if response.LLMUsage == nil || *response.LLMUsage != (models.LLMUsage{PromptTokens: 6, CompletionTokens: 3, TotalTokens: 9}) {
		t.Fatalf("LLM usage = %#v", response.LLMUsage)
	}
	if client.extractCalls != 1 || client.repairCalls != 1 {
		t.Fatalf("extract/repair calls = %d/%d, want 1/1", client.extractCalls, client.repairCalls)
	}
	if client.content != runner.result.Response.Content || client.params != (llm.ExtractParams{APIKey: "secret", Model: "model-a", BaseURL: "https://llm.example/v1"}) {
		t.Fatalf("LLM call content=%q params=%#v", client.content, client.params)
	}
	if !strings.Contains(string(client.schema), `"properties":{"count"`) {
		t.Fatalf("legacy schema was not normalized: %s", client.schema)
	}
	if runner.calls != 1 || runner.request == nil {
		t.Fatalf("runner calls/request = %d/%#v", runner.calls, runner.request)
	}
	if runner.request.MaxAge != 0 || runner.request.CSSSelector != "main" || runner.request.OutputFormat != "text" || runner.request.ExtractMode != "pruning" || runner.request.Timeout != 17 || !runner.request.Stealth || runner.request.ProxyURL != "https://proxy.example:8443" {
		t.Fatalf("canonical scrape request = %#v", runner.request)
	}
	if runner.request.WaitForNetworkIdle == nil || *runner.request.WaitForNetworkIdle {
		t.Fatalf("wait_for_network_idle = %#v, want false", runner.request.WaitForNetworkIdle)
	}
	if !reflect.DeepEqual(request.Schema, wantSchema) || request.WaitForNetworkIdle != &waitForNetwork || waitForNetwork {
		t.Fatalf("caller request mutated: %#v", request)
	}
	if response.Timing.NavigationMs != runner.result.Response.Timing.NavigationMs || response.Timing.CleaningMs != runner.result.Response.Timing.CleaningMs {
		t.Fatalf("timing = %#v", response.Timing)
	}
}

func TestServiceExtractReturnsPartialAfterExactlyOneRepair(t *testing.T) {
	client := &recordingExtractor{
		initial:  &llm.ExtractResult{Data: json.RawMessage(`{"count":"three"}`)},
		repaired: &llm.ExtractResult{Data: json.RawMessage(`{"count":"still-three"}`)},
	}
	service := newTestService(t, &recordingRunner{result: successfulScrapeResult()}, client, nil)
	response, err := service.Extract(context.Background(), validExtractRequest())
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if !response.Partial || len(response.Violations) == 0 || string(response.Data) != `{"count":"still-three"}` {
		t.Fatalf("partial response = %#v", response)
	}
	if client.extractCalls != 1 || client.repairCalls != 1 {
		t.Fatalf("extract/repair calls = %d/%d, want 1/1", client.extractCalls, client.repairCalls)
	}
}

func TestServiceExtractBuildsEvidenceAndReceiptsFromSelectedSource(t *testing.T) {
	fetchedAt := time.Date(2026, time.August, 9, 8, 0, 0, 0, time.UTC)
	result := successfulScrapeResult()
	result.Response.Content = "Ada\nPrice: $29.99"
	result.Source = &scraper.ScrapeResult{
		RawHTML:     `<html><body><main><h1>Ada</h1><span class="price">$29.99</span></main></body></html>`,
		FinalURL:    "https://example.test/final",
		SnapshotID:  snapshot.ID("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		FetchedAt:   fetchedAt,
		StatusCode:  200,
		ContentType: "text/html",
	}
	client := &recordingExtractor{initial: &llm.ExtractResult{Data: json.RawMessage(`{"name":"Ada","price":29.99}`)}}
	signer := &recordingSigner{}
	service := newTestService(t, &recordingRunner{result: result}, client, signer)
	request := validExtractRequest()
	request.Evidence = true
	request.Schema = json.RawMessage(`{
		"type":"object",
		"properties":{"name":{"type":"string"},"price":{"type":"number"}},
		"required":["name","price"],"additionalProperties":false
	}`)

	response, err := service.Extract(context.Background(), request)
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if response.SnapshotID != string(result.Source.SnapshotID) || response.Basis == nil || response.Receipts == nil || response.UnlocatedRate == nil {
		t.Fatalf("evidence response = %#v", response)
	}
	if got := *response.UnlocatedRate; got != 0 {
		t.Fatalf("unlocated rate = %v, want 0", got)
	}
	if len(*response.Basis) != 2 || len(*response.Receipts) != 2 || len(signer.payloads) != 2 {
		t.Fatalf("basis/receipts/signed = %d/%d/%d", len(*response.Basis), len(*response.Receipts), len(signer.payloads))
	}
	for index, payload := range signer.payloads {
		if index > 0 && signer.payloads[index-1].Path > payload.Path {
			t.Fatalf("receipt signing order is unstable: %#v", signer.payloads)
		}
		if payload.URL != result.Source.FinalURL || payload.Anchor.SnapshotID != string(result.Source.SnapshotID) || !payload.Anchor.FetchedAt.Equal(fetchedAt) {
			t.Fatalf("receipt payload = %#v", payload)
		}
		if payload.ExtractorVersion != "" {
			t.Fatalf("LLM receipt extractor version = %q, want empty", payload.ExtractorVersion)
		}
		if token := (*response.Receipts)[payload.Path]; token != "signed:"+payload.Path {
			t.Fatalf("receipt[%q] = %q", payload.Path, token)
		}
	}
}

func TestEvidenceFailsClosedWithoutRawSnapshotOrSigner(t *testing.T) {
	tests := []struct {
		name   string
		result *scrape.Result
		signer ReceiptSigner
	}{
		{name: "response-only artifact", result: func() *scrape.Result {
			result := successfulScrapeResult()
			result.Source = nil
			return result
		}(), signer: &recordingSigner{}},
		{name: "missing snapshot", result: func() *scrape.Result {
			result := successfulScrapeResult()
			result.Source.SnapshotID = ""
			return result
		}(), signer: &recordingSigner{}},
		{name: "missing signer", result: successfulScrapeResult()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := newTestService(t, &recordingRunner{result: test.result}, &recordingExtractor{initial: &llm.ExtractResult{Data: json.RawMessage(`{"count":3}`)}}, test.signer)
			request := validExtractRequest()
			request.Evidence = true
			_, err := service.Extract(context.Background(), request)
			assertScrapeErrorCode(t, err, models.ErrCodeEvidenceUnavailable)
			if timing, ok := TimingFromError(err); !ok || timing.TotalMs < 0 {
				t.Fatalf("TimingFromError() = %#v, %v", timing, ok)
			}
		})
	}
}

func TestResponseOnlyArtifactSupportsExtractionWithoutEvidence(t *testing.T) {
	result := successfulScrapeResult()
	artifact := &Artifact{Public: result.Response}
	runner := &recordingRunner{err: errors.New("must not fetch")}
	service := newTestService(t, runner, &recordingExtractor{initial: &llm.ExtractResult{Data: json.RawMessage(`{"count":3}`)}}, nil)
	response, err := service.ExtractArtifact(context.Background(), artifact, validExtractRequest())
	if err != nil || !response.Success {
		t.Fatalf("ExtractArtifact() = %#v, %v", response, err)
	}
	if runner.calls != 0 {
		t.Fatalf("ExtractArtifact fetched %d times", runner.calls)
	}
}

func TestFetchArtifactBypassesCacheAndDetachesRequest(t *testing.T) {
	runner := &recordingRunner{result: successfulScrapeResult()}
	service := newTestService(t, runner, &recordingExtractor{}, nil)
	wait := false
	onlyMain := false
	request := &models.ScrapeRequest{
		URL:                "https://example.test/page",
		MaxAge:             60_000,
		WaitForNetworkIdle: &wait,
		OnlyMainContent:    &onlyMain,
		Headers:            map[string]string{"X-Test": "one"},
		Cookies:            []models.Cookie{{Name: "a", Value: "one"}},
		Actions:            []models.Action{{Type: "click", Selector: "#one"}},
		IncludeTags:        []string{"main"},
		ExcludeTags:        []string{"nav"},
	}
	artifact, err := service.FetchArtifact(context.Background(), request)
	if err != nil || artifact.Public == nil {
		t.Fatalf("FetchArtifact() = %#v, %v", artifact, err)
	}
	if runner.request == request || runner.request.MaxAge != 0 || request.MaxAge != 60_000 {
		t.Fatalf("request ownership/cache = runner:%#v caller:%#v", runner.request, request)
	}
	runner.request.Headers["X-Test"] = "runner-mutated"
	runner.request.Cookies[0].Value = "runner-mutated"
	runner.request.Actions[0].Selector = "#runner-mutated"
	runner.request.IncludeTags[0] = "runner-mutated"
	runner.request.ExcludeTags[0] = "runner-mutated"
	*runner.request.WaitForNetworkIdle = true
	*runner.request.OnlyMainContent = true
	if request.Headers["X-Test"] != "one" || request.Cookies[0].Value != "one" || request.Actions[0].Selector != "#one" || request.IncludeTags[0] != "main" || request.ExcludeTags[0] != "nav" || wait || onlyMain {
		t.Fatalf("runner mutation escaped to caller: %#v", request)
	}
}

func TestFetchPublicArtifactUsesFixedProfileAndReusesOneFetch(t *testing.T) {
	result := successfulPublicArtifactResult()
	result.Response.FinalURL = "HTTPS://Final.Example.Test:443/article#public"
	result.Response.Metadata.SourceURL = "https://FINAL.example.test/article#metadata"
	result.Source.FinalURL = "https://final.example.test:443/article#source"
	runner := &recordingRunner{result: result}
	client := &recordingExtractor{initial: &llm.ExtractResult{Data: json.RawMessage(`{"count":3}`)}}
	service := newPublicArtifactTestService(t, runner, client)

	artifact, err := service.FetchPublicArtifact(
		context.Background(),
		"HTTPS://Example.Test:443/page#provider-fragment",
	)
	if err != nil {
		t.Fatalf("FetchPublicArtifact() error = %v", err)
	}
	if artifact == nil || artifact.Public == nil || artifact.Source == nil {
		t.Fatalf("artifact = %#v", artifact)
	}
	if artifact.Public.FinalURL != "https://final.example.test/article" ||
		artifact.Public.Metadata.SourceURL != artifact.Public.FinalURL ||
		artifact.Source.FinalURL != artifact.Public.FinalURL {
		t.Fatalf("canonical final URLs = public:%q metadata:%q source:%q",
			artifact.Public.FinalURL, artifact.Public.Metadata.SourceURL, artifact.Source.FinalURL)
	}
	if runner.calls != 1 || runner.request == nil {
		t.Fatalf("runner calls/request = %d/%#v", runner.calls, runner.request)
	}
	request := runner.request
	if request.URL != "https://example.test/page" || request.ProxyURL != testPublicArtifactProxyURL ||
		request.Timeout != defaultPublicArtifactTimeout || request.MaxAge != 0 ||
		request.MaximumBodyBytes != maximumExtractArtifactBytes || request.OutputFormat != "markdown" ||
		request.ExtractMode != "readability" || request.WaitForNetworkIdle == nil || !*request.WaitForNetworkIdle {
		t.Fatalf("fixed public artifact request = %#v", request)
	}
	if request.Stealth || request.CDPURL != "" || request.CSSSelector != "" || len(request.Headers) != 0 ||
		len(request.Cookies) != 0 || len(request.Actions) != 0 || len(request.IncludeTags) != 0 ||
		len(request.ExcludeTags) != 0 || request.OnlyMainContent != nil || request.RemoveOverlays || request.BlockAds {
		t.Fatalf("caller-controlled scrape state escaped fixed profile: %#v", request)
	}

	extractRequest := validExtractRequest()
	extractRequest.URL = "https://example.test/page"
	response, err := service.ExtractArtifact(context.Background(), artifact, extractRequest)
	if err != nil || response == nil || !response.Success {
		t.Fatalf("ExtractArtifact(reused) = %#v, %v", response, err)
	}
	if runner.calls != 1 || client.extractCalls != 1 {
		t.Fatalf("reuse calls = runner:%d extractor:%d, want 1/1", runner.calls, client.extractCalls)
	}
}

func TestFetchPublicArtifactValidatesURLBeforeExternalWork(t *testing.T) {
	exactURL := "https://example.test/" + strings.Repeat("a", maximumExtractURLBytes-len("https://example.test/"))
	invalidUTF8 := string(append([]byte("https://example.test/"), 0xff))
	tests := []struct {
		name    string
		rawURL  string
		wantErr bool
	}{
		{name: "exact URL bytes", rawURL: exactURL},
		{name: "URL bytes N plus one", rawURL: exactURL + "a", wantErr: true},
		{name: "empty", rawURL: "", wantErr: true},
		{name: "relative", rawURL: "/relative", wantErr: true},
		{name: "unsupported scheme", rawURL: "file:///etc/passwd", wantErr: true},
		{name: "userinfo", rawURL: "https://user:secret@example.test/", wantErr: true},
		{name: "localhost", rawURL: "http://localhost/", wantErr: true},
		{name: "localhost suffix", rawURL: "http://api.localhost/", wantErr: true},
		{name: "private IPv4", rawURL: "http://10.0.0.1/", wantErr: true},
		{name: "reserved IPv4", rawURL: "http://169.254.169.254/latest/meta-data/", wantErr: true},
		{name: "loopback IPv6", rawURL: "http://[::1]/", wantErr: true},
		{name: "control", rawURL: "https://example.test/\nsecret", wantErr: true},
		{name: "invalid UTF-8", rawURL: invalidUTF8, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &recordingRunner{result: successfulPublicArtifactResult()}
			service := newPublicArtifactTestService(t, runner, &recordingExtractor{})
			artifact, err := service.FetchPublicArtifact(context.Background(), test.rawURL)
			if test.wantErr {
				assertScrapeErrorCode(t, err, models.ErrCodeInvalidInput)
				if artifact != nil || runner.calls != 0 {
					t.Fatalf("invalid URL artifact/calls = %#v/%d", artifact, runner.calls)
				}
				return
			}
			if err != nil || artifact == nil || runner.calls != 1 || runner.request.URL != exactURL {
				t.Fatalf("exact URL artifact/error/calls/request = %#v/%v/%d/%#v", artifact, err, runner.calls, runner.request)
			}
		})
	}
}

func TestFetchPublicArtifactFailsClosedWithoutCapabilityOrContext(t *testing.T) {
	t.Run("safe relay unavailable", func(t *testing.T) {
		runner := &recordingRunner{result: successfulPublicArtifactResult()}
		service := newTestService(t, runner, &recordingExtractor{}, nil)
		_, err := service.FetchPublicArtifact(context.Background(), "https://example.test/")
		assertScrapeErrorCode(t, err, models.ErrCodeInternal)
		if runner.calls != 0 {
			t.Fatalf("runner calls = %d, want zero", runner.calls)
		}
	})

	t.Run("nil context", func(t *testing.T) {
		runner := &recordingRunner{result: successfulPublicArtifactResult()}
		service := newPublicArtifactTestService(t, runner, &recordingExtractor{})
		_, err := service.FetchPublicArtifact(nil, "https://example.test/")
		assertScrapeErrorCode(t, err, models.ErrCodeInvalidInput)
		if runner.calls != 0 {
			t.Fatalf("runner calls = %d, want zero", runner.calls)
		}
	})

	t.Run("canceled context", func(t *testing.T) {
		runner := &recordingRunner{result: successfulPublicArtifactResult()}
		service := newPublicArtifactTestService(t, runner, &recordingExtractor{})
		_, err := service.FetchPublicArtifact(canceledContext(), "https://example.test/")
		assertScrapeErrorCode(t, err, models.ErrCodeTimeout)
		if !errors.Is(err, context.Canceled) || runner.calls != 0 {
			t.Fatalf("canceled error/calls = %v/%d", err, runner.calls)
		}
	})

	t.Run("runner cancels before returning success", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		runner := &recordingRunner{
			result:       successfulPublicArtifactResult(),
			beforeReturn: cancel,
		}
		service := newPublicArtifactTestService(t, runner, &recordingExtractor{})
		artifact, err := service.FetchPublicArtifact(ctx, "https://example.test/")
		assertScrapeErrorCode(t, err, models.ErrCodeTimeout)
		if artifact != nil || !errors.Is(err, context.Canceled) || runner.calls != 1 {
			t.Fatalf("canceled artifact/error/calls = %#v/%v/%d", artifact, err, runner.calls)
		}
	})
}

func TestFetchPublicArtifactEnforcesArtifactByteBounds(t *testing.T) {
	tests := []struct {
		name     string
		content  int
		rawHTML  int
		wantCode string
	}{
		{name: "cleaned exact N", content: maximumExtractArtifactBytes, rawHTML: 16},
		{name: "raw exact N", content: 16, rawHTML: maximumExtractArtifactBytes},
		{name: "cleaned N plus one", content: maximumExtractArtifactBytes + 1, rawHTML: 16, wantCode: models.ErrCodeNavigation},
		{name: "raw N plus one", content: 16, rawHTML: maximumExtractArtifactBytes + 1, wantCode: models.ErrCodeNavigation},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := successfulPublicArtifactResult()
			result.Response.Content = strings.Repeat("c", test.content)
			result.Source.RawHTML = strings.Repeat("r", test.rawHTML)
			runner := &recordingRunner{result: result}
			service := newPublicArtifactTestService(t, runner, &recordingExtractor{})
			artifact, err := service.FetchPublicArtifact(context.Background(), "https://example.test/")
			if test.wantCode != "" {
				assertScrapeErrorCode(t, err, test.wantCode)
				if artifact != nil {
					t.Fatalf("oversized artifact = %#v", artifact)
				}
				return
			}
			if err != nil || artifact == nil {
				t.Fatalf("exact artifact = %#v, %v", artifact, err)
			}
		})
	}
}

func TestFetchPublicArtifactRejectsUnsafeOrInconsistentResults(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*scrape.Result)
		wantCode string
	}{
		{name: "missing source", mutate: func(result *scrape.Result) { result.Source = nil }, wantCode: models.ErrCodeInternal},
		{name: "empty raw source", mutate: func(result *scrape.Result) { result.Source.RawHTML = "" }, wantCode: models.ErrCodeInternal},
		{name: "zero fetched at", mutate: func(result *scrape.Result) { result.Source.FetchedAt = time.Time{} }, wantCode: models.ErrCodeInternal},
		{name: "invalid status", mutate: func(result *scrape.Result) { result.Source.StatusCode = 0 }, wantCode: models.ErrCodeInternal},
		{name: "status mismatch", mutate: func(result *scrape.Result) { result.Response.StatusCode = 201 }, wantCode: models.ErrCodeInternal},
		{name: "private final URL", mutate: func(result *scrape.Result) { result.Source.FinalURL = "http://127.0.0.1/admin" }, wantCode: models.ErrCodeNavigation},
		{name: "relative final URL", mutate: func(result *scrape.Result) { result.Source.FinalURL = "/relative" }, wantCode: models.ErrCodeNavigation},
		{name: "credential final URL", mutate: func(result *scrape.Result) { result.Source.FinalURL = "https://user:secret@example.test/" }, wantCode: models.ErrCodeNavigation},
		{name: "public final mismatch", mutate: func(result *scrape.Result) { result.Response.FinalURL = "https://other.example.test/" }, wantCode: models.ErrCodeInternal},
		{name: "metadata final mismatch", mutate: func(result *scrape.Result) { result.Response.Metadata.SourceURL = "https://other.example.test/" }, wantCode: models.ErrCodeInternal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := successfulPublicArtifactResult()
			test.mutate(result)
			runner := &recordingRunner{result: result}
			service := newPublicArtifactTestService(t, runner, &recordingExtractor{})
			artifact, err := service.FetchPublicArtifact(context.Background(), "https://example.test/")
			assertScrapeErrorCode(t, err, test.wantCode)
			if artifact != nil || runner.calls != 1 {
				t.Fatalf("artifact/calls = %#v/%d", artifact, runner.calls)
			}
		})
	}
}

func TestFetchPublicArtifactRejectsEmptyOrUnsuccessfulRunnerResults(t *testing.T) {
	tests := []struct {
		name     string
		result   *scrape.Result
		wantCode string
	}{
		{name: "nil result", wantCode: models.ErrCodeInternal},
		{name: "nil response", result: &scrape.Result{}, wantCode: models.ErrCodeInternal},
		{name: "unsuccessful response", result: &scrape.Result{
			Response: &models.ScrapeResponse{Success: false, Error: &models.ErrorDetail{
				Code: models.ErrCodeNavigation, Message: "private runner detail",
			}},
		}, wantCode: models.ErrCodeNavigation},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &recordingRunner{result: test.result}
			service := newPublicArtifactTestService(t, runner, &recordingExtractor{})
			artifact, err := service.FetchPublicArtifact(context.Background(), "https://example.test/")
			assertScrapeErrorCode(t, err, test.wantCode)
			var scrapeError *models.ScrapeError
			if artifact != nil || runner.calls != 1 || !errors.As(err, &scrapeError) ||
				scrapeError.ToDetail().Message != "public artifact fetch failed" {
				t.Fatalf("artifact/calls/error = %#v/%d/%#v", artifact, runner.calls, scrapeError)
			}
		})
	}
}

func TestFetchPublicArtifactPreservesDeadHTTPStatuses(t *testing.T) {
	for _, statusCode := range []int{404, 410} {
		t.Run(strconv.Itoa(statusCode), func(t *testing.T) {
			result := successfulPublicArtifactResult()
			result.Response.StatusCode = statusCode
			result.Source.StatusCode = statusCode
			runner := &recordingRunner{result: result}
			service := newPublicArtifactTestService(t, runner, &recordingExtractor{})
			artifact, err := service.FetchPublicArtifact(context.Background(), "https://example.test/dead")
			if err != nil || artifact == nil || artifact.Public.StatusCode != statusCode || artifact.Source.StatusCode != statusCode {
				t.Fatalf("dead artifact = %#v, %v", artifact, err)
			}
		})
	}
}

func TestFetchPublicArtifactSanitizesRunnerErrorDetail(t *testing.T) {
	runner := &recordingRunner{err: models.NewScrapeError(
		models.ErrCodeNavigation,
		"private upstream detail",
		errors.New("private socket detail"),
	)}
	service := newPublicArtifactTestService(t, runner, &recordingExtractor{})
	_, err := service.FetchPublicArtifact(context.Background(), "https://example.test/")
	assertScrapeErrorCode(t, err, models.ErrCodeNavigation)
	var scrapeError *models.ScrapeError
	if !errors.As(err, &scrapeError) || scrapeError.ToDetail().Message != "public artifact fetch failed" ||
		strings.Contains(scrapeError.ToDetail().Message, "private") {
		t.Fatalf("public detail = %#v", scrapeError)
	}
}

func TestFetchPublicArtifactRejectsUnknownRunnerErrorCode(t *testing.T) {
	runner := &recordingRunner{err: models.NewScrapeError(
		"PRIVATE_PROVIDER_FAILURE",
		"private upstream detail",
		errors.New("private socket detail"),
	)}
	service := newPublicArtifactTestService(t, runner, &recordingExtractor{})
	_, err := service.FetchPublicArtifact(context.Background(), "https://example.test/")
	assertScrapeErrorCode(t, err, models.ErrCodeNavigation)
	var scrapeError *models.ScrapeError
	if !errors.As(err, &scrapeError) || scrapeError.ToDetail().Message != "public artifact fetch failed" ||
		strings.Contains(scrapeError.ToDetail().Code, "PRIVATE") ||
		strings.Contains(scrapeError.ToDetail().Message, "private") {
		t.Fatalf("public detail = %#v", scrapeError)
	}
}

func TestServiceValidationAndCancellationAvoidExternalCalls(t *testing.T) {
	tests := []struct {
		name    string
		request *models.ExtractRequest
		ctx     context.Context
	}{
		{name: "nil request", ctx: context.Background()},
		{name: "relative URL", request: func() *models.ExtractRequest { r := validExtractRequest(); r.URL = "/relative"; return r }(), ctx: context.Background()},
		{name: "missing LLM key", request: func() *models.ExtractRequest {
			r := validExtractRequest()
			r.Engine = "llm"
			r.LLMAPIKey = ""
			return r
		}(), ctx: context.Background()},
		{name: "invalid schema", request: func() *models.ExtractRequest { r := validExtractRequest(); r.Schema = json.RawMessage(`{`); return r }(), ctx: context.Background()},
		{name: "invalid engine", request: func() *models.ExtractRequest { r := validExtractRequest(); r.Engine = "hybrid"; return r }(), ctx: context.Background()},
		{name: "canceled context", request: validExtractRequest(), ctx: canceledContext()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &recordingRunner{result: successfulScrapeResult()}
			client := &recordingExtractor{initial: &llm.ExtractResult{Data: json.RawMessage(`{"count":3}`)}}
			service := newTestService(t, runner, client, nil)
			_, err := service.Extract(test.ctx, test.request)
			if err == nil {
				t.Fatal("Extract() error = nil")
			}
			if test.name == "canceled context" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error = %v, want context.Canceled", err)
				}
				assertScrapeErrorCode(t, err, models.ErrCodeTimeout)
			} else {
				assertScrapeErrorCode(t, err, models.ErrCodeInvalidInput)
			}
			if runner.calls != 0 || client.extractCalls != 0 || client.repairCalls != 0 {
				t.Fatalf("external calls = runner:%d extract:%d repair:%d", runner.calls, client.extractCalls, client.repairCalls)
			}
		})
	}
}

func TestPrepareRequestAcceptsExactFieldLimitsAndRejectsNPlusOne(t *testing.T) {
	tests := []struct {
		name   string
		atN    func(*models.ExtractRequest)
		atNOne func(*models.ExtractRequest)
	}{
		{
			name: "url",
			atN: func(request *models.ExtractRequest) {
				prefix := "https://example.test/"
				request.URL = prefix + strings.Repeat("u", maximumExtractURLBytes-len(prefix))
			},
			atNOne: func(request *models.ExtractRequest) {
				prefix := "https://example.test/"
				request.URL = prefix + strings.Repeat("u", maximumExtractURLBytes+1-len(prefix))
			},
		},
		{
			name: "schema",
			atN: func(request *models.ExtractRequest) {
				request.Schema = exactSchemaBytes(t, maximumExtractSchemaBytes)
			},
			atNOne: func(request *models.ExtractRequest) {
				request.Schema = exactSchemaBytes(t, maximumExtractSchemaBytes+1)
			},
		},
		{
			name: "llm_api_key",
			atN: func(request *models.ExtractRequest) {
				request.LLMAPIKey = strings.Repeat("k", maximumExtractCredentialBytes)
			},
			atNOne: func(request *models.ExtractRequest) {
				request.LLMAPIKey = strings.Repeat("k", maximumExtractCredentialBytes+1)
			},
		},
		{
			name: "llm_model",
			atN: func(request *models.ExtractRequest) {
				request.LLMModel = strings.Repeat("m", maximumExtractModelBytes)
			},
			atNOne: func(request *models.ExtractRequest) {
				request.LLMModel = strings.Repeat("m", maximumExtractModelBytes+1)
			},
		},
		{
			name: "llm_base_url",
			atN: func(request *models.ExtractRequest) {
				prefix := "https://provider.example/"
				request.LLMBaseURL = prefix + strings.Repeat("b", maximumExtractBaseURLBytes-len(prefix))
			},
			atNOne: func(request *models.ExtractRequest) {
				prefix := "https://provider.example/"
				request.LLMBaseURL = prefix + strings.Repeat("b", maximumExtractBaseURLBytes+1-len(prefix))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			atLimit := validExtractRequest()
			test.atN(atLimit)
			if _, err := prepareRequest(atLimit); err != nil {
				t.Fatalf("prepareRequest(N) error = %v", err)
			}

			overLimit := validExtractRequest()
			test.atNOne(overLimit)
			if _, err := prepareRequest(overLimit); err == nil {
				t.Fatal("prepareRequest(N+1) error = nil")
			} else {
				assertScrapeErrorCode(t, err, models.ErrCodeInvalidInput)
			}
		})
	}
}

func TestServiceBoundaryValidationAvoidsRunnerAndProvider(t *testing.T) {
	expandedSchema := oversizedNormalizedLegacySchema(t)
	tests := []struct {
		name   string
		mutate func(*models.ExtractRequest)
	}{
		{name: "oversized URL", mutate: func(request *models.ExtractRequest) { request.URL = strings.Repeat("u", maximumExtractURLBytes+1) }},
		{name: "oversized raw schema", mutate: func(request *models.ExtractRequest) {
			request.Schema = exactSchemaBytes(t, maximumExtractSchemaBytes+1)
		}},
		{name: "oversized normalized schema", mutate: func(request *models.ExtractRequest) { request.Schema = expandedSchema }},
		{name: "oversized API key", mutate: func(request *models.ExtractRequest) {
			request.LLMAPIKey = strings.Repeat("k", maximumExtractCredentialBytes+1)
		}},
		{name: "oversized model", mutate: func(request *models.ExtractRequest) {
			request.LLMModel = strings.Repeat("m", maximumExtractModelBytes+1)
		}},
		{name: "oversized base URL", mutate: func(request *models.ExtractRequest) {
			request.LLMBaseURL = strings.Repeat("b", maximumExtractBaseURLBytes+1)
		}},
		{name: "invalid URL UTF-8", mutate: func(request *models.ExtractRequest) { request.URL = "https://example.test/\xff" }},
		{name: "invalid schema UTF-8", mutate: func(request *models.ExtractRequest) {
			request.Schema = json.RawMessage{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}
		}},
		{name: "invalid API key UTF-8", mutate: func(request *models.ExtractRequest) { request.LLMAPIKey = "key-\xff" }},
		{name: "invalid model UTF-8", mutate: func(request *models.ExtractRequest) { request.LLMModel = "model-\xff" }},
		{name: "invalid base URL UTF-8", mutate: func(request *models.ExtractRequest) { request.LLMBaseURL = "https://provider.example/\xff" }},
		{name: "URL control", mutate: func(request *models.ExtractRequest) { request.URL = "https://example.test/boundary-secret\n" }},
		{name: "API key control", mutate: func(request *models.ExtractRequest) { request.LLMAPIKey = "boundary-secret\n" }},
		{name: "model whitespace", mutate: func(request *models.ExtractRequest) { request.LLMModel = "boundary secret" }},
		{name: "base URL control", mutate: func(request *models.ExtractRequest) { request.LLMBaseURL = "https://boundary-secret.example/\n" }},
		{name: "base URL userinfo", mutate: func(request *models.ExtractRequest) {
			request.LLMBaseURL = "https://boundary-secret@provider.example/v1"
		}},
		{name: "base URL query", mutate: func(request *models.ExtractRequest) {
			request.LLMBaseURL = "https://provider.example/v1?secret=boundary-secret"
		}},
		{name: "base URL fragment", mutate: func(request *models.ExtractRequest) {
			request.LLMBaseURL = "https://provider.example/v1#boundary-secret"
		}},
		{name: "base URL empty port", mutate: func(request *models.ExtractRequest) { request.LLMBaseURL = "https://provider.example:/v1" }},
		{name: "base URL private literal", mutate: func(request *models.ExtractRequest) { request.LLMBaseURL = "http://127.0.0.1:8080/v1" }},
		{name: "base URL localhost", mutate: func(request *models.ExtractRequest) { request.LLMBaseURL = "http://localhost:8080/v1" }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := &recordingRunner{result: successfulScrapeResult()}
			provider := &recordingExtractor{initial: &llm.ExtractResult{Data: json.RawMessage(`{"count":3}`)}}
			service := newTestService(t, runner, provider, nil)
			request := validExtractRequest()
			test.mutate(request)

			_, err := service.Extract(context.Background(), request)
			if err == nil {
				t.Fatal("Extract() error = nil")
			}
			assertScrapeErrorCode(t, err, models.ErrCodeInvalidInput)
			if strings.Contains(err.Error(), "boundary-secret") {
				t.Fatalf("error leaked request value: %v", err)
			}
			if runner.calls != 0 || provider.extractCalls != 0 || provider.repairCalls != 0 {
				t.Fatalf("external calls = runner:%d provider:%d/%d", runner.calls, provider.extractCalls, provider.repairCalls)
			}
		})
	}
}

func TestServicePreservesRunnerAndLLMErrorsWithTiming(t *testing.T) {
	runnerErr := models.NewScrapeError(models.ErrCodeTimeout, "fetch timed out", context.DeadlineExceeded)
	service := newTestService(t, &recordingRunner{err: runnerErr}, &recordingExtractor{}, nil)
	_, err := service.Extract(context.Background(), validExtractRequest())
	assertScrapeErrorCode(t, err, models.ErrCodeTimeout)
	if timing, ok := TimingFromError(err); !ok || timing.TotalMs < 0 || timing.NavigationMs < 0 {
		t.Fatalf("runner TimingFromError() = %#v, %v", timing, ok)
	}

	llmErr := models.NewScrapeError(models.ErrCodeLLMRateLimited, "rate limited", nil)
	service = newTestService(t, &recordingRunner{result: successfulScrapeResult()}, &recordingExtractor{extractErr: llmErr}, nil)
	_, err = service.Extract(context.Background(), validExtractRequest())
	assertScrapeErrorCode(t, err, models.ErrCodeLLMRateLimited)
	if timing, ok := TimingFromError(err); !ok || timing.CleaningMs != successfulScrapeResult().Response.Timing.CleaningMs {
		t.Fatalf("LLM TimingFromError() = %#v, %v", timing, ok)
	}
}

func TestServiceRejectsInvalidProviderJSONAsLLMFailure(t *testing.T) {
	service := newTestService(
		t,
		&recordingRunner{result: successfulScrapeResult()},
		&recordingExtractor{initial: &llm.ExtractResult{Data: json.RawMessage(`{"count":`)}},
		nil,
	)
	_, err := service.Extract(context.Background(), validExtractRequest())
	assertScrapeErrorCode(t, err, models.ErrCodeLLMFailure)
}

func TestNewServiceRejectsMissingRequiredDependencies(t *testing.T) {
	runner := &recordingRunner{}
	client := &recordingExtractor{}
	if _, err := NewService(nil, client, nil, Config{}); err == nil {
		t.Fatal("NewService accepted nil runner")
	}
	if _, err := NewService(runner, nil, nil, Config{}); err == nil {
		t.Fatal("NewService accepted nil extractor")
	}
	if _, err := NewService(runner, client, nil, Config{}); err != nil {
		t.Fatalf("NewService rejected optional nil signer: %v", err)
	}
}

type recordingRunner struct {
	result       *scrape.Result
	err          error
	beforeReturn func()
	calls        int
	request      *models.ScrapeRequest
}

func (runner *recordingRunner) Run(ctx context.Context, request *models.ScrapeRequest, _ scrape.Observer) (*scrape.Result, error) {
	runner.calls++
	if request != nil {
		cloned := *request
		models.ApplyScrapeOptions(&cloned, models.ScrapeOptionsFromRequest(request))
		runner.request = &cloned
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if runner.beforeReturn != nil {
		runner.beforeReturn()
	}
	return runner.result, runner.err
}

type recordingExtractor struct {
	initial      *llm.ExtractResult
	repaired     *llm.ExtractResult
	extractErr   error
	repairErr    error
	extractCalls int
	repairCalls  int
	content      string
	schema       json.RawMessage
	params       llm.ExtractParams
}

func (client *recordingExtractor) Extract(_ context.Context, content string, schema json.RawMessage, params llm.ExtractParams) (*llm.ExtractResult, error) {
	client.extractCalls++
	client.content = content
	client.schema = append(json.RawMessage(nil), schema...)
	client.params = params
	return cloneExtractResult(client.initial), client.extractErr
}

func (client *recordingExtractor) ExtractWithRepair(_ context.Context, _ string, _ json.RawMessage, _ json.RawMessage, _ []llm.Violation, _ llm.ExtractParams) (*llm.ExtractResult, error) {
	client.repairCalls++
	return cloneExtractResult(client.repaired), client.repairErr
}

type recordingSigner struct {
	payloads []receipts.Payload
	err      error
}

func (signer *recordingSigner) Sign(payload receipts.Payload) (string, error) {
	signer.payloads = append(signer.payloads, payload)
	if signer.err != nil {
		return "", signer.err
	}
	return "signed:" + payload.Path, nil
}

func newTestService(t *testing.T, runner Runner, client StructuredExtractor, signer ReceiptSigner) *Service {
	t.Helper()
	service, err := NewService(runner, client, signer, Config{})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return service
}

func successfulScrapeResult() *scrape.Result {
	return &scrape.Result{
		Response: &models.ScrapeResponse{
			Success:    true,
			StatusCode: 200,
			FinalURL:   "https://example.test/final",
			Content:    "Count: 3",
			Metadata: models.Metadata{
				Title:     "Example",
				SourceURL: "https://example.test/final",
			},
			Tokens: models.TokenInfo{OriginalEstimate: 8, CleanedEstimate: 3, SavingsPercent: 62.5},
			Timing: models.TimingInfo{TotalMs: 9, NavigationMs: 5, CleaningMs: 4},
		},
		Source: &scraper.ScrapeResult{
			RawHTML:    `<html><body><main>Count: 3</main></body></html>`,
			FinalURL:   "https://example.test/final",
			SnapshotID: snapshot.ID("sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
			FetchedAt:  time.Date(2026, time.August, 9, 8, 0, 0, 0, time.UTC),
			StatusCode: 200,
		},
	}
}

func successfulPublicArtifactResult() *scrape.Result {
	return successfulScrapeResult()
}

func newPublicArtifactTestService(t *testing.T, runner Runner, client StructuredExtractor) *Service {
	t.Helper()
	service, err := NewService(runner, client, nil, Config{SafeProxyURL: testPublicArtifactProxyURL})
	if err != nil {
		t.Fatalf("NewService(public artifact) error = %v", err)
	}
	return service
}

func validExtractRequest() *models.ExtractRequest {
	return &models.ExtractRequest{
		URL:       "https://example.test/product",
		Schema:    json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"],"additionalProperties":false}`),
		LLMAPIKey: "secret",
	}
}

func exactSchemaBytes(t *testing.T, size int) json.RawMessage {
	t.Helper()
	prefix := `{"type":"object","description":"`
	suffix := `"}`
	if size < len(prefix)+len(suffix) {
		t.Fatalf("schema size %d is too small", size)
	}
	result := json.RawMessage(prefix + strings.Repeat("s", size-len(prefix)-len(suffix)) + suffix)
	if len(result) != size || !json.Valid(result) {
		t.Fatalf("schema fixture is %d bytes or invalid, want %d", len(result), size)
	}
	return result
}

func oversizedNormalizedLegacySchema(t *testing.T) json.RawMessage {
	t.Helper()
	var builder strings.Builder
	builder.WriteByte('{')
	for index := 0; index < 20_000; index++ {
		if index > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(`"field`)
		builder.WriteString(strconv.Itoa(index))
		builder.WriteString(`":"string"`)
	}
	builder.WriteByte('}')
	raw := json.RawMessage(builder.String())
	if len(raw) > maximumExtractSchemaBytes {
		t.Fatalf("legacy fixture raw size = %d, maximum = %d", len(raw), maximumExtractSchemaBytes)
	}
	normalized, err := llm.NormalizeSchema(raw)
	if err != nil {
		t.Fatalf("NormalizeSchema(legacy fixture) error = %v", err)
	}
	if len(normalized) <= maximumExtractSchemaBytes {
		t.Fatalf("legacy fixture normalized size = %d, want > %d", len(normalized), maximumExtractSchemaBytes)
	}
	return raw
}

func cloneExtractResult(result *llm.ExtractResult) *llm.ExtractResult {
	if result == nil {
		return nil
	}
	cloned := *result
	cloned.Data = append(json.RawMessage(nil), result.Data...)
	cloned.Usage = cloneLLMUsage(result.Usage)
	return &cloned
}

func assertScrapeErrorCode(t *testing.T, err error, want string) {
	t.Helper()
	var scrapeError *models.ScrapeError
	if !errors.As(err, &scrapeError) {
		t.Fatalf("error %T %v is not a ScrapeError", err, err)
	}
	if scrapeError.Code != want {
		t.Fatalf("ScrapeError.Code = %q, want %q", scrapeError.Code, want)
	}
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestSignFieldReceiptsRejectsPathMismatch(t *testing.T) {
	_, err := signFieldReceipts(
		json.RawMessage(`{"name":"Ada"}`),
		models.EvidenceBasis{"other": {Method: evidence.MethodUnlocated}},
		"https://example.test",
		time.Now(),
		&recordingSigner{},
	)
	if err == nil {
		t.Fatal("signFieldReceipts accepted mismatched evidence paths")
	}
}
