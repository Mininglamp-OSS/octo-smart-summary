package worker

import (
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

func TestExtractCitationIndexes(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []int
	}{
		{"single", "参考消息 [1] 中的内容", []int{1}},
		{"multiple", "根据 [2] 和 [5] 的讨论", []int{2, 5}},
		{"none", "没有引用标记", nil},
		{"dedup", "[3] 重复引用 [3]", []int{3}},
		{"consecutive", "[74][83][91]", []int{74, 83, 91}},
		{"spaced", "[74] [83]", []int{74, 83}},
		{"skip markdown link", "[点击这里](https://example.com)", nil},
		{"mixed", "text [1] and [link](url) and [2][3]", []int{1, 2, 3}},
		{"empty", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractCitationIndexes(tt.text)
			if len(got) != len(tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("index %d: got %d, want %d", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestBuildCitations_WithNameMap(t *testing.T) {
	messages := []pipeline.Message{
		{CitationIndex: 1, SenderUID: "uid_alice", Content: "Hello", SendTime: "2025-01-01T10:00:00Z", SourceName: "群聊A", ChannelID: "ch_group_a", MessageSeq: 1001},
		{CitationIndex: 2, SenderUID: "uid_bob", Content: "World", SendTime: "2025-01-01T10:01:00Z", SourceName: "群聊A", ChannelID: "ch_group_a", MessageSeq: 1002},
		{CitationIndex: 3, SenderUID: "uid_charlie", Content: "Test", SendTime: "2025-01-01T10:02:00Z", SourceName: "群聊B", ChannelID: "ch_group_b", MessageSeq: 2001},
	}
	nameMap := map[string]string{
		"uid_alice": "Alice",
		"uid_bob":   "Bob",
		// uid_charlie is intentionally missing
	}

	text := "讨论内容 [1] 和 [3] 很有价值"
	citations := buildCitations(text, messages, messages, nameMap)

	if len(citations) != 2 {
		t.Fatalf("expected 2 citations, got %d", len(citations))
	}

	if citations[0].Sender != "Alice" {
		t.Errorf("citation[0].Sender = %q, want %q", citations[0].Sender, "Alice")
	}

	if citations[1].Sender != "uid_charlie" {
		t.Errorf("citation[1].Sender = %q, want %q", citations[1].Sender, "uid_charlie")
	}
}

func TestBuildCitations_NilNameMap(t *testing.T) {
	messages := []pipeline.Message{
		{CitationIndex: 1, SenderUID: "uid_alice", Content: "Hello", SendTime: "2025-01-01T10:00:00Z", SourceName: "群聊A", ChannelID: "ch_group_a", MessageSeq: 1001},
	}
	text := "消息 [1] 的内容"
	citations := buildCitations(text, messages, messages, nil)

	if len(citations) != 1 {
		t.Fatalf("expected 1 citation, got %d", len(citations))
	}
	if citations[0].Sender != "uid_alice" {
		t.Errorf("Sender = %q, want %q (should fallback to UID when nameMap is nil)", citations[0].Sender, "uid_alice")
	}
}

func TestBuildCitations_NoCitations(t *testing.T) {
	messages := []pipeline.Message{
		{CitationIndex: 1, SenderUID: "uid_alice", Content: "Hello", SendTime: "2025-01-01T10:00:00Z", ChannelID: "ch_group_a", MessageSeq: 1001},
	}
	nameMap := map[string]string{"uid_alice": "Alice"}
	citations := buildCitations("no citations here", messages, messages, nameMap)
	if len(citations) != 0 {
		t.Errorf("expected empty citations, got %d", len(citations))
	}
}

func TestBuildCitations_ContentTruncation(t *testing.T) {
	longContent := strings.Repeat("测", 300)
	messages := []pipeline.Message{
		{CitationIndex: 1, SenderUID: "uid_a", Content: longContent, SendTime: "2025-01-01T10:00:00Z", ChannelID: "ch_group_a", MessageSeq: 1001},
	}
	citations := buildCitations("[1] long message", messages, messages, nil)
	if len(citations) != 1 {
		t.Fatalf("expected 1 citation, got %d", len(citations))
	}
	runes := []rune(citations[0].Content)
	if len(runes) != 203 {
		t.Errorf("expected 203 runes, got %d", len(runes))
	}
}

func TestBuildCitations_ContextIntegration(t *testing.T) {
	allMessages := []pipeline.Message{
		{MessageSeq: 3001, ChannelID: "ch_proj_dev", SenderUID: "uid_alice", Content: "I pushed the fix for the login bug", SendTime: "2025-01-15T09:00:00Z", SourceName: "项目开发群"},
		{MessageSeq: 3002, ChannelID: "ch_proj_dev", SenderUID: "uid_bob", Content: "Nice, does it handle the edge case?", SendTime: "2025-01-15T09:01:00Z", SourceName: "项目开发群", CitationIndex: 1},
		{MessageSeq: 3003, ChannelID: "ch_proj_dev", SenderUID: "uid_alice", Content: "Yes, added tests for that too", SendTime: "2025-01-15T09:02:00Z", SourceName: "项目开发群"},
		{MessageSeq: 3004, ChannelID: "ch_proj_dev", SenderUID: "uid_charlie", Content: "LGTM, merging now", SendTime: "2025-01-15T09:03:00Z", SourceName: "项目开发群"},
		{MessageSeq: 4001, ChannelID: "ch_design", SenderUID: "uid_dave", Content: "Updated the mockups", SendTime: "2025-01-15T09:04:00Z", SourceName: "设计群", CitationIndex: 2},
		{MessageSeq: 4002, ChannelID: "ch_design", SenderUID: "uid_eve", Content: "Looks great, shipping it", SendTime: "2025-01-15T09:05:00Z", SourceName: "设计群"},
	}

	cited := []pipeline.Message{allMessages[1], allMessages[4]}

	nameMap := map[string]string{
		"uid_alice":   "Alice",
		"uid_bob":     "Bob",
		"uid_charlie": "Charlie",
		"uid_dave":    "Dave",
		"uid_eve":     "Eve",
	}

	text := "Bob raised a question about edge cases [1], and Dave updated the design mockups [2]."
	citations := buildCitations(text, cited, allMessages, nameMap)

	if len(citations) != 2 {
		t.Fatalf("expected 2 citations, got %d", len(citations))
	}

	c1 := citations[0]
	if c1.Index != 1 {
		t.Errorf("c1.Index = %d, want 1", c1.Index)
	}
	if c1.ChannelID != "ch_proj_dev" {
		t.Errorf("c1.ChannelID = %q, want %q", c1.ChannelID, "ch_proj_dev")
	}
	if len(c1.ContextBefore) == 0 {
		t.Fatal("c1.ContextBefore is empty, expected at least 1 message")
	}
	if len(c1.ContextAfter) == 0 {
		t.Fatal("c1.ContextAfter is empty, expected at least 1 message")
	}
	if c1.ContextBefore[0].Content != "I pushed the fix for the login bug" {
		t.Errorf("c1.ContextBefore[0].Content = %q", c1.ContextBefore[0].Content)
	}
	if c1.ContextAfter[0].Content != "Yes, added tests for that too" {
		t.Errorf("c1.ContextAfter[0].Content = %q", c1.ContextAfter[0].Content)
	}

	c2 := citations[1]
	if c2.Index != 2 {
		t.Errorf("c2.Index = %d, want 2", c2.Index)
	}
	if c2.ChannelID != "ch_design" {
		t.Errorf("c2.ChannelID = %q, want %q", c2.ChannelID, "ch_design")
	}
	if len(c2.ContextBefore) != 0 {
		t.Errorf("c2.ContextBefore should be empty (first in channel), got %d", len(c2.ContextBefore))
	}
	if len(c2.ContextAfter) == 0 {
		t.Fatal("c2.ContextAfter is empty, expected at least 1 message")
	}
	if c2.ContextAfter[0].Content != "Looks great, shipping it" {
		t.Errorf("c2.ContextAfter[0].Content = %q", c2.ContextAfter[0].Content)
	}
}

func TestFindContext_WithNameMap(t *testing.T) {
	allMessages := []pipeline.Message{
		{MessageSeq: 5001, ChannelID: "ch_team", SenderUID: "uid_alice", Content: "standup starting", SendTime: "2025-02-01T09:00:00Z"},
		{MessageSeq: 5002, ChannelID: "ch_team", SenderUID: "uid_bob", Content: "working on API refactor", SendTime: "2025-02-01T09:01:00Z"},
		{MessageSeq: 5003, ChannelID: "ch_team", SenderUID: "uid_charlie", Content: "reviewing PRs today", SendTime: "2025-02-01T09:02:00Z"},
		{MessageSeq: 5004, ChannelID: "ch_team", SenderUID: "uid_dave", Content: "fixing CI pipeline", SendTime: "2025-02-01T09:03:00Z"},
		{MessageSeq: 5005, ChannelID: "ch_team", SenderUID: "uid_eve", Content: "on PTO tomorrow", SendTime: "2025-02-01T09:04:00Z"},
	}

	nameMap := map[string]string{
		"uid_alice":   "Alice",
		"uid_bob":     "Bob",
		"uid_charlie": "Charlie",
		"uid_dave":    "Dave",
		// uid_eve intentionally missing
	}

	target := allMessages[2]
	channelMap := buildChannelMessageMap(allMessages)
	before, after := findContext(target, channelMap, nameMap, 2)

	if len(before) != 2 {
		t.Fatalf("expected 2 before, got %d", len(before))
	}
	if before[0].Sender != "Alice" {
		t.Errorf("before[0].Sender = %q, want %q", before[0].Sender, "Alice")
	}
	if before[1].Sender != "Bob" {
		t.Errorf("before[1].Sender = %q, want %q", before[1].Sender, "Bob")
	}

	if len(after) != 2 {
		t.Fatalf("expected 2 after, got %d", len(after))
	}
	if after[0].Sender != "Dave" {
		t.Errorf("after[0].Sender = %q, want %q", after[0].Sender, "Dave")
	}
	if after[1].Sender != "uid_eve" {
		t.Errorf("after[1].Sender = %q, want %q (missing from nameMap, should use UID)", after[1].Sender, "uid_eve")
	}
}

func TestDedupCitations_NoDuplicates(t *testing.T) {
	text := "讨论 [1] 和 [2] 的内容"
	citations := []model.Citation{
		{Index: 1, Sender: "Alice", Content: "Hello"},
		{Index: 2, Sender: "Bob", Content: "World"},
	}
	newText, newCitations := dedupCitations(text, citations)
	if newText != text {
		t.Errorf("text changed: got %q, want %q", newText, text)
	}
	if len(newCitations) != 2 {
		t.Fatalf("expected 2 citations, got %d", len(newCitations))
	}
}

func TestDedupCitations_DuplicateContent(t *testing.T) {
	text := "重复消息 [1][2][3] 都说了同样的话"
	citations := []model.Citation{
		{Index: 1, Sender: "Alice", Content: "same content"},
		{Index: 2, Sender: "Alice", Content: "same content"},
		{Index: 3, Sender: "Alice", Content: "same content"},
	}
	newText, newCitations := dedupCitations(text, citations)

	if newText != "重复消息 [1] 都说了同样的话" {
		t.Errorf("unexpected text: %q", newText)
	}
	if len(newCitations) != 1 {
		t.Fatalf("expected 1 citation, got %d", len(newCitations))
	}
	if newCitations[0].Index != 1 {
		t.Errorf("expected index 1, got %d", newCitations[0].Index)
	}
}

func TestDedupCitations_ConsecutiveDuplicateMarkers(t *testing.T) {
	// Simulates the case where after replacement, consecutive identical markers appear.
	text := "消息 [1][1][1] 很重要"
	citations := []model.Citation{
		{Index: 1, Sender: "Alice", Content: "Hello"},
	}
	// Even though there's only 1 citation (no dedup needed on citations),
	// we won't enter the remap path. So test with actual duplicates that produce [1][1][1].
	text2 := "消息 [1][2][3] 很重要"
	citations2 := []model.Citation{
		{Index: 1, Sender: "Alice", Content: "Hello"},
		{Index: 2, Sender: "Alice", Content: "Hello"},
		{Index: 3, Sender: "Alice", Content: "Hello"},
	}
	newText, newCitations := dedupCitations(text2, citations2)
	if newText != "消息 [1] 很重要" {
		t.Errorf("consecutive markers not collapsed: got %q", newText)
	}
	if len(newCitations) != 1 {
		t.Fatalf("expected 1 citation, got %d", len(newCitations))
	}

	// Also verify global dedup collapses repeated markers even when no remap needed.
	newText, _ = dedupCitations(text, citations)
	if newText != "消息 [1] 很重要" {
		t.Errorf("expected global dedup to collapse [1][1][1]: got %q", newText)
	}
}

func TestDedupCitations_MixedDuplicatesAndUnique(t *testing.T) {
	text := "[1] unique, [2] dup, [3] dup, [4] another unique"
	citations := []model.Citation{
		{Index: 1, Sender: "Alice", Content: "unique msg"},
		{Index: 2, Sender: "Bob", Content: "repeated"},
		{Index: 3, Sender: "Bob", Content: "repeated"},
		{Index: 4, Sender: "Charlie", Content: "also unique"},
	}
	newText, newCitations := dedupCitations(text, citations)
	if newText != "[1] unique, [2] dup, dup, [4] another unique" {
		t.Errorf("unexpected text: %q", newText)
	}
	if len(newCitations) != 3 {
		t.Fatalf("expected 3 citations, got %d", len(newCitations))
	}
	// Should be indexes 1, 2, 4
	wantIndexes := []int{1, 2, 4}
	for i, c := range newCitations {
		if c.Index != wantIndexes[i] {
			t.Errorf("citation[%d].Index = %d, want %d", i, c.Index, wantIndexes[i])
		}
	}
}

func TestDedupCitations_Empty(t *testing.T) {
	text, citations := dedupCitations("no citations", []model.Citation{})
	if text != "no citations" {
		t.Errorf("text changed: %q", text)
	}
	if len(citations) != 0 {
		t.Errorf("expected 0 citations, got %d", len(citations))
	}
}

// --- P1: Global dedup tests ---

func TestDedupCitations_GlobalDuplicate_SameMarkerDifferentParagraphs(t *testing.T) {
	text := "第一段提到 [1] 很重要。\n\n第二段又引用了 [1] 作为佐证。"
	citations := []model.Citation{
		{Index: 1, Sender: "Alice", Content: "Hello"},
	}
	newText, newCitations := dedupCitations(text, citations)

	count := strings.Count(newText, "[1]")
	if count != 1 {
		t.Errorf("expected [1] to appear once, got %d times in: %q", count, newText)
	}
	if len(newCitations) != 1 {
		t.Errorf("expected 1 citation, got %d", len(newCitations))
	}
}

func TestDedupCitations_GlobalDuplicate_MultipleMarkers(t *testing.T) {
	text := "讨论 [1] 和 [2] 的内容。后面再次提到 [1] 和 [2]。"
	citations := []model.Citation{
		{Index: 1, Sender: "Alice", Content: "msg1"},
		{Index: 2, Sender: "Bob", Content: "msg2"},
	}
	newText, _ := dedupCitations(text, citations)

	if strings.Count(newText, "[1]") != 1 {
		t.Errorf("[1] count != 1 in: %q", newText)
	}
	if strings.Count(newText, "[2]") != 1 {
		t.Errorf("[2] count != 1 in: %q", newText)
	}
}

func TestDedupCitations_GlobalDuplicate_EmptyLineCleanup(t *testing.T) {
	text := "第一段 [1] 很重要。\n[1]\n第三段继续。"
	citations := []model.Citation{
		{Index: 1, Sender: "Alice", Content: "Hello"},
	}
	newText, _ := dedupCitations(text, citations)

	if strings.Contains(newText, "\n\n") {
		t.Errorf("empty line not cleaned: %q", newText)
	}
	if strings.Count(newText, "[1]") != 1 {
		t.Errorf("[1] should appear once in: %q", newText)
	}
}

func TestDedupCitations_GlobalDuplicate_NoDuplicates(t *testing.T) {
	text := "引用 [1] 和 [2] 各出现一次"
	citations := []model.Citation{
		{Index: 1, Sender: "Alice", Content: "msg1"},
		{Index: 2, Sender: "Bob", Content: "msg2"},
	}
	newText, _ := dedupCitations(text, citations)
	if newText != text {
		t.Errorf("text should not change, got: %q", newText)
	}
}

// --- P2: Context and truncation tests ---

func TestFindContext_SameChannel(t *testing.T) {
	allMessages := []pipeline.Message{
		{MessageSeq: 1, ChannelID: "ch1", SenderUID: "u1", Content: "msg1", SendTime: "10:00"},
		{MessageSeq: 2, ChannelID: "ch1", SenderUID: "u2", Content: "msg2", SendTime: "10:01"},
		{MessageSeq: 3, ChannelID: "ch1", SenderUID: "u1", Content: "msg3", SendTime: "10:02"},
		{MessageSeq: 4, ChannelID: "ch1", SenderUID: "u3", Content: "msg4", SendTime: "10:03"},
		{MessageSeq: 5, ChannelID: "ch1", SenderUID: "u2", Content: "msg5", SendTime: "10:04"},
	}
	target := allMessages[2]
	channelMap := buildChannelMessageMap(allMessages)
	before, after := findContext(target, channelMap, nil, 2)

	if len(before) != 2 {
		t.Fatalf("expected 2 before, got %d", len(before))
	}
	if before[0].Content != "msg1" || before[1].Content != "msg2" {
		t.Errorf("unexpected before: %+v", before)
	}
	if len(after) != 2 {
		t.Fatalf("expected 2 after, got %d", len(after))
	}
	if after[0].Content != "msg4" || after[1].Content != "msg5" {
		t.Errorf("unexpected after: %+v", after)
	}
}

func TestFindContext_CrossChannelIsolation(t *testing.T) {
	allMessages := []pipeline.Message{
		{MessageSeq: 1, ChannelID: "ch1", SenderUID: "u1", Content: "ch1-msg1", SendTime: "10:00"},
		{MessageSeq: 2, ChannelID: "ch2", SenderUID: "u2", Content: "ch2-msg1", SendTime: "10:01"},
		{MessageSeq: 3, ChannelID: "ch1", SenderUID: "u1", Content: "ch1-msg2", SendTime: "10:02"},
		{MessageSeq: 4, ChannelID: "ch2", SenderUID: "u3", Content: "ch2-msg2", SendTime: "10:03"},
		{MessageSeq: 5, ChannelID: "ch1", SenderUID: "u2", Content: "ch1-msg3", SendTime: "10:04"},
	}
	target := allMessages[2]
	channelMap := buildChannelMessageMap(allMessages)
	before, after := findContext(target, channelMap, nil, 2)

	if len(before) != 1 {
		t.Fatalf("expected 1 before (same channel), got %d", len(before))
	}
	if before[0].Content != "ch1-msg1" {
		t.Errorf("before[0] should be ch1-msg1, got %q", before[0].Content)
	}
	if len(after) != 1 {
		t.Fatalf("expected 1 after (same channel), got %d", len(after))
	}
	if after[0].Content != "ch1-msg3" {
		t.Errorf("after[0] should be ch1-msg3, got %q", after[0].Content)
	}
}

func TestFindContext_BoundaryPositions(t *testing.T) {
	allMessages := []pipeline.Message{
		{MessageSeq: 1, ChannelID: "ch1", SenderUID: "u1", Content: "first", SendTime: "10:00"},
		{MessageSeq: 2, ChannelID: "ch1", SenderUID: "u2", Content: "second", SendTime: "10:01"},
	}
	channelMap := buildChannelMessageMap(allMessages)

	before, after := findContext(allMessages[0], channelMap, nil, 2)
	if len(before) != 0 {
		t.Errorf("expected 0 before for first msg, got %d", len(before))
	}
	if len(after) != 1 {
		t.Errorf("expected 1 after for first msg, got %d", len(after))
	}

	before, after = findContext(allMessages[1], channelMap, nil, 2)
	if len(before) != 1 {
		t.Errorf("expected 1 before for last msg, got %d", len(before))
	}
	if len(after) != 0 {
		t.Errorf("expected 0 after for last msg, got %d", len(after))
	}
}

func TestTruncateRunes(t *testing.T) {
	cn := strings.Repeat("中", 250)
	result := truncateRunes(cn, 200)
	runes := []rune(result)
	if len(runes) != 203 {
		t.Errorf("expected 203 runes, got %d", len(runes))
	}

	short := "短文本"
	if truncateRunes(short, 200) != short {
		t.Errorf("short text should not be truncated")
	}
}
