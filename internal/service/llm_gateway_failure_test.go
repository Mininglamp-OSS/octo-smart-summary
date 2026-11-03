package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/llmfallback"
)

// Exercise a real Map HTTP request: an actionable gateway rejection must not
// be swallowed by the historical partial-chunk marker fallback.
func TestMapGatewayQuotaAndTimeout(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"quota", 403, `{"error":{"code":"insufficient_quota"}}`, llmfallback.ErrGatewayQuota},
		{"denied without quota", 403, `{"error":"forbidden"}`, nil},
		{"deadline", 408, `{"error":"upstream timeout"}`, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer gateway.Close()
			client := NewLLMClient(gateway.URL, "test", "mock", 1, 64, false, 1, nil)
			content, _, _, err := client.CallMapStreamWithModel(context.Background(), "chat message", "group", 0, 1, "start", "end", "summary", "user", nil)
			if !errors.Is(err, tc.want) || (tc.want == nil && content == "") {
				t.Fatalf("content=%q err=%v want=%v", content, err, tc.want)
			}
		})
	}
}
