//go:build cgo
// +build cgo

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/finishgate"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/summaryrun"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

type handlerTruncatedPlanner struct{}

func (*handlerTruncatedPlanner) Chat(context.Context, []agent.Message, []agent.Tool) (agent.AssistantTurn, error) {
	return agent.AssistantTurn{Content: "truncated final answer", Truncated: true}, nil
}

type handlerSequencePlanner struct {
	turns []agent.AssistantTurn
	errs  []error
	call  int
}

func TestMaybePersistSummaryRunCanonicalizesDMChannelID(t *testing.T) {
	db := newFinalizeTestDB(t)
	if db == nil {
		return
	}
	store := summaryrun.NewStore(db)
	h := &AgentChatHandler{runStore: store}
	ctx := context.Background()
	const uid = "self-uid"
	req := agentChatRequest{
		Message:   "总结私聊",
		SessionID: "dm-spec-session",
		RequestID: "dm-spec-request",
		SelectedChannels: []selectedChannel{{
			ChannelID: "peer-uid", ChannelType: "direct", Name: "同事",
		}},
	}

	runID := h.maybePersistSummaryRun(ctx, uid, req, true)
	spec, found, err := store.GetLatestSpec(ctx, uid, runID)
	if err != nil || !found || len(spec.Channels) != 1 {
		t.Fatalf("load persisted spec: found=%t channels=%v err=%v", found, spec.Channels, err)
	}
	want := pipeline.NormalizeDMChannelID("peer-uid", uid, model.ChannelTypeDM)
	if spec.Channels[0].ChannelID != want {
		t.Fatalf("persisted DM channel_id = %q, want canonical %q", spec.Channels[0].ChannelID, want)
	}
}

func (p *handlerSequencePlanner) Chat(context.Context, []agent.Message, []agent.Tool) (agent.AssistantTurn, error) {
	i := p.call
	p.call++
	if i < len(p.errs) && p.errs[i] != nil {
		return agent.AssistantTurn{}, p.errs[i]
	}
	if i >= len(p.turns) {
		return agent.AssistantTurn{}, errors.New("unexpected planner call")
	}
	return p.turns[i], nil
}

// This pins the production wiring the lower-level agent tests cannot prove:
// both HTTP handlers must put the authenticated uid and persisted run id on the
// exact context handed to Runner.RunWithHistory.
func TestAgentChatHandlersRecordPlannerOutputTruncation(t *testing.T) {
	db := newFinalizeTestDB(t)
	if db == nil {
		return
	}
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")
	agent.SetSummaryDeps(db, nil, nil, config.Config{})
	t.Cleanup(func() { agent.SetSummaryDeps(nil, nil, nil, config.Config{}) })

	store := summaryrun.NewStore(db)
	reg := agent.NewRegistry()
	runner := agent.NewRunner(&handlerTruncatedPlanner{}, reg, agent.NewPool(1), agent.Policy{
		MaxSteps: 1, MaxTokens: 1000, StepTimeout: time.Second,
	})
	historyStore := newFakeHistoryStore()
	handler := newAgentChatHandlerWithRunner(runner, "test-system", historyStore, 10)
	handler.runStore = store
	router := setupAgentChatRouter(handler)

	tests := []struct {
		name      string
		path      string
		sessionID string
		requestID string
	}{
		{name: "chat", path: "/api/v1/agent/chat", sessionID: "sess-handler-chat", requestID: "req-handler-chat"},
		{name: "stream", path: "/api/v1/agent/chat/stream", sessionID: "sess-handler-stream", requestID: "req-handler-stream"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			body := strings.NewReader(`{"message":"summarize","session_id":"` + tc.sessionID + `","request_id":"` + tc.requestID + `","profile":"summary"}`)
			req := httptest.NewRequest(http.MethodPost, tc.path, body)
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
			}

			run, err := store.GetByRequest(context.Background(), testAgentChatUID, tc.sessionID, tc.requestID)
			if err != nil {
				t.Fatalf("get run: %v", err)
			}
			if !run.OutputTruncated {
				t.Fatal("planner truncation was not recorded through the real handler context")
			}
			history, err := historyStore.LoadHistory(context.Background(), tc.sessionID, testAgentChatUID)
			if err != nil {
				t.Fatalf("load persisted history: %v", err)
			}
			if len(history) == 0 {
				t.Fatal("handler did not persist the final assistant message")
			}
			final := history[len(history)-1]
			if final.RunID != run.RunID || !final.OutputTruncated {
				t.Fatalf("final assistant metadata = {run:%q truncated:%t}, want {%q true}", final.RunID, final.OutputTruncated, run.RunID)
			}
		})
	}
}

