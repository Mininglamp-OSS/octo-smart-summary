package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

func mockToolCallFn(result TopicResolveResult) LLMToolCallFn {
	return func(ctx context.Context, msgs []service.ChatMessage, tools []service.Tool, forceFn string) (string, error) {
		b, _ := json.Marshal(result)
		return string(b), nil
	}
}

func TestResolveTopicTarget_IncludeSelf_MeAndAlice(t *testing.T) {
	nameMap := map[string]string{
		"creator_uid": "我",
		"alice_uid":    "Alice",
	}
	stubFn := mockToolCallFn(TopicResolveResult{
		HasTarget:   true,
		UIDs:        []string{"alice_uid"},
		IncludeSelf: true,
		Reasoning:   "主题'我和Alice聊了什么'包含第一人称参与者",
	})

	result := ResolveTopicTarget(context.Background(), "我和Alice聊了什么", nameMap, "creator_uid", stubFn)

	if len(result) != 2 {
		t.Fatalf("expected 2 UIDs, got %d: %v", len(result), result)
	}
	hasAlice, hasCreator := false, false
	for _, uid := range result {
		if uid == "alice_uid" {
			hasAlice = true
		}
		if uid == "creator_uid" {
			hasCreator = true
		}
	}
	if !hasAlice {
		t.Error("expected alice_uid in result")
	}
	if !hasCreator {
		t.Error("expected creator_uid in result (include_self=true)")
	}
}

func TestResolveTopicTarget_IncludeSelf_False(t *testing.T) {
	nameMap := map[string]string{
		"creator_uid": "我",
		"wang_uid":     "老王",
	}
	stubFn := mockToolCallFn(TopicResolveResult{
		HasTarget:   true,
		UIDs:        []string{"wang_uid"},
		IncludeSelf: false,
		Reasoning:   "主题'老王的发言'只关注老王",
	})

	result := ResolveTopicTarget(context.Background(), "老王的发言", nameMap, "creator_uid", stubFn)

	if len(result) != 1 {
		t.Fatalf("expected 1 UID, got %d: %v", len(result), result)
	}
	if result[0] != "wang_uid" {
		t.Errorf("expected wang_uid, got %s", result[0])
	}
}

func TestResolveTopicTarget_IncludeSelf_MultiplePersons(t *testing.T) {
	nameMap := map[string]string{
		"creator_uid": "我",
		"person1_uid": "小李",
		"person2_uid": "小红",
	}
	stubFn := mockToolCallFn(TopicResolveResult{
		HasTarget:   true,
		UIDs:        []string{"person1_uid", "person2_uid"},
		IncludeSelf: true,
		Reasoning:   "主题'我和多人的讨论'包含第一人称参与者",
	})

	result := ResolveTopicTarget(context.Background(), "我和多人的讨论", nameMap, "creator_uid", stubFn)

	if len(result) != 3 {
		t.Fatalf("expected 3 UIDs, got %d: %v", len(result), result)
	}
	expected := map[string]bool{"person1_uid": false, "person2_uid": false, "creator_uid": false}
	for _, uid := range result {
		if _, ok := expected[uid]; ok {
			expected[uid] = true
		}
	}
	for uid, found := range expected {
		if !found {
			t.Errorf("expected %s in result", uid)
		}
	}
}

func TestResolveTopicTarget_IncludeSelf_NoDuplicate(t *testing.T) {
	nameMap := map[string]string{
		"creator_uid": "我",
		"alice_uid":    "Alice",
	}
	stubFn := mockToolCallFn(TopicResolveResult{
		HasTarget:   true,
		UIDs:        []string{"alice_uid", "creator_uid"},
		IncludeSelf: true,
		Reasoning:   "LLM已返回creator_uid",
	})

	result := ResolveTopicTarget(context.Background(), "我和Alice聊了什么", nameMap, "creator_uid", stubFn)

	if len(result) != 2 {
		t.Fatalf("expected 2 UIDs (no duplicate), got %d: %v", len(result), result)
	}
	count := 0
	for _, uid := range result {
		if uid == "creator_uid" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected creator_uid exactly once, got %d times", count)
	}
}

func TestResolveTopicTarget_IncludeSelf_ZeroValue_BackwardCompat(t *testing.T) {
	nameMap := map[string]string{
		"creator_uid": "我",
		"bob_uid":     "Bob",
	}
	stubFn := mockToolCallFn(TopicResolveResult{
		HasTarget: true,
		UIDs:      []string{"bob_uid"},
		Reasoning: "主题指向Bob",
	})

	result := ResolveTopicTarget(context.Background(), "Bob的发言", nameMap, "creator_uid", stubFn)

	if len(result) != 1 {
		t.Fatalf("expected 1 UID (backward compat, include_self defaults false), got %d: %v", len(result), result)
	}
	if result[0] != "bob_uid" {
		t.Errorf("expected bob_uid, got %s", result[0])
	}
}

func TestResolveTopicTarget_NoTarget_GeneralTopic(t *testing.T) {
	nameMap := map[string]string{
		"creator_uid": "我",
		"alice_uid":    "Alice",
	}
	stubFn := mockToolCallFn(TopicResolveResult{
		HasTarget:   false,
		UIDs:        []string{},
		IncludeSelf: false,
		Reasoning:   "主题'看看最近在聊什么'不指向特定成员",
	})

	result := ResolveTopicTarget(context.Background(), "看看最近在聊什么", nameMap, "creator_uid", stubFn)

	if result != nil {
		t.Fatalf("expected nil for general topic, got %v", result)
	}
}

