//go:build cgo

package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupRegenerateDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.AutoMigrate(
		&model.SummaryTask{},
		&model.SummarySource{},
		&model.SummaryParticipant{},
		&model.PersonalResult{},
		&model.SummaryResult{},
		&model.SummaryChunk{},
	)
	return db
}

func setupRegenerateRouter(h *TaskHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.POST("/api/v1/summaries/:id/regenerate", h.Regenerate)
	return r
}

func seedCompletedTask(t *testing.T, db *gorm.DB) (taskID int64, participantID int64, prID int64) {
	t.Helper()
	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:      "TST-REGEN-001",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
		Title:       "原始提示词",
	}
	db.Create(&task)

	db.Create(&model.SummarySource{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "grp_abc"})

	participant := model.SummaryParticipant{
		TaskID:          task.ID,
		UserID:          "creator1",
		UserName:        "Creator",
		Status:          model.ParticipantCompleted,
		ConfirmedAt:     &now,
		WorkerStartedAt: &now,
	}
	db.Create(&participant)

	pr := model.PersonalResult{
		TaskID:           task.ID,
		ParticipantRefID: participant.ID,
		UserID:           "creator1",
		WorkerStatus:     model.PersonalStatusCompleted,
		Content:          "old personal content",
		CitationsJSON:    `[{"index":1}]`,
		MsgCount:         5,
		TotalTokenUsed:   100,
		ModelVersion:     "test-v1",
		SubmittedAt:      &now,
		GeneratedAt:      &now,
	}
	db.Create(&pr)
	db.Model(&participant).Update("personal_result_id", pr.ID)

	result := model.SummaryResult{
		TaskID:         task.ID,
		Content:        "old summary content",
		TotalMsgCount:  5,
		TotalTokenUsed: 100,
		ModelVersion:   "test-v1",
		Version:        1,
		GeneratedAt:    now,
	}
	db.Create(&result)

	db.Create(&model.SummaryChunk{
		TaskID:       task.ID,
		ChunkIndex:   0,
		ChunkSummary: "old chunk",
		TokenUsed:    50,
	})

	return task.ID, participant.ID, pr.ID
}

func TestRegenerate_ResetsAllAssociatedData(t *testing.T) {
	db := setupRegenerateDB(t)
	imDB, _ := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	imDB.Exec("CREATE TABLE group_member (group_no TEXT NOT NULL, uid TEXT NOT NULL, is_deleted INTEGER DEFAULT 0)")
	imDB.Exec("INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp_abc', 'creator1', 0)")

	taskID, participantID, prID := seedCompletedTask(t, db)
	h := NewTaskHandler(db, imDB, "")
	r := setupRegenerateRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/summaries/%d/regenerate", taskID), nil)
	req.Header.Set("Token", "creator1")
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	data := resp["data"].(map[string]interface{})
	if int(data["status"].(float64)) != model.StatusPending {
		t.Errorf("expected status %d, got %v", model.StatusPending, data["status"])
	}

	// Verify task status reset
	var task model.SummaryTask
	db.First(&task, taskID)
	if task.Status != model.StatusPending {
		t.Errorf("task status: want %d, got %d", model.StatusPending, task.Status)
	}
	if task.RetryCount != 0 {
		t.Errorf("task retry_count: want 0, got %d", task.RetryCount)
	}

	// Verify participant reset
	var participant model.SummaryParticipant
	db.First(&participant, participantID)
	if participant.Status != model.ParticipantPending {
		t.Errorf("participant status: want %d, got %d", model.ParticipantPending, participant.Status)
	}
	if participant.WorkerStartedAt != nil {
		t.Error("participant worker_started_at should be nil")
	}
	if participant.ConfirmedAt != nil {
		t.Error("participant confirmed_at should be nil")
	}
	if participant.PersonalResultID != nil {
		t.Error("participant personal_result_id should be nil")
	}

	// Verify PersonalResult reset
	var pr model.PersonalResult
	db.First(&pr, prID)
	if pr.WorkerStatus != model.PersonalStatusPending {
		t.Errorf("personal_result worker_status: want %d, got %d", model.PersonalStatusPending, pr.WorkerStatus)
	}
	if pr.Content != "" {
		t.Errorf("personal_result content: want empty, got %q", pr.Content)
	}
	if pr.CitationsJSON != "" {
		t.Errorf("personal_result citations_json: want empty, got %q", pr.CitationsJSON)
	}
	if pr.ErrorMessage != nil {
		t.Error("personal_result error_message should be nil")
	}
	if pr.GeneratedAt != nil {
		t.Error("personal_result generated_at should be nil")
	}
	if pr.SubmittedAt != nil {
		t.Error("personal_result submitted_at should be nil")
	}

	// Verify SummaryResult deleted
	var resultCount int64
	db.Model(&model.SummaryResult{}).Where("task_id = ?", taskID).Count(&resultCount)
	if resultCount != 0 {
		t.Errorf("summary_result count: want 0, got %d", resultCount)
	}

	// Verify SummaryChunk deleted
	var chunkCount int64
	db.Model(&model.SummaryChunk{}).Where("task_id = ?", taskID).Count(&chunkCount)
	if chunkCount != 0 {
		t.Errorf("summary_chunk count: want 0, got %d", chunkCount)
	}
}

