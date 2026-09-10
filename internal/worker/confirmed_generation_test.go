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
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
)

// This runs the production poller -> source authorization -> MySQL retrieval ->
// real personal pipeline -> local HTTP model adapter -> formal commit. Only the
// model response is synthetic; it never contacts an external model or IM user.
func TestContentWorkerConfirmedScheduleMySQLPipeline(t *testing.T) {
	db := contentWorkerTestDB(t)
	if db.Dialector.Name() != "mysql" {
		t.Skip("requires isolated MySQL for the production retrieval SQL")
	}
	t.Setenv("SUMMARY_CONTENT_WRITE_SPACES", "")
	t.Setenv("SUMMARY_CONTENT_EXECUTION_SPACES", "s")
	ctx := context.Background()
	initial := seedWorkerRefine(t, db)
	s := service.NewContentService(db).WithExecution(pipeline.GenerationSourceAuthorizer{DB: db}, 90)
	workerCheck(t, s.CancelGeneration(ctx, "s", initial.TaskID, "owner", initial.ContentID, initial.ID))
	for _, statement := range []string{
		"CREATE TABLE `group` (group_no VARCHAR(64), name VARCHAR(64), space_id VARCHAR(64), status INT, updated_at BIGINT)",
		"CREATE TABLE group_member (group_no VARCHAR(64), uid VARCHAR(64), is_deleted INT)",
		"CREATE TABLE space_member (space_id VARCHAR(64), uid VARCHAR(64), status INT)",
		"CREATE TABLE conversation_extra (uid VARCHAR(64), channel_id VARCHAR(64), channel_type INT, updated_at BIGINT)",
		"CREATE TABLE thread (id BIGINT, short_id VARCHAR(64), name VARCHAR(64), group_no VARCHAR(64), status INT, updated_at BIGINT)",
		"CREATE TABLE thread_member (thread_id BIGINT, uid VARCHAR(64))",
		"CREATE TABLE `user` (uid VARCHAR(64), name VARCHAR(64))",
		"CREATE TABLE message (message_seq BIGINT, from_uid VARCHAR(64), channel_id VARCHAR(64), channel_type INT, timestamp BIGINT, payload BLOB, is_deleted INT)",
		"INSERT INTO `group` VALUES ('group1','Project','s',1,0)",
		"INSERT INTO group_member VALUES ('group1','owner',0)",
		"INSERT INTO space_member VALUES ('s','owner',1)",
		"INSERT INTO `user` VALUES ('owner','Fixture author')",
	} {
		workerCheck(t, db.Exec(statement).Error)
	}
	now := timezone.Now().Truncate(time.Second)
	workerCheck(t, db.Exec("INSERT INTO message VALUES (1,'owner','group1',2,?,?,0)", now.Add(-time.Hour).Unix(), []byte(`{"type":1,"content":"Alpha release shipped successfully with all required checks."}`)).Error)
	requirement := "Confirmed requirements only"
	spec := model.SummaryGenerationSpec{
		SchemaVersion: 1, SummaryMode: model.ModeByPerson, Collaboration: "single", Participants: []string{"owner"},
		Sources:     []model.SummaryGenerationSource{{SourceID: "group1", SourceType: model.SourceGroup, Confirmation: "user_confirmed"}},
		Requirement: &requirement, TimeSelector: model.SummaryTimeSelector{Mode: "relative", Days: 7, Timezone: timezone.Name},
		CitationRules: model.SummaryCitationRules{Policy: "message_evidence"},
	}
	_, err := s.SaveGenerationConfiguration(ctx, "s", initial.TaskID, "owner", initial.ContentID, service.SaveGenerationConfigurationRequest{
		Spec: spec, Schedule: &service.GenerationScheduleConfig{Enabled: true, IntervalDays: 1, RunTime: "09:00"},
	})
	workerCheck(t, err)
	var task model.SummaryTask
	workerCheck(t, db.First(&task, initial.TaskID).Error)
	due := now.Add(-time.Minute)
	workerCheck(t, db.Model(&model.SummarySchedule{}).Where("id = ?", *task.ScheduleID).Update("next_run_at", due).Error)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if len(body["tools"]) > 0 || !strings.Contains(string(body["messages"]), requirement) ||
			!strings.Contains(string(body["messages"]), "Alpha release shipped") {
			t.Error("production adapter guessed scope or lost frozen requirements/evidence")
			w.WriteHeader(400)
			return
		}
		calls.Add(1)
		if string(body["stream"]) == "true" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Alpha completed [1]\"},\"finish_reason\":null}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":10}}\n\ndata: [DONE]\n\n")
		} else {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[{"message":{"content":"Alpha completed [1]"},"finish_reason":"stop"}],"usage":{"total_tokens":10}}`)
		}
	}))
	defer server.Close()
	cfg := config.Load()
	cfg.MessageFetchBackend, cfg.MsgTableCount, cfg.MaxMessagesPerChannel = "mysql", 1, 100
	cfg.LLMModel = "fixture"
	llm := service.NewLLMClient(server.URL, "fixture-key", "fixture", 5, 1000, false, 5, nil)
	pool := NewWorkerPool(1)
	proc := NewProcessor(db, db, pool, llm, cfg)
	w := NewContentGenerationWorker(db, pool, llm, time.Second).WithExecution(s, proc)
	count, err := w.poll(ctx)
	workerCheck(t, err)
	if count != 1 {
		t.Fatalf("due schedule was not queued and dispatched: %d", count)
	}
	pool.Drain()
	var run model.SummaryGenerationRun
	workerCheck(t, db.Where("task_id = ? AND operation_type = ?", task.ID, "scheduled_generate").First(&run).Error)
	if run.Status != "completed" || !run.Applied || calls.Load() != 1 {
		t.Fatalf("actual pipeline not completed: status=%s error=%s calls=%d", run.Status, run.ErrorCode, calls.Load())
	}
	catalog, err := s.Catalog(ctx, "s", task.ID, "owner")
	workerCheck(t, err)
	current := catalog.Contents[0].CurrentVersion
	if current.Version != 2 || current.Content != "Alpha completed [1]" || len(current.Citations) != 1 || !run.EffectiveAt.Equal(due) {
		t.Fatalf("incorrect pipeline output: %+v", current)
	}
}
