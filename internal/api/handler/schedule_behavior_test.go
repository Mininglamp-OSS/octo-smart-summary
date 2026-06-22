package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// ---------------------------------------------------------------------------
// Self-contained harness for the scheduled-summary handler behavior tests.
//
// These tests exercise the HTTP contract of the schedule endpoints against an
// in-memory sqlite DB. Note: sqlite cannot enforce the MySQL generated-column
// live-binding unique index, so binding invariants are asserted through the
// handler's own application-level guards (the path real callers hit), not the
// DB constraint. The DB constraint itself is validated separately against a
// real MySQL 8.0 instance (see migrations/sql validation).
// ---------------------------------------------------------------------------

func newScheduleTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(
		&model.SummaryTask{},
		&model.SummarySchedule{},
		&model.SummaryParticipant{},
		&model.SummarySource{},
		&model.SummaryResult{},
		&model.SummaryChunk{},
		&model.PersonalResult{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func newScheduleTestRouter(db *gorm.DB) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	sh := NewScheduleHandler(db)
	th := NewTaskHandler(db, db, "")
	r.POST("/api/v1/summary-schedules", sh.CreateSchedule)
	r.PUT("/api/v1/summary-schedules/:id", sh.UpdateSchedule)
	r.DELETE("/api/v1/summary-schedules/:id", sh.DeleteSchedule)
	r.PUT("/api/v1/summary-schedules/:id/toggle", sh.ToggleSchedule)
	r.DELETE("/api/v1/summaries/:id", th.DeleteSummary)
	return r
}

func scheduleReq(t *testing.T, r *gin.Engine, userID, spaceID, method, path string, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req.Header.Set("Token", userID)
	req.Header.Set("X-Space-Id", spaceID)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func seedScheduleTask(t *testing.T, db *gorm.DB, taskNo, space, creator string) int64 {
	t.Helper()
	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo: taskNo, SpaceID: space, CreatorID: creator,
		SummaryMode: model.ModeByPerson, Status: model.StatusCompleted,
		TimeRangeStart: now, TimeRangeEnd: now,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("seed task: %v", err)
	}
	// The single-person guard counts participants; a sole creator participant
	// keeps the task single-person.
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: creator, UserName: "C"})
	return task.ID
}

func sid(v int64) string { return strconv.FormatInt(v, 10) }

// ---------------------------------------------------------------------------
// Create: validation contract.
// ---------------------------------------------------------------------------

