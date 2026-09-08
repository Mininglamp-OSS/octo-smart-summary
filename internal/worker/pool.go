package worker

import "sync"

// WorkerPool controls concurrency via a semaphore channel.
type WorkerPool struct {
	sem chan struct{}
	wg  sync.WaitGroup
}

// NewWorkerPool creates a pool with the given max concurrency.
func NewWorkerPool(maxConcurrent int) *WorkerPool {
	return &WorkerPool{sem: make(chan struct{}, maxConcurrent)}
}

// Submit runs fn in a goroutine, blocking if pool is full.
// wg.Add(1) is called before acquiring the semaphore to prevent
// a race where Drain() returns before the goroutine starts.
func (p *WorkerPool) Submit(fn func()) {
	p.wg.Add(1)
	p.sem <- struct{}{}
	go func() {
		defer func() {
			<-p.sem
			p.wg.Done()
		}()
		fn()
	}()
}

// TrySubmit never parks a polling loop behind a slow model call. Producers must
// stop before Drain, just as with Submit.
func (p *WorkerPool) TrySubmit(fn func()) bool {
	select {
	case p.sem <- struct{}{}:
		p.wg.Add(1)
		go func() {
			defer func() {
				<-p.sem
				p.wg.Done()
			}()
			fn()
		}()
		return true
	default:
		return false
	}
}

// Drain waits for all submitted tasks to finish.
func (p *WorkerPool) Drain() {
	p.wg.Wait()
}
