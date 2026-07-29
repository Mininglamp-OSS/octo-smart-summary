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
		// space_member is what the DM authorization branch consults; without
		// this table the SQL for peer-in-space would fail on SQLite. Seed
		// two peers: one in the token's space, one outside it, so DM tests
		// can exercise both the allow and deny paths of issue #181 P1-1.
		`CREATE TABLE space_member (space_id TEXT, uid TEXT, status INTEGER)`,
	}
	for _, statement := range statements {
		if err := imDB.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	imDB.Exec(`INSERT INTO "group" VALUES (?, ?, ?, 1, ?), (?, ?, ?, 1, ?)`, "group-a", "A", "space-a", now, "group-b", "B", "space-b", now)
	imDB.Exec(`INSERT INTO group_member VALUES (?, ?, 0), (?, ?, 0)`, "group-a", "owner", "group-b", "owner")
	// Owner has DM conversations with two peers. peer-in-a shares the token's
	// space; peer-in-b belongs to a different space entirely. The canonical
	// DM channel_id layout stores the larger-CRC32 uid first, so we seed with
	// the raw values pipeline.GetUserChannels would emit and let
	// NormalizeDMChannelID canonicalize at request time.
	imDB.Exec(`INSERT INTO conversation_extra VALUES ('peer-in-a', 'owner', 1, ?), ('peer-in-b', 'owner', 1, ?)`, now, now)
	// Space membership: only peer-in-a is a member of space-a. peer-in-b
	// belongs to space-b, matching the failure scenario in the review.
	imDB.Exec(`INSERT INTO space_member VALUES ('space-a','owner',1), ('space-a','peer-in-a',1), ('space-b','peer-in-b',1)`)

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

// dmSourceBody builds a create body that names a single DM source (source_type=3).
// The peer uid is the raw stored channel_id — canonicalBotSourceID normalizes it
// against the owner at request time, so the caller passes the peer verbatim.
func dmSourceBody(peerUID string) []byte {
	start := time.Now().Add(-time.Hour).Format(time.RFC3339)
	end := time.Now().Format(time.RFC3339)
	return []byte(fmt.Sprintf(`{"title":"test","time_range":{"start":%q,"end":%q},"sources":[{"source_type":3,"source_id":%q}]}`, start, end, peerUID))
}

// TestCreateBotSummaryRejectsCrossSpaceDMSource reproduces the failure scenario
// described in yujiawei's review (issue #181 P1-1): the owner is a member of
// space-a and has a DM with peer-in-b (a member of space-b only). A bot token
// scoped to space-a must NOT be allowed to reach that DM, since the token's
// stated authority is one space. The pre-fix code returned 201 because the DM
// branch bypassed the space check entirely; this test locks in the 40302 that
// the space_member probe now produces.
func TestCreateBotSummaryRejectsCrossSpaceDMSource(t *testing.T) {
	t.Setenv("BOT_SUMMARY_CREATE_ENABLED", "true")
	_, r, _ := setupBotCreateTest(t)
	w := requestBotCreate(r, "key-cross-dm", dmSourceBody("peer-in-b"))
	if w.Code != http.StatusForbidden || !bytes.Contains(w.Body.Bytes(), []byte(`"code":40302`)) {
		t.Fatalf("cross-space DM must be rejected 40302, got status=%d body=%s", w.Code, w.Body.String())
	}
}

// TestCreateBotSummaryAcceptsInSpaceDMSource is the positive companion: same
// DM path, but this time the peer is a member of the token's space. The
// request must succeed — the P1-1 fix must not over-reject legitimate DMs.
func TestCreateBotSummaryAcceptsInSpaceDMSource(t *testing.T) {
	t.Setenv("BOT_SUMMARY_CREATE_ENABLED", "true")
	_, r, _ := setupBotCreateTest(t)
	w := requestBotCreate(r, "key-in-space-dm", dmSourceBody("peer-in-a"))
	if w.Code != http.StatusCreated {
		t.Fatalf("in-space DM must succeed 201, got status=%d body=%s", w.Code, w.Body.String())
	}
}

// TestCreateBotSummaryIdempotencyBodyMismatch reproduces yujiawei's P1-2
// failure scenario: same (space, bot, idempotency_key) tuple used with two
// different bodies. Pre-fix, the second request returned 200 with the first
// task's identifiers, silently discarding the new intent. Post-fix, the hash
// stored alongside the binding is compared and a mismatch surfaces as 40009,
// mirroring summary_share_snapshot's existing contract.
func TestCreateBotSummaryIdempotencyBodyMismatch(t *testing.T) {
	t.Setenv("BOT_SUMMARY_CREATE_ENABLED", "true")
	_, r, db := setupBotCreateTest(t)

	first := requestBotCreate(r, "same-key", botCreateBody("group-a"))
	if first.Code != http.StatusCreated {
		t.Fatalf("first create must succeed 201, got status=%d body=%s", first.Code, first.Body.String())
	}

	// Second request: same key, different title AND a different time window.
	// Either change alone is enough to shift the hash; using both makes the
	// assertion resilient to future field additions.
	altStart := time.Now().Add(-72 * time.Hour).Format(time.RFC3339)
	altEnd := time.Now().Add(-48 * time.Hour).Format(time.RFC3339)
	altBody := []byte(fmt.Sprintf(`{"title":"COMPLETELY DIFFERENT","time_range":{"start":%q,"end":%q},"sources":[{"source_type":1,"source_id":"group-a"}]}`, altStart, altEnd))
	second := requestBotCreate(r, "same-key", altBody)
	if second.Code != http.StatusConflict || !bytes.Contains(second.Body.Bytes(), []byte(`"code":40009`)) {
		t.Fatalf("mismatched-body replay must be 40009 conflict, got status=%d body=%s", second.Code, second.Body.String())
	}

	// Verify no phantom second task was created — the mismatch must reject
	// before any additional SummaryTask row hits the DB.
	var count int64
	db.Model(&model.SummaryTask{}).Count(&count)
	if count != 1 {
		t.Fatalf("task count after mismatched replay = %d, want 1", count)
	}
}

// TestCreateBotSummaryIdempotencyHashMatchReplays confirms the positive path
// for P1-2: same key AND semantically-equivalent body (identical resolved
// sources, time range, title, etc.) still replays cleanly at HTTP 200 with the
// original task id.
func TestCreateBotSummaryIdempotencyHashMatchReplays(t *testing.T) {
	t.Setenv("BOT_SUMMARY_CREATE_ENABLED", "true")
	_, r, db := setupBotCreateTest(t)

	body := botCreateBody("group-a")
	first := requestBotCreate(r, "replay-key", body)
	second := requestBotCreate(r, "replay-key", body)
	if first.Code != http.StatusCreated || second.Code != http.StatusOK {
		t.Fatalf("expected 201 then 200, got first=%d second=%d", first.Code, second.Code)
	}

	var firstResp, secondResp struct {
		Data struct {
			TaskID int64 `json:"task_id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(first.Body.Bytes(), &firstResp)
	_ = json.Unmarshal(second.Body.Bytes(), &secondResp)
	if firstResp.Data.TaskID == 0 || firstResp.Data.TaskID != secondResp.Data.TaskID {
		t.Fatalf("replay must return same task_id: first=%d second=%d", firstResp.Data.TaskID, secondResp.Data.TaskID)
	}

	var count int64
	db.Model(&model.SummaryTask{}).Count(&count)
	if count != 1 {
		t.Fatalf("replay must not create a second task; count=%d", count)
	}
}
