package handler

import (
	"bytes"
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

func setupOriginTestDB(t *testing.T) (db *gorm.DB, imDB *gorm.DB) {
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
	)
	imDB, err = gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open im db: %v", err)
	}
	imDB.Exec("CREATE TABLE group_member (group_no TEXT NOT NULL, uid TEXT NOT NULL, is_deleted INTEGER DEFAULT 0)")
	return db, imDB
}

func setupOriginRouter(h *TaskHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.POST("/api/v1/summaries", h.CreateSummary)
	r.GET("/api/v1/summaries", h.ListSummaries)
	r.GET("/api/v1/summaries/:id", h.GetSummary)
	r.GET("/api/v1/summary-templates", h.GetTemplates)
	return r
}

func doCreateRequest(r *gin.Engine, body interface{}, userID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	var buf bytes.Buffer
	json.NewEncoder(&buf).Encode(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/summaries", &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Token", userID)
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)
	return w
}

func TestCreateSummary_WithOriginChannel(t *testing.T) {
	db, imDB := setupOriginTestDB(t)
	h := NewTaskHandler(db, imDB, "")
	r := setupOriginRouter(h)

	body := map[string]interface{}{
		"title":               "test",
		"origin_channel_id":   "group_123",
		"origin_channel_type": 1,
		"time_range": map[string]interface{}{
			"start": time.Now().Add(-24 * time.Hour).Format(time.RFC3339),
			"end":   time.Now().Format(time.RFC3339),
		},
	}
	w := doCreateRequest(r, body, "user1")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	data := resp["data"].(map[string]interface{})
	taskID := int64(data["task_id"].(float64))

	var task model.SummaryTask
	db.First(&task, taskID)
	if task.OriginChannelID != "group_123" {
		t.Errorf("origin_channel_id: want group_123, got %s", task.OriginChannelID)
	}
	if task.OriginChannelType != 1 {
		t.Errorf("origin_channel_type: want 1, got %d", task.OriginChannelType)
	}

	// Verify auto-fill source was created
	var sources []model.SummarySource
	db.Where("task_id = ?", taskID).Find(&sources)
	if len(sources) != 1 {
		t.Fatalf("expected 1 source (auto-filled), got %d", len(sources))
	}
	if sources[0].SourceType != 1 {
		t.Errorf("source_type: want 1, got %d", sources[0].SourceType)
	}
	if sources[0].SourceID != "group_123" {
		t.Errorf("source_id: want group_123, got %s", sources[0].SourceID)
	}
}

func TestCreateSummary_OriginChannelValidation_InvalidType(t *testing.T) {
	db, imDB := setupOriginTestDB(t)
	h := NewTaskHandler(db, imDB, "")
	r := setupOriginRouter(h)

	body := map[string]interface{}{
		"title":               "test",
		"origin_channel_id":   "group_123",
		"origin_channel_type": 5,
		"time_range": map[string]interface{}{
			"start": time.Now().Add(-24 * time.Hour).Format(time.RFC3339),
			"end":   time.Now().Format(time.RFC3339),
		},
	}
	w := doCreateRequest(r, body, "user1")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if int(resp["code"].(float64)) != 40001 {
		t.Errorf("expected code 40001, got %v", resp["code"])
	}
}

func TestCreateSummary_OriginChannelValidation_TypeWithoutID(t *testing.T) {
	db, imDB := setupOriginTestDB(t)
	h := NewTaskHandler(db, imDB, "")
	r := setupOriginRouter(h)

	body := map[string]interface{}{
		"title":               "test",
		"origin_channel_id":   "",
		"origin_channel_type": 2,
		"time_range": map[string]interface{}{
			"start": time.Now().Add(-24 * time.Hour).Format(time.RFC3339),
			"end":   time.Now().Format(time.RFC3339),
		},
	}
	w := doCreateRequest(r, body, "user1")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if int(resp["code"].(float64)) != 40001 {
		t.Errorf("expected code 40001, got %v", resp["code"])
	}
}

func TestCreateSummary_OriginChannel_NoAutoFillWhenSourcesProvided(t *testing.T) {
	db, imDB := setupOriginTestDB(t)
	h := NewTaskHandler(db, imDB, "")
	r := setupOriginRouter(h)

	body := map[string]interface{}{
		"title":               "test",
		"origin_channel_id":   "group_123",
		"origin_channel_type": 1,
		"sources": []map[string]interface{}{
			{"source_type": 1, "source_id": "group_456"},
		},
		"time_range": map[string]interface{}{
			"start": time.Now().Add(-24 * time.Hour).Format(time.RFC3339),
			"end":   time.Now().Format(time.RFC3339),
		},
	}
	w := doCreateRequest(r, body, "user1")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	data := resp["data"].(map[string]interface{})
	taskID := int64(data["task_id"].(float64))

	var sources []model.SummarySource
	db.Where("task_id = ?", taskID).Find(&sources)
	if len(sources) != 1 {
		t.Fatalf("expected 1 source (user-provided), got %d", len(sources))
	}
	if sources[0].SourceID != "group_456" {
		t.Errorf("source_id should be user-provided group_456, got %s", sources[0].SourceID)
	}
}

