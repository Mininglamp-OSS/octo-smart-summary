//go:build cgo

package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestContentCommandsRequireSeparateGateAndRevision(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&model.SummaryTask{}, &model.SummaryParticipant{}, &model.PersonalResult{},
		&model.PersonalResultVersion{}, &model.SummaryGenerationRun{}, &model.SummaryContentAudit{}); err != nil {
		t.Fatal(err)
	}
	for _, row := range []any{
		&model.SummaryTask{ID: 1, TaskNo: "cmd-test", SpaceID: "s", CreatorID: "owner", SummaryMode: model.ModeByPerson, Status: model.StatusCompleted},
		&model.SummaryParticipant{ID: 1, TaskID: 1, UserID: "owner", Status: model.ParticipantSubmitted},
		&model.PersonalResult{TaskID: 1, ParticipantRefID: 1, UserID: "owner", Content: "legacy", WorkerStatus: model.PersonalStatusCompleted},
	} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	makeRouter := func(writeSpaces string) *gin.Engine {
		r := gin.New()
		r.Use(func(c *gin.Context) {
			c.Set("space_id", c.GetHeader("X-Space-Id"))
			c.Set("user_id", c.GetHeader("Test-Actor"))
		})
		read := NewContentReadHandler(db, "s")
		write := NewContentCommandHandler(db, writeSpaces)
		r.GET("/summaries/:id/contents", read.Catalog)
		r.POST("/summaries/:id/contents/:content_id/edit", write.Edit)
		return r
	}
	request := func(r *gin.Engine, method, path, actor string, body any) *httptest.ResponseRecorder {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(encoded))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Space-Id", "s")
		req.Header.Set("Test-Actor", actor)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	r := makeRouter("")
	read := request(r, http.MethodGet, "/summaries/1/contents", "owner", nil)
	if read.Code != 200 {
		t.Fatalf("catalog: %d %s", read.Code, read.Body)
	}
	var catalog struct {
		Data service.FormalContentCatalog `json:"data"`
	}
	if err := json.Unmarshal(read.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	content := catalog.Data.Contents[0]
	path := "/summaries/1/contents/" + content.ContentID + "/edit"
	body := service.EditContentRequest{Content: "edited",
		ContentBaseline: service.ContentBaseline{ExpectedCurrentVersionID: content.CurrentVersion.VersionID, ExpectedContentRevision: content.ContentRevision}}
	if rec := request(r, http.MethodPost, path, "owner", body); rec.Code != 404 {
		t.Fatalf("read gate enabled a command: %d %s", rec.Code, rec.Body)
	}
	r = makeRouter("s")
	for _, test := range []struct {
		actor string
		body  any
		want  int
	}{
		{"", body, 401},
		{"outsider", body, 403},
		{"owner", map[string]any{"content": "missing baseline"}, 400},
		{"owner", body, 200},
		{"owner", body, 409},
	} {
		rec := request(r, http.MethodPost, path, test.actor, test.body)
		if rec.Code != test.want {
			t.Fatalf("command actor=%q: want=%d got=%d %s", test.actor, test.want, rec.Code, rec.Body)
		}
	}
	var versions int64
	if err := db.Model(&model.PersonalResultVersion{}).Count(&versions).Error; err != nil || versions != 1 {
		t.Fatalf("HTTP edit materialized more than V1: %d %v", versions, err)
	}
}
