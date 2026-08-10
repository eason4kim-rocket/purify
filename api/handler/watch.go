package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/use-agent/purify/ledger"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/search"
	watchdomain "github.com/use-agent/purify/watch"
)

const (
	// MaxWatchRequestCost is both the fixed admission charge for every Watch
	// or Facts request and the minimum safe router burst.
	MaxWatchRequestCost = 1

	maximumWatchCursorBytes = 512
	maximumWatchPathBytes   = 4096
	maximumWatchErrorBytes  = 64
	maximumWatchReceipt     = 2 << 20
	maximumWatchFactValue   = 64 << 10
)

var watchResponseEncodingSlots = make(chan struct{}, 4)

// WatchService is the complete transport-neutral Watch and bitemporal Facts
// boundary. FactAt returns found=false for a valid gap in history.
type WatchService interface {
	Create(context.Context, models.FactSpec) (watchdomain.Watch, bool, error)
	Get(context.Context, string) (watchdomain.Watch, error)
	List(context.Context, watchdomain.WatchListOptions) (watchdomain.WatchPage, error)
	Pause(context.Context, string) (watchdomain.Watch, error)
	Resume(context.Context, string) (watchdomain.Watch, error)
	Delete(context.Context, string) error
	FactAt(context.Context, string, string, time.Time) (watchdomain.Fact, bool, error)
}

// WatchRateLimiter consumes the fixed Watch/Facts charge from the router's
// shared per-identity limiter.
type WatchRateLimiter interface {
	Allow(*gin.Context, int) bool
}

// CreateWatchWithRateLimiter adapts WatchService.Create to POST /watches.
func CreateWatchWithRateLimiter(service WatchService, limiter WatchRateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireWatchService(c, service, false) || !admitWatchRequest(c, limiter, "watch") {
			return
		}
		spec, err := decodeCreateWatchRequest(c)
		if err != nil {
			var maximumBytesError *http.MaxBytesError
			if errors.As(err, &maximumBytesError) {
				respondWatchError(c, http.StatusRequestEntityTooLarge, models.ErrCodeInvalidInput, "watch request is too large")
				return
			}
			respondWatchError(c, http.StatusBadRequest, models.ErrCodeInvalidInput, "invalid watch request")
			return
		}

		ctx := c.Request.Context()
		value, created, err := service.Create(ctx, spec)
		if contextErr := ctx.Err(); contextErr != nil {
			respondWatchContextError(c, contextErr, false)
			return
		}
		if err != nil {
			status, code, message := mapWatchServiceError(err)
			respondWatchError(c, status, code, message)
			return
		}
		projected, err := projectWatch(value)
		if err != nil || projected.Spec != spec || created && !validNewWatch(value) {
			respondWatchError(c, http.StatusInternalServerError, models.ErrCodeInternal, "internal watch failure")
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeWatchPayload(c, ctx, status, models.CreateWatchResponse{Watch: projected, Created: created}, false)
	}
}

// GetWatchWithRateLimiter adapts WatchService.Get to GET /watches/:id.
func GetWatchWithRateLimiter(service WatchService, limiter WatchRateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireWatchService(c, service, false) || !admitWatchRequest(c, limiter, "watch") {
			return
		}
		if err := requireEmptyWatchBody(c); err != nil {
			respondWatchError(c, http.StatusBadRequest, models.ErrCodeInvalidInput, "watch request body must be empty")
			return
		}
		id := c.Param("id")
		if !validWatchUUID(id) {
			respondWatchError(c, http.StatusBadRequest, models.ErrCodeInvalidInput, "invalid watch identifier")
			return
		}
		ctx := c.Request.Context()
		value, err := service.Get(ctx, id)
		writeWatchResult(c, ctx, value, err, id, nil)
	}
}

