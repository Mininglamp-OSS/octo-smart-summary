//go:build cgo

package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// newFileBackedAgentSummaryTestDB returns a file-backed SQLite DB. The
// citation borrow/redaction tests CANNOT use setupAgentSummaryTestDB's
// ":memory:" DB: borrowCitationsFromReference (and the origin borrow)
// run inside the CreateAgentSummary transaction on h.db, so they grab a
// SECOND pool connection — and every ":memory:" connection is its own
// empty database ("no such table: summary_task"), making the borrow fail
// for the wrong reason. A file-backed DB is shared across all pool
// connections (same pattern as newFileBackedTestDB in the worker tests).
func newFileBackedAgentSummaryTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "agent_cit.db") + "?_busy_timeout=10000&_journal_mode=WAL"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(
		&model.AgentMessage{},
		&model.SummaryTask{},
		&model.SummarySource{},
		&model.SummaryParticipant{},
		&model.SummaryResult{},
		&model.PersonalResult{},
		&model.AgentMessageEvidence{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// postAgentSave issues POST /api/v1/summaries/agent with the given body and
// returns the recorder. Shared by all origin-derive tests.
func postAgentSave(r interface {
	ServeHTTP(w http.ResponseWriter, req *http.Request)
}, body map[string]interface{}) *httptest.ResponseRecorder {
	bodyBytes, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/summaries/agent", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Token", "test-user")
	req.Header.Set("X-Space-Id", "test-space")
	r.ServeHTTP(w, req)
	return w
}

// seedPipelineRefTask seeds a completed pipeline-style summary task:
// origin_channel_id empty, creator = test-user (accessible), given task_no.
func seedPipelineRefTask(t *testing.T, h *AgentSummaryHandler, taskNo string) model.SummaryTask {
	t.Helper()
	now := time.Now()
	ref := model.SummaryTask{
		TaskNo:         taskNo,
		SpaceID:        "test-space",
		CreatorID:      "test-user",
		Title:          "pipeline summary",
		Topic:          "pipeline summary",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: now,
		TimeRangeEnd:   now,
		Status:         model.StatusCompleted,
		TriggerType:    model.TriggerScheduled,
		// OriginChannelID deliberately empty — the shape that tier-3 misses.
	}
	if err := h.db.Create(&ref).Error; err != nil {
		t.Fatalf("seed ref task: %v", err)
	}
	return ref
}

// TestCreateAgentSummary_DeriveOriginFromSingleSource is the owner's core
// scenario (2026-08-13): reference a pipeline-generated summary (no
// origin_channel_id), let the agent refine it, save. Tier-3 borrow finds the
// task but origin is empty → tier-4 derives from the task's summary_source
// rows → save succeeds with the source channel as origin.
func TestCreateAgentSummary_DeriveOriginFromSingleSource(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	ref := seedPipelineRefTask(t, h, "ST-PIPE-001")
	if err := db.Create(&model.SummarySource{
		TaskID:     ref.ID,
		SourceType: model.SourceGroup,
		SourceID:   "CH-PIPELINE",
	}).Error; err != nil {
		t.Fatalf("seed summary_source: %v", err)
	}

	sessionID := "session-refine-pipeline"
	db.Create(&model.AgentMessage{
		UserID:    "test-user",
		SessionID: sessionID,
		Role:      "assistant",
		Content:   "Refined pipeline summary.",
	})

	w := postAgentSave(r, map[string]interface{}{
		"session_id":          sessionID,
		"title":               "Refined from pipeline",
		"referenced_task_ids": []int64{ref.ID},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// The created task (the newest one) must carry the derived origin.
	var tasks []model.SummaryTask
	db.Order("id DESC").Find(&tasks)
	if len(tasks) == 0 {
		t.Fatal("no task created")
	}
	created := tasks[0]
	if created.OriginChannelID != "CH-PIPELINE" {
		t.Errorf("origin_channel_id = %q, want CH-PIPELINE (derived from summary_source)", created.OriginChannelID)
	}
	if created.OriginChannelType != model.OriginChannelGroup {
		t.Errorf("origin_channel_type = %d, want %d (SourceGroup, same enum)", created.OriginChannelType, model.OriginChannelGroup)
	}
}

// TestCreateAgentSummary_DeriveOriginMultiSourceInheritsFirst: a task
// generated from two channels inherits the FIRST source row (by id, creation
// order) — deterministic, consistent with tier-3's first-referenced-task
// precedent.
func TestCreateAgentSummary_DeriveOriginMultiSourceInheritsFirst(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	ref := seedPipelineRefTask(t, h, "ST-PIPE-002")
	for _, src := range []struct {
		id  string
		typ int
	}{
		{"CH-FIRST", model.SourceGroup},
		{"CH-SECOND", model.SourceGroup},
	} {
		if err := db.Create(&model.SummarySource{
			TaskID:     ref.ID,
			SourceType: src.typ,
			SourceID:   src.id,
		}).Error; err != nil {
			t.Fatalf("seed summary_source: %v", err)
		}
	}

	sessionID := "session-refine-multi"
	db.Create(&model.AgentMessage{
		UserID:    "test-user",
		SessionID: sessionID,
		Role:      "assistant",
		Content:   "Refined multi-source summary.",
	})

	w := postAgentSave(r, map[string]interface{}{
		"session_id":          sessionID,
		"title":               "Refined multi-source",
		"referenced_task_ids": []int64{ref.ID},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var tasks []model.SummaryTask
	db.Order("id DESC").Find(&tasks)
	if tasks[0].OriginChannelID != "CH-FIRST" {
		t.Errorf("origin_channel_id = %q, want CH-FIRST (first usable source row wins)", tasks[0].OriginChannelID)
	}
}

// TestCreateAgentSummary_DeriveOriginSkipsUnusableRows: source rows with an
// empty source_id or an out-of-range source_type are skipped; the next usable
// row wins. If nothing usable remains the save still 40001s.
func TestCreateAgentSummary_DeriveOriginSkipsUnusableRows(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	ref := seedPipelineRefTask(t, h, "ST-PIPE-003")
	// Row 1: empty id (unusable). Row 2: source_type=0 (unusable). Row 3: valid.
	for _, src := range []struct {
		id  string
		typ int
	}{
		{"", model.SourceGroup},
		{"CH-ZERO-TYPE", 0},
		{"CH-VALID", model.SourceThread},
	} {
		if err := db.Create(&model.SummarySource{
			TaskID:     ref.ID,
			SourceType: src.typ,
			SourceID:   src.id,
		}).Error; err != nil {
			t.Fatalf("seed summary_source: %v", err)
		}
	}

	sessionID := "session-refine-skip"
	db.Create(&model.AgentMessage{
		UserID:    "test-user",
		SessionID: sessionID,
		Role:      "assistant",
		Content:   "Refined summary.",
	})

	w := postAgentSave(r, map[string]interface{}{
		"session_id":          sessionID,
		"title":               "Refined",
		"referenced_task_ids": []int64{ref.ID},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var tasks []model.SummaryTask
	db.Order("id DESC").Find(&tasks)
	if tasks[0].OriginChannelID != "CH-VALID" || tasks[0].OriginChannelType != model.OriginChannelThread {
		t.Errorf("origin = %q/%d, want CH-VALID/%d", tasks[0].OriginChannelID, tasks[0].OriginChannelType, model.OriginChannelThread)
	}
}

// TestCreateAgentSummary_DeriveOriginNoSourcesStill40001: referenced task
// with NO summary_source rows (legacy/erased data) still dead-ends in 40001 —
// the fail-closed contract is unchanged for genuinely origin-less summaries.
func TestCreateAgentSummary_DeriveOriginNoSourcesStill40001(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	ref := seedPipelineRefTask(t, h, "ST-PIPE-004") // no summary_source rows

	sessionID := "session-refine-nosrc"
	db.Create(&model.AgentMessage{
		UserID:    "test-user",
		SessionID: sessionID,
		Role:      "assistant",
		Content:   "Refined summary.",
	})

	w := postAgentSave(r, map[string]interface{}{
		"session_id":          sessionID,
		"title":               "Refined",
		"referenced_task_ids": []int64{ref.ID},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["code"].(float64) != 40001 {
		t.Errorf("expected code=40001, got %v", resp["code"])
	}
}

// TestCreateAgentSummary_DeriveOriginNoAccessRefused: a user who cannot read
// the referenced task (not creator, not participant) must NOT derive its
// source channels — same authz posture as tier-3.
func TestCreateAgentSummary_DeriveOriginNoAccessRefused(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	now := time.Now()
	ref := model.SummaryTask{
		TaskNo:         "ST-PIPE-005",
		SpaceID:        "test-space",
		CreatorID:      "someone-else", // not test-user, no participant row either
		Title:          "foreign pipeline summary",
		Topic:          "foreign pipeline summary",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: now,
		TimeRangeEnd:   now,
		Status:         model.StatusCompleted,
		TriggerType:    model.TriggerScheduled,
	}
	if err := db.Create(&ref).Error; err != nil {
		t.Fatalf("seed ref task: %v", err)
	}
	db.Create(&model.SummarySource{TaskID: ref.ID, SourceType: model.SourceGroup, SourceID: "CH-FOREIGN"})

	sessionID := "session-refine-foreign"
	db.Create(&model.AgentMessage{
		UserID:    "test-user",
		SessionID: sessionID,
		Role:      "assistant",
		Content:   "Refined summary.",
	})

	w := postAgentSave(r, map[string]interface{}{
		"session_id":          sessionID,
		"title":               "Refined",
		"referenced_task_ids": []int64{ref.ID},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (no access → no origin), got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["code"].(float64) != 40001 {
		t.Errorf("expected code=40001, got %v", resp["code"])
	}
}

// TestCreateAgentSummary_Tier3BorrowStillWins: when the referenced task HAS
// an origin_channel_id, tier-3 borrows it directly and tier-4 is never
// consulted (regression guard for the pre-existing path).
func TestCreateAgentSummary_Tier3BorrowStillWins(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	now := time.Now()
	ref := model.SummaryTask{
		TaskNo:            "ST-AGENT-006",
		SpaceID:           "test-space",
		CreatorID:         "test-user",
		Title:             "agent summary",
		Topic:             "agent summary",
		SummaryMode:       model.ModeByPerson,
		TimeRangeStart:    now,
		TimeRangeEnd:      now,
		Status:            model.StatusCompleted,
		TriggerType:       model.TriggerAgent,
		OriginChannelID:   "CH-TIER3",
		OriginChannelType: model.OriginChannelDM,
	}
	if err := db.Create(&ref).Error; err != nil {
		t.Fatalf("seed ref task: %v", err)
	}
	// Even with sources present, tier-3 must win.
	db.Create(&model.SummarySource{TaskID: ref.ID, SourceType: model.SourceGroup, SourceID: "CH-SHOULD-NOT-WIN"})

	sessionID := "session-refine-tier3"
	db.Create(&model.AgentMessage{
		UserID:    "test-user",
		SessionID: sessionID,
		Role:      "assistant",
		Content:   "Refined agent summary.",
	})

	w := postAgentSave(r, map[string]interface{}{
		"session_id":          sessionID,
		"title":               "Refined",
		"referenced_task_ids": []int64{ref.ID},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var tasks []model.SummaryTask
	db.Order("id DESC").Find(&tasks)
	if tasks[0].OriginChannelID != "CH-TIER3" || tasks[0].OriginChannelType != model.OriginChannelDM {
		t.Errorf("origin = %q/%d, want CH-TIER3/%d (tier-3 wins over sources)",
			tasks[0].OriginChannelID, tasks[0].OriginChannelType, model.OriginChannelDM)
	}
}

// R9 P2 (yujiawei, PR #190 review 4926742282, raised in the two preceding
// reviews too): "Origin borrow bypasses the resolver's deleted_at / completed
// gates ... SummaryTask.DeletedAt is *time.Time, not gorm.DeletedAt, so GORM
// does not auto-filter it — the borrow query really does match soft-deleted
// rows. The asymmetry is now sharper than when it was first raised: on the
// same request, borrowCitationsFromReference routes through
// resolveReferencedArtifact and correctly refuses a deleted task, while the
// origin borrow twenty lines earlier accepts it."
//
// Both a soft-deleted and a non-completed referenced task must now be
// refused by the origin borrow → the save lands on the no-origin 40001 path,
// same as an inaccessible task.
func TestCreateAgentSummary_OriginBorrowRefusesDeletedReferencedTask(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	now := time.Now()
	ref := model.SummaryTask{
		TaskNo:          "ST-PIPE-DEL-01",
		SpaceID:         "test-space",
		CreatorID:       "test-user",
		Title:           "deleted pipeline summary",
		SummaryMode:     model.ModeByPerson,
		TimeRangeStart:  now,
		TimeRangeEnd:    now,
		Status:          model.StatusCompleted,
		OriginChannelID: "CH-DELETED", // tier-3 would borrow this if ungated
	}
	if err := db.Create(&ref).Error; err != nil {
		t.Fatalf("seed ref task: %v", err)
	}
	db.Create(&model.SummarySource{TaskID: ref.ID, SourceType: model.SourceGroup, SourceID: "CH-DELETED"})
	deleted := time.Now()
	if err := db.Model(&model.SummaryTask{}).Where("id = ?", ref.ID).Update("deleted_at", deleted).Error; err != nil {
		t.Fatalf("soft-delete ref task: %v", err)
	}

	sessionID := "session-refine-deleted"
	db.Create(&model.AgentMessage{
		UserID:    "test-user",
		SessionID: sessionID,
		Role:      "assistant",
		Content:   "Refined summary.",
	})

	w := postAgentSave(r, map[string]interface{}{
		"session_id":          sessionID,
		"title":               "Refined",
		"referenced_task_ids": []int64{ref.ID},
	})
	// Soft-deleted referenced task: borrow must refuse it, same posture as an
	// inaccessible task (DeriveOriginNoAccessRefused) — no origin anywhere →
	// 40001. Symmetric with resolveReferencedArtifact, which already returns
	// not_found for deleted tasks on the citation/context paths.
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (deleted ref task → no origin), got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["code"].(float64) != 40001 {
		t.Errorf("expected code=40001, got %v", resp["code"])
	}
}

func TestCreateAgentSummary_OriginBorrowRefusesNonCompletedReferencedTask(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	now := time.Now()
	ref := model.SummaryTask{
		TaskNo:          "ST-PIPE-OPEN-01",
		SpaceID:         "test-space",
		CreatorID:       "test-user",
		Title:           "still processing",
		SummaryMode:     model.ModeByPerson,
		TimeRangeStart:  now,
		TimeRangeEnd:    now,
		Status:          model.StatusProcessing, // not completed
		OriginChannelID: "CH-OPEN",
	}
	if err := db.Create(&ref).Error; err != nil {
		t.Fatalf("seed ref task: %v", err)
	}
	db.Create(&model.SummarySource{TaskID: ref.ID, SourceType: model.SourceGroup, SourceID: "CH-OPEN"})

	sessionID := "session-refine-open"
	db.Create(&model.AgentMessage{
		UserID:    "test-user",
		SessionID: sessionID,
		Role:      "assistant",
		Content:   "Refined summary.",
	})

	w := postAgentSave(r, map[string]interface{}{
		"session_id":          sessionID,
		"title":               "Refined",
		"referenced_task_ids": []int64{ref.ID},
	})
	// Non-completed referenced task: borrow must refuse it — same as a
	// deleted one. resolveReferencedArtifact already refuses with
	// "not_completed" on the citation/context paths.
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (non-completed ref task → no origin), got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["code"].(float64) != 40001 {
		t.Errorf("expected code=40001, got %v", resp["code"])
	}
}

// R9 P2 (yujiawei, PR #190 review 4926742282): "CreateAgentSummary silently
// truncates over-cap referenced_task_ids; chat/stream return 400 ... a 400
// here would make the contract symmetric and is a two-line change."
//
// Chat (agent_chat.go:352) and ChatStream (agent_chat.go:522) both reject
// with apiResponse{Code: 40000, Message: "too many referenced task IDs: ..."}
// after dedup. CreateAgentSummary must do the same instead of persisting a
// partial lineage.
func TestCreateAgentSummary_OverCapReferencedTaskIDsRejected400(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	overCap := make([]int64, 0, maxReferencedTaskIDs+3)
	for i := 0; i < maxReferencedTaskIDs+3; i++ {
		overCap = append(overCap, int64(i+1))
	}

	sessionID := "session-refine-overcap"
	db.Create(&model.AgentMessage{
		UserID:    "test-user",
		SessionID: sessionID,
		Role:      "assistant",
		Content:   "Refined summary.",
	})

	w := postAgentSave(r, map[string]interface{}{
		"session_id":          sessionID,
		"title":               "Refined",
		"referenced_task_ids": overCap,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for over-cap referenced_task_ids, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["code"].(float64) != 40000 {
		t.Errorf("expected code=40000 (parity with chat/stream), got %v", resp["code"])
	}
	if msg, _ := resp["message"].(string); !strings.Contains(msg, "too many referenced task IDs") {
		t.Errorf("expected 'too many referenced task IDs' message, got %q", msg)
	}
	// Nothing persisted: no new summary task beyond the seeded ones.
	var count int64
	db.Model(&model.SummaryTask{}).Count(&count)
	if count != 0 {
		t.Errorf("over-cap request persisted %d task(s), want 0", count)
	}
}

// Dedup still happens before the cap check: 21 duplicates of the same id
// collapse to 1 and are accepted (mirrors agent_chat_test's
// dedup-then-check posture).
func TestCreateAgentSummary_DuplicateIDsCollapseBelowCapAccepted(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	ref := seedPipelineRefTask(t, h, "ST-PIPE-DUP-01")
	// seedPipelineRefTask leaves origin empty and seeds no sources; add one
	// so tier-4 can derive an origin and the save succeeds end-to-end.
	db.Create(&model.SummarySource{TaskID: ref.ID, SourceType: model.SourceGroup, SourceID: "CH-DUP"})
	dups := make([]int64, maxReferencedTaskIDs+1)
	for i := range dups {
		dups[i] = ref.ID
	}

	sessionID := "session-refine-dup"
	db.Create(&model.AgentMessage{
		UserID:    "test-user",
		SessionID: sessionID,
		Role:      "assistant",
		Content:   "Refined summary.",
	})

	w := postAgentSave(r, map[string]interface{}{
		"session_id":          sessionID,
		"title":               "Refined",
		"referenced_task_ids": dups,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (21 dups of one id dedupe to 1 ≤ cap), got %d: %s", w.Code, w.Body.String())
	}
}

// R9 P2-2 (yujiawei, PR #190 review 4926742282): "borrowCitationsFromReference
// now returns [] where it used to borrow ... the caller does not [handle the
// dangling markers]: CreateAgentSummary goes straight to
// creatorPR.SetCitations(cits) with an empty slice while content still
// carries the referenced summary's [n] markers, so the saved summary renders
// broken citation links. Either strip unresolvable [n] markers from content
// on this path, or drop the promise from the comment."
//
// Scenario: refine a BY_PERSON task whose team result has citations the
// caller cannot see (callerPlainCitationsVisible redacts → borrow returns
// []). The refined content preserves the original [n] marker; with no
// borrowable citations the marker dangles and must be stripped before save.
func TestCreateAgentSummary_DanglingMarkersStrippedWhenBorrowRedacted(t *testing.T) {
	db := newFileBackedAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	now := time.Now()
	ref := model.SummaryTask{
		TaskNo:            "ST-PIPE-CIT-01",
		SpaceID:           "test-space",
		CreatorID:         "test-user",
		Title:             "by-person task with redacted citations",
		SummaryMode:       model.ModeByPerson,
		TimeRangeStart:    now,
		TimeRangeEnd:      now,
		Status:            model.StatusCompleted,
		OriginChannelID:   "CH-REF",
		OriginChannelType: model.OriginChannelGroup,
	}
	if err := db.Create(&ref).Error; err != nil {
		t.Fatalf("seed ref task: %v", err)
	}
	// Team result WITH citations → callerPlainCitationsVisible returns false
	// for a caller lacking the matching PersonalResult, redacting them and
	// making borrow return [].
	result := model.SummaryResult{
		TaskID:        ref.ID,
		Version:       1,
		Content:       "Original summary [1].",
		CitationsJSON: `[{"index":1,"sender":"a","content":"msg","sent_at":"","source":"g1","channel_id":"g1","channel_type":2}]`,
	}
	if err := db.Create(&result).Error; err != nil {
		t.Fatalf("seed result: %v", err)
	}
	if err := db.Model(&model.SummaryTask{}).Where("id = ?", ref.ID).
		Update("current_result_id", result.ID).Error; err != nil {
		t.Fatalf("set current_result_id: %v", err)
	}

	sessionID := "session-refine-dangling"
	db.Create(&model.AgentMessage{
		UserID:    "test-user",
		SessionID: sessionID,
		Role:      "assistant",
		Content:   "Refined version [1] of the summary.",
	})

	w := postAgentSave(r, map[string]interface{}{
		"session_id":          sessionID,
		"title":               "Refined",
		"referenced_task_ids": []int64{ref.ID},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var tasks []model.SummaryTask
	db.Order("id DESC").Find(&tasks)
	newTaskID := tasks[0].ID
	var pr model.PersonalResult
	if err := db.Where("task_id = ?", newTaskID).First(&pr).Error; err != nil {
		t.Fatalf("load saved personal_result: %v", err)
	}
	if strings.Contains(pr.Content, "[1]") {
		t.Errorf("saved content still carries dangling [1] marker (borrow was redacted): %q", pr.Content)
	}
	if got := pr.GetCitations(); len(got) != 0 {
		t.Errorf("expected empty citations, got %d", len(got))
	}
}

// Borrow SUCCEEDS (citations visible → borrowed verbatim): markers must be
// PRESERVED, not stripped. Guards the strip against over-reaching into the
// happy path.
func TestCreateAgentSummary_MarkersPreservedWhenBorrowSucceeds(t *testing.T) {
	db := newFileBackedAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)

	now := time.Now()
	ref := model.SummaryTask{
		TaskNo:            "ST-PIPE-CIT-02",
		SpaceID:           "test-space",
		CreatorID:         "test-user",
		Title:             "non-by-person task with borrowable citations",
		SummaryMode:       1, // not BY_PERSON (ModeByPerson=2) → no redaction
		TimeRangeStart:    now,
		TimeRangeEnd:      now,
		Status:            model.StatusCompleted,
		OriginChannelID:   "CH-REF2",
		OriginChannelType: model.OriginChannelGroup,
	}
	if err := db.Create(&ref).Error; err != nil {
		t.Fatalf("seed ref task: %v", err)
	}
	citsJSON := `[{"index":1,"sender":"a","content":"msg","sent_at":"","source":"g1","channel_id":"g1","channel_type":2}]`
	result := model.SummaryResult{
		TaskID:        ref.ID,
		Version:       1,
		Content:       "Original summary [1].",
		CitationsJSON: citsJSON,
	}
	if err := db.Create(&result).Error; err != nil {
		t.Fatalf("seed result: %v", err)
	}
	if err := db.Model(&model.SummaryTask{}).Where("id = ?", ref.ID).
		Update("current_result_id", result.ID).Error; err != nil {
		t.Fatalf("set current_result_id: %v", err)
	}

	sessionID := "session-refine-borrowok"
	db.Create(&model.AgentMessage{
		UserID:    "test-user",
		SessionID: sessionID,
		Role:      "assistant",
		Content:   "Refined version [1] of the summary.",
	})

	w := postAgentSave(r, map[string]interface{}{
		"session_id":          sessionID,
		"title":               "Refined",
		"referenced_task_ids": []int64{ref.ID},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var tasks []model.SummaryTask
	db.Order("id DESC").Find(&tasks)
	var pr model.PersonalResult
	if err := db.Where("task_id = ?", tasks[0].ID).First(&pr).Error; err != nil {
		t.Fatalf("load saved personal_result: %v", err)
	}
	if !strings.Contains(pr.Content, "[1]") {
		t.Errorf("borrow succeeded but [1] marker was stripped: %q", pr.Content)
	}
	if got := pr.GetCitations(); len(got) != 1 {
		t.Errorf("expected 1 borrowed citation, got %d", len(got))
	}
}
