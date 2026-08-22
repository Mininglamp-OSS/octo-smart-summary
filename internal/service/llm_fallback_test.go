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

	content, tokens, err := c.Call(ctx, []ChatMessage{{Role: "user", Content: "hi"}}, 0.1)
	if err != nil {
		t.Fatalf("403 on primary must escalate to fallback, got err: %v (seen=%v)", err, seen)
	}
	if !strings.Contains(content, fallback) || tokens != 7 {
		t.Errorf("expected fallback content, got content=%q tokens=%d", content, tokens)
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
