package pipeline

import (
	"testing"
)

func TestFilterWithContext_SingleTarget(t *testing.T) {
	msgs := []Message{
		{SenderUID: "alice", Content: "hello"},
		{SenderUID: "bob", Content: "hi"},
		{SenderUID: "alice", Content: "update"},
		{SenderUID: "carol", Content: "ok"},
	}

	result := FilterWithContext(msgs, []string{"bob"}, 0)
	if len(result) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result))
	}
	if result[0].SenderUID != "bob" {
		t.Errorf("expected bob, got %s", result[0].SenderUID)
	}
	if !result[0].IsTargetUser {
		t.Error("expected IsTargetUser=true for bob")
	}
}

func TestFilterWithContext_MultipleTargets(t *testing.T) {
	msgs := []Message{
		{SenderUID: "alice", Content: "hello"},
		{SenderUID: "bob", Content: "hi"},
		{SenderUID: "carol", Content: "update"},
		{SenderUID: "dave", Content: "ok"},
	}

	result := FilterWithContext(msgs, []string{"bob", "carol"}, 0)
	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}
	for _, m := range result {
		if !m.IsTargetUser {
			t.Errorf("expected IsTargetUser=true for %s", m.SenderUID)
		}
	}
}

func TestFilterWithContext_ContextWindow(t *testing.T) {
	msgs := []Message{
		{SenderUID: "alice", Content: "before-2"},
		{SenderUID: "alice", Content: "before-1"},
		{SenderUID: "bob", Content: "target"},
		{SenderUID: "carol", Content: "after-1"},
		{SenderUID: "dave", Content: "after-2"},
	}

	result := FilterWithContext(msgs, []string{"bob"}, 1)
	if len(result) != 3 {
		t.Fatalf("expected 3 messages (1 before + target + 1 after), got %d", len(result))
	}
	if result[0].SenderUID != "alice" || result[0].Content != "before-1" {
		t.Errorf("expected alice/before-1 at pos 0, got %s/%s", result[0].SenderUID, result[0].Content)
	}
	if result[1].SenderUID != "bob" || !result[1].IsTargetUser {
		t.Errorf("expected bob/IsTargetUser at pos 1")
	}
	if result[2].SenderUID != "carol" || result[2].IsTargetUser {
		t.Errorf("expected carol/not-target at pos 2, got target=%v", result[2].IsTargetUser)
	}
}

func TestFilterWithContext_EmptyMessages(t *testing.T) {
	result := FilterWithContext(nil, []string{"bob"}, 1)
	if len(result) != 0 {
		t.Fatalf("expected 0 messages for nil input, got %d", len(result))
	}
}

func TestFilterWithContext_EmptyTargets(t *testing.T) {
	msgs := []Message{
		{SenderUID: "alice", Content: "hello"},
	}
	result := FilterWithContext(msgs, nil, 1)
	if result != nil {
		t.Fatalf("expected nil for empty targets, got %d messages", len(result))
	}
}

func TestFilterWithContext_NoMatchingTarget(t *testing.T) {
	msgs := []Message{
		{SenderUID: "alice", Content: "hello"},
		{SenderUID: "bob", Content: "hi"},
	}
	result := FilterWithContext(msgs, []string{"nobody"}, 2)
	if result != nil {
		t.Fatalf("expected nil when target not found, got %d messages", len(result))
	}
}

func TestFilterWithContext_NegativeWindow(t *testing.T) {
	msgs := []Message{
		{SenderUID: "alice", Content: "before"},
		{SenderUID: "bob", Content: "target"},
		{SenderUID: "carol", Content: "after"},
	}
	result := FilterWithContext(msgs, []string{"bob"}, -5)
	if len(result) != 1 {
		t.Fatalf("expected 1 message with negative window (treated as 0), got %d", len(result))
	}
	if result[0].SenderUID != "bob" {
		t.Errorf("expected bob, got %s", result[0].SenderUID)
	}
}

func TestFilterWithContext_OverlappingWindows(t *testing.T) {
	msgs := []Message{
		{SenderUID: "alice", Content: "ctx"},
		{SenderUID: "bob", Content: "target1"},
		{SenderUID: "carol", Content: "between"},
		{SenderUID: "bob", Content: "target2"},
		{SenderUID: "dave", Content: "ctx"},
	}

	result := FilterWithContext(msgs, []string{"bob"}, 1)
	if len(result) != 5 {
		t.Fatalf("expected 5 messages (overlapping windows cover all), got %d", len(result))
	}
	targetCount := 0
	for _, m := range result {
		if m.IsTargetUser {
			targetCount++
		}
	}
	if targetCount != 2 {
		t.Errorf("expected 2 target messages, got %d", targetCount)
	}
}

