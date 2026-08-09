package search

// ProviderErrorKind is the complete internal provider failure taxonomy. It is
// deliberately numeric so provider-specific labels cannot leak through JSON.
type ProviderErrorKind uint8

const (
	ProviderErrorAuthentication ProviderErrorKind = iota + 1
	ProviderErrorRateLimited
	ProviderErrorTimeout
	ProviderErrorUpstream
	ProviderErrorInvalidResponse
)

// ProviderError retains a typed failure and optional upstream status for
// internal mapping. Error returns a fixed sanitized string; StatusCode and Err
// must never be copied directly into a public ErrorDetail.
type ProviderError struct {
	Kind       ProviderErrorKind `json:"-"`
	StatusCode int               `json:"-"`
	Err        error             `json:"-"`
}

// NewProviderError constructs a provider failure without formatting its cause.
func NewProviderError(kind ProviderErrorKind, statusCode int, cause error) *ProviderError {
	return &ProviderError{Kind: kind, StatusCode: statusCode, Err: cause}
}

func (failure *ProviderError) Error() string {
	if failure == nil {
		return "search provider failed"
	}
	switch failure.Kind {
	case ProviderErrorAuthentication:
		return "search provider authentication failed"
	case ProviderErrorRateLimited:
		return "search provider rate limited"
	case ProviderErrorTimeout:
		return "search provider timed out"
	case ProviderErrorUpstream:
		return "search provider upstream failed"
	case ProviderErrorInvalidResponse:
		return "search provider returned an invalid response"
	default:
		return "search provider failed"
	}
}

func (failure *ProviderError) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.Err
}
