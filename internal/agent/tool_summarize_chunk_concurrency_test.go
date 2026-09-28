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
// them touches a real LLM; they assert the properties the concurrent Map path
// must preserve relative to the previous serial loop: bounded fan-out, original
// output order, an all-failed error classified on the lowest-index cause, and
// prompt cancellation.

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

// Recovery-first (contract §2): a transient per-chunk failure, with the run still
// alive, makes the WHOLE invocation return an error (it owes a retry) rather than
// shipping the other chunks' summaries as a partial — so a hole can never reach
// Reduce silently. The lowest-index cause drives classification.
func TestSummarizeChunksConcurrently_FailedChunkAborts(t *testing.T) {
	for _, concurrency := range []int{1, 5} {
		t.Run(fmt.Sprintf("concurrency=%d", concurrency), func(t *testing.T) {
			withMapConcurrency(t, concurrency)

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
			if err == nil {
				t.Fatal("a transient chunk failure must abort the invocation (owes a retry), got nil error")
			}
			if got != nil {
				t.Fatalf("no partial summaries may be shipped on a failed invocation, got %d", len(got))
			}
			// Keyed on the lowest-index cause (chunk 1), not the later one.
			if !errors.Is(err, errEarly) {
				t.Fatalf("error must key on the lowest-index cause, got %v", err)
			}
			if cov.FailedChunkCount != 2 {
				t.Errorf("FailedChunkCount = %d, want 2", cov.FailedChunkCount)
			}
		})
	}
}

// A blank-success chunk is COVERED, not lost (contract §2–§4): the model
// considered the messages and found nothing worth reporting, so they count as
// processed and a fully-covered-but-noisy run must not assert incompleteness.
// BlankChunkCount tracks it for observability only.
func TestSummarizeChunksConcurrently_BlankSuccessCovered(t *testing.T) {
	for _, concurrency := range []int{1, 4} {
		t.Run(fmt.Sprintf("concurrency=%d", concurrency), func(t *testing.T) {
			withMapConcurrency(t, concurrency)
			withStubMapCall(t, func(_ context.Context, chunk []map[string]interface{}, _ string) (string, int, int, error) {
				switch chunk[0]["content"].(string) {
				case "chunk-1":
					return "   ", 5, 0, nil // whitespace-only "success"
				case "chunk-3":
					return "", 5, 0, nil // empty "success"
				}
				return "s", 5, 0, nil
			})

			var cov chunkCoverage
			got, err := summarizeChunksConcurrently(context.Background(), makeChunks(5), "", &cov)
			if err != nil {
				t.Fatalf("blank successes are not failures, got %v", err)
			}
			// Only the 3 non-blank chunks contribute joined summaries.
			if len(got) != 3 {
				t.Fatalf("want the 3 non-blank summaries joined, got %d", len(got))
			}
			if cov.BlankChunkCount != 2 {
				t.Errorf("BlankChunkCount = %d, want 2", cov.BlankChunkCount)
			}
			if cov.FailedChunkCount != 0 {
				t.Errorf("blank success is not a failure: FailedChunkCount = %d, want 0", cov.FailedChunkCount)
			}
			// All 5 chunks (5 messages each) are COVERED — the 2 blank ones included,
			// so a noisy run does not read as incomplete.
			if cov.ProcessedCount != 25 {
				t.Fatalf("ProcessedCount = %d, want 25 — blank chunks must count as covered", cov.ProcessedCount)
			}
		})
	}
}

