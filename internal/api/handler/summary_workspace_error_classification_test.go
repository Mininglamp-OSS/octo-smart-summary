package handler

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/llmfallback"
)

func TestWorkspaceModelRejectionDoesNotRestartRequest(t *testing.T) {
	for _, err := range []error{
		&agent.InvalidToolArgumentsError{},
		&agent.SummaryDraftError{Reason: "invalid_citations"},
		&agent.SummaryDraftError{Reason: "model_call", Cause: errors.New("network unavailable")},
		&agent.SummaryDraftError{Reason: "model_call", Cause: &llmfallback.HTTPError{StatusCode: 429, Err: errors.New("rate limited")}},
		&agent.SummaryDraftError{Reason: "model_call", Cause: &llmfallback.HTTPError{StatusCode: 503, Err: errors.New("unavailable")}},
		fmt.Errorf("wrapped: %w", &agent.InvalidToolArgumentsError{}),
		&llmfallback.HTTPError{StatusCode: 400, Err: errors.New("private provider details")},
		fmt.Errorf("wrapped: %w", &llmfallback.HTTPError{StatusCode: 401, Err: errors.New("private provider details")}),
		&llmfallback.HTTPError{StatusCode: 403, Err: errors.New("private provider details")},
	} {
		status, code, message, transient, data := classifySummaryWorkspaceServiceError(err, "fallback")
		if status != http.StatusBadGateway || code != 50003 || transient || data != nil || message == err.Error() {
			t.Fatalf("incorrect rejection contract: status=%d code=%d transient=%t", status, code, transient)
		}
	}
}

func TestWorkspaceTransientFailuresRemainRetryable(t *testing.T) {
	for _, err := range []error{
		&llmfallback.HTTPError{StatusCode: 429, Err: errors.New("rate limit")},
		&llmfallback.HTTPError{StatusCode: 503, Err: errors.New("unavailable")},
		errors.New("network unavailable"),
	} {
		status, _, _, transient, _ := classifySummaryWorkspaceServiceError(err, "fallback")
		if status != 500 || !transient {
			t.Fatal("transient failure lost existing recovery")
		}
	}
}
