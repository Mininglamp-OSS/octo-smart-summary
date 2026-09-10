package worker

import (
	"context"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/streaming"
)

// GenerateConfirmed reuses the existing personal Map/Reduce and citation
// machinery without reading mutable source/config projections or persisting
// through legacy task-status callbacks.
//
// A live SSE preview is streamed over the same worker→ingest→SSE channel the
// legacy personal pipeline uses, keyed by the durable run ID so the detail page
// can subscribe by (task, personal scope, actor). The stream is a view only:
// the authoritative final state is still the applied version written by
// CompleteFullGeneration and read back by the client's poll/refresh.
func (p *Processor) GenerateConfirmed(ctx context.Context, run model.SummaryGenerationRun, input service.FrozenGenerationInput) (service.FullGenerationOutput, error) {
	task := model.SummaryTask{ID: run.TaskID, SpaceID: run.SpaceID, TaskNo: input.TaskNo,
		CreatorID: run.ActorID, SummaryMode: model.ModeByPerson, Topic: *input.Spec.Requirement,
		TimeRangeStart: input.Time.Start, TimeRangeEnd: input.Time.End}

	stream := newSummaryStreamSender(ctx, p.cfg, run.TaskID, run.ActorID, streaming.ScopePersonal, run.ID)
	defer stream.Close()
	reportStage := func(stage string) {
		if stage == model.WorkflowStageGenerateSummary {
			stream.Stage(stage)
		}
	}
	streamDelta := func(delta string) error { return stream.Delta(delta) }

	content, citations, count, tokens, usedModel, err := p.executePersonalPipelineInput(ctx, task, run.ActorID, reportStage, streamDelta, &input)
	if err != nil {
		stream.Error("summary generation failed")
		return service.FullGenerationOutput{}, err
	}
	stream.Done(model.StatusCompleted)
	return service.FullGenerationOutput{Content: content, Citations: citations, MsgCount: count, Tokens: tokens, Model: usedModel}, err
}
