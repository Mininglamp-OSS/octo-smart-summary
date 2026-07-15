//go:build cgo

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

func setupPersonalRefineDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(
		&model.SummarySchedule{},
		&model.SummaryTask{},
		&model.SummaryParticipant{},
		&model.PersonalResult{},
		&model.PersonalResultVersion{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func setupPersonalRefineRouter(h *PersonalHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.POST("/api/v1/summaries/:id/personal-refine", h.RefinePersonalSummary)
	r.POST("/api/v1/summaries/:id/personal-regenerate", h.RegeneratePersonalSummary)
	return r
}

func seedScheduledMultiPersonPersonalTask(t *testing.T, db *gorm.DB) (task model.SummaryTask, sched model.SummarySchedule, pr model.PersonalResult) {
	t.Helper()
	now := time.Now().UTC()
	sched = model.SummarySchedule{
		SpaceID:               "space1",
		CreatorID:             "creator1",
		Title:                 "shared schedule title",
		GenerationInstruction: "existing shared instruction",
		IsActive:              1,
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("seed schedule: %v", err)
	}
	task = model.SummaryTask{
		TaskNo:      "TST-PERSONAL-REFINE",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		Title:       "shared task title",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
		ScheduleID:  &sched.ID,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("seed task: %v", err)
	}
	creator := model.SummaryParticipant{TaskID: task.ID, UserID: "creator1", UserName: "Creator", Status: model.ParticipantCompleted}
	member := model.SummaryParticipant{TaskID: task.ID, UserID: "member_a", UserName: "Member A", Status: model.ParticipantCompleted}
	if err := db.Create(&creator).Error; err != nil {
		t.Fatalf("seed creator participant: %v", err)
	}
	if err := db.Create(&member).Error; err != nil {
		t.Fatalf("seed member participant: %v", err)
	}
	pr = model.PersonalResult{
		TaskID:           task.ID,
		ParticipantRefID: member.ID,
		UserID:           "member_a",
		WorkerStatus:     model.PersonalStatusCompleted,
		Content:          "old personal summary with [1]",
		GeneratedAt:      &now,
	}
	pr.SetCitations([]model.Citation{{Index: 1, Sender: "Member A", Content: "raw", SentAt: "2026-01-01T00:00:00Z", Source: "grp", ChannelID: "ch1", ChannelType: 2, MessageSeq: 1}})
	if err := db.Create(&pr).Error; err != nil {
		t.Fatalf("seed personal result: %v", err)
	}
	return task, sched, pr
}

func doPersonalRefineRequest(r *gin.Engine, taskID int64, userID string, body interface{}) *httptest.ResponseRecorder {
	var bodyBytes []byte
	if body != nil {
		bodyBytes, _ = json.Marshal(body)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/summaries/%d/personal-refine", taskID), bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	if userID != "" {
		req.Header.Set("Token", userID)
	}
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)
	return w
}

func doPersonalRegenerateRequest(r *gin.Engine, taskID int64, userID string, body interface{}) *httptest.ResponseRecorder {
	var bodyBytes []byte
	if body != nil {
		bodyBytes, _ = json.Marshal(body)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/summaries/%d/personal-regenerate", taskID), bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	if userID != "" {
		req.Header.Set("Token", userID)
	}
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)
	return w
}

func TestRefinePersonalSummary_RequiresBaseVersion(t *testing.T) {
	db := setupPersonalRefineDB(t)
	task, _, pr := seedScheduledMultiPersonPersonalTask(t, db)
	llm, closeLLM := newTestRefineLLM(t, "new personal summary")
	defer closeLLM()

	h := NewPersonalHandler(db, "", nil)
	h.SetLLM(llm)
	r := setupPersonalRefineRouter(h)

	w := doPersonalRefineRequest(r, task.ID, "member_a", map[string]interface{}{
		"feedback":       "make mine shorter",
		"base_result_id": pr.ID,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when base_version is missing, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRefinePersonalSummary_StaleBaseVersionConflicts(t *testing.T) {
	db := setupPersonalRefineDB(t)
	task, _, pr := seedScheduledMultiPersonPersonalTask(t, db)
	llm, closeLLM := newTestRefineLLM(t, "new personal summary")
	defer closeLLM()

	h := NewPersonalHandler(db, "", nil)
	h.SetLLM(llm)
	r := setupPersonalRefineRouter(h)

	w := doPersonalRefineRequest(r, task.ID, "member_a", map[string]interface{}{
		"feedback":       "make mine shorter",
		"base_result_id": pr.ID,
		"base_version":   2,
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for stale base_version, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRefinePersonalSummary_DoesNotMutateSharedScheduleInstruction(t *testing.T) {
	db := setupPersonalRefineDB(t)
	task, sched, pr := seedScheduledMultiPersonPersonalTask(t, db)
	llm, closeLLM := newTestRefineLLM(t, "new personal summary")
	defer closeLLM()

	h := NewPersonalHandler(db, "", nil)
	h.SetLLM(llm)
	r := setupPersonalRefineRouter(h)

	w := doPersonalRefineRequest(r, task.ID, "member_a", map[string]interface{}{
		"feedback":       "make my section terser",
		"base_result_id": pr.ID,
		"base_version":   1,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var got model.SummarySchedule
	if err := db.First(&got, sched.ID).Error; err != nil {
		t.Fatalf("load schedule: %v", err)
	}
	if got.GenerationInstruction != sched.GenerationInstruction {
		t.Fatalf("personal refine must not mutate shared schedule instruction, got %q want %q", got.GenerationInstruction, sched.GenerationInstruction)
	}
}

func TestRegeneratePersonalSummary_DoesNotMutateSharedTaskOrSchedule(t *testing.T) {
	db := setupPersonalRefineDB(t)
	task, sched, _ := seedScheduledMultiPersonPersonalTask(t, db)

	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalRefineRouter(h)

	w := doPersonalRegenerateRequest(r, task.ID, "member_a", map[string]interface{}{
		"topic": "private one-off personal topic",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var gotTask model.SummaryTask
	if err := db.First(&gotTask, task.ID).Error; err != nil {
		t.Fatalf("load task: %v", err)
	}
	if gotTask.Title != task.Title {
		t.Fatalf("personal regenerate must not mutate shared task title, got %q want %q", gotTask.Title, task.Title)
	}
	var gotSched model.SummarySchedule
	if err := db.First(&gotSched, sched.ID).Error; err != nil {
		t.Fatalf("load schedule: %v", err)
	}
	if gotSched.GenerationInstruction != sched.GenerationInstruction {
		t.Fatalf("personal regenerate must not mutate shared schedule instruction, got %q want %q", gotSched.GenerationInstruction, sched.GenerationInstruction)
	}
}
