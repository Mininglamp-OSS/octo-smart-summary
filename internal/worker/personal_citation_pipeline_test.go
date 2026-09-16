//go:build cgo

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func TestPersonalPipelineCitationFinalization(t *testing.T) {
	for _, tc := range []struct {
		name, content, want string
		invalid             bool
	}{
		{"compound", "Budget [1,2].", "Budget [1][2].", false},
		{"year range", "Budget [1]. Plan [2024-2025].", "Budget [1]. Plan [2024-2025].", false},
		{"date", "Budget [1]. Date [2026-09-14].", "Budget [1]. Date [2026-09-14].", false},
		{"pages", "Budget [1]. Pages [100-120].", "Budget [1]. Pages [100-120].", false},
		{"orphan single", "Budget [1]. Other [999].", "", false},
		{"corrupt", "Budget [1,3].", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				data, _ := json.Marshal(map[string]interface{}{"choices": []interface{}{
					map[string]interface{}{"delta": map[string]string{"content": tc.content}, "finish_reason": "stop"},
				}})
				fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
			}))
			defer srv.Close()
			db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.AutoMigrate(&model.SummarySource{}); err != nil {
				t.Fatal(err)
			}
			p := &Processor{
				db: db, cfg: &config.Config{LLMModel: "test", CharsPerTokenASCII: 4, MapMaxTokens: 10000},
				llm: service.NewLLMClient(srv.URL, "test", "test", 5, 1024, false, 5, nil),
				fetchPersonalMessagesFn: func(context.Context, model.SummaryTask, string) ([]pipeline.Message, *pipeline.IntentResult, error) {
					return []pipeline.Message{
						{SenderUID: "creator", Content: "Budget approved", ChannelID: "group", MessageSeq: 1},
						{SenderUID: "creator", Content: "Budget details", ChannelID: "group", MessageSeq: 2},
					}, &pipeline.IntentResult{Skipped: true, SkipReason: "pure_generic_topic"}, nil
				},
			}
			task := model.SummaryTask{ID: 1, TaskNo: "citation-" + tc.name, CreatorID: "creator",
				TimeRangeStart: time.Now().Add(-time.Hour), TimeRangeEnd: time.Now()}
			got, cits, _, _, _, err := p.executePersonalPipeline(context.Background(), task, "creator", nil, func(string) error { return nil })
			if calls.Load() != 1 {
				t.Fatalf("expected one deterministic model call, got %d", calls.Load())
			}
			if tc.invalid {
				if err == nil {
					t.Fatalf("corrupt group persisted: %q", got)
				}
				return
			}
			if err != nil || len(cits) == 0 {
				t.Fatalf("pipeline error=%v content=%q citations=%v", err, got, cits)
			}
			if tc.want != "" && got != tc.want {
				t.Fatalf("content=%q want=%q", got, tc.want)
			}
			if strings.Contains(got, "[999]") {
				t.Fatal("orphan single no longer stripped")
			}
			if tc.name == "compound" && len(cits) != 2 {
				t.Fatal("normalization lost sources")
			}
		})
	}
}
