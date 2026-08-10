package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/use-agent/purify/ledger"
	"github.com/use-agent/purify/models"
	"github.com/use-agent/purify/publicnet"
	watchdomain "github.com/use-agent/purify/watch"
	"golang.org/x/net/publicsuffix"
)

// GetFactAtWithRateLimiter adapts the half-open bitemporal lookup to
// GET /facts?subject&predicate&as_of. A valid gap is a successful null fact.
func GetFactAtWithRateLimiter(service WatchService, limiter WatchRateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireWatchService(c, service, true) || !admitWatchRequest(c, limiter, "fact") {
			return
		}
		if err := requireEmptyWatchBody(c); err != nil {
			respondWatchError(c, http.StatusBadRequest, models.ErrCodeInvalidInput, "fact query body must be empty")
			return
		}
		subject, predicate, asOf, err := decodeFactQuery(c.Request.URL.RawQuery)
		if err != nil {
			respondWatchError(c, http.StatusBadRequest, models.ErrCodeInvalidInput, "invalid fact query")
			return
		}

		ctx := c.Request.Context()
		fact, found, err := service.FactAt(ctx, subject, predicate, asOf)
		if contextErr := ctx.Err(); contextErr != nil {
			respondWatchContextError(c, contextErr, true)
			return
		}
		if err != nil {
			status, code, message := mapFactServiceError(err)
			respondWatchError(c, status, code, message)
			return
		}
		response := models.FactLookupResponse{Fact: nil, AsOf: asOf}
		if !found {
			if !reflect.DeepEqual(fact, watchdomain.Fact{}) {
				respondWatchError(c, http.StatusInternalServerError, models.ErrCodeInternal, "internal fact failure")
				return
			}
			writeWatchPayload(c, ctx, http.StatusOK, response, true)
			return
		}
		projected, err := projectFactAt(fact, subject, predicate, asOf)
		if err != nil {
			respondWatchError(c, http.StatusInternalServerError, models.ErrCodeInternal, "internal fact failure")
			return
		}
		response.Fact = &projected
		writeWatchPayload(c, ctx, http.StatusOK, response, true)
	}
}

func decodeFactQuery(rawQuery string) (string, string, time.Time, error) {
	if len(rawQuery) > models.MaxWatchRequestBytes ||
		validateRawWatchQueryNames(rawQuery, false, "subject", "predicate", "as_of") != nil {
		return "", "", time.Time{}, errors.New("fact query is not canonical")
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", "", time.Time{}, err
	}
	for name, entries := range values {
		switch name {
		case "subject", "predicate", "as_of":
		default:
			return "", "", time.Time{}, errors.New("fact query has an unknown parameter")
		}
		if len(entries) != 1 || entries[0] == "" {
			return "", "", time.Time{}, errors.New("fact query parameter is missing or repeated")
		}
	}
	if len(values) != 3 {
		return "", "", time.Time{}, errors.New("fact query is incomplete")
	}
	subject := values.Get("subject")
	predicate := values.Get("predicate")
	canonical, err := canonicalWatchSpec(models.FactSpec{
		Subject: subject, Predicate: predicate, Freshness: "day",
		MinIndependentSources: 1, OnConflict: models.FactConflictExpose,
	})
	if err != nil || canonical.Subject != subject || canonical.Predicate != predicate {
		return "", "", time.Time{}, errors.New("fact identity is not canonical")
	}
	asOf, err := parseCanonicalWatchTime(values.Get("as_of"))
	if err != nil {
		return "", "", time.Time{}, err
	}
	return strings.Clone(subject), strings.Clone(predicate), asOf, nil
}