func TestCreateSchedule_RejectsCronWrite(t *testing.T) {
	db := newScheduleTestDB(t)
	r := newScheduleTestRouter(db)
	taskID := seedScheduleTask(t, db, "T1", "s1", "u1")

	w := scheduleReq(t, r, "u1", "s1", http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"scope": "task", "task_id": taskID, "cron_expr": "0 9 * * *",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 rejecting cron write, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateSchedule_RejectsNonTaskScope(t *testing.T) {
	db := newScheduleTestDB(t)
	r := newScheduleTestRouter(db)

	w := scheduleReq(t, r, "u1", "s1", http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"interval_days": 1, "run_time": "09:00",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing scope=task, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateSchedule_RejectsMalformedRunTime(t *testing.T) {
	db := newScheduleTestDB(t)
	r := newScheduleTestRouter(db)
	taskID := seedScheduleTask(t, db, "T1", "s1", "u1")

	w := scheduleReq(t, r, "u1", "s1", http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"scope": "task", "task_id": taskID, "interval_days": 1, "run_time": "9:0",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed run_time, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateSchedule_RejectsAnchorModeMismatch(t *testing.T) {
	db := newScheduleTestDB(t)
	r := newScheduleTestRouter(db)
	taskID := seedScheduleTask(t, db, "T1", "s1", "u1")

	// day_of_month is only valid in month mode; supplying it in day mode fails.
	w := scheduleReq(t, r, "u1", "s1", http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"scope": "task", "task_id": taskID, "interval_days": 3, "run_time": "09:00", "day_of_month": 5,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for day-mode + day_of_month, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateSchedule_BindsUnscheduledTask(t *testing.T) {
	db := newScheduleTestDB(t)
	r := newScheduleTestRouter(db)
	taskID := seedScheduleTask(t, db, "T1", "s1", "u1")

	w := scheduleReq(t, r, "u1", "s1", http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"scope": "task", "task_id": taskID, "interval_days": 7, "run_time": "09:00", "day_of_week": 1,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 binding schedule, got %d: %s", w.Code, w.Body.String())
	}
	var task model.SummaryTask
	if err := db.First(&task, taskID).Error; err != nil {
		t.Fatal(err)
	}
	if task.ScheduleID == nil {
		t.Fatal("task should be bound to a schedule after create")
	}
}

// ---------------------------------------------------------------------------
// Single-person invariant: scheduled summary rejects multi-person tasks.
// ---------------------------------------------------------------------------

func TestCreateSchedule_MultiPersonTaskRejected(t *testing.T) {
	db := newScheduleTestDB(t)
	r := newScheduleTestRouter(db)
	taskID := seedScheduleTask(t, db, "T1", "s1", "u1")
	// Add a second, non-creator participant -> task is multi-person.
	db.Create(&model.SummaryParticipant{TaskID: taskID, UserID: "other", UserName: "O"})

	w := scheduleReq(t, r, "u1", "s1", http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"scope": "task", "task_id": taskID, "interval_days": 1, "run_time": "09:00",
	})
	if w.Code == http.StatusOK {
		t.Fatalf("expected rejection for multi-person task, got 200: %s", w.Body.String())
	}
}

func TestCreateSchedule_RebindIdempotentForSameTask(t *testing.T) {
	db := newScheduleTestDB(t)
	r := newScheduleTestRouter(db)
	taskID := seedScheduleTask(t, db, "T1", "s1", "u1")

	// First create binds a schedule.
	w := scheduleReq(t, r, "u1", "s1", http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"scope": "task", "task_id": taskID, "interval_days": 1, "run_time": "09:00",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("first create: %d %s", w.Code, w.Body.String())
	}
	// A second create for the same task must reuse/update the existing schedule
	// rather than create a second one (one-to-one binding).
	w = scheduleReq(t, r, "u1", "s1", http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"scope": "task", "task_id": taskID, "interval_days": 2, "run_time": "10:00",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("second create: %d %s", w.Code, w.Body.String())
	}
	var count int64
	db.Model(&model.SummarySchedule{}).Where("deleted_at IS NULL").Count(&count)
	if count != 1 {
		t.Fatalf("expected exactly 1 live schedule, got %d", count)
	}
}

// ---------------------------------------------------------------------------
// Cascade delete authorization: deleting a summary may only cascade-delete a
// bound schedule when the caller owns the schedule; otherwise it unbinds.
// ---------------------------------------------------------------------------

func TestDeleteSummary_CreatorCascadeDeletesOwnSchedule(t *testing.T) {
	db := newScheduleTestDB(t)
	r := newScheduleTestRouter(db)
	taskID := seedScheduleTask(t, db, "T1", "s1", "u1")

	w := scheduleReq(t, r, "u1", "s1", http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"scope": "task", "task_id": taskID, "interval_days": 1, "run_time": "09:00",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("bind: %d %s", w.Code, w.Body.String())
	}
	var task model.SummaryTask
	db.First(&task, taskID)
	schedID := *task.ScheduleID

	// Creator deletes their own summary -> the schedule is cascade soft-deleted.
	w = scheduleReq(t, r, "u1", "s1", http.MethodDelete, "/api/v1/summaries/"+sid(taskID), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("delete summary: %d %s", w.Code, w.Body.String())
	}
	var sched model.SummarySchedule
	if err := db.Unscoped().First(&sched, schedID).Error; err != nil {
		t.Fatal(err)
	}
	if sched.DeletedAt == nil {
		t.Error("schedule should be cascade soft-deleted by its creator")
	}
}

func TestToggleSchedule_ReenableInvalidRecurrenceRejected(t *testing.T) {
	db := newScheduleTestDB(t)
	r := newScheduleTestRouter(db)
	taskID := seedScheduleTask(t, db, "T1", "s1", "u1")

	// Bind a valid schedule via the API so the task<->schedule link exists.
	w := scheduleReq(t, r, "u1", "s1", http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"scope": "task", "task_id": taskID, "interval_days": 1, "run_time": "09:00",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("bind: %d %s", w.Code, w.Body.String())
	}
	var task model.SummaryTask
	db.First(&task, taskID)
	schedID := *task.ScheduleID

	// Force the schedule inactive and strip every recurrence source, simulating
	// a row that can no longer compute a next_run.
	if err := db.Model(&model.SummarySchedule{}).Where("id = ?", schedID).Updates(map[string]interface{}{
		"is_active": 0, "interval_days": 0, "interval_months": 0, "cron_expr": "",
	}).Error; err != nil {
		t.Fatal(err)
	}

	w = scheduleReq(t, r, "u1", "s1", http.MethodPut, "/api/v1/summary-schedules/"+sid(schedID)+"/toggle", map[string]interface{}{
		"is_active": true,
	})
	if w.Code == http.StatusOK {
		t.Fatalf("expected rejection re-enabling invalid-recurrence schedule, got 200: %s", w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// Display order (B1): the latest result wins. A newer scheduled/regenerated
// result (higher version) must be shown even when an older row was hand-edited,
// so a scheduled update is never permanently masked by a stale edited row.
// ---------------------------------------------------------------------------

func TestQueryDisplayResult_NewerScheduledResultWinsOverEditedRow(t *testing.T) {
	db := newScheduleTestDB(t)
	taskID := int64(42)

	edited := time.Now()
	// Older, hand-edited result (lower version).
	if err := db.Create(&model.SummaryResult{TaskID: taskID, Content: "edited v1", Version: 1, EditedAt: &edited, GeneratedAt: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}
	// Newer scheduled result (higher version, not edited).
	if err := db.Create(&model.SummaryResult{TaskID: taskID, Content: "scheduled v2", Version: 2, GeneratedAt: time.Now()}).Error; err != nil {
		t.Fatal(err)
	}

	got, err := queryDisplayResult(db, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "scheduled v2" {
		t.Fatalf("display result = %q, want the newer scheduled result %q (edited row must not mask it)", got.Content, "scheduled v2")
	}
}

func TestQueryDisplayResult_EditedRowShownWhenItIsLatest(t *testing.T) {
	db := newScheduleTestDB(t)
	taskID := int64(43)

	edited := time.Now()
	db.Create(&model.SummaryResult{TaskID: taskID, Content: "auto v1", Version: 1, GeneratedAt: time.Now()})
	// The latest version happens to be the edited one -> it is shown.
	db.Create(&model.SummaryResult{TaskID: taskID, Content: "edited v2", Version: 2, EditedAt: &edited, GeneratedAt: time.Now()})

	got, err := queryDisplayResult(db, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "edited v2" {
		t.Fatalf("display result = %q, want latest edited %q", got.Content, "edited v2")
	}
}

// ---------------------------------------------------------------------------
// Participant dedup (B3): a create payload with duplicate participant ids must
// not blow up the (task_id,user_id) unique index; the handler de-duplicates so
// each participant is inserted once.
// ---------------------------------------------------------------------------

func TestCreateSummary_DeduplicatesDuplicateParticipants(t *testing.T) {
	db := newScheduleTestDB(t)
	// Enforce the production unique constraint in sqlite too, so a missing
	// dedup would surface as an insert error here.
	if err := db.Exec("CREATE UNIQUE INDEX uk_part ON summary_participant(task_id, user_id)").Error; err != nil {
		t.Fatal(err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	th := NewTaskHandler(db, db, "")
	r.POST("/api/v1/summaries", th.CreateSummary)

	body := map[string]interface{}{
		"sources": []map[string]interface{}{{"source_type": 1, "source_id": "grp1"}},
		"participants": []map[string]interface{}{
			{"user_id": "p1"}, {"user_id": "p1"}, {"user_id": "p2"}, {"user_id": "creator"},
		},
		"time_range_type": 2,
	}
	w := scheduleReq(t, r, "creator", "s1", http.MethodPost, "/api/v1/summaries", body)
	if w.Code != http.StatusOK {
		t.Fatalf("create with duplicate participants should succeed (deduped), got %d: %s", w.Code, w.Body.String())
	}

	// p1 must exist exactly once despite being listed twice.
	var p1Count int64
	db.Model(&model.SummaryParticipant{}).Where("user_id = ?", "p1").Count(&p1Count)
	if p1Count != 1 {
		t.Fatalf("participant p1 count = %d, want 1 (deduped)", p1Count)
	}
}

// ---------------------------------------------------------------------------
// FEATURE_TEAM_SCHEDULE flag gating (merged from team_schedule_flag_test.go).
// ---------------------------------------------------------------------------

// newScheduleTestRouterWithFlag mirrors newScheduleTestRouter but lets a test
// drive the FEATURE_TEAM_SCHEDULE flag through the handler, exercising the
// flag-gated multi-person guard on create/update/toggle.
func newScheduleTestRouterWithFlag(db *gorm.DB, featureTeamSchedule bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	sh := NewScheduleHandlerWithFlag(db, featureTeamSchedule)
	r.POST("/api/v1/summary-schedules", sh.CreateSchedule)
	r.PUT("/api/v1/summary-schedules/:id", sh.UpdateSchedule)
	r.PUT("/api/v1/summary-schedules/:id/toggle", sh.ToggleSchedule)
	r.POST("/api/v1/summary-schedules/:id/confirm", sh.ConfirmSchedule)
	r.GET("/api/v1/summary-schedules/:id", sh.GetSchedule)
	return r
}

// Flag OFF (default): a multi-person task is rejected with 40015. Regression
// guard for the existing behavior under the new flag-gated code path.
func TestCreateSchedule_FlagOff_MultiPersonRejected(t *testing.T) {
	db := newScheduleTestDB(t)
	r := newScheduleTestRouterWithFlag(db, false)
	taskID := seedScheduleTask(t, db, "T1", "s1", "u1")
	db.Create(&model.SummaryParticipant{TaskID: taskID, UserID: "other", UserName: "O"})

	w := scheduleReq(t, r, "u1", "s1", http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"scope": "task", "task_id": taskID, "interval_days": 1, "run_time": "09:00",
	})
	if w.Code == http.StatusOK {
		t.Fatalf("flag off: expected rejection for multi-person task, got 200: %s", w.Body.String())
	}
	// Precise 40015 (errMultiPersonNotSupported) must be returned.
	var resp apiResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if resp.Code != 40015 {
		t.Fatalf("flag off multi-person reject code = %d, want 40015; body=%s", resp.Code, w.Body.String())
	}
}

// Flag ON: the multi-person guard is bypassed, so binding a schedule to a
// multi-person task succeeds.
func TestCreateSchedule_FlagOn_MultiPersonAllowed(t *testing.T) {
	db := newScheduleTestDB(t)
	r := newScheduleTestRouterWithFlag(db, true)
	taskID := seedScheduleTask(t, db, "T1", "s1", "u1")
	db.Create(&model.SummaryParticipant{TaskID: taskID, UserID: "other", UserName: "O"})

	w := scheduleReq(t, r, "u1", "s1", http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"scope": "task", "task_id": taskID, "interval_days": 1, "run_time": "09:00",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("flag on: expected 200 binding multi-person schedule, got %d: %s", w.Code, w.Body.String())
	}
	var task model.SummaryTask
	if err := db.First(&task, taskID).Error; err != nil {
		t.Fatal(err)
	}
	if task.ScheduleID == nil {
		t.Fatal("multi-person task should be bound to a schedule when flag is on")
	}
}

// Flag ON: update + toggle of a multi-person schedule are not blocked either.
func TestUpdateAndToggleSchedule_FlagOn_MultiPersonAllowed(t *testing.T) {
	db := newScheduleTestDB(t)
	r := newScheduleTestRouterWithFlag(db, true)
	taskID := seedScheduleTask(t, db, "T1", "s1", "u1")
	db.Create(&model.SummaryParticipant{TaskID: taskID, UserID: "other", UserName: "O"})

	// Bind first.
	w := scheduleReq(t, r, "u1", "s1", http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"scope": "task", "task_id": taskID, "interval_days": 1, "run_time": "09:00",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("bind: got %d: %s", w.Code, w.Body.String())
	}
	var sched model.SummarySchedule
	if err := db.Where("creator_id = ?", "u1").First(&sched).Error; err != nil {
		t.Fatalf("load schedule: %v", err)
	}

	// Update must succeed under flag-on.
	wu := scheduleReq(t, r, "u1", "s1", http.MethodPut, "/api/v1/summary-schedules/"+sid(sched.ID), map[string]interface{}{
		"interval_days": 2, "run_time": "10:00",
	})
	if wu.Code != http.StatusOK {
		t.Fatalf("flag on update multi-person: got %d: %s", wu.Code, wu.Body.String())
	}

	// Toggle must succeed under flag-on.
	wt := scheduleReq(t, r, "u1", "s1", http.MethodPut, "/api/v1/summary-schedules/"+sid(sched.ID)+"/toggle", map[string]interface{}{
		"is_active": false,
	})
	if wt.Code != http.StatusOK {
		t.Fatalf("flag on toggle multi-person: got %d: %s", wt.Code, wt.Body.String())
	}
	var got model.SummarySchedule
	db.First(&got, sched.ID)
	if got.IsActive != 0 {
		t.Errorf("toggle should set is_active=0, got %d", got.IsActive)
	}
}

// ---------------------------------------------------------------------------
// /submit concurrency safety (Blocker-2): the manual /submit must never
// overwrite a system back-fill (submit_source=2) and must be idempotent under
// a conditional UPDATE ... WHERE submitted_at IS NULL.
// ---------------------------------------------------------------------------

func newPersonalTestRouter(db *gorm.DB) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	ph := NewPersonalHandler(db, "", nil)
	r.POST("/api/v1/summaries/:id/submit", ph.Submit)
	return r
}

func seedSubmitFixture(t *testing.T, db *gorm.DB, taskNo, user string) (int64, int64) {
	t.Helper()
	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo: taskNo, SpaceID: "s1", CreatorID: user, SummaryMode: model.ModeByPerson,
		Status: model.StatusProcessing, TriggerType: model.TriggerScheduled,
		TimeRangeStart: now, TimeRangeEnd: now,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	part := model.SummaryParticipant{TaskID: task.ID, UserID: user, Status: model.ParticipantCompleted}
	if err := db.Create(&part).Error; err != nil {
		t.Fatalf("create participant: %v", err)
	}
	pr := model.PersonalResult{
		TaskID: task.ID, ParticipantRefID: part.ID, UserID: user,
		WorkerStatus: model.PersonalStatusCompleted,
	}
	if err := db.Create(&pr).Error; err != nil {
		t.Fatalf("create personal_result: %v", err)
	}
	return task.ID, pr.ID
}

// A normal /submit on an un-submitted, completed personal result writes
// submit_source=1 and succeeds.
func TestSubmit_FirstSubmit_SetsManualSource(t *testing.T) {
	db := newScheduleTestDB(t)
	r := newPersonalTestRouter(db)
	taskID, prID := seedSubmitFixture(t, db, "T-SUB-1", "u1")

	w := scheduleReq(t, r, "u1", "s1", http.MethodPost, "/api/v1/summaries/"+sid(taskID)+"/submit", map[string]interface{}{})
	if w.Code != http.StatusOK {
		t.Fatalf("submit: got %d: %s", w.Code, w.Body.String())
	}
	var got model.PersonalResult
	db.First(&got, prID)
	if got.SubmittedAt == nil {
		t.Fatal("submitted_at must be set after manual submit")
	}
	if got.SubmitSource != model.SubmitSourceManual {
		t.Errorf("submit_source=%d, want SubmitSourceManual(1)", got.SubmitSource)
	}
}

// If the system already back-filled (submit_source=2), a racing manual /submit
// must NOT overwrite it (RowsAffected==0 path), and must still respond
// idempotently with "submitted".
func TestSubmit_DoesNotOverwriteSystemBackfill(t *testing.T) {
	db := newScheduleTestDB(t)
	r := newPersonalTestRouter(db)
	taskID, prID := seedSubmitFixture(t, db, "T-SUB-2", "u1")

	// Simulate the system back-fill having already won the race.
	sysTime := time.Now().UTC().Add(-time.Minute)
	if err := db.Model(&model.PersonalResult{}).Where("id = ?", prID).
		Updates(map[string]interface{}{
			"submitted_at":  sysTime,
			"submit_source": model.SubmitSourceSystem,
		}).Error; err != nil {
		t.Fatalf("seed system back-fill: %v", err)
	}

	w := scheduleReq(t, r, "u1", "s1", http.MethodPost, "/api/v1/summaries/"+sid(taskID)+"/submit", map[string]interface{}{})
	if w.Code != http.StatusOK {
		t.Fatalf("submit (already system-submitted) should be idempotent 200, got %d: %s", w.Code, w.Body.String())
	}
	var got model.PersonalResult
	db.First(&got, prID)
	// The crucial assertion: system source is NOT flipped to manual.
	if got.SubmitSource != model.SubmitSourceSystem {
		t.Errorf("manual submit overwrote system source: submit_source=%d, want SubmitSourceSystem(2)", got.SubmitSource)
	}
	if got.SubmittedAt == nil || !got.SubmittedAt.Equal(sysTime) {
		t.Errorf("submitted_at overwritten: got %v want %v", got.SubmittedAt, sysTime)
	}
}

// ---------------------------------------------------------------------------
// /accept idempotency (unique-key 500 fix): the AUTO scheduled dispatch path may
// pre-create a summary_personal_result row (uk_task_participant(task_id,
// participant_ref_id)) while the participant is still Pending. The old Accept did
// an UNCONDITIONAL tx.Create(&pr), which violated the unique key and returned a
// 500 "Duplicate entry". Accept must be idempotent: reuse the existing row, never
// insert a duplicate, and never 500.
// ---------------------------------------------------------------------------

// newPersonalAcceptTestRouter wires the /accept route against the personal
// handler. workerTriggerURL is empty so triggerWorker is a no-op in tests.
func newPersonalAcceptTestRouter(db *gorm.DB) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	ph := NewPersonalHandler(db, "", nil)
	r.POST("/api/v1/summaries/:id/accept", ph.Accept)
	return r
}

// newAcceptTestDBWithUniqueKey mirrors the production unique constraint in
// sqlite so a missing idempotency guard surfaces as an insert error (500),
// exactly like MySQL's uk_task_participant.
func newAcceptTestDBWithUniqueKey(t *testing.T) *gorm.DB {
	t.Helper()
	db := newScheduleTestDB(t)
	if err := db.Exec(
		"CREATE UNIQUE INDEX uk_task_participant ON summary_personal_result(task_id, participant_ref_id)",
	).Error; err != nil {
		t.Fatalf("create unique index: %v", err)
	}
	return db
}

// (a) AUTO pre-dispatch scenario: a personal_result already exists while the
// participant is still Pending. Accept must NOT 500, must be idempotent, and
// must leave exactly one personal_result row (the pre-existing one reused).
func TestAccept_PersonalResultPreCreated_IsIdempotent(t *testing.T) {
	db := newAcceptTestDBWithUniqueKey(t)
	r := newPersonalAcceptTestRouter(db)

	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo: "T-ACC-1", SpaceID: "s1", CreatorID: "u1", SummaryMode: model.ModeByPerson,
		Status: model.StatusProcessing, TriggerType: model.TriggerScheduled,
		TimeRangeStart: now, TimeRangeEnd: now,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	// Participant still Pending (the AUTO path has not flipped it to Accepted yet).
	part := model.SummaryParticipant{TaskID: task.ID, UserID: "u1", Status: model.ParticipantPending}
	if err := db.Create(&part).Error; err != nil {
		t.Fatalf("create participant: %v", err)
	}
	// AUTO dispatch pre-created the personal_result row already.
	preexisting := model.PersonalResult{
		TaskID: task.ID, ParticipantRefID: part.ID, UserID: "u1",
		WorkerStatus: model.PersonalStatusPending, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&preexisting).Error; err != nil {
		t.Fatalf("pre-create personal_result: %v", err)
	}

	w := scheduleReq(t, r, "u1", "s1", http.MethodPost,
		"/api/v1/summaries/"+sid(task.ID)+"/accept", map[string]interface{}{})
	// Before the fix this returned 500 ("Duplicate entry"); after the fix it is 200.
	if w.Code != http.StatusOK {
		t.Fatalf("accept with pre-created personal_result should be 200 idempotent, got %d: %s",
			w.Code, w.Body.String())
	}

	// Unique key invariant: still exactly one personal_result row.
	var prCount int64
	db.Model(&model.PersonalResult{}).
		Where("task_id = ? AND participant_ref_id = ?", task.ID, part.ID).Count(&prCount)
	if prCount != 1 {
		t.Fatalf("personal_result count = %d, want 1 (reused, no duplicate)", prCount)
	}

	// Participant flipped to Accepted and linked to the pre-existing row.
	var gotPart model.SummaryParticipant
	db.First(&gotPart, part.ID)
	if gotPart.Status != model.ParticipantAccepted {
		t.Errorf("participant status = %d, want ParticipantAccepted(1)", gotPart.Status)
	}
	if gotPart.PersonalResultID == nil || *gotPart.PersonalResultID != preexisting.ID {
		t.Errorf("personal_result_id = %v, want %d (the reused row)",
			gotPart.PersonalResultID, preexisting.ID)
	}
}

// (b) Normal path: no personal_result yet. Accept must create exactly one,
// flip the participant to Accepted, and back-fill personal_result_id.
func TestAccept_NoPersonalResult_CreatesOne(t *testing.T) {
	db := newAcceptTestDBWithUniqueKey(t)
	r := newPersonalAcceptTestRouter(db)

	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo: "T-ACC-2", SpaceID: "s1", CreatorID: "u1", SummaryMode: model.ModeByPerson,
		Status: model.StatusProcessing, TriggerType: model.TriggerScheduled,
		TimeRangeStart: now, TimeRangeEnd: now,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	part := model.SummaryParticipant{TaskID: task.ID, UserID: "u1", Status: model.ParticipantPending}
	if err := db.Create(&part).Error; err != nil {
		t.Fatalf("create participant: %v", err)
	}

	w := scheduleReq(t, r, "u1", "s1", http.MethodPost,
		"/api/v1/summaries/"+sid(task.ID)+"/accept", map[string]interface{}{})
	if w.Code != http.StatusOK {
		t.Fatalf("accept should be 200, got %d: %s", w.Code, w.Body.String())
	}

	var prs []model.PersonalResult
	db.Where("task_id = ? AND participant_ref_id = ?", task.ID, part.ID).Find(&prs)
	if len(prs) != 1 {
		t.Fatalf("personal_result count = %d, want 1 (created)", len(prs))
	}

	var gotPart model.SummaryParticipant
	db.First(&gotPart, part.ID)
	if gotPart.Status != model.ParticipantAccepted {
		t.Errorf("participant status = %d, want ParticipantAccepted(1)", gotPart.Status)
	}
	if gotPart.PersonalResultID == nil || *gotPart.PersonalResultID != prs[0].ID {
		t.Errorf("personal_result_id = %v, want %d (the created row)",
			gotPart.PersonalResultID, prs[0].ID)
	}
}

// Calling Accept twice (duplicate user clicks racing the AUTO pre-create) must
// stay idempotent and never produce a second personal_result row.
func TestAccept_DoubleCall_StaysIdempotent(t *testing.T) {
	db := newAcceptTestDBWithUniqueKey(t)
	r := newPersonalAcceptTestRouter(db)

	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo: "T-ACC-3", SpaceID: "s1", CreatorID: "u1", SummaryMode: model.ModeByPerson,
		Status: model.StatusProcessing, TriggerType: model.TriggerScheduled,
		TimeRangeStart: now, TimeRangeEnd: now,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	part := model.SummaryParticipant{TaskID: task.ID, UserID: "u1", Status: model.ParticipantPending}
	if err := db.Create(&part).Error; err != nil {
		t.Fatalf("create participant: %v", err)
	}

	path := "/api/v1/summaries/" + sid(task.ID) + "/accept"
	w1 := scheduleReq(t, r, "u1", "s1", http.MethodPost, path, map[string]interface{}{})
	if w1.Code != http.StatusOK {
		t.Fatalf("first accept should be 200, got %d: %s", w1.Code, w1.Body.String())
	}
	// Second call: participant is now Accepted, so the status-only fast path returns
	// 200 without touching the DB. Either way must remain 200 and single-row.
	w2 := scheduleReq(t, r, "u1", "s1", http.MethodPost, path, map[string]interface{}{})
	if w2.Code != http.StatusOK {
		t.Fatalf("second accept should be 200 idempotent, got %d: %s", w2.Code, w2.Body.String())
	}

	var prCount int64
	db.Model(&model.PersonalResult{}).
		Where("task_id = ? AND participant_ref_id = ?", task.ID, part.ID).Count(&prCount)
	if prCount != 1 {
		t.Fatalf("personal_result count = %d, want 1 after double accept", prCount)
	}
}

// Terminal-state protection: when the pre-existing personal_result is already
// terminal (Completed or already Submitted) and the participant is still Pending,
// Accept must reuse the row WITHOUT resetting worker_status back to Pending and
// WITHOUT clobbering submitted_at -- otherwise an accept could overwrite a finished
// summary. (triggerWorker is a no-op here since workerTriggerURL is empty; the
// observable invariant is that the terminal fields are left untouched.)
func TestAccept_TerminalPersonalResult_NotReset(t *testing.T) {
	db := newAcceptTestDBWithUniqueKey(t)
	r := newPersonalAcceptTestRouter(db)

	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo: "T-ACC-4", SpaceID: "s1", CreatorID: "u1", SummaryMode: model.ModeByPerson,
		Status: model.StatusProcessing, TriggerType: model.TriggerScheduled,
		TimeRangeStart: now, TimeRangeEnd: now,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	part := model.SummaryParticipant{TaskID: task.ID, UserID: "u1", Status: model.ParticipantPending}
	if err := db.Create(&part).Error; err != nil {
		t.Fatalf("create participant: %v", err)
	}
	// Pre-existing row is already terminal: Completed + submitted.
	submittedAt := now.Add(-time.Hour)
	terminal := model.PersonalResult{
		TaskID: task.ID, ParticipantRefID: part.ID, UserID: "u1",
		Content: "final summary", WorkerStatus: model.PersonalStatusCompleted,
		SubmittedAt: &submittedAt, SubmitSource: model.SubmitSourceManual,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&terminal).Error; err != nil {
		t.Fatalf("pre-create terminal personal_result: %v", err)
	}

	w := scheduleReq(t, r, "u1", "s1", http.MethodPost,
		"/api/v1/summaries/"+sid(task.ID)+"/accept", map[string]interface{}{})
	if w.Code != http.StatusOK {
		t.Fatalf("accept over terminal result should be 200, got %d: %s", w.Code, w.Body.String())
	}

	// Still exactly one row, and its terminal fields are untouched.
	var got model.PersonalResult
	if err := db.Where("task_id = ? AND participant_ref_id = ?", task.ID, part.ID).
		First(&got).Error; err != nil {
		t.Fatalf("read back personal_result: %v", err)
	}
	if got.WorkerStatus != model.PersonalStatusCompleted {
		t.Errorf("worker_status was reset: got %d, want PersonalStatusCompleted(2)", got.WorkerStatus)
	}
	if got.SubmittedAt == nil || !got.SubmittedAt.Equal(submittedAt) {
		t.Errorf("submitted_at clobbered: got %v, want %v", got.SubmittedAt, submittedAt)
	}
	if got.Content != "final summary" {
		t.Errorf("content clobbered: got %q, want %q", got.Content, "final summary")
	}

	var cnt int64
	db.Model(&model.PersonalResult{}).
		Where("task_id = ? AND participant_ref_id = ?", task.ID, part.ID).Count(&cnt)
	if cnt != 1 {
		t.Fatalf("personal_result count = %d, want 1", cnt)
	}
}

// ---------------------------------------------------------------------------
// V5 one-time confirm (schedule-level) handler tests.
// ---------------------------------------------------------------------------

// loadV5Cfg reads a schedule's participant_config in the V5 normalized form.
func loadV5Cfg(t *testing.T, db *gorm.DB, schedID int64) model.ScheduleParticipantConfig {
	t.Helper()
	var s model.SummarySchedule
	if err := db.First(&s, schedID).Error; err != nil {
		t.Fatalf("load schedule %d: %v", schedID, err)
	}
	return model.ParseScheduleParticipantConfig(s.ParticipantConfig)
}

// createV5ConfirmSchedule binds a multi-person task to a CONFIRM schedule (flag on)
// and returns the schedule id. participant_config is persisted in V5 object form
// with creator+others all confirmed=false.
func createV5ConfirmSchedule(t *testing.T, db *gorm.DB, r *gin.Engine) int64 {
	t.Helper()
	taskID := seedScheduleTask(t, db, "TC", "s1", "u1")
	db.Create(&model.SummaryParticipant{TaskID: taskID, UserID: "u2", UserName: "B"})
	w := scheduleReq(t, r, "u1", "s1", http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"scope": "task", "task_id": taskID, "interval_days": 1, "run_time": "09:00",
		"participants":   []map[string]interface{}{{"user_id": "u2", "user_name": "B"}},
		"confirm_policy": 1,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("create v5 confirm schedule: %d %s", w.Code, w.Body.String())
	}
	var sched model.SummarySchedule
	if err := db.Where("creator_id = ?", "u1").First(&sched).Error; err != nil {
		t.Fatalf("load schedule: %v", err)
	}
	return sched.ID
}

// V5 item 6: confirm_policy=1 multi-person create persists V5 object-form config
// with creator(u1)+u2 both confirmed=false and gate not passed.
func TestCreateSchedule_V5_ConfirmPolicyPersistsObjectForm(t *testing.T) {
	db := newScheduleTestDB(t)
	r := newScheduleTestRouterWithFlag(db, true)
	schedID := createV5ConfirmSchedule(t, db, r)

	var sched model.SummarySchedule
	db.First(&sched, schedID)
	if sched.ConfirmPolicy != model.SchedConfirmRequire {
		t.Fatalf("confirm_policy not persisted, got %d", sched.ConfirmPolicy)
	}
	cfg := model.ParseScheduleParticipantConfig(sched.ParticipantConfig)
	if cfg.FindParticipant("u1") == nil || cfg.FindParticipant("u2") == nil {
		t.Fatalf("creator+u2 must be in roster, got %+v", cfg.Participants)
	}
	if cfg.IsConfirmed("u1") || cfg.IsConfirmed("u2") {
		t.Errorf("nobody should be confirmed at create, got %+v", cfg.Participants)
	}
	if cfg.ConfirmGatePassed {
		t.Errorf("gate must be false at create")
	}
}

// V5 item 4: ConfirmSchedule writes confirmed=true for the caller; gate passes
// only after EVERY roster member (creator included, Q2) confirms.
func TestConfirmSchedule_OneTime_GateAfterAll(t *testing.T) {
	db := newScheduleTestDB(t)
	r := newScheduleTestRouterWithFlag(db, true)
	schedID := createV5ConfirmSchedule(t, db, r)

	// u2 confirms first -> u2 confirmed, gate still false (creator not yet).
	w := scheduleReq(t, r, "u2", "s1", http.MethodPost, "/api/v1/summary-schedules/"+sid(schedID)+"/confirm", map[string]interface{}{})
	if w.Code != http.StatusOK {
		t.Fatalf("u2 confirm: %d %s", w.Code, w.Body.String())
	}
	cfg := loadV5Cfg(t, db, schedID)
	if !cfg.IsConfirmed("u2") || cfg.IsConfirmed("u1") {
		t.Fatalf("after u2 confirm: u2 true, u1 false; got %+v", cfg.Participants)
	}
	if cfg.ConfirmGatePassed {
		t.Errorf("gate must NOT pass until creator confirms (Q2)")
	}

	// creator confirms -> gate passes.
	w2 := scheduleReq(t, r, "u1", "s1", http.MethodPost, "/api/v1/summary-schedules/"+sid(schedID)+"/confirm", map[string]interface{}{})
	if w2.Code != http.StatusOK {
		t.Fatalf("u1 confirm: %d %s", w2.Code, w2.Body.String())
	}
	cfg = loadV5Cfg(t, db, schedID)
	if !cfg.ConfirmGatePassed {
		t.Fatalf("gate must pass after all confirmed, got %+v", cfg)
	}

	// Idempotent re-confirm.
	w3 := scheduleReq(t, r, "u2", "s1", http.MethodPost, "/api/v1/summary-schedules/"+sid(schedID)+"/confirm", map[string]interface{}{})
	if w3.Code != http.StatusOK {
		t.Fatalf("idempotent re-confirm: %d %s", w3.Code, w3.Body.String())
	}
}

// V5 item 4: a non-roster user cannot confirm.
func TestConfirmSchedule_NonMemberRejected(t *testing.T) {
	db := newScheduleTestDB(t)
	r := newScheduleTestRouterWithFlag(db, true)
	schedID := createV5ConfirmSchedule(t, db, r)

	w := scheduleReq(t, r, "stranger", "s1", http.MethodPost, "/api/v1/summary-schedules/"+sid(schedID)+"/confirm", map[string]interface{}{})
	if w.Code == http.StatusOK {
		t.Fatalf("non-member confirm must be rejected, got 200")
	}
}

// createAutoMultiSchedule binds a multi-person AUTO schedule (confirm_policy=0,
// flag on) with creator u1 + u2 and returns its id. AUTO schedules have no
// confirm step, but membership must still be enforced on the confirm endpoint.
func createAutoMultiSchedule(t *testing.T, db *gorm.DB, r *gin.Engine) int64 {
	t.Helper()
	taskID := seedScheduleTask(t, db, "TA", "s1", "u1")
	db.Create(&model.SummaryParticipant{TaskID: taskID, UserID: "u2", UserName: "B"})
	w := scheduleReq(t, r, "u1", "s1", http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"scope": "task", "task_id": taskID, "interval_days": 1, "run_time": "09:00",
		"participants":   []map[string]interface{}{{"user_id": "u2", "user_name": "B"}},
		"confirm_policy": 0,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("create auto multi schedule: %d %s", w.Code, w.Body.String())
	}
	var sched model.SummarySchedule
	if err := db.Where("creator_id = ?", "u1").First(&sched).Error; err != nil {
		t.Fatalf("load schedule: %v", err)
	}
	return sched.ID
}

// 🟠 REGRESSION (finding 4): ConfirmSchedule on an AUTO schedule previously
// returned 200 BEFORE any membership check, so a non-member could call confirm on
// an AUTO schedule and get a success. Membership must now be enforced ahead of the
// AUTO fast-path: a non-member gets a 4xx (not 200).
func TestConfirmSchedule_AUTO_NonMemberRejected(t *testing.T) {
	db := newScheduleTestDB(t)
	r := newScheduleTestRouterWithFlag(db, true)
	schedID := createAutoMultiSchedule(t, db, r)

	// Sanity: it is an AUTO schedule.
	var sched model.SummarySchedule
	db.First(&sched, schedID)
	if sched.ConfirmPolicy != model.SchedConfirmAuto {
		t.Fatalf("expected AUTO schedule, got confirm_policy=%d", sched.ConfirmPolicy)
	}

	w := scheduleReq(t, r, "stranger", "s1", http.MethodPost, "/api/v1/summary-schedules/"+sid(schedID)+"/confirm", map[string]interface{}{})
	if w.Code == http.StatusOK {
		t.Fatalf("non-member confirm on AUTO schedule must be rejected, got 200: %s", w.Body.String())
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-member AUTO confirm want 403, got %d: %s", w.Code, w.Body.String())
	}
}

// Counterpart: a roster MEMBER confirming an AUTO schedule still succeeds (no-op
// success) — the membership check must not break the legitimate AUTO path.
func TestConfirmSchedule_AUTO_MemberStillSucceeds(t *testing.T) {
	db := newScheduleTestDB(t)
	r := newScheduleTestRouterWithFlag(db, true)
	schedID := createAutoMultiSchedule(t, db, r)

	for _, uid := range []string{"u1", "u2"} {
		w := scheduleReq(t, r, uid, "s1", http.MethodPost, "/api/v1/summary-schedules/"+sid(schedID)+"/confirm", map[string]interface{}{})
		if w.Code != http.StatusOK {
			t.Fatalf("member %s confirm on AUTO schedule must succeed, got %d: %s", uid, w.Code, w.Body.String())
		}
	}
}

// V5 item 5 / Q3: a member change (participant_config update) keeps existing
// members' confirm state and only resets NEW members; the gate is recomputed.
func TestUpdateSchedule_V5_MemberChange_OnlyNewReconfirm(t *testing.T) {
	db := newScheduleTestDB(t)
	r := newScheduleTestRouterWithFlag(db, true)
	schedID := createV5ConfirmSchedule(t, db, r)

	// Fully confirm (u2 then u1) so gate passes.
	scheduleReq(t, r, "u2", "s1", http.MethodPost, "/api/v1/summary-schedules/"+sid(schedID)+"/confirm", map[string]interface{}{})
	scheduleReq(t, r, "u1", "s1", http.MethodPost, "/api/v1/summary-schedules/"+sid(schedID)+"/confirm", map[string]interface{}{})
	if !loadV5Cfg(t, db, schedID).ConfirmGatePassed {
		t.Fatalf("precondition: gate must be passed")
	}

	// Add a new member u3 (keep u2). Q3: u2 stays confirmed, u3 is new -> false,
	// gate drops.
	wu := scheduleReq(t, r, "u1", "s1", http.MethodPut, "/api/v1/summary-schedules/"+sid(schedID), map[string]interface{}{
		"participants": []map[string]interface{}{
			{"user_id": "u2", "user_name": "B"},
			{"user_id": "u3", "user_name": "C"},
		},
	})
	if wu.Code != http.StatusOK {
		t.Fatalf("member-change update: %d %s", wu.Code, wu.Body.String())
	}
	cfg := loadV5Cfg(t, db, schedID)
	if !cfg.IsConfirmed("u2") {
		t.Errorf("Q3: existing member u2 must KEEP its confirm state")
	}
	if !cfg.IsConfirmed("u1") {
		t.Errorf("Q3: creator (unchanged) must keep confirm state")
	}
	if cfg.IsConfirmed("u3") {
		t.Errorf("Q3: newly added u3 must start unconfirmed")
	}
	if cfg.ConfirmGatePassed {
		t.Errorf("gate must drop when a new unconfirmed member is added")
	}
}

// V5 item 5 / §4.2: converting an AUTO schedule to CONFIRM (policy change) triggers
// a FULL re-confirm — every member (creator included) is reset to unconfirmed.
func TestUpdateSchedule_V5_AutoToConfirm_FullReconfirm(t *testing.T) {
	db := newScheduleTestDB(t)
	r := newScheduleTestRouterWithFlag(db, true)

	// Create an AUTO multi-person schedule (confirm_policy=0 explicit).
	taskID := seedScheduleTask(t, db, "TA", "s1", "u1")
	db.Create(&model.SummaryParticipant{TaskID: taskID, UserID: "u2", UserName: "B"})
	w := scheduleReq(t, r, "u1", "s1", http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"scope": "task", "task_id": taskID, "interval_days": 1, "run_time": "09:00",
		"participants":   []map[string]interface{}{{"user_id": "u2", "user_name": "B"}},
		"confirm_policy": 0,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("create auto schedule: %d %s", w.Code, w.Body.String())
	}
	var sched model.SummarySchedule
	db.Where("creator_id = ?", "u1").First(&sched)
	if sched.ConfirmPolicy != model.SchedConfirmAuto {
		t.Fatalf("precondition: AUTO, got %d", sched.ConfirmPolicy)
	}

	// Convert to CONFIRM.
	wu := scheduleReq(t, r, "u1", "s1", http.MethodPut, "/api/v1/summary-schedules/"+sid(sched.ID), map[string]interface{}{
		"confirm_policy": 1,
	})
	if wu.Code != http.StatusOK {
		t.Fatalf("auto->confirm update: %d %s", wu.Code, wu.Body.String())
	}
	var got model.SummarySchedule
	db.First(&got, sched.ID)
	if got.ConfirmPolicy != model.SchedConfirmRequire {
		t.Fatalf("confirm_policy must be CONFIRM after conversion, got %d", got.ConfirmPolicy)
	}
	cfg := model.ParseScheduleParticipantConfig(got.ParticipantConfig)
	if cfg.FindParticipant("u1") == nil || cfg.FindParticipant("u2") == nil {
		t.Fatalf("roster must contain creator+u2, got %+v", cfg.Participants)
	}
	if cfg.IsConfirmed("u1") || cfg.IsConfirmed("u2") {
		t.Errorf("§4.2: auto->confirm must reset ALL members to unconfirmed, got %+v", cfg.Participants)
	}
	if cfg.ConfirmGatePassed {
		t.Errorf("gate must be false after full re-confirm reset")
	}
}

// V5 §4.2: re-activating a CONFIRM schedule (is_active 0->1 via toggle) triggers a
// full re-confirm of all members.
func TestToggleSchedule_V5_Reactivate_FullReconfirm(t *testing.T) {
	db := newScheduleTestDB(t)
	r := newScheduleTestRouterWithFlag(db, true)
	schedID := createV5ConfirmSchedule(t, db, r)

	// Confirm everyone so gate passes.
	scheduleReq(t, r, "u2", "s1", http.MethodPost, "/api/v1/summary-schedules/"+sid(schedID)+"/confirm", map[string]interface{}{})
	scheduleReq(t, r, "u1", "s1", http.MethodPost, "/api/v1/summary-schedules/"+sid(schedID)+"/confirm", map[string]interface{}{})
	if !loadV5Cfg(t, db, schedID).ConfirmGatePassed {
		t.Fatalf("precondition: gate passed")
	}

	// Deactivate then re-activate.
	scheduleReq(t, r, "u1", "s1", http.MethodPut, "/api/v1/summary-schedules/"+sid(schedID)+"/toggle", map[string]interface{}{"is_active": false})
	wt := scheduleReq(t, r, "u1", "s1", http.MethodPut, "/api/v1/summary-schedules/"+sid(schedID)+"/toggle", map[string]interface{}{"is_active": true})
	if wt.Code != http.StatusOK {
		t.Fatalf("re-activate: %d %s", wt.Code, wt.Body.String())
	}
	cfg := loadV5Cfg(t, db, schedID)
	if cfg.IsConfirmed("u1") || cfg.IsConfirmed("u2") {
		t.Errorf("§4.2: re-activation must reset all confirm state, got %+v", cfg.Participants)
	}
	if cfg.ConfirmGatePassed {
		t.Errorf("gate must drop after re-activation reset")
	}
}

// 🟠 REGRESSION (finding 3): storedParticipantConfigSubsetOfCreator /
// validateEffectiveParticipantsSubsetOfCreator must normalize the V5 object-form
// participant_config. Previously the bare-array Unmarshal failed on the object
// form and the function fell into the fail-closed (unsafe) path, wrongly
// rejecting Update/Toggle of a CREATOR-ONLY V5 schedule when
// FEATURE_TEAM_SCHEDULE is off.
func TestStoredParticipantConfigSubsetOfCreator_V5ObjectForm(t *testing.T) {
	creator := "u1"

	// Creator-only V5 object form -> subset of {creator} -> PASS (true).
	creatorOnly := model.JSON(`{"participants":[{"user_id":"u1","confirmed":false}],"confirm_gate_passed":false}`)
	if !storedParticipantConfigSubsetOfCreator(creatorOnly, creator) {
		t.Fatalf("creator-only V5 object config must be a subset of {creator} (not fail-closed)")
	}

	// Multi-person V5 object form -> NOT a subset -> false.
	multi := model.JSON(`{"participants":[{"user_id":"u1"},{"user_id":"u2"}],"confirm_gate_passed":false}`)
	if storedParticipantConfigSubsetOfCreator(multi, creator) {
		t.Fatalf("multi-person V5 object config must NOT be a subset of {creator}")
	}

	// Empty config -> subset (PASS).
	if !storedParticipantConfigSubsetOfCreator(model.JSON(``), creator) {
		t.Fatalf("empty config must be a subset of {creator}")
	}

	// validateEffectiveParticipantsSubsetOfCreator (flag off, req nil -> uses
	// stored config): creator-only object form must NOT error.
	if err := validateEffectiveParticipantsSubsetOfCreator(false, nil, creatorOnly, creator); err != nil {
		t.Fatalf("creator-only V5 object config must pass the subset guard (flag off), got %v", err)
	}
	// multi-person stored config must error.
	if err := validateEffectiveParticipantsSubsetOfCreator(false, nil, multi, creator); err == nil {
		t.Fatalf("multi-person V5 object config must be rejected by the subset guard (flag off)")
	}
}

// ---------------------------------------------------------------------------
// 🐛 REGRESSION: "manual -> scheduled" conversion must NOT drop collaborators.
//
// The frontend detail-page "convert to scheduled" flow historically called
// createSchedule/updateSchedule WITHOUT forwarding participants. With multi-user
// collaboration enabled (FEATURE_TEAM_SCHEDULE on), participant_config then
// degenerated to creator-only and the scheduled run lost every collaborator.
// The backend now backfills the roster from the task's REAL participants.
// ---------------------------------------------------------------------------

// CreateSchedule on a multi-person task with an EMPTY req.Participants must still
// persist the full roster (creator + collaborator), defaulting to CONFIRM, with
// nobody confirmed yet — instead of degenerating to a single-person summary.
func TestCreateSchedule_ManualToScheduled_BackfillsTaskParticipants(t *testing.T) {
	db := newScheduleTestDB(t)
	r := newScheduleTestRouterWithFlag(db, true)

	// Multi-person manual task: creator u1 + collaborator Danno.
	taskID := seedScheduleTask(t, db, "TBF", "s1", "u1")
	db.Create(&model.SummaryParticipant{TaskID: taskID, UserID: "danno", UserName: "Danno", Status: model.ParticipantAccepted})

	// "Convert to scheduled" with NO participants forwarded (the bug trigger).
	w := scheduleReq(t, r, "u1", "s1", http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"scope": "task", "task_id": taskID, "interval_days": 1, "run_time": "09:00",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("create schedule: %d %s", w.Code, w.Body.String())
	}

	var sched model.SummarySchedule
	if err := db.Where("creator_id = ?", "u1").First(&sched).Error; err != nil {
		t.Fatalf("load schedule: %v", err)
	}
	// Multi-person => default CONFIRM.
	if sched.ConfirmPolicy != model.SchedConfirmRequire {
		t.Fatalf("multi-person task must default to CONFIRM, got confirm_policy=%d", sched.ConfirmPolicy)
	}
	cfg := model.ParseScheduleParticipantConfig(sched.ParticipantConfig)
	if cfg.FindParticipant("u1") == nil {
		t.Errorf("roster must contain creator u1, got %+v", cfg.Participants)
	}
	if cfg.FindParticipant("danno") == nil {
		t.Fatalf("BUG: collaborator Danno dropped from schedule roster (single-person degeneration), got %+v", cfg.Participants)
	}
	if len(cfg.Participants) != 2 {
		t.Fatalf("roster must have exactly 2 members (creator + Danno), got %d: %+v", len(cfg.Participants), cfg.Participants)
	}
	if cfg.IsConfirmed("u1") || cfg.IsConfirmed("danno") {
		t.Errorf("nobody should be confirmed on a fresh manual->scheduled conversion, got %+v", cfg.Participants)
	}
	if cfg.ConfirmGatePassed {
		t.Errorf("gate must be false until all members confirm")
	}
}

// Counterpart: a genuinely single-person task (creator only) must NOT be inflated
// — config stays creator-only and remains AUTO (no false multi-person promotion).
func TestCreateSchedule_SinglePersonTask_NotInflated(t *testing.T) {
	db := newScheduleTestDB(t)
	r := newScheduleTestRouterWithFlag(db, true)

	// seedScheduleTask seeds a sole creator participant -> single-person.
	taskID := seedScheduleTask(t, db, "TSP", "s1", "u1")

	w := scheduleReq(t, r, "u1", "s1", http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"scope": "task", "task_id": taskID, "interval_days": 1, "run_time": "09:00",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("create schedule: %d %s", w.Code, w.Body.String())
	}

	var sched model.SummarySchedule
	if err := db.Where("creator_id = ?", "u1").First(&sched).Error; err != nil {
		t.Fatalf("load schedule: %v", err)
	}
	if sched.ConfirmPolicy != model.SchedConfirmAuto {
		t.Fatalf("single-person task must stay AUTO, got confirm_policy=%d", sched.ConfirmPolicy)
	}
	cfg := model.ParseScheduleParticipantConfig(sched.ParticipantConfig)
	for _, p := range cfg.Participants {
		if p.UserID != "u1" {
			t.Fatalf("single-person schedule must not contain non-creator %q, got %+v", p.UserID, cfg.Participants)
		}
	}
}

// Security/correctness: a creator-only task whose request body carries a crafted
// or stale `participants` entry for a user that is NOT a real task participant
// must NOT be inflated into a multi-person CONFIRM schedule. The task roster is
// the sole membership authority; req-only ids are ignored.
func TestCreateSchedule_SinglePersonTask_StaleReqParticipantIgnored(t *testing.T) {
	db := newScheduleTestDB(t)
	r := newScheduleTestRouterWithFlag(db, true)

	// Sole creator participant -> genuinely single-person task.
	taskID := seedScheduleTask(t, db, "TSP", "s1", "u1")

	// Request body injects a non-participant "ghost" id.
	w := scheduleReq(t, r, "u1", "s1", http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"scope": "task", "task_id": taskID, "interval_days": 1, "run_time": "09:00",
		"participants": []map[string]interface{}{{"user_id": "ghost", "user_name": "Ghost"}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("create schedule: %d %s", w.Code, w.Body.String())
	}

	var sched model.SummarySchedule
	if err := db.Where("creator_id = ?", "u1").First(&sched).Error; err != nil {
		t.Fatalf("load schedule: %v", err)
	}
	if sched.ConfirmPolicy != model.SchedConfirmAuto {
		t.Fatalf("stale req participant must NOT promote single-person task to CONFIRM, got confirm_policy=%d", sched.ConfirmPolicy)
	}
	cfg := model.ParseScheduleParticipantConfig(sched.ParticipantConfig)
	for _, p := range cfg.Participants {
		if p.UserID != "u1" {
			t.Fatalf("schedule roster must stay creator-only; ghost req id leaked: %+v", cfg.Participants)
		}
	}
}

// UpdateSchedule "manual -> scheduled / convert to CONFIRM" on a multi-person task
// with an EMPTY req.Participants must also backfill the roster from the task's
// real participants (parity with CreateSchedule), not degenerate to creator-only.
func TestUpdateSchedule_ManualToScheduled_BackfillsTaskParticipants(t *testing.T) {
	db := newScheduleTestDB(t)
	r := newScheduleTestRouterWithFlag(db, true)

	// Multi-person task creator u1 + Danno, first bound as an AUTO schedule
	// (no participants forwarded, mirroring the degraded create path).
	taskID := seedScheduleTask(t, db, "TUBF", "s1", "u1")
	db.Create(&model.SummaryParticipant{TaskID: taskID, UserID: "danno", UserName: "Danno", Status: model.ParticipantAccepted})
	wc := scheduleReq(t, r, "u1", "s1", http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"scope": "task", "task_id": taskID, "interval_days": 1, "run_time": "09:00",
		"confirm_policy": 0, // force AUTO so the update is the conversion under test
	})
	if wc.Code != http.StatusOK {
		t.Fatalf("create auto schedule: %d %s", wc.Code, wc.Body.String())
	}
	var sched model.SummarySchedule
	db.Where("creator_id = ?", "u1").First(&sched)

	// Convert to CONFIRM via update WITHOUT forwarding participants.
	wu := scheduleReq(t, r, "u1", "s1", http.MethodPut, "/api/v1/summary-schedules/"+sid(sched.ID), map[string]interface{}{
		"scope": "task", "task_id": taskID, "confirm_policy": 1,
	})
	if wu.Code != http.StatusOK {
		t.Fatalf("update manual->confirm: %d %s", wu.Code, wu.Body.String())
	}

	cfg := loadV5Cfg(t, db, sched.ID)
	if cfg.FindParticipant("u1") == nil || cfg.FindParticipant("danno") == nil {
		t.Fatalf("BUG: update dropped collaborator Danno, got %+v", cfg.Participants)
	}
	if len(cfg.Participants) != 2 {
		t.Fatalf("roster must have creator + Danno, got %d: %+v", len(cfg.Participants), cfg.Participants)
	}
	if cfg.IsConfirmed("u1") || cfg.IsConfirmed("danno") {
		t.Errorf("manual->confirm conversion must reset everyone to unconfirmed, got %+v", cfg.Participants)
	}
}
