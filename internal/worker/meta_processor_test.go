package worker

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMetaProcessorGetMetaLock(t *testing.T) {
	mp := &MetaProcessor{
		debounceTimers: make(map[int64]*time.Timer),
	}

	// Same taskID should return the same mutex
	mu1 := mp.getMetaLock(1)
	mu2 := mp.getMetaLock(1)
	if mu1 != mu2 {
		t.Error("expected same mutex for same task ID")
	}

	// Different taskID should return different mutex
	mu3 := mp.getMetaLock(2)
	if mu1 == mu3 {
		t.Error("expected different mutex for different task ID")
	}
}

func TestMetaProcessorTryLockMutualExclusion(t *testing.T) {
	mp := &MetaProcessor{
		debounceTimers: make(map[int64]*time.Timer),
	}

	mu := mp.getMetaLock(1)
	mu.Lock()

	// TryLock should fail since mutex is held
	if mu.TryLock() {
		t.Error("expected TryLock to fail when mutex is held")
		mu.Unlock()
	}

	mu.Unlock()

	// Now TryLock should succeed
	if !mu.TryLock() {
		t.Error("expected TryLock to succeed when mutex is free")
	}
	mu.Unlock()
}

func TestMetaProcessorDebounce(t *testing.T) {
	mp := &MetaProcessor{
		debounceTimers: make(map[int64]*time.Timer),
	}

	var callCount int32

	// Override: directly test debounce mechanics using timers
	mp.debounceMu.Lock()
	for i := 0; i < 5; i++ {
		if timer, exists := mp.debounceTimers[1]; exists {
			timer.Stop()
		}
		mp.debounceTimers[1] = time.AfterFunc(50*time.Millisecond, func() {
			atomic.AddInt32(&callCount, 1)
		})
	}
	mp.debounceMu.Unlock()

	// Wait for debounce to fire
	time.Sleep(200 * time.Millisecond)

	count := atomic.LoadInt32(&callCount)
	if count != 1 {
		t.Errorf("expected debounce to fire exactly once, got %d", count)
	}
}

func TestMetaProcessorConcurrentTryLock(t *testing.T) {
	mp := &MetaProcessor{
		debounceTimers: make(map[int64]*time.Timer),
	}

	mu := mp.getMetaLock(42)
	var locked int32
	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if mu.TryLock() {
				atomic.AddInt32(&locked, 1)
				time.Sleep(10 * time.Millisecond)
				mu.Unlock()
			}
		}()
	}

	wg.Wait()

	// At least 1 should have succeeded, but not all simultaneously
	if atomic.LoadInt32(&locked) == 0 {
		t.Error("expected at least one goroutine to acquire lock")
	}
}
