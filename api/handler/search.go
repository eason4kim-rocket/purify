package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	"github.com/use-agent/purify/models"
)

// SearchService is the transport-neutral Search orchestration boundary.
type SearchService interface {
	Search(context.Context, *models.SearchRequest) (*models.SearchResponse, error)
}

// SearchResponseEncoder lets a production Search service account for final
// JSON encoding in its own shared CPU/resource budget. Services that do not
// implement it use the bounded four-slot fallback below.
type SearchResponseEncoder interface {
	EncodeSearchResponse(context.Context, *models.SearchResponse) ([]byte, error)
}

// SearchRateLimiter consumes one weighted charge for a decoded Search
// request. It is deliberately invoked once: Search must not also pass through
// the fixed-cost middleware used by the other protected routes.
type SearchRateLimiter interface {
	Allow(*gin.Context, int) bool
}

// MaxSearchRequestCost is the maximum weighted charge accepted by Search:
// ceil(20/10) + 5 content + 5 verify + 2*5 schema extraction.
const MaxSearchRequestCost = 22

var fallbackSearchEncodingSlots = make(chan struct{}, 4)

// Search returns the HTTP adapter for POST /api/v1/search. Direct users get
// the same validation and resource boundaries without an implicit limiter;
// the API router supplies its shared per-identity limiter through
// SearchWithRateLimiter.
func Search(service SearchService) gin.HandlerFunc {
	return SearchWithRateLimiter(service, nil)
}

// SearchWithRateLimiter returns a Search handler using the supplied shared
// identity limiter for the endpoint's weighted, single token-bucket charge.
func SearchWithRateLimiter(service SearchService, limiter SearchRateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		if isNilSearchService(service) {
			respondSearchError(c, http.StatusServiceUnavailable, models.ErrCodeSearchUnavailable, "search is unavailable")
			return
		}
		request, err := decodeSearchRequest(c)
		if err != nil {
			if limiter != nil && !limiter.Allow(c, 1) {
				respondSearchError(c, http.StatusTooManyRequests, models.ErrCodeRateLimited, "search rate limited")
				return
			}
			var maximumBytesError *http.MaxBytesError
			if errors.As(err, &maximumBytesError) {
				respondSearchError(c, http.StatusRequestEntityTooLarge, models.ErrCodeInvalidInput, "search request is too large")
				return
			}
			respondSearchError(c, http.StatusBadRequest, models.ErrCodeInvalidInput, "invalid search request")
			return
		}

		if limiter != nil && !limiter.Allow(c, searchRequestCost(request)) {
			respondSearchError(c, http.StatusTooManyRequests, models.ErrCodeRateLimited, "search rate limited")
			return
		}

		timeoutSeconds := request.Timeout
		if timeoutSeconds == 0 {
			timeoutSeconds = models.DefaultSearchTimeoutSeconds
		}
		taskContext, cancel := context.WithTimeout(c.Request.Context(), time.Duration(timeoutSeconds)*time.Second)
		defer cancel()
		if err := taskContext.Err(); err != nil {
			respondSearchError(c, http.StatusGatewayTimeout, models.ErrCodeTimeout, "search timed out")
			return
		}

		response, err := service.Search(taskContext, request)
		if err != nil {
			if contextErr := taskContext.Err(); contextErr != nil {
				respondSearchError(c, http.StatusGatewayTimeout, models.ErrCodeTimeout, "search timed out")
				return
			}
			status, code, message := mapSearchError(err)
			respondSearchError(c, status, code, message)
			return
		}
		if err := taskContext.Err(); err != nil {
			respondSearchError(c, http.StatusGatewayTimeout, models.ErrCodeTimeout, "search timed out")
			return
		}
		if response == nil {
			respondSearchError(c, http.StatusInternalServerError, models.ErrCodeInternal, "internal search failure")
			return
		}
		if !response.Success || response.Error != nil {
			respondSearchError(c, http.StatusInternalServerError, models.ErrCodeInternal, "internal search failure")
			return
		}

		// Never expose a nullable results collection, including when a service
		// returns the zero value. Clone the envelope so transport normalization
		// does not mutate service-owned state.
		normalized := *response
		if normalized.Results == nil {
			normalized.Results = []models.SearchResult{}
		}
		encoded, err := encodeSearchResponse(taskContext, service, &normalized)
		if err != nil {
			status, code, message := mapSearchError(err)
			respondSearchError(c, status, code, message)
			return
		}
		c.Data(http.StatusOK, "application/json; charset=utf-8", encoded)
	}
}

func decodeSearchRequest(c *gin.Context) (*models.SearchRequest, error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, models.MaxSearchRequestBytes)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(body) {
		return nil, errors.New("search request is not valid UTF-8")
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var request *models.SearchRequest
	if err := decoder.Decode(&request); err != nil {
		return nil, err
	}
	if request == nil {
		return nil, errors.New("search request must be a JSON object")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("search request contains multiple JSON values")
		}
		return nil, err
	}
	if err := binding.Validator.ValidateStruct(request); err != nil {
		return nil, err
	}
	return request, nil
}

func searchRequestCost(request *models.SearchRequest) int {
	if request == nil {
		return 1
	}
	limit := request.Limit
	if limit == 0 {
		limit = models.DefaultSearchLimit
	}
	// decodeSearchRequest validates the public range. Keep this helper safe
	// for direct package callers as well, and preserve the documented max 22.
	if limit < 1 {
		limit = 1
	}
	if limit > models.MaxSearchLimit {
		limit = models.MaxSearchLimit
	}
	heavyResults := limit
	if heavyResults > models.MaxSearchHeavyResults {
		heavyResults = models.MaxSearchHeavyResults
	}
	cost := (limit + 9) / 10
	if request.IncludeContent {
		cost += heavyResults
	}
	if request.Verify {
		cost += heavyResults
	}
	if len(request.Schema) > 0 {
		cost += 2 * heavyResults
	}
	return cost
}