// TC-POST-02: FilterWithContext 上下文窗口边界测试
// 场景: 目标消息在中间位置，CONTEXT_WINDOW=2
func TestFilterWithContext_MiddlePosition_Window2(t *testing.T) {
	msgs := []Message{
		{SenderUID: "alice", Content: "msg-0"},
		{SenderUID: "alice", Content: "msg-1"},
		{SenderUID: "alice", Content: "msg-2"},
		{SenderUID: "alice", Content: "msg-3"},
		{SenderUID: "bob", Content: "msg-4-target"}, // 目标消息在 index 4
		{SenderUID: "alice", Content: "msg-5"},
		{SenderUID: "alice", Content: "msg-6"},
		{SenderUID: "alice", Content: "msg-7"},
	}

	result := FilterWithContext(msgs, []string{"bob"}, 2)

	// 预期保留: msg-2, msg-3, msg-4-target, msg-5, msg-6 (前后各2条)
	if len(result) != 5 {
		t.Fatalf("expected 5 messages (2 before + target + 2 after), got %d", len(result))
	}
	if result[0].Content != "msg-2" {
		t.Errorf("expected first message msg-2, got %s", result[0].Content)
	}
	if result[2].Content != "msg-4-target" || !result[2].IsTargetUser {
		t.Errorf("expected target message at pos 2, got %s (IsTargetUser=%v)", result[2].Content, result[2].IsTargetUser)
	}
	if result[4].Content != "msg-6" {
		t.Errorf("expected last message msg-6, got %s", result[4].Content)
	}
}

// TC-POST-02: 目标消息在开头位置
func TestFilterWithContext_HeadPosition_Window2(t *testing.T) {
	msgs := []Message{
		{SenderUID: "bob", Content: "msg-0-target"}, // 目标消息在 index 0
		{SenderUID: "alice", Content: "msg-1"},
		{SenderUID: "alice", Content: "msg-2"},
		{SenderUID: "alice", Content: "msg-3"},
	}

	result := FilterWithContext(msgs, []string{"bob"}, 2)

	// 预期保留: msg-0-target, msg-1, msg-2 (只有后2条，前面没有)
	if len(result) != 3 {
		t.Fatalf("expected 3 messages (target + 2 after), got %d", len(result))
	}
	if result[0].Content != "msg-0-target" || !result[0].IsTargetUser {
		t.Errorf("expected target message at pos 0")
	}
	if result[2].Content != "msg-2" {
		t.Errorf("expected last message msg-2, got %s", result[2].Content)
	}
}

// TC-POST-02: 连续目标消息，窗口合并
func TestFilterWithContext_ConsecutiveTargets_Window2(t *testing.T) {
	msgs := []Message{
		{SenderUID: "alice", Content: "msg-0"},
		{SenderUID: "alice", Content: "msg-1"},
		{SenderUID: "bob", Content: "msg-2-target"}, // 连续目标1
		{SenderUID: "bob", Content: "msg-3-target"}, // 连续目标2
		{SenderUID: "alice", Content: "msg-4"},
		{SenderUID: "alice", Content: "msg-5"},
	}

	result := FilterWithContext(msgs, []string{"bob"}, 2)

	// 预期保留: msg-0, msg-1, msg-2-target, msg-3-target, msg-4, msg-5 (窗口合并覆盖全部)
	if len(result) != 6 {
		t.Fatalf("expected 6 messages (merged windows), got %d", len(result))
	}

	// 验证目标标记
	targetCount := 0
	for _, m := range result {
		if m.IsTargetUser {
			targetCount++
		}
	}
	if targetCount != 2 {
		t.Errorf("expected 2 target messages, got %d", targetCount)
	}
}

// TC-POST-02: 目标消息在末尾位置
func TestFilterWithContext_TailPosition_Window2(t *testing.T) {
	msgs := []Message{
		{SenderUID: "alice", Content: "msg-0"},
		{SenderUID: "alice", Content: "msg-1"},
		{SenderUID: "alice", Content: "msg-2"},
		{SenderUID: "bob", Content: "msg-3-target"}, // 目标消息在末尾
	}

	result := FilterWithContext(msgs, []string{"bob"}, 2)

	// 预期保留: msg-1, msg-2, msg-3-target (前2条，后面没有)
	if len(result) != 3 {
		t.Fatalf("expected 3 messages (2 before + target), got %d", len(result))
	}
	if result[0].Content != "msg-1" {
		t.Errorf("expected first message msg-1, got %s", result[0].Content)
	}
	if result[2].Content != "msg-3-target" || !result[2].IsTargetUser {
		t.Errorf("expected target message at pos 2")
	}
}
