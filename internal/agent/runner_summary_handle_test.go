package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

type reduceAwareClient struct {
	calls           int
	largeBody       string
	bodyLeaked      bool
	sawNudge        bool
	sawBudgetReduce bool
}

type malformedMapRetryClient struct {
	calls    int
	sawNudge bool
}

func (c *malformedMapRetryClient) Chat(_ context.Context, msgs []Message, _ []Tool) (AssistantTurn, error) {
	c.calls++
	for _, msg := range msgs {
		if strings.Contains(msg.Content, "参数无效或缺少 messages_handle") {
			c.sawNudge = true
		}
	}
	switch c.calls {
	case 1:
		return AssistantTurn{ToolCalls: []ToolCall{
			mkToolCall("bad-map-call", "summarize_chunk", `{"chunk_size":10}`),
		}}, nil
	case 2:
		return AssistantTurn{Content: "premature final after malformed Map"}, nil
	case 3:
		return AssistantTurn{ToolCalls: []ToolCall{
			mkToolCall("good-map-call", "summarize_chunk", `{"messages_handle":"messages-1"}`),
		}}, nil
	case 4:
		var handle string
		for _, msg := range msgs {
			if msg.Role != "tool" || msg.Name != "summarize_chunk" {
				continue
			}
			var result struct {
				SummaryHandle string `json:"summary_handle"`
			}
			if json.Unmarshal([]byte(msg.Content), &result) == nil && result.SummaryHandle != "" {
				handle = result.SummaryHandle
			}
		}
		if handle == "" {
			return AssistantTurn{}, fmt.Errorf("corrected Map did not return a summary_handle")
		}
		args, _ := json.Marshal(map[string]interface{}{"summary_handles": []string{handle}})
		return AssistantTurn{ToolCalls: []ToolCall{
			mkToolCall("reduce-call", "merge_summaries", string(args)),
		}}, nil
	default:
		return AssistantTurn{Content: "final after corrected retry"}, nil
	}
}

func (c *reduceAwareClient) Chat(_ context.Context, msgs []Message, _ []Tool) (AssistantTurn, error) {
	c.calls++
	for _, msg := range msgs {
		if strings.Contains(msg.Content, c.largeBody) {
			c.bodyLeaked = true
		}
		if strings.Contains(msg.Content, "未合并的 Map 结果") {
			c.sawNudge = true
		}
		if strings.Contains(msg.Content, "已达token预算") && strings.Contains(msg.Content, "merge_summaries") {
			c.sawBudgetReduce = true
		}
	}

	switch c.calls {
	case 1:
		return AssistantTurn{ToolCalls: []ToolCall{mkToolCall("map-call", "summarize_chunk", `{}`)}, Tokens: 10}, nil
	case 2:
		return AssistantTurn{Content: "premature final"}, nil
	case 3:
		var handles []string
		for _, msg := range msgs {
			if msg.Role != "tool" || msg.Name != "summarize_chunk" {
				continue
			}
			var result struct {
				SummaryHandle string `json:"summary_handle"`
			}
			if json.Unmarshal([]byte(msg.Content), &result) == nil && result.SummaryHandle != "" {
				handles = append(handles, result.SummaryHandle)
			}
		}
		args, _ := json.Marshal(map[string]interface{}{"summary_handles": handles})
		return AssistantTurn{ToolCalls: []ToolCall{mkToolCall("reduce-call", "merge_summaries", string(args))}}, nil
	default:
		return AssistantTurn{Content: "final after reduce"}, nil
	}
}

func TestRunnerRequiresReduceAndKeepsMapBodyOutOfPlanner(t *testing.T) {
	largeBody := strings.Repeat("large-map-body-", 10000)
	client := &reduceAwareClient{largeBody: largeBody}
	reg := NewRegistry()
	reg.Register(Tool{Type: "function", Function: ToolFunction{Name: "summarize_chunk"}},
		func(ctx context.Context, _ json.RawMessage) (string, error) {
			store, _ := summaryHandleStoreFromContext(ctx)
			handle, err := store.PutAtStep(largeBody, 4, summaryToolStepFromContext(ctx))
			if err != nil {
				return "", err
			}
			data, _ := json.Marshal(map[string]interface{}{"summary_handle": handle, "chunk_count": 4})
			return string(data), nil
		})
	reg.Register(Tool{Type: "function", Function: ToolFunction{Name: "merge_summaries"}},
		func(ctx context.Context, args json.RawMessage) (string, error) {
			var req struct {
				Handles []string `json:"summary_handles"`
			}
			if err := json.Unmarshal(args, &req); err != nil {
				return "", err
			}
			store, _ := summaryHandleStoreFromContext(ctx)
			resolved, err := store.ResolveAllBefore(req.Handles, summaryToolStepFromContext(ctx))
			if err != nil {
				return "", err
			}
			store.MarkReduced(resolved.Generation)
			return `{"merged_summary":"merged","chunk_count":4}`, nil
		})

	runner := NewRunner(client, reg, NewPool(2), Policy{MaxSteps: 6, MaxTokens: 1, StepTimeout: time.Second})
	out, newMsgs, err := runner.RunWithHistory(context.Background(), "system", nil, "summarize")
	if err != nil {
		t.Fatalf("RunWithHistory: %v", err)
	}
	if out != "final after reduce" || client.calls != 4 {
		t.Fatalf("out=%q calls=%d, want reduced final after 4 planner turns", out, client.calls)
	}
	if !client.sawNudge {
		t.Fatal("planner did not receive the Reduce-required nudge")
	}
	if !client.sawBudgetReduce {
		t.Fatal("token-budget instruction contradicted the pending Reduce requirement")
	}
	if client.bodyLeaked {
		t.Fatal("full Map body leaked back into planner messages")
	}
	for _, msg := range newMsgs {
		if msg.Content == "premature final" || strings.Contains(msg.Content, "未合并的 Map 结果") {
			t.Fatalf("rejected final/nudge must not be persisted: %+v", msg)
		}
	}
}

