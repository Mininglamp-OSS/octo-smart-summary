package agent

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestCompactSummaryToolHistoryRemovesLegacyBodiesAndKeepsPairs(t *testing.T) {
	legacyBody := strings.Repeat("legacy-map-body-", 5000)
	summarizeCall := mkToolCall("map-1", "summarize_chunk", `{"messages_handle":"old-messages"}`)
	mergeCall := mkToolCall("reduce-1", "merge_summaries", `{"summaries":["`+legacyBody+`"]}`)
	history := []Message{
		{Role: "user", Content: "old request"},
		{Role: "assistant", ToolCalls: []ToolCall{summarizeCall}},
		{Role: "tool", ToolCallID: "map-1", Name: "summarize_chunk", Content: `{"summary":"` + legacyBody + `"}`},
		{Role: "assistant", ToolCalls: []ToolCall{mergeCall}},
		{Role: "tool", ToolCallID: "reduce-1", Name: "merge_summaries", Content: `{"merged_summary":"` + legacyBody + `"}`},
		{Role: "assistant", Content: "durable final summary"},
		{Role: "tool", ToolCallID: "error-1", Name: "merge_summaries", Content: `{"ok": false, "message":"keep this error"}`},
		{Role: "tool", ToolCallID: "quoted-1", Name: "summarize_chunk", Content: `{"summary":"quoted marker: \"ok\":false should not look like an envelope"}`},
	}

	got := compactSummaryToolHistory(history)
	if !strings.Contains(history[2].Content, legacyBody[:100]) || !strings.Contains(history[3].ToolCalls[0].Function.Arguments, legacyBody[:100]) {
		t.Fatal("compaction mutated caller-owned history")
	}
	for _, msg := range got {
		if strings.Contains(msg.Content, legacyBody[:100]) {
			t.Fatalf("legacy body remained in message content: %+v", msg)
		}
		for _, call := range msg.ToolCalls {
			if strings.Contains(call.Function.Arguments, legacyBody[:100]) {
				t.Fatalf("legacy body remained in tool arguments: %+v", call)
			}
		}
	}
	if got[1].ToolCalls[0].ID != "map-1" || got[2].ToolCallID != "map-1" || got[3].ToolCalls[0].ID != "reduce-1" || got[4].ToolCallID != "reduce-1" {
		t.Fatalf("tool call/result pairing changed: %+v", got)
	}
	if got[5].Content != "durable final summary" {
		t.Fatalf("final assistant summary was changed: %+v", got[5])
	}
	if got[6].Content != history[6].Content {
		t.Fatalf("historical tool error should be preserved: %q", got[6].Content)
	}
	if got[7].Content != compactedMapHistoryResult {
		t.Fatalf("summary body quoting an ok:false marker should still be compacted: %q", got[7].Content)
	}
}

func TestRunnerCompactsLegacySummaryHistoryBeforePlanner(t *testing.T) {
	legacyBody := strings.Repeat("legacy-large-body-", 5000)
	history := []Message{
		{Role: "user", Content: "old request"},
		{Role: "assistant", ToolCalls: []ToolCall{mkToolCall("map-1", "summarize_chunk", `{"messages_handle":"old"}`)}},
		{Role: "tool", ToolCallID: "map-1", Name: "summarize_chunk", Content: `{"summary":"` + legacyBody + `"}`},
		{Role: "assistant", ToolCalls: []ToolCall{mkToolCall("reduce-1", "merge_summaries", `{"summaries":["`+legacyBody+`"]}`)}},
		{Role: "tool", ToolCallID: "reduce-1", Name: "merge_summaries", Content: `{"merged_summary":"` + legacyBody + `"}`},
		{Role: "assistant", Content: "old final"},
	}
	client := &fakeClient{turns: []AssistantTurn{{Content: "new final"}}}
	runner := NewRunner(client, NewRegistry(), NewPool(1), Policy{MaxSteps: 2, MaxTokens: 1000, StepTimeout: time.Second})
	if _, _, err := runner.RunWithHistory(context.Background(), "system", history, "new request"); err != nil {
		t.Fatalf("RunWithHistory: %v", err)
	}
	for _, msg := range client.lastMsgs {
		if strings.Contains(msg.Content, legacyBody[:100]) {
			t.Fatalf("legacy body reached planner content: %+v", msg)
		}
		for _, call := range msg.ToolCalls {
			if strings.Contains(call.Function.Arguments, legacyBody[:100]) {
				t.Fatalf("legacy body reached planner tool arguments: %+v", call)
			}
		}
	}
}
