package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// streamingRefineModel satisfies both RefineModel and the optional
// RefineStreamModel, so ExecuteRefine takes the streaming branch when a sink is
// present. It records whether the streaming entry point was actually used.
type streamingRefineModel struct {
	deltas    []string
	body      string
	tokens    int
	usedModel string
	err       error
	streamed  bool
}

func (m *streamingRefineModel) CallWithModel(context.Context, []ChatMessage, float64) (string, int, string, error) {
	return m.body, m.tokens, m.usedModel, m.err
}

func (m *streamingRefineModel) CallStreamWithModel(_ context.Context, _ []ChatMessage, _ float64, onDelta func(string) error) (string, int, string, error) {
	m.streamed = true
	if m.err != nil {
		return "", 0, "", m.err
	}
	for _, d := range m.deltas {
		if err := onDelta(d); err != nil {
			return "", 0, "", err
		}
	}
	return m.body, m.tokens, m.usedModel, nil
}

// captureSink records the live-preview lifecycle. ExecuteRefine drives it
// synchronously on the calling goroutine, so no locking is required here.
type captureSink struct {
	stages     []string
	deltas     []string
	done       int
	doneCalled bool
	errMsg     string
	closed     bool
}

func (c *captureSink) Stage(stage string)   { c.stages = append(c.stages, stage) }
func (c *captureSink) Delta(d string) error { c.deltas = append(c.deltas, d); return nil }
func (c *captureSink) Done(status int)      { c.done, c.doneCalled = status, true }
func (c *captureSink) Error(message string) { c.errMsg = message }
func (c *captureSink) Close()               { c.closed = true }

func queueRefineRun(t *testing.T, s *ContentService, ctx context.Context, current *FormalContentVersion, target ContentTarget, key string) *model.SummaryGenerationRun {
	t.Helper()
	run, err := s.QueueRefine(ctx, "s", 1, "owner", target.ID(), RefineContentRequest{
		ContentBaseline: baselineOf(current), IdempotencyKey: key, Feedback: "short",
	})
	requireWriteOK(t, err)
	return run
}

func TestContentRefineExecutorStreamsPreviewOnCapableModel(t *testing.T) {
	db := contentTestDB(t)
	ctx := context.Background()
	target, current := seedWritableContent(t, db, 1, ContentPersonal)
	s := NewContentService(db)
	run := queueRefineRun(t, s, ctx, current, target, "stream")

	llm := &streamingRefineModel{deltas: []string{"sh", "ort [7]"}, body: "short [7]", tokens: 3, usedModel: "fixture-model"}
	sink := &captureSink{}
	completed, err := s.ExecuteRefine(ctx, run.ID, llm, func(*model.SummaryGenerationRun) RefineStreamSink { return sink })
	requireWriteOK(t, err)

	if !llm.streamed {
		t.Fatal("a streaming-capable model must be driven through the streaming entry point")
	}
	if got := strings.Join(sink.deltas, ""); got != "short [7]" {
		t.Fatalf("streamed preview = %q, want the model's deltas concatenated", got)
	}
	if len(sink.stages) != 1 || sink.stages[0] != model.WorkflowStageGenerateSummary {
		t.Fatalf("stages = %v, want a single generate-summary stage", sink.stages)
	}
	if !sink.doneCalled || sink.done != model.StatusCompleted {
		t.Fatalf("done = (%v,%d), want completed", sink.doneCalled, sink.done)
	}
	if sink.errMsg != "" {
		t.Fatalf("unexpected error surfaced on success: %q", sink.errMsg)
	}
	if !sink.closed {
		t.Fatal("the sink must be closed even on success")
	}
	if completed == nil || completed.OutputVersionID == "" {
		t.Fatalf("refine did not produce a candidate version: %+v", completed)
	}
}

func TestContentRefineExecutorSurfacesStreamErrorAndNeverFinishes(t *testing.T) {
	db := contentTestDB(t)
	ctx := context.Background()
	target, current := seedWritableContent(t, db, 1, ContentPersonal)
	s := NewContentService(db)
	run := queueRefineRun(t, s, ctx, current, target, "stream-err")

	llm := &streamingRefineModel{err: errors.New("provider error with sensitive details")}
	sink := &captureSink{}
	_, err := s.ExecuteRefine(ctx, run.ID, llm, func(*model.SummaryGenerationRun) RefineStreamSink { return sink })
	requireContentCode(t, err, "model_failed")

	if sink.doneCalled {
		t.Fatal("a failed refine must not emit a completed done event")
	}
	if sink.errMsg == "" {
		t.Fatal("the failure must be surfaced to the preview as an error")
	}
	if strings.Contains(sink.errMsg, "sensitive") {
		t.Fatalf("the raw provider error leaked into the preview: %q", sink.errMsg)
	}
	if !sink.closed {
		t.Fatal("the sink must be closed on failure")
	}
	status, err := s.Generation(ctx, "s", 1, "owner", target.ID(), run.ID)
	requireWriteOK(t, err)
	if status.Status != "failed" || status.ActiveSlot != nil {
		t.Fatalf("a stream failure must fail the run and drop the active slot: %+v", status)
	}
}

func TestContentRefineExecutorBufferedFallbackStillFinishes(t *testing.T) {
	db := contentTestDB(t)
	ctx := context.Background()
	target, current := seedWritableContent(t, db, 1, ContentPersonal)
	s := NewContentService(db)
	run := queueRefineRun(t, s, ctx, current, target, "buffered")

	// A model with no streaming capability plus a live sink: the executor must
	// fall back to the buffered call, so no stage/delta previews are emitted,
	// but the run still finalizes and the sink is completed and closed.
	sink := &captureSink{}
	completed, err := s.ExecuteRefine(ctx, run.ID,
		contentRefineModelFunc(func(context.Context, []ChatMessage, float64) (string, int, string, error) {
			return "short [7]", 2, "fixture-model", nil
		}),
		func(*model.SummaryGenerationRun) RefineStreamSink { return sink })
	requireWriteOK(t, err)

	if len(sink.stages) != 0 || len(sink.deltas) != 0 {
		t.Fatalf("buffered fallback must not emit previews: stages=%v deltas=%v", sink.stages, sink.deltas)
	}
	if !sink.doneCalled || sink.done != model.StatusCompleted {
		t.Fatalf("done = (%v,%d), want completed", sink.doneCalled, sink.done)
	}
	if !sink.closed || completed == nil {
		t.Fatalf("buffered refine did not finalize: closed=%v completed=%+v", sink.closed, completed)
	}
}
