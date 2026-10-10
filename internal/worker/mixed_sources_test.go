package worker

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

// PR2: mixed-source worker orchestration tests (plan §4.4).

func mixedTestSources() []model.SummarySource {
	return []model.SummarySource{
		{SourceType: model.SourceGroup, SourceID: "group-1", SourceName: "项目群"},
		{SourceType: model.SourceDocument, SourceID: "doc-1", SourceName: "产品方案", SourceVersion: "v3"},
	}
}

func TestSplitMixedSourcesPartitionsByClass(t *testing.T) {
	chat, docs := splitMixedSources(mixedTestSources())
	if len(chat) != 1 || chat[0].SourceID != "group-1" {
		t.Fatalf("chat side = %#v", chat)
	}
	if len(docs) != 1 || docs[0].SourceID != "doc-1" {
		t.Fatalf("document side = %#v", docs)
	}
}

// Derived rows are worker-internal provenance and must not enter either side
// (plan §4.4 来源分流: Derived 来源不变成用户显式选择).
func TestSplitMixedSourcesExcludesDerived(t *testing.T) {
	sources := append(mixedTestSources(), model.SummarySource{
		SourceType: model.SourceGroup, SourceID: "group-derived", Derived: true,
	})
	chat, docs := splitMixedSources(sources)
	if len(chat) != 1 || len(docs) != 1 {
		t.Fatalf("derived row leaked: chat=%d docs=%d", len(chat), len(docs))
	}
}

func TestMixedHasDocumentSource(t *testing.T) {
	if mixedHasDocumentSource(mixedTestSources()) != true {
		t.Fatal("mixed sources not detected")
	}
	if mixedHasDocumentSource([]model.SummarySource{{SourceType: model.SourceGroup, SourceID: "g"}}) {
		t.Fatal("pure chat detected as mixed")
	}
	if mixedHasDocumentSource([]model.SummarySource{{SourceType: model.SourceDocument, SourceID: "d"}}) {
		t.Fatal("pure document detected as mixed")
	}
	// A derived document row does NOT make a chat task mixed.
	if mixedHasDocumentSource([]model.SummarySource{
		{SourceType: model.SourceGroup, SourceID: "g"},
		{SourceType: model.SourceDocument, SourceID: "d", Derived: true},
	}) {
		t.Fatal("derived document row treated as user-selected document")
	}
}

func TestAppendDocumentEvidenceContinuesCitationIndexes(t *testing.T) {
	chatMessages := []pipeline.Message{
		{CitationIndex: 1, ChannelID: "group-1", Content: "消息一"},
		{CitationIndex: 2, ChannelID: "group-1", Content: "消息二"},
	}
	documentMessages := []pipeline.Message{
		{CitationIndex: 1, ChannelID: "doc-1", ChannelType: model.SourceDocument, Content: "文档片段一", SourceVersion: "v3"},
		{CitationIndex: 2, ChannelID: "doc-1", ChannelType: model.SourceDocument, Content: "文档片段二", SourceVersion: "v3"},
	}
	loader := func([]model.SummarySource) ([]pipeline.Message, error) {
		return documentMessages, nil
	}
	merged, err := appendDocumentEvidence(chatMessages, mixedTestSources()[1:], loader, 3)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if len(merged) != 4 {
		t.Fatalf("merged = %d messages, want 4", len(merged))
	}
	// Unified numbering pool: chat keeps [1,2], document continues [3,4]
	// (plan §4.4 证据编号: 不能各自从 1 开始).
	if merged[2].CitationIndex != 3 || merged[3].CitationIndex != 4 {
		t.Fatalf("document citation indexes = [%d, %d], want [3, 4]",
			merged[2].CitationIndex, merged[3].CitationIndex)
	}
	if merged[0].CitationIndex != 1 || merged[1].CitationIndex != 2 {
		t.Fatal("chat citation indexes must be untouched")
	}
}

func TestAppendDocumentEvidenceLoaderErrorIsHard(t *testing.T) {
	loader := func([]model.SummarySource) ([]pipeline.Message, error) {
		return nil, errTestSnapshotMissing
	}
	if _, err := appendDocumentEvidence(nil, mixedTestSources()[1:], loader, 1); err == nil {
		t.Fatal("document evidence load failure must propagate as a hard error")
	}
}

func TestAppendDocumentEvidenceNoDocumentsNoop(t *testing.T) {
	chatMessages := []pipeline.Message{{CitationIndex: 1}}
	merged, err := appendDocumentEvidence(chatMessages, nil, nil, 2)
	if err != nil {
		t.Fatalf("noop append: %v", err)
	}
	if len(merged) != 1 {
		t.Fatalf("noop append changed messages: %d", len(merged))
	}
}

// Chat formatting and document formatting must stay distinct in mixed mode:
// the chat cleaner's time/bot handling never applies to document rows.
func TestFormatMixedEvidenceDispatchesByChannelType(t *testing.T) {
	chatMsg := pipeline.Message{
		CitationIndex: 1, ChannelID: "group-1", ChannelType: model.SourceGroup,
		SendTime: "2026-09-22 10:00", SenderName: "张三", Content: "hello [2] world",
	}
	docMsg := pipeline.Message{
		CitationIndex: 3, ChannelID: "doc-1", ChannelType: model.SourceDocument,
		Content: "正文 [2] 内容", SourceName: "产品方案", SourceVersion: "v3", MessageSeq: 1,
	}
	chatFormatted := formatMixedEvidence(chatMsg)
	docFormatted := formatMixedEvidence(docMsg)
	if !contains(chatFormatted, "[1][2026-09-22 10:00] 张三:") {
		t.Fatalf("chat format wrong: %q", chatFormatted)
	}
	if !contains(docFormatted, "【文档：产品方案") || !contains(docFormatted, "片段：1") {
		t.Fatalf("document format wrong: %q", docFormatted)
	}
}

func TestMixedSourceLabel(t *testing.T) {
	chat := []model.SummarySource{{SourceType: model.SourceGroup, SourceID: "g"}}
	docs := []model.SummarySource{{SourceType: model.SourceDocument, SourceID: "d"}}
	label := mixedSourceLabel(chat, docs)
	if !contains(label, "会话 1 个") || !contains(label, "文档 1 篇") {
		t.Fatalf("label = %q", label)
	}
	if mixedSourceLabel(chat, nil) == mixedSourceLabel(nil, docs) {
		t.Fatal("single-class labels must differ")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

var errTestSnapshotMissing = &testError{"snapshot missing"}

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }
