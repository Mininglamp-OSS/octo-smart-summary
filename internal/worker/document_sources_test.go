//go:build cgo

package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

type runeCountTokenizer struct{}

func (runeCountTokenizer) Count(text string) int    { return utf8.RuneCountInString(text) }
func (runeCountTokenizer) Estimate(text string) int { return utf8.RuneCountInString(text) }
func (runeCountTokenizer) IsExact() bool            { return true }
func (runeCountTokenizer) ModelName() string        { return "test" }

func TestLoadDocumentEvidenceUsesPersistedSnapshot(t *testing.T) {
	db := setupProcessorTestDB(t)
	if err := db.AutoMigrate(&model.SummarySourceSnapshot{}); err != nil {
		t.Fatal(err)
	}
	content := "第一段\n\n第二段"
	hash := sha256.Sum256([]byte(content))
	hashText := hex.EncodeToString(hash[:])
	source := model.SummarySource{TaskID: 1, SourceType: model.SourceDocument, SourceID: "d_1", SourceName: "设计文档", SourceVersion: "v5", SourceHash: hashText}
	if err := db.Create(&source).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.SummarySourceSnapshot{SummarySourceID: source.ID, Content: content, ContentBytes: len([]byte(content)), ContentHash: hashText}).Error; err != nil {
		t.Fatal(err)
	}

	messages, err := loadDocumentEvidence(db, []model.SummarySource{source}, runeCountTokenizer{}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Content != content || messages[0].ChannelID != "d_1" || messages[0].SourceVersion != "v5" {
		t.Fatalf("messages = %#v", messages)
	}
}

func TestLoadDocumentEvidenceSurfacesSnapshotTruncation(t *testing.T) {
	db := setupProcessorTestDB(t)
	if err := db.AutoMigrate(&model.SummarySourceSnapshot{}); err != nil {
		t.Fatal(err)
	}
	content := "retained body"
	hash := sha256.Sum256([]byte(content))
	hashText := hex.EncodeToString(hash[:])
	source := model.SummarySource{TaskID: 1, SourceType: model.SourceDocument, SourceID: "d_1", SourceHash: hashText}
	if err := db.Create(&source).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.SummarySourceSnapshot{
		SummarySourceID: source.ID,
		Content:         content,
		ContentBytes:    len(content),
		ContentHash:     hashText,
		Truncated:       true,
	}).Error; err != nil {
		t.Fatal(err)
	}

	messages, err := loadDocumentEvidence(db, []model.SummarySource{source}, runeCountTokenizer{}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Content != content+documentSnapshotTruncatedMarker {
		t.Fatalf("messages = %#v, want retained content plus truncation marker", messages)
	}
}

func TestLoadDocumentEvidenceRejectsChangedSnapshot(t *testing.T) {
	db := setupProcessorTestDB(t)
	if err := db.AutoMigrate(&model.SummarySourceSnapshot{}); err != nil {
		t.Fatal(err)
	}
	source := model.SummarySource{TaskID: 1, SourceType: model.SourceDocument, SourceID: "d_1", SourceHash: strings.Repeat("0", 64)}
	if err := db.Create(&source).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.SummarySourceSnapshot{SummarySourceID: source.ID, Content: "changed", ContentBytes: 7, ContentHash: strings.Repeat("0", 64)}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := loadDocumentEvidence(db, []model.SummarySource{source}, runeCountTokenizer{}, 1000); err == nil {
		t.Fatal("loadDocumentEvidence accepted a snapshot whose content hash changed")
	}
}

func TestExecutePipelineDocumentTaskDoesNotRequireChatBackend(t *testing.T) {
	db := setupProcessorTestDB(t)
	if err := db.AutoMigrate(&model.SummarySourceSnapshot{}); err != nil {
		t.Fatal(err)
	}
	task := model.SummaryTask{TaskNo: "DOC-1", CreatorID: "u1"}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	content := "只来自创建时保存的正文"
	hash := sha256.Sum256([]byte(content))
	hashText := hex.EncodeToString(hash[:])
	source := model.SummarySource{TaskID: task.ID, SourceType: model.SourceDocument, SourceID: "d_1", SourceName: "文档", SourceHash: hashText}
	if err := db.Create(&source).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.SummarySourceSnapshot{SummarySourceID: source.ID, Content: content, ContentBytes: len([]byte(content)), ContentHash: hashText}).Error; err != nil {
		t.Fatal(err)
	}
	processor := &Processor{db: db}
	if err := processor.executePipeline(task); err != nil {
		t.Fatalf("executePipeline() required chat/Docs network dependencies: %v", err)
	}
}

