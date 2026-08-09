package models

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFactSpecDefaultsAndAnswerUnknownJSONShape(t *testing.T) {
	spec := FactSpec{}
	spec.Defaults()
	if spec.Freshness != DefaultAnswerFreshness || spec.MinIndependentSources != DefaultAnswerMinIndependentSources ||
		spec.OnConflict != FactConflictExpose {
		t.Fatalf("FactSpec.Defaults() = %#v", spec)
	}
	request := AnswerRequest{}
	request.Defaults()
	if request.Timeout != DefaultAnswerTimeoutSeconds || request.Spec.Freshness != DefaultAnswerFreshness ||
		request.Spec.MinIndependentSources != DefaultAnswerMinIndependentSources || request.Spec.OnConflict != FactConflictExpose {
		t.Fatalf("AnswerRequest.Defaults() = %#v", request)
	}

	response := AnswerResponse{
		Status: AnswerStatusUnknown,
		Belief: nil,
		Reason: AnswerUnknownInsufficient,
		Needs:  &AnswerNeeds{MoreIndependentSources: 1},
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if !strings.Contains(string(encoded), `"status":"unknown"`) || !strings.Contains(string(encoded), `"belief":null`) ||
		strings.Contains(string(encoded), "calibrat") {
		t.Fatalf("encoded response = %s", encoded)
	}
	errorEncoded, err := json.Marshal(AnswerErrorResponse{Error: &ErrorDetail{Code: ErrCodeAnswerFailed, Message: "answer failed"}})
	if err != nil || !strings.Contains(string(errorEncoded), `"code":"ANSWER_FAILED"`) {
		t.Fatalf("encoded error response = %s, %v", errorEncoded, err)
	}
}