func TestResolveTopicTarget_NoTarget_SummarizeAll(t *testing.T) {
	nameMap := map[string]string{
		"creator_uid": "我",
		"person1_uid": "小李",
	}
	stubFn := mockToolCallFn(TopicResolveResult{
		HasTarget:   false,
		UIDs:        []string{},
		IncludeSelf: false,
		Reasoning:   "主题'总结这些群在聊些什么'不指向特定成员",
	})

	result := ResolveTopicTarget(context.Background(), "总结这些群在聊些什么", nameMap, "creator_uid", stubFn)

	if result != nil {
		t.Fatalf("expected nil for summarize-all topic, got %v", result)
	}
}

func TestResolveTopicTarget_SelfReference(t *testing.T) {
	nameMap := map[string]string{
		"creator_uid": "我",
		"alice_uid":    "Alice",
	}
	stubFn := mockToolCallFn(TopicResolveResult{
		HasTarget:   true,
		UIDs:        []string{},
		IncludeSelf: true,
		Reasoning:   "主题'我最近说了什么'是自我指代",
	})

	result := ResolveTopicTarget(context.Background(), "我最近说了什么", nameMap, "creator_uid", stubFn)

	if len(result) != 1 {
		t.Fatalf("expected 1 UID for self-reference, got %d: %v", len(result), result)
	}
	if result[0] != "creator_uid" {
		t.Errorf("expected creator_uid, got %s", result[0])
	}
}

func TestResolveTopicTarget_AllUIDsInvalid_IncludeSelf(t *testing.T) {
	nameMap := map[string]string{
		"creator_uid": "我",
		"alice_uid":    "Alice",
	}
	stubFn := mockToolCallFn(TopicResolveResult{
		HasTarget:   true,
		UIDs:        []string{"unknown_uid_1", "unknown_uid_2"},
		IncludeSelf: true,
		Reasoning:   "LLM returned invalid UIDs but include_self is true",
	})

	result := ResolveTopicTarget(context.Background(), "我和某人聊了什么", nameMap, "creator_uid", stubFn)

	if len(result) != 1 {
		t.Fatalf("expected 1 UID (creator), got %d: %v", len(result), result)
	}
	if result[0] != "creator_uid" {
		t.Errorf("expected creator_uid, got %s", result[0])
	}
}

func TestResolveTopicTarget_NoSemanticMatch_AliceNotCarol(t *testing.T) {
	nameMap := map[string]string{
		"creator_uid": "我",
		"carol_uid":   "Carol",
	}
	stubFn := mockToolCallFn(TopicResolveResult{
		HasTarget:   false,
		UIDs:        []string{},
		IncludeSelf: false,
		Reasoning:   "Alice与成员Carol之间没有语义关联，不算匹配",
	})

	result := ResolveTopicTarget(context.Background(), "Alice的发言", nameMap, "creator_uid", stubFn)

	if result != nil {
		t.Fatalf("expected nil when topic name has no semantic match in members, got %v", result)
	}
}

func TestResolveTopicTarget_SemanticMatch_Nickname(t *testing.T) {
	nameMap := map[string]string{
		"creator_uid":    "我",
		"wang_uid": "王明",
	}
	stubFn := mockToolCallFn(TopicResolveResult{
		HasTarget:   true,
		UIDs:        []string{"wang_uid"},
		IncludeSelf: false,
		Reasoning:   "老王中的'王'与王明的'王'存在语义关联",
	})

	result := ResolveTopicTarget(context.Background(), "老王的发言", nameMap, "creator_uid", stubFn)

	if len(result) != 1 {
		t.Fatalf("expected 1 UID, got %d: %v", len(result), result)
	}
	if result[0] != "wang_uid" {
		t.Errorf("expected wang_uid, got %s", result[0])
	}
}

func TestResolveTopicTarget_SemanticMatch_BilingualName(t *testing.T) {
	nameMap := map[string]string{
		"creator_uid": "我",
		"tom_uid":  "汤姆",
	}
	stubFn := mockToolCallFn(TopicResolveResult{
		HasTarget:   true,
		UIDs:        []string{"tom_uid"},
		IncludeSelf: false,
		Reasoning:   "Tom是汤姆的英文形式，语义关联明确",
	})

	result := ResolveTopicTarget(context.Background(), "Tom说了什么", nameMap, "creator_uid", stubFn)

	if len(result) != 1 {
		t.Fatalf("expected 1 UID, got %d: %v", len(result), result)
	}
	if result[0] != "tom_uid" {
		t.Errorf("expected tom_uid, got %s", result[0])
	}
}

func TestResolveTopicTarget_EmptyTopic(t *testing.T) {
	nameMap := map[string]string{
		"creator_uid": "我",
	}
	stubFn := mockToolCallFn(TopicResolveResult{})

	result := ResolveTopicTarget(context.Background(), "", nameMap, "creator_uid", stubFn)

	if result != nil {
		t.Fatalf("expected nil for empty topic, got %v", result)
	}
}

func TestResolveTopicTarget_LLMError(t *testing.T) {
	nameMap := map[string]string{
		"creator_uid": "我",
		"alice_uid":    "Alice",
	}
	errorFn := func(ctx context.Context, msgs []service.ChatMessage, tools []service.Tool, forceFn string) (string, error) {
		return "", fmt.Errorf("LLM service unavailable")
	}

	result := ResolveTopicTarget(context.Background(), "Alice的观点", nameMap, "creator_uid", errorFn)

	if result != nil {
		t.Fatalf("expected nil on LLM error, got %v", result)
	}
}
