package worker

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"gorm.io/gorm"
)

// ContentGenerationWorker consumes committed runs independently of HTTP
// wakeups and legacy task.status. Database claims, not this local map, are the
// cross-process authority. No retrieval client or notifier is available here.
type ContentGenerationWorker struct {
	content  *service.ContentService
	model    service.RefineModel
	pool     *WorkerPool
	interval time.Duration
	inFlight sync.Map
}

func NewContentGenerationWorker(db *gorm.DB, pool *WorkerPool, model service.RefineModel, interval time.Duration) *ContentGenerationWorker {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &ContentGenerationWorker{
		content: service.NewContentService(db), model: model, pool: pool, interval: interval,
	}
}

// Run scans immediately on startup, then periodically. Cancel and await Run
// before draining the shared pool. Interrupted executions retain their leases
// and are recoverable by a replacement worker after expiry.
func (w *ContentGenerationWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if _, err := w.poll(ctx); err != nil && ctx.Err() == nil {
			log.Printf("[content-worker] queue scan failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *ContentGenerationWorker) poll(ctx context.Context) (int, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if w.model == nil || w.pool == nil {
		return 0, nil
	}
	runs, err := w.content.RecoverableGenerations(ctx, service.ContentWriteSpaces(), 100)
	if err != nil {
		return 0, err
	}
	submitted := 0
	for _, run := range runs {
		if ctx.Err() != nil {
			break
		}
		id := run.ID
		if _, loaded := w.inFlight.LoadOrStore(id, struct{}{}); loaded {
			continue
		}
		if !w.pool.TrySubmit(func() {
			defer w.inFlight.Delete(id)
			_, err := w.content.ExecuteRefine(ctx, id, w.model)
			if err == nil || ctx.Err() != nil {
				return
			}
			var ce *service.ContentError
			if errors.As(err, &ce) {
				// Expected when another worker wins a claim or a cancellation.
				if ce.Code == "generation_not_claimable" || ce.Code == "generation_lease_lost" {
					return
				}
				log.Printf("[content-worker] run=%s code=%s", id, ce.Code)
				return
			}
			log.Printf("[content-worker] run=%s execution could not commit", id)
		}) {
			w.inFlight.Delete(id)
			break // leave durable pending rows for the next scan
		}
		submitted++
	}
	return submitted, nil
}
