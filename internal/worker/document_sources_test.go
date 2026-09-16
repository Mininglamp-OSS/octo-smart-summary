//go:build cgo

package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

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

	messages, err := loadDocumentEvidence(db, []model.SummarySource{source})
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

	messages, err := loadDocumentEvidence(db, []model.SummarySource{source})
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
	if _, err := loadDocumentEvidence(db, []model.SummarySource{source}); err == nil {
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

func TestSplitDocumentEvidenceKeepsAllText(t *testing.T) {
	content := "第一段\n第二段\n第三段"
	parts := splitDocumentEvidence(content, 6)
	if len(parts) < 2 {
		t.Fatalf("parts = %#v, want multiple chunks", parts)
	}
	if got := strings.Join(parts, ""); strings.ReplaceAll(got, "\n", "") != strings.ReplaceAll(content, "\n", "") {
		t.Fatalf("joined chunks = %q, want all content from %q", got, content)
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
