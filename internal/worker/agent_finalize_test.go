package worker

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
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

// --- cross-turn citation drift (BLOCKING 1) -------------------------------

// evidenceRow builds one agent_message_evidence row holding the given messages,
// stamped at the moment that turn's tool call persisted it.
func evidenceRow(t *testing.T, handle string, at time.Time, msgs []pipeline.Message) model.AgentMessageEvidence {
	t.Helper()
	buf, err := json.Marshal(msgs)
	if err != nil {
		t.Fatalf("marshal evidence: %v", err)
	}
	return model.AgentMessageEvidence{
		UserID: "u1", SessionID: "s1", Handle: handle,
		Evidence:  string(buf),
		CreatedAt: at,
		UpdatedAt: at,
	}
}

func poolMsg(channel string, seq int64, ts int64, content string) pipeline.Message {
	return pipeline.Message{
		MessageSeq: seq, ChannelID: channel, Timestamp: ts,
		SenderUID: "sender-" + channel, SenderName: "Sender " + channel,
		Content: content,
	}
}

// THE regression the reviewers asked for by name.
//
// Turn 1 fetches #alpha (today) and cites [3] = alpha's 3rd message. Turn 3
// then fetches #beta (LAST WEEK) — evidence persisted LATER whose messages sort
// EARLIER. In the merged pool beta occupies 1..4 and alpha shifts to 5..7, so
// turn 1's preserved [3] would resolve to a BETA message: in range, so
// BuildCitations clamps nothing and logs nothing — confidently wrong
// attribution. The remap must rewrite it to alpha's message 3 new index.
func TestRemapFinalizeCitations_LaterEvidenceSortingEarlierDoesNotStealMarkers(t *testing.T) {
	turn1At := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	turn3At := turn1At.Add(2 * time.Hour)

	// alpha: today 10:00-ish (timestamps 1000..1002)
	alpha := []pipeline.Message{
		poolMsg("alpha", 1, 1000, "alpha-1"),
		poolMsg("alpha", 2, 1001, "alpha-2"),
		poolMsg("alpha", 3, 1002, "alpha-3 THE ONE"),
	}
	// beta: last week (timestamps 10..13) — sorts BEFORE all of alpha.
	beta := []pipeline.Message{
		poolMsg("beta", 1, 10, "beta-1"),
		poolMsg("beta", 2, 11, "beta-2"),
		poolMsg("beta", 3, 12, "beta-3"),
		poolMsg("beta", 4, 13, "beta-4"),
	}

	rows := []model.AgentMessageEvidence{
		evidenceRow(t, "msg_u1_1", turn1At, alpha),
		evidenceRow(t, "msg_u1_2", turn3At, beta),
	}
	finalPool := buildPoolFromEvidenceRows(rows)

	// Sanity: the merged pool really does put beta first, i.e. the drift
	// precondition holds. Without this the test could pass vacuously.
	if finalPool[0].ChannelID != "beta" {
		t.Fatalf("precondition failed: merged pool starts with %s, want beta (later-fetched, earlier-timestamped)",
			finalPool[0].ChannelID)
	}
	var alpha3Final int
	for _, m := range finalPool {
		if m.ChannelID == "alpha" && m.MessageSeq == 3 {
			alpha3Final = m.CitationIndex
		}
	}
	if alpha3Final == 3 {
		t.Fatal("precondition failed: alpha#3 still has index 3 in the merged pool, so there is no drift to remap")
	}

	replies := []model.AgentMessage{
		{ID: 1, CreatedAt: turn1At, Content: "确认了发布时间 [3]"},                             // turn 1 numbering
		{ID: 2, CreatedAt: turn3At, Content: "上周也讨论过 [1],今天确认 " + citeStr(alpha3Final)}, // turn 3 numbering
	}

	got, dropped := remapFinalizeCitations(replies, rows, finalPool)
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0 — every marker here is resolvable", dropped)
	}

	// Turn 1's [3] must now name alpha#3 in the merged numbering, NOT beta#3.
	want := citeStr(alpha3Final)
	if !strings.Contains(got[0].Content, want) {
		t.Fatalf("turn-1 fragment = %q, want it remapped to %s (alpha#3). Unremapped, [3] silently means %q",
			got[0].Content, want, finalPool[2].Content)
	}
	if strings.Contains(got[0].Content, "[3]") && want != "[3]" {
		t.Fatalf("turn-1 fragment still carries the stale [3]: %q", got[0].Content)
	}
	// Turn 3's markers were already in the final numbering; they must survive
	// unchanged (its own pool IS the final pool).
	if !strings.Contains(got[1].Content, "[1]") || !strings.Contains(got[1].Content, want) {
		t.Fatalf("turn-3 fragment lost markers: %q", got[1].Content)
	}
}