func TestListSummaries_FilterByOriginChannelID(t *testing.T) {
	db, imDB := setupOriginTestDB(t)
	h := NewTaskHandler(db, imDB, "")
	r := setupOriginRouter(h)

	now := time.Now().UTC()
	task1 := model.SummaryTask{
		TaskNo:            "LST-001",
		SpaceID:           "space1",
		CreatorID:         "user1",
		SummaryMode:       model.ModeByPerson,
		Status:            model.StatusCompleted,
		OriginChannelID:   "group_aaa",
		OriginChannelType: 1,
		TimeRangeStart:    now.Add(-24 * time.Hour),
		TimeRangeEnd:      now,
	}
	task2 := model.SummaryTask{
		TaskNo:            "LST-002",
		SpaceID:           "space1",
		CreatorID:         "user1",
		SummaryMode:       model.ModeByPerson,
		Status:            model.StatusCompleted,
		OriginChannelID:   "group_bbb",
		OriginChannelType: 1,
		TimeRangeStart:    now.Add(-24 * time.Hour),
		TimeRangeEnd:      now,
	}
	db.Create(&task1)
	db.Create(&task2)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/summaries?origin_channel_id=group_aaa", nil)
	req.Header.Set("Token", "user1")
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	data := resp["data"].(map[string]interface{})
	total := int(data["total"].(float64))
	if total != 1 {
		t.Errorf("expected 1 result, got %d", total)
	}
	items := data["items"].([]interface{})
	item := items[0].(map[string]interface{})
	if item["origin_channel_id"] != "group_aaa" {
		t.Errorf("expected origin_channel_id=group_aaa, got %v", item["origin_channel_id"])
	}
	if int(item["origin_channel_type"].(float64)) != 1 {
		t.Errorf("expected origin_channel_type=1, got %v", item["origin_channel_type"])
	}
}

func TestGetSummary_IncludesOriginFields(t *testing.T) {
	db, imDB := setupOriginTestDB(t)
	h := NewTaskHandler(db, imDB, "")
	r := setupOriginRouter(h)

	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:            "GET-001",
		SpaceID:           "space1",
		CreatorID:         "user1",
		SummaryMode:       model.ModeByPerson,
		Status:            model.StatusCompleted,
		OriginChannelID:   "thread_xyz",
		OriginChannelType: 2,
		TimeRangeStart:    now.Add(-24 * time.Hour),
		TimeRangeEnd:      now,
	}
	db.Create(&task)
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "user1", UserName: "U1"})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/summaries/%d", task.ID), nil)
	req.Header.Set("Token", "user1")
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	data := resp["data"].(map[string]interface{})

	if data["origin_channel_id"] != "thread_xyz" {
		t.Errorf("origin_channel_id: want thread_xyz, got %v", data["origin_channel_id"])
	}
	if int(data["origin_channel_type"].(float64)) != 2 {
		t.Errorf("origin_channel_type: want 2, got %v", data["origin_channel_type"])
	}
}

func TestGetTemplates_ReturnsCorrectStructure(t *testing.T) {
	db, imDB := setupOriginTestDB(t)
	h := NewTaskHandler(db, imDB, "")
	r := setupOriginRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/summary-templates", nil)
	req.Header.Set("Token", "user1")
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	data := resp["data"].(map[string]interface{})
	templates := data["templates"].([]interface{})

	if len(templates) != 4 {
		t.Fatalf("expected 4 templates, got %d", len(templates))
	}

	expectedIDs := []string{"project_progress", "task_tracking", "weekly_report", "chat_content"}
	for i, tmpl := range templates {
		m := tmpl.(map[string]interface{})
		if m["id"] != expectedIDs[i] {
			t.Errorf("template[%d] id: want %s, got %v", i, expectedIDs[i], m["id"])
		}
		if m["label"] == nil || m["label"] == "" {
			t.Errorf("template[%d] label is empty", i)
		}
		if m["icon"] == nil || m["icon"] == "" {
			t.Errorf("template[%d] icon is empty", i)
		}
		if m["pattern"] == nil || m["pattern"] == "" {
			t.Errorf("template[%d] pattern is empty", i)
		}
		if m["type"] == nil || m["type"] == "" {
			t.Errorf("template[%d] type is empty", i)
		}
	}

	// Verify parameterized template has placeholders
	taskTracking := templates[1].(map[string]interface{})
	if taskTracking["type"] != "parameterized" {
		t.Errorf("task_tracking type: want parameterized, got %v", taskTracking["type"])
	}
	placeholders := taskTracking["placeholders"].([]interface{})
	if len(placeholders) != 1 {
		t.Fatalf("task_tracking placeholders: want 1, got %d", len(placeholders))
	}
	ph := placeholders[0].(map[string]interface{})
	if ph["key"] != "task_name" {
		t.Errorf("placeholder key: want task_name, got %v", ph["key"])
	}
}