// ListWatchesWithRateLimiter adapts WatchService.List to GET /watches.
func ListWatchesWithRateLimiter(service WatchService, limiter WatchRateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireWatchService(c, service, false) || !admitWatchRequest(c, limiter, "watch") {
			return
		}
		if err := requireEmptyWatchBody(c); err != nil {
			respondWatchError(c, http.StatusBadRequest, models.ErrCodeInvalidInput, "watch request body must be empty")
			return
		}
		options, err := decodeWatchListOptions(c.Request.URL.RawQuery)
		if err != nil {
			respondWatchError(c, http.StatusBadRequest, models.ErrCodeInvalidInput, "invalid watch cursor")
			return
		}
		frozen := cloneWatchListOptions(options)
		ctx := c.Request.Context()
		page, err := service.List(ctx, cloneWatchListOptions(options))
		if contextErr := ctx.Err(); contextErr != nil {
			respondWatchContextError(c, contextErr, false)
			return
		}
		if err != nil {
			status, code, message := mapWatchServiceError(err)
			respondWatchError(c, status, code, message)
			return
		}
		response, err := projectWatchPage(page, frozen)
		if err != nil {
			respondWatchError(c, http.StatusInternalServerError, models.ErrCodeInternal, "internal watch failure")
			return
		}
		writeWatchPayload(c, ctx, http.StatusOK, response, false)
	}
}

// PauseWatchWithRateLimiter adapts WatchService.Pause to POST /watches/:id/pause.
func PauseWatchWithRateLimiter(service WatchService, limiter WatchRateLimiter) gin.HandlerFunc {
	return mutateWatchWithRateLimiter(
		service,
		limiter,
		func(ctx context.Context, id string) (watchdomain.Watch, error) { return service.Pause(ctx, id) },
		func(state watchdomain.State) bool { return state == watchdomain.StatePaused },
	)
}

// ResumeWatchWithRateLimiter adapts WatchService.Resume to POST /watches/:id/resume.
func ResumeWatchWithRateLimiter(service WatchService, limiter WatchRateLimiter) gin.HandlerFunc {
	return mutateWatchWithRateLimiter(
		service,
		limiter,
		func(ctx context.Context, id string) (watchdomain.Watch, error) { return service.Resume(ctx, id) },
		func(state watchdomain.State) bool {
			return state == watchdomain.StatePending || state == watchdomain.StateActive
		},
	)
}

func mutateWatchWithRateLimiter(
	service WatchService,
	limiter WatchRateLimiter,
	operation func(context.Context, string) (watchdomain.Watch, error),
	stateAllowed func(watchdomain.State) bool,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireWatchService(c, service, false) || !admitWatchRequest(c, limiter, "watch") {
			return
		}
		if err := requireEmptyWatchBody(c); err != nil {
			respondWatchError(c, http.StatusBadRequest, models.ErrCodeInvalidInput, "watch action body must be empty")
			return
		}
		id := c.Param("id")
		if !validWatchUUID(id) {
			respondWatchError(c, http.StatusBadRequest, models.ErrCodeInvalidInput, "invalid watch identifier")
			return
		}
		ctx := c.Request.Context()
		value, err := operation(ctx, id)
		writeWatchResult(c, ctx, value, err, id, stateAllowed)
	}
}

