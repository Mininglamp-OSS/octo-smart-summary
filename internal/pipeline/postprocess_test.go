package pipeline

import (
	"testing"
)

func TestDecideMessages_NoTarget(t *testing.T) {
	all := []Message{{SenderUID: "a"}, {SenderUID: "b"}}
	msgs, early := DecideMessages(nil, "creator", all, nil)
	if len(msgs) != 2 || early != "" {
		t.Fatalf("expected all messages, got %d msgs and early=%q", len(msgs), early)
	}
}

func TestDecideMessages_FilteredNonEmpty(t *testing.T) {
	all := []Message{{SenderUID: "a"}, {SenderUID: "b"}}
	filtered := []Message{{SenderUID: "a"}}
	msgs, early := DecideMessages([]string{"a"}, "creator", all, filtered)
	if len(msgs) != 1 || early != "" {
		t.Fatalf("expected filtered messages, got %d msgs and early=%q", len(msgs), early)
	}
}

func TestDecideMessages_FilteredEmpty_SelfOnly_EarlyReturn(t *testing.T) {
	all := []Message{{SenderUID: "other"}}
	msgs, early := DecideMessages([]string{"creator"}, "creator", all, nil)
	if msgs != nil || early != NoSelfMessagesMessage {
		t.Fatalf("expected early return with NoSelfMessagesMessage, got msgs=%v early=%q", msgs, early)
	}
}

func TestDecideMessages_FilteredEmpty_NamedOther_FallsBackToAll(t *testing.T) {
	all := []Message{{SenderUID: "a"}, {SenderUID: "b"}}
	msgs, early := DecideMessages([]string{"other"}, "creator", all, nil)
	if len(msgs) != 2 || early != "" {
		t.Fatalf("expected fallback to all, got %d msgs and early=%q", len(msgs), early)
	}
}

func TestDecideMessages_FilteredEmpty_SelfPlusOther_FallsBackToAll(t *testing.T) {
	all := []Message{{SenderUID: "a"}}
	msgs, early := DecideMessages([]string{"creator", "other"}, "creator", all, nil)
	if len(msgs) != 1 || early != "" {
		t.Fatalf("expected fallback to all, got %d msgs and early=%q", len(msgs), early)
	}
}

func TestPostProcess_NoTargets_AllMessages(t *testing.T) {
	messages := []Message{
		{SenderUID: "user1", Content: "hello"},
		{SenderUID: "user2", Content: "world"},
	}
	resolver := func(msgs []Message) map[string]string {
		return map[string]string{"user1": "Alice", "user2": "Bob"}
	}
	opts := PostProcessOptions{ContextWindow: 5, CreatorUID: "creator"}

	result := PostProcess(messages, nil, opts, resolver)

	if len(result.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result.Messages))
	}
	if result.EarlyReturn != "" {
		t.Fatalf("expected no early return, got %q", result.EarlyReturn)
	}
	if result.Messages[0].SenderName != "Alice" {
		t.Errorf("expected SenderName=Alice, got %q", result.Messages[0].SenderName)
	}
	if result.Messages[1].SenderName != "Bob" {
		t.Errorf("expected SenderName=Bob, got %q", result.Messages[1].SenderName)
	}
}

func TestPostProcess_WithTargets_Filtered(t *testing.T) {
	messages := []Message{
		{SenderUID: "user1", Content: "hello"},
		{SenderUID: "user2", Content: "world"},
	}
	resolver := func(msgs []Message) map[string]string {
		return map[string]string{"user1": "Alice", "user2": "Bob"}
	}
	opts := PostProcessOptions{ContextWindow: 5, CreatorUID: "creator"}

	result := PostProcess(messages, []string{"user1"}, opts, resolver)

	// FilterWithContext should mark user1's message with IsTargetUser
	// DecideMessages should return filtered (non-empty) or fallback
	if result.EarlyReturn != "" {
		t.Fatalf("expected no early return, got %q", result.EarlyReturn)
	}
	// Since FilterWithContext is already tested elsewhere, we just verify it runs
}

func TestPostProcess_NilResolver(t *testing.T) {
	messages := []Message{
		{SenderUID: "user1", Content: "hello"},
	}
	opts := PostProcessOptions{ContextWindow: 5, CreatorUID: "creator"}

	result := PostProcess(messages, nil, opts, nil)

	if len(result.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result.Messages))
	}
	// Without resolver, SenderName should fall back to SenderUID
	if result.Messages[0].SenderName != "user1" {
		t.Errorf("expected SenderName=user1 (fallback), got %q", result.Messages[0].SenderName)
	}
}

func TestPostProcess_EarlyReturn_SelfNoMessages(t *testing.T) {
	// Simulate: creator asks about their own messages but has none
	messages := []Message{
		{SenderUID: "other", Content: "hello"},
	}
	resolver := func(msgs []Message) map[string]string {
		return map[string]string{"other": "Bob"}
	}
	opts := PostProcessOptions{ContextWindow: 5, CreatorUID: "creator"}

	result := PostProcess(messages, []string{"creator"}, opts, resolver)

	if result.EarlyReturn != NoSelfMessagesMessage {
		t.Fatalf("expected early return %q, got %q", NoSelfMessagesMessage, result.EarlyReturn)
	}
	if result.Messages != nil {
		t.Errorf("expected nil messages on early return, got %d", len(result.Messages))
	}
}

func TestConstants(t *testing.T) {
	// Ensure constants match expected values (regression test for i18n)
	if NoSelfMessagesMessage != "你在所选范围内没有发言记录。" {
		t.Errorf("NoSelfMessagesMessage changed: %q", NoSelfMessagesMessage)
	}
	if NoRelevantContentMessage != "在当前范围内未找到与主题相关的聊天记录。" {
		t.Errorf("NoRelevantContentMessage changed: %q", NoRelevantContentMessage)
	}
}
