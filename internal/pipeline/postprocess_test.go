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

func TestConstants(t *testing.T) {
	// Ensure constants match expected values (regression test for i18n)
	if NoSelfMessagesMessage != "你在所选范围内没有发言记录。" {
		t.Errorf("NoSelfMessagesMessage changed: %q", NoSelfMessagesMessage)
	}
	if NoRelevantContentMessage != "在当前范围内未找到与主题相关的聊天记录。" {
		t.Errorf("NoRelevantContentMessage changed: %q", NoRelevantContentMessage)
	}
}