func TestRegenerate_OnlyCreatorAllowed(t *testing.T) {
	db := setupRegenerateDB(t)
	imDB, _ := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	imDB.Exec("CREATE TABLE group_member (group_no TEXT NOT NULL, uid TEXT NOT NULL, is_deleted INTEGER DEFAULT 0)")
	imDB.Exec("INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp_abc', 'other_user', 0)")

	taskID, _, _ := seedCompletedTask(t, db)

	db.Create(&model.SummaryParticipant{TaskID: taskID, UserID: "other_user", UserName: "Other"})

	h := NewTaskHandler(db, imDB, "")
	r := setupRegenerateRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/summaries/%d/regenerate", taskID), nil)
	req.Header.Set("Token", "other_user")
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-creator, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRegenerate_RejectsInvalidStatus(t *testing.T) {
	db := setupRegenerateDB(t)
	imDB, _ := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	imDB.Exec("CREATE TABLE group_member (group_no TEXT NOT NULL, uid TEXT NOT NULL, is_deleted INTEGER DEFAULT 0)")
	imDB.Exec("INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp_abc', 'creator1', 0)")

	task := model.SummaryTask{
		TaskNo:      "TST-REGEN-002",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusProcessing,
	}
	db.Create(&task)
	db.Create(&model.SummarySource{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "grp_abc"})
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "creator1", UserName: "Creator"})

	h := NewTaskHandler(db, imDB, "")
	r := setupRegenerateRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/summaries/%d/regenerate", task.ID), nil)
	req.Header.Set("Token", "creator1")
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 for processing task, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRegenerate_AllowsFailedStatus(t *testing.T) {
	db := setupRegenerateDB(t)
	imDB, _ := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	imDB.Exec("CREATE TABLE group_member (group_no TEXT NOT NULL, uid TEXT NOT NULL, is_deleted INTEGER DEFAULT 0)")
	imDB.Exec("INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp_abc', 'creator1', 0)")

	errMsg := "something failed"
	task := model.SummaryTask{
		TaskNo:       "TST-REGEN-003",
		SpaceID:      "space1",
		CreatorID:    "creator1",
		SummaryMode:  model.ModeByPerson,
		Status:       model.StatusFailed,
		ErrorMessage: &errMsg,
		RetryCount:   3,
	}
	db.Create(&task)
	db.Create(&model.SummarySource{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "grp_abc"})

	participant := model.SummaryParticipant{
		TaskID:   task.ID,
		UserID:   "creator1",
		UserName: "Creator",
		Status:   model.ParticipantCompleted,
	}
	db.Create(&participant)

	failedErr := "pipeline error"
	pr := model.PersonalResult{
		TaskID:           task.ID,
		ParticipantRefID: participant.ID,
		UserID:           "creator1",
		WorkerStatus:     model.PersonalStatusFailed,
		ErrorMessage:     &failedErr,
	}
	db.Create(&pr)

	h := NewTaskHandler(db, imDB, "")
	r := setupRegenerateRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/summaries/%d/regenerate", task.ID), nil)
	req.Header.Set("Token", "creator1")
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify resets
	var updatedTask model.SummaryTask
	db.First(&updatedTask, task.ID)
	if updatedTask.Status != model.StatusPending {
		t.Errorf("task status: want %d, got %d", model.StatusPending, updatedTask.Status)
	}
	if updatedTask.RetryCount != 0 {
		t.Errorf("retry_count: want 0, got %d", updatedTask.RetryCount)
	}
	if updatedTask.ErrorMessage != nil {
		t.Error("error_message should be nil")
	}

	var updatedPR model.PersonalResult
	db.First(&updatedPR, pr.ID)
	if updatedPR.WorkerStatus != model.PersonalStatusPending {
		t.Errorf("worker_status: want %d, got %d", model.PersonalStatusPending, updatedPR.WorkerStatus)
	}
	if updatedPR.ErrorMessage != nil {
		t.Error("personal_result error_message should be nil")
	}
}

