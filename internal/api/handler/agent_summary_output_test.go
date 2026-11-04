package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLegacySummaryOutputChatAndHistory(t *testing.T) {
	const raw = `{"content":"## Progress\nReady for testing [1].","citations":[]}`
	const body = "## Progress\nReady for testing [1]."
	for _, mode := range []string{"off", "on"} {
		for _, profile := range []string{"summary", "summary_refine", "chat"} {
			for _, endpoint := range []string{"/api/v1/agent/chat", "/api/v1/agent/chat/stream"} {
				t.Run(fmt.Sprintf("%s/%s/%s", mode, profile, endpoint), func(t *testing.T) {
					t.Setenv("AGENT_SUMMARY_V2_MODE", mode)
					h := newTestAgentChatHandler(raw)
					r := setupAgentChatRouter(h)
					want := body
					if profile == "chat" {
						want = raw
					}
					requestBody := fmt.Sprintf(`{"message":"Rewrite the summary","session_id":"legacy-output","profile":%q}`, profile)
					w := httptest.NewRecorder()
					req := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(requestBody))
					req.Header.Set("Content-Type", "application/json")
					r.ServeHTTP(w, req)
					if w.Code != http.StatusOK {
						t.Fatalf("chat status=%d body=%s", w.Code, w.Body.String())
					}
					var got string
					if strings.HasSuffix(endpoint, "/stream") {
						for _, frame := range strings.Split(w.Body.String(), "\n\n") {
							if strings.HasPrefix(frame, "event: done\ndata: ") {
								var done struct {
									Reply string `json:"reply"`
								}
								if err := json.Unmarshal([]byte(strings.TrimPrefix(frame, "event: done\ndata: ")), &done); err != nil {
									t.Fatal(err)
								}
								got = done.Reply
							}
						}
					} else {
						var response struct {
							Data struct {
								Reply string `json:"reply"`
							} `json:"data"`
						}
						if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
							t.Fatal(err)
						}
						got = response.Data.Reply
					}
					if got != want {
						t.Fatalf("reply=%q, want %q", got, want)
					}

					history, err := h.store.LoadHistory(context.Background(), "legacy-output", testAgentChatUID)
					if err != nil {
						t.Fatal(err)
					}
					if len(history) != 2 || history[1].Content != want {
						t.Fatalf("persisted messages=%+v", history)
					}
					w = httptest.NewRecorder()
					r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/agent/chat/history?session_id=legacy-output", nil))
					var restored struct {
						Data struct {
							Messages []historyBubble `json:"messages"`
						} `json:"data"`
					}
					if err := json.Unmarshal(w.Body.Bytes(), &restored); err != nil {
						t.Fatal(err)
					}
					if w.Code != http.StatusOK || len(restored.Data.Messages) != 2 || restored.Data.Messages[1].Content != want {
						t.Fatalf("restored history=%s", w.Body.String())
					}
				})
			}
		}
	}
}
