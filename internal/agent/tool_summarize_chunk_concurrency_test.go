package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

// Map-phase concurrency tests. All of them swap summarizeChunkFn, so none of
// them touches a real LLM; they assert the four properties the concurrent Map
// path must preserve relative to the previous serial loop: bounded fan-out,
// original output order, deterministic (lowest-index) error, and prompt
// cancellation.

// withMapConcurrency installs cfg.AgentMapConcurrency for the duration of a
// test and restores whatever deps were set before.
func withMapConcurrency(t *testing.T, n int) {
	t.Helper()
	prev := func() (cfg config.Config) {
		defer func() { _ = recover() }() // deps may be unset in a fresh package run
		_, _, _, cfg = GetSummaryDeps()
		return cfg
	}()

	cfg := prev
	cfg.AgentMapConcurrency = n
	SetSummaryDeps(nil, nil, nil, cfg)
	t.Cleanup(func() { SetSummaryDeps(nil, nil, nil, prev) })
}

// withStubMapCall swaps the Map seam and restores it afterwards.
func withStubMapCall(t *testing.T, fn func(ctx context.Context, chunk []map[string]interface{}, specGuidance string) (string, int, int, error)) {
	t.Helper()
	prev := summarizeChunkFn
	summarizeChunkFn = fn
	t.Cleanup(func() { summarizeChunkFn = prev })
}

func makeChunks(n int) [][]map[string]interface{} {
	chunks := make([][]map[string]interface{}, n)
	for i := range chunks {
		chunks[i] = []map[string]interface{}{{"content": fmt.Sprintf("chunk-%d", i)}}
	}
	return chunks
}

