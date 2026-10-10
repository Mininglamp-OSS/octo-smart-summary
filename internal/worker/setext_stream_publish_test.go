//go:build cgo

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// RED round-4 for PR#268 — mochashanyao's round-3 P1: publishing raw bytes on
// the wire while persisting normalized bytes breaks the byte-equality
// no-change guard in edit.go:121 (a no-op team edit can overwrite normalized
// content and auto-pause a scheduled task via is_active=0). The reviewer
// prescription: publish a streaming.EventSnapshot with the normalized bytes
// after normalization, so the hub's done frame (hub.go:154-155 re-stamps
// ev.Content = st.snapshot) carries exactly the persisted bytes — the same
// contract the refine streams already satisfy by re-sending finalized
// content in their done payloads.
//
// The probes below play the internal stream ingest: WorkerCallbackURL points
// at an httptest server that records the NDJSON event lines. Each must FAIL
// at head 7e9fef7 (no snapshot event ever arrives) and pass once the worker
// publishes the normalized snapshot.

type recordedEvents struct {
	mu     sync.Mutex
	types  []string
	bodies []map[string]any
	done   chan struct{}
}

func (r *recordedEvents) snapshot() ([]string, []map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.types...), r.bodies
}

func startEventIngest(t *testing.T) (*httptest.Server, *recordedEvents) {
	t.Helper()
	rec := &recordedEvents{done: make(chan struct{})}
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		dec := json.NewDecoder(req.Body)
		for {
			var ev map[string]any
			if err := dec.Decode(&ev); err != nil {
				break
			}
			rec.mu.Lock()
			rec.types = append(rec.types, fmt.Sprint(ev["type"]))
			rec.bodies = append(rec.bodies, ev)
			rec.mu.Unlock()
		}
		once.Do(func() { close(rec.done) })
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func waitIngest(t *testing.T, rec *recordedEvents) {
	t.Helper()
	select {
	case <-rec.done:
	case <-time.After(5 * time.Second):
		t.Fatal("internal stream POST never closed")
	}
}

// recordedSnapshotContent replays the last snapshot event's content.
func recordedSnapshotContent(t *testing.T, rec *recordedEvents) string {
	t.Helper()
	types, bodies := rec.snapshot()
	for i := len(types) - 1; i >= 0; i-- {
		if types[i] == "snapshot" {
			content, _ := bodies[i]["content"].(string)
			return content
		}
	}
	t.Fatal("no snapshot event recorded")
	return ""
}

func assertNormalizedSnapshotEvent(t *testing.T, rec *recordedEvents) {
	t.Helper()
	types, bodies := rec.snapshot()
	snapIdx := -1
	doneIdx := -1
	for i, ty := range types {
		if ty == "snapshot" {
			snapIdx = i
		}
		if ty == "done" {
			doneIdx = i
		}
	}
	if snapIdx == -1 {
		t.Fatalf("P1: no snapshot event published on the internal stream; wire carries raw bytes while persistence is normalized (events: %v)", types)
	}
	content, _ := bodies[snapIdx]["content"].(string)
	// The neutralized signature is a blank line before the rule. (The
	// personal pipeline's CanonicalizeAdjacent may insert the [n] marker
	// between the lead-in text and its colon, so anchor on the colon+rule
	// shape, not on the lead-in words.)
	if !strings.Contains(content, "：\n\n---") && !strings.Contains(content, "：\n \n---") {
		if strings.Contains(content, "：\n---") {
			t.Fatalf("P1: snapshot event still carries the raw setext shape: %q", content)
		}
		t.Fatalf("P1: snapshot event content not neutralized (no blank line before rule): %q", content)
	}
	if doneIdx != -1 && doneIdx < snapIdx {
		t.Fatalf("P1: done frame arrived before the normalized snapshot; done would still carry raw accumulation")
	}
}

// A-16 (mocha r4 finding 4, PR#268): the fixture emits the RAW setext shape —
// a pre-normalized fixture would keep passing under a remove-the-normalize
// mutant once the A-5 emptyLineRe fix stops deleting the blank line first, so
// the pin's killing power must not depend on that unrelated bug.
const setextStreamContent = "现将进展整理如下：\n---\n### 一、已完成事项\n\n内容甲[2]。\n"

func TestPersonalPipelinePublishesNormalizedSnapshot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		data, _ := json.Marshal(map[string]interface{}{"choices": []interface{}{
			map[string]interface{}{"delta": map[string]string{"content": setextStreamContent}, "finish_reason": "stop"},
		}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
	}))
	defer srv.Close()
	ingest, rec := startEventIngest(t)

	dsn := fmt.Sprintf("%s?_busy_timeout=10000&_journal_mode=WAL", t.TempDir()+"/snap.db")
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
	task := model.SummaryTask{TaskNo: "T-SEXT-SNAP", SpaceID: "sp", CreatorID: "creator",
		SummaryMode: model.ModeByPerson, Status: model.StatusPending,
		TimeRangeStart: now.Add(-time.Hour), TimeRangeEnd: now}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	part := model.SummaryParticipant{TaskID: task.ID, UserID: "creator", Status: model.ParticipantPending}
	db.Create(&part)
	pr := model.PersonalResult{TaskID: task.ID, ParticipantRefID: part.ID, UserID: "creator", WorkerStatus: model.PersonalStatusPending}
	db.Create(&pr)

	p := &Processor{
		db: db,
		cfg: &config.Config{LLMModel: "test", CharsPerTokenASCII: 4, MapMaxTokens: 10000,
			WorkerLeaseMinutes: 1, WorkerMaxRetry: 3, WorkerCallbackURL: ingest.URL},
		llm: service.NewLLMClient(srv.URL, "test", "test", 5, 1024, false, 5, nil),
		fetchPersonalMessagesFn: func(context.Context, model.SummaryTask, string) ([]pipeline.Message, *pipeline.IntentResult, error) {
			return []pipeline.Message{
				{SenderUID: "creator", Content: "进展说明", ChannelID: "group", MessageSeq: 1},
				{SenderUID: "creator", Content: "内容甲证据", ChannelID: "group", MessageSeq: 2},
			}, &pipeline.IntentResult{Skipped: true, SkipReason: "pure_generic_topic"}, nil
		},
	}
	p.processPersonalSummary(context.Background(), task.ID, part.ID)
	waitIngest(t, rec)
	assertNormalizedSnapshotEvent(t, rec)
	// A-16 parity pin (yujiawei r4 P2-4 / Jerry §2 adoption): the wire bytes
	// the client sees must equal the persisted bytes edit.go:121 compares
	// against — read the row back and diff against the replayed snapshot.
	var saved model.PersonalResult
	if err := db.First(&saved, pr.ID).Error; err != nil {
		t.Fatal(err)
	}
	wireContent := recordedSnapshotContent(t, rec)
	if wireContent != saved.Content {
		t.Fatalf("A-16: wire snapshot bytes != persisted row bytes:\n wire=%q\n row =%q", wireContent, saved.Content)
	}
}

func TestMetaReducePublishesNormalizedSnapshot(t *testing.T) {
	content := "现将进展整理如下：\n---\n### 一、已完成事项\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		data, _ := json.Marshal(map[string]interface{}{"choices": []interface{}{
			map[string]interface{}{"delta": map[string]string{"content": content}, "finish_reason": "stop"},
		}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
	}))
	defer srv.Close()
	ingest, rec := startEventIngest(t)

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
		cfg: &config.Config{WorkerCallbackURL: ingest.URL},
	}, debounceTimers: make(map[int64]*time.Timer)}
	mp.processMetaSummary(context.Background(), taskID)
	waitIngest(t, rec)
	assertNormalizedSnapshotEvent(t, rec)
}
