package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/use-agent/purify/models"
)

func TestSearchAuthUsesSearchEnvelopeForMissingAndInvalidKeys(t *testing.T) {
	gin.SetMode(gin.TestMode)
	calls := 0
	router := gin.New()
	router.POST("/search", SearchAuth([]string{"required-secret"}), func(c *gin.Context) {
		calls++
		c.Status(http.StatusNoContent)
	})

	tests := []struct {
		name   string
		header string
	}{
		{name: "missing"},
		{name: "invalid", header: "wrong-secret"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/search", nil)
			if test.header != "" {
				request.Header.Set("X-API-Key", test.header)
			}
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body)
			}
			var response models.SearchResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Success || response.Results == nil || response.Error == nil || response.Error.Code != models.ErrCodeUnauthorized {
				t.Fatalf("response = %#v", response)
			}
		})
	}
	if calls != 0 {
		t.Fatalf("protected handler calls = %d, want zero", calls)
	}
}

func TestSearchAuthAcceptsBothHeaderFormsAndSetsIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/search", SearchAuth([]string{"required-secret"}), func(c *gin.Context) {
		identity, exists := c.Get("api_key")
		if !exists || identity != "required-secret" {
			c.Status(http.StatusInternalServerError)
			return
		}
		c.Status(http.StatusNoContent)
	})

	tests := []struct {
		name        string
		header      string
		headerValue string
	}{
		{name: "X API key", header: "X-API-Key", headerValue: "required-secret"},
		{name: "bearer", header: "Authorization", headerValue: "Bearer required-secret"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/search", nil)
			request.Header.Set(test.header, test.headerValue)
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusNoContent {
				t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body)
			}
		})
	}
}

func TestSearchAuthEmptyEffectiveKeySetIsInertForFailClosedRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, keys := range [][]string{nil, {}, {"", " \t"}} {
		calls := 0
		router := gin.New()
		router.POST("/search", SearchAuth(keys), func(c *gin.Context) {
			calls++
			c.Status(http.StatusServiceUnavailable)
		})
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/search", nil))
		if recorder.Code != http.StatusServiceUnavailable || calls != 1 {
			t.Fatalf("keys=%#v status/calls = %d/%d", keys, recorder.Code, calls)
		}
	}
}

func TestStandardAuthKeepsLegacyScrapeResponseEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/extract", Auth([]string{"required-secret"}), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/extract", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if _, exists := document["results"]; exists {
		t.Fatalf("legacy auth response unexpectedly changed to Search envelope: %s", recorder.Body)
	}
	if string(document["success"]) != "false" {
		t.Fatalf("legacy auth response = %s", recorder.Body)
	}
}