// THE LOAD-BEARING TEST: the truncation disclosure must not be defeatable by
// the model.
//
// The failure this guards against is specific. When a Reduce is length-
// truncated, merge_summaries appends service.TruncationNotice to the
// `merged_summary` field of a TOOL RESULT. That tool result is not the
// deliverable — it is context handed back to the planner, which then writes the
// final user-facing answer in its own words. Nothing forces the notice through.
// It reads like meta-commentary rather than content, so a model producing a
// polished final answer will plausibly drop it, and the user receives a summary
// that is silently unfinished while looking complete.
//
// So: simulate exactly that. The run is marked output-truncated (as the
// producing paths do), and the content passed to finalizeRun is a clean final
// answer containing NO notice — the model rewrote it away. The verdict must
// still be PARTIAL with a truncation gap, because the gate assembles its
// disclosure from the persisted run row, outside the model's control.
func TestFinalizeRunDisclosesOutputTruncationDespiteCleanAnswer(t *testing.T) {
	db := newFinalizeTestDB(t)
	if db == nil {
		return
	}
	store := summaryrun.NewStore(db)
	ctx := context.Background()
	run, _, err := store.CreateOrGetRun(ctx, "u1", "sess1", "req-trunc", model.ScopePolicyClosed)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := db.Model(&model.AgentSummaryRun{}).Where("run_id = ?", run.RunID).
		Update("spec_id", "spec-1").Error; err != nil {
		t.Fatalf("set spec_id: %v", err)
	}
	// Full, clean coverage: every channel in scope was fetched successfully and
	// nothing was dropped. The ONLY defect in this run is that the model's own
	// output was cut off, so any gap reported here must come from that fact.
	if err := store.RecordChannelFetch(ctx, "u1", run.RunID, "ch-1", true, false); err != nil {
		t.Fatalf("record fetch: %v", err)
	}

	// The producing side (merge_summaries / the planner loop) latches the fact.
	if err := store.MarkOutputTruncated(ctx, "u1", run.RunID); err != nil {
		t.Fatalf("mark output truncated: %v", err)
	}

	// The model's final answer. Note what is NOT here: no TruncationNotice, no
	// hint of degradation, no ellipsis. This is a confident, well-formed summary
	// — precisely what a planner emits after quietly discarding the notice.
	cleanAnswer := "本周项目进展顺利，核心功能已全部交付 [1]。"
	if strings.Contains(cleanAnswer, strings.TrimSpace(service.TruncationNotice)) {
		t.Fatal("test setup is wrong: the answer must contain NO prose notice")
	}

	h := &AgentSummaryHandler{db: db}
	verdict, gaps := h.finalizeRun(ctx, "u1", "sess1", "req-trunc", cleanAnswer, []model.Citation{{Index: 1}})

	if verdict != finishgate.Partial {
		t.Fatalf("verdict = %s, want PARTIAL: the model dropped the prose notice, so the gate is the only thing standing between the user and a silently unfinished summary (gaps=%v)", verdict, gaps)
	}
	if !hasGapKind(gaps, finishgate.GapOutputTruncation) {
		t.Fatalf("gaps = %v, want a %s gap assembled from the run row rather than from model-authored text", gaps, finishgate.GapOutputTruncation)
	}
	// Coverage was perfect; the disclosure must name the right failure and not
	// send the user off to narrow a time range that was never the problem.
	if hasGapKind(gaps, finishgate.GapTruncation) {
		t.Fatalf("gaps = %v: an OUTPUT truncation must not be reported as a fetch-pool truncation", gaps)
	}
}