// When no usable summary survives, the phase errors — and the error must be
// keyed on the LOWEST-INDEX cause ALONE, never a union of every cause.
// classifyToolError matches on both errors.Is and the error TEXT, so wrapping
// errors.Join would let a single non-transient cause (a recovered panic, an
// oversized chunk) anywhere in the set drag the whole Map phase to
// fatal+non-retryable — a recoverable transient storm latching a FAILED run
// (#256 round-7 P1-1). Pin: idx0 is a retryable 429, the later chunks panic
// (fatal-shaped); classification must follow idx0 and stay retryable/non-fatal.
func TestSummarizeChunksConcurrently_AllFailedClassifiesOnLowestIndex(t *testing.T) {
	for _, concurrency := range []int{1, 3} {
		t.Run(fmt.Sprintf("concurrency=%d", concurrency), func(t *testing.T) {
			withMapConcurrency(t, concurrency)
			errRetryable := errors.New("rate limited: status 429")
			withStubMapCall(t, func(_ context.Context, chunk []map[string]interface{}, _ string) (string, int, int, error) {
				if chunk[0]["content"].(string) == "chunk-0" {
					return "", 0, 0, errRetryable
				}
				panic("boom in a later chunk") // recovered → tolerated per-chunk failure
			})

			var cov chunkCoverage
			got, err := summarizeChunksConcurrently(context.Background(), makeChunks(3), "", &cov)
			if err == nil || got != nil {
				t.Fatalf("expected an error and no summaries, got err=%v summaries=%d", err, len(got))
			}
			// Lowest-index cause drives errors.Is; the later panic must NOT be unioned in.
			if !errors.Is(err, errRetryable) {
				t.Fatalf("all-failed error must be keyed on the lowest-index cause, got %v", err)
			}
			if strings.Contains(err.Error(), "panicked") {
				t.Fatalf("a later chunk's fatal cause leaked into the classified error text: %v", err)
			}
			// The whole point: classification follows idx0 (429 → retryable), not the
			// later panic (→ fatal+non-retryable). A union would fail this.
			env := classifyToolError("summarize_chunk", err)
			if !env.Retryable || env.Fatal {
				t.Fatalf("lowest-index 429 must classify retryable/non-fatal; got retryable=%v fatal=%v — a later cause poisoned it", env.Retryable, env.Fatal)
			}
		})
	}
}

