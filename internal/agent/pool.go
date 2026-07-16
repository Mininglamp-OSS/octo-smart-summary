package agent

import "sync"

// Pool 是 agent 自建的信号量并发池，不共用 worker.WorkerPool，仅依赖标准库。
type Pool struct {
	sem chan struct{}
	wg  sync.WaitGroup
}

func NewPool(max int) *Pool {
	if max < 1 {
		max = 1
	}
	return &Pool{sem: make(chan struct{}, max)}
}

// Submit：先登记 WaitGroup 再抢信号量（Add 必须早于 goroutine 结束），
// 阻塞直到有空位，保证在途任务数不超过 max。
func (p *Pool) Submit(fn func()) {
	p.wg.Add(1)
	p.sem <- struct{}{}
	go func() {
		defer p.wg.Done()
		defer func() { <-p.sem }()
		fn()
	}()
}

// Drain 等待所有已提交任务结束。
func (p *Pool) Drain() {
	p.wg.Wait()
}
