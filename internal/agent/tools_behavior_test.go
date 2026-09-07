package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

// TestMessageCache_BasicOperations tests cache store/retrieve with uid isolation.
func TestMessageCache_BasicOperations(t *testing.T) {
	// Clear cache
	ResetForTest()

	// Store messages with uid
	msgs := []pipeline.Message{
		{SenderName: "Alice", Content: "Hello"},
		{SenderName: "Bob", Content: "World"},
	}
	uid := "user-abc123"
	handle := messageCache.Store(msgs, uid, "session-1")

	if handle == "" {
		t.Fatal("store should return non-empty handle")
	}

	// Retrieve with correct uid
	retrieved := messageCache.Retrieve(handle, uid, "session-1")
	if retrieved == nil {
		t.Fatal("retrieve should return messages")
	}
	if len(retrieved) != 2 {
		t.Errorf("expected 2 messages, got %d", len(retrieved))
	}

	// Retrieve with wrong uid (ownership mismatch)
	wrongRetrieved := messageCache.Retrieve(handle, "other-user", "session-1")
	if wrongRetrieved != nil {
		t.Error("wrong uid should return nil (access denied)")
	}

	// Retrieve invalid handle
	invalid := messageCache.Retrieve("nonexistent-handle", uid, "session-1")
	if invalid != nil {
		t.Error("invalid handle should return nil")
	}
}

func TestMessageCache_SessionScopeIsolation(t *testing.T) {
	ResetForTest()
	uid := "same-user"
	if handle := messageCache.Store([]pipeline.Message{{Content: "unscoped"}}, uid, ""); handle != "" {
		t.Fatalf("cache accepted an unscoped handle: %q", handle)
	}
	spaceAScope1 := "summaryws:space-a:session-1:scope-1"
	handle := messageCache.Store([]pipeline.Message{{Content: "scoped"}}, uid, spaceAScope1)

	if got := messageCache.Retrieve(handle, uid, spaceAScope1); len(got) != 1 || got[0].Content != "scoped" {
		t.Fatalf("same scope retrieve = %#v", got)
	}
	for _, other := range []string{
		"summaryws:space-a:session-2:scope-1", // public session changed
		"summaryws:space-b:session-1:scope-1", // space changed
		"summaryws:space-a:session-1:scope-2", // scope version changed
	} {
		if got := messageCache.Retrieve(handle, uid, other); got != nil {
			t.Fatalf("cross identity %q retrieved %#v", other, got)
		}
	}
}

func TestDerivedMessageHandlesInheritSessionScope(t *testing.T) {
	ResetForTest()
	SetSummaryDeps(nil, nil, nil, config.Config{})
	uid, sessionID := "user-1", "summaryws:space-a:session-1:scope-3"
	ctx := context.WithValue(context.Background(), ContextKeyUID, uid)
	ctx = context.WithValue(ctx, ContextKeySessionID, sessionID)
	source := messageCache.Store([]pipeline.Message{
		{Content: "alpha decision", SenderUID: "alice"},
		{Content: "unrelated", SenderUID: "bob"},
	}, uid, sessionID)

	_, search := SearchMessagesTool()
	searchArgs, _ := json.Marshal(map[string]interface{}{
		"messages_handle": source,
		"keywords":        []string{"alpha"},
	})
	searchResult, err := search(ctx, searchArgs)
	if err != nil {
		t.Fatalf("search same scope: %v", err)
	}
	var searched struct {
		Handle string `json:"messages_handle"`
	}
	if err := json.Unmarshal([]byte(searchResult), &searched); err != nil || searched.Handle == "" {
		t.Fatalf("search result = %q, err=%v", searchResult, err)
	}
	if got := messageCache.Retrieve(searched.Handle, uid, sessionID); len(got) != 1 {
		t.Fatalf("derived search handle in same scope = %#v", got)
	}
	if got := messageCache.Retrieve(searched.Handle, uid, "summaryws:space-a:session-1:scope-4"); got != nil {
		t.Fatalf("derived search handle crossed scope: %#v", got)
	}

	_, filter := FilterRelevantTool()
	filterArgs, _ := json.Marshal(map[string]interface{}{
		"messages_handle":  searched.Handle,
		"participant_uids": []string{"alice"},
	})
	filterResult, err := filter(ctx, filterArgs)
	if err != nil {
		t.Fatalf("filter same scope: %v", err)
	}
	var filtered struct {
		Handle string `json:"messages_handle"`
	}
	if err := json.Unmarshal([]byte(filterResult), &filtered); err != nil || filtered.Handle == "" {
		t.Fatalf("filter result = %q, err=%v", filterResult, err)
	}
	if got := messageCache.Retrieve(filtered.Handle, uid, sessionID); len(got) != 1 {
		t.Fatalf("derived filter handle in same scope = %#v", got)
	}
	if got := messageCache.Retrieve(filtered.Handle, uid, "summaryws:space-b:session-1:scope-3"); got != nil {
		t.Fatalf("derived filter handle crossed space: %#v", got)
	}

	_, summarize := SummarizeChunkTool()
	wrongCtx := context.WithValue(context.Background(), ContextKeyUID, uid)
	wrongCtx = context.WithValue(wrongCtx, ContextKeySessionID, "summaryws:space-b:session-1:scope-3")
	summarizeArgs, _ := json.Marshal(map[string]string{"messages_handle": filtered.Handle})
	if _, err := summarize(wrongCtx, summarizeArgs); err == nil || !strings.Contains(err.Error(), "invalid or expired messages_handle") {
		t.Fatalf("summarize cross-space error = %v", err)
	}
}