func TestRegenerate_TriggersWorker(t *testing.T) {
	db := setupRegenerateDB(t)
	imDB, _ := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	imDB.Exec("CREATE TABLE group_member (group_no TEXT NOT NULL, uid TEXT NOT NULL, is_deleted INTEGER DEFAULT 0)")
	imDB.Exec("INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp_abc', 'creator1', 0)")

	taskID, participantID, _ := seedCompletedTask(t, db)

	triggered := make(chan model.WorkerTriggerRequest, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req model.WorkerTriggerRequest
		json.NewDecoder(r.Body).Decode(&req)
		triggered <- req
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	h := NewTaskHandler(db, imDB, ts.URL)
	r := setupRegenerateRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/summaries/%d/regenerate", taskID), nil)
	req.Header.Set("Token", "creator1")
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	select {
	case got := <-triggered:
		if got.Type != "personal_summary" {
			t.Errorf("trigger type: want personal_summary, got %s", got.Type)
		}
		if got.TaskID != taskID {
			t.Errorf("trigger task_id: want %d, got %d", taskID, got.TaskID)
		}
		if got.ParticipantRefID != participantID {
			t.Errorf("trigger participant_ref_id: want %d, got %d", participantID, got.ParticipantRefID)
		}
	case <-time.After(2 * time.Second):
		t.Error("triggerWorker was not called within 2s")
	}
}

func TestRegenerate_WithNewTopic(t *testing.T) {
	db := setupRegenerateDB(t)
	imDB, _ := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	imDB.Exec("CREATE TABLE group_member (group_no TEXT NOT NULL, uid TEXT NOT NULL, is_deleted INTEGER DEFAULT 0)")
	imDB.Exec("INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp_abc', 'creator1', 0)")

	taskID, _, _ := seedCompletedTask(t, db)
	h := NewTaskHandler(db, imDB, "")
	r := setupRegenerateRouter(h)

	w := httptest.NewRecorder()
	body := bytes.NewBufferString(`{"topic":"新的提示词"}`)
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/summaries/%d/regenerate", taskID), body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Token", "creator1")
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	data := resp["data"].(map[string]interface{})
	if data["title"] != "新的提示词" {
		t.Errorf("response title: want 新的提示词, got %v", data["title"])
	}

	var task model.SummaryTask
	db.First(&task, taskID)
	if task.Title != "新的提示词" {
		t.Errorf("task title: want 新的提示词, got %q", task.Title)
	}
}

func TestRegenerate_WithEmptyTopic(t *testing.T) {
	db := setupRegenerateDB(t)
	imDB, _ := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	imDB.Exec("CREATE TABLE group_member (group_no TEXT NOT NULL, uid TEXT NOT NULL, is_deleted INTEGER DEFAULT 0)")
	imDB.Exec("INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp_abc', 'creator1', 0)")

	taskID, _, _ := seedCompletedTask(t, db)
	h := NewTaskHandler(db, imDB, "")
	r := setupRegenerateRouter(h)

	w := httptest.NewRecorder()
	body := bytes.NewBufferString(`{"topic":""}`)
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/summaries/%d/regenerate", taskID), body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Token", "creator1")
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var task model.SummaryTask
	db.First(&task, taskID)
	if task.Title != "原始提示词" {
		t.Errorf("task title should be unchanged: want 原始提示词, got %q", task.Title)
	}
}