// DeleteWatchWithRateLimiter adapts WatchService.Delete to DELETE /watches/:id.
func DeleteWatchWithRateLimiter(service WatchService, limiter WatchRateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireWatchService(c, service, false) || !admitWatchRequest(c, limiter, "watch") {
			return
		}
		if err := requireEmptyWatchBody(c); err != nil {
			respondWatchError(c, http.StatusBadRequest, models.ErrCodeInvalidInput, "watch action body must be empty")
			return
		}
		id := c.Param("id")
		if !validWatchUUID(id) {
			respondWatchError(c, http.StatusBadRequest, models.ErrCodeInvalidInput, "invalid watch identifier")
			return
		}
		ctx := c.Request.Context()
		err := service.Delete(ctx, id)
		if contextErr := ctx.Err(); contextErr != nil {
			respondWatchContextError(c, contextErr, false)
			return
		}
		if err != nil {
			status, code, message := mapWatchServiceError(err)
			respondWatchError(c, status, code, message)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

func writeWatchResult(
	c *gin.Context,
	ctx context.Context,
	value watchdomain.Watch,
	serviceErr error,
	expectedID string,
	stateAllowed func(watchdomain.State) bool,
) {
	if contextErr := ctx.Err(); contextErr != nil {
		respondWatchContextError(c, contextErr, false)
		return
	}
	if serviceErr != nil {
		status, code, message := mapWatchServiceError(serviceErr)
		respondWatchError(c, status, code, message)
		return
	}
	projected, err := projectWatch(value)
	if err != nil || projected.ID != expectedID || stateAllowed != nil && !stateAllowed(value.State) {
		respondWatchError(c, http.StatusInternalServerError, models.ErrCodeInternal, "internal watch failure")
		return
	}
	writeWatchPayload(c, ctx, http.StatusOK, models.WatchResponse{Watch: projected}, false)
}

func validNewWatch(value watchdomain.Watch) bool {
	return value.State == watchdomain.StatePending && value.NextCheckAt != nil &&
		value.NextCheckAt.Equal(value.CreatedAt) && value.UpdatedAt.Equal(value.CreatedAt) &&
		value.LastChangeAt == nil && value.LastCheckedAt == nil && value.PausedAt == nil &&
		value.ConsecutiveFailures == 0 && value.LastErrorCode == "" &&
		value.LastVerificationID == "" && value.LastVerificationClaimIndex == nil
}

func decodeCreateWatchRequest(c *gin.Context) (models.FactSpec, error) {
	if c == nil || c.Request == nil {
		return models.FactSpec{}, errors.New("missing watch request")
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, models.MaxWatchRequestBytes)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return models.FactSpec{}, err
	}
	if len(body) == 0 || !utf8.Valid(body) {
		return models.FactSpec{}, errors.New("watch request is not one UTF-8 object")
	}
	if err := rejectDuplicateWatchObjectKeys(body); err != nil {
		return models.FactSpec{}, err
	}
	if err := validateCanonicalCreateWatchKeys(body); err != nil {
		return models.FactSpec{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var request *models.CreateWatchRequest
	if err := decoder.Decode(&request); err != nil {
		return models.FactSpec{}, err
	}
	if request == nil {
		return models.FactSpec{}, errors.New("watch request must be an object")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return models.FactSpec{}, errors.New("watch request contains multiple values")
		}
		return models.FactSpec{}, err
	}
	return canonicalWatchSpec(request.Spec)
}

func validateCanonicalCreateWatchKeys(body []byte) error {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(body, &document); err != nil {
		return err
	}
	if len(document) != 1 || document["spec"] == nil {
		return errors.New("watch request has non-canonical fields")
	}
	var spec map[string]json.RawMessage
	if err := json.Unmarshal(document["spec"], &spec); err != nil {
		return err
	}
	for name := range spec {
		switch name {
		case "subject", "predicate", "freshness", "min_independent_sources", "on_conflict":
		default:
			return errors.New("watch specification has non-canonical fields")
		}
	}
	return nil
}

func rejectDuplicateWatchObjectKeys(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := scanWatchJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("watch request contains multiple values")
		}
		return err
	}
	return nil
}

func scanWatchJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := nameToken.(string)
			if !ok {
				return errors.New("watch request has an invalid object name")
			}
			if _, duplicate := seen[name]; duplicate {
				return errors.New("watch request has a duplicate object name")
			}
			seen[name] = struct{}{}
			if err := scanWatchJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return errors.New("watch request has an invalid object")
		}
	case '[':
		for decoder.More() {
			if err := scanWatchJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return errors.New("watch request has an invalid array")
		}
	default:
		return errors.New("watch request has an unexpected delimiter")
	}
	return nil
}

