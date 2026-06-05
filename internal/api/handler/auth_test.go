package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type mockTokenResolver struct{}

func (m *mockTokenResolver) ResolveUID(_ context.Context, token string) (string, error) {
	return token, nil
}

func setupTestDBs(t *testing.T) (db *gorm.DB, imDB *gorm.DB) {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open summary db: %v", err)
	}
	db.AutoMigrate(
		&model.SummaryTask{},
		&model.SummarySource{},
		&model.SummaryParticipant{},
		&model.SummarySchedule{},
		&model.SummaryResult{},
		&model.PersonalResult{},
	)

	imDB, err = gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open im db: %v", err)
	}
	imDB.Exec("CREATE TABLE group_member (group_no TEXT NOT NULL, uid TEXT NOT NULL, is_deleted INTEGER DEFAULT 0)")

	return db, imDB
}

func seedTask(t *testing.T, db *gorm.DB, imDB *gorm.DB) int64 {
	t.Helper()

	task := model.SummaryTask{
		TaskNo:      "TST-001",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
	}
	db.Create(&task)

	db.Create(&model.SummarySource{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "grp_abc"})
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "participant1", UserName: "P1"})

	imDB.Exec("INSERT INTO group_member (group_no, uid, is_deleted) VALUES (?, ?, 0)", "grp_abc", "groupmember1")
	imDB.Exec("INSERT INTO group_member (group_no, uid, is_deleted) VALUES (?, ?, 1)", "grp_abc", "deletedmember1")

	return task.ID
}

func setupRouter(h *TaskHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.GET("/api/v1/summaries/:id", h.GetSummary)
	r.DELETE("/api/v1/summaries/:id", h.DeleteSummary)
	r.POST("/api/v1/summaries/:id/cancel", h.CancelSummary)
	return r
}

func doRequest(r *gin.Engine, method, path, userID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	if userID != "" {
		req.Header.Set("Token", userID)
	}
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)
	return w
}

