//go:build cgo

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// M1 pin (Jerry-Xin P2-9, adopting yujiawei's round-1 follow-up root-cause
// trace, byte-verified): on the personal-worker path the setext shape is
// MANUFACTURED IN-PIPELINE — dedupCitations (citation.go emptyLineRe) deletes
// every blank line, converting the model's correct "：\n\n---\n\n" into
// "：\n---\n"; the NormalizeSetextHeadings call at personal_processor.go:427
// then re-inserts exactly the one blank line before the rule.
//
// The probe drives the REAL worker entry (processPersonalSummaryWithOptions →
// executePersonalPipeline → persistCompletedPersonalResult), the byte-verified
// choke point Jerry-Xin's M1 mutation attacked (removing the wiring kept the
// shipped suite green). It deliberately does NOT assert paragraph preservation
// ("内容甲。\n\n内容乙。") — blanket blank-line deletion in dedupCitations
// still merges model-separated paragraphs, and narrowing emptyLineRe is
// explicitly deferred out of this PR (acknowledged in the round-2 response;
// it subsumes the setext case and is the real fix for the paragraph-merging
// half).
func TestPersonalPipelineSetextShapeNeutralizedAfterDedup(t *testing.T) {
	var calls atomic.Int32
	content := "现将进展整理如下[1]：\n\n---\n\n### 一、已完成事项\n\n内容甲[2]。\n\n内容乙。\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		data, _ := json.Marshal(map[string]interface{}{"choices": []interface{}{
			map[string]interface{}{"delta": map[string]string{"content": content}, "finish_reason": "stop"},
		}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
	}))
	defer srv.Close()
	// File-backed DB: the run holds leases via a heartbeat goroutine writing
	// concurrently with the main flow (:memory: + second pool connection =
	// "no such table", see local-cgo-test-toolchain ref).
	dsn := filepath.Join(t.TempDir(), "setext-m1.db") + "?_busy_timeout=10000&_journal_mode=WAL"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&model.SummarySchedule{}, &model.SummaryTask{}, &model.SummaryParticipant{},
		&model.PersonalResult{}, &model.SummarySource{}, &model.SummaryNotification{},
		&model.PersonalResultVersion{}, &model.SummaryEvent{}, &model.SummaryResult{},
	); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo: "T-SEXT-M1", SpaceID: "sp", CreatorID: "creator",
		SummaryMode: model.ModeByPerson, Status: model.StatusPending,
		TimeRangeStart: now.Add(-time.Hour), TimeRangeEnd: now,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	part := model.SummaryParticipant{TaskID: task.ID, UserID: "creator", Status: model.ParticipantPending}
	if err := db.Create(&part).Error; err != nil {
		t.Fatalf("create participant: %v", err)
	}
	pr := model.PersonalResult{TaskID: task.ID, ParticipantRefID: part.ID, UserID: "creator", WorkerStatus: model.PersonalStatusPending}
	if err := db.Create(&pr).Error; err != nil {
		t.Fatalf("create personal result: %v", err)
	}
	p := &Processor{
		db: db, cfg: &config.Config{LLMModel: "test", CharsPerTokenASCII: 4, MapMaxTokens: 10000, WorkerLeaseMinutes: 1, WorkerMaxRetry: 3},
		llm: service.NewLLMClient(srv.URL, "test", "test", 5, 1024, false, 5, nil),
		fetchPersonalMessagesFn: func(context.Context, model.SummaryTask, string) ([]pipeline.Message, *pipeline.IntentResult, error) {
			return []pipeline.Message{
				{SenderUID: "creator", Content: "进展说明", ChannelID: "group", MessageSeq: 1},
				{SenderUID: "creator", Content: "内容甲证据", ChannelID: "group", MessageSeq: 2},
			}, &pipeline.IntentResult{Skipped: true, SkipReason: "pure_generic_topic"}, nil
		},
	}
	p.processPersonalSummary(context.Background(), task.ID, part.ID)
	if calls.Load() != 1 {
		t.Fatalf("expected one deterministic model call, got %d", calls.Load())
	}
	var got model.PersonalResult
	if err := db.First(&got, pr.ID).Error; err != nil {
		t.Fatalf("load persisted personal result: %v", err)
	}
	// Fixture guard (Fatal): a completed run proves the real entry executed.
	if got.WorkerStatus != model.PersonalStatusCompleted {
		t.Fatalf("fixture broken: run did not complete, worker_status=%d content=%q", got.WorkerStatus, got.Content)
	}
	if !strings.Contains(got.Content, "如下[1]：\n\n---\n") {
		t.Fatalf("M1: manufactured setext shape survived the personal-worker entry: %q", got.Content)
	}
}
