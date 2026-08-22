package worker

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

func TestBuildFinalizeConsolidationPrompt(t *testing.T) {
	replies := []model.AgentMessage{
		{Content: "第一段:讨论了 A [1]"},
		{Content: "第二段:结论是 B [2]"},
	}
	p := buildFinalizeConsolidationPrompt("会议纪要", replies, 0)

	// Fragments appear, in order.
	iA := strings.Index(p, "讨论了 A [1]")
	iB := strings.Index(p, "结论是 B [2]")
	if iA < 0 || iB < 0 {
		t.Fatalf("prompt missing fragment content:\n%s", p)
	}
	if iA > iB {
		t.Fatalf("fragments out of order (片段1 must precede 片段2)")
	}
	// Title woven in.
	if !strings.Contains(p, "会议纪要") {
		t.Fatalf("prompt missing the confirmed title")
	}
	// The load-bearing instruction: citation markers must be preserved.
	if !strings.Contains(p, "严格保留引用") {
		t.Fatalf("prompt must instruct verbatim [n] preservation")
	}
	// It must be a MERGE task, not a re-summarize-from-raw task.
	if !strings.Contains(p, "合并") {
		t.Fatalf("prompt must frame the task as consolidation/merge")
	}
}

func TestBuildFinalizeConsolidationPrompt_NoTitle(t *testing.T) {
	p := buildFinalizeConsolidationPrompt("   ", []model.AgentMessage{{Content: "只有一段"}}, 0)
	if strings.Contains(p, "用户确认的标题") {
		t.Fatalf("blank title must not emit the title section")
	}
	if !strings.Contains(p, "只有一段") {
		t.Fatalf("prompt missing the single fragment")
	}
}

// --- Session-Finalize v0 worker-side behaviour ----------------------------

// The prompt must disclose an over-budget truncation. Silently dropping the
// oldest fragments while the model claims to summarize the whole session is the
// same class of defect as a silently truncated Map chunk.
func TestBuildFinalizeConsolidationPrompt_DisclosesDroppedFragments(t *testing.T) {
	p := buildFinalizeConsolidationPrompt("标题", []model.AgentMessage{{Content: "保留的片段"}}, 3)
	if !strings.Contains(p, "未纳入") {
		t.Fatalf("prompt must disclose that older fragments were dropped:\n%s", p)
	}
	if !strings.Contains(p, "3") {
		t.Fatalf("prompt should name how many fragments were dropped:\n%s", p)
	}
	if !strings.Contains(p, "不要声称覆盖了整场会话") {
		t.Fatalf("prompt must forbid claiming full coverage after a drop:\n%s", p)
	}
}

// Under budget nothing is dropped and no disclosure is emitted — the common case
// must not carry a scary notice.
func TestBudgetFinalizeReplies_UnderBudgetKeepsEverything(t *testing.T) {
	p := &Processor{cfg: &config.Config{LLMModel: "test-model", CharsPerTokenASCII: 4, CharsPerTokenCJK: 1}}
	replies := []model.AgentMessage{{Content: "一"}, {Content: "二"}, {Content: "三"}}
	got, dropped := p.budgetFinalizeReplies("标题", replies)
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0 for a tiny session", dropped)
	}
	if len(got) != len(replies) {
		t.Fatalf("kept %d fragments, want all %d", len(got), len(replies))
	}
}

// Over budget, the OLDEST fragments go first: the newest replies are the
// session's most refined conclusions and the merge prompt already treats the
// latest statement as authoritative.
func TestBudgetFinalizeReplies_DropsOldestFirst(t *testing.T) {
	// MapMaxTokens is explicit so the test does not depend on per-model defaults.
	// systemPromptOverhead (3000) is subtracted inside, so the budget below leaves
	// room for roughly one of these fragments.
	big := strings.Repeat("x", 4000) // ~1000 tokens at 4 chars/token
	p := &Processor{cfg: &config.Config{
		LLMModel: "test-model", MapMaxTokens: 3000 + 1200,
		CharsPerTokenASCII: 4, CharsPerTokenCJK: 1,
	}}
	replies := []model.AgentMessage{
		{Content: "OLDEST" + big},
		{Content: "MIDDLE" + big},
		{Content: "NEWEST" + big},
	}
	got, dropped := p.budgetFinalizeReplies("", replies)
	if dropped == 0 {
		t.Fatalf("expected an over-budget drop, got none (kept %d)", len(got))
	}
	if len(got) == 0 {
		t.Fatal("budgeting must never produce an empty prompt")
	}
	if !strings.HasPrefix(got[len(got)-1].Content, "NEWEST") {
		t.Fatalf("newest fragment must survive, got last=%.10q", got[len(got)-1].Content)
	}
	if strings.HasPrefix(got[0].Content, "OLDEST") {
		t.Fatalf("oldest fragment must be dropped first, but it survived")
	}
	if dropped+len(got) != len(replies) {
		t.Fatalf("dropped(%d) + kept(%d) != total(%d)", dropped, len(got), len(replies))
	}
}