func projectFactAt(value watchdomain.Fact, subject, predicate string, asOf time.Time) (models.FactView, error) {
	if !validWatchHex(value.ID, 64) || !validWatchUUID(value.WatchID) || value.Subject != subject ||
		value.Predicate != predicate || value.Path == "" || len(value.Path) > maximumWatchPathBytes ||
		!validWatchText(value.Path, true) || len(value.Value) == 0 || len(value.Value) > maximumWatchFactValue ||
		!validWatchScalar(value.Value) || value.Root == "" || len(value.Root) > 253 ||
		value.Root != strings.ToLower(value.Root) || !validWatchText(value.Root, true) ||
		len(value.SourceURL) == 0 || len(value.SourceURL) > models.MaxSearchURLBytes ||
		len(value.Receipt) == 0 || len(value.Receipt) > maximumWatchReceipt || !validWatchText(value.Receipt, true) ||
		len(value.SnapshotID) != len("sha256:")+64 || !strings.HasPrefix(value.SnapshotID, "sha256:") ||
		!validWatchHex(strings.TrimPrefix(value.SnapshotID, "sha256:"), 64) {
		return models.FactView{}, errors.New("invalid fact projection")
	}
	canonical, parsed, err := publicnet.NormalizeHTTPURL(value.SourceURL, nil, false)
	if err != nil || parsed == nil || parsed.User != nil || canonical != value.SourceURL {
		return models.FactView{}, errors.New("invalid fact source URL")
	}
	root, err := watchFactRoot(parsed.Hostname())
	if err != nil || root != value.Root {
		return models.FactView{}, errors.New("invalid fact source root")
	}
	if !validWatchInternalIdentity(value.CreatedVerificationID) || value.CreatedClaimIndex < 0 ||
		!validWatchInternalIdentity(value.LatestVerificationID) || value.LatestClaimIndex < 0 ||
		!validWatchTime(value.ObservedAt) || !validWatchTime(value.ValidFrom) ||
		!validWatchTime(value.LastVerifiedAt) || !value.ObservedAt.Equal(value.ValidFrom) ||
		value.LastVerifiedAt.Before(value.ObservedAt) {
		return models.FactView{}, errors.New("invalid fact provenance")
	}
	if value.ValidTo != nil && (!validWatchTime(*value.ValidTo) || !value.ValidTo.After(value.ValidFrom) ||
		value.ValidTo.Before(value.LastVerifiedAt)) {
		return models.FactView{}, errors.New("invalid fact validity interval")
	}
	if value.ClosedVerificationID == "" != (value.ClosedClaimIndex == nil) ||
		value.ClosedVerificationID != "" && !validWatchInternalIdentity(value.ClosedVerificationID) ||
		value.ClosedClaimIndex != nil && *value.ClosedClaimIndex < 0 {
		return models.FactView{}, errors.New("invalid fact closure provenance")
	}
	if value.ValidTo == nil {
		if value.ClosedVerificationID != "" || value.ClosedClaimIndex != nil || value.SupersededBy != "" ||
			value.ClosedOutcome != "" || value.GoneScope != "" {
			return models.FactView{}, errors.New("open fact contains closure fields")
		}
	} else {
		switch value.ClosedOutcome {
		case ledger.OutcomeChanged:
			if value.ClosedVerificationID == "" || !validWatchHex(value.SupersededBy, 64) ||
				value.SupersededBy == value.ID || value.GoneScope != "" {
				return models.FactView{}, errors.New("invalid changed fact closure")
			}
		case ledger.OutcomeGone:
			if value.ClosedVerificationID == "" || value.SupersededBy != "" ||
				value.GoneScope != ledger.GoneScopeField && value.GoneScope != ledger.GoneScopePage {
				return models.FactView{}, errors.New("invalid gone fact closure")
			}
		default:
			return models.FactView{}, errors.New("invalid fact closure outcome")
		}
	}
	if value.ValidFrom.After(asOf) || value.ValidTo != nil && !value.ValidTo.After(asOf) {
		return models.FactView{}, errors.New("fact does not contain requested time")
	}
	return models.FactView{
		ID: value.ID, WatchID: value.WatchID, Subject: strings.Clone(value.Subject),
		Predicate: strings.Clone(value.Predicate), Path: strings.Clone(value.Path),
		Value: append(json.RawMessage(nil), value.Value...), Root: strings.Clone(value.Root),
		SourceURL: strings.Clone(value.SourceURL), Receipt: strings.Clone(value.Receipt),
		SnapshotID: strings.Clone(value.SnapshotID), ObservedAt: value.ObservedAt,
		ValidFrom: value.ValidFrom, LastVerifiedAt: value.LastVerifiedAt,
		ValidTo: cloneWatchTime(value.ValidTo), SupersededBy: strings.Clone(value.SupersededBy),
		ClosedOutcome: strings.Clone(string(value.ClosedOutcome)), GoneScope: strings.Clone(string(value.GoneScope)),
	}, nil
}

func validWatchScalar(raw json.RawMessage) bool {
	if len(raw) == 0 || !utf8.Valid(raw) || !json.Valid(raw) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return false
	}
	switch scalar := value.(type) {
	case string:
		return bytes.Equal(raw, canonicalWatchJSONString(scalar))
	case json.Number:
		return string(raw) == scalar.String() && validWatchNumber(scalar.String())
	case bool:
		if scalar {
			return bytes.Equal(raw, []byte("true"))
		}
		return bytes.Equal(raw, []byte("false"))
	default:
		return false
	}
}

func canonicalWatchJSONString(value string) []byte {
	output := make([]byte, 0, len(value)+2)
	output = append(output, '"')
	for _, character := range value {
		switch character {
		case '"', '\\':
			output = append(output, '\\', byte(character))
		case '\b':
			output = append(output, `\b`...)
		case '\f':
			output = append(output, `\f`...)
		case '\n':
			output = append(output, `\n`...)
		case '\r':
			output = append(output, `\r`...)
		case '\t':
			output = append(output, `\t`...)
		default:
			if character < 0x20 {
				const hexadecimal = "0123456789abcdef"
				output = append(output, '\\', 'u', '0', '0', hexadecimal[character>>4], hexadecimal[character&0xf])
				continue
			}
			output = utf8.AppendRune(output, character)
		}
	}
	return append(output, '"')
}

func validWatchNumber(value string) bool {
	if value == "0" {
		return true
	}
	if value == "" || value == "-0" || strings.ContainsAny(value, "eE") {
		return false
	}
	if dot := strings.IndexByte(value, '.'); dot >= 0 {
		return dot+1 < len(value) && value[len(value)-1] != '0'
	}
	return true
}

func watchFactRoot(hostname string) (string, error) {
	hostname = strings.ToLower(hostname)
	if address, err := netip.ParseAddr(hostname); err == nil {
		return address.Unmap().String(), nil
	}
	root, err := publicsuffix.EffectiveTLDPlusOne(hostname)
	if err != nil {
		return "", err
	}
	return strings.ToLower(root), nil
}

func mapFactServiceError(err error) (int, string, string) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, models.ErrCodeTimeout, "fact query timed out"
	}
	if errors.Is(err, watchdomain.ErrInvalidWatchSpec) || errors.Is(err, watchdomain.ErrInvalidCursor) ||
		errors.Is(err, watchdomain.ErrInvalidWatchID) {
		return http.StatusBadRequest, models.ErrCodeInvalidInput, "invalid fact query"
	}
	if errors.Is(err, watchdomain.ErrInvalidStore) || errors.Is(err, ledger.ErrClosed) {
		return http.StatusServiceUnavailable, models.ErrCodeFactUnavailable, "facts are unavailable"
	}
	return http.StatusInternalServerError, models.ErrCodeInternal, "internal fact failure"
}
