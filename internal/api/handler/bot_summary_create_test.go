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

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupBotCreateTest(t *testing.T) (*TaskHandler, *gin.Engine, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.SummaryTask{}, &model.SummarySource{}, &model.SummaryParticipant{}, &model.PersonalResult{}, &model.SummaryBotCreateIdempotency{}); err != nil {
		t.Fatal(err)
	}
	imDB, err := gorm.Open(sqlite.Open("file:"+t.Name()+"-im?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`CREATE TABLE "group" (group_no TEXT, name TEXT, space_id TEXT, status INTEGER, updated_at DATETIME)`,
		`CREATE TABLE group_member (group_no TEXT, uid TEXT, is_deleted INTEGER)`,
		`CREATE TABLE conversation_extra (channel_id TEXT, uid TEXT, channel_type INTEGER, updated_at DATETIME)`,
		`CREATE TABLE thread (group_no TEXT, short_id TEXT, name TEXT, status INTEGER, updated_at DATETIME)`,
	}
	for _, statement := range statements {
		if err := imDB.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	imDB.Exec(`INSERT INTO "group" VALUES (?, ?, ?, 1, ?), (?, ?, ?, 1, ?)`, "group-a", "A", "space-a", now, "group-b", "B", "space-b", now)
	imDB.Exec(`INSERT INTO group_member VALUES (?, ?, 0), (?, ?, 0)`, "group-a", "owner", "group-b", "owner")

	h := NewTaskHandler(db, imDB, "")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("user_id", "owner")
		c.Set("bot_id", "bot-1")
		c.Set("space_id", "space-a")
		c.Set("bot_request", true)
		c.Next()
	})
	r.POST("/api/v1/bot/summaries", h.CreateBotSummary)
	return h, r, db
}

func botCreateBody(sourceID string) []byte {
	start := time.Now().Add(-time.Hour).Format(time.RFC3339)
	end := time.Now().Format(time.RFC3339)
	return []byte(fmt.Sprintf(`{"title":"test","time_range":{"start":%q,"end":%q},"sources":[{"source_type":1,"source_id":%q}]}`, start, end, sourceID))
}

func requestBotCreate(r *gin.Engine, key string, body []byte) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/bot/summaries", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	r.ServeHTTP(w, req)
	return w
}

func TestCreateBotSummaryFeatureDisabled(t *testing.T) {
	t.Setenv("BOT_SUMMARY_CREATE_ENABLED", "false")
	_, r, _ := setupBotCreateTest(t)
	w := requestBotCreate(r, "key-1", botCreateBody("group-a"))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestCreateBotSummaryRejectsUnknownUID(t *testing.T) {
	t.Setenv("BOT_SUMMARY_CREATE_ENABLED", "true")
	_, r, _ := setupBotCreateTest(t)
	body := bytes.TrimSuffix(botCreateBody("group-a"), []byte("}"))
	body = append(body, []byte(`,"uid":"other"}`)...)
	w := requestBotCreate(r, "key-1", body)
	if w.Code != http.StatusBadRequest || !bytes.Contains(w.Body.Bytes(), []byte(`"code":40004`)) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestCreateBotSummaryRejectsCrossSpaceSource(t *testing.T) {
	t.Setenv("BOT_SUMMARY_CREATE_ENABLED", "true")
	_, r, _ := setupBotCreateTest(t)
	w := requestBotCreate(r, "key-1", botCreateBody("group-b"))
	if w.Code != http.StatusForbidden || !bytes.Contains(w.Body.Bytes(), []byte(`"code":40302`)) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestCreateBotSummaryIdempotent(t *testing.T) {
	t.Setenv("BOT_SUMMARY_CREATE_ENABLED", "true")
	_, r, db := setupBotCreateTest(t)
	first := requestBotCreate(r, "stable-key", botCreateBody("group-a"))
	second := requestBotCreate(r, "stable-key", botCreateBody("group-a"))
	if first.Code != http.StatusCreated || second.Code != http.StatusOK {
		t.Fatalf("first=%d %s second=%d %s", first.Code, first.Body.String(), second.Code, second.Body.String())
	}
	var firstResp, secondResp struct {
		Data struct {
			TaskID int64 `json:"task_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstResp); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondResp); err != nil {
		t.Fatal(err)
	}
	if firstResp.Data.TaskID == 0 || firstResp.Data.TaskID != secondResp.Data.TaskID {
		t.Fatalf("task ids differ: first=%d second=%d", firstResp.Data.TaskID, secondResp.Data.TaskID)
	}
	var count int64
	db.Model(&model.SummaryTask{}).Count(&count)
	if count != 1 {
		t.Fatalf("task count=%d want 1", count)
	}
	var task model.SummaryTask
	db.First(&task)
	if task.CreatorID != "owner" || task.CreatorBotID != "bot-1" || task.TriggerType != model.TriggerBot || task.SpaceID != "space-a" {
		t.Fatalf("unexpected task identity: %+v", task)
	}
}
