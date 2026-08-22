package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// PR #208 round-5 P2-4. The capacity messages spell "summary handle" with a
// SPACE, so classifyToolError's "summary_handle" substring branch never matched
// them and they fell through to the retryable critical-tool default. A typed
// sentinel is what makes the classification immune to that class of drift, so
// pin that these errors actually carry it — and that the store's OTHER errors
// deliberately do not.
func TestSummaryHandleStoreCapacityErrorsAreTyped(t *testing.T) {
	store := newSummaryHandleStore()
	for i := 0; i < maxSummaryHandles; i++ {
		if _, err := store.Put("x", 0); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	_, err := store.Put("one too many", 0)
	if !errors.Is(err, ErrSummaryHandleCapacity) {
		t.Fatalf("handle-count cap err = %v, want ErrSummaryHandleCapacity", err)
	}
	if !strings.Contains(err.Error(), "too many summary handles in one request") {
		t.Fatalf("err = %q, want the operator-readable wording preserved", err)
	}

	big := newSummaryHandleStore()
	if _, err := big.Put(strings.Repeat("x", maxSummaryHandleText), 0); err != nil {
		t.Fatalf("Put at the byte limit: %v", err)
	}
	_, err = big.Put("x", 0)
	if !errors.Is(err, ErrSummaryHandleCapacity) {
		t.Fatalf("text-size cap err = %v, want ErrSummaryHandleCapacity", err)
	}

	// An empty body is the model's mistake, fixable by retrying — it must NOT be
	// classified as a request-scoped dead end.
	if _, err := newSummaryHandleStore().Put("   ", 0); err == nil || errors.Is(err, ErrSummaryHandleCapacity) {
		t.Fatalf("empty-text err = %v, want a plain non-capacity error", err)
	}
	// Same for the model passing an impossible handle COUNT: it can pass fewer.
	small := newSummaryHandleStore()
	handles := make([]string, maxSummaryHandles+1)
	if _, err := small.ResolveAll(handles); err == nil || errors.Is(err, ErrSummaryHandleCapacity) {
		t.Fatalf("oversized argument list err = %v, want a plain non-capacity error", err)
	}
}

func TestSummaryHandleStoreResolveAll(t *testing.T) {
	store := newSummaryHandleStore()
	h1, err := store.Put("first summary", 2)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := store.Put("second summary", 3)
	if err != nil {
		t.Fatal(err)
	}

	resolved, err := store.ResolveAll([]string{h2, h1})
	if err != nil {
		t.Fatalf("ResolveAll: %v", err)
	}
	if len(resolved.Entries) != 2 || resolved.Entries[0].Text != "second summary" || resolved.Entries[1].Text != "first summary" {
		t.Fatalf("resolution did not preserve requested order: %+v", resolved.Entries)
	}
	if !store.NeedsReduce() {
		t.Fatal("new Map results must require Reduce")
	}
	store.MarkReduced(resolved.Generation)
	if store.NeedsReduce() {
		t.Fatal("successful full Reduce should satisfy the current generation")
	}
	if _, err := store.Put("late summary", 1); err != nil {
		t.Fatal(err)
	}
	if !store.NeedsReduce() {
		t.Fatal("a Map result added after Reduce must require another Reduce")
	}
}

func TestSummaryHandleStoreRejectsIncompleteOrInvalidSets(t *testing.T) {
	store := newSummaryHandleStore()
	h1, _ := store.Put("one", 1)
	h2, _ := store.Put("two", 1)

	for _, tc := range []struct {
		name    string
		handles []string
		want    string
	}{
		{name: "empty", handles: nil, want: "at least one"},
		{name: "partial", handles: []string{h1}, want: "all Map results"},
		{name: "duplicate", handles: []string{h1, h1}, want: "duplicate"},
		{name: "unknown", handles: []string{h1, "map_unknown_9"}, want: "invalid or expired"},
		{name: "empty handle", handles: []string{h1, ""}, want: "must not be empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.ResolveAll(tc.handles); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ResolveAll(%v) error = %v, want substring %q", tc.handles, err, tc.want)
			}
		})
	}
	_ = h2
}

func TestSummaryHandleStoreIsRequestScoped(t *testing.T) {
	ctxA := withSummaryHandleStore(context.Background())
	ctxB := withSummaryHandleStore(context.Background())
	storeA, _ := summaryHandleStoreFromContext(ctxA)
	storeB, _ := summaryHandleStoreFromContext(ctxB)

	hA, _ := storeA.Put("request A", 1)
	hB, _ := storeB.Put("request B", 1)
	if hA == hB {
		t.Fatalf("request handles must carry distinct namespaces: %q", hA)
	}
	if _, err := storeB.ResolveAll([]string{hA}); err == nil || !strings.Contains(err.Error(), "invalid or expired") {
		t.Fatalf("request B resolved request A handle: %v", err)
	}
	if _, err := storeA.ResolveAll([]string{hB}); err == nil || !strings.Contains(err.Error(), "invalid or expired") {
		t.Fatalf("request A resolved request B handle: %v", err)
	}
}

