//go:build cgo

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

func setupShareTest(t *testing.T) (*gorm.DB, *gorm.DB, *gin.Engine, int64) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&model.SummaryTask{}, &model.SummarySource{}, &model.SummaryParticipant{},
		&model.SummaryResult{}, &model.PersonalResult{},
		&model.SummaryShareSnapshot{}, &model.SummaryShareGrant{},
	); err != nil {
		t.Fatal(err)
	}
	imDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	imDB.Exec(`CREATE TABLE space (space_id TEXT, status INTEGER)`)
	imDB.Exec(`CREATE TABLE space_member (space_id TEXT, uid TEXT, status INTEGER)`)
	imDB.Exec(`CREATE TABLE "group" (group_no TEXT, space_id TEXT, status INTEGER)`)
	imDB.Exec(`CREATE TABLE group_member (group_no TEXT, uid TEXT, is_deleted INTEGER)`)
	imDB.Exec(`INSERT INTO space VALUES ('space1',1),('space2',1)`)
	imDB.Exec(`INSERT INTO space_member VALUES ('space1','creator',1),('space1','reader',1),('space1','outsider',1),('space1','peer',1)`)
	imDB.Exec(`INSERT INTO "group" VALUES ('group1','space1',1)`)
	imDB.Exec(`INSERT INTO group_member VALUES ('group1','creator',0),('group1','reader',0)`)

	now := time.Now()
	task := model.SummaryTask{TaskNo: "ST-share-1", SpaceID: "space1", CreatorID: "creator", Title: "Weekly review", SummaryMode: 1, Status: model.StatusCompleted, TimeRangeStart: now.Add(-24 * time.Hour), TimeRangeEnd: now, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	db.Create(&model.SummarySource{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "source", SourceName: "Source group", CreatedAt: now})
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "creator", UserName: "Creator", Status: model.ParticipantAccepted, CreatedAt: now, UpdatedAt: now})
	result := model.SummaryResult{TaskID: task.ID, Content: "## Result\nGrowth [1] and [123](https://example.com).", TotalMsgCount: 38, Version: 2, GeneratedAt: now, CreatedAt: now, UpdatedAt: now}
	result.SetCitations([]model.Citation{{Index: 1}})
	db.Create(&result)

	h := NewShareHandler(db, imDB)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.POST("/api/v1/summaries/:id/shares", h.Create)
	r.GET("/api/v1/summary-shares/:share_id", h.Get)
	r.DELETE("/api/v1/summary-shares/:share_id", h.Revoke)
	return db, imDB, r, task.ID
}

