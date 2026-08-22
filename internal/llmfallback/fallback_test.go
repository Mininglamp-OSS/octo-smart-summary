package llmfallback

import (
	"context"
	"errors"
	"testing"
	"time"
)

func noBackoff(int) time.Duration { return 0 }

func TestClassifyStatus(t *testing.T) {
	cases := []struct {
		code int
		want Outcome
	}{
		{200, Success},
		{429, RetrySameModel},
		{500, RetrySameModel},
		{503, RetrySameModel},
		{401, TryNextModel},
		{403, TryNextModel}, // Bedrock SCP-deny must escalate to a fallback provider
		{400, Terminal},
		{404, Terminal},
		{422, Terminal},
	}
	for _, c := range cases {
		if got := ClassifyStatus(c.code); got != c.want {
			t.Errorf("ClassifyStatus(%d)=%v want %v", c.code, got, c.want)
		}
	}
}

func TestRun_PrimarySuccess(t *testing.T) {
	var tried []string
	cfg := Config{Models: []string{"primary", "backup"}, MaxAttempts: 3, Backoff: noBackoff}
	val, used, err := Run(context.Background(), cfg, func(_ context.Context, m string) (string, Outcome, error) {
		tried = append(tried, m)
		return "ok-" + m, Success, nil
	})
	if err != nil || val != "ok-primary" || used != "primary" {
		t.Fatalf("got (%q,%q,%v)", val, used, err)
	}
	if len(tried) != 1 {
		t.Fatalf("primary success must not touch fallback, tried=%v", tried)
	}
}

func TestRun_403EscalatesWithoutSameModelRetry(t *testing.T) {
	// The incident case: primary is denied (403 → TryNextModel), fallback works.
	var tried []string
	cfg := Config{Models: []string{"claude-sonnet-4-6", "gpt-4.1"}, MaxAttempts: 3, Backoff: noBackoff}
	val, used, err := Run(context.Background(), cfg, func(_ context.Context, m string) (string, Outcome, error) {
		tried = append(tried, m)
		if m == "claude-sonnet-4-6" {
			return "", ClassifyStatus(403), errors.New("AccessDeniedException: explicit deny in SCP")
		}
		return "summary", Success, nil
	})
	if err != nil || val != "summary" || used != "gpt-4.1" {
		t.Fatalf("got (%q,%q,%v)", val, used, err)
	}
	// primary must be attempted exactly once (no wasteful same-model retries on 403)
	if len(tried) != 2 || tried[0] != "claude-sonnet-4-6" || tried[1] != "gpt-4.1" {
		t.Fatalf("tried=%v; expected single primary attempt then fallback", tried)
	}
}

func TestRun_RetryableThenFallback(t *testing.T) {
	attempts := map[string]int{}
	cfg := Config{Models: []string{"primary", "backup"}, MaxAttempts: 3, Backoff: noBackoff}
	val, used, err := Run(context.Background(), cfg, func(_ context.Context, m string) (string, Outcome, error) {
		attempts[m]++
		if m == "primary" {
			return "", ClassifyStatus(429), errors.New("rate limited")
		}
		return "ok", Success, nil
	})
	if err != nil || used != "backup" || val != "ok" {
		t.Fatalf("got (%q,%q,%v)", val, used, err)
	}
	if attempts["primary"] != 3 {
		t.Fatalf("primary should exhaust %d retries on 429, got %d", 3, attempts["primary"])
	}
}

func TestRun_TerminalStopsImmediately(t *testing.T) {
	var tried []string
	cfg := Config{Models: []string{"primary", "backup"}, MaxAttempts: 3, Backoff: noBackoff}
	_, _, err := Run(context.Background(), cfg, func(_ context.Context, m string) (string, Outcome, error) {
		tried = append(tried, m)
		return "", ClassifyStatus(400), errors.New("bad request")
	})
	if err == nil {
		t.Fatal("terminal error must surface")
	}
	if len(tried) != 1 {
		t.Fatalf("terminal on primary must not try fallback, tried=%v", tried)
	}
}

func TestRun_SingleModelPreservesError(t *testing.T) {
	sentinel := errors.New("upstream 500")
	cfg := Config{Models: []string{"only"}, MaxAttempts: 2, Backoff: noBackoff}
	_, _, err := Run(context.Background(), cfg, func(_ context.Context, _ string) (string, Outcome, error) {
		return "", ClassifyStatus(500), sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("single-model error should be preserved verbatim, got %v", err)
	}
}

func TestRun_AllModelsFail(t *testing.T) {
	cfg := Config{Models: []string{"a", "b"}, MaxAttempts: 1, Backoff: noBackoff}
	_, _, err := Run(context.Background(), cfg, func(_ context.Context, _ string) (string, Outcome, error) {
		return "", ClassifyStatus(503), errors.New("down")
	})
	if err == nil {
		t.Fatal("expected error when all models fail")
	}
}
