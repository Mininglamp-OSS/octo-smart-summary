package agent

import (
	"context"
	"encoding/json"
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

// PR #208 round-5 P2-3. Compaction rewrites past merge_summaries arguments, and
// the model is few-shot conditioned by its own transcript: a placeholder that
// violates the LIVE tool schema is a worked example of a malformed call. The
// schema is additionalProperties:false with required:["summary_handles"], so
// `{"history_compacted":true}` was exactly that — and every merge_summaries
// failure latches the run fatal until a later merge succeeds.
func TestCompactionPlaceholdersMatchTheLiveToolSchemas(t *testing.T) {
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(compactedReduceHistoryArgs), &args); err != nil {
		t.Fatalf("compacted Reduce arguments are not valid JSON: %v", err)
	}
	schema, _ := MergeSummariesTool()
	params, _ := schema.Function.Parameters.(map[string]interface{})
	props, _ := params["properties"].(map[string]interface{})
	if additional, ok := params["additionalProperties"].(bool); !ok || additional {
		t.Fatal("test assumes merge_summaries forbids additional properties")
	}
	for key := range args {
		if _, declared := props[key]; !declared {
			t.Fatalf("compacted Reduce arguments carry %q, which additionalProperties:false forbids: %s",
				key, compactedReduceHistoryArgs)
		}
	}
	for _, required := range params["required"].([]string) {
		if _, present := args[required]; !present {
			t.Fatalf("compacted Reduce arguments omit required field %q: %s", required, compactedReduceHistoryArgs)
		}
	}
	handles, ok := args["summary_handles"].([]interface{})
	if !ok || len(handles) < 1 {
		t.Fatalf("summary_handles must satisfy minItems:1: %s", compactedReduceHistoryArgs)
	}
	// The placeholder must not be mistaken for a live handle. Real handles are
	// minted as map_<uuid>_<n>; this one resolves to nothing.
	store := newSummaryHandleStore()
	if _, err := store.ResolveAll([]string{handles[0].(string)}); err == nil {
		t.Fatal("the placeholder handle must never resolve against a real store")
	}

	// The Map placeholder is a summarize_chunk RESULT, so it must parse as one:
	// the planner reads summary_handle out of it to build the next Reduce call.
	var mapResult summarizeChunkToolResult
	if err := json.Unmarshal([]byte(compactedMapHistoryResult), &mapResult); err != nil {
		t.Fatalf("compacted Map result does not parse as a summarize_chunk result: %v", err)
	}
	if mapResult.SummaryHandle == "" {
		t.Fatalf("compacted Map result has no summary_handle field: %s", compactedMapHistoryResult)
	}
	if _, err := store.ResolveAll([]string{mapResult.SummaryHandle}); err == nil {
		t.Fatal("the placeholder Map handle must never resolve against a real store")
	}
}

// A RETAINED diagnostic error result must keep the arguments that caused it.
// The rewrite used to run BEFORE the error check, so the model was shown a
// complaint about a call it was simultaneously shown never making.
func TestCompactionKeepsArgumentsThatProducedARetainedError(t *testing.T) {
	badArgs := `{"summary_handles":["map_stale_7"]}`
	history := []Message{
		{Role: "user", Content: "old request"},
		{Role: "assistant", ToolCalls: []ToolCall{mkToolCall("reduce-bad", "merge_summaries", badArgs)}},
		{Role: "tool", ToolCallID: "reduce-bad", Name: "merge_summaries",
			Content: `{"ok":false,"error_code":"INVALID_ARGUMENT","message":"invalid or expired summary_handle: map_stale_7"}`},
		{Role: "assistant", ToolCalls: []ToolCall{mkToolCall("reduce-ok", "merge_summaries", `{"summary_handles":["map_live_1"]}`)}},
		{Role: "tool", ToolCallID: "reduce-ok", Name: "merge_summaries", Content: `{"merged_summary":"body","chunk_count":1}`},
	}

	got := compactSummaryToolHistory(history)
	if got[1].ToolCalls[0].Function.Arguments != badArgs {
		t.Fatalf("arguments of a retained error were rewritten to %q; the preserved exchange no longer shows what failed",
			got[1].ToolCalls[0].Function.Arguments)
	}
	if got[2].Content != history[2].Content {
		t.Fatalf("diagnostic error result was compacted away: %q", got[2].Content)
	}
	// The SUCCESSFUL call still gets compacted — its arguments carry no
	// diagnostic value and its result is reproduced by the final answer.
	if got[3].ToolCalls[0].Function.Arguments != compactedReduceHistoryArgs {
		t.Fatalf("successful Reduce arguments were not compacted: %q", got[3].ToolCalls[0].Function.Arguments)
	}
	if got[4].Content != compactedReduceHistoryResult {
		t.Fatalf("successful Reduce result was not compacted: %q", got[4].Content)
	}
}

