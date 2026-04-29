package handler

import (
	"bytes"
	"encoding/json"
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

func setupBatchTestDBs(t *testing.T) (db *gorm.DB, imDB *gorm.DB) {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open summary db: %v", err)
	}
	db.AutoMigrate(&model.SummaryTask{}, &model.SummarySource{}, &model.SummaryParticipant{}, &model.SummaryEvent{})

	imDB, err = gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open im db: %v", err)
	}
	imDB.Exec("CREATE TABLE group_member (group_no TEXT NOT NULL, uid TEXT NOT NULL, is_deleted INTEGER DEFAULT 0)")

	return db, imDB
}

func setupBatchRouter(h *TaskHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.POST("/api/v1/summaries/batch-status", h.BatchStatus)
	return r
}

func doBatchRequest(r *gin.Engine, body interface{}, userID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/summaries/batch-status", &buf)
	req.Header.Set("Content-Type", "application/json")
	if userID != "" {
		req.Header.Set("Token", userID)
	}
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)
	return w
}

type batchResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		Tasks []batchStatusItem `json:"tasks"`
	} `json:"data"`
}

func parseBatchResponse(t *testing.T, w *httptest.ResponseRecorder) batchResponse {
	t.Helper()
	var resp batchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v\nbody: %s", err, w.Body.String())
	}
	return resp
}

func TestBatchStatus_Success(t *testing.T) {
	db, imDB := setupBatchTestDBs(t)
	h := NewTaskHandler(db, imDB, "")
	r := setupBatchRouter(h)

	tasks := make([]model.SummaryTask, 3)
	for i := range tasks {
		tasks[i] = model.SummaryTask{
			TaskNo:    "BATCH-" + string(rune('A'+i)),
			SpaceID:   "space1",
			CreatorID: "user1",
			Status:    model.StatusProcessing,
		}
		db.Create(&tasks[i])
	}

	db.Create(&model.SummaryEvent{TaskID: tasks[0].ID, Status: model.StatusProcessing, Progress: 50, CreatedAt: time.Now()})
	db.Create(&model.SummaryEvent{TaskID: tasks[1].ID, Status: model.StatusProcessing, Progress: 75, CreatedAt: time.Now()})

	ids := []int64{tasks[0].ID, tasks[1].ID, tasks[2].ID}
	w := doBatchRequest(r, map[string]interface{}{"task_ids": ids}, "user1")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	resp := parseBatchResponse(t, w)
	if len(resp.Data.Tasks) != 3 {
		t.Fatalf("expected 3 tasks, got %d", len(resp.Data.Tasks))
	}

	progressByID := make(map[int64]int)
	for _, item := range resp.Data.Tasks {
		progressByID[item.ID] = item.Progress
	}
	if progressByID[tasks[0].ID] != 50 {
		t.Errorf("task[0] progress: expected 50, got %d", progressByID[tasks[0].ID])
	}
	if progressByID[tasks[1].ID] != 75 {
		t.Errorf("task[1] progress: expected 75, got %d", progressByID[tasks[1].ID])
	}
	if progressByID[tasks[2].ID] != 0 {
		t.Errorf("task[2] progress: expected 0, got %d", progressByID[tasks[2].ID])
	}
}

func TestBatchStatus_Participant(t *testing.T) {
	db, imDB := setupBatchTestDBs(t)
	h := NewTaskHandler(db, imDB, "")
	r := setupBatchRouter(h)

	task := model.SummaryTask{
		TaskNo:    "BATCH-PART",
		SpaceID:   "space1",
		CreatorID: "other_user",
		Status:    model.StatusProcessing,
	}
	db.Create(&task)
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "participant_user", UserName: "P"})

	w := doBatchRequest(r, map[string]interface{}{"task_ids": []int64{task.ID}}, "participant_user")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := parseBatchResponse(t, w)
	if len(resp.Data.Tasks) != 1 {
		t.Errorf("expected 1 task for participant, got %d", len(resp.Data.Tasks))
	}
}

