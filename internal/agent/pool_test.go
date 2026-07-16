package agent

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPool_RespectsMaxConcurrency(t *testing.T) {
	tests := []struct {
		name string
		max  int
		jobs int
	}{
		{name: "max1", max: 1, jobs: 10},
		{name: "max3", max: 3, jobs: 30},
		{name: "max5", max: 5, jobs: 50},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewPool(tt.max)
			var cur, peak int64
			var mu sync.Mutex
			var done int64

			for i := 0; i < tt.jobs; i++ {
				p.Submit(func() {
					c := atomic.AddInt64(&cur, 1)
					mu.Lock()
					if c > peak {
						peak = c
					}
					mu.Unlock()
					time.Sleep(2 * time.Millisecond)
					atomic.AddInt64(&cur, -1)
					atomic.AddInt64(&done, 1)
				})
			}
			p.Drain()

			if done != int64(tt.jobs) {
				t.Fatalf("done = %d, want %d (Drain did not wait for all)", done, tt.jobs)
			}
			if peak > int64(tt.max) {
				t.Fatalf("peak concurrency = %d, exceeds max %d", peak, tt.max)
			}
		})
	}
}
