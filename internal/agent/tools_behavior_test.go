package agent

import (
	"testing"

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
	handle := messageCache.Store(msgs, uid)

	if handle == "" {
		t.Fatal("store should return non-empty handle")
	}

	// Retrieve with correct uid
	retrieved := messageCache.Retrieve(handle, uid)
	if retrieved == nil {
		t.Fatal("retrieve should return messages")
	}
	if len(retrieved) != 2 {
		t.Errorf("expected 2 messages, got %d", len(retrieved))
	}

	// Retrieve with wrong uid (ownership mismatch)
	wrongRetrieved := messageCache.Retrieve(handle, "other-user")
	if wrongRetrieved != nil {
		t.Error("wrong uid should return nil (access denied)")
	}

	// Retrieve invalid handle
	invalid := messageCache.Retrieve("nonexistent-handle", uid)
	if invalid != nil {
		t.Error("invalid handle should return nil")
	}
}

// TestMessageCache_StoreRetrieveCycle tests multiple store/retrieve cycles with uid isolation.
func TestMessageCache_StoreRetrieveCycle(t *testing.T) {
	// Clear cache
	ResetForTest()
	uid1 := "user-aaa"
	uid2 := "user-bbb"

	// First store with different uids
	h1 := messageCache.Store([]pipeline.Message{{Content: "msg1"}}, uid1)
	h2 := messageCache.Store([]pipeline.Message{{Content: "msg2"}}, uid2)

	if h1 == h2 {
		t.Error("handles should be unique")
	}

	// Retrieve both with correct uids
	r1 := messageCache.Retrieve(h1, uid1)
	r2 := messageCache.Retrieve(h2, uid2)

	if len(r1) != 1 || r1[0].Content != "msg1" {
		t.Errorf("r1 mismatch: %+v", r1)
	}
	if len(r2) != 1 || r2[0].Content != "msg2" {
		t.Errorf("r2 mismatch: %+v", r2)
	}

	// Cross-uid access should fail
	rWrong := messageCache.Retrieve(h1, uid2)
	if rWrong != nil {
		t.Error("cross-uid access should return nil")
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
