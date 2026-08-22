package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/finishgate"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/summaryrun"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// These tests cover the PRODUCING half of the truncation disclosure: the run
// row must carry the fact by the time the run ends, so the finish gate can
// disclose it structurally.
//
// The gap they close: PR #208 made a truncated Reduce detectable and appended
// service.TruncationNotice to the merge_summaries tool result. But that result
// is an INTERMEDIATE artifact handed back to the planner as context — the
// planner writes the final answer, and nothing forces the notice through. The
// test below models exactly that adversary: a planner that reads the truncated
// merge and then writes a clean, confident summary with the notice stripped.

// newTruncationRunDB builds an in-memory run store for the agent package.
func newTruncationRunDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Skipf("CGO required for sqlite: %v", err)
		return nil
	}
	if err := db.AutoMigrate(&model.AgentSummaryRun{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// withReduceLLMAndDB is withReduceLLM plus a real summary DB, so the recording
// path (which is a no-op without one) actually runs.
func withReduceLLMAndDB(t *testing.T, url string, db *gorm.DB) {
	t.Helper()
	prev := func() (cfg config.Config) {
		defer func() { _ = recover() }()
		_, _, _, cfg = GetSummaryDeps()
		return cfg
	}()
	cfg := prev
	cfg.LLMApiURL = url
	cfg.LLMApiKey = "test-key"
	cfg.LLMModel = "test-model"
	cfg.LLMTimeout = 5
	cfg.LLMMaxToken = 4096
	cfg.LLMEnableThinking = false
	SetSummaryDeps(db, nil, nil, cfg)
	t.Cleanup(func() { SetSummaryDeps(nil, nil, nil, prev) })
}

// noticeStrippingPlanner is the adversary. It performs Map then Reduce exactly
// like truncationPlannerClient, but its FINAL ANSWER is its own clean prose: it
// deliberately discards the truncation notice the Reduce disclosed to it.
//
// This is not a strawman. The notice reads as meta-commentary about the tool
// call rather than as summary content, so a model asked for a polished final
// answer has every reason to drop it.
type noticeStrippingPlanner struct {
	sawTruncatedMerge bool
}

func (c *noticeStrippingPlanner) Chat(_ context.Context, msgs []Message, _ []Tool) (AssistantTurn, error) {
	var handles []string
	merged := false
	for _, msg := range msgs {
		if msg.Role != "tool" {
			continue
		}
		switch msg.Name {
		case "summarize_chunk":
			var result struct {
				SummaryHandle string `json:"summary_handle"`
			}
			if json.Unmarshal([]byte(msg.Content), &result) == nil && result.SummaryHandle != "" {
				handles = append(handles, result.SummaryHandle)
			}
		case "merge_summaries":
			var result struct {
				MergedSummary string `json:"merged_summary"`
				Truncated     bool   `json:"truncated"`
			}
			if json.Unmarshal([]byte(msg.Content), &result) == nil && result.MergedSummary != "" {
				merged = true
				c.sawTruncatedMerge = result.Truncated
			}
		}
	}
	if merged {
		// The whole point: a clean answer with the disclosure rewritten away.
		return AssistantTurn{Content: "本周项目进展顺利，核心功能已全部交付，无风险项。"}, nil
	}
	if len(handles) > 0 {
		args, _ := json.Marshal(map[string]interface{}{"summary_handles": handles})
		return AssistantTurn{ToolCalls: []ToolCall{
			mkToolCall("reduce-call", "merge_summaries", string(args)),
		}}, nil
	}
	return AssistantTurn{ToolCalls: []ToolCall{
		mkToolCall("map-call", "summarize_chunk", `{}`),
	}}, nil
}

func registerTruncationTools(reg *Registry) {
	reg.Register(Tool{Type: "function", Function: ToolFunction{Name: "summarize_chunk"}},
		func(ctx context.Context, _ json.RawMessage) (string, error) {
			store, err := summaryHandleStoreFromContext(ctx)
			if err != nil {
				return "", err
			}
			handle, err := store.PutAtStep("局部总结正文", 3, summaryToolStepFromContext(ctx))
			if err != nil {
				return "", err
			}
			data, _ := json.Marshal(map[string]interface{}{"summary_handle": handle, "chunk_count": 3})
			return string(data), nil
		})
	reg.Register(MergeSummariesTool())
}

// cleanReduceServer answers with a COMPLETE response (finish_reason=stop). It is
// the control for the truncated server: same shape, no degradation.
func cleanReduceServer(t *testing.T, content string) *httptest.Server {
	t.Helper()
	body, err := json.Marshal(map[string]interface{}{
		"choices": []map[string]interface{}{{
			"message":       map[string]interface{}{"content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]interface{}{"total_tokens": 128, "completion_tokens": 64},
	})
	if err != nil {
		t.Fatalf("marshal stub response: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runTruncationScenario drives a full agent run against the given LLM stub and
// returns the final answer plus the run row the gate would later read.
func runTruncationScenario(t *testing.T, srvURL string, client chatter) (string, *model.AgentSummaryRun) {
	t.Helper()
	db := newTruncationRunDB(t)
	if db == nil {
		return "", nil
	}
	withReduceLLMAndDB(t, srvURL, db)
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")

	store := summaryrun.NewStore(db)
	ctx := context.Background()
	run, _, err := store.CreateOrGetRun(ctx, "u1", "sess1", "req-1", model.ScopePolicyClosed)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	ctx = context.WithValue(ctx, ContextKeyUID, "u1")
	ctx = context.WithValue(ctx, ContextKeyRunID, run.RunID)

	reg := NewRegistry()
	registerTruncationTools(reg)
	runner := NewRunner(client, reg, NewPool(2), Policy{MaxSteps: 8, MaxTokens: 1 << 20, StepTimeout: 5 * time.Second})
	out, _, err := runner.RunWithHistory(ctx, "system", nil, "summarize")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got, err := store.GetByID(ctx, "u1", run.RunID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	return out, got
}

// REQUIREMENT 1: the disclosure must not be defeatable by the model.
//
// The Reduce truncates. The planner reads the disclosure and throws it away,
// answering with confident, clean prose. The delivered text therefore carries NO
// warning at all — and yet the run must still evaluate to PARTIAL with a
// truncation gap, because the fact was latched on the run row where the model
// cannot reach it.
func TestOutputTruncationSurvivesPlannerDroppingTheNotice(t *testing.T) {
	srv, _ := truncatedReduceServer(t, "合并结果：项目进展、风险与待办（正文在此被截断")
	client := &noticeStrippingPlanner{}
	out, run := runTruncationScenario(t, srv.URL, client)
	if run == nil {
		return
	}

	if !client.sawTruncatedMerge {
		t.Fatal("test setup: the planner never saw a truncated merge result")
	}
	// Precondition for the whole test: the user-visible text really is clean.
	if strings.Contains(out, "输出因长度限制被截断") {
		t.Fatalf("test setup: the answer still carries the notice, so this proves nothing: %q", out)
	}

	if !run.OutputTruncated {
		t.Fatal("run row was not marked output-truncated: the ONLY disclosure was prose the model just deleted, so the user receives a silently unfinished summary")
	}

	// And that fact must actually produce a gap through the real gate.
	verdict, gaps := finishgate.Evaluate(finishgate.RunState{
		ScopeResolved:            true,
		HasUsableEvidence:        true,
		SummaryGenerated:         true,
		CitationValidationPassed: true,
		FetchExpected:            true,
		CoverageMeasured:         true,
		OutputTruncated:          run.OutputTruncated,
	})
	if verdict != finishgate.Partial {
		t.Fatalf("verdict = %s, want PARTIAL (gaps=%v)", verdict, gaps)
	}
	hasGap := false
	for _, g := range gaps {
		if g.Kind == finishgate.GapOutputTruncation {
			hasGap = true
		}
	}
	if !hasGap {
		t.Fatalf("gaps = %v, want %s", gaps, finishgate.GapOutputTruncation)
	}
}

// REQUIREMENT 4: the prose notice is NOT removed. Gate disclosure and inline
// notice are complementary — the gate is structured and reliable, the inline
// notice tells the reader WHERE the text stops. A planner that faithfully
// relays the merge result must still deliver the notice.
func TestOutputTruncationKeepsInlineNoticeForFaithfulPlanner(t *testing.T) {
	srv, _ := truncatedReduceServer(t, "合并结果：项目进展、风险与待办（正文在此被截断")
	client := &truncationPlannerClient{}
	out, run := runTruncationScenario(t, srv.URL, client)
	if run == nil {
		return
	}
	if !strings.Contains(out, "输出因长度限制被截断") {
		t.Fatalf("inline notice was lost; removing it is a regression: %q", out)
	}
	if !run.OutputTruncated {
		t.Fatal("run row must be marked even when the prose notice survives: both disclosures, not either")
	}
}

// REQUIREMENT 3: no false positives. An identical run whose Reduce completes
// normally must leave output_truncated false, so the gate keeps reporting
// COMPLETE. Without this control the feature could "pass" by marking every run.
func TestNoOutputTruncationRecordedForCleanReduce(t *testing.T) {
	srv := cleanReduceServer(t, "合并结果：项目进展、风险与待办，全文完整。")
	client := &truncationPlannerClient{}
	out, run := runTruncationScenario(t, srv.URL, client)
	if run == nil {
		return
	}
	if strings.Contains(out, "输出因长度限制被截断") {
		t.Fatalf("a clean Reduce must not disclose a truncation: %q", out)
	}
	if run.OutputTruncated {
		t.Fatal("clean run marked output-truncated: this would make PARTIAL the standing verdict for every healthy summary")
	}

	verdict, gaps := finishgate.Evaluate(finishgate.RunState{
		ScopeResolved:            true,
		HasUsableEvidence:        true,
		SummaryGenerated:         true,
		CitationValidationPassed: true,
		FetchExpected:            true,
		CoverageMeasured:         true,
		OutputTruncated:          run.OutputTruncated,
	})
	if verdict != finishgate.Complete {
		t.Fatalf("verdict = %s, want COMPLETE for a clean run (gaps=%v)", verdict, gaps)
	}
}