func TestSummaryHandleStoreRejectsSameStepReduce(t *testing.T) {
	store := newSummaryHandleStore()
	handle, err := store.PutAtStep("map body", 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveAllBefore([]string{handle}, 3); err == nil || !strings.Contains(err.Error(), "same or a later tool step") {
		t.Fatalf("same-step Reduce error = %v", err)
	}
	if _, err := store.ResolveAllBefore([]string{handle}, 4); err != nil {
		t.Fatalf("next-step Reduce should resolve: %v", err)
	}
}

func TestSummaryHandleStoreMapFailureNeedsLaterSuccessfulRetry(t *testing.T) {
	store := newSummaryHandleStore()
	handle, _ := store.PutAtStep("successful map", 1, 1)
	store.MarkMapFailed("messages_handle=failed", 1)
	store.MarkMapSucceeded("messages_handle=other", 1)
	if store.PendingMapFailures() != 1 {
		t.Fatal("same-step success on another target masked a failed Map")
	}
	if _, err := store.ResolveAllBefore([]string{handle}, 2); err == nil || !strings.Contains(err.Error(), "successful retry") {
		t.Fatalf("Reduce should be blocked by failed Map, got %v", err)
	}
	store.MarkMapSucceeded("messages_handle=failed", 1)
	if store.PendingMapFailures() != 1 {
		t.Fatal("same-step success must not clear a failure")
	}
	store.MarkMapSucceeded("messages_handle=failed", 2)
	if store.PendingMapFailures() != 0 {
		t.Fatal("later successful retry did not clear the failed target")
	}
	if _, err := store.ResolveAllBefore([]string{handle}, 2); err != nil {
		t.Fatalf("Reduce should proceed after recovery: %v", err)
	}
}

func TestSummaryHandleStoreUnrelatedSuccessCannotClearFailure(t *testing.T) {
	store := newSummaryHandleStore()
	store.MarkMapFailed("messages_handle=expired", 1)
	store.MarkMapSucceeded("messages_handle=fresh", 1)
	if store.PendingMapFailures() != 1 {
		t.Fatal("same-step fresh handle must not mask an expired-handle failure")
	}
	store.MarkMapSucceeded("messages_handle=fresh", 2)
	if store.PendingMapFailures() != 1 {
		t.Fatal("an unrelated later handle must not clear a failed Map target")
	}
}

func TestSummaryHandleStoreLaterSuccessClearsOneAnonymousArgumentFailure(t *testing.T) {
	store := newSummaryHandleStore()
	store.MarkMapFailed(anonymousMapFailurePrefix+"bad-1", 1)
	store.MarkMapFailed(anonymousMapFailurePrefix+"bad-2", 1)
	if store.PendingAnonymousMapFailures() != 2 {
		t.Fatalf("anonymous failures = %d, want 2", store.PendingAnonymousMapFailures())
	}

	store.MarkMapSucceeded("messages_handle=corrected", 1)
	if store.PendingMapFailures() != 2 {
		t.Fatal("same-step success must not clear an anonymous argument failure")
	}
	store.MarkMapSucceeded("messages_handle=corrected", 2)
	if store.PendingMapFailures() != 1 {
		t.Fatalf("first corrected retry should clear one anonymous failure, pending=%d", store.PendingMapFailures())
	}
	store.MarkMapSucceeded("messages_handle=corrected-again", 2)
	if store.PendingMapFailures() != 0 {
		t.Fatalf("second corrected retry should clear the remaining anonymous failure, pending=%d", store.PendingMapFailures())
	}
	if store.PendingAnonymousMapFailures() != 0 {
		t.Fatalf("anonymous failures remained after recovery: %d", store.PendingAnonymousMapFailures())
	}
}

func TestSummaryHandleStoreConcurrentPut(t *testing.T) {
	const n = 64
	store := newSummaryHandleStore()
	handles := make(chan string, n)
	errCh := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			handle, err := store.Put("summary", 1)
			if err != nil {
				errCh <- err
				return
			}
			handles <- handle
		}()
	}
	wg.Wait()
	close(handles)
	close(errCh)
	for err := range errCh {
		t.Fatalf("Put: %v", err)
	}
	seen := map[string]bool{}
	for handle := range handles {
		if seen[handle] {
			t.Fatalf("duplicate handle %q", handle)
		}
		seen[handle] = true
	}
	if len(seen) != n {
		t.Fatalf("unique handles = %d, want %d", len(seen), n)
	}
}
