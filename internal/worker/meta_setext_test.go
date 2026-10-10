//go:build cgo

package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// A-1 (Jerry-Xin round-2 advisory, downgraded from codex Critical; mocha P2
// (1), PR#268): the single-submission team fast path copies
// `submitted[0].Content` verbatim into the team SummaryResult — no
// normalization on this branch. New-run rows are normalized at personal-write
// time, but pre-merge historical rows and client-authored hand-edit content
// flow through raw. The direct call is cheap, idempotent defense-in-depth;
// this probe must FAIL at head d9ecaf9 and pass once meta_processor.go:183
// normalizes before persistence.
func TestProcessMetaSummarySingleSubmissionNeutralizesSetext(t *testing.T) {
	db := newReplaceTestDB(t)
	taskID := seedProcessingTask(t, db)
	prID, _ := seedSubmittedMember(t, db, taskID, "uA")
	if err := db.Model(&model.PersonalResult{}).Where("id = ?", prID).
		Update("content", "现将进展整理如下：\n---\n### 一、已完成事项\n").Error; err != nil {
		t.Fatalf("override personal content: %v", err)
	}
	mp := &MetaProcessor{proc: &Processor{db: db, llm: newOfflineLLM(), cfg: &config.Config{}}, debounceTimers: make(map[int64]*time.Timer)}
	mp.processMetaSummary(context.Background(), taskID)
	var res model.SummaryResult
	if err := db.Where("task_id = ?", taskID).Order("version DESC").First(&res).Error; err != nil {
		t.Fatalf("load meta result: %v", err)
	}
	if !strings.Contains(res.Content, "如下：\n\n---") {
		t.Fatalf("A-1: single-submission fast path persisted raw setext content: %q", res.Content)
	}
}
