//go:build cgo

package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/gin-gonic/gin"
)

func TestBotGetResultFallsBackToOwnersCompletedPersonalResult(t *testing.T) {
	db, imDB := setupTestDBs(t)
	task := model.SummaryTask{TaskNo: "BOT-PERSONAL-1", SpaceID: "space1", CreatorID: "owner1", SummaryMode: model.ModeByPerson, Status: model.StatusCompleted}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	participant := model.SummaryParticipant{TaskID: task.ID, UserID: "owner1", UserName: "Owner", Status: model.ParticipantCompleted}
	if err := db.Create(&participant).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	personal := model.PersonalResult{TaskID: task.ID, ParticipantRefID: participant.ID, UserID: "owner1", Content: "personal body [1]", WorkerStatus: model.PersonalStatusCompleted, GeneratedAt: &now}
	personal.SetCitations([]model.Citation{{Index: 1, Content: "evidence", ContextBefore: []model.ContextMsg{{Content: "hidden"}}}})
	if err := db.Create(&personal).Error; err != nil {
		t.Fatal(err)
	}

	h := NewTaskHandler(db, imDB, "")
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", "owner1")
		c.Set("space_id", "space1")
		c.Set("bot_request", true)
		c.Next()
	})
	r.GET("/api/v1/bot/summaries/:id/result", h.GetResult)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/bot/summaries/%d/result", task.ID), nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var envelope struct {
		Data struct {
			Content      string           `json:"content"`
			ResultSource string           `json:"result_source"`
			Citations    []model.Citation `json:"citations"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.Content != "personal body [1]" || envelope.Data.ResultSource != "personal" {
		t.Fatalf("unexpected result: %+v", envelope.Data)
	}
	if len(envelope.Data.Citations) != 1 || len(envelope.Data.Citations[0].ContextBefore) != 0 {
		t.Fatalf("citations not safely returned: %+v", envelope.Data.Citations)
	}
}

func TestHumanGetResultDoesNotUseBotPersonalFallback(t *testing.T) {
	db, imDB := setupTestDBs(t)
	task := model.SummaryTask{TaskNo: "HUMAN-PERSONAL-1", SpaceID: "space1", CreatorID: "owner1", SummaryMode: model.ModeByPerson, Status: model.StatusCompleted}
	_ = db.Create(&task).Error
	_ = db.Create(&model.PersonalResult{TaskID: task.ID, ParticipantRefID: 1, UserID: "owner1", Content: "personal body", WorkerStatus: model.PersonalStatusCompleted}).Error
	h := NewTaskHandler(db, imDB, "")
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.GET("/api/v1/summaries/:id/result", h.GetResult)
	w := doRequest(r, http.MethodGet, fmt.Sprintf("/api/v1/summaries/%d/result", task.ID), "owner1")
	if w.Code != http.StatusNotFound {
		t.Fatalf("human API behavior changed: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestBotGetResultDoesNotFallbackForNonPersonalMode(t *testing.T) {
	db, imDB := setupTestDBs(t)
	task := model.SummaryTask{TaskNo: "BOT-GROUP-1", SpaceID: "space1", CreatorID: "owner1", SummaryMode: 1, Status: model.StatusCompleted}
	_ = db.Create(&task).Error
	_ = db.Create(&model.PersonalResult{TaskID: task.ID, ParticipantRefID: 1, UserID: "owner1", Content: "anomalous personal body", WorkerStatus: model.PersonalStatusCompleted}).Error
	h := NewTaskHandler(db, imDB, "")
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", "owner1")
		c.Set("space_id", "space1")
		c.Set("bot_request", true)
		c.Next()
	})
	r.GET("/api/v1/bot/summaries/:id/result", h.GetResult)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/bot/summaries/%d/result", task.ID), nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("non-personal task used personal fallback: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestBotListKeywordMatchesTitleOrTopic(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	for _, task := range []model.SummaryTask{
		{TaskNo: "BOT-LIST-TITLE", SpaceID: "space1", CreatorID: "owner1", Title: "Launch review", Topic: "other", SummaryMode: model.ModeByPerson, Status: model.StatusCompleted},
		{TaskNo: "BOT-LIST-TOPIC", SpaceID: "space1", CreatorID: "owner1", Title: "Other", Topic: "Launch risks", SummaryMode: model.ModeByPerson, Status: model.StatusCompleted},
		{TaskNo: "BOT-LIST-MISS", SpaceID: "space1", CreatorID: "owner1", Title: "Unrelated", Topic: "Notes", SummaryMode: model.ModeByPerson, Status: model.StatusCompleted},
	} {
		if err := db.Create(&task).Error; err != nil {
			t.Fatal(err)
		}
	}
	h := NewTaskHandler(db, imDB, "")
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", "owner1")
		c.Set("space_id", "space1")
		c.Set("bot_request", true)
		c.Next()
	})
	r.GET("/api/v1/bot/summaries", h.ListSummaries)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/bot/summaries?keyword=Launch", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var envelope struct {
		Data struct {
			Total int `json:"total"`
			Items []struct {
				TaskNo string `json:"task_no"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.Total != 2 || len(envelope.Data.Items) != 2 {
		t.Fatalf("keyword should match title OR topic: %+v", envelope.Data)
	}
}