// Retaining a failed call's arguments must not reintroduce transcript bloat: a
// LEGACY failed merge carried the full summary bodies inline, which is exactly
// what this function exists to strip.
func TestCompactionDropsOversizedRetainedFailureArguments(t *testing.T) {
	legacyBody := strings.Repeat("legacy-inline-body-", 5000)
	history := []Message{
		{Role: "user", Content: "old request"},
		{Role: "assistant", ToolCalls: []ToolCall{mkToolCall("reduce-huge", "merge_summaries", `{"summaries":["`+legacyBody+`"]}`)}},
		{Role: "tool", ToolCallID: "reduce-huge", Name: "merge_summaries", Content: `{"ok":false,"message":"boom"}`},
	}
	got := compactSummaryToolHistory(history)
	if strings.Contains(got[1].ToolCalls[0].Function.Arguments, legacyBody[:100]) {
		t.Fatal("an oversized failed-call argument blob was retained into the planner prompt")
	}
	if got[1].ToolCalls[0].Function.Arguments != compactedReduceHistoryArgs {
		t.Fatalf("oversized arguments = %q, want the placeholder", got[1].ToolCalls[0].Function.Arguments)
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

func TestStripTerminalToolHistoryRemovesDraftArgumentsAndKeepsOrdinaryPairs(t *testing.T) {
	history := []Message{
		{Role: "user", Content: "old request"},
		{Role: "assistant", ToolCalls: []ToolCall{mkToolCall("emit-only", "emit_summary_response", validPreviewTerminalArgs)}},
		{Role: "tool", ToolCallID: "emit-only", Name: "emit_summary_response", Content: `{"accepted":true}`},
		{Role: "assistant", Content: "synthetic final"},
		{Role: "assistant", Content: "# draft that must not persist", ToolCalls: []ToolCall{
			mkToolCall("emit-mixed", "emit_summary_response", validPreviewTerminalArgs),
			mkToolCall("ordinary", "echo", `{"value":"ok"}`),
		}},
		{Role: "tool", ToolCallID: "emit-mixed", Name: "emit_summary_response", Content: "error"},
		{Role: "tool", ToolCallID: "ordinary", Name: "echo", Content: "ok"},
	}

	got := stripTerminalToolHistory(history, "emit_summary_response")
	for _, msg := range got {
		if msg.Name == "emit_summary_response" || msg.ToolCallID == "emit-only" || msg.ToolCallID == "emit-mixed" {
			t.Fatalf("terminal tool result remained: %+v", got)
		}
		for _, call := range msg.ToolCalls {
			if call.Function.Name == "emit_summary_response" || strings.Contains(call.Function.Arguments, "# 正文") {
				t.Fatalf("terminal call arguments remained: %+v", got)
			}
		}
	}
	if len(got) != 4 || len(got[2].ToolCalls) != 1 || got[2].ToolCalls[0].Function.Name != "echo" || got[3].ToolCallID != "ordinary" {
		t.Fatalf("ordinary protocol pair was not preserved: %+v", got)
	}
	if got[2].Content != "" {
		t.Fatalf("mixed terminal turn retained draft-like assistant content: %q", got[2].Content)
	}
}

func TestStripTerminalToolHistoryScopesToolCallIDsToOneAssistantTurn(t *testing.T) {
	history := []Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", ToolCalls: []ToolCall{mkToolCall("reused", "emit_summary_response", validPreviewTerminalArgs)}},
		{Role: "tool", ToolCallID: "reused", Name: "emit_summary_response", Content: "rejected"},
		{Role: "user", Content: "second"},
		{Role: "assistant", ToolCalls: []ToolCall{mkToolCall("reused", "echo", `{}`)}},
		{Role: "tool", ToolCallID: "reused", Name: "echo", Content: "ok"},
	}
	got := stripTerminalToolHistory(history, "emit_summary_response")
	if len(got) != 4 || got[2].ToolCalls[0].Function.Name != "echo" || got[3].Name != "echo" {
		t.Fatalf("a reused id in a later ordinary turn was removed: %+v", got)
	}
}

func TestSanitizeToolProtocolHistoryDropsTruncatedPrefixAndUnpairedGroups(t *testing.T) {
	history := []Message{
		{Role: "tool", ToolCallID: "orphan", Name: "echo", Content: "orphan"},
		{Role: "assistant", Content: "prefix"},
		{Role: "user", Content: "kept turn"},
		{Role: "assistant", ToolCalls: []ToolCall{mkToolCall("missing", "echo", `{}`)}},
		{Role: "assistant", ToolCalls: []ToolCall{mkToolCall("paired", "echo", `{}`)}},
		{Role: "tool", ToolCallID: "paired", Name: "echo", Content: "ok"},
		{Role: "assistant", Content: "final"},
	}
	got := sanitizeToolProtocolHistory(history)
	if len(got) != 4 || got[0].Role != "user" || got[1].ToolCalls[0].ID != "paired" || got[2].ToolCallID != "paired" || got[3].Content != "final" {
		t.Fatalf("sanitized history = %+v", got)
	}
}