// The assistant-row flag is the authoritative source for new messages. This
// remains safe even if the older best-effort run-row write was lost: content and
// its degradation flag are inserted together by AppendMessages.
func TestFinalizeRunUsesMessageBoundOutputTruncation(t *testing.T) {
	db := newFinalizeTestDB(t)
	if db == nil {
		return
	}
	store := summaryrun.NewStore(db)
	ctx := context.Background()
	run, _, err := store.CreateOrGetRun(ctx, "u1", "sess-msg-bound", "req-msg-bound", model.ScopePolicyClosed)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := db.Model(&model.AgentSummaryRun{}).Where("run_id = ?", run.RunID).
		Update("spec_id", "spec-1").Error; err != nil {
		t.Fatalf("set spec_id: %v", err)
	}
	if err := store.RecordChannelFetch(ctx, "u1", run.RunID, "ch-1", true, false); err != nil {
		t.Fatalf("record coverage: %v", err)
	}

	msg := model.AgentMessage{RunID: run.RunID, OutputTruncated: true}
	verdict, gaps := (&AgentSummaryHandler{db: db}).finalizeRunForMessage(
		ctx, "u1", "sess-msg-bound", "req-msg-bound", "外表完整但实际截断的总结。", nil, msg,
	)
	if verdict != finishgate.Partial || !hasGapKind(gaps, finishgate.GapOutputTruncation) {
		t.Fatalf("message-bound verdict = %s gaps=%v, want PARTIAL with output_truncation", verdict, gaps)
	}
	if got, err := store.GetByID(ctx, "u1", run.RunID); err != nil {
		t.Fatalf("reload run: %v", err)
	} else if got.OutputTruncated {
		t.Fatal("test precondition broken: run aggregate should remain false")
	}
}

func TestResolveAgentMessageRequestIDUsesPersistedRunBinding(t *testing.T) {
	db := newFinalizeTestDB(t)
	if db == nil {
		return
	}
	store := summaryrun.NewStore(db)
	ctx := context.Background()
	run, _, err := store.CreateOrGetRun(ctx, "u1", "sess-bound", "req-bound", model.ScopePolicyClosed)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	msg := model.AgentMessage{RunID: run.RunID}

	if got, err := resolveAgentMessageRequestID(ctx, db, "u1", "sess-bound", "", msg); err != nil || got != "req-bound" {
		t.Fatalf("derive request id = %q err=%v, want req-bound", got, err)
	}
	if got, err := resolveAgentMessageRequestID(ctx, db, "u1", "sess-bound", "req-bound", msg); err != nil || got != "req-bound" {
		t.Fatalf("matching request id = %q err=%v, want req-bound", got, err)
	}
	if _, err := resolveAgentMessageRequestID(ctx, db, "u1", "sess-bound", "req-other", msg); !errors.Is(err, errAgentMessageRunMismatch) {
		t.Fatalf("explicit mismatch err=%v, want errAgentMessageRunMismatch", err)
	}
	if _, err := resolveAgentMessageRequestID(ctx, db, "u1", "sess-other", "", msg); !errors.Is(err, errAgentMessageRunMismatch) {
		t.Fatalf("session mismatch err=%v, want errAgentMessageRunMismatch", err)
	}
	if got, err := resolveAgentMessageRequestID(ctx, db, "u1", "sess-bound", "legacy-req", model.AgentMessage{}); err != nil || got != "legacy-req" {
		t.Fatalf("legacy fallback = %q err=%v, want legacy-req", got, err)
	}
}

