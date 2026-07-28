package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// LLM fallback: when the primary model exhausts its per-model retry budget on
// retryable failures (5xx / 429 / network), Chat should switch to each
// configured fallback model in order and try again. On terminal errors
// (4xx / decode / empty choices) Chat must not escalate to fallback — those
// signal a caller-side problem the next model would hit too.
//
// Motivation: issue #179.

// newFallbackTestServer returns an httptest server that:
//   - fails with the given status for the first `failFor` chat/completions
//     requests it sees whose payload references `primaryModel`
//   - responds 200 with a canned choice for any request that references a
//     fallback model (or the primary model after failFor exhausts)
//
// Every recorded request's `model` field is appended to `seenModels` so the
// test can assert the exact fail-over sequence.
func newFallbackTestServer(t *testing.T, primaryModel string, primaryFailStatus int, primaryFailFor *atomic.Int32, seenModels *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		*seenModels = append(*seenModels, body.Model)

		if body.Model == primaryModel && primaryFailFor.Load() > 0 {
			primaryFailFor.Add(-1)
			http.Error(w, "upstream unavailable", primaryFailStatus)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"content":"reply from %s","tool_calls":null}}],"usage":{"total_tokens":10}}`, body.Model)
	}))
}

func TestChat_FailoverToFallbackAfterRetriesExhausted(t *testing.T) {
	primary := "gpt-5.6-sol"
	fallback := "claude-sonnet-4-6"

	// Primary will 503 forever; fallback should succeed on its first attempt.
	// We set failFor to a very large value so the primary loop always fails,
	// exhausting all 3 attempts.
	var failFor atomic.Int32
	failFor.Store(1000)
	var seen []string
	srv := newFallbackTestServer(t, primary, http.StatusServiceUnavailable, &failFor, &seen)
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", primary, 5, 512, []string{fallback})
	c.http = &http.Client{Timeout: 2 * time.Second}
	// Force zero backoff would require reworking Chat; the exponential 1s+2s
	// budget is short enough to keep the test under a few seconds.

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	turn, err := c.Chat(ctx, []Message{{Role: "user", Content: "hi"}}, nil)
	if err != nil {
		t.Fatalf("Chat should have fallen over to %q: %v", fallback, err)
	}
	if !strings.Contains(turn.Content, fallback) {
		t.Errorf("expected reply to come from fallback %q, got %q", fallback, turn.Content)
	}

	// Sanity check: we should see 3 primary attempts (retry budget) followed
	// by at least one fallback attempt. Anything more means fallback also
	// retried spuriously.
	if len(seen) < 4 {
		t.Fatalf("expected >=4 requests recorded (3 primary + >=1 fallback), got %d: %v", len(seen), seen)
	}
	for i := 0; i < 3; i++ {
		if seen[i] != primary {
			t.Errorf("attempt %d: expected primary %q, got %q", i, primary, seen[i])
		}
	}
	if seen[3] != fallback {
		t.Errorf("attempt 3: expected fallback %q, got %q", fallback, seen[3])
	}
}

func TestChat_NoFallbackOnTerminalError(t *testing.T) {
	primary := "gpt-5.6-sol"
	fallback := "claude-sonnet-4-6"

	// Primary returns 400 (terminal); Chat should surface it immediately and
	// never touch the fallback. A 4xx (non-429) from model A almost certainly
	// indicates a caller-side problem — model B would hit it too.
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen = append(seen, body.Model)
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", primary, 5, 512, []string{fallback})
	c.http = &http.Client{Timeout: 2 * time.Second}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := c.Chat(ctx, []Message{{Role: "user", Content: "hi"}}, nil)
	if err == nil {
		t.Fatalf("Chat should have failed with a terminal 400, got nil error")
	}
	if len(seen) != 1 {
		t.Fatalf("expected exactly 1 request (terminal, no fallback), got %d: %v", len(seen), seen)
	}
	if seen[0] != primary {
		t.Errorf("expected the one request to hit primary %q, got %q", primary, seen[0])
	}
}

func TestChat_NoFallbackConfiguredKeepsSingleModelBehavior(t *testing.T) {
	primary := "qwen3.6-max"
	// Persistent 503 with no fallback configured: Chat should exhaust its
	// per-model retries and return an error, without any panic or unexpected
	// behavior. This locks in the backward-compat contract from before #179.
	var failFor atomic.Int32
	failFor.Store(1000)
	var seen []string
	srv := newFallbackTestServer(t, primary, http.StatusServiceUnavailable, &failFor, &seen)
	defer srv.Close()

	c := NewClient(srv.URL, "test-key", primary, 5, 512, nil)
	c.http = &http.Client{Timeout: 2 * time.Second}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := c.Chat(ctx, []Message{{Role: "user", Content: "hi"}}, nil)
	if err == nil {
		t.Fatalf("Chat should have failed after exhausting retries")
	}
	// Exactly 3 primary attempts, no fallback traffic.
	if len(seen) != 3 {
		t.Fatalf("expected 3 primary attempts, got %d: %v", len(seen), seen)
	}
	for i, m := range seen {
		if m != primary {
			t.Errorf("attempt %d: expected %q, got %q", i, primary, m)
		}
	}
}

func TestNewClient_DedupesFallbackList(t *testing.T) {
	// The primary model appearing in fallbackModels would waste a retry
	// budget without gaining coverage; the constructor drops it, along with
	// empty strings and duplicates within the fallback list. Locking this in
	// so future refactors don't re-introduce the wasted round-trip.
	primary := "qwen3.6-max"
	c := NewClient("http://x", "k", primary, 5, 512, []string{"", primary, "claude-sonnet-4-6", "claude-sonnet-4-6", "deepseek-v4-flash"})
	if got, want := len(c.fallbackModels), 2; got != want {
		t.Fatalf("fallbackModels length = %d, want %d (deduped): %v", got, want, c.fallbackModels)
	}
	if c.fallbackModels[0] != "claude-sonnet-4-6" || c.fallbackModels[1] != "deepseek-v4-flash" {
		t.Errorf("fallbackModels order/content unexpected: %v", c.fallbackModels)
	}
}