func encodeSearchResponse(ctx context.Context, service SearchService, response *models.SearchResponse) ([]byte, error) {
	if encoder, ok := service.(SearchResponseEncoder); ok {
		if err := ctx.Err(); err != nil {
			return nil, searchEncodingTimeout(err)
		}
		encoded, err := encoder.EncodeSearchResponse(ctx, response)
		if err := ctx.Err(); err != nil {
			return nil, searchEncodingTimeout(err)
		}
		if err != nil {
			return nil, err
		}
		if len(encoded) == 0 || len(encoded) > models.MaxSearchResponseBytes {
			return nil, errors.New("search response exceeds its output budget")
		}
		if !json.Valid(encoded) {
			return nil, errors.New("search response encoder returned invalid JSON")
		}
		if err := ctx.Err(); err != nil {
			return nil, searchEncodingTimeout(err)
		}
		return encoded, nil
	}
	return encodeSearchResponseFallback(ctx, response)
}

func encodeSearchResponseFallback(ctx context.Context, response *models.SearchResponse) ([]byte, error) {
	select {
	case fallbackSearchEncodingSlots <- struct{}{}:
		defer func() { <-fallbackSearchEncodingSlots }()
	case <-ctx.Done():
		return nil, searchEncodingTimeout(ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return nil, searchEncodingTimeout(err)
	}
	preflightOK, err := preflightSearchResponseSize(ctx, response)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, searchEncodingTimeout(contextErr)
		}
		return nil, err
	}
	if !preflightOK {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, searchEncodingTimeout(contextErr)
		}
		return nil, errors.New("search response exceeds its output budget")
	}
	if err := ctx.Err(); err != nil {
		return nil, searchEncodingTimeout(err)
	}
	encoded, err := json.Marshal(response)
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, searchEncodingTimeout(contextErr)
	}
	if err != nil {
		return nil, err
	}
	if len(encoded) > models.MaxSearchResponseBytes {
		return nil, errors.New("search response exceeds its output budget")
	}
	return encoded, nil
}

func preflightSearchResponseSize(ctx context.Context, response *models.SearchResponse) (bool, error) {
	if response == nil {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	remaining := models.MaxSearchResponseBytes
	if !reserveSearchResponseBytes(&remaining, 2) {
		return false, nil
	}
	fields := 0
	var preflightErr error
	addField := func(name string, value any) bool {
		if err := ctx.Err(); err != nil {
			preflightErr = err
			return false
		}
		encodedName, err := json.Marshal(name)
		if err != nil {
			preflightErr = err
			return false
		}
		encodedValue, err := json.Marshal(value)
		if err != nil {
			preflightErr = err
			return false
		}
		if err := ctx.Err(); err != nil {
			preflightErr = err
			return false
		}
		if fields > 0 && !reserveSearchResponseBytes(&remaining, 1) {
			return false
		}
		if !reserveSearchResponseBytes(&remaining, len(encodedName)) ||
			!reserveSearchResponseBytes(&remaining, 1) ||
			!reserveSearchResponseBytes(&remaining, len(encodedValue)) {
			return false
		}
		fields++
		return true
	}

	valid := addField("success", response.Success) &&
		addField("query", response.Query) &&
		addField("results", response.Results) &&
		addField("deduplicated", response.Deduplicated) &&
		addField("dropped_stale", response.DroppedStale) &&
		addField("partial", response.Partial) &&
		addField("timing", response.Timing)
	if response.Error != nil {
		valid = valid && addField("error", response.Error)
	}
	if preflightErr != nil {
		return false, preflightErr
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return valid, nil
}

func reserveSearchResponseBytes(remaining *int, requested int) bool {
	if remaining == nil || requested < 0 || requested > *remaining {
		return false
	}
	*remaining -= requested
	return true
}

func searchEncodingTimeout(cause error) error {
	return models.NewScrapeError(models.ErrCodeTimeout, "search timed out", cause)
}

func respondSearchError(c *gin.Context, status int, code, message string) {
	c.JSON(status, models.SearchResponse{
		Success: false,
		Results: []models.SearchResult{},
		Error:   &models.ErrorDetail{Code: code, Message: message},
	})
}

func mapSearchError(err error) (int, string, string) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, models.ErrCodeTimeout, "search timed out"
	}
	var scrapeError *models.ScrapeError
	if errors.As(err, &scrapeError) {
		switch scrapeError.Code {
		case models.ErrCodeInvalidInput:
			return http.StatusBadRequest, models.ErrCodeInvalidInput, "invalid search request"
		case models.ErrCodeRateLimited:
			return http.StatusTooManyRequests, models.ErrCodeRateLimited, "search rate limited"
		case models.ErrCodeSearchFailed:
			return http.StatusBadGateway, models.ErrCodeSearchFailed, "search failed"
		case models.ErrCodeSearchUnavailable:
			return http.StatusServiceUnavailable, models.ErrCodeSearchUnavailable, "search is unavailable"
		case models.ErrCodeTimeout:
			return http.StatusGatewayTimeout, models.ErrCodeTimeout, "search timed out"
		case models.ErrCodeInternal:
			return http.StatusInternalServerError, models.ErrCodeInternal, "internal search failure"
		}
	}
	return http.StatusInternalServerError, models.ErrCodeInternal, "internal search failure"
}

func isNilSearchService(service SearchService) bool {
	if service == nil {
		return true
	}
	value := reflect.ValueOf(service)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
