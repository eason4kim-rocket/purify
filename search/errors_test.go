package search

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type providerSecretCause struct {
	Secret string `json:"secret"`
}

func (cause providerSecretCause) Error() string { return "private provider cause" }

func TestProviderErrorKindsReturnStableSanitizedMessages(t *testing.T) {
	cause := errors.New("brave secret-token response body: private socket detail")
	tests := []struct {
		kind ProviderErrorKind
		want string
	}{
		{kind: ProviderErrorAuthentication, want: "search provider authentication failed"},
		{kind: ProviderErrorRateLimited, want: "search provider rate limited"},
		{kind: ProviderErrorTimeout, want: "search provider timed out"},
		{kind: ProviderErrorUpstream, want: "search provider upstream failed"},
		{kind: ProviderErrorInvalidResponse, want: "search provider returned an invalid response"},
		{kind: ProviderErrorKind(255), want: "search provider failed"},
	}
	for _, test := range tests {
		failure := NewProviderError(test.kind, 599, cause)
		if failure.Kind != test.kind || failure.StatusCode != 599 || failure.Err != cause {
			t.Fatalf("NewProviderError() = %#v", failure)
		}
		if got := failure.Error(); got != test.want {
			t.Fatalf("ProviderError(%d).Error() = %q, want %q", test.kind, got, test.want)
		}
		for _, secret := range []string{"brave", "secret-token", "response body", "private socket", "599"} {
			if strings.Contains(failure.Error(), secret) {
				t.Fatalf("public-safe error %q leaked %q", failure, secret)
			}
		}
		if !errors.Is(failure, cause) {
			t.Fatal("ProviderError does not unwrap its internal diagnostic cause")
		}
		var typed *ProviderError
		if !errors.As(failure, &typed) || typed != failure {
			t.Fatalf("errors.As() = %#v", typed)
		}
	}
}

func TestProviderErrorNilAndNilCauseAreSafe(t *testing.T) {
	var failure *ProviderError
	if got := failure.Error(); got != "search provider failed" || failure.Unwrap() != nil {
		t.Fatalf("nil ProviderError = %q, unwrap=%v", got, failure.Unwrap())
	}

	withoutCause := NewProviderError(ProviderErrorUpstream, 0, nil)
	if withoutCause.Unwrap() != nil || fmt.Sprint(withoutCause) != "search provider upstream failed" {
		t.Fatalf("cause-free ProviderError = %#v", withoutCause)
	}
}

func TestProviderErrorJSONNeverSerializesDiagnostics(t *testing.T) {
	failure := NewProviderError(ProviderErrorAuthentication, 401, providerSecretCause{Secret: "provider-secret-token"})
	encoded, err := json.Marshal(failure)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{}` || strings.Contains(string(encoded), "401") ||
		strings.Contains(string(encoded), "provider-secret-token") {
		t.Fatalf("ProviderError JSON leaked diagnostics: %s", encoded)
	}
}