func shareRequest(t *testing.T, r *gin.Engine, method, path, uid, space string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Token", uid)
	req.Header.Set("X-Space-Id", space)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func decodeShareID(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var envelope struct {
		Data struct {
			Grants []struct {
				ShareID string `json:"share_id"`
			} `json:"grants"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Data.Grants) != 1 {
		t.Fatalf("grants=%s", w.Body.String())
	}
	return envelope.Data.Grants[0].ShareID
}

func decodeSourceAccessible(t *testing.T, w *httptest.ResponseRecorder) bool {
	t.Helper()
	var envelope struct {
		Data struct {
			SourceAccessible bool `json:"source_accessible"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Data.SourceAccessible
}

func TestSummaryShare_GroupGrantAndIdempotency(t *testing.T) {
	db, imDB, r, taskID := setupShareTest(t)
	body := gin.H{"idempotency_key": "share-request-1", "targets": []gin.H{{"channel_id": "group1", "channel_type": model.ChannelTypeGroup}}}
	w := shareRequest(t, r, http.MethodPost, "/api/v1/summaries/ST-share-1/shares", "creator", "space1", body)
	if w.Code != http.StatusOK {
		t.Fatalf("create=%d %s", w.Code, w.Body.String())
	}
	shareID := decodeShareID(t, w)
	w = shareRequest(t, r, http.MethodPost, "/api/v1/summaries/ST-share-1/shares", "creator", "space1", body)
	if got := decodeShareID(t, w); got != shareID {
		t.Fatalf("idempotency changed share id: %s != %s", got, shareID)
	}
	var snapshots, grants int64
	db.Model(&model.SummaryShareSnapshot{}).Count(&snapshots)
	db.Model(&model.SummaryShareGrant{}).Count(&grants)
	if snapshots != 1 || grants != 1 {
		t.Fatalf("snapshots=%d grants=%d", snapshots, grants)
	}

	w = shareRequest(t, r, http.MethodGet, "/api/v1/summary-shares/"+shareID, "reader", "space1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("reader=%d %s", w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("Growth [1]")) || !bytes.Contains(w.Body.Bytes(), []byte("[123](https://example.com)")) {
		t.Fatalf("citation cleanup corrupted content: %s", w.Body.String())
	}
	if decodeSourceAccessible(t, w) {
		t.Fatal("non-participant reader must not access the original summary")
	}
	now := time.Now()
	db.Create(&model.SummaryParticipant{TaskID: taskID, UserID: "reader", UserName: "Reader", Status: model.ParticipantAccepted, CreatedAt: now, UpdatedAt: now})
	w = shareRequest(t, r, http.MethodGet, "/api/v1/summary-shares/"+shareID, "reader", "space1", nil)
	if w.Code != http.StatusOK || !decodeSourceAccessible(t, w) {
		t.Fatalf("participant reader source_accessible=%v response=%d %s", decodeSourceAccessible(t, w), w.Code, w.Body.String())
	}
	deletedAt := time.Now()
	db.Model(&model.SummaryTask{}).Where("id = ?", taskID).Update("deleted_at", deletedAt)
	w = shareRequest(t, r, http.MethodGet, "/api/v1/summary-shares/"+shareID, "reader", "space1", nil)
	if w.Code != http.StatusOK || decodeSourceAccessible(t, w) {
		t.Fatalf("deleted source must fall back to snapshot: %d %s", w.Code, w.Body.String())
	}
	db.Model(&model.SummaryTask{}).Where("id = ?", taskID).Update("deleted_at", nil)
	w = shareRequest(t, r, http.MethodGet, "/api/v1/summary-shares/"+shareID, "outsider", "space1", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("outsider=%d %s", w.Code, w.Body.String())
	}
	imDB.Exec(`UPDATE group_member SET is_deleted=1 WHERE group_no='group1' AND uid='creator'`)
	w = shareRequest(t, r, http.MethodGet, "/api/v1/summary-shares/"+shareID, "creator", "space1", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("departed creator=%d %s", w.Code, w.Body.String())
	}
	imDB.Exec(`UPDATE group_member SET is_deleted=0 WHERE group_no='group1' AND uid='creator'`)
	imDB.Exec(`UPDATE group_member SET is_deleted=1 WHERE group_no='group1' AND uid='reader'`)
	w = shareRequest(t, r, http.MethodGet, "/api/v1/summary-shares/"+shareID, "reader", "space1", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("departed reader=%d %s", w.Code, w.Body.String())
	}
	imDB.Exec(`UPDATE group_member SET is_deleted=0 WHERE group_no='group1' AND uid='reader'`)
	imDB.Exec(`UPDATE space SET status=0 WHERE space_id='space1'`)
	w = shareRequest(t, r, http.MethodGet, "/api/v1/summary-shares/"+shareID, "reader", "space1", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("deleted space reader=%d %s", w.Code, w.Body.String())
	}
}

func TestSummaryShare_DirectAndCrossSpace(t *testing.T) {
	_, _, r, _ := setupShareTest(t)
	body := gin.H{"idempotency_key": "share-request-dm", "targets": []gin.H{{"channel_id": "peer", "channel_type": model.ChannelTypeDM}}}
	w := shareRequest(t, r, http.MethodPost, "/api/v1/summaries/ST-share-1/shares", "creator", "space1", body)
	if w.Code != http.StatusOK {
		t.Fatalf("create=%d %s", w.Code, w.Body.String())
	}
	shareID := decodeShareID(t, w)
	w = shareRequest(t, r, http.MethodGet, "/api/v1/summary-shares/"+shareID, "peer", "space1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("peer=%d %s", w.Code, w.Body.String())
	}
	w = shareRequest(t, r, http.MethodGet, "/api/v1/summary-shares/"+shareID, "peer", "space2", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-space=%d %s", w.Code, w.Body.String())
	}
	w = shareRequest(t, r, http.MethodGet, "/api/v1/summary-shares/"+shareID, "outsider", "space1", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unrelated dm reader=%d %s", w.Code, w.Body.String())
	}
	w = shareRequest(t, r, http.MethodDelete, "/api/v1/summary-shares/"+shareID, "creator", "space1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("revoke=%d %s", w.Code, w.Body.String())
	}
	w = shareRequest(t, r, http.MethodGet, "/api/v1/summary-shares/"+shareID, "peer", "space1", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("revoked grant=%d %s", w.Code, w.Body.String())
	}
}
