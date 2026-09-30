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
	"sync/atomic"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"gorm.io/gorm"
)

// Integration fixtures driving a SUCCESSFUL mixed run through the real
// executePersonalPipeline path (real snapshot rows + real chat fetch seam +
// stub LLM), so the citation numbering, empty-chat fallback and partial-Map
// contracts are exercised where they live — not bypassed by stub loaders.

func seedMixedTask(t *testing.T, db *gorm.DB, taskNo string, docContent string) model.SummaryTask {
	t.Helper()
	task := model.SummaryTask{TaskNo: taskNo, CreatorID: "u1", Title: "混合总结"}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.SummarySource{
		TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "groupA", SourceName: "项目群",
	}).Error; err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(docContent))
	hashText := hex.EncodeToString(hash[:])
	doc := model.SummarySource{
		TaskID: task.ID, SourceType: model.SourceDocument, SourceID: "docA",
		SourceName: "产品方案", SourceVersion: "v3", SourceHash: hashText,
	}
	if err := db.Create(&doc).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.SummarySourceSnapshot{
		SummarySourceID: doc.ID, Content: docContent,
		ContentBytes: len([]byte(docContent)), ContentHash: hashText,
	}).Error; err != nil {
		t.Fatal(err)
	}
	return task
}

func mixedStubLLM(t *testing.T, content string, calls *atomic.Int32) *service.LLMClient {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		data, _ := json.Marshal(map[string]interface{}{"choices": []interface{}{
			map[string]interface{}{"delta": map[string]string{"content": content}, "finish_reason": "stop"},
		}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
	}))
	t.Cleanup(srv.Close)
	return service.NewLLMClient(srv.URL, "test-key", "test-model", 5, 1000, false, 1, nil)
}

func mixedCapturingStubLLM(t *testing.T, content string, calls *atomic.Int32) (*service.LLMClient, *atomic.Value) {
	t.Helper()
	captured := &atomic.Value{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request struct {
			Messages []service.ChatMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode LLM request: %v", err)
		}
		parts := make([]string, 0, len(request.Messages))
		for _, message := range request.Messages {
			parts = append(parts, message.Content)
		}
		captured.Store(strings.Join(parts, "\n"))
		w.Header().Set("Content-Type", "text/event-stream")
		data, _ := json.Marshal(map[string]interface{}{"choices": []interface{}{
			map[string]interface{}{"delta": map[string]string{"content": content}, "finish_reason": "stop"},
		}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
	}))
	t.Cleanup(srv.Close)
	return service.NewLLMClient(srv.URL, "test-key", "test-model", 5, 1000, false, 1, nil), captured
}

