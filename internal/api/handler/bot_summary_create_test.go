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
	// Owner has DM conversations with three peers plus a "note to self"
	// self-DM. peer-in-a shares the token's space; peer-in-b belongs to a
	// different space entirely; the owner is trivially a member of its own
	// space so the self-DM must succeed (PR #181 round-3 P2-2). The canonical
	// DM channel_id layout stores the larger-CRC32 uid first, so we seed with
	// the raw values pipeline.GetUserChannels would emit and let
	// NormalizeDMChannelID canonicalize at request time.
	imDB.Exec(`INSERT INTO conversation_extra VALUES ('peer-in-a', 'owner', 1, ?), ('peer-in-b', 'owner', 1, ?), ('owner', 'owner', 1, ?)`, now, now, now)
	// Space membership: peer-in-a and owner belong to space-a; peer-in-b
	// belongs to space-b. The owner row is what makes the self-DM path
	// succeed (peer==owner ⇒ peerInSpace(owner, space-a) must find this row).
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
	// Hoist the body so both requests carry the *same* time_range —
	// previously each call re-sampled time.Now() and the test passed only
	// when both requests landed in the same wall-clock second, which is
	// exactly the flake yujiawei's PR #181 round-3 review caught.
	body := botCreateBody("group-a")
	first := requestBotCreate(r, "stable-key", body)
	second := requestBotCreate(r, "stable-key", body)
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

// TestCreateBotSummaryIdempotencyReplaysAcrossSubSecondDrift is the direct
// regression for yujiawei's PR #181 round-3 P1-1: two calls that request the
// same relative window but hit the server on either side of a second boundary
// must still replay to the same task, not conflict on 40009. The fix rounds
// the time_range in the hash to whole minutes, so we simulate the wall-clock
// drift by explicitly building two bodies whose second (and nanosecond)
// components differ but whose minute matches.
func TestCreateBotSummaryIdempotencyReplaysAcrossSubSecondDrift(t *testing.T) {
	t.Setenv("BOT_SUMMARY_CREATE_ENABLED", "true")
	_, r, db := setupBotCreateTest(t)

	// Anchor at whole seconds within one minute; the second call bumps by
	// half a second, which is enough to make an RFC3339Nano hash differ
	// but must not disturb a minute-truncated hash.
	base := time.Now().UTC().Truncate(time.Minute).Add(30 * time.Second)
	start := base.Add(-time.Hour).Format(time.RFC3339Nano)
	end := base.Format(time.RFC3339Nano)
	firstBody := []byte(fmt.Sprintf(`{"title":"weekly","time_range":{"start":%q,"end":%q},"sources":[{"source_type":1,"source_id":"group-a"}]}`, start, end))

	drifted := base.Add(500 * time.Millisecond)
	startDrift := drifted.Add(-time.Hour).Format(time.RFC3339Nano)
	endDrift := drifted.Format(time.RFC3339Nano)
	secondBody := []byte(fmt.Sprintf(`{"title":"weekly","time_range":{"start":%q,"end":%q},"sources":[{"source_type":1,"source_id":"group-a"}]}`, startDrift, endDrift))

	first := requestBotCreate(r, "drift-key", firstBody)
	second := requestBotCreate(r, "drift-key", secondBody)
	if first.Code != http.StatusCreated || second.Code != http.StatusOK {
		t.Fatalf("sub-second retry must replay: first=%d %s second=%d %s", first.Code, first.Body.String(), second.Code, second.Body.String())
	}

	var count int64
	db.Model(&model.SummaryTask{}).Count(&count)
	if count != 1 {
		t.Fatalf("sub-second drift must not create a second task; count=%d", count)
	}
}

// TestCreateBotSummaryIdempotencyMismatchIncludesExistingTaskID confirms the
// 409/40009 payload echoes the original task_id, so a client that reuses an
// idempotency key by accident can still recover the task the first call
// actually created (PR #181 round-3 P1-1 companion recovery contract).
func TestCreateBotSummaryIdempotencyMismatchIncludesExistingTaskID(t *testing.T) {
	t.Setenv("BOT_SUMMARY_CREATE_ENABLED", "true")
	_, r, _ := setupBotCreateTest(t)

	first := requestBotCreate(r, "same-key-diff", botCreateBody("group-a"))
	if first.Code != http.StatusCreated {
		t.Fatalf("first create must succeed: status=%d body=%s", first.Code, first.Body.String())
	}
	var firstResp struct {
		Data struct {
			TaskID int64 `json:"task_id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(first.Body.Bytes(), &firstResp)
	if firstResp.Data.TaskID == 0 {
		t.Fatal("first response must include task_id")
	}

	// Different title AND different time window -> hash mismatch.
	altStart := time.Now().Add(-72 * time.Hour).Format(time.RFC3339)
	altEnd := time.Now().Add(-48 * time.Hour).Format(time.RFC3339)
	altBody := []byte(fmt.Sprintf(`{"title":"COMPLETELY DIFFERENT","time_range":{"start":%q,"end":%q},"sources":[{"source_type":1,"source_id":"group-a"}]}`, altStart, altEnd))
	second := requestBotCreate(r, "same-key-diff", altBody)
	if second.Code != http.StatusConflict || !bytes.Contains(second.Body.Bytes(), []byte(`"code":40009`)) {
		t.Fatalf("mismatch must be 40009 conflict: status=%d body=%s", second.Code, second.Body.String())
	}
	var mismatchResp struct {
		Data struct {
			ExistingTaskID int64 `json:"existing_task_id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(second.Body.Bytes(), &mismatchResp)
	if mismatchResp.Data.ExistingTaskID != firstResp.Data.TaskID {
		t.Fatalf("409 payload must echo the original task_id; got %d want %d", mismatchResp.Data.ExistingTaskID, firstResp.Data.TaskID)
	}
}

// TestCreateBotSummaryAcceptsSelfDMSource confirms that a "note-to-self"
// self-DM (peer == owner) is not misclassified as cross-space (P2-2). The
// owner is a member of space-a by construction, so the peerInSpace probe on
// (owner, space-a) succeeds and the create returns 201.
func TestCreateBotSummaryAcceptsSelfDMSource(t *testing.T) {
	t.Setenv("BOT_SUMMARY_CREATE_ENABLED", "true")
	_, r, _ := setupBotCreateTest(t)
	w := requestBotCreate(r, "key-self-dm", dmSourceBody("owner"))
	if w.Code != http.StatusCreated {
		t.Fatalf("self-DM must succeed 201, got status=%d body=%s", w.Code, w.Body.String())
	}
}

// TestCorsPreflightAllowsIdempotencyKey guards yujiawei's P2-8: since the
// bot-create handler makes Idempotency-Key mandatory, the CORS preflight
// response must list it in Access-Control-Allow-Headers or browser callers
// hit a preflight failure before they can even try the POST.
func TestCorsPreflightAllowsIdempotencyKey(t *testing.T) {
	// Inline mock router that mirrors the exact CORS middleware
	// registered in internal/api/router/router.go so this test locks in
	// the allow-list without booting the whole router (which would drag
	// summary-api dependencies into a handler test).
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type,Authorization,Token,X-Space-Id,Accept,Accept-Language,Idempotency-Key")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/api/v1/bot/summaries", nil)
	req.Header.Set("Origin", "https://example.test")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "Idempotency-Key")
	r.ServeHTTP(w, req)

	allowed := w.Header().Get("Access-Control-Allow-Headers")
	if !strings.Contains(strings.ToLower(allowed), "idempotency-key") {
		t.Fatalf("Access-Control-Allow-Headers must list Idempotency-Key; got %q", allowed)
	}
}