// Mixed failed + blank-but-successful with no usable content must still surface a
// failure cause (not a bare nil error), so classifyToolError keys on a real shape
// rather than the generic no-usable-output string (#256 round-7 P2-2). The guard
// is len(summaries)==0 && len(errs)>0, not "every chunk failed".
func TestSummarizeChunksConcurrently_MixedFailedAndBlankSurfacesCause(t *testing.T) {
	withMapConcurrency(t, 4)
	errCause := errors.New("upstream: status 503")
	withStubMapCall(t, func(_ context.Context, chunk []map[string]interface{}, _ string) (string, int, int, error) {
		switch chunk[0]["content"].(string) {
		case "chunk-0":
			return "", 0, 0, errCause // failure
		default:
			return "   ", 1, 0, nil // blank successes
		}
	})

	var cov chunkCoverage
	got, err := summarizeChunksConcurrently(context.Background(), makeChunks(3), "", &cov)
	if err == nil || got != nil {
		t.Fatalf("mixed failed+blank with nothing usable must error, got err=%v summaries=%d", err, len(got))
	}
	if !errors.Is(err, errCause) {
		t.Fatalf("the failure cause must survive on the mixed path, got %v", err)
	}
	if env := classifyToolError("summarize_chunk", err); !env.Retryable {
		t.Fatalf("a 503 cause must stay retryable, got %+v", env)
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

// A panic below Registry.Dispatch's recovery boundary must be RECOVERED (not
// terminate the process) and then treated as a per-chunk failure: recovery-first
// (contract §2) makes the invocation return an error (it owes a retry), it does
// not ship the other chunks as a partial. Covers BOTH the concurrent goroutine
// recover and the serial path's summarizeChunkRecovered wrapper — the key
// property is that no panic escapes to crash the process.
func TestSummarizeChunksConcurrently_PanicIsRecoveredThenAborts(t *testing.T) {
	for _, concurrency := range []int{1, 2} {
		t.Run(fmt.Sprintf("concurrency=%d", concurrency), func(t *testing.T) {
			withMapConcurrency(t, concurrency)
			withStubMapCall(t, func(_ context.Context, chunk []map[string]interface{}, _ string) (string, int, int, error) {
				if chunk[0]["content"].(string) == "chunk-1" {
					panic("boom")
				}
				return "s", 1, 0, nil
			})

			var cov chunkCoverage
			// Must not panic out of here — a recovered panic becomes an error.
			got, err := summarizeChunksConcurrently(context.Background(), makeChunks(3), "", &cov)
			if err == nil {
				t.Fatal("a recovered panic must abort the invocation (owes a retry), got nil error")
			}
			if !strings.Contains(err.Error(), "panicked") {
				t.Fatalf("error must carry the recovered panic cause, got %v", err)
			}
			if got != nil {
				t.Fatalf("no partial summaries may ship on a panicked invocation, got %d", len(got))
			}
			if cov.FailedChunkCount != 1 {
				t.Errorf("FailedChunkCount = %d, want 1", cov.FailedChunkCount)
			}
		})
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

// A truncation / reasoning-budget-exhaustion result is FATAL, not a droppable
// transient (mirrors the worker's isFatalMapError): one such chunk aborts the
// whole Map phase rather than being silently omitted (#256 P2-3). ErrRequestTooLarge
// is fatal too (#256 r9 P1): it is deterministic (the chunk can't shrink on retry)
// and item 2's tolerance must not swallow item 1's fatal REQUEST_TOO_LARGE.
func TestSummarizeChunksConcurrently_FatalChunkErrorAborts(t *testing.T) {
	fatals := []struct {
		name string
		err  error
	}{
		{"output-truncated", service.ErrOutputTruncated},
		{"reasoning-budget", service.ErrReasoningBudgetExhausted},
		{"request-too-large", service.ErrRequestTooLarge},
	}
	for _, fatal := range fatals {
		for _, concurrency := range []int{1, 3} {
			t.Run(fmt.Sprintf("%s/concurrency=%d", fatal.name, concurrency), func(t *testing.T) {
				withMapConcurrency(t, concurrency)
				withStubMapCall(t, func(_ context.Context, chunk []map[string]interface{}, _ string) (string, int, int, error) {
					if chunk[0]["content"].(string) == "chunk-2" {
						return "", 0, 0, fatal.err
					}
					return "s", 1, 0, nil
				})

				var cov chunkCoverage
				got, err := summarizeChunksConcurrently(context.Background(), makeChunks(4), "", &cov)
				if err == nil {
					t.Fatalf("a %s chunk must abort the phase, got nil error", fatal.name)
				}
				if !errors.Is(err, fatal.err) {
					t.Fatalf("abort error must wrap the fatal cause, got %v", err)
				}
				if got != nil {
					t.Fatalf("expected no summaries on fatal abort, got %d", len(got))
				}
			})
		}
	}
}

// Under recovery-first ANY failure aborts, so "aborts" alone no longer pins that
// ErrRequestTooLarge is FATAL. The observable difference is classification: a
// fatal cause must win over a lower-index transient (isFatalChunkError short-
// circuits the walk), so the invocation classifies non-retryable+fatal instead of
// retryable. This mutation-kills removing ErrRequestTooLarge from isFatalChunkError
// (#256 r9 P1, re-pinned at r12).
func TestSummarizeChunksConcurrently_FatalWinsOverLowerIndexTransient(t *testing.T) {
	for _, concurrency := range []int{1, 3} {
		t.Run(fmt.Sprintf("concurrency=%d", concurrency), func(t *testing.T) {
			withMapConcurrency(t, concurrency)
			withStubMapCall(t, func(_ context.Context, chunk []map[string]interface{}, _ string) (string, int, int, error) {
				switch chunk[0]["content"].(string) {
				case "chunk-0":
					return "", 0, 0, errors.New("rate limited: status 429") // lower-index transient
				case "chunk-1":
					return "", 0, 0, service.ErrRequestTooLarge // higher-index fatal
				}
				return "s", 1, 0, nil
			})

			var cov chunkCoverage
			_, err := summarizeChunksConcurrently(context.Background(), makeChunks(3), "", &cov)
			if err == nil {
				t.Fatal("expected an error")
			}
			// The fatal cause must drive classification, not the lower-index 429.
			env := classifyToolError("summarize_chunk", err)
			if env.Retryable || !env.Fatal || env.ErrorCode != "REQUEST_TOO_LARGE" {
				t.Fatalf("fatal ErrRequestTooLarge must win over the lower-index transient; got %+v (err %v)", env, err)
			}
		})
	}
}

// TestFinalizeDropCounts pins the handler's coverage arithmetic (#256 round-7
// P2-1/P2-5): DroppedCount is accidental loss only (input − processed − capped),
// but Truncated — the loudest model-facing boolean — flags ANY real gap, so a
// cap-only run (the single largest loss this tool can produce) must still read
// truncated=true even though its DroppedCount is 0.
func TestFinalizeDropCounts(t *testing.T) {
	cases := []struct {
		name          string
		cov           chunkCoverage
		wantDropped   int
		wantTruncated bool
	}{
		{"clean full coverage", chunkCoverage{InputCount: 100, ProcessedCount: 100}, 0, false},
		{"accidental loss only", chunkCoverage{InputCount: 100, ProcessedCount: 80}, 20, true},
		{"cap only: dropped 0 but truncated", chunkCoverage{InputCount: 100, ProcessedCount: 60, CappedDroppedCount: 40}, 0, true},
		{"both accidental + cap", chunkCoverage{InputCount: 100, ProcessedCount: 55, CappedDroppedCount: 30}, 15, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cov := c.cov
			cov.finalizeDropCounts()
			if cov.DroppedCount != c.wantDropped {
				t.Errorf("DroppedCount = %d, want %d", cov.DroppedCount, c.wantDropped)
			}
			if cov.Truncated != c.wantTruncated {
				t.Errorf("Truncated = %v, want %v (a disclosed cap must still raise the loudest gap boolean)", cov.Truncated, c.wantTruncated)
			}
		})
	}
}

// TestCapChunks pins the #241 fan-out bound: at or under the cap nothing is
// dropped; over it, the MOST RECENT maxChunkCalls chunks are kept (newest
// conversation) and capped is reported so the caller can disclose the gap.
func TestCapChunks(t *testing.T) {
	t.Run("under cap: unchanged", func(t *testing.T) {
		in := makeChunks(maxChunkCalls)
		got, capped := capChunks(in)
		if capped || len(got) != maxChunkCalls {
			t.Fatalf("got capped=%v len=%d, want false / %d", capped, len(got), maxChunkCalls)
		}
	})
	t.Run("over cap: keeps the most recent, reports capped", func(t *testing.T) {
		in := makeChunks(maxChunkCalls + 5)
		got, capped := capChunks(in)
		if !capped {
			t.Fatal("capped = false, want true")
		}
		if len(got) != maxChunkCalls {
			t.Fatalf("len = %d, want %d", len(got), maxChunkCalls)
		}
		// makeChunks tags content "chunk-<i>"; the kept slice must start at the
		// 5th chunk (the oldest 5 at the head are dropped), proving the newest
		// tail slice is retained.
		if first := got[0][0]["content"].(string); first != fmt.Sprintf("chunk-%d", 5) {
			t.Fatalf("kept slice starts at %q, want chunk-5 (oldest head dropped)", first)
		}
	})
}

// TestAssembleMapOutput pins the #256 P1-R4 empty-Map guard + the disclosure
// contract on the production assembly path: real content + a drop appends the
// notice; all-blank output errors (recoverable) instead of shipping a
// notice-only body; a clean full-coverage run gets no notice.
func TestAssembleMapOutput(t *testing.T) {
	t.Run("drop with real content: notice appended", func(t *testing.T) {
		out, err := assembleMapOutput([]string{"real summary"}, true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(out, "real summary") || !strings.Contains(out, mapCoverageGapNotice) {
			t.Fatalf("want content + gap notice, got %q", out)
		}
	})
	t.Run("capped with real content: notice appended", func(t *testing.T) {
		out, err := assembleMapOutput([]string{"s"}, true)
		if err != nil || !strings.Contains(out, mapCoverageGapNotice) {
			t.Fatalf("want notice on cap, got %q err %v", out, err)
		}
	})
	t.Run("all-blank output errors (no notice-only body ships)", func(t *testing.T) {
		out, err := assembleMapOutput([]string{"", "  "}, true)
		if err == nil {
			t.Fatalf("want a no-usable-Map error, got %q", out)
		}
	})
	t.Run("full coverage: no notice", func(t *testing.T) {
		out, err := assembleMapOutput([]string{"a", "b"}, false)
		if err != nil || strings.Contains(out, mapCoverageGapNotice) {
			t.Fatalf("clean run must not carry a gap notice, got %q err %v", out, err)
		}
	})
}