func TestRegenerate_MalformedJSON(t *testing.T) {
	db := setupRegenerateDB(t)
	imDB, _ := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	imDB.Exec("CREATE TABLE group_member (group_no TEXT NOT NULL, uid TEXT NOT NULL, is_deleted INTEGER DEFAULT 0)")
	imDB.Exec("INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp_abc', 'creator1', 0)")

	taskID, _, _ := seedCompletedTask(t, db)
	h := NewTaskHandler(db, imDB, "")
	r := setupRegenerateRouter(h)

	w := httptest.NewRecorder()
	body := bytes.NewBufferString(`{"topic": "test`)
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/summaries/%d/regenerate", taskID), body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Token", "creator1")
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed JSON, got %d: %s", w.Code, w.Body.String())
	}

	// Verify task was NOT reset
	var task model.SummaryTask
	db.First(&task, taskID)
	if task.Status != model.StatusCompleted {
		t.Errorf("task status should remain completed, got %d", task.Status)
	}
}

func TestRegenerate_NonStringTopic(t *testing.T) {
	db := setupRegenerateDB(t)
	imDB, _ := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	imDB.Exec("CREATE TABLE group_member (group_no TEXT NOT NULL, uid TEXT NOT NULL, is_deleted INTEGER DEFAULT 0)")
	imDB.Exec("INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp_abc', 'creator1', 0)")

	taskID, _, _ := seedCompletedTask(t, db)
	h := NewTaskHandler(db, imDB, "")
	r := setupRegenerateRouter(h)

	w := httptest.NewRecorder()
	body := bytes.NewBufferString(`{"topic": 123}`)
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/summaries/%d/regenerate", taskID), body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Token", "creator1")
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-string topic, got %d: %s", w.Code, w.Body.String())
	}

	// Verify task was NOT reset
	var task model.SummaryTask
	db.First(&task, taskID)
	if task.Status != model.StatusCompleted {
		t.Errorf("task status should remain completed, got %d", task.Status)
	}
}

func TestRegenerate_WhitespaceOnlyTopic(t *testing.T) {
	db := setupRegenerateDB(t)
	imDB, _ := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	imDB.Exec("CREATE TABLE group_member (group_no TEXT NOT NULL, uid TEXT NOT NULL, is_deleted INTEGER DEFAULT 0)")
	imDB.Exec("INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp_abc', 'creator1', 0)")

	taskID, _, _ := seedCompletedTask(t, db)
	h := NewTaskHandler(db, imDB, "")
	r := setupRegenerateRouter(h)

	w := httptest.NewRecorder()
	body := bytes.NewBufferString(`{"topic": "   "}`)
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/summaries/%d/regenerate", taskID), body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Token", "creator1")
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Whitespace-only topic should NOT overwrite the title
	var task model.SummaryTask
	db.First(&task, taskID)
	if task.Title != "原始提示词" {
		t.Errorf("whitespace-only topic should not overwrite title: want 原始提示词, got %q", task.Title)
	}
}

func TestRegenerate_TopicTooLong(t *testing.T) {
	db := setupRegenerateDB(t)
	imDB, _ := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	imDB.Exec("CREATE TABLE group_member (group_no TEXT NOT NULL, uid TEXT NOT NULL, is_deleted INTEGER DEFAULT 0)")
	imDB.Exec("INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp_abc', 'creator1', 0)")

	taskID, _, _ := seedCompletedTask(t, db)
	h := NewTaskHandler(db, imDB, "")
	r := setupRegenerateRouter(h)

	longTopic := strings.Repeat("a", 1001)
	w := httptest.NewRecorder()
	body := bytes.NewBufferString(fmt.Sprintf(`{"topic":%q}`, longTopic))
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/summaries/%d/regenerate", taskID), body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Token", "creator1")
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for too-long topic, got %d: %s", w.Code, w.Body.String())
	}

	var task model.SummaryTask
	db.First(&task, taskID)
	if task.Title != "原始提示词" {
		t.Errorf("task title should be unchanged: want 原始提示词, got %q", task.Title)
	}
}
