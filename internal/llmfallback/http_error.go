package llmfallback

// HTTPError preserves the upstream status through wrapping and fallback. API
// boundaries can distinguish deterministic rejection from transient failures
// without parsing localized or provider-authored error strings.
type HTTPError struct {
	StatusCode int
	Err        error
}

func (e *HTTPError) Error() string { return e.Err.Error() }
func (e *HTTPError) Unwrap() error { return e.Err }
