package llmfallback

import (
	"context"
	"errors"
	"strings"
)

// ActionableGatewayFailure preserves only actionable failures through Map's usual
// partial-chunk fallback. A generic 403 or a rate limit is not a quota diagnosis.
func ActionableGatewayFailure(err error) error {
	var upstream *HTTPError
	if errors.As(err, &upstream) {
		body := strings.ToLower(upstream.Error())
		switch upstream.StatusCode {
		case 402, 403, 429:
			if strings.Contains(body, "insufficient_quota") || strings.Contains(body, "额度不足") || strings.Contains(body, "余额不足") {
				return ErrGatewayQuota
			}
		case 504, 408:
			return context.DeadlineExceeded
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}

var ErrGatewayQuota = errors.New("summary gateway quota exhausted")

// HTTPError preserves the upstream status through wrapping and fallback. API
// boundaries can distinguish deterministic rejection from transient failures
// without parsing localized or provider-authored error strings.
type HTTPError struct {
	StatusCode int
	Err        error
}

func (e *HTTPError) Error() string { return e.Err.Error() }
func (e *HTTPError) Unwrap() error { return e.Err }