func canonicalWatchSpec(source models.FactSpec) (models.FactSpec, error) {
	source.Defaults()
	if len(source.Subject) > models.MaxAnswerSubjectBytes || !validWatchText(source.Subject, false) {
		return models.FactSpec{}, errors.New("invalid watch subject")
	}
	subject := strings.Join(strings.Fields(strings.TrimSpace(source.Subject)), " ")
	if subject == "" || utf8.RuneCountInString(subject) > models.MaxAnswerSubjectRunes ||
		len(strings.Fields(subject)) > models.MaxAnswerSubjectWords {
		return models.FactSpec{}, errors.New("invalid watch subject")
	}
	if source.Predicate == "" || len(source.Predicate) > models.MaxAnswerPredicateBytes ||
		!validWatchText(source.Predicate, true) || containsWatchSpace(source.Predicate) {
		return models.FactSpec{}, errors.New("invalid watch predicate")
	}
	if source.MinIndependentSources < 1 || source.MinIndependentSources > models.MaxAnswerMinIndependentSources ||
		source.OnConflict != models.FactConflictExpose {
		return models.FactSpec{}, errors.New("invalid watch consensus policy")
	}
	freshness, err := search.ParseFreshness(source.Freshness)
	if err != nil || freshness == search.FreshnessAny {
		return models.FactSpec{}, errors.New("invalid watch freshness")
	}
	var canonicalFreshness string
	switch freshness {
	case search.FreshnessDay:
		canonicalFreshness = "day"
	case search.FreshnessWeek:
		canonicalFreshness = "week"
	case search.FreshnessMonth:
		canonicalFreshness = "month"
	case search.FreshnessYear:
		canonicalFreshness = "year"
	default:
		return models.FactSpec{}, errors.New("invalid watch freshness")
	}
	if utf8.RuneCountInString(subject)+1+utf8.RuneCountInString(source.Predicate) > models.MaxSearchQueryRunes ||
		len(strings.Fields(subject+" "+source.Predicate)) > models.MaxSearchQueryWords {
		return models.FactSpec{}, errors.New("watch query is too large")
	}
	return models.FactSpec{
		Subject: subject, Predicate: strings.Clone(source.Predicate), Freshness: canonicalFreshness,
		MinIndependentSources: source.MinIndependentSources, OnConflict: models.FactConflictExpose,
	}, nil
}

func decodeWatchListOptions(rawQuery string) (watchdomain.WatchListOptions, error) {
	if len(rawQuery) > models.MaxWatchRequestBytes || validateRawWatchQueryNames(rawQuery, true, "limit", "cursor") != nil {
		return watchdomain.WatchListOptions{}, errors.New("invalid watch list query")
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return watchdomain.WatchListOptions{}, err
	}
	for name, entries := range values {
		if name != "limit" && name != "cursor" || len(entries) != 1 {
			return watchdomain.WatchListOptions{}, errors.New("invalid watch list query")
		}
	}
	options := watchdomain.WatchListOptions{}
	if entries, exists := values["limit"]; exists {
		raw := entries[0]
		if raw == "" || raw[0] == '+' || len(raw) > 1 && raw[0] == '0' {
			return options, errors.New("invalid watch list limit")
		}
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > watchdomain.MaxListLimit || strconv.Itoa(limit) != raw {
			return options, errors.New("invalid watch list limit")
		}
		options.Limit = limit
	}
	if entries, exists := values["cursor"]; exists {
		cursor, err := decodeWatchCursor(entries[0])
		if err != nil {
			return options, err
		}
		options.Cursor = &cursor
	}
	return options, nil
}

func validateRawWatchQueryNames(raw string, allowEmpty bool, allowed ...string) error {
	if raw == "" {
		if allowEmpty {
			return nil
		}
		return errors.New("query is empty")
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allowedSet[name] = struct{}{}
	}
	seen := make(map[string]struct{}, len(allowed))
	for _, part := range strings.Split(raw, "&") {
		name, _, found := strings.Cut(part, "=")
		if !found || name == "" {
			return errors.New("query parameter is not canonical")
		}
		if _, ok := allowedSet[name]; !ok {
			return errors.New("query parameter name is not canonical")
		}
		if _, duplicate := seen[name]; duplicate {
			return errors.New("query parameter is repeated")
		}
		seen[name] = struct{}{}
	}
	return nil
}

func encodeWatchCursor(cursor watchdomain.WatchCursor) (string, error) {
	if !validWatchTime(cursor.CreatedAt) || !validWatchUUID(cursor.ID) {
		return "", errors.New("invalid watch cursor")
	}
	payload := "v1\n" + cursor.CreatedAt.Format(time.RFC3339Nano) + "\n" + cursor.ID
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
	if len(encoded) == 0 || len(encoded) > maximumWatchCursorBytes {
		return "", errors.New("invalid watch cursor")
	}
	return encoded, nil
}

