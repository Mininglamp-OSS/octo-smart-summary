package config

import "testing"

// ResolveAgentMapConcurrency guards operator input: the value multiplies with
// the agent runner's outer tool pool (agent.NewPool(4)), so an unbounded or
// zero value is not merely odd — 0 would deadlock a zero-capacity semaphore and
// a large value fans out 4*N concurrent completions from one user request.
func TestResolveAgentMapConcurrency(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"unset -> default", 0, defaultAgentMapConcurrency},
		{"negative -> default", -3, defaultAgentMapConcurrency},
		{"one -> serial rollback path", 1, 1},
		{"in range -> kept", 4, 4},
		{"at cap -> kept", maxAgentMapConcurrency, maxAgentMapConcurrency},
		{"over cap -> clamped", maxAgentMapConcurrency + 10, maxAgentMapConcurrency},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := Config{AgentMapConcurrency: c.in}
			if got := cfg.ResolveAgentMapConcurrency(); got != c.want {
				t.Fatalf("ResolveAgentMapConcurrency(%d) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

// The resolved value must never be 0: summarizeChunksConcurrently sizes a
// buffered channel with it, and make(chan struct{}, 0) blocks forever.
func TestResolveAgentMapConcurrency_NeverZero(t *testing.T) {
	for _, in := range []int{-100, -1, 0} {
		cfg := Config{AgentMapConcurrency: in}
		if got := cfg.ResolveAgentMapConcurrency(); got < 1 {
			t.Fatalf("ResolveAgentMapConcurrency(%d) = %d — a zero-capacity semaphore deadlocks the Map phase", in, got)
		}
	}
}

// The agent knob must stay independent of the worker knob: they apply to
// different code paths and only the agent one multiplies with a tool pool.
func TestAgentAndWorkerMapConcurrencyAreIndependent(t *testing.T) {
	cfg := Config{AgentMapConcurrency: 2, WorkerMapConcurrency: 5}
	if got := cfg.ResolveAgentMapConcurrency(); got != 2 {
		t.Fatalf("agent resolve = %d, want 2 (worker value must not leak in)", got)
	}
	if cfg.WorkerMapConcurrency != 5 {
		t.Fatalf("worker value = %d, want 5 (agent resolve must not mutate it)", cfg.WorkerMapConcurrency)
	}
}