func TestAuthorizeTaskAccess_NonMember(t *testing.T) {
	db, imDB := setupTestDBs(t)
	taskID := seedTask(t, db, imDB)
	h := NewTaskHandler(db, imDB, "")
	r := setupRouter(h)

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", taskID), "stranger1")
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-member, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthorizeTaskAccess_NoAuth(t *testing.T) {
	db, imDB := setupTestDBs(t)
	taskID := seedTask(t, db, imDB)
	h := NewTaskHandler(db, imDB, "")
	r := setupRouter(h)

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", taskID), "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthenticated, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthorizeTaskAccess_Creator(t *testing.T) {
	db, imDB := setupTestDBs(t)
	taskID := seedTask(t, db, imDB)
	h := NewTaskHandler(db, imDB, "")
	r := setupRouter(h)

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", taskID), "creator1")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for creator, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthorizeTaskAccess_Participant(t *testing.T) {
	db, imDB := setupTestDBs(t)
	taskID := seedTask(t, db, imDB)
	h := NewTaskHandler(db, imDB, "")
	r := setupRouter(h)

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", taskID), "participant1")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for participant, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthorizeTaskAccess_GroupMemberDenied(t *testing.T) {
	db, imDB := setupTestDBs(t)
	taskID := seedTask(t, db, imDB)
	h := NewTaskHandler(db, imDB, "")
	r := setupRouter(h)

	// groupmember1 is only a source-group member, neither creator nor participant → 403
	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", taskID), "groupmember1")
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for source-group member, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthorizeTaskAccess_DeletedGroupMember(t *testing.T) {
	db, imDB := setupTestDBs(t)
	taskID := seedTask(t, db, imDB)
	h := NewTaskHandler(db, imDB, "")
	r := setupRouter(h)

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", taskID), "deletedmember1")
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for deleted group member, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeleteSummary_RequiresAuth(t *testing.T) {
	db, imDB := setupTestDBs(t)
	taskID := seedTask(t, db, imDB)
	h := NewTaskHandler(db, imDB, "")
	r := setupRouter(h)

	w := doRequest(r, "DELETE", fmt.Sprintf("/api/v1/summaries/%d", taskID), "stranger1")
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for delete by stranger, got %d: %s", w.Code, w.Body.String())
	}

	// Creator can delete
	w = doRequest(r, "DELETE", fmt.Sprintf("/api/v1/summaries/%d", taskID), "creator1")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for delete by creator, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeleteSummary_SoftDeletesExclusiveSchedule(t *testing.T) {
	db, imDB := setupTestDBs(t)
	now := time.Now().UTC()
	due := now.Add(-time.Minute)
	sched := model.SummarySchedule{
		SpaceID:       "space1",
		CreatorID:     "creator1",
		Title:         "solo",
		SummaryMode:   model.ModeByPerson,
		IntervalDays:  1,
		RunTime:       "09:00",
		TimeRangeType: 2,
		IsActive:      1,
		NextRunAt:     &due,
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	task := model.SummaryTask{
		TaskNo:         "TST-EXCLUSIVE",
		SpaceID:        "space1",
		CreatorID:      "creator1",
		SummaryMode:    model.ModeByPerson,
		Status:         model.StatusCompleted,
		TimeRangeStart: now,
		TimeRangeEnd:   now,
		ScheduleID:     &sched.ID,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}

	h := NewTaskHandler(db, imDB, "")
	r := setupRouter(h)
	w := doRequest(r, "DELETE", fmt.Sprintf("/api/v1/summaries/%d", task.ID), "creator1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var gotTask model.SummaryTask
	if err := db.First(&gotTask, task.ID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if gotTask.DeletedAt == nil || gotTask.Status != -1 {
		t.Fatalf("expected task soft deleted, status=%d deleted_at=%v", gotTask.Status, gotTask.DeletedAt)
	}

	var gotSched model.SummarySchedule
	if err := db.First(&gotSched, sched.ID).Error; err != nil {
		t.Fatalf("reload schedule: %v", err)
	}
	if gotSched.DeletedAt == nil {
		t.Fatalf("expected exclusive schedule soft deleted")
	}

	var dueCount int64
	if err := db.Model(&model.SummarySchedule{}).
		Where("is_active = 1 AND next_run_at <= ? AND deleted_at IS NULL", now).
		Count(&dueCount).Error; err != nil {
		t.Fatalf("count due schedules: %v", err)
	}
	if dueCount != 0 {
		t.Fatalf("expected worker scan predicate to exclude deleted schedule, count=%d", dueCount)
	}
}

func TestDeleteSummary_SoftDeletesScheduleEvenIfLegacyDataSharesIt(t *testing.T) {
	db, imDB := setupTestDBs(t)
	now := time.Now().UTC()
	due := now.Add(-time.Minute)
	sched := model.SummarySchedule{
		SpaceID:       "space1",
		CreatorID:     "creator1",
		Title:         "shared",
		SummaryMode:   model.ModeByPerson,
		IntervalDays:  1,
		RunTime:       "09:00",
		TimeRangeType: 2,
		IsActive:      1,
		NextRunAt:     &due,
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	taskA := model.SummaryTask{
		TaskNo:         "TST-SHARED-A",
		SpaceID:        "space1",
		CreatorID:      "creator1",
		SummaryMode:    model.ModeByPerson,
		Status:         model.StatusCompleted,
		TimeRangeStart: now,
		TimeRangeEnd:   now,
		ScheduleID:     &sched.ID,
	}
	taskB := model.SummaryTask{
		TaskNo:         "TST-SHARED-B",
		SpaceID:        "space1",
		CreatorID:      "creator1",
		SummaryMode:    model.ModeByPerson,
		Status:         model.StatusCompleted,
		TimeRangeStart: now,
		TimeRangeEnd:   now,
		ScheduleID:     &sched.ID,
	}
	if err := db.Create(&taskA).Error; err != nil {
		t.Fatalf("create taskA: %v", err)
	}
	if err := db.Create(&taskB).Error; err != nil {
		t.Fatalf("create taskB: %v", err)
	}

	h := NewTaskHandler(db, imDB, "")
	r := setupRouter(h)
	w := doRequest(r, "DELETE", fmt.Sprintf("/api/v1/summaries/%d", taskA.ID), "creator1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var gotSched model.SummarySchedule
	if err := db.First(&gotSched, sched.ID).Error; err != nil {
		t.Fatalf("reload schedule: %v", err)
	}
	if gotSched.DeletedAt == nil {
		t.Fatalf("expected schedule soft deleted even when legacy shared rows exist")
	}

	var gotTaskA, gotTaskB model.SummaryTask
	if err := db.First(&gotTaskA, taskA.ID).Error; err != nil {
		t.Fatalf("reload taskA: %v", err)
	}
	if err := db.First(&gotTaskB, taskB.ID).Error; err != nil {
		t.Fatalf("reload taskB: %v", err)
	}
	if gotTaskA.DeletedAt == nil {
		t.Fatalf("expected deleted task to be soft deleted")
	}
	if gotTaskB.ScheduleID == nil || *gotTaskB.ScheduleID != sched.ID {
		t.Fatalf("expected sibling task still points to legacy schedule %d, got %v", sched.ID, gotTaskB.ScheduleID)
	}
}

func TestCancelSummary_RequiresAuth(t *testing.T) {
	db, imDB := setupTestDBs(t)
	h := NewTaskHandler(db, imDB, "")

	// Create a pending task for cancellation
	task := model.SummaryTask{
		TaskNo:      "TST-002",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusPending,
	}
	db.Create(&task)

	r := setupRouter(h)

	w := doRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/cancel", task.ID), "stranger1")
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for cancel by stranger, got %d: %s", w.Code, w.Body.String())
	}

	w = doRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/cancel", task.ID), "creator1")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for cancel by creator, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetSummary_HidesDeletedOrInactiveSchedule(t *testing.T) {
	db, imDB := setupTestDBs(t)
	now := time.Now().UTC()
	nextRun := now.Add(time.Hour)
	sched := model.SummarySchedule{
		SpaceID:       "space1",
		CreatorID:     "creator1",
		Title:         "sched",
		SummaryMode:   model.ModeByPerson,
		IntervalDays:  1,
		RunTime:       "09:00",
		TimeRangeType: 2,
		IsActive:      1,
		NextRunAt:     &nextRun,
	}
	db.Create(&sched)
	task := model.SummaryTask{
		TaskNo:         "TST-SCHED-001",
		SpaceID:        "space1",
		CreatorID:      "creator1",
		SummaryMode:    model.ModeByPerson,
		Status:         model.StatusCompleted,
		TimeRangeStart: now.Add(-time.Hour),
		TimeRangeEnd:   now,
		ScheduleID:     &sched.ID,
	}
	db.Create(&task)

	h := NewTaskHandler(db, imDB, "")
	r := setupRouter(h)

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", task.ID), "creator1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Data["schedule_id"] == nil {
		t.Fatalf("expected active schedule_id to be exposed")
	}

	db.Model(&sched).Update("is_active", 0)
	w = doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", task.ID), "creator1")
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal inactive: %v", err)
	}
	if resp.Data["schedule_id"] != nil {
		t.Fatalf("expected inactive schedule_id hidden, got %v", resp.Data["schedule_id"])
	}

	deletedAt := time.Now().UTC()
	db.Model(&sched).Updates(map[string]interface{}{"is_active": 1, "deleted_at": &deletedAt})
	w = doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", task.ID), "creator1")
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal deleted: %v", err)
	}
	if resp.Data["schedule_id"] != nil {
		t.Fatalf("expected deleted schedule_id hidden, got %v", resp.Data["schedule_id"])
	}
}

func TestDeleteSummary_GroupMemberDenied(t *testing.T) {
	db, imDB := setupTestDBs(t)
	taskID := seedTask(t, db, imDB)
	h := NewTaskHandler(db, imDB, "")
	r := setupRouter(h)

	// Source-group member must not be able to delete another user's summary.
	w := doRequest(r, "DELETE", fmt.Sprintf("/api/v1/summaries/%d", taskID), "groupmember1")
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for delete by source-group member, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCancelSummary_GroupMemberDenied(t *testing.T) {
	db, imDB := setupTestDBs(t)
	h := NewTaskHandler(db, imDB, "")

	// Create a pending task whose source group contains groupmember1.
	task := model.SummaryTask{
		TaskNo:      "TST-003",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusPending,
	}
	db.Create(&task)
	db.Create(&model.SummarySource{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "grp_abc"})
	imDB.Exec("INSERT INTO group_member (group_no, uid, is_deleted) VALUES (?, ?, 0)", "grp_abc", "groupmember1")

	r := setupRouter(h)

	// Source-group member must not be able to cancel another user's summary.
	w := doRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/cancel", task.ID), "groupmember1")
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for cancel by source-group member, got %d: %s", w.Code, w.Body.String())
	}
}

// setupListTestDBs migrates the extra tables ListSummaries touches (summary_result)
// to avoid no-such-table noise during list rendering.
func setupListTestDBs(t *testing.T) (db *gorm.DB, imDB *gorm.DB) {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open summary db: %v", err)
	}
	db.AutoMigrate(&model.SummaryTask{}, &model.SummarySource{}, &model.SummaryParticipant{}, &model.SummaryResult{})

	imDB, err = gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open im db: %v", err)
	}
	imDB.Exec("CREATE TABLE group_member (group_no TEXT NOT NULL, uid TEXT NOT NULL, is_deleted INTEGER DEFAULT 0)")

	return db, imDB
}

func setupListRouter(h *TaskHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.GET("/api/v1/summaries", h.ListSummaries)
	return r
}

type listResponse struct {
	Code int `json:"code"`
	Data struct {
		Total int           `json:"total"`
		Items []interface{} `json:"items"`
	} `json:"data"`
}

func parseListResponse(t *testing.T, w *httptest.ResponseRecorder) listResponse {
	t.Helper()
	var resp listResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v\nbody: %s", err, w.Body.String())
	}
	return resp
}

func seedListTask(t *testing.T, db *gorm.DB, imDB *gorm.DB) {
	t.Helper()

	task := model.SummaryTask{
		TaskNo:      "TST-LIST-001",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
	}
	db.Create(&task)
	db.Create(&model.SummarySource{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "grp_abc"})
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "participant1", UserName: "P1"})
	imDB.Exec("INSERT INTO group_member (group_no, uid, is_deleted) VALUES (?, ?, 0)", "grp_abc", "groupmember1")
}

func TestListSummaries_GroupMemberSeesNothing(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	seedListTask(t, db, imDB)
	h := NewTaskHandler(db, imDB, "")
	r := setupListRouter(h)

	w := doRequest(r, "GET", "/api/v1/summaries", "groupmember1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := parseListResponse(t, w)
	if resp.Data.Total != 0 {
		t.Errorf("expected total 0 for source-group member, got %d", resp.Data.Total)
	}
	if len(resp.Data.Items) != 0 {
		t.Errorf("expected 0 items for source-group member, got %d", len(resp.Data.Items))
	}
}

func TestListSummaries_CreatorSeesTask(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	seedListTask(t, db, imDB)
	h := NewTaskHandler(db, imDB, "")
	r := setupListRouter(h)

	w := doRequest(r, "GET", "/api/v1/summaries", "creator1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := parseListResponse(t, w)
	if resp.Data.Total != 1 {
		t.Errorf("expected total 1 for creator, got %d", resp.Data.Total)
	}
}

func TestListSummaries_ParticipantSeesTask(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	seedListTask(t, db, imDB)
	h := NewTaskHandler(db, imDB, "")
	r := setupListRouter(h)

	w := doRequest(r, "GET", "/api/v1/summaries", "participant1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := parseListResponse(t, w)
	if resp.Data.Total != 1 {
		t.Errorf("expected total 1 for participant, got %d", resp.Data.Total)
	}
}