func decodeWatchCursor(raw string) (watchdomain.WatchCursor, error) {
	if raw == "" || len(raw) > maximumWatchCursorBytes || strings.Contains(raw, "=") {
		return watchdomain.WatchCursor{}, errors.New("invalid watch cursor")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != raw {
		return watchdomain.WatchCursor{}, errors.New("invalid watch cursor")
	}
	parts := strings.Split(string(decoded), "\n")
	if len(parts) != 3 || parts[0] != "v1" || !utf8.Valid(decoded) {
		return watchdomain.WatchCursor{}, errors.New("invalid watch cursor")
	}
	createdAt, err := parseCanonicalWatchTime(parts[1])
	if err != nil || !validWatchUUID(parts[2]) {
		return watchdomain.WatchCursor{}, errors.New("invalid watch cursor")
	}
	return watchdomain.WatchCursor{CreatedAt: createdAt, ID: strings.Clone(parts[2])}, nil
}

func projectWatchPage(page watchdomain.WatchPage, options watchdomain.WatchListOptions) (models.WatchListResponse, error) {
	limit := options.Limit
	if limit == 0 {
		limit = watchdomain.DefaultListLimit
	}
	if len(page.Items) > limit {
		return models.WatchListResponse{}, errors.New("watch page exceeds requested limit")
	}
	result := models.WatchListResponse{Watches: make([]models.WatchView, len(page.Items))}
	seenIDs := make(map[string]struct{}, len(page.Items))
	seenSpecs := make(map[models.FactSpec]struct{}, len(page.Items))
	for index, item := range page.Items {
		projected, err := projectWatch(item)
		if err != nil {
			return models.WatchListResponse{}, err
		}
		if _, duplicate := seenIDs[projected.ID]; duplicate {
			return models.WatchListResponse{}, errors.New("watch page repeats an identifier")
		}
		if _, duplicate := seenSpecs[projected.Spec]; duplicate {
			return models.WatchListResponse{}, errors.New("watch page repeats a live specification")
		}
		seenIDs[projected.ID] = struct{}{}
		seenSpecs[projected.Spec] = struct{}{}
		if index > 0 && compareWatchPosition(
			page.Items[index-1].CreatedAt, page.Items[index-1].ID, item.CreatedAt, item.ID,
		) >= 0 {
			return models.WatchListResponse{}, errors.New("watch page is not strictly ordered")
		}
		if options.Cursor != nil && compareWatchPosition(
			options.Cursor.CreatedAt, options.Cursor.ID, item.CreatedAt, item.ID,
		) >= 0 {
			return models.WatchListResponse{}, errors.New("watch page did not advance its cursor")
		}
		result.Watches[index] = projected
	}
	if page.Next != nil {
		if len(page.Items) == 0 || len(page.Items) != limit {
			return models.WatchListResponse{}, errors.New("watch page has an impossible continuation")
		}
		last := page.Items[len(page.Items)-1]
		if !page.Next.CreatedAt.Equal(last.CreatedAt) || page.Next.ID != last.ID {
			return models.WatchListResponse{}, errors.New("watch page continuation is not its final item")
		}
		cursor, err := encodeWatchCursor(*page.Next)
		if err != nil {
			return models.WatchListResponse{}, err
		}
		result.NextCursor = cursor
	}
	return result, nil
}

func projectWatch(value watchdomain.Watch) (models.WatchView, error) {
	if !validWatchUUID(value.ID) || !validWatchTime(value.CreatedAt) || !validWatchTime(value.UpdatedAt) ||
		value.UpdatedAt.Before(value.CreatedAt) || value.EWMAInterval < 10*time.Minute ||
		value.EWMAInterval > 7*24*time.Hour || value.ConsecutiveFailures < 0 ||
		int64(value.ConsecutiveFailures) > int64(math.MaxInt32) {
		return models.WatchView{}, errors.New("invalid watch projection")
	}
	spec, err := canonicalWatchSpec(value.Spec)
	if err != nil || spec != value.Spec {
		return models.WatchView{}, errors.New("invalid watch specification projection")
	}
	if value.ConsecutiveFailures == 0 != (value.LastErrorCode == "") ||
		value.LastErrorCode != "" && !validWatchErrorCode(value.LastErrorCode) {
		return models.WatchView{}, errors.New("invalid watch failure projection")
	}
	if value.LastVerificationID == "" != (value.LastVerificationClaimIndex == nil) ||
		value.LastVerificationID != "" && (!validWatchInternalIdentity(value.LastVerificationID) || value.LastCheckedAt == nil) ||
		value.LastVerificationClaimIndex != nil && *value.LastVerificationClaimIndex < 0 {
		return models.WatchView{}, errors.New("invalid watch provenance projection")
	}
	for _, timestamp := range []*time.Time{
		value.NextCheckAt, value.LastChangeAt, value.LastCheckedAt, value.PausedAt,
	} {
		if timestamp != nil && !validWatchTime(*timestamp) {
			return models.WatchView{}, errors.New("invalid watch timestamp projection")
		}
	}
	if value.NextCheckAt != nil && value.NextCheckAt.Before(value.CreatedAt) ||
		value.LastCheckedAt != nil && (value.LastCheckedAt.Before(value.CreatedAt) || value.LastCheckedAt.After(value.UpdatedAt)) ||
		value.LastChangeAt != nil && (value.LastCheckedAt == nil || value.LastChangeAt.Before(value.CreatedAt) || value.LastChangeAt.After(*value.LastCheckedAt)) ||
		value.PausedAt != nil && (value.PausedAt.Before(value.CreatedAt) || value.PausedAt.After(value.UpdatedAt)) {
		return models.WatchView{}, errors.New("inconsistent watch timestamps")
	}
	switch value.State {
	case watchdomain.StatePending, watchdomain.StateActive:
		if value.NextCheckAt == nil || value.PausedAt != nil {
			return models.WatchView{}, errors.New("invalid actionable watch projection")
		}
	case watchdomain.StatePaused:
		if value.NextCheckAt != nil || value.PausedAt == nil {
			return models.WatchView{}, errors.New("invalid paused watch projection")
		}
	default:
		return models.WatchView{}, errors.New("non-live watch projection")
	}
	seconds := float64(value.EWMAInterval) / float64(time.Second)
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 600 || seconds > 604800 {
		return models.WatchView{}, errors.New("invalid watch interval projection")
	}
	return models.WatchView{
		ID: value.ID, Spec: spec, State: string(value.State), NextCheckAt: cloneWatchTime(value.NextCheckAt),
		EWMAIntervalSeconds: seconds, LastChangeAt: cloneWatchTime(value.LastChangeAt),
		LastCheckedAt: cloneWatchTime(value.LastCheckedAt), ConsecutiveFailures: value.ConsecutiveFailures,
		LastErrorCode: strings.Clone(value.LastErrorCode), CreatedAt: value.CreatedAt,
		UpdatedAt: value.UpdatedAt, PausedAt: cloneWatchTime(value.PausedAt),
	}, nil
}

func cloneWatchListOptions(source watchdomain.WatchListOptions) watchdomain.WatchListOptions {
	result := watchdomain.WatchListOptions{Limit: source.Limit}
	if source.Cursor != nil {
		copy := *source.Cursor
		copy.ID = strings.Clone(copy.ID)
		result.Cursor = &copy
	}
	return result
}

func compareWatchPosition(leftTime time.Time, leftID string, rightTime time.Time, rightID string) int {
	if leftTime.Before(rightTime) {
		return -1
	}
	if leftTime.After(rightTime) {
		return 1
	}
	return strings.Compare(leftID, rightID)
}

func requireEmptyWatchBody(c *gin.Context) error {
	if c == nil || c.Request == nil {
		return errors.New("missing watch request")
	}
	if c.Request.Body == nil {
		return nil
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 0)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return err
	}
	if len(body) != 0 {
		return errors.New("watch request body is not empty")
	}
	return nil
}

