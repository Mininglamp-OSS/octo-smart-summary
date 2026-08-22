package agent

import (
	"context"
	"log"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/summaryrun"
)

// recordOutputTruncated latches "a model completion on this run's answer path
// was cut off at its token limit" onto the run row, where the SS-07 finish gate
// can read it (finishgate.RunState.OutputTruncated -> GapOutputTruncation).
//
// WHY THIS EXISTS AT ALL — the disclosure must not be defeatable by the model.
//
// Before this, an LLM-output truncation was disclosed only as PROSE: the client
// appends service.TruncationNotice, and merge_summaries puts it in the
// `merged_summary` field of a TOOL RESULT. But a tool result is an INTERMEDIATE
// artifact. It is fed back to the planner as context, and the planner — not the
// tool — writes the final user-facing answer. Nothing forces the notice through:
// it reads like meta-commentary rather than content, so a model polishing a
// final answer will plausibly drop it. The user then receives a deliverable that
// is silently incomplete and LOOKS complete, which is the precise failure class
// this area of the codebase exists to prevent.
//
// Routing the fact through the run row instead makes the disclosure structural:
// it is assembled by finishgate.Evaluate from persisted evidence, outside the
// generating model's control, so no rewrite of the prose can suppress it.
//
// This does NOT replace the inline notice — the two are complementary. The gate
// is reliable and machine-readable; the inline notice is what tells a reader
// WHERE the text stops. Both are kept deliberately.
//
// Same channel as every other coverage fact (recordFetch in
// tool_fetch_channel.go, recordDroppedMessages in tool_summarize_chunk.go): the
// summaryrun store writes a run column that finalizeRun reads back. No second
// mechanism — one definition per mapping.
//
// Best-effort and never fatal. Failing a usable-but-degraded deliverable because
// its bookkeeping write failed would trade a partial answer for no answer, which
// the surrounding design explicitly refuses. A failed write is logged loudly.
func recordOutputTruncated(ctx context.Context, uid, runID string) {
	if !SummaryV2Enabled() || runID == "" {
		return
	}
	if uid == "" {
		log.Printf("[truncation] skip output truncation record: missing uid for run=%s", runID)
		return
	}
	summaryDB, _, _, _ := GetSummaryDeps()
	if summaryDB == nil {
		return
	}
	// WithoutCancel, mirroring recordFetch: this records a DEGRADATION, and one
	// live cause of degradation is a client that disconnected mid-answer, which
	// cancels this ctx. The record must not fail for the same reason the run did.
	recordCtx := context.WithoutCancel(ctx)
	if err := summaryrun.NewStore(summaryDB).MarkOutputTruncated(recordCtx, uid, runID); err != nil {
		log.Printf("[truncation] record output truncation failed run=%s: %v", runID, err)
	}
}

// recordOutputTruncatedFromContext is the convenience form for call sites that
// already carry uid/run_id in the tool context (the standard shape for tool
// handlers and the planner loop).
func recordOutputTruncatedFromContext(ctx context.Context) {
	uid, _ := ctx.Value(ContextKeyUID).(string)
	runID, _ := ctx.Value(ContextKeyRunID).(string)
	recordOutputTruncated(ctx, uid, runID)
}