func TestBatchStatus_Unauthorized(t *testing.T) {
	db, imDB := setupBatchTestDBs(t)
	h := NewTaskHandler(db, imDB, "")
	r := setupBatchRouter(h)

	task := model.SummaryTask{
		TaskNo:    "BATCH-UNAUTH",
		SpaceID:   "space1",
		CreatorID: "owner",
		Status:    model.StatusCompleted,
	}
	db.Create(&task)

	w := doBatchRequest(r, map[string]interface{}{"task_ids": []int64{task.ID}}, "stranger")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := parseBatchResponse(t, w)
	if len(resp.Data.Tasks) != 0 {
		t.Errorf("expected 0 tasks for unauthorized user, got %d", len(resp.Data.Tasks))
	}
}

func TestBatchStatus_EmptyRequest(t *testing.T) {
	db, imDB := setupBatchTestDBs(t)
	h := NewTaskHandler(db, imDB, "")
	r := setupBatchRouter(h)

	w := doBatchRequest(r, map[string]interface{}{"task_ids": []int64{}}, "user1")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	resp := parseBatchResponse(t, w)
	if resp.Code != 40050 {
		t.Errorf("expected code 40050, got %d", resp.Code)
	}
}

func TestBatchStatus_ExceedsLimit(t *testing.T) {
	db, imDB := setupBatchTestDBs(t)
	h := NewTaskHandler(db, imDB, "")
	r := setupBatchRouter(h)

	ids := make([]int64, 51)
	for i := range ids {
		ids[i] = int64(i + 1)
	}

	w := doBatchRequest(r, map[string]interface{}{"task_ids": ids}, "user1")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	resp := parseBatchResponse(t, w)
	if resp.Code != 40051 {
		t.Errorf("expected code 40051, got %d", resp.Code)
	}
}

func TestBatchStatus_DuplicateIDs(t *testing.T) {
	db, imDB := setupBatchTestDBs(t)
	h := NewTaskHandler(db, imDB, "")
	r := setupBatchRouter(h)

	task1 := model.SummaryTask{TaskNo: "BATCH-D1", SpaceID: "space1", CreatorID: "user1", Status: model.StatusCompleted}
	task2 := model.SummaryTask{TaskNo: "BATCH-D2", SpaceID: "space1", CreatorID: "user1", Status: model.StatusCompleted}
	db.Create(&task1)
	db.Create(&task2)

	ids := []int64{task1.ID, task1.ID, task2.ID, task2.ID}
	w := doBatchRequest(r, map[string]interface{}{"task_ids": ids}, "user1")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := parseBatchResponse(t, w)
	if len(resp.Data.Tasks) != 2 {
		t.Errorf("expected 2 deduped tasks, got %d", len(resp.Data.Tasks))
	}
}

func TestBatchStatus_DeletedTasks(t *testing.T) {
	db, imDB := setupBatchTestDBs(t)
	h := NewTaskHandler(db, imDB, "")
	r := setupBatchRouter(h)

	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:    "BATCH-DEL",
		SpaceID:   "space1",
		CreatorID: "user1",
		Status:    -1,
		DeletedAt: &now,
	}
	db.Create(&task)

	w := doBatchRequest(r, map[string]interface{}{"task_ids": []int64{task.ID}}, "user1")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := parseBatchResponse(t, w)
	if len(resp.Data.Tasks) != 0 {
		t.Errorf("expected 0 tasks (deleted), got %d", len(resp.Data.Tasks))
	}
}

func TestBatchStatus_DBError(t *testing.T) {
	db, imDB := setupBatchTestDBs(t)
	h := NewTaskHandler(db, imDB, "")
	r := setupBatchRouter(h)

	// Drop the table to cause a DB error
	db.Exec("DROP TABLE summary_task")

	w := doBatchRequest(r, map[string]interface{}{"task_ids": []int64{1, 2, 3}}, "user1")

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	resp := parseBatchResponse(t, w)
	if resp.Code != 50000 {
		t.Errorf("expected code 50000, got %d", resp.Code)
	}
}