// TestMessageCache_StoreRetrieveCycle tests multiple store/retrieve cycles with uid isolation.
func TestMessageCache_StoreRetrieveCycle(t *testing.T) {
	// Clear cache
	ResetForTest()
	uid1 := "user-aaa"
	uid2 := "user-bbb"

	// First store with different uids
	h1 := messageCache.Store([]pipeline.Message{{Content: "msg1"}}, uid1, "session-1")
	h2 := messageCache.Store([]pipeline.Message{{Content: "msg2"}}, uid2, "session-2")

	if h1 == h2 {
		t.Error("handles should be unique")
	}

	// Retrieve both with correct uids
	r1 := messageCache.Retrieve(h1, uid1, "session-1")
	r2 := messageCache.Retrieve(h2, uid2, "session-2")

	if len(r1) != 1 || r1[0].Content != "msg1" {
		t.Errorf("r1 mismatch: %+v", r1)
	}
	if len(r2) != 1 || r2[0].Content != "msg2" {
		t.Errorf("r2 mismatch: %+v", r2)
	}

	// Cross-uid access should fail
	rWrong := messageCache.Retrieve(h1, uid2, "session-1")
	if rWrong != nil {
		t.Error("cross-uid access should return nil")
	}
}

func TestMessageCache_HandlesDoNotCollideAcrossCacheInstances(t *testing.T) {
	first := newMessageCache().Store([]pipeline.Message{{Content: "first"}}, "user-1", "session-1")
	second := newMessageCache().Store([]pipeline.Message{{Content: "second"}}, "user-1", "session-1")
	if first == "" || second == "" || first == second {
		t.Fatalf("independent cache handles collided: first=%q second=%q", first, second)
	}
}

// TestTruncateStr_Behavior tests string truncation edge cases.
func TestTruncateStr_Behavior(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxLen   int
		expected string
	}{
		{"no truncation needed", "short", 10, "short"},
		{"exact length", "12345", 5, "12345"},
		{"truncation", "hello world", 5, "hello..."},
		{"unicode", "你好世界测试", 3, "你好世..."},
		{"empty input", "", 5, ""},
		{"zero max", "abc", 0, "..."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := truncateStr(tt.input, tt.maxLen)
			if result != tt.expected {
				t.Errorf("truncateStr(%q, %d) = %q, want %q", tt.input, tt.maxLen, result, tt.expected)
			}
		})
	}
}

// TestSearchMessages_KeywordMatch tests keyword matching logic.
func TestSearchMessages_KeywordMatch(t *testing.T) {
	tests := []struct {
		text     string
		keywords []string
		want     bool
	}{
		{"hello world", []string{"hello"}, true},
		{"HELLO WORLD", []string{"hello"}, true}, // case-insensitive
		{"test message", []string{"foo", "bar"}, false},
		{"", []string{"test"}, false},
		{"special chars @#$%", []string{"@"}, true},
	}

	for _, tt := range tests {
		result := matchesKeywords(tt.text, tt.keywords)
		if result != tt.want {
			t.Errorf("matchesKeywords(%q, %v) = %v, want %v", tt.text, tt.keywords, result, tt.want)
		}
	}
}

// Helper function to check if a string contains a substring.
func containsStr(s, substr string) bool {
	if len(substr) == 0 {
		return true
	}
	if len(s) < len(substr) {
		return false
	}
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