func admitWatchRequest(c *gin.Context, limiter WatchRateLimiter, resource string) bool {
	if limiter != nil && !limiter.Allow(c, MaxWatchRequestCost) {
		message := "watch rate limited"
		if resource == "fact" {
			message = "facts rate limited"
		}
		respondWatchError(c, http.StatusTooManyRequests, models.ErrCodeRateLimited, message)
		return false
	}
	return true
}

func requireWatchService(c *gin.Context, service WatchService, fact bool) bool {
	if !isNilWatchService(service) {
		return true
	}
	if fact {
		respondWatchError(c, http.StatusServiceUnavailable, models.ErrCodeFactUnavailable, "facts are unavailable")
	} else {
		respondWatchError(c, http.StatusServiceUnavailable, models.ErrCodeWatchUnavailable, "watch is unavailable")
	}
	return false
}

func isNilWatchService(service WatchService) bool {
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

func writeWatchPayload(c *gin.Context, ctx context.Context, status int, payload any, fact bool) {
	encoded, err := encodeWatchPayload(ctx, payload)
	if err != nil {
		if contextErr := contextError(ctx, err); contextErr != nil {
			respondWatchContextError(c, contextErr, fact)
			return
		}
		message := "internal watch failure"
		if fact {
			message = "internal fact failure"
		}
		respondWatchError(c, http.StatusInternalServerError, models.ErrCodeInternal, message)
		return
	}
	c.Data(status, "application/json; charset=utf-8", encoded)
}

func encodeWatchPayload(ctx context.Context, payload any) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("missing watch response context")
	}
	select {
	case watchResponseEncodingSlots <- struct{}{}:
		defer func() { <-watchResponseEncodingSlots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(payload)
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if err != nil {
		return nil, err
	}
	if len(encoded) == 0 || len(encoded) > models.MaxWatchResponseBytes {
		return nil, errors.New("watch response exceeds its output budget")
	}
	return encoded, nil
}

func mapWatchServiceError(err error) (int, string, string) {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, models.ErrCodeTimeout, "watch request timed out"
	case errors.Is(err, watchdomain.ErrInvalidWatchSpec), errors.Is(err, watchdomain.ErrInvalidWatchID),
		errors.Is(err, watchdomain.ErrInvalidCursor):
		return http.StatusBadRequest, models.ErrCodeInvalidInput, "invalid watch request"
	case errors.Is(err, watchdomain.ErrWatchNotFound):
		return http.StatusNotFound, models.ErrCodeWatchNotFound, "watch not found"
	case errors.Is(err, watchdomain.ErrWatchLimit):
		return http.StatusConflict, models.ErrCodeWatchLimitReached, "watch limit reached"
	case errors.Is(err, watchdomain.ErrInvalidStore), errors.Is(err, ledger.ErrClosed):
		return http.StatusServiceUnavailable, models.ErrCodeWatchUnavailable, "watch is unavailable"
	default:
		return http.StatusInternalServerError, models.ErrCodeInternal, "internal watch failure"
	}
}