func citeStr(n int) string { return "[" + strconv.Itoa(n) + "]" }

// R4 blocking 1 REVISED THIS CONTRACT, deliberately.
//
// Round 3 deleted an ordinal that was out of range in its own turn's pool. That
// branch is what destroyed `GB/T 7714 [2020]` and `待办共 [3] 项`: an out-of-range
// number is, by construction, a number the turn could NOT have been citing, so
// treating it as a broken citation and deleting it is deleting prose. It is now
// left byte-identical.
//
// The fail-closed half is unchanged and is asserted in the sibling test below:
// a marker that DOES resolve to a real turn-pool message but whose message is
// absent from the frozen merged pool is still dropped, because that one really
// is a citation and a wrong citation is worse than a missing one.
func TestRemapFinalizeCitations_OutOfRangeOrdinalIsTreatedAsProse(t *testing.T) {
	at := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	rows := []model.AgentMessageEvidence{
		evidenceRow(t, "msg_u1_1", at, []pipeline.Message{
			poolMsg("alpha", 1, 1000, "alpha-1"),
			poolMsg("alpha", 2, 1001, "alpha-2"),
		}),
	}
	finalPool := buildPoolFromEvidenceRows(rows)

	// [9] is out of range in the reply's own turn pool (which holds 2 messages),
	// so it cannot be a citation this turn emitted — it is prose.
	replies := []model.AgentMessage{
		{ID: 1, CreatedAt: at, Content: "有效 [1],越界 [9]"},
	}
	got, dropped := remapFinalizeCitations(replies, rows, finalPool)
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0 — an out-of-range ordinal is content, not a broken citation", dropped)
	}
	if !strings.Contains(got[0].Content, "[9]") {
		t.Fatalf("out-of-range token was deleted (this is the R4 blocking-1 defect): %q", got[0].Content)
	}
	if !strings.Contains(got[0].Content, "[1]") {
		t.Fatalf("resolvable marker was lost: %q", got[0].Content)
	}
}

// The other unresolvable case: the marker resolves to a real message in its own
// turn, but that message is absent from the frozen merged pool. It must be
// dropped rather than silently re-pointed.
func TestRemapFinalizeCitations_MarkerAbsentFromFinalPoolIsDropped(t *testing.T) {
	at := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	turnRows := []model.AgentMessageEvidence{
		evidenceRow(t, "msg_u1_1", at, []pipeline.Message{
			poolMsg("alpha", 1, 1000, "alpha-1"),
			poolMsg("gone", 7, 1001, "evicted from the frozen pool"),
		}),
	}
	// The frozen merged pool lacks the "gone" channel entirely.
	finalPool := buildPoolFromEvidenceRows([]model.AgentMessageEvidence{
		evidenceRow(t, "msg_u1_1", at, []pipeline.Message{poolMsg("alpha", 1, 1000, "alpha-1")}),
	})

	replies := []model.AgentMessage{{ID: 1, CreatedAt: at, Content: "保留 [1],悬空 [2]"}}
	got, dropped := remapFinalizeCitations(replies, turnRows, finalPool)
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1 (the marker whose message is not in the frozen pool)", dropped)
	}
	if strings.Contains(got[0].Content, "[2]") {
		t.Fatalf("dangling marker survived: %q", got[0].Content)
	}
	if !strings.Contains(got[0].Content, "[1]") {
		t.Fatalf("resolvable marker was lost: %q", got[0].Content)
	}
}

// A single-turn session must be a no-op: its own pool IS the final pool, so
// every marker maps to itself. This pins that the remap does not disturb the
// common case.
func TestRemapFinalizeCitations_SingleTurnIsIdentity(t *testing.T) {
	at := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	rows := []model.AgentMessageEvidence{
		evidenceRow(t, "msg_u1_1", at, []pipeline.Message{
			poolMsg("alpha", 1, 1000, "a"),
			poolMsg("alpha", 2, 1001, "b"),
			poolMsg("alpha", 3, 1002, "c"),
		}),
	}
	finalPool := buildPoolFromEvidenceRows(rows)
	replies := []model.AgentMessage{{ID: 1, CreatedAt: at, Content: "x [1] y [3]"}}
	got, dropped := remapFinalizeCitations(replies, rows, finalPool)
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0", dropped)
	}
	if got[0].Content != "x [1] y [3]" {
		t.Fatalf("single-turn remap changed the fragment: %q", got[0].Content)
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
