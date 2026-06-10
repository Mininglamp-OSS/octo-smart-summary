package worker

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

func TestDecidePersonalMessages_NoTarget_AllMessages(t *testing.T) {
	all := []pipeline.Message{
		{SenderUID: "alice", Content: "hello"},
		{SenderUID: "bob", Content: "hi"},
	}
	msgs, early := decidePersonalMessages(nil, "creator", all, nil)
	if early != "" {
		t.Fatalf("expected no early message, got %q", early)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected all 2 messages, got %d", len(msgs))
	}
}

func TestDecidePersonalMessages_FilteredNonEmpty_UsesFiltered(t *testing.T) {
	all := []pipeline.Message{
		{SenderUID: "alice", Content: "hello"},
		{SenderUID: "bob", Content: "hi"},
	}
	filtered := []pipeline.Message{
		{SenderUID: "bob", Content: "hi", IsTargetUser: true},
	}
	msgs, early := decidePersonalMessages([]string{"bob"}, "creator", all, filtered)
	if early != "" {
		t.Fatalf("expected no early message, got %q", early)
	}
	if len(msgs) != 1 || msgs[0].SenderUID != "bob" {
		t.Fatalf("expected filtered [bob], got %v", msgs)
	}
}

func TestDecidePersonalMessages_FilteredEmpty_SelfOnly_EarlyReturn(t *testing.T) {
	all := []pipeline.Message{
		{SenderUID: "alice", Content: "hello"},
		{SenderUID: "bob", Content: "hi"},
	}
	// True first-person query: target is exactly the creator, who never spoke.
	msgs, early := decidePersonalMessages([]string{"creator"}, "creator", all, nil)
	if early != noSelfMessagesMessage {
		t.Fatalf("expected noSelfMessagesMessage, got %q", early)
	}
	if msgs != nil {
		t.Fatalf("expected nil messages on self-empty early return, got %v", msgs)
	}
}

func TestDecidePersonalMessages_FilteredEmpty_NamedOther_FallsBackToAll(t *testing.T) {
	all := []pipeline.Message{
		{SenderUID: "alice", Content: "hello"},
		{SenderUID: "bob", Content: "hi"},
	}
	// Named someone (wang) who didn't speak in this source → fall back to all.
	msgs, early := decidePersonalMessages([]string{"wang"}, "creator", all, nil)
	if early != "" {
		t.Fatalf("expected no early message, got %q", early)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected fallback to all 2 messages, got %d", len(msgs))
	}
}

func TestDecidePersonalMessages_FilteredEmpty_SelfPlusOther_FallsBackToAll(t *testing.T) {
	all := []pipeline.Message{
		{SenderUID: "alice", Content: "hello"},
	}
	// "我和老王" → targets [wang, creator]; not a pure self-reference, so fall back.
	msgs, early := decidePersonalMessages([]string{"wang", "creator"}, "creator", all, nil)
	if early != "" {
		t.Fatalf("expected no early message, got %q", early)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected fallback to all 1 message, got %d", len(msgs))
	}
}

// normalizeTargetMsgCount mirrors the F1 fix in executePersonalPipeline: on
// untrimmed / fallback paths no message has IsTargetUser set, so the accumulated
// count is 0 and must be normalized to the full message count.
func normalizeTargetMsgCount(msgs []pipeline.Message) int {
	count := 0
	for _, m := range msgs {
		if m.IsTargetUser {
			count++
		}
	}
	if count == 0 {
		count = len(msgs)
	}
	return count
}

func TestTargetMsgCount_Normalization_Untrimmed(t *testing.T) {
	// No IsTargetUser anywhere (fallback / no-target path) → count = len.
	msgs := []pipeline.Message{
		{SenderUID: "alice"},
		{SenderUID: "bob"},
		{SenderUID: "carol"},
	}
	if got := normalizeTargetMsgCount(msgs); got != 3 {
		t.Fatalf("expected normalized count 3, got %d", got)
	}
}

func TestTargetMsgCount_Normalization_TrueNarrowUnaffected(t *testing.T) {
	// True narrow path has ≥1 IsTargetUser → count reflects targets only.
	msgs := []pipeline.Message{
		{SenderUID: "alice"},
		{SenderUID: "bob", IsTargetUser: true},
		{SenderUID: "carol"},
	}
	if got := normalizeTargetMsgCount(msgs); got != 1 {
		t.Fatalf("expected count 1 (true narrow unaffected), got %d", got)
	}
}

func TestShouldSkipScheduledPlaceholder_BothPlaceholdersSkipped(t *testing.T) {
	// Both empty-placeholder messages must be skipped for scheduled tasks so a
	// transient empty window never overwrites a previous valid result.
	cases := []struct {
		name        string
		triggerType int
		content     string
		want        bool
	}{
		{"scheduled + no-relevant", model.TriggerScheduled, noRelevantContentMessage, true},
		{"scheduled + no-self-messages", model.TriggerScheduled, noSelfMessagesMessage, true},
		{"scheduled + real content", model.TriggerScheduled, "正常的总结内容", false},
		{"manual + no-self-messages", model.TriggerManual, noSelfMessagesMessage, false},
	}
	for _, c := range cases {
		if got := shouldSkipScheduledPlaceholderResult(c.triggerType, c.content); got != c.want {
			t.Fatalf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