// NO FALSE POSITIVES (requirement 3). The same run shape with nothing truncated
// must stay COMPLETE. gate.go documents a prior bug where a new disclosure path
// reported correct behaviour as a defect; this is the guard against repeating it
// and it is the exact control for the test above — identical setup minus the
// MarkOutputTruncated call.
func TestFinalizeRunCleanRunStaysCompleteWithoutOutputTruncation(t *testing.T) {
	db := newFinalizeTestDB(t)
	if db == nil {
		return
	}
	store := summaryrun.NewStore(db)
	ctx := context.Background()
	run, _, err := store.CreateOrGetRun(ctx, "u1", "sess1", "req-clean", model.ScopePolicyClosed)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := db.Model(&model.AgentSummaryRun{}).Where("run_id = ?", run.RunID).
		Update("spec_id", "spec-1").Error; err != nil {
		t.Fatalf("set spec_id: %v", err)
	}
	if err := store.RecordChannelFetch(ctx, "u1", run.RunID, "ch-1", true, false); err != nil {
		t.Fatalf("record fetch: %v", err)
	}

	h := &AgentSummaryHandler{db: db}
	verdict, gaps := h.finalizeRun(ctx, "u1", "sess1", "req-clean", "本周项目进展顺利 [1]。", []model.Citation{{Index: 1}})
	if verdict != finishgate.Complete {
		t.Fatalf("verdict = %s, want COMPLETE for a run with no truncation anywhere (gaps=%v)", verdict, gaps)
	}
	if len(gaps) != 0 {
		t.Fatalf("a clean run must disclose nothing, got %v", gaps)
	}
}

