package service

import (
	"context"
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
