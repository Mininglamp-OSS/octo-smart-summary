package worker

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/streaming"
	"gorm.io/gorm"
)

// ContentGenerationWorker consumes committed runs independently of HTTP
// wakeups and legacy task.status. Database claims, not this local map, are the
// cross-process authority. Full retrieval is delegated to the frozen-input
// executor; refinement never uses that executor.
type ContentGenerationWorker struct {
	content  *service.ContentService
	model    service.RefineModel
	executor service.FullGenerationExecutor
	cfg      *config.Config
	pool     *WorkerPool
	interval time.Duration
	inFlight sync.Map
}

func (w *ContentGenerationWorker) WithExecution(content *service.ContentService, executor service.FullGenerationExecutor) *ContentGenerationWorker {
	w.content, w.executor = content, executor
	return w
}

// WithStreaming enables the refine live-preview channel. Without it (cfg nil or
// no callback URL configured) refinement still runs, buffered, exactly as before.
func (w *ContentGenerationWorker) WithStreaming(cfg *config.Config) *ContentGenerationWorker {
	w.cfg = cfg
	return w
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
	if (w.model == nil && w.executor == nil) || w.pool == nil {
		return 0, nil
	}
	spaces := service.ContentWriteSpaces()
	if w.executor != nil {
		pilot := service.ContentExecutionSpaces()
		if err := w.content.QueueDueGenerations(ctx, time.Now(), pilot); err != nil {
			return 0, err
		}
		spaces = append(spaces, pilot...)
	}
	runs, err := w.content.RecoverableGenerations(ctx, spaces, 100)
	if err != nil {
		return 0, err
	}
	submitted := 0
	for _, run := range runs {
		if (run.Executor == "workflow" && w.executor == nil) || (run.Executor == "refine" && w.model == nil) {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		id := run.ID
		executor := run.Executor
		if _, loaded := w.inFlight.LoadOrStore(id, struct{}{}); loaded {
			continue
		}
		if !w.pool.TrySubmit(func() {
			defer w.inFlight.Delete(id)
			var err error
			if executor == "workflow" {
				_, err = w.content.ExecuteFullGeneration(ctx, id, w.executor)
			} else {
				_, err = w.content.ExecuteRefine(ctx, id, w.model, w.refineSink(ctx))
			}
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

// refineSink returns a factory that builds the refine live-preview sink for a
// claimed run, or nil when streaming is not configured (buffered refine only).
// The sink is created after the claim so only the winning worker previews.
func (w *ContentGenerationWorker) refineSink(ctx context.Context) func(*model.SummaryGenerationRun) service.RefineStreamSink {
	if w.cfg == nil {
		return nil
	}
	return func(run *model.SummaryGenerationRun) service.RefineStreamSink {
		return newSummaryStreamSender(ctx, w.cfg, run.TaskID, run.ActorID, streaming.ScopePersonal, run.ID)
	}
}