// The semaphore must cap in-flight Map calls at the configured concurrency.
func TestSummarizeChunksConcurrently_RespectsConcurrencyLimit(t *testing.T) {
	const concurrency = 3
	withMapConcurrency(t, concurrency)

	var inFlight, maxInFlight int64
	withStubMapCall(t, func(ctx context.Context, chunk []map[string]interface{}, _ string) (string, int, int, error) {
		cur := atomic.AddInt64(&inFlight, 1)
		for {
			old := atomic.LoadInt64(&maxInFlight)
			if cur <= old || atomic.CompareAndSwapInt64(&maxInFlight, old, cur) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt64(&inFlight, -1)
		return "s", 1, 0, nil
	})

	var cov chunkCoverage
	got, err := summarizeChunksConcurrently(context.Background(), makeChunks(6), "", &cov)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 6 {
		t.Fatalf("got %d summaries, want 6", len(got))
	}
	if maxInFlight > concurrency {
		t.Fatalf("max in-flight Map calls = %d, want <= %d", maxInFlight, concurrency)
	}
	if maxInFlight < 2 {
		t.Fatalf("max in-flight Map calls = %d — calls never overlapped, concurrency is not in effect", maxInFlight)
	}
}

// Completion order must not affect output order: the joined document and its
// [n] citation markers depend on chunk position.
func TestSummarizeChunksConcurrently_PreservesChunkOrder(t *testing.T) {
	withMapConcurrency(t, 5)

	// Finish in reverse: the last chunk returns first.
	const n = 5
	withStubMapCall(t, func(ctx context.Context, chunk []map[string]interface{}, _ string) (string, int, int, error) {
		id := chunk[0]["content"].(string)
		var idx int
		if _, err := fmt.Sscanf(id, "chunk-%d", &idx); err != nil {
			t.Errorf("unexpected chunk payload %q", id)
		}
		time.Sleep(time.Duration(n-idx) * 15 * time.Millisecond)
		return id, 1, 0, nil
	})

	var cov chunkCoverage
	got, err := summarizeChunksConcurrently(context.Background(), makeChunks(n), "", &cov)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i, s := range got {
		if want := fmt.Sprintf("chunk-%d", i); s != want {
			t.Fatalf("summaries[%d] = %q, want %q — completion order leaked into output order", i, s, want)
		}
	}
}

// Coverage counters were incremented inside the old serial loop. Aggregating
// them concurrently would be a data race and would under-count; assert the
// totals are exact. Run this one under -race for full value.
func TestSummarizeChunksConcurrently_AggregatesCoverageExactly(t *testing.T) {
	withMapConcurrency(t, 4)
	withStubMapCall(t, func(ctx context.Context, chunk []map[string]interface{}, _ string) (string, int, int, error) {
		return "s", 7, 2, nil
	})

	cov := chunkCoverage{InputCount: 70}
	if _, err := summarizeChunksConcurrently(context.Background(), makeChunks(10), "", &cov); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cov.ProcessedCount != 70 {
		t.Fatalf("ProcessedCount = %d, want 70", cov.ProcessedCount)
	}
	if cov.OversizedMessageCount != 20 {
		t.Fatalf("OversizedMessageCount = %d, want 20", cov.OversizedMessageCount)
	}
}

// A non-abort chunk failure is TOLERATED (#241 item 2): the summaries already
// computed for other chunks are kept, the failed slice gets a sentinel, and no
// error is returned — so a single bad chunk cannot discard a whole run's work.
func TestSummarizeChunksConcurrently_FailedChunkGetsSentinel(t *testing.T) {
	withMapConcurrency(t, 5)

	errEarly := errors.New("chunk 1 failed")
	errLate := errors.New("chunk 4 failed")
	withStubMapCall(t, func(ctx context.Context, chunk []map[string]interface{}, _ string) (string, int, int, error) {
		id := chunk[0]["content"].(string)
		switch id {
		case "chunk-4":
			return "", 0, 0, errLate
		case "chunk-1":
			return "", 0, 0, errEarly
		}
		return "s", 1, 0, nil
	})

	var cov chunkCoverage
	got, err := summarizeChunksConcurrently(context.Background(), makeChunks(5), "", &cov)
	if err != nil {
		t.Fatalf("a tolerated chunk failure must not return an error, got %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("want all 5 slots filled (sentinels for failures), got %d", len(got))
	}
	if cov.FailedChunkCount != 2 {
		t.Errorf("FailedChunkCount = %d, want 2", cov.FailedChunkCount)
	}
	for _, idx := range []int{1, 4} {
		if !strings.Contains(got[idx], service.MapFailedMarker) {
			t.Errorf("chunk %d slot = %q, want a failure sentinel", idx, got[idx])
		}
	}
	for _, idx := range []int{0, 2, 3} {
		if got[idx] != "s" {
			t.Errorf("chunk %d slot = %q, want the successful summary preserved", idx, got[idx])
		}
	}
}

// When EVERY chunk fails there is nothing usable to return, so the whole phase
// errors rather than shipping an all-sentinel deliverable.
func TestSummarizeChunksConcurrently_AllFailuresError(t *testing.T) {
	withMapConcurrency(t, 3)
	boom := errors.New("boom")
	withStubMapCall(t, func(ctx context.Context, chunk []map[string]interface{}, _ string) (string, int, int, error) {
		return "", 0, 0, boom
	})

	var cov chunkCoverage
	got, err := summarizeChunksConcurrently(context.Background(), makeChunks(4), "", &cov)
	if err == nil {
		t.Fatal("expected an error when all chunks fail, got nil")
	}
	if got != nil {
		t.Fatalf("expected no summaries when all fail, got %d", len(got))
	}
}

// Queued chunks must abort on cancellation instead of waiting for an in-flight
// LLM call to release a semaphore slot.
func TestSummarizeChunksConcurrently_CancelReleasesQueuedChunks(t *testing.T) {
	const concurrency = 3
	withMapConcurrency(t, concurrency)

	ctx, cancel := context.WithCancel(context.Background())
	var started sync.WaitGroup
	started.Add(1)
	var once sync.Once
	var calls int64

	withStubMapCall(t, func(ctx context.Context, chunk []map[string]interface{}, _ string) (string, int, int, error) {
		atomic.AddInt64(&calls, 1)
		once.Do(started.Done)
		<-ctx.Done() // first call blocks until cancellation
		return "", 0, 0, ctx.Err()
	})

	done := make(chan struct{})
	var cov chunkCoverage
	var err error
	go func() {
		_, err = summarizeChunksConcurrently(ctx, makeChunks(8), "", &cov)
		close(done)
	}()

	started.Wait()
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("summarizeChunksConcurrently did not return after cancellation — queued chunks are not selecting on ctx.Done()")
	}
	if err == nil {
		t.Fatal("expected a cancellation error, got nil")
	}
	if n := atomic.LoadInt64(&calls); n > concurrency {
		t.Fatalf("%d Map calls started after cancellation, want <=%d — queued chunks should abort at the semaphore", n, concurrency)
	}
}

// A panic below Registry.Dispatch's recovery boundary must be recovered (not
// terminate the process) and then TOLERATED like any other chunk failure
// (#241 item 2): the panicked slice gets a sentinel, the others are preserved,
// and no error escapes.
func TestSummarizeChunksConcurrently_PanicIsRecoveredAndTolerated(t *testing.T) {
	withMapConcurrency(t, 2)
	withStubMapCall(t, func(_ context.Context, chunk []map[string]interface{}, _ string) (string, int, int, error) {
		if chunk[0]["content"].(string) == "chunk-1" {
			panic("boom")
		}
		return "s", 1, 0, nil
	})

	var cov chunkCoverage
	got, err := summarizeChunksConcurrently(context.Background(), makeChunks(3), "", &cov)
	if err != nil {
		t.Fatalf("a recovered panic in one chunk must be tolerated, got err %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 slots, got %d", len(got))
	}
	if cov.FailedChunkCount != 1 || !strings.Contains(got[1], service.MapFailedMarker) {
		t.Fatalf("panicked chunk 1 slot = %q, FailedChunkCount = %d; want sentinel + 1", got[1], cov.FailedChunkCount)
	}
}

// Concurrency 1 must behave exactly like the previous serial loop: one call at
// a time, in order. This is the documented rollback path.
func TestSummarizeChunksConcurrently_ConcurrencyOneIsSerial(t *testing.T) {
	withMapConcurrency(t, 1)

	var mu sync.Mutex
	var order []string
	var inFlight, maxInFlight int64
	withStubMapCall(t, func(ctx context.Context, chunk []map[string]interface{}, _ string) (string, int, int, error) {
		cur := atomic.AddInt64(&inFlight, 1)
		if cur > atomic.LoadInt64(&maxInFlight) {
			atomic.StoreInt64(&maxInFlight, cur)
		}
		id := chunk[0]["content"].(string)
		mu.Lock()
		order = append(order, id)
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		atomic.AddInt64(&inFlight, -1)
		return id, 1, 0, nil
	})

	var cov chunkCoverage
	got, err := summarizeChunksConcurrently(context.Background(), makeChunks(4), "", &cov)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if maxInFlight != 1 {
		t.Fatalf("max in-flight = %d with concurrency 1, want 1", maxInFlight)
	}
	for i := range got {
		if want := fmt.Sprintf("chunk-%d", i); got[i] != want || order[i] != want {
			t.Fatalf("serial path diverged at %d: got %q order %q, want %q", i, got[i], order[i], want)
		}
	}
}