// A mixed task whose chat side yields ZERO messages in the window must NOT
// return 「没有可总结内容」: the loaded document evidence is summarized and
// the chat gap is stated (plan §3.4 / A08).
func TestExecutePersonalPipelineMixedEmptyChatFallsBackToDocuments(t *testing.T) {
	db := setupProcessorTestDB(t)
	if err := db.AutoMigrate(&model.SummarySourceSnapshot{}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	llm, capturedPrompt := mixedCapturingStubLLM(t, "文档结论 [1]。", &calls)
	p := &Processor{
		db:  db,
		llm: llm,
		cfg: &config.Config{LLMModel: "test-model", MapMaxTokens: 10000, CharsPerTokenCJK: 1, CharsPerTokenASCII: 4},
		fetchPersonalMessagesFn: func(context.Context, model.SummaryTask, string) ([]pipeline.Message, *pipeline.IntentResult, error) {
			// Chat side is empty: the selected window holds no messages.
			return []pipeline.Message{}, &pipeline.IntentResult{Skipped: true, SkipReason: "pure_generic_topic"}, nil
		},
	}
	task := seedMixedTask(t, db, "MIXED-EMPTY-CHAT", "文档正文：项目目标与里程碑。")
	result, citations, _, _, _, err := p.executePersonalPipeline(context.Background(), task, "u1", nil, nil)
	if err != nil {
		t.Fatalf("mixed task with empty chat must complete from documents: %v", err)
	}
	if result == pipeline.NoRelevantContentMessage || result == pipeline.NoSelfMessagesMessage {
		t.Fatalf("empty chat side must not collapse to the no-content placeholder: %q", result)
	}
	if !strings.Contains(result, "[1]") || len(citations) == 0 {
		t.Fatalf("document evidence must be citable, result=%q citations=%v", result, citations)
	}
	if calls.Load() == 0 {
		t.Fatal("the document evidence was never sent to the model")
	}
	prompt, _ := capturedPrompt.Load().(string)
	if !strings.Contains(prompt, "所选会话在该时间范围内无可用消息") {
		t.Fatalf("empty-chat gap was not carried into the model prompt: %q", prompt)
	}
	if strings.Contains(prompt, "时间范围（仅聊天侧）") {
		t.Fatalf("document-only Map chunk must not claim chat evidence: %q", prompt)
	}
}

// A targeted-user empty (creator never spoke) with documents present must
// also degrade to the document summary instead of the self-empty notice.
func TestExecutePersonalPipelineMixedSelfEmptyFallsBackToDocuments(t *testing.T) {
	db := setupProcessorTestDB(t)
	if err := db.AutoMigrate(&model.SummarySourceSnapshot{}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	p := &Processor{
		db:  db,
		llm: mixedStubLLM(t, "文档结论 [1]。", &calls),
		cfg: &config.Config{LLMModel: "test-model", MapMaxTokens: 10000, CharsPerTokenCJK: 1, CharsPerTokenASCII: 4},
		fetchPersonalMessagesFn: func(context.Context, model.SummaryTask, string) ([]pipeline.Message, *pipeline.IntentResult, error) {
			intent := &pipeline.IntentResult{}
			intent.TargetPersons.UIDs = []string{"u1"}
			intent.TargetPersons.HasTarget = true
			return []pipeline.Message{}, intent, nil
		},
	}
	task := seedMixedTask(t, db, "MIXED-SELF-EMPTY", "文档正文：架构决策记录。")
	_, _, _, _, _, err := p.executePersonalPipeline(context.Background(), task, "u1", nil, nil)
	if err != nil {
		t.Fatalf("mixed task must not fail on empty chat: %v", err)
	}
	if calls.Load() == 0 {
		t.Fatal("document evidence never reached the model")
	}
}

// Chat and document citation indexes must be UNIQUE and non-colliding when a
// mixed run completes with real snapshot rows (the document chunks previously
// collapsed onto the last chat message's index).
func TestExecutePersonalPipelineMixedCitationIndexesUnique(t *testing.T) {
	db := setupProcessorTestDB(t)
	if err := db.AutoMigrate(&model.SummarySourceSnapshot{}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	p := &Processor{
		db:  db,
		llm: mixedStubLLM(t, "聊天要点 [1][2]，文档要点 [3]。", &calls),
		cfg: &config.Config{LLMModel: "test-model", MapMaxTokens: 10000, CharsPerTokenCJK: 1, CharsPerTokenASCII: 4},
		fetchPersonalMessagesFn: func(context.Context, model.SummaryTask, string) ([]pipeline.Message, *pipeline.IntentResult, error) {
			return []pipeline.Message{
				{SenderUID: "u1", SenderName: "张三", Content: "聊天消息一", ChannelID: "groupA", MessageSeq: 1},
				{SenderUID: "u2", SenderName: "李四", Content: "聊天消息二", ChannelID: "groupA", MessageSeq: 2},
			}, &pipeline.IntentResult{Skipped: true, SkipReason: "pure_generic_topic"}, nil
		},
	}
	task := seedMixedTask(t, db, "MIXED-CITATION", "文档正文：关键结论。")
	_, citations, _, _, _, err := p.executePersonalPipeline(context.Background(), task, "u1", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[int]int, len(citations))
	for _, c := range citations {
		seen[c.Index]++
		if seen[c.Index] > 1 {
			t.Fatalf("duplicate citation index %d across chat/document classes: %v", c.Index, citations)
		}
	}
	if len(citations) < 2 {
		t.Fatalf("expected citations from BOTH evidence classes, got %v", citations)
	}
	hasDoc := false
	for _, c := range citations {
		if c.DocumentID == "docA" {
			hasDoc = true
			if c.DocumentChunk < 1 {
				t.Fatalf("document citation missing chunk coordinate: %v", c)
			}
		}
	}
	if !hasDoc {
		t.Fatalf("no document-coordinate citation built: %v", citations)
	}
}

// A partial Map failure on a MIXED task must fail the whole task — the
// surviving chunks must not be reduced into a "complete" summary (plan §3.4:
// 某个 Map 分块最终失败 → 混合任务一期按失败处理).
func TestExecutePersonalPipelineMixedPartialMapFailureFailsTask(t *testing.T) {
	db := setupProcessorTestDB(t)
	if err := db.AutoMigrate(&model.SummarySourceSnapshot{}); err != nil {
		t.Fatal(err)
	}
	// Two chunks: first succeeds, second returns the MapFailedMarker with nil
	// error (the non-sentinel failure contract). The mixed filter must turn
	// this into a task failure instead of reducing the surviving chunk.
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"分片一结论"}}],"usage":{"total_tokens":9}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"(分片 1 总结失败)"}}],"usage":{"total_tokens":9}}`))
	}))
	defer srv.Close()

	// Enough evidence to force two chunks: small budget via MapMaxTokens.
	longChat := strings.Repeat("聊天内容填充", 200)
	p := &Processor{
		db:  db,
		llm: service.NewLLMClient(srv.URL, "test-key", "test-model", 5, 1000, false, 1, nil),
		cfg: &config.Config{LLMModel: "test-model", MapMaxTokens: 120, WorkerMapConcurrency: 1, CharsPerTokenCJK: 1, CharsPerTokenASCII: 4},
		fetchPersonalMessagesFn: func(context.Context, model.SummaryTask, string) ([]pipeline.Message, *pipeline.IntentResult, error) {
			return []pipeline.Message{
				{SenderUID: "u1", Content: longChat, ChannelID: "groupA", MessageSeq: 1},
			}, &pipeline.IntentResult{Skipped: true, SkipReason: "pure_generic_topic"}, nil
		},
	}
	task := seedMixedTask(t, db, "MIXED-PARTIAL-FAIL", "文档正文。")
	_, _, _, _, _, err := p.executePersonalPipeline(context.Background(), task, "u1", nil, nil)
	if err == nil {
		t.Fatal("mixed task with one failed Map chunk must FAIL, not reduce the surviving chunk")
	}
	if !strings.Contains(err.Error(), "mixed Map phase failed") {
		t.Fatalf("failure must name the mixed partial-Map contract, got: %v", err)
	}
}
