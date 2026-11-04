package handler

import (
	"reflect"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
)

func TestNormalizeLegacySummaryContent(t *testing.T) {
	const envelope = `{"content":"# Progress\nReady [1].","citations":[]}`
	const body = "# Progress\nReady [1]."
	for _, input := range []string{envelope, " \n" + envelope + "\n", "```json\n" + envelope + "\n```", "```\n" + envelope + "\n```"} {
		if got := normalizeLegacySummaryContent(input); got != body {
			t.Errorf("input=%q: got %q, want %q", input, got, body)
		}
	}
	for _, input := range []string{
		"", "  Plain summary.\n", "# Progress\nReady [1].", "```json\n{\"example\":true}\n```",
		`{"content":"An example"}`, `{"content":"An example","citations":[],"extra":true}`,
		`{"content":null,"citations":[]}`, `{"content":1,"citations":[]}`, `{"content":" ","citations":[]}`,
		`{"content":"Ready","citations":null}`, `{"content":"Ready","citations":{}}`,
		`{"content":"unfinished`, envelope + "\nExplanation", envelope + envelope,
		"Example:\n" + envelope, "```json\n" + envelope, "```js\n" + envelope + "\n```",
	} {
		if got := normalizeLegacySummaryContent(input); got != input {
			t.Errorf("non-envelope changed: input=%q, got %q", input, got)
		}
	}
}

func TestNormalizeLegacySummaryReplyPreservesTrace(t *testing.T) {
	const envelope = `{"content":"Ready [1].","citations":[]}`
	messages := []agent.Message{
		{Role: "user", Content: envelope},
		{Role: "assistant", Content: envelope, ToolCalls: []agent.ToolCall{{ID: "tool-1"}}},
		{Role: "tool", Content: envelope, ToolCallID: "tool-1"},
		{Role: "assistant", Content: envelope, RunID: "run-1", OutputTruncated: true},
	}
	want := append([]agent.Message(nil), messages...)
	want[3].Content = "Ready [1]."
	if reply := normalizeLegacySummaryReply("summary_refine", envelope, messages); reply != want[3].Content {
		t.Fatalf("reply=%q", reply)
	}
	if !reflect.DeepEqual(messages, want) {
		t.Fatalf("tool trace or final-message metadata changed: %+v", messages)
	}
	if reply := normalizeLegacySummaryReply(summaryWorkspaceProfile, envelope, nil); reply != envelope {
		t.Fatalf("workspace reply changed: %q", reply)
	}
}