func TestExecutePersonalPipelineUsesDocumentSnapshotsAndKeepsCoordinates(t *testing.T) {
	db := setupProcessorTestDB(t)
	if err := db.AutoMigrate(&model.SummarySourceSnapshot{}); err != nil {
		t.Fatal(err)
	}
	task := model.SummaryTask{TaskNo: "DOC-INTEGRATION", CreatorID: "u1", Title: "文档总结"}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	for i, docID := range []string{"docA", "docB"} {
		content := "共同前缀：" + strings.Repeat("内容", 120) + fmt.Sprintf(" 文档%d", i+1)
		hash := sha256.Sum256([]byte(content))
		hashText := hex.EncodeToString(hash[:])
		source := model.SummarySource{
			TaskID: task.ID, SourceType: model.SourceDocument, SourceID: docID,
			SourceName: "同名文档", SourceVersion: fmt.Sprintf("v%d", i+1), SourceHash: hashText,
		}
		if err := db.Create(&source).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&model.SummarySourceSnapshot{
			SummarySourceID: source.ID, Content: content, ContentBytes: len([]byte(content)), ContentHash: hashText,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"结论一 [1]，结论二 [2]"}}],"usage":{"total_tokens":12}}`)
		_, _ = fmt.Fprintln(w, "data: [DONE]")
	}))
	defer server.Close()

	p := &Processor{
		db:  db,
		llm: service.NewLLMClient(server.URL, "test-key", "test-model", 5, 1000, false, 1, nil),
		cfg: &config.Config{
			LLMModel: "test-model", MapMaxTokens: 10000, WorkerMapConcurrency: 1,
			CharsPerTokenCJK: 1, CharsPerTokenASCII: 4,
		},
	}
	result, citations, msgCount, _, _, err := p.executePersonalPipeline(context.Background(), task, "u1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result != "结论一 [1]，结论二 [2]" || msgCount != 2 || len(citations) != 2 {
		t.Fatalf("result=%q msgCount=%d citations=%#v", result, msgCount, citations)
	}
	if citations[0].DocumentID != "docA" || citations[1].DocumentID != "docB" {
		t.Fatalf("document coordinates=%#v", citations)
	}
}

func TestExecutePersonalPipelineRejectsMixedDocumentAndChatSources(t *testing.T) {
	db := setupProcessorTestDB(t)
	task := model.SummaryTask{TaskNo: "DOC-MIXED", CreatorID: "u1"}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	for _, source := range []model.SummarySource{
		{TaskID: task.ID, SourceType: model.SourceDocument, SourceID: "docA"},
		{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "groupA"},
	} {
		if err := db.Create(&source).Error; err != nil {
			t.Fatal(err)
		}
	}
	p := &Processor{db: db, cfg: &config.Config{}}
	if _, _, _, _, _, err := p.executePersonalPipeline(context.Background(), task, "u1", nil, nil); err == nil || !strings.Contains(err.Error(), "cannot be mixed") {
		t.Fatalf("mixed document/chat error=%v", err)
	}
}

func TestDocumentSourcesOnlyRejectsEmpty(t *testing.T) {
	if documentSourcesOnly(nil) {
		t.Fatal("empty sources must not enter document mode")
	}
}

func TestSplitDocumentEvidenceKeepsAllText(t *testing.T) {
	content := "第一段\n第二段\n第三段"
	parts := splitDocumentEvidence(content, runeCountTokenizer{}, 6)
	if len(parts) < 2 {
		t.Fatalf("parts = %#v, want multiple chunks", parts)
	}
	if got := strings.Join(parts, ""); strings.ReplaceAll(got, "\n", "") != strings.ReplaceAll(content, "\n", "") {
		t.Fatalf("joined chunks = %q, want all content from %q", got, content)
	}
}

func TestSplitDocumentEvidenceUsesTokenBudget(t *testing.T) {
	content := strings.Repeat("文", 21)
	parts := splitDocumentEvidence(content, runeCountTokenizer{}, 7)
	if len(parts) != 3 {
		t.Fatalf("parts=%d, want 3", len(parts))
	}
	for i, part := range parts {
		if got := utf8.RuneCountInString(part); got > 7 {
			t.Fatalf("part[%d] runes=%d, exceeds token budget", i, got)
		}
	}
}

func TestFormatDocumentEvidenceEscapesMetadataCitationMarkers(t *testing.T) {
	got := formatDocumentEvidence(pipeline.Message{
		CitationIndex: 5,
		SourceName:    "Roadmap [2]",
		SourceVersion: "v9 [7]",
		MessageSeq:    1,
		Content:       "正文 [3]",
	})
	for _, marker := range []string{"[2]", "[7]", "[3]"} {
		if strings.Contains(got, marker) {
			t.Fatalf("formatted evidence contains raw marker %s: %q", marker, got)
		}
	}
	if !strings.HasPrefix(got, "[5]") {
		t.Fatalf("formatted evidence lost canonical marker: %q", got)
	}
}

func TestDocumentCitationWireContract(t *testing.T) {
	if model.SourceDocument != 4 {
		t.Fatalf("SourceDocument=%d, want wire value 4", model.SourceDocument)
	}
	data, err := json.Marshal(model.Citation{DocumentID: "d_1", DocumentVersion: "v2", DocumentChunk: 3})
	if err != nil {
		t.Fatal(err)
	}
	jsonText := string(data)
	for _, field := range []string{`"document_id":"d_1"`, `"document_version":"v2"`, `"document_chunk":3`} {
		if !strings.Contains(jsonText, field) {
			t.Fatalf("citation JSON %s missing %s", jsonText, field)
		}
	}
}

func TestBuildCitationsIncludesDocumentCoordinates(t *testing.T) {
	messages := []pipeline.Message{{
		CitationIndex: 1, ChannelID: "d_1", ChannelType: model.SourceDocument,
		MessageSeq: 2, SourceName: "设计文档", SourceVersion: "v5", Content: "关键结论",
	}}
	citations := buildCitations("结论 [1]", messages, messages, nil)
	if len(citations) != 1 || citations[0].DocumentID != "d_1" || citations[0].DocumentVersion != "v5" || citations[0].DocumentChunk != 2 {
		t.Fatalf("citations = %#v", citations)
	}
}
