package handler

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupMembersTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open summary db: %v", err)
	}
	db.AutoMigrate(&model.SummaryTask{}, &model.SummarySource{}, &model.SummaryParticipant{}, &model.PersonalResult{})
	return db
}

func setupMembersRouter(h *PersonalHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.GET("/api/v1/summaries/:id/members", h.GetMembers)
	return r
}

func seedMembersTask(t *testing.T, db *gorm.DB) int64 {
	t.Helper()

	task := model.SummaryTask{
		TaskNo:      "TST-MEMBERS-001",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
	}
	db.Create(&task)
	db.Create(&model.SummarySource{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "grp_abc"})
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "participant1", UserName: "P1"})
	return task.ID
}

func TestGetMembers_Creator(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID := seedMembersTask(t, db)
	h := NewPersonalHandler(db, "", nil)
	r := setupMembersRouter(h)

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d/members", taskID), "creator1")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for creator, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetMembers_Participant(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID := seedMembersTask(t, db)
	h := NewPersonalHandler(db, "", nil)
	r := setupMembersRouter(h)

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d/members", taskID), "participant1")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for participant, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetMembers_GroupMemberDenied(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID := seedMembersTask(t, db)
	h := NewPersonalHandler(db, "", nil)
	r := setupMembersRouter(h)

	// Source-group membership alone does not grant access to the member list.
	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d/members", taskID), "groupmember1")
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for source-group member, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetMembers_StrangerDenied(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID := seedMembersTask(t, db)
	h := NewPersonalHandler(db, "", nil)
	r := setupMembersRouter(h)

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d/members", taskID), "stranger1")
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for stranger, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetMembers_NoAuth(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID := seedMembersTask(t, db)
	h := NewPersonalHandler(db, "", nil)
	r := setupMembersRouter(h)

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d/members", taskID), "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthenticated, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetMembers_TaskNotFound(t *testing.T) {
	db := setupMembersTestDB(t)
	seedMembersTask(t, db)
	h := NewPersonalHandler(db, "", nil)
	r := setupMembersRouter(h)

	w := doRequest(r, "GET", "/api/v1/summaries/999999/members", "creator1")
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for missing task, got %d: %s", w.Code, w.Body.String())
	}
}