// Even a single fragment larger than the whole budget must still be sent: an
// empty prompt is strictly worse than an over-budget one, and the LLM error path
// already surfaces the overflow loudly.
func TestBudgetFinalizeReplies_NeverEmptiesThePrompt(t *testing.T) {
	p := &Processor{cfg: &config.Config{
		LLMModel: "test-model", MapMaxTokens: 3001,
		CharsPerTokenASCII: 4, CharsPerTokenCJK: 1,
	}}
	replies := []model.AgentMessage{{Content: strings.Repeat("y", 100000)}}
	got, _ := p.budgetFinalizeReplies("", replies)
	if len(got) != 1 {
		t.Fatalf("kept %d fragments, want the single oversized one", len(got))
	}
}

// --- P2-7: the TriggerAgentFinalize routing branch ------------------------

// A finalize task must NOT run executePipeline. executePipeline exists to
// discover channels and fetch raw messages; a finalize task has neither, and
// running it would pay the exact discovery + intent-LLM + zero-width-fetch cost
// this feature exists to avoid — and could fail the finalize for reasons that
// have nothing to do with finalizing.
func TestTaskExecutor_FinalizeTaskDoesNotUsePipeline(t *testing.T) {
	p := &Processor{}

	finalize := p.taskExecutor(model.SummaryTask{TriggerType: model.TriggerAgentFinalize})
	if reflect.ValueOf(finalize).Pointer() != reflect.ValueOf(p.executeFinalizeTask).Pointer() {
		t.Fatal("a TriggerAgentFinalize task must route to executeFinalizeTask")
	}
	if reflect.ValueOf(finalize).Pointer() == reflect.ValueOf(p.executePipeline).Pointer() {
		t.Fatal("a TriggerAgentFinalize task must NOT route to executePipeline")
	}

	// Every other trigger keeps the legacy pipeline.
	for _, tt := range []int{model.TriggerManual, model.TriggerScheduled, model.TriggerAgent} {
		got := p.taskExecutor(model.SummaryTask{TriggerType: tt})
		if reflect.ValueOf(got).Pointer() != reflect.ValueOf(p.executePipeline).Pointer() {
			t.Fatalf("trigger_type %d must still route to executePipeline", tt)
		}
	}
}

// The test seam still wins when injected, so the existing pipeline tests keep
// driving processTask deterministically.
func TestTaskExecutor_InjectedSeamWinsOverFinalizeRouting(t *testing.T) {
	called := false
	p := &Processor{executePipelineFn: func(model.SummaryTask) error { called = true; return nil }}
	if err := p.taskExecutor(model.SummaryTask{TriggerType: model.TriggerAgentFinalize})(model.SummaryTask{}); err != nil {
		t.Fatalf("seam returned error: %v", err)
	}
	if !called {
		t.Fatal("injected executePipelineFn must win over the finalize routing branch")
	}
}

// P2-2: executeFinalizeTask must NOT bootstrap the creator participant. The
// handler already created it in the same tx as the task, so the call was a
// guaranteed unique-key conflict on every run — and bootstrapCreatorParticipant
// decides insert-vs-conflict via RowsAffected == 0, which under
// clientFoundRows=true leaves participant.ID == 0 and writes an orphan
// personal_result with participant_ref_id = 0. processTask bootstraps
// defensively one line later anyway.
//
// A nil db proves it: if executeFinalizeTask touched the DB at all this would
// panic instead of returning nil.
func TestExecuteFinalizeTask_DoesNotTouchTheDatabase(t *testing.T) {
	p := &Processor{} // db is nil on purpose
	if err := p.executeFinalizeTask(model.SummaryTask{ID: 1, AgentSessionID: "s1"}); err != nil {
		t.Fatalf("executeFinalizeTask must be a pure validation step, got: %v", err)
	}
	if err := p.executeFinalizeTask(model.SummaryTask{ID: 2}); err == nil {
		t.Fatal("a finalize task with no agent_session_id must be rejected")
	}
}
