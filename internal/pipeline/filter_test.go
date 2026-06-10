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

// applyTargetFilterWithFallback mirrors the caller-side logic in
// executePersonalPipeline (internal/worker/personal_processor.go): when a target
// is resolved but has no messages in the selected source(s), fall back to all
// fetched messages instead of returning "no chat data" (issue #87).
func applyTargetFilterWithFallback(messages []Message, targetUIDs []string, contextWindow int) []Message {
	if len(targetUIDs) == 0 {
		return messages
	}
	filtered := FilterWithContext(messages, targetUIDs, contextWindow)
	if len(filtered) == 0 {
		return messages
	}
	return filtered
}

// Target person has no messages in the selected source → fall back to all messages.
func TestApplyTargetFilterWithFallback_TargetAbsent_FallsBackToAll(t *testing.T) {
	msgs := []Message{
		{SenderUID: "alice", Content: "hello"},
		{SenderUID: "bob", Content: "hi"},
	}
	// "creator" said nothing in this source.
	result := applyTargetFilterWithFallback(msgs, []string{"creator"}, 1)
	if len(result) != 2 {
		t.Fatalf("expected fallback to all 2 messages, got %d", len(result))
	}
}

// Target person present → normal narrowing applies (no fallback).
func TestApplyTargetFilterWithFallback_TargetPresent_Narrows(t *testing.T) {
	msgs := []Message{
		{SenderUID: "alice", Content: "hello"},
		{SenderUID: "bob", Content: "hi"},
		{SenderUID: "carol", Content: "ok"},
	}
	result := applyTargetFilterWithFallback(msgs, []string{"bob"}, 0)
	if len(result) != 1 {
		t.Fatalf("expected 1 narrowed message, got %d", len(result))
	}
	if result[0].SenderUID != "bob" {
		t.Errorf("expected bob, got %s", result[0].SenderUID)
	}
}

// No target → all messages pass through unchanged.
func TestApplyTargetFilterWithFallback_NoTarget_AllMessages(t *testing.T) {
	msgs := []Message{
		{SenderUID: "alice", Content: "hello"},
		{SenderUID: "bob", Content: "hi"},
	}
	result := applyTargetFilterWithFallback(msgs, nil, 1)
	if len(result) != 2 {
		t.Fatalf("expected all 2 messages with no target, got %d", len(result))
	}
}