// MarkOutputTruncated is idempotent and owner-scoped. The within-attempt latch
// semantics are covered by the rejected-premature and repeated-Reduce tests;
// the replay-only reset boundary is covered separately below.
func TestMarkOutputTruncatedIsIdempotentAndOwnerScoped(t *testing.T) {
	db := newFinalizeTestDB(t)
	if db == nil {
		return
	}
	store := summaryrun.NewStore(db)
	ctx := context.Background()
	run, _, err := store.CreateOrGetRun(ctx, "u1", "sess1", "req-latch", model.ScopePolicyClosed)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	if got, err := store.GetByID(ctx, "u1", run.RunID); err != nil {
		t.Fatalf("get run: %v", err)
	} else if got.OutputTruncated {
		t.Fatal("a fresh run must not be marked output-truncated")
	}

	// Mark twice: the second call is a no-op, not an error.
	for i := 0; i < 2; i++ {
		if err := store.MarkOutputTruncated(ctx, "u1", run.RunID); err != nil {
			t.Fatalf("mark #%d: %v", i+1, err)
		}
	}
	got, err := store.GetByID(ctx, "u1", run.RunID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if !got.OutputTruncated {
		t.Fatal("output_truncated must stay latched after repeated marks")
	}

	// Owner scoping: another user's mark must not touch this run, and must not
	// error (it simply matches no row).
	if err := store.MarkOutputTruncated(ctx, "u2", run.RunID); err != nil {
		t.Fatalf("cross-owner mark should be a silent no-op, got: %v", err)
	}
}

func postOutputTruncationChat(t *testing.T, router http.Handler, sessionID, requestID string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	body := strings.NewReader(`{"message":"总结项目进展","session_id":"` + sessionID + `","request_id":"` + requestID + `","profile":"summary","selected_channels":[{"chat_id":"ch-1","chat_type":"group","name":"项目群"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/chat", body)
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)
	return w
}

// A replay that FAILS must not mutate the degradation attached to the previous
// persisted deliverable. This is the exact SSE window from the CR: attempt A's
// assistant row commits, the done event is lost, attempt B starts and fails,
// then save still resolves A and must report its truncation.
func TestReplayFailureKeepsPreviousMessageOutputTruncation(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	if err := db.AutoMigrate(
		&model.AgentSummaryRun{},
		&model.AgentSummarySpec{},
		&model.AgentEvidenceArtifact{},
		&model.AgentCitationManifest{},
	); err != nil {
		t.Fatalf("migrate V2 tables: %v", err)
	}
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")
	agent.SetSummaryDeps(db, nil, nil, config.Config{})
	t.Cleanup(func() { agent.SetSummaryDeps(nil, nil, nil, config.Config{}) })

	store := summaryrun.NewStore(db)
	ctx := context.Background()
	planner := &handlerSequencePlanner{
		turns: []agent.AssistantTurn{
			{Content: "截断但外表完整的回复。", Truncated: true},
			{},
		},
		errs: []error{nil, errors.New("attempt B failed")},
	}
	runner := agent.NewRunner(planner, agent.NewRegistry(), agent.NewPool(1), agent.Policy{
		MaxSteps: 1, MaxTokens: 1000, StepTimeout: time.Second,
	})
	h := newAgentChatHandlerWithRunner(runner, "test-system", newAgentMessageRepo(db), 10)
	h.db = db
	h.runStore = store
	router := setupAgentChatRouter(h)
	const sessionID = "sess-trunc-replay-fail"
	const requestID = "req-trunc-replay-fail"

	if w := postOutputTruncationChat(t, router, sessionID, requestID); w.Code != http.StatusOK {
		t.Fatalf("attempt A status = %d, body=%s", w.Code, w.Body.String())
	}
	run, err := store.GetByRequest(ctx, testAgentChatUID, sessionID, requestID)
	if err != nil {
		t.Fatalf("get run after attempt A: %v", err)
	}
	if err := store.RecordChannelFetch(ctx, testAgentChatUID, run.RunID, "ch-1", true, false); err != nil {
		t.Fatalf("record coverage: %v", err)
	}

	if w := postOutputTruncationChat(t, router, sessionID, requestID); w.Code != http.StatusInternalServerError {
		t.Fatalf("attempt B status = %d, want 500, body=%s", w.Code, w.Body.String())
	}

	draft, err := loadAgentMessageForSave(db, sessionID, testAgentChatUID, 0)
	if err != nil {
		t.Fatalf("load persisted attempt A: %v", err)
	}
	if draft.RunID != run.RunID || !draft.OutputTruncated {
		t.Fatalf("persisted A metadata = {run:%q truncated:%t}, want {%q true}", draft.RunID, draft.OutputTruncated, run.RunID)
	}

	// Exercise the real save endpoint, not just finalizeRunForMessage. The
	// currently deployed client omits request_id on save, so the handler must
	// derive it from A's persisted RunID for binding validation/finalization.
	// Citation building deliberately keeps the original empty request_id and
	// therefore preserves the legacy recompute contract.
	saveBody, err := json.Marshal(map[string]interface{}{
		"session_id":          sessionID,
		"origin_channel_id":   "ch-1",
		"origin_channel_type": 1,
	})
	if err != nil {
		t.Fatalf("encode save request: %v", err)
	}
	saveHandler := NewAgentSummaryHandler(db, nil, "", "", "", 0, 0)
	w := httptest.NewRecorder()
	saveReq := httptest.NewRequest(http.MethodPost, "/api/v1/summaries/agent", bytes.NewReader(saveBody))
	saveReq.Header.Set("Content-Type", "application/json")
	saveReq.Header.Set("Token", testAgentChatUID)
	saveReq.Header.Set("X-Space-Id", "test-space")
	setupAgentSummaryRouter(saveHandler).ServeHTTP(w, saveReq)
	if w.Code != http.StatusOK {
		t.Fatalf("save after failed replay status = %d, body=%s", w.Code, w.Body.String())
	}
	var response struct {
		Data struct {
			FinishStatus string           `json:"finish_status"`
			Gaps         []finishgate.Gap `json:"gaps"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode save response: %v", err)
	}
	if response.Data.FinishStatus != string(finishgate.Partial) || !hasGapKind(response.Data.Gaps, finishgate.GapOutputTruncation) {
		t.Fatalf("failed replay save response = %s, want PARTIAL with output_truncation", w.Body.String())
	}
	var saved model.PersonalResult
	if err := db.First(&saved).Error; err != nil {
		t.Fatalf("load saved deliverable: %v", err)
	}
	if saved.Content != draft.Content {
		t.Fatalf("saved content = %q, want attempt A %q", saved.Content, draft.Content)
	}
}

// If attempt A recorded only the conservative run latch but never persisted a
// message, a successful clean attempt B is the first real deliverable. Its
// message-bound false value must override the stale run aggregate and restore a
// COMPLETE verdict.
func TestCleanReplayMessageOverridesAbandonedRunLatch(t *testing.T) {
	db := newFinalizeTestDB(t)
	if db == nil {
		return
	}
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")
	agent.SetSummaryDeps(db, nil, nil, config.Config{})
	t.Cleanup(func() { agent.SetSummaryDeps(nil, nil, nil, config.Config{}) })

	store := summaryrun.NewStore(db)
	ctx := context.Background()
	const sessionID = "sess-trunc-replay-clean"
	const requestID = "req-trunc-replay-clean"
	req := agentChatRequest{
		Message:   "总结项目进展",
		SessionID: sessionID,
		RequestID: requestID,
		SelectedChannels: []selectedChannel{{
			ChannelID: "ch-1", ChannelType: "group", Name: "项目群",
		}},
	}
	h := &AgentChatHandler{runStore: store}
	runID := h.maybePersistSummaryRun(ctx, testAgentChatUID, req, true)
	if runID == "" {
		t.Fatal("attempt A did not create a run")
	}
	if err := store.MarkOutputTruncated(ctx, testAgentChatUID, runID); err != nil {
		t.Fatalf("mark abandoned attempt A truncated: %v", err)
	}

	planner := &handlerSequencePlanner{turns: []agent.AssistantTurn{{Content: "完整回复 [1]。"}}}
	runner := agent.NewRunner(planner, agent.NewRegistry(), agent.NewPool(1), agent.Policy{
		MaxSteps: 1, MaxTokens: 1000, StepTimeout: time.Second,
	})
	h = newAgentChatHandlerWithRunner(runner, "test-system", newAgentMessageRepo(db), 10)
	h.db = db
	h.runStore = store
	if w := postOutputTruncationChat(t, setupAgentChatRouter(h), sessionID, requestID); w.Code != http.StatusOK {
		t.Fatalf("attempt B status = %d, body=%s", w.Code, w.Body.String())
	}
	if err := store.RecordChannelFetch(ctx, testAgentChatUID, runID, "ch-1", true, false); err != nil {
		t.Fatalf("record coverage: %v", err)
	}

	draft, err := loadAgentMessageForSave(db, sessionID, testAgentChatUID, 0)
	if err != nil {
		t.Fatalf("load clean attempt B: %v", err)
	}
	if draft.RunID != runID || draft.OutputTruncated {
		t.Fatalf("persisted B metadata = {run:%q truncated:%t}, want {%q false}", draft.RunID, draft.OutputTruncated, runID)
	}

	verdict, gaps := (&AgentSummaryHandler{db: db}).finalizeRunForMessage(
		ctx, testAgentChatUID, sessionID, requestID, draft.Content, []model.Citation{{Index: 1}}, draft,
	)
	if verdict != finishgate.Complete {
		t.Fatalf("clean replay verdict = %s, want COMPLETE (gaps=%v)", verdict, gaps)
	}
	if hasGapKind(gaps, finishgate.GapOutputTruncation) {
		t.Fatalf("clean replay inherited stale output_truncation gap: %v", gaps)
	}

}
