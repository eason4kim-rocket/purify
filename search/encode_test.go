package search

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/use-agent/purify/models"
)

func TestEncodeSearchResponseMatchesCanonicalJSON(t *testing.T) {
	service, err := NewService(&stubSearchProvider{name: "stub"})
	if err != nil {
		t.Fatal(err)
	}
	response := &models.SearchResponse{
		Success: true, Query: "q", Results: []models.SearchResult{},
	}
	want, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	got, err := service.EncodeSearchResponse(context.Background(), response)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("EncodeSearchResponse() = %s, %v; want %s", got, err, want)
	}
}

func TestEncodeSearchResponseExactWholeResponseBudget(t *testing.T) {
	service, _ := NewService(&stubSearchProvider{name: "stub"})

	t.Run("exact limit succeeds", func(t *testing.T) {
		response := exactSizedSearchResponse(t, models.MaxSearchResponseBytes)
		encoded, err := service.EncodeSearchResponse(context.Background(), response)
		if err != nil || len(encoded) != models.MaxSearchResponseBytes {
			t.Fatalf("EncodeSearchResponse() bytes/error = %d/%v", len(encoded), err)
		}
	})

	t.Run("one byte over fails without output", func(t *testing.T) {
		response := exactSizedSearchResponse(t, models.MaxSearchResponseBytes+1)
		encoded, err := service.EncodeSearchResponse(context.Background(), response)
		if encoded != nil {
			t.Fatalf("EncodeSearchResponse() returned %d partial bytes", len(encoded))
		}
		requireSearchErrorCode(t, err, models.ErrCodeInternal)
		if strings.Contains(err.Error(), response.Query[:16]) {
			t.Fatalf("encoding error leaked response data: %v", err)
		}
	})
}

func TestEncodeSearchResponseUsesSharedFourSlotsAndHonorsContext(t *testing.T) {
	service, _ := NewService(&stubSearchProvider{name: "stub"})
	response := &models.SearchResponse{Success: true, Results: []models.SearchResult{}}
	entered := make(chan struct{}, 5)
	release := make(chan struct{})
	service.encodeSearch = func(value any) ([]byte, error) {
		entered <- struct{}{}
		<-release
		return json.Marshal(value)
	}
	errorsByCall := make(chan error, 5)
	for range 5 {
		go func() {
			_, err := service.EncodeSearchResponse(context.Background(), response)
			errorsByCall <- err
		}()
	}
	for range defaultSearchEncodingSlots {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("four response encoders did not enter")
		}
	}
	select {
	case <-entered:
		t.Fatal("fifth response encoder entered before a shared slot was released")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	for range 5 {
		if err := <-errorsByCall; err != nil {
			t.Fatalf("EncodeSearchResponse() error = %v", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	service.encodeSearch = func(any) ([]byte, error) {
		cancel()
		return nil, errors.New("private encoding error")
	}
	encoded, err := service.EncodeSearchResponse(ctx, response)
	if encoded != nil {
		t.Fatalf("canceled encoder returned %d bytes", len(encoded))
	}
	requireSearchErrorCode(t, err, models.ErrCodeTimeout)
	if strings.Contains(err.Error(), "private encoding error") {
		t.Fatalf("encoding error leaked private cause: %v", err)
	}
}

func TestEncodeSearchResponseRecoversPanicsAndRejectsInvalidInputs(t *testing.T) {
	service, _ := NewService(&stubSearchProvider{name: "stub"})
	response := &models.SearchResponse{Success: true, Results: []models.SearchResult{}}
	service.encodeSearch = func(any) ([]byte, error) { panic("private encoding panic") }
	encoded, err := service.EncodeSearchResponse(context.Background(), response)
	if encoded != nil {
		t.Fatalf("panic returned %d bytes", len(encoded))
	}
	requireSearchErrorCode(t, err, models.ErrCodeInternal)
	if strings.Contains(err.Error(), "panic") {
		t.Fatalf("panic detail leaked: %v", err)
	}

	if _, err := service.EncodeSearchResponse(nil, response); err == nil {
		t.Fatal("nil context unexpectedly succeeded")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = service.EncodeSearchResponse(canceled, response)
	requireSearchErrorCode(t, err, models.ErrCodeTimeout)
	_, err = service.EncodeSearchResponse(context.Background(), nil)
	requireSearchErrorCode(t, err, models.ErrCodeInternal)
}

func exactSizedSearchResponse(t *testing.T, target int) *models.SearchResponse {
	t.Helper()
	response := &models.SearchResponse{Success: true, Results: []models.SearchResult{}}
	base, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	padding := target - len(base)
	if padding < 0 {
		t.Fatalf("target %d is below response overhead %d", target, len(base))
	}
	response.Query = strings.Repeat("q", padding)
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != target {
		t.Fatalf("fixture bytes = %d, want %d", len(encoded), target)
	}
	return response
}
