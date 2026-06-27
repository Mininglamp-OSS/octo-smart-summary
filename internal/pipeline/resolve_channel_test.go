package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func mockChannelScopeToolCallFn(result ChannelScopeResult) LLMToolCallFn {
	return func(ctx context.Context, msgs []service.ChatMessage, tools []service.Tool, forceFn string) (string, error) {
		b, _ := json.Marshal(result)
		return string(b), nil
	}
}

func extractIDs(channels []ChannelInfo) []string {
	var ids []string
	for _, ch := range channels {
		ids = append(ids, ch.ChannelID)
	}
	return ids
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- filterByChannelTypes tests ---

func TestFilterByChannelTypes(t *testing.T) {
	candidates := []ChannelInfo{
		{ChannelID: "g1", ChannelType: 2, ChannelName: "group1"},
		{ChannelID: "d1", ChannelType: 1, ChannelName: "dm1"},
		{ChannelID: "t1", ChannelType: 5, ChannelName: "thread1"},
	}
	tests := []struct {
		name    string
		types   []string
		wantIDs []string
	}{
		{"filter group only", []string{"group"}, []string{"g1"}},
		{"filter dm only", []string{"dm"}, []string{"d1"}},
		{"filter thread only", []string{"thread"}, []string{"t1"}},
		{"filter group and dm", []string{"group", "dm"}, []string{"g1", "d1"}},
		{"filter all three", []string{"group", "dm", "thread"}, []string{"g1", "d1", "t1"}},
		{"empty array no filter", []string{}, []string{"g1", "d1", "t1"}},
		{"invalid type no filter", []string{"invalid"}, []string{"g1", "d1", "t1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := filterByChannelTypes(candidates, tt.types)
			gotIDs := extractIDs(result)
			if !sliceEqual(gotIDs, tt.wantIDs) {
				t.Errorf("got %v, want %v", gotIDs, tt.wantIDs)
			}
		})
	}
}

// --- filterByChannelIDs tests ---

func TestFilterByChannelIDs(t *testing.T) {
	candidates := []ChannelInfo{
		{ChannelID: "ch1", ChannelName: "octo-dev"},
		{ChannelID: "ch2", ChannelName: "octo-design"},
		{ChannelID: "ch3", ChannelName: "random"},
	}
	tests := []struct {
		name        string
		selectedIDs []string
		wantIDs     []string
	}{
		{"single match", []string{"ch1"}, []string{"ch1"}},
		{"multiple match", []string{"ch1", "ch3"}, []string{"ch1", "ch3"}},
		{"invalid ID returns current unchanged", []string{"nonexistent"}, []string{"ch1", "ch2", "ch3"}},
		{"mix valid and invalid", []string{"ch2", "bad"}, []string{"ch2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := filterByChannelIDs(candidates, tt.selectedIDs, candidates)
			gotIDs := extractIDs(result)
			if !sliceEqual(gotIDs, tt.wantIDs) {
				t.Errorf("got %v, want %v", gotIDs, tt.wantIDs)
			}
		})
	}
}

// --- filterByOwnership tests ---

func TestFilterByOwnership_NilDB(t *testing.T) {
	candidates := []ChannelInfo{
		{ChannelID: "g1", ChannelType: 2},
		{ChannelID: "d1", ChannelType: 1},
	}
	result := filterByOwnership(context.Background(), candidates, "creator_uid", []string{"creator"}, nil)
	gotIDs := extractIDs(result)
	wantIDs := []string{"g1", "d1"}
	if !sliceEqual(gotIDs, wantIDs) {
		t.Errorf("got %v, want %v (nil DB should return candidates as-is)", gotIDs, wantIDs)
	}
}

func TestFilterByOwnership_NoGroups(t *testing.T) {
	candidates := []ChannelInfo{
		{ChannelID: "d1", ChannelType: 1},
		{ChannelID: "t1", ChannelType: 5},
	}
	result := filterByOwnership(context.Background(), candidates, "creator_uid", []string{"creator"}, nil)
	// nil DB returns candidates as-is; but with no groups even a real DB should return nil
	// For nil DB it returns candidates (early return). Test the nil case only.
	gotIDs := extractIDs(result)
	wantIDs := []string{"d1", "t1"}
	if !sliceEqual(gotIDs, wantIDs) {
		t.Errorf("got %v, want %v", gotIDs, wantIDs)
	}
}

// --- ResolveChannelScope tests ---

func TestResolveChannelScope_NoConstraint(t *testing.T) {
	candidates := []ChannelInfo{
		{ChannelID: "g1", ChannelType: 2},
		{ChannelID: "d1", ChannelType: 1},
	}
	memberMap := map[string]string{"uid_a": "Alice"}
	mockFn := mockChannelScopeToolCallFn(ChannelScopeResult{
		HasConstraint: false,
		Rules:         []ChannelScopeRule{},
		Reasoning:     "generic topic",
	})

	result := ResolveChannelScope(context.Background(), "项目进度", candidates, "creator", memberMap, nil, mockFn)
	gotIDs := extractIDs(result)
	wantIDs := []string{"g1", "d1"}
	if !sliceEqual(gotIDs, wantIDs) {
		t.Errorf("got %v, want %v", gotIDs, wantIDs)
	}
}

func TestResolveChannelScope_ByChannelType(t *testing.T) {
	candidates := []ChannelInfo{
		{ChannelID: "g1", ChannelType: 2, ChannelName: "group1"},
		{ChannelID: "g2", ChannelType: 2, ChannelName: "group2"},
		{ChannelID: "d1", ChannelType: 1, ChannelName: "dm1"},
		{ChannelID: "t1", ChannelType: 5, ChannelName: "thread1"},
	}
	memberMap := map[string]string{"uid_a": "Alice"}
	mockFn := mockChannelScopeToolCallFn(ChannelScopeResult{
		HasConstraint: true,
		Rules:         []ChannelScopeRule{{ChannelType: []string{"dm"}}},
		Reasoning:     "DM only",
	})

	result := ResolveChannelScope(context.Background(), "私聊里说了什么", candidates, "creator", memberMap, nil, mockFn)
	gotIDs := extractIDs(result)
	wantIDs := []string{"d1"}
	if !sliceEqual(gotIDs, wantIDs) {
		t.Errorf("got %v, want %v", gotIDs, wantIDs)
	}
}

func TestResolveChannelScope_ByPersonIntersection(t *testing.T) {
	candidates := []ChannelInfo{
		{ChannelID: "ch1", ChannelType: 2},
		{ChannelID: "ch2", ChannelType: 2},
		{ChannelID: "ch3", ChannelType: 2},
	}
	memberMap := map[string]string{"uid_a": "PersonA"}

	// Mock GetUserChannels: uid_a has ch1, ch2 (not ch3)
	// We can't easily mock GetUserChannels since it queries DB.
	// Instead, test the filter functions directly and test ResolveChannelScope
	// with channel_ids which don't require DB.
	mockFn := mockChannelScopeToolCallFn(ChannelScopeResult{
		HasConstraint: true,
		Rules:         []ChannelScopeRule{{ChannelIDs: []string{"ch1", "ch2"}, PersonMode: "intersection"}},
		Reasoning:     "person intersection via channel_ids",
	})

	result := ResolveChannelScope(context.Background(), "我和PersonA聊了什么", candidates, "creator", memberMap, nil, mockFn)
	gotIDs := extractIDs(result)
	wantIDs := []string{"ch1", "ch2"}
	if !sliceEqual(gotIDs, wantIDs) {
		t.Errorf("got %v, want %v", gotIDs, wantIDs)
	}
}

func TestResolveChannelScope_ByPersonUnion_IncludeSelf(t *testing.T) {
	candidates := []ChannelInfo{
		{ChannelID: "ch1", ChannelType: 2},
		{ChannelID: "ch2", ChannelType: 2},
		{ChannelID: "ch3", ChannelType: 2},
	}
	memberMap := map[string]string{"uid_a": "PersonA"}

	// With include_self=true in union mode + persons, all candidates should be returned
	// since the creator's channels = all candidates.
	// But since we can't mock DB for person lookup, use include_self only to verify
	// that all candidates are returned (creator's channels = full set).
	mockFn := mockChannelScopeToolCallFn(ChannelScopeResult{
		HasConstraint: true,
		Rules:         []ChannelScopeRule{{IncludeSelf: true, PersonMode: "union"}},
		Reasoning:     "union with include_self = all candidates",
	})

	result := ResolveChannelScope(context.Background(), "我在搞什么", candidates, "creator", memberMap, nil, mockFn)
	gotIDs := extractIDs(result)
	wantIDs := []string{"ch1", "ch2", "ch3"}
	if !sliceEqual(gotIDs, wantIDs) {
		t.Errorf("got %v, want %v", gotIDs, wantIDs)
	}
}

func TestResolveChannelScope_ByOwnership(t *testing.T) {
	// Without a real DB, ownership filter with nil DB returns candidates as-is.
	// So we test the rule logic flow: channel_type + ownership (nil DB bypass).
	candidates := []ChannelInfo{
		{ChannelID: "g1", ChannelType: 2, ChannelName: "group1"},
		{ChannelID: "d1", ChannelType: 1, ChannelName: "dm1"},
	}
	memberMap := map[string]string{"uid_a": "Alice"}
	mockFn := mockChannelScopeToolCallFn(ChannelScopeResult{
		HasConstraint: true,
		Rules:         []ChannelScopeRule{{Ownership: []string{"creator"}, ChannelType: []string{"group"}}},
		Reasoning:     "creator's groups",
	})

	// channel_type filter runs first → only g1 left
	// ownership filter with nil DB returns candidates (g1) unchanged
	result := ResolveChannelScope(context.Background(), "我建的群", candidates, "creator", memberMap, nil, mockFn)
	gotIDs := extractIDs(result)
	wantIDs := []string{"g1"}
	if !sliceEqual(gotIDs, wantIDs) {
		t.Errorf("got %v, want %v", gotIDs, wantIDs)
	}
}

func TestResolveChannelScope_DNF_MultiRules(t *testing.T) {
	candidates := []ChannelInfo{
		{ChannelID: "g1", ChannelType: 2, ChannelName: "group1"},
		{ChannelID: "g2", ChannelType: 2, ChannelName: "group2"},
		{ChannelID: "d1", ChannelType: 1, ChannelName: "dm1"},
		{ChannelID: "d2", ChannelType: 1, ChannelName: "dm2"},
	}
	memberMap := map[string]string{"uid_a": "PersonA"}

	// Rule 1: channel_type=["dm"] → d1, d2
	// Rule 2: channel_ids=["g1"] → g1
	// Union → g1, d1, d2
	mockFn := mockChannelScopeToolCallFn(ChannelScopeResult{
		HasConstraint: true,
		Rules: []ChannelScopeRule{
			{ChannelType: []string{"dm"}},
			{ChannelIDs: []string{"g1"}},
		},
		Reasoning: "DNF: all DMs + specific group",
	})

	result := ResolveChannelScope(context.Background(), "我的私聊以及g1群", candidates, "creator", memberMap, nil, mockFn)
	gotIDs := extractIDs(result)
	wantIDs := []string{"g1", "d1", "d2"}
	if !sliceEqual(gotIDs, wantIDs) {
		t.Errorf("got %v, want %v", gotIDs, wantIDs)
	}
}

func TestResolveChannelScope_EmptyRuleResult(t *testing.T) {
	candidates := []ChannelInfo{
		{ChannelID: "g1", ChannelType: 2},
		{ChannelID: "d1", ChannelType: 1},
	}
	memberMap := map[string]string{"uid_a": "Alice"}

	// Rule filters channel_type=["thread"] but no thread exists → empty result
	// Single rule produces empty, no per-step fallback.
	// But global fallback: all rules union is empty → return all candidates.
	mockFn := mockChannelScopeToolCallFn(ChannelScopeResult{
		HasConstraint: true,
		Rules:         []ChannelScopeRule{{ChannelType: []string{"thread"}}},
		Reasoning:     "thread only",
	})

	result := ResolveChannelScope(context.Background(), "子区讨论", candidates, "creator", memberMap, nil, mockFn)
	gotIDs := extractIDs(result)
	wantIDs := []string{"g1", "d1"}
	if !sliceEqual(gotIDs, wantIDs) {
		t.Errorf("got %v, want %v (should fallback to all candidates)", gotIDs, wantIDs)
	}
}

func TestResolveChannelScope_GlobalFallback(t *testing.T) {
	candidates := []ChannelInfo{
		{ChannelID: "g1", ChannelType: 2},
		{ChannelID: "g2", ChannelType: 2},
		{ChannelID: "d1", ChannelType: 1},
	}
	memberMap := map[string]string{"uid_a": "Alice"}

	// All channel_ids are invalid → filterByChannelIDs returns current unchanged,
	// but channel_type=["thread"] filters all out → empty result → global fallback
	mockFn := mockChannelScopeToolCallFn(ChannelScopeResult{
		HasConstraint: true,
		Rules: []ChannelScopeRule{
			{ChannelType: []string{"thread"}, ChannelIDs: []string{"nonexistent"}},
		},
		Reasoning:     "all invalid",
	})

	result := ResolveChannelScope(context.Background(), "不存在", candidates, "creator", memberMap, nil, mockFn)
	gotIDs := extractIDs(result)
	wantIDs := []string{"g1", "g2", "d1"}
	if !sliceEqual(gotIDs, wantIDs) {
		t.Errorf("got %v, want %v (global fallback)", gotIDs, wantIDs)
	}
}

// TestResolveChannelScope_ForceFitGuard reproduces issue #79: the LLM returns
// has_constraint=true with a single channel_id that is a valid candidate but
// has no keyword overlap with the topic (e.g. topic "octo" → channel "hello-v2").
// The deterministic guard should detect this and fall back to all candidates.
func TestResolveChannelScope_ForceFitGuard(t *testing.T) {
	candidates := []ChannelInfo{
		{ChannelID: "ch1", ChannelType: 2, ChannelName: "hello-v2"},
		{ChannelID: "ch2", ChannelType: 2, ChannelName: "random-chat"},
		{ChannelID: "ch3", ChannelType: 2, ChannelName: "water-cooler"},
		{ChannelID: "ch4", ChannelType: 2, ChannelName: "announcements"},
		{ChannelID: "ch5", ChannelType: 2, ChannelName: "general"},
	}
	memberMap := map[string]string{"uid_a": "Alice"}

	// Simulate LLM force-fitting: topic mentions "octo" but no channel matches.
	// LLM picks "hello-v2" as the "nearest" channel.
	mockFn := mockChannelScopeToolCallFn(ChannelScopeResult{
		HasConstraint: true,
		Rules:         []ChannelScopeRule{{ChannelIDs: []string{"ch1"}}},
		Reasoning:     "no channel named octo, picking hello-v2 as nearest",
	})

	result := ResolveChannelScope(context.Background(), "octo的项目进展", candidates, "creator", memberMap, nil, mockFn)
	gotIDs := extractIDs(result)
	wantIDs := extractIDs(candidates) // should fall back to all candidates
	if !sliceEqual(gotIDs, wantIDs) {
		t.Errorf("force-fit guard: got %v, want %v (should fallback to all candidates)", gotIDs, wantIDs)
	}
}

// TestResolveChannelScope_ForceFitGuard_LegitimateNarrow ensures the guard
// does NOT trigger when the topic keyword legitimately matches a channel name.
func TestResolveChannelScope_ForceFitGuard_LegitimateNarrow(t *testing.T) {
	candidates := []ChannelInfo{
		{ChannelID: "ch1", ChannelType: 2, ChannelName: "octo-dev"},
		{ChannelID: "ch2", ChannelType: 2, ChannelName: "random-chat"},
		{ChannelID: "ch3", ChannelType: 2, ChannelName: "water-cooler"},
		{ChannelID: "ch4", ChannelType: 2, ChannelName: "announcements"},
	}
	memberMap := map[string]string{"uid_a": "Alice"}

	// Topic mentions "octo" and "octo-dev" is a real match.
	mockFn := mockChannelScopeToolCallFn(ChannelScopeResult{
		HasConstraint: true,
		Rules:         []ChannelScopeRule{{ChannelIDs: []string{"ch1"}}},
		Reasoning:     "octo matches octo-dev",
	})

	result := ResolveChannelScope(context.Background(), "octo的项目进展", candidates, "creator", memberMap, nil, mockFn)
	gotIDs := extractIDs(result)
	wantIDs := []string{"ch1"} // should NOT fall back; legitimate match
	if !sliceEqual(gotIDs, wantIDs) {
		t.Errorf("legitimate narrow: got %v, want %v", gotIDs, wantIDs)
	}
}

// TestTopicOverlapsChannel tests the keyword overlap helper directly.
func TestTopicOverlapsChannel(t *testing.T) {
	tests := []struct {
		topic       string
		channelName string
		want        bool
	}{
		// Force-fit: no overlap
		{"octo的项目进展", "hello-v2", false},
		{"octo项目进展", "random-chat", false},
		// Legitimate match: ASCII keyword present in name
		{"octo项目进展", "octo-dev", true},
		{"octo项目进展", "my-octo-group", true},
		// Multiple tokens, at least one matches
		{"octo项目进展", "octo讨论组", true}, // "octo" matches
		// "octo" not in name, only CJK tokens remain → no overlap
		{"octo项目进展", "进展讨论组", false},
		// All-Chinese topic: no extractable tokens → assume match (conservative)
		{"项目进展总结", "random", true},
		// Single-char ASCII tokens ignored, CJK tokens ignored → no extractable → conservative match
		{"a的b项目", "hello", true},
	}
	for _, tt := range tests {
		t.Run(tt.topic+"→"+tt.channelName, func(t *testing.T) {
			got := topicOverlapsChannel(tt.topic, tt.channelName)
			if got != tt.want {
				t.Errorf("topicOverlapsChannel(%q, %q) = %v, want %v", tt.topic, tt.channelName, got, tt.want)
			}
		})
	}
}

func TestResolveChannelScope_LLMError_Fallback(t *testing.T) {
	candidates := []ChannelInfo{
		{ChannelID: "g1", ChannelType: 2},
		{ChannelID: "d1", ChannelType: 1},
	}
	memberMap := map[string]string{"uid_a": "Alice"}
	errorFn := func(ctx context.Context, msgs []service.ChatMessage, tools []service.Tool, forceFn string) (string, error) {
		return "", fmt.Errorf("timeout")
	}

	result := ResolveChannelScope(context.Background(), "topic", candidates, "creator", memberMap, nil, errorFn)
	gotIDs := extractIDs(result)
	wantIDs := []string{"g1", "d1"}
	if !sliceEqual(gotIDs, wantIDs) {
		t.Errorf("got %v, want %v (LLM error fallback)", gotIDs, wantIDs)
	}
}

func TestResolveChannelScope_EmptyTopic(t *testing.T) {
	candidates := []ChannelInfo{
		{ChannelID: "g1", ChannelType: 2},
	}
	memberMap := map[string]string{"uid_a": "Alice"}
	mockFn := mockChannelScopeToolCallFn(ChannelScopeResult{})

	result := ResolveChannelScope(context.Background(), "", candidates, "creator", memberMap, nil, mockFn)
	if len(result) != 1 || result[0].ChannelID != "g1" {
		t.Errorf("empty topic should return candidates unchanged, got %v", extractIDs(result))
	}
}

func TestResolveChannelScope_EmptyCandidates(t *testing.T) {
	memberMap := map[string]string{"uid_a": "Alice"}
	mockFn := mockChannelScopeToolCallFn(ChannelScopeResult{})

	result := ResolveChannelScope(context.Background(), "topic", nil, "creator", memberMap, nil, mockFn)
	if result != nil {
		t.Errorf("empty candidates should return nil, got %v", result)
	}
}

// --- BuildCandidateMemberMap tests ---

func TestBuildCandidateMemberMap_NilDB(t *testing.T) {
	candidates := []ChannelInfo{
		{ChannelID: "g1", ChannelType: 2},
		{ChannelID: "d1", ChannelType: 1, PeerUID: "uid_a"},
	}
	result, err := BuildCandidateMemberMap(context.Background(), candidates, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("nil DB should return nil, got %v", result)
	}
}

func TestBuildCandidateMemberMap_EmptyCandidates(t *testing.T) {
	result, err := BuildCandidateMemberMap(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("empty candidates should return nil, got %v", result)
	}
}

// --- executeRule tests ---

func TestExecuteRule_ChannelTypeOnly(t *testing.T) {
	candidates := []ChannelInfo{
		{ChannelID: "g1", ChannelType: 2},
		{ChannelID: "g2", ChannelType: 2},
		{ChannelID: "d1", ChannelType: 1},
	}
	rule := ChannelScopeRule{ChannelType: []string{"group"}}
	result := executeRule(context.Background(), rule, candidates, "creator", nil, nil)
	gotIDs := extractIDs(result)
	wantIDs := []string{"g1", "g2"}
	if !sliceEqual(gotIDs, wantIDs) {
		t.Errorf("got %v, want %v", gotIDs, wantIDs)
	}
}

func TestExecuteRule_ChannelTypeAndIDs(t *testing.T) {
	candidates := []ChannelInfo{
		{ChannelID: "g1", ChannelType: 2},
		{ChannelID: "g2", ChannelType: 2},
		{ChannelID: "d1", ChannelType: 1},
	}
	rule := ChannelScopeRule{
		ChannelType: []string{"group"},
		ChannelIDs:  []string{"g1"},
	}
	result := executeRule(context.Background(), rule, candidates, "creator", nil, nil)
	gotIDs := extractIDs(result)
	wantIDs := []string{"g1"}
	if !sliceEqual(gotIDs, wantIDs) {
		t.Errorf("got %v, want %v", gotIDs, wantIDs)
	}
}

func TestExecuteRule_IncludeSelfIntersection(t *testing.T) {
	candidates := []ChannelInfo{
		{ChannelID: "ch1", ChannelType: 2},
		{ChannelID: "ch2", ChannelType: 2},
	}
	memberMap := map[string]string{"uid_a": "Alice"}
	rule := ChannelScopeRule{
		IncludeSelf: true,
		PersonMode:  "intersection",
	}
	// include_self with intersection mode, no other persons:
	// only creator in filterUIDs → intersection with creator skipped → all candidates
	result := executeRule(context.Background(), rule, candidates, "creator", memberMap, nil)
	gotIDs := extractIDs(result)
	wantIDs := []string{"ch1", "ch2"}
	if !sliceEqual(gotIDs, wantIDs) {
		t.Errorf("got %v, want %v", gotIDs, wantIDs)
	}
}

func TestExecuteRule_IncludeSelfUnion(t *testing.T) {
	candidates := []ChannelInfo{
		{ChannelID: "ch1", ChannelType: 2},
		{ChannelID: "ch2", ChannelType: 2},
		{ChannelID: "ch3", ChannelType: 1},
	}
	memberMap := map[string]string{"uid_a": "Alice"}
	rule := ChannelScopeRule{
		IncludeSelf: true,
		PersonMode:  "union",
	}
	// include_self with union mode: creator's channels = all candidates
	result := executeRule(context.Background(), rule, candidates, "creator", memberMap, nil)
	gotIDs := extractIDs(result)
	wantIDs := []string{"ch1", "ch2", "ch3"}
	if !sliceEqual(gotIDs, wantIDs) {
		t.Errorf("got %v, want %v", gotIDs, wantIDs)
	}
}

func TestExecuteRule_InvalidPersonSkipped(t *testing.T) {
	candidates := []ChannelInfo{
		{ChannelID: "ch1", ChannelType: 2},
		{ChannelID: "ch2", ChannelType: 2},
	}
	memberMap := map[string]string{"uid_a": "Alice"}
	rule := ChannelScopeRule{
		Persons:    []string{"uid_invalid"},
		PersonMode: "intersection",
	}
	// Invalid UID skipped → no valid UIDs, no include_self → returns candidates unchanged
	result := executeRule(context.Background(), rule, candidates, "creator", memberMap, nil)
	gotIDs := extractIDs(result)
	wantIDs := []string{"ch1", "ch2"}
	if !sliceEqual(gotIDs, wantIDs) {
		t.Errorf("got %v, want %v (invalid person should not filter)", gotIDs, wantIDs)
	}
}

// --- filterByPersonIntersection / filterByPersonUnion unit tests ---

func TestFilterByPersonIntersection_CreatorOnly(t *testing.T) {
	candidates := []ChannelInfo{
		{ChannelID: "ch1"},
		{ChannelID: "ch2"},
	}
	candidateSet := map[string]bool{"ch1": true, "ch2": true}
	// Only creator in filterUIDs → skip (creator's channels = candidates) → return all
	result := filterByPersonIntersection(context.Background(), candidates, []string{"creator"}, "creator", candidateSet, nil)
	gotIDs := extractIDs(result)
	wantIDs := []string{"ch1", "ch2"}
	if !sliceEqual(gotIDs, wantIDs) {
		t.Errorf("got %v, want %v", gotIDs, wantIDs)
	}
}

func TestFilterByPersonUnion_CreatorOnly(t *testing.T) {
	candidates := []ChannelInfo{
		{ChannelID: "ch1"},
		{ChannelID: "ch2"},
		{ChannelID: "ch3"},
	}
	candidateSet := map[string]bool{"ch1": true, "ch2": true, "ch3": true}
	// Creator in union → all candidates added
	result := filterByPersonUnion(context.Background(), candidates, []string{"creator"}, "creator", candidateSet, nil)
	gotIDs := extractIDs(result)
	wantIDs := []string{"ch1", "ch2", "ch3"}
	if !sliceEqual(gotIDs, wantIDs) {
		t.Errorf("got %v, want %v", gotIDs, wantIDs)
	}
}

// --- callResolveChannelScope tests ---

func TestCallResolveChannelScope_ParseError(t *testing.T) {
	invalidFn := func(ctx context.Context, msgs []service.ChatMessage, tools []service.Tool, forceFn string) (string, error) {
		return "invalid json{{{", nil
	}
	memberMap := map[string]string{"uid_a": "Alice"}
	candidates := []ChannelInfo{{ChannelID: "g1", ChannelType: 2, ChannelName: "test"}}

	_, err := callResolveChannelScope(context.Background(), "topic", candidates, memberMap, invalidFn)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestCallResolveChannelScope_MemberTruncation(t *testing.T) {
	// Build a memberMap with > 500 members
	memberMap := make(map[string]string, 600)
	for i := 0; i < 600; i++ {
		memberMap[fmt.Sprintf("uid_%04d", i)] = fmt.Sprintf("Name%04d", i)
	}
	candidates := []ChannelInfo{{ChannelID: "g1", ChannelType: 2, ChannelName: "test"}}

	var capturedMsg string
	captureFn := func(ctx context.Context, msgs []service.ChatMessage, tools []service.Tool, forceFn string) (string, error) {
		for _, m := range msgs {
			if m.Role == "user" {
				capturedMsg = m.Content
			}
		}
		return `{"has_constraint":false,"rules":[],"reasoning":"test"}`, nil
	}

	_, err := callResolveChannelScope(context.Background(), "topic", candidates, memberMap, captureFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Count "- UID:" lines in captured message
	count := 0
	for _, line := range splitLines(capturedMsg) {
		if len(line) > 6 && line[:6] == "- UID:" {
			count++
		}
	}
	if count > 500 {
		t.Errorf("member list should be truncated to 500, got %d", count)
	}
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// --- filterByOwnership Thread/DM tests ---

func TestFilterByOwnership_ThreadCreator(t *testing.T) {
	// Thread channels with ownership=["creator"] — only creator_uid matched threads kept.
	// Since we can't use a real DB, nil DB returns candidates as-is (early return).
	// This test verifies the collection/parsing logic doesn't panic and the nil DB path works.
	candidates := []ChannelInfo{
		{ChannelID: "grp001____tid_abc", ChannelType: 5, ChannelName: "thread1"},
		{ChannelID: "grp001____tid_def", ChannelType: 5, ChannelName: "thread2"},
	}
	result := filterByOwnership(context.Background(), candidates, "uid_creator", []string{"creator"}, nil)
	gotIDs := extractIDs(result)
	wantIDs := []string{"grp001____tid_abc", "grp001____tid_def"}
	if !sliceEqual(gotIDs, wantIDs) {
		t.Errorf("got %v, want %v (nil DB should return candidates as-is)", gotIDs, wantIDs)
	}
}

func TestFilterByOwnership_DMPassthrough(t *testing.T) {
	// DM channels are always passed through regardless of ownership filter.
	candidates := []ChannelInfo{
		{ChannelID: "dm1", ChannelType: 1, ChannelName: "DM with Alice"},
		{ChannelID: "dm2", ChannelType: 1, ChannelName: "DM with Bob"},
		{ChannelID: "g1", ChannelType: 2, ChannelName: "group1"},
	}
	// nil DB → returns candidates unchanged (early return for nil DB)
	result := filterByOwnership(context.Background(), candidates, "uid_creator", []string{"creator"}, nil)
	gotIDs := extractIDs(result)
	wantIDs := []string{"dm1", "dm2", "g1"}
	if !sliceEqual(gotIDs, wantIDs) {
		t.Errorf("got %v, want %v", gotIDs, wantIDs)
	}
}

func TestFilterByOwnership_MixedTypes(t *testing.T) {
	// Mixed Group + Thread + DM candidates: DM always passes through.
	// With nil DB it returns candidates as-is.
	candidates := []ChannelInfo{
		{ChannelID: "g1", ChannelType: 2, ChannelName: "group1"},
		{ChannelID: "grp001____tid_abc", ChannelType: 5, ChannelName: "thread1"},
		{ChannelID: "dm1", ChannelType: 1, ChannelName: "dm1"},
	}
	result := filterByOwnership(context.Background(), candidates, "uid_creator", []string{"creator"}, nil)
	gotIDs := extractIDs(result)
	wantIDs := []string{"g1", "grp001____tid_abc", "dm1"}
	if !sliceEqual(gotIDs, wantIDs) {
		t.Errorf("got %v, want %v (nil DB returns candidates)", gotIDs, wantIDs)
	}
}

func TestFilterByOwnership_NoGroupsNoThreads(t *testing.T) {
	// All DM channels → early return (no groups, no threads to filter).
	candidates := []ChannelInfo{
		{ChannelID: "dm1", ChannelType: 1},
		{ChannelID: "dm2", ChannelType: 1},
		{ChannelID: "dm3", ChannelType: 1},
	}
	result := filterByOwnership(context.Background(), candidates, "uid_creator", []string{"creator"}, nil)
	gotIDs := extractIDs(result)
	wantIDs := []string{"dm1", "dm2", "dm3"}
	if !sliceEqual(gotIDs, wantIDs) {
		t.Errorf("got %v, want %v (all DM = early return)", gotIDs, wantIDs)
	}
}

func TestFilterByOwnership_MalformedThreadID(t *testing.T) {
	// Malformed thread channel_id (no "____" separator) should be skipped without panic.
	candidates := []ChannelInfo{
		{ChannelID: "malformed_no_separator", ChannelType: 5},
		{ChannelID: "____", ChannelType: 5},
		{ChannelID: "____tid_only", ChannelType: 5},
		{ChannelID: "grp____", ChannelType: 5},
		{ChannelID: "a____b____c", ChannelType: 5},
		{ChannelID: "dm1", ChannelType: 1},
	}
	// nil DB → early return since no valid groups or threads parsed
	// But dm1 is type 1, malformed type 5 won't parse into threadShortIDs.
	// groupIDs=[] and threadShortIDs=[] → early return → returns candidates.
	result := filterByOwnership(context.Background(), candidates, "uid_creator", []string{"creator"}, nil)
	gotIDs := extractIDs(result)
	wantIDs := []string{"malformed_no_separator", "____", "____tid_only", "grp____", "a____b____c", "dm1"}
	if !sliceEqual(gotIDs, wantIDs) {
		t.Errorf("got %v, want %v (malformed thread IDs + DM = early return)", gotIDs, wantIDs)
	}
}

func TestCallResolveChannelScope_ChannelTruncation(t *testing.T) {
	memberMap := map[string]string{"uid_a": "Alice"}
	// Build > 200 candidates
	candidates := make([]ChannelInfo, 250)
	for i := 0; i < 250; i++ {
		candidates[i] = ChannelInfo{ChannelID: fmt.Sprintf("ch_%03d", i), ChannelType: 2, ChannelName: fmt.Sprintf("group_%03d", i)}
	}

	var capturedMsg string
	captureFn := func(ctx context.Context, msgs []service.ChatMessage, tools []service.Tool, forceFn string) (string, error) {
		for _, m := range msgs {
			if m.Role == "user" {
				capturedMsg = m.Content
			}
		}
		return `{"has_constraint":false,"rules":[],"reasoning":"test"}`, nil
	}

	_, err := callResolveChannelScope(context.Background(), "topic", candidates, memberMap, captureFn)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Count "- ID:" lines
	count := 0
	for _, line := range splitLines(capturedMsg) {
		if len(line) > 5 && line[:5] == "- ID:" {
			count++
		}
	}
	if count > 200 {
		t.Errorf("channel list should be truncated to 200, got %d", count)
	}
}

func TestFilterByOwnership_AllQueriesFail(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	// Pin to a single connection so the in-memory DB stays consistent across the
	// connection pool (bare ":memory:" is per-connection); avoids shuffle-order flakiness.
	if sqlDB, e := db.DB(); e == nil {
		sqlDB.SetMaxOpenConns(1)
	}

	candidates := []ChannelInfo{
		{ChannelID: "g1", ChannelType: 2, ChannelName: "group1"},
		{ChannelID: "g2", ChannelType: 2, ChannelName: "group2"},
		{ChannelID: "grp001____tid_abc", ChannelType: 5, ChannelName: "thread1"},
		{ChannelID: "dm1", ChannelType: 1, ChannelName: "dm1"},
	}

	result := filterByOwnership(context.Background(), candidates, "uid_creator", []string{"creator"}, db)
	gotIDs := extractIDs(result)
	wantIDs := extractIDs(candidates)
	if !sliceEqual(gotIDs, wantIDs) {
		t.Errorf("got %v, want %v (all queries failed should return candidates)", gotIDs, wantIDs)
	}
}
