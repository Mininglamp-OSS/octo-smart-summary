//go:build cgo

package handler

import (
	"fmt"
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

func TestContentReadHandlerAllowlistAndAuthorization(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&model.SummaryTask{}, &model.SummaryParticipant{}, &model.SummaryResult{},
		&model.PersonalResult{}, &model.PersonalResultVersion{}, &model.SummaryGenerationRun{}); err != nil {
		t.Fatal(err)
	}
	task := model.SummaryTask{ID: 1, TaskNo: "ST-content", SpaceID: "s", CreatorID: "owner", SummaryMode: 1}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	h := NewContentReadHandler(db, "s")
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("space_id", c.GetHeader("X-Space-Id"))
		c.Set("user_id", c.GetHeader("Test-Actor"))
	})
	r.GET("/summaries/:id/contents", h.Catalog)
	r.GET("/summaries/:id/contents/:content_id/versions", h.Versions)
	target := service.ContentTarget{SpaceID: "s", TaskID: 1, Kind: service.ContentResult}
	tests := []struct {
		path, space, actor string
		want               int
	}{
		{"/summaries/1/contents", "s", "owner", 200},
		{"/summaries/1/contents", "s", "", 401},
		{"/summaries/1/contents", "", "owner", 404},
		{"/summaries/1/contents", "other", "owner", 404},
		{"/summaries/1/contents", "s", "outsider", 403},
		{"/summaries/bad/contents", "s", "owner", 400},
		{fmt.Sprintf("/summaries/1/contents/%s/versions?limit=0", target.ID()), "s", "owner", 400},
		{fmt.Sprintf("/summaries/1/contents/%s/versions?cursor=bad", target.ID()), "s", "owner", 400},
	}
	for _, test := range tests {
		req := httptest.NewRequest(http.MethodGet, test.path, nil)
		req.Header.Set("X-Space-Id", test.space)
		req.Header.Set("Test-Actor", test.actor)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != test.want {
			t.Errorf("%s space=%q actor=%q: got=%d want=%d body=%s", test.path, test.space, test.actor, rec.Code, test.want, rec.Body)
		}
	}
}