func respondWatchContextError(c *gin.Context, _ error, fact bool) {
	message := "watch request timed out"
	if fact {
		message = "fact query timed out"
	}
	respondWatchError(c, http.StatusGatewayTimeout, models.ErrCodeTimeout, message)
}

func respondWatchError(c *gin.Context, status int, code, message string) {
	c.JSON(status, models.WatchErrorResponse{Error: &models.ErrorDetail{Code: code, Message: message}})
}

func contextError(ctx context.Context, err error) error {
	if ctx != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

func parseCanonicalWatchTime(raw string) (time.Time, error) {
	if raw == "" || !strings.HasSuffix(raw, "Z") || !utf8.ValidString(raw) {
		return time.Time{}, errors.New("invalid UTC timestamp")
	}
	value, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || !validWatchTime(value) || value.Format(time.RFC3339Nano) != raw {
		return time.Time{}, errors.New("invalid UTC timestamp")
	}
	return value, nil
}

func validWatchTime(value time.Time) bool {
	if value.IsZero() || value.Year() < 1 || value.Year() > 9999 || value.Location() != time.UTC || value != value.Round(0) {
		return false
	}
	_, err := value.MarshalJSON()
	return err == nil
}

func validWatchUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' ||
		value[14] != '4' || !strings.ContainsRune("89ab", rune(value[19])) {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}

func validWatchHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' && character < 'a' || character > 'f' {
			return false
		}
	}
	return true
}

func validWatchText(value string, requireTrimmed bool) bool {
	if !utf8.ValidString(value) || containsWatchControl(value) {
		return false
	}
	return !requireTrimmed || strings.TrimSpace(value) == value
}

func containsWatchControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}

func containsWatchSpace(value string) bool {
	for _, character := range value {
		if unicode.IsSpace(character) {
			return true
		}
	}
	return false
}

func validWatchErrorCode(value string) bool {
	if len(value) == 0 || len(value) > maximumWatchErrorBytes || strings.TrimSpace(value) != value ||
		strings.ToUpper(value) != value || value[0] < 'A' || value[0] > 'Z' {
		return false
	}
	for _, character := range value {
		if character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' {
			continue
		}
		return false
	}
	return true
}

func validWatchInternalIdentity(value string) bool {
	return len(value) >= 1 && len(value) <= 512 && validWatchText(value, true)
}

func cloneWatchTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