func TestRunnerFailsClosedWhenReduceStillPendingAtLastStep(t *testing.T) {
	reg := NewRegistry()
	reg.Register(Tool{Type: "function", Function: ToolFunction{Name: "summarize_chunk"}},
		func(ctx context.Context, _ json.RawMessage) (string, error) {
			store, _ := summaryHandleStoreFromContext(ctx)
			handle, err := store.PutAtStep("map body", 1, summaryToolStepFromContext(ctx))
			if err != nil {
				return "", err
			}
			return `{"summary_handle":"` + handle + `"}`, nil
		})
	client := &fakeClient{turns: []AssistantTurn{
		{ToolCalls: []ToolCall{mkToolCall("map-call", "summarize_chunk", `{}`)}},
		{Content: "final without reduce"},
	}}
	runner := NewRunner(client, reg, NewPool(1), Policy{MaxSteps: 2, MaxTokens: 100000, StepTimeout: time.Second})
	if _, _, err := runner.RunWithHistory(context.Background(), "system", nil, "summarize"); err == nil || !strings.Contains(err.Error(), "Reduce required") {
		t.Fatalf("error = %v, want Reduce-required failure", err)
	}
}

func TestRunnerShadowsSummaryStoreFromParentContext(t *testing.T) {
	parent := withSummaryHandleStore(context.Background())
	parentStore, err := summaryHandleStoreFromContext(parent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parentStore.Put("stale Map result from an earlier run", 1); err != nil {
		t.Fatal(err)
	}

	client := &fakeClient{turns: []AssistantTurn{{Content: "fresh final"}}}
	runner := NewRunner(client, NewRegistry(), NewPool(1), Policy{
		MaxSteps:    1,
		MaxTokens:   100000,
		StepTimeout: time.Second,
	})
	out, _, err := runner.RunWithHistory(parent, "system", nil, "new request")
	if err != nil {
		t.Fatalf("RunWithHistory reused stale parent store: %v", err)
	}
	if out != "fresh final" {
		t.Fatalf("out = %q, want fresh final", out)
	}
	if !parentStore.NeedsReduce() {
		t.Fatal("runner mutated the parent store instead of shadowing it")
	}
}

func TestRunnerMalformedMapArgsCanRecoverWithCorrectedRetry(t *testing.T) {
	client := &malformedMapRetryClient{}
	reg := NewRegistry()
	reg.Register(Tool{Type: "function", Function: ToolFunction{Name: "summarize_chunk"}},
		func(ctx context.Context, args json.RawMessage) (string, error) {
			var req struct {
				MessagesHandle string `json:"messages_handle"`
			}
			if err := json.Unmarshal(args, &req); err != nil {
				return "", fmt.Errorf("parse args: %w", err)
			}
			if req.MessagesHandle == "" {
				return "", fmt.Errorf("messages_handle is required")
			}
			store, _ := summaryHandleStoreFromContext(ctx)
			handle, err := store.PutAtStep("corrected Map body", 1, summaryToolStepFromContext(ctx))
			if err != nil {
				return "", err
			}
			return `{"summary_handle":"` + handle + `","chunk_count":1}`, nil
		})
	reg.Register(Tool{Type: "function", Function: ToolFunction{Name: "merge_summaries"}},
		func(ctx context.Context, args json.RawMessage) (string, error) {
			var req struct {
				Handles []string `json:"summary_handles"`
			}
			if err := json.Unmarshal(args, &req); err != nil {
				return "", err
			}
			store, _ := summaryHandleStoreFromContext(ctx)
			resolved, err := store.ResolveAllBefore(req.Handles, summaryToolStepFromContext(ctx))
			if err != nil {
				return "", err
			}
			store.MarkReduced(resolved.Generation)
			return `{"merged_summary":"merged","chunk_count":1}`, nil
		})

	runner := NewRunner(client, reg, NewPool(2), Policy{MaxSteps: 5, MaxTokens: 100000, StepTimeout: time.Second})
	out, _, err := runner.RunWithHistory(context.Background(), "system", nil, "summarize")
	if err != nil {
		t.Fatalf("RunWithHistory: %v", err)
	}
	if out != "final after corrected retry" || client.calls != 5 {
		t.Fatalf("out=%q calls=%d, want successful recovery in 5 turns", out, client.calls)
	}
	if !client.sawNudge {
		t.Fatal("runner did not explain how to recover a handle-less Map failure")
	}
}
