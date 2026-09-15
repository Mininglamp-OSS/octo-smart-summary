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
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestPersonalRefinePreservesIsolatedNumericGroupsBothTransports(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, valid := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/valid=%t", stream, valid), func(t *testing.T) {
				content := "Budget [9,73]. ROI [88]."
				if !valid {
					content = "Missing [9,74]. ROI [88]."
				}
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						delta, _ := json.Marshal(map[string]interface{}{"choices": []interface{}{
							map[string]interface{}{"delta": map[string]string{"content": content}},
						}})
						fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", delta)
					} else {
						_ = json.NewEncoder(w).Encode(map[string]interface{}{"choices": []interface{}{
							map[string]interface{}{"message": map[string]string{"content": content}, "finish_reason": "stop"},
						}})
					}
				}))
				defer srv.Close()
				db := setupPersonalRefineDB(t)
				task, _, pr := seedScheduledMultiPersonPersonalTask(t, db)
				pr.Content = "Before [9][73][88]"
				pr.SetCitations([]model.Citation{{Index: 9}, {Index: 73}, {Index: 88}})
				if err := db.Save(&pr).Error; err != nil {
					t.Fatal(err)
				}
				h := NewPersonalHandler(db, "", nil)
				h.SetLLM(service.NewLLMClient(srv.URL, "test", "test", 5, 256, false, 5, nil))
				r := setupPersonalRefineRouter(h)
				path := fmt.Sprintf("/api/v1/summaries/%d/personal-refine", task.ID)
				if stream {
					r.POST("/api/v1/summaries/:id/personal-refine-stream", h.RefinePersonalSummaryStream)
					path += "-stream"
				}
				req := httptest.NewRequest("POST", path, strings.NewReader(`{"feedback":"adjust","base_version":1}`))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Token", "member_a")
				req.Header.Set("X-Space-Id", "space1")
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				var saved model.PersonalResult
				if err := db.First(&saved, pr.ID).Error; err != nil {
					t.Fatal(err)
				}
				var versions int64
				if err := db.Model(&model.PersonalResultVersion{}).Count(&versions).Error; err != nil {
					t.Fatal(err)
				}
				if w.Code != http.StatusOK || saved.Content != content || versions != 2 {
					t.Fatalf("status=%d body=%s content=%q versions=%d", w.Code, w.Body.String(), saved.Content, versions)
				}
			})
		}
	}
}

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

func TestRegeneratePersonalSummaryPreservesSelectedBaselineIdentity(t *testing.T) {
	db := setupPersonalRefineDB(t)
	task, _, pr := seedScheduledMultiPersonPersonalTask(t, db)
	old := model.PersonalResultVersion{TaskID: task.ID, ParticipantRefID: pr.ParticipantRefID, UserID: pr.UserID, Version: 1, Content: "selected older body"}
	newer := model.PersonalResultVersion{TaskID: task.ID, ParticipantRefID: pr.ParticipantRefID, UserID: pr.UserID, Version: 2, Content: "newest historical body"}
	if err := db.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&newer).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&pr).Updates(map[string]interface{}{"current_version_id": old.ID, "content": old.Content}).Error; err != nil {
		t.Fatal(err)
	}

	w := doPersonalRegenerateRequest(setupPersonalRefineRouter(NewPersonalHandler(db, "", nil)), task.ID, pr.UserID, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	db.First(&pr, pr.ID)
	if pr.CurrentVersionID == nil || *pr.CurrentVersionID != old.ID || pr.Content != "" {
		t.Fatalf("selected baseline identity lost during reset: current=%v content=%q", pr.CurrentVersionID, pr.Content)
	}
}

func TestPersonalEditAllowsCompletedMemberWhileTeamStillProcessing(t *testing.T) {
	db := setupPersonalRefineDB(t)
	task, _, pr := seedScheduledMultiPersonPersonalTask(t, db)
	if err := db.Model(&task).Update("status", model.StatusProcessing).Error; err != nil {
		t.Fatal(err)
	}
	w := doJSONRequest(setupPersonalEditRouter(NewPersonalHandler(db, "", nil)), "PUT",
		fmt.Sprintf("/api/v1/summaries/%d/personal-edit", task.ID), pr.UserID, map[string]interface{}{"content": "edited while teammate is running"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	db.First(&pr, pr.ID)
	if pr.Content != "edited while teammate is running" {
		t.Fatalf("edit was not persisted: %q", pr.Content)
	}
}
