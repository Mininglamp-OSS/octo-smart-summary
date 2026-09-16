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
		wantCits            int
	}{
		// The model emits singles directly per OutputRule — the happy path.
		{"singles", "Budget [1][2].", "Budget [1][2].", false, 2},
		// A genuine citation cluster (compound abutting another marker) is
		// normalized to singles by CanonicalizeAdjacent.
		{"cluster", "Budget [1][1,2].", "Budget [1][2].", false, 2},
		// An isolated compound is treated as prose and left byte-identical — it
		// must never be rewritten into [1][2] (PR#248 review B-2), and per
		// PR#251 review P1-3 its indices are NOT harvested into citation rows:
		// extract and strip must agree that isolated groups are prose, so the
		// content keeps zero citations and would be strip-cleaned if orphaned.
		{"isolated compound", "Budget [1,2].", "Budget [1,2].", false, 0},
		{"year range", "Budget [1]. Plan [2024-2025].", "Budget [1]. Plan [2024-2025].", false, 1},
		{"date", "Budget [1]. Date [2026-09-14].", "Budget [1]. Date [2026-09-14].", false, 1},
		{"pages", "Budget [1]. Pages [100-120].", "Budget [1]. Pages [100-120].", false, 1},
		{"orphan single", "Budget [1]. Other [999].", "", false, 1},
		// An unresolvable group must NOT abort the whole summary; it is left as
		// prose rather than failing the task (PR#248 review B-1). No citation
		// rows are built from its indices (PR#251 review P1-3: extract must
		// not harvest compound groups — they stay prose end-to-end).
		{"unresolved group", "Budget [1,3].", "Budget [1,3].", false, 0},
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
			if err != nil {
				t.Fatalf("pipeline error=%v content=%q citations=%v", err, got, cits)
			}
			if tc.wantCits == 0 && len(cits) != 0 {
				t.Fatalf("prose-only content built citation rows: %v", cits)
			}
			if tc.wantCits > 0 && len(cits) == 0 {
				t.Fatalf("expected citation rows, content=%q citations=%v", got, cits)
			}
			if tc.want != "" && got != tc.want {
				t.Fatalf("content=%q want=%q", got, tc.want)
			}
			if strings.Contains(got, "[999]") {
				t.Fatal("orphan single no longer stripped")
			}
			if tc.wantCits != 0 && len(cits) != tc.wantCits {
				t.Fatalf("citations=%d want %d", len(cits), tc.wantCits)
			}
		})
	}
}
