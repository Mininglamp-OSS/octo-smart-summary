package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestCall_403EscalatesToFallback locks in the #211 fix at the worker/service
// layer: when the primary model returns an account-level denial (HTTP 403 —
// e.g. a Bedrock SCP explicit-deny), Call must escalate to the configured
// fallback model (which the gateway can route to a different provider) instead
// of failing. Before #211 the worker consumed no fallback at all, and even the
// classifier treated 403 as terminal.
func TestCall_403EscalatesToFallback(t *testing.T) {
	primary := "claude-sonnet-4-6"
	fallback := "gpt-4.1"

	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		seen = append(seen, body.Model)
		mu.Unlock()
		if body.Model == primary {
			http.Error(w, "AccessDeniedException: explicit deny in a service control policy", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"content":"summary from %s"}}],"usage":{"total_tokens":7}}`, body.Model)
	}))
	defer srv.Close()

	c := NewLLMClient(srv.URL, "k", primary, 5, 4096, false, 5, []string{fallback})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	content, tokens, usedModel, err := c.CallWithModel(ctx, []ChatMessage{{Role: "user", Content: "hi"}}, 0.1)
	if err != nil {
		t.Fatalf("403 on primary must escalate to fallback, got err: %v (seen=%v)", err, seen)
	}
	if !strings.Contains(content, fallback) || tokens != 7 {
		t.Errorf("expected fallback content, got content=%q tokens=%d", content, tokens)
	}
	if usedModel != fallback {
		t.Errorf("used model=%q want %q", usedModel, fallback)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != primary || seen[1] != fallback {
		t.Fatalf("expected [primary, fallback], got %v", seen)
	}
}

// TestCall_NoFallbackConfiguredSingleModel confirms the single-model contract
// is unchanged when no fallback is configured: a 500 surfaces as an error and
// no phantom fallback traffic occurs.
func TestCall_NoFallbackConfiguredSingleModel(t *testing.T) {
	var count int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewLLMClient(srv.URL, "k", "only-model", 5, 4096, false, 5, nil)
	_, _, err := c.Call(context.Background(), []ChatMessage{{Role: "user", Content: "hi"}}, 0.1)
	if err == nil {
		t.Fatal("expected error from single-model 500")
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 request (MaxAttempts=1, no fallback), got %d", count)
	}
}

func TestCall_FallbackUsesAttemptedModelThinkingConfig(t *testing.T) {
	tests := []struct {
		name                string
		primary             string
		fallback            string
		wantThinking        string
		wantDisableThinking bool
	}{
		{
			name:         "claude to kimi",
			primary:      "claude-sonnet-4-6",
			fallback:     "tencent/kimi-k2.6",
			wantThinking: "disabled",
		},
		{
			name:                "kimi to qwen",
			primary:             "tencent/kimi-k2.6",
			fallback:            "qwen3.6-max",
			wantDisableThinking: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			type requestBody struct {
				Model              string                 `json:"model"`
				Thinking           *ThinkingParam         `json:"thinking"`
				ChatTemplateKwargs map[string]interface{} `json:"chat_template_kwargs"`
			}
			var seen []requestBody
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body requestBody
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				seen = append(seen, body)
				if body.Model == tc.primary {
					http.Error(w, "denied", http.StatusForbidden)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"total_tokens":1}}`)
			}))
			defer srv.Close()

			client := NewLLMClient(srv.URL, "k", tc.primary, 5, 4096, false, 5, []string{tc.fallback})
			if _, _, err := client.Call(context.Background(), []ChatMessage{{Role: "user", Content: "hi"}}, 0.1); err != nil {
				t.Fatalf("Call failed: %v", err)
			}
			if len(seen) != 2 {
				t.Fatalf("expected primary and fallback requests, got %+v", seen)
			}
			got := seen[1]
			if got.Model != tc.fallback {
				t.Fatalf("fallback model=%q want %q", got.Model, tc.fallback)
			}
			if tc.wantThinking != "" {
				if got.Thinking == nil || got.Thinking.Type != tc.wantThinking || got.ChatTemplateKwargs != nil {
					t.Fatalf("Kimi fallback config mismatch: thinking=%+v kwargs=%v", got.Thinking, got.ChatTemplateKwargs)
				}
			}
			if tc.wantDisableThinking {
				value, ok := got.ChatTemplateKwargs["enable_thinking"].(bool)
				if got.Thinking != nil || !ok || value {
					t.Fatalf("Qwen fallback config mismatch: thinking=%+v kwargs=%v", got.Thinking, got.ChatTemplateKwargs)
				}
			}
		})
	}
}

func TestCallStream_MidStreamFailurePreservesPartialResult(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen = append(seen, body.Model)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello \"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"world\"}}],\"usage\":{\"total_tokens\":42}}\n\n")
	}))
	defer srv.Close()

	client := NewLLMClient(srv.URL, "k", "primary", 5, 4096, false, 5, []string{"backup"})
	var streamed strings.Builder
	content, tokens, usedModel, err := client.CallStreamWithModel(context.Background(), []ChatMessage{{Role: "user", Content: "hi"}}, 0.1, func(delta string) error {
		streamed.WriteString(delta)
		return nil
	})
	if err == nil || content != "Hello world" || streamed.String() != content || tokens != 42 {
		t.Fatalf("expected preserved partial result, content=%q streamed=%q tokens=%d err=%v", content, streamed.String(), tokens, err)
	}
	if usedModel != "primary" {
		t.Fatalf("partial result attributed to %q, want primary", usedModel)
	}
	if len(seen) != 1 || seen[0] != "primary" {
		t.Fatalf("must not fallback after emitting content, seen=%v", seen)
	}
}

func TestCallStream_PreEmissionFailureFallsBack(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen = append(seen, body.Model)
		w.Header().Set("Content-Type", "text/event-stream")
		if body.Model == "primary" {
			_, _ = fmt.Fprint(w, "data: not-json\n\n")
			return
		}
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":5}}\n\n")
	}))
	defer srv.Close()

	client := NewLLMClient(srv.URL, "k", "primary", 5, 4096, false, 5, []string{"backup"})
	var streamed strings.Builder
	content, tokens, usedModel, err := client.CallStreamWithModel(context.Background(), []ChatMessage{{Role: "user", Content: "hi"}}, 0.1, func(delta string) error {
		streamed.WriteString(delta)
		return nil
	})
	if err != nil || content != "ok" || streamed.String() != "ok" || tokens != 5 {
		t.Fatalf("expected fallback stream success, content=%q streamed=%q tokens=%d err=%v", content, streamed.String(), tokens, err)
	}
	if usedModel != "backup" {
		t.Fatalf("fallback stream attributed to %q, want backup", usedModel)
	}
	if len(seen) != 2 || seen[0] != "primary" || seen[1] != "backup" {
		t.Fatalf("expected pre-emission fallback, seen=%v", seen)
	}
}
