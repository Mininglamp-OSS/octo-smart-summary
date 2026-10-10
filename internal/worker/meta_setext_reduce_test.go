//go:build cgo

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// M8 pin (Jerry-Xin round-2 A-2, PR#268): no shipped test drove the meta
// multi-submission reduce branch with a setext shape, so identity-replacing
// the :254 normalize call kept the whole suite green (r1 mutant M8 survivor).
// This probe drives processMetaSummary's reduce path with 2+ submitted
// results and a stub reduce LLM emitting the setext shape, asserting the
// persisted SummaryResult.Content is neutralized. Passes today (the :254 call
// landed in round 2); it exists to die under the mutation.
func TestProcessMetaSummaryReduceNeutralizesSetext(t *testing.T) {
	content := "现将进展整理如下：\n---\n### 一、已完成事项\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		data, _ := json.Marshal(map[string]interface{}{"choices": []interface{}{
			map[string]interface{}{"delta": map[string]string{"content": content}, "finish_reason": "stop"},
		}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
	}))
	defer srv.Close()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&model.SummaryTask{}, &model.SummaryResult{}, &model.SummaryChunk{},
		&model.SummaryParticipant{}, &model.PersonalResult{},
	); err != nil {
		t.Fatal(err)
	}
	taskID := seedProcessingTask(t, db)
	seedSubmittedMember(t, db, taskID, "uA")
	seedSubmittedMember(t, db, taskID, "uB")
	mp := &MetaProcessor{proc: &Processor{
		db:  db,
		llm: service.NewLLMClient(srv.URL, "test", "test", 5, 1024, false, 5, nil),
		cfg: &config.Config{},
	}, debounceTimers: make(map[int64]*time.Timer)}
	mp.processMetaSummary(context.Background(), taskID)
	var res model.SummaryResult
	if err := db.Where("task_id = ?", taskID).Order("version DESC").First(&res).Error; err != nil {
		t.Fatalf("load meta result: %v", err)
	}
	if !strings.Contains(res.Content, "如下：\n\n---") {
		t.Fatalf("M8: meta reduce persisted raw setext content: %q", res.Content)
	}
}
