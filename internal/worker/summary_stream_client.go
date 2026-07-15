package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/streaming"
)

var summaryStreamHTTPClient = &http.Client{Timeout: 0}

type summaryStreamSender struct {
	url          string
	taskID       int64
	runID        string
	scope        string
	targetUserID string
	ch           chan streaming.Event
	closed       chan struct{}
	once         sync.Once
	degraded     atomic.Bool
}

func newSummaryStreamSender(ctx context.Context, cfg *config.Config, taskID int64, targetUserID, scope, runID string) *summaryStreamSender {
	callbackURL := ""
	if cfg != nil {
		callbackURL = cfg.WorkerCallbackURL
	}
	url := summaryStreamURL(callbackURL)
	s := &summaryStreamSender{
		url:          url,
		taskID:       taskID,
		runID:        runID,
		scope:        streaming.NormalizeScope(scope),
		targetUserID: targetUserID,
		ch:           make(chan streaming.Event, 256),
		closed:       make(chan struct{}),
	}
	if url == "" {
		s.degraded.Store(true)
		close(s.closed)
		return s
	}
	s.start(ctx)
	s.Send(streaming.Event{Type: streaming.EventStart})
	return s
}

func summaryStreamURL(callbackURL string) string {
	callbackURL = strings.TrimRight(strings.TrimSpace(callbackURL), "/")
	if callbackURL == "" {
		return ""
	}
	if strings.HasSuffix(callbackURL, "/internal/task-event") {
		return strings.TrimSuffix(callbackURL, "/internal/task-event") + "/internal/summary-stream"
	}
	return strings.TrimRight(callbackURL, "/") + "/summary-stream"
}

func (s *summaryStreamSender) start(ctx context.Context) {
	pr, pw := io.Pipe()

	go func() {
		defer close(s.closed)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, pr)
		if err != nil {
			s.markDegraded("create internal stream request", err)
			_ = pr.CloseWithError(err)
			return
		}
		req.Header.Set("Content-Type", "application/x-ndjson")
		resp, err := summaryStreamHTTPClient.Do(req)
		if err != nil {
			s.markDegraded("post internal stream", err)
			_ = pr.CloseWithError(err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			s.markDegraded("internal stream non-2xx", fmt.Errorf("status=%d body=%s", resp.StatusCode, string(body)))
		}
	}()

	go func() {
		enc := json.NewEncoder(pw)
		defer pw.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-s.ch:
				if !ok {
					return
				}
				if err := enc.Encode(ev); err != nil {
					s.markDegraded("write internal stream", err)
					return
				}
			}
		}
	}()
}

func (s *summaryStreamSender) Send(ev streaming.Event) bool {
	if s == nil || s.degraded.Load() {
		return false
	}
	ev.TaskID = s.taskID
	ev.RunID = s.runID
	ev.Scope = s.scope
	ev.TargetUserID = s.targetUserID
	select {
	case s.ch <- ev:
		return true
	default:
		s.markDegraded("enqueue internal stream", fmt.Errorf("buffer full"))
		return false
	}
}

func (s *summaryStreamSender) Stage(stage string) {
	if stage == "" {
		return
	}
	s.Send(streaming.Event{Type: streaming.EventStage, Stage: stage})
}

func (s *summaryStreamSender) Delta(delta string) error {
	if delta == "" {
		return nil
	}
	s.Send(streaming.Event{Type: streaming.EventDelta, Delta: delta})
	return nil
}

func (s *summaryStreamSender) Done(status int) {
	s.Send(streaming.Event{Type: streaming.EventDone, Status: status})
}

func (s *summaryStreamSender) Error(message string) {
	s.Send(streaming.Event{Type: streaming.EventError, Message: message})
}

func (s *summaryStreamSender) Close() {
	if s == nil || s.url == "" {
		return
	}
	s.once.Do(func() {
		close(s.ch)
		select {
		case <-s.closed:
		case <-time.After(2 * time.Second):
		}
	})
}

func (s *summaryStreamSender) markDegraded(where string, err error) {
	if s == nil {
		return
	}
	if s.degraded.CompareAndSwap(false, true) {
		log.Printf("[summary-stream] degraded task=%d user=%s run=%s at %s: %v", s.taskID, s.targetUserID, s.runID, where, err)
	}
}

func newSummaryRunID(taskID int64, userID string) string {
	var b bytes.Buffer
	_, _ = fmt.Fprintf(&b, "%d:%s:%d", taskID, userID, time.Now().UnixNano())
	return b.String()
}
