package pipeline

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

func TestRecognizeIntentWithShortcut_PureGenericTopic(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	start := now.Add(-24 * time.Hour)
	end := now

	result, err := RecognizeIntentWithShortcut(
		ctx, "总结", nil, start, end,
		nil, nil, "user123", true, nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.Skipped {
		t.Error("expected Skipped=true for pure generic topic")
	}
	if result.SkipReason != "pure_generic_topic" {
		t.Errorf("expected SkipReason=pure_generic_topic, got %q", result.SkipReason)
	}
	if result.TimeRange.Start != start || result.TimeRange.End != end {
		t.Error("expected original time range to be preserved")
	}
}

func TestRecognizeIntentWithShortcut_SimpleChannelConstraint(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	start := now.Add(-24 * time.Hour)
	end := now

	result, err := RecognizeIntentWithShortcut(
		ctx,
		"总结本群的关键内容",
		[]string{"group_123"},
		start, end,
		nil, nil, "user123", true, nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.Skipped {
		t.Error("expected Skipped=true for simple channel constraint")
	}
	if result.SkipReason != "simple_channel_constraint" {
		t.Errorf("expected SkipReason=simple_channel_constraint, got %q", result.SkipReason)
	}
}

func TestRecognizeIntentWithShortcut_NeedsLLM(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	start := now.Add(-24 * time.Hour)
	end := now

	// Mock LLM callback
	called := false
	mockToolCallFn := func(ctx context.Context, messages []service.ChatMessage, tools []service.Tool, forceFn string) (string, error) {
		called = true
		// Return a valid response
		resp := recognizeIntentResult{
			HasTimeExpr:          true,
			TimeStart:            now.Add(-7 * 24 * time.Hour).Format(time.RFC3339),
			TimeEnd:              now.Format(time.RFC3339),
			HasChannelConstraint: false,
			HasTarget:            false,
			IncludeSelf:          false,
			Reasoning:            "本周 = 最近7天",
		}
		data, _ := json.Marshal(resp)
		return string(data), nil
	}

	result, err := RecognizeIntentWithShortcut(
		ctx,
		"总结本周工作",
		[]string{"group_123"},
		start, end,
		nil, nil, "user123", true, mockToolCallFn,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !called {
		t.Error("expected LLM callback to be called for topic with time word")
	}
	if result.Skipped {
		t.Error("expected Skipped=false when LLM was called")
	}
	if !result.TimeRange.HasTimeExpr {
		t.Error("expected TimeRange.HasTimeExpr=true")
	}
}

func TestRecognizeIntentWithShortcut_DisabledShortcut(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	start := now.Add(-24 * time.Hour)
	end := now

	called := false
	mockToolCallFn := func(ctx context.Context, messages []service.ChatMessage, tools []service.Tool, forceFn string) (string, error) {
		called = true
		resp := recognizeIntentResult{Reasoning: "test"}
		data, _ := json.Marshal(resp)
		return string(data), nil
	}

	_, err := RecognizeIntentWithShortcut(
		ctx, "总结", nil, start, end,
		nil, nil, "user123", false, mockToolCallFn, // Disabled shortcut
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !called {
		t.Error("expected LLM callback to be called when shortcut is disabled")
	}
}

func TestRecognizeIntent_WithMemberMap(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	start := now.Add(-7 * 24 * time.Hour)
	end := now

	memberMap := map[string]string{
		"uid_alice": "Alice",
		"uid_bob":   "Bob",
		"uid_me":    "我自己",
	}

	mockToolCallFn := func(ctx context.Context, messages []service.ChatMessage, tools []service.Tool, forceFn string) (string, error) {
		resp := recognizeIntentResult{
			HasTimeExpr:          false,
			HasChannelConstraint: false,
			HasTarget:            true,
			TargetUIDs:           []string{"uid_alice"},
			IncludeSelf:          true,
			Reasoning:            "主题指向 Alice 和我",
		}
		data, _ := json.Marshal(resp)
		return string(data), nil
	}

	result, err := RecognizeIntent(ctx, "我和Alice聊了什么", start, end, nil, memberMap, "uid_me", mockToolCallFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.TargetPersons.HasTarget {
		t.Error("expected HasTarget=true")
	}
	if !result.TargetPersons.IncludeSelf {
		t.Error("expected IncludeSelf=true")
	}
	// Should have both Alice and self
	if len(result.TargetPersons.UIDs) != 2 {
		t.Errorf("expected 2 target UIDs, got %d: %v", len(result.TargetPersons.UIDs), result.TargetPersons.UIDs)
	}
}

func TestRecognizeIntent_InvalidUID(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	start := now.Add(-7 * 24 * time.Hour)
	end := now

	memberMap := map[string]string{
		"uid_alice": "Alice",
	}

	mockToolCallFn := func(ctx context.Context, messages []service.ChatMessage, tools []service.Tool, forceFn string) (string, error) {
		resp := recognizeIntentResult{
			HasTarget:   true,
			TargetUIDs:  []string{"uid_alice", "uid_invalid"}, // uid_invalid not in memberMap
			IncludeSelf: false,
			Reasoning:   "test",
		}
		data, _ := json.Marshal(resp)
		return string(data), nil
	}

	result, err := RecognizeIntent(ctx, "Alice和Bob说了什么", start, end, nil, memberMap, "uid_me", mockToolCallFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Only valid UID should be kept
	if len(result.TargetPersons.UIDs) != 1 {
		t.Errorf("expected 1 valid UID, got %d: %v", len(result.TargetPersons.UIDs), result.TargetPersons.UIDs)
	}
	if result.TargetPersons.UIDs[0] != "uid_alice" {
		t.Errorf("expected uid_alice, got %s", result.TargetPersons.UIDs[0])
	}
}

func TestRecognizeIntent_TimeNarrowing(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	start := now.Add(-30 * 24 * time.Hour) // 30 days ago
	end := now

	weekStart := now.Add(-7 * 24 * time.Hour)

	mockToolCallFn := func(ctx context.Context, messages []service.ChatMessage, tools []service.Tool, forceFn string) (string, error) {
		resp := recognizeIntentResult{
			HasTimeExpr: true,
			TimeStart:   weekStart.Format(time.RFC3339),
			TimeEnd:     now.Format(time.RFC3339),
			Reasoning:   "本周 = 最近7天",
		}
		data, _ := json.Marshal(resp)
		return string(data), nil
	}

	result, err := RecognizeIntent(ctx, "本周工作总结", start, end, nil, nil, "uid_me", mockToolCallFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.TimeRange.HasTimeExpr {
		t.Error("expected HasTimeExpr=true")
	}
	if !result.TimeRange.Narrowed {
		t.Error("expected Narrowed=true")
	}
	// Time should be narrowed to ~7 days
	duration := result.TimeRange.End.Sub(result.TimeRange.Start)
	if duration > 8*24*time.Hour || duration < 6*24*time.Hour {
		t.Errorf("expected ~7 days duration, got %v", duration)
	}
}

func TestBuildIntentResultFromLegacy(t *testing.T) {
	now := time.Now()
	originalStart := now.Add(-7 * 24 * time.Hour)
	originalEnd := now
	narrowedStart := now.Add(-3 * 24 * time.Hour)
	narrowedEnd := now

	channelScope := &ChannelScopeResult{
		HasConstraint: true,
		Reasoning:     "user specified group",
	}

	targetUIDs := []string{"user456", "user789"}

	result := BuildIntentResultFromLegacy(
		originalStart, originalEnd,
		narrowedStart, narrowedEnd,
		channelScope,
		targetUIDs,
		true, // includeSelf
	)

	if result.Skipped {
		t.Error("expected Skipped=false for legacy result")
	}
	if !result.TimeRange.Narrowed {
		t.Error("expected TimeRange.Narrowed=true when time was changed")
	}
	if !result.ChannelScope.HasConstraint {
		t.Error("expected ChannelScope.HasConstraint=true")
	}
	if len(result.TargetPersons.UIDs) != 2 {
		t.Errorf("expected 2 target UIDs, got %d", len(result.TargetPersons.UIDs))
	}
	if !result.TargetPersons.IncludeSelf {
		t.Error("expected TargetPersons.IncludeSelf=true")
	}
}

func TestBuildIntentResultFromLegacy_NoChanges(t *testing.T) {
	now := time.Now()
	start := now.Add(-7 * 24 * time.Hour)
	end := now

	result := BuildIntentResultFromLegacy(
		start, end,
		start, end, // same as original
		nil,        // no channel scope
		nil,        // no targets
		false,
	)

	if result.TimeRange.Narrowed {
		t.Error("expected TimeRange.Narrowed=false when time unchanged")
	}
	if result.ChannelScope.HasConstraint {
		t.Error("expected ChannelScope.HasConstraint=false when nil")
	}
	if result.TargetPersons.HasTarget {
		t.Error("expected TargetPersons.HasTarget=false when no targets")
	}
}

// TC-INT-01: 验证 RecognizeIntent 单次 LLM 调用同时返回时间/频道/人物三个维度
func TestRecognizeIntent_CombinedTimeChannelPerson(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	start := now.Add(-30 * 24 * time.Hour)
	end := now

	yesterdayStart := now.Add(-24 * time.Hour).Truncate(24 * time.Hour)
	yesterdayEnd := yesterdayStart.Add(24*time.Hour - time.Second)

	memberMap := map[string]string{
		"uid_alice":   "Alice",
		"uid_bob":     "Bob",
		"uid_creator": "我",
	}

	channels := []ChannelInfo{
		{ChannelID: "dm_alice_creator", ChannelName: "Alice私聊", ChannelType: 1},
		{ChannelID: "group_dev", ChannelName: "开发群", ChannelType: 2},
	}

	mockToolCallFn := func(ctx context.Context, messages []service.ChatMessage, tools []service.Tool, forceFn string) (string, error) {
		// 模拟 LLM 返回：昨天 + 私聊类型 + 目标 Alice + 包含自己
		resp := recognizeIntentResult{
			HasTimeExpr:          true,
			TimeStart:            yesterdayStart.Format(time.RFC3339),
			TimeEnd:              yesterdayEnd.Format(time.RFC3339),
			HasChannelConstraint: true,
			ChannelType:          []string{"dm"},
			ChannelPersons:       []string{"uid_alice"},
			ChannelPersonMode:    "intersection",
			ChannelIncludeSelf:   true,
			HasTarget:            true,
			TargetUIDs:           []string{"uid_alice"},
			IncludeSelf:          true,
			Reasoning:            "昨天我和Alice的私聊 = 时间约束+频道约束+人物约束",
		}
		data, _ := json.Marshal(resp)
		return string(data), nil
	}

	result, err := RecognizeIntent(ctx, "昨天我和Alice聊了什么", start, end, channels, memberMap, "uid_creator", mockToolCallFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 验证时间维度
	if !result.TimeRange.HasTimeExpr {
		t.Error("expected TimeRange.HasTimeExpr=true")
	}
	if !result.TimeRange.Narrowed {
		t.Error("expected TimeRange.Narrowed=true")
	}

	// 验证频道维度
	if !result.ChannelScope.HasConstraint {
		t.Error("expected ChannelScope.HasConstraint=true")
	}
	if len(result.ChannelScope.ChannelType) != 1 || result.ChannelScope.ChannelType[0] != "dm" {
		t.Errorf("expected ChannelType=[dm], got %v", result.ChannelScope.ChannelType)
	}
	if !result.ChannelScope.IncludeSelf {
		t.Error("expected ChannelScope.IncludeSelf=true")
	}

	// 验证人物维度
	if !result.TargetPersons.HasTarget {
		t.Error("expected TargetPersons.HasTarget=true")
	}
	if !result.TargetPersons.IncludeSelf {
		t.Error("expected TargetPersons.IncludeSelf=true")
	}
	// 应该包含 Alice 和 creator
	if len(result.TargetPersons.UIDs) != 2 {
		t.Errorf("expected 2 target UIDs (alice + creator), got %d: %v", len(result.TargetPersons.UIDs), result.TargetPersons.UIDs)
	}
}
