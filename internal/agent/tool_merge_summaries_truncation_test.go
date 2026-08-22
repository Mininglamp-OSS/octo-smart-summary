package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

// PR #208 round-4 B2/P1. A length-truncated agent Reduce used to hard-fail the
// whole request with ZERO output and no way to recover:
//
//	merge_summaries -> CallStrict -> any finish_reason=length is an error, the
//	non-empty partial merge is DISCARDED -> the handler returns before
//	store.MarkReduced -> NeedsReduce() stays true -> tool_error marks every
//	merge_summaries failure fatal -> the runner gate rejects every final answer
//	and nudges a retry -> the retry is byte-identical (same handles, same
//	combined text, same temperature; the tool exposes no parameter that could
//	shrink the output) -> it truncates again every step until MaxSteps.
//
// These tests pin the disclosure contract that replaced it, and the one case
// that must still fail.

// withReduceLLM points the summary deps at a stub LLM endpoint and restores
// whatever deps the package had before (they are process-global).
func withReduceLLM(t *testing.T, url string) {
	t.Helper()
	prev := func() (cfg config.Config) {
		defer func() { _ = recover() }() // deps may be unset in a fresh package run
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
	SetSummaryDeps(nil, nil, nil, cfg)
	t.Cleanup(func() { SetSummaryDeps(nil, nil, nil, prev) })
}

// truncatedReduceServer always answers with finish_reason=length and the given
// content, and counts how many Reduce round-trips the run actually spent.
func truncatedReduceServer(t *testing.T, content string) (*httptest.Server, *int64) {
	t.Helper()
	var hits int64
	body, err := json.Marshal(map[string]interface{}{
		"choices": []map[string]interface{}{{
			"message":       map[string]interface{}{"content": content},
			"finish_reason": "length",
		}},
		"usage": map[string]interface{}{"total_tokens": 4096, "completion_tokens": 4096},
	})
	if err != nil {
		t.Fatalf("marshal stub response: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// truncationPlannerClient is a minimal planner: Map, then Reduce over every
// returned handle, then answer with whatever the Reduce produced. If Reduce
// errors it keeps retrying, which is exactly the pre-fix behavior that burned
// MaxSteps.
type truncationPlannerClient struct {
	calls           int
	mergeAttempts   int
	sawNudge        bool
	lastMergeResult string
}

func (c *truncationPlannerClient) Chat(_ context.Context, msgs []Message, _ []Tool) (AssistantTurn, error) {
	c.calls++
	var handles []string
	mergeResult := ""
	for _, msg := range msgs {
		if strings.HasPrefix(msg.Content, "本次请求仍有未合并的 Map 结果") {
			c.sawNudge = true
		}
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
			}
			if json.Unmarshal([]byte(msg.Content), &result) == nil && result.MergedSummary != "" {
				mergeResult = msg.Content
			}
		}
	}

	if mergeResult != "" {
		c.lastMergeResult = mergeResult
		var result struct {
			MergedSummary string `json:"merged_summary"`
		}
		_ = json.Unmarshal([]byte(mergeResult), &result)
		// The planner's final answer is the Reduce deliverable verbatim, so the
		// disclosure it carries reaches the user.
		return AssistantTurn{Content: result.MergedSummary}, nil
	}

	if len(handles) > 0 {
		c.mergeAttempts++
		args, _ := json.Marshal(map[string]interface{}{"summary_handles": handles})
		return AssistantTurn{ToolCalls: []ToolCall{
			mkToolCall("reduce-call", "merge_summaries", string(args)),
		}}, nil
	}
	return AssistantTurn{ToolCalls: []ToolCall{
		mkToolCall("map-call", "summarize_chunk", `{}`),
	}}, nil
}

// A non-empty truncated Reduce must still produce output, disclose the
// truncation, clear the Reduce gate, and cost exactly ONE Reduce round-trip.
func TestRunnerDeliversTruncatedReduceWithDisclosure(t *testing.T) {
	srv, hits := truncatedReduceServer(t, "合并结果：项目进展、风险与待办（正文在此被截断")
	withReduceLLM(t, srv.URL)

	reg := NewRegistry()
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

	client := &truncationPlannerClient{}
	// MaxTokens is set high on purpose: a zero budget makes the runner inject its
	// budget instruction every step, which is unrelated noise for this test.
	runner := NewRunner(client, reg, NewPool(2), Policy{MaxSteps: 8, MaxTokens: 1 << 20, StepTimeout: 5 * time.Second})
	out, _, err := runner.RunWithHistory(context.Background(), "system", nil, "summarize")
	if err != nil {
		t.Fatalf("a non-empty truncated Reduce must not fail the request: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("request produced ZERO output for a usable truncated Reduce")
	}
	if !strings.Contains(out, "合并结果：项目进展") {
		t.Fatalf("final answer lost the partial merge: %q", out)
	}
	if !strings.Contains(out, "输出因长度限制被截断") {
		t.Fatalf("truncation was not disclosed to the user: %q", out)
	}
	if got := atomic.LoadInt64(hits); got != 1 {
		t.Fatalf("Reduce LLM round-trips = %d, want exactly 1 (identical retries are pure waste)", got)
	}
	if client.mergeAttempts != 1 {
		t.Fatalf("merge_summaries attempts = %d, want 1", client.mergeAttempts)
	}
	if client.sawNudge {
		t.Fatal("runner still nudged for a Reduce after a usable merge; NeedsReduce() was never cleared")
	}
	// The planner must be able to see the degradation structurally, not only as
	// prose inside merged_summary.
	var result struct {
		Truncated bool   `json:"truncated"`
		Notice    string `json:"truncation_notice"`
	}
	if err := json.Unmarshal([]byte(client.lastMergeResult), &result); err != nil {
		t.Fatalf("unmarshal merge result: %v", err)
	}
	if !result.Truncated || result.Notice == "" {
		t.Fatalf("merge result did not flag truncation: %s", client.lastMergeResult)
	}
}

// An EMPTY truncated Reduce has nothing to deliver and nothing to disclose
// against, so it must still be an error rather than a bare notice presented as
// a summary.
//
// The whitespace cases are PR #208 round-5 P1-1. " \n\t" is not "", so the
// service client's `content == ""` guard let it take the disclose branch; the
// client then appended TruncationNotice itself, which pre-satisfied this
// handler's own `strings.TrimSpace(merged) == ""` check. The result was a
// deliverable whose entire body was the truncation notice, with MarkReduced
// called and NeedsReduce() false — the completeness gate cleared by a Reduce
// that produced nothing.
func TestMergeSummariesRejectsEmptyTruncatedReduce(t *testing.T) {
	for _, body := range []string{"", " \n\t", "   ", "\n"} {
		t.Run(strconv.Quote(body), func(t *testing.T) {
			srv, _ := truncatedReduceServer(t, body)
			withReduceLLM(t, srv.URL)

			ctx := withSummaryHandleStore(context.Background())
			store, err := summaryHandleStoreFromContext(ctx)
			if err != nil {
				t.Fatalf("store: %v", err)
			}
			handle, err := store.Put("局部总结正文", 2)
			if err != nil {
				t.Fatalf("Put: %v", err)
			}

			_, handler := MergeSummariesTool()
			args, _ := json.Marshal(map[string]interface{}{"summary_handles": []string{handle}})
			out, err := handler(ctx, args)
			if err == nil || !strings.Contains(err.Error(), "truncated") {
				t.Fatalf("out=%q err=%v, want empty truncation to fail loudly", out, err)
			}
			if strings.Contains(out, "输出因长度限制被截断") {
				t.Fatalf("out=%q, want no bare disclosure returned as a deliverable", out)
			}
			if !store.NeedsReduce() {
				t.Fatal("a failed Reduce must leave the completeness gate armed")
			}
		})
	}
}

// The notice must appear exactly ONCE inside merged_summary. A planner that
// concatenates merged_summary and truncation_notice would otherwise repeat the
// same sentence, and if BOTH the client and the handler appended it the body
// itself would already carry it twice.
func TestMergeSummariesAppendsDisclosureExactlyOnce(t *testing.T) {
	srv, _ := truncatedReduceServer(t, "合并结果：项目进展（正文在此被截断")
	withReduceLLM(t, srv.URL)

	ctx := withSummaryHandleStore(context.Background())
	store, err := summaryHandleStoreFromContext(ctx)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	handle, err := store.Put("局部总结正文", 2)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	_, handler := MergeSummariesTool()
	args, _ := json.Marshal(map[string]interface{}{"summary_handles": []string{handle}})
	out, err := handler(ctx, args)
	if err != nil {
		t.Fatalf("a non-empty truncated Reduce must remain usable: %v", err)
	}
	var result struct {
		MergedSummary string `json:"merged_summary"`
		Truncated     bool   `json:"truncated"`
		Notice        string `json:"truncation_notice"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("unmarshal merge result: %v", err)
	}
	if !result.Truncated || result.Notice == "" {
		t.Fatalf("merge result did not flag truncation structurally: %s", out)
	}
	if n := strings.Count(result.MergedSummary, "输出因长度限制被截断"); n != 1 {
		t.Fatalf("disclosure appears %d times in merged_summary, want exactly 1: %q", n, result.MergedSummary)
	}
	if !strings.HasSuffix(result.MergedSummary, service.TruncationNotice) {
		t.Fatalf("merged_summary = %q, want the shared notice appended verbatim at the end", result.MergedSummary)
	}
}

// The Map phase must NOT inherit the disclosure: a truncated chunk summary
// silently drops messages that no later stage can recover (SS-01).
func TestSummarizeChunkStillRejectsTruncatedMap(t *testing.T) {
	srv, _ := truncatedReduceServer(t, "局部总结被截断")
	withReduceLLM(t, srv.URL)

	_, _, _, err := summarizeMessagesChunk(context.Background(), []map[string]interface{}{
		{"content": "hello", "sender_name": "a"},
	}, "")
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("error = %v, want a truncated Map chunk to keep failing loudly", err)
	}
}
