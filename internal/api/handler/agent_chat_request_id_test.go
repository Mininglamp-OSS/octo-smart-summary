package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRequestIDPattern covers the VARCHAR(128) backstop for the optional V2
// idempotency key: request_id flows into agent_summary_run uk_run_request's
// VARCHAR(128) column. Strict-mode MySQL rejects overlong values; the handler
// gate gives clients a stable 400 before persistence is attempted.
func TestRequestIDPattern(t *testing.T) {
	valid := []string{
		"req-1",
		"A_b-9",
		strings.Repeat("a", 128), // exactly the column width
	}
	for _, v := range valid {
		if !requestIDPattern.MatchString(v) {
			t.Errorf("requestIDPattern should accept %q", v[:min(20, len(v))])
		}
	}

	invalid := []string{
		strings.Repeat("a", 129), // would be truncated by VARCHAR(128)
		"req 1",                  // space
		"req;drop",               // quote/injection-ish chars
		"",                       // empty never reaches the gate (optional field)
	}
	for _, v := range invalid {
		if requestIDPattern.MatchString(v) {
			t.Errorf("requestIDPattern should reject %q", v[:min(20, len(v))])
		}
	}
}

func TestRequestIDValidationRespectsV2Flag(t *testing.T) {
	invalid := "mobile:retry/1"
	for _, mode := range []struct {
		name       string
		value      string
		wantStatus int
	}{
		{name: "off accepts and ignores new field", value: "off", wantStatus: http.StatusOK},
		{name: "on validates persisted key", value: "on", wantStatus: http.StatusBadRequest},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Setenv("AGENT_SUMMARY_V2_MODE", mode.value)
			h := newTestAgentChatHandler("ok")
			r := setupAgentChatRouter(h)
			for _, path := range []string{"/api/v1/agent/chat", "/api/v1/agent/chat/stream"} {
				body := strings.NewReader(`{"message":"hello","session_id":"request-id-flag","request_id":"` + invalid + `"}`)
				w := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, path, body)
				req.Header.Set("Content-Type", "application/json")
				r.ServeHTTP(w, req)
				if w.Code != mode.wantStatus {
					t.Fatalf("%s status=%d body=%s, want %d", path, w.Code, w.Body.String(), mode.wantStatus)
				}
			}
		})
	}
}
