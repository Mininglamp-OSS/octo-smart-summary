package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// derivePerModelTimeout arms llmfallback.Run's deadline-aware escalation from the
// REMAINING parent deadline (#254 Part B). It must stay inert for the
// deadline-less worker, produce ~remaining/(maxAttempts+1) for a bounded parent
// (API refine ~90s → ~22s), and never exceed one attempt's real cost.
func TestDerivePerModelTimeout(t *testing.T) {
	// timeoutSec = 180 (LLM_TIMEOUT default), so c.timeout = 180s.
	c := NewLLMClient("http://x", "test-fake-key", "m", 180, 4096, false, 30, nil)

	t.Run("no parent deadline: inert (worker path)", func(t *testing.T) {
		if got := c.derivePerModelTimeout(context.Background(), 3); got != 0 {
			t.Fatalf("got %v, want 0 — the guard must stay inert without a deadline", got)
		}
	})

	t.Run("~90s refine budget: remaining/(maxAttempts+1)", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		got := c.derivePerModelTimeout(ctx, 3)
		// 90s / (3+1) = 22.5s; a little elapses, so expect (20s, 22.5s].
		if got <= 20*time.Second || got > 22500*time.Millisecond {
			t.Fatalf("got %v, want ~22.5s in (20s, 22.5s]", got)
		}
		if got >= c.timeout {
			t.Fatalf("got %v, must stay under the per-attempt cap %v", got, c.timeout)
		}
	})

	t.Run("very long budget: capped at the client per-attempt timeout", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		if got := c.derivePerModelTimeout(ctx, 3); got != c.timeout {
			t.Fatalf("got %v, want the cap %v", got, c.timeout)
		}
	})

	t.Run("expired deadline: 0", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), -time.Second)
		defer cancel()
		if got := c.derivePerModelTimeout(ctx, 3); got != 0 {
			t.Fatalf("got %v, want 0 on an already-expired deadline", got)
		}
	})

	t.Run("nonsensical maxAttempts: 0 (no divide-by-zero)", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if got := c.derivePerModelTimeout(ctx, 0); got != 0 {
			t.Fatalf("got %v, want 0", got)
		}
	})
}

func TestPerModelTimeoutWiredIntoPublicCallPaths(t *testing.T) {
	for _, tt := range []struct {
		name string
		call func(context.Context, *LLMClient) (string, int, error)
	}{
		{
			name: "complete",
			call: func(ctx context.Context, c *LLMClient) (string, int, error) {
				return c.Call(ctx, []ChatMessage{{Role: "user", Content: "hello"}}, 0.3)
			},
		},
		{
			name: "stream",
			call: func(ctx context.Context, c *LLMClient) (string, int, error) {
				return c.CallStream(ctx, []ChatMessage{{Role: "user", Content: "hello"}}, 0.3, nil)
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var primaryAttempts atomic.Int32
			var fallbackAttempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Model string `json:"model"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				if body.Model == "primary" {
					primaryAttempts.Add(1)
					http.Error(w, "try fallback", http.StatusInternalServerError)
					return
				}
				fallbackAttempts.Add(1)
				if tt.name == "stream" {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":2}}\n\n")
					return
				}
				_, _ = fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"total_tokens":2}}`)
			}))
			defer server.Close()

			client := NewLLMClient(server.URL, "key", "primary", 180, 128, false, 5, []string{"fallback"})
			ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
			defer cancel()
			content, _, err := tt.call(ctx, client)
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if content != "ok" {
				t.Fatalf("content=%q, want fallback result", content)
			}
			if got := primaryAttempts.Load(); got != 1 {
				t.Fatalf("primary attempts=%d, want 1; PerModelTimeout wiring should skip same-model retries", got)
			}
			if got := fallbackAttempts.Load(); got != 1 {
				t.Fatalf("fallback attempts=%d, want 1", got)
			}
		})
	}
}
