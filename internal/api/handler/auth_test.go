package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type mockTokenResolver struct{}

func (m *mockTokenResolver) ResolveUID(_ context.Context, token string) (string, error) {
	return token, nil
}

func setupTestDBs(t *testing.T) (db *gorm.DB, imDB *gorm.DB) {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open summary db: %v", err)
	}
	db.AutoMigrate(&model.SummaryTask{}, &model.SummarySource{}, &model.SummaryParticipant{})

	imDB, err = gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open im db: %v", err)
	}
	imDB.Exec("CREATE TABLE group_member (group_no TEXT NOT NULL, uid TEXT NOT NULL, is_deleted INTEGER DEFAULT 0)")

	return db, imDB
}

func seedTask(t *testing.T, db *gorm.DB, imDB *gorm.DB) int64 {
	t.Helper()

	task := model.SummaryTask{
		TaskNo:      "TST-001",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
	}
	db.Create(&task)

	db.Create(&model.SummarySource{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "grp_abc"})
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "participant1", UserName: "P1"})

	imDB.Exec("INSERT INTO group_member (group_no, uid, is_deleted) VALUES (?, ?, 0)", "grp_abc", "groupmember1")
	imDB.Exec("INSERT INTO group_member (group_no, uid, is_deleted) VALUES (?, ?, 1)", "grp_abc", "deletedmember1")

	return task.ID
}

func setupRouter(h *TaskHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.GET("/api/v1/summaries/:id", h.GetSummary)
	r.DELETE("/api/v1/summaries/:id", h.DeleteSummary)
	r.POST("/api/v1/summaries/:id/cancel", h.CancelSummary)
	return r
}

func doRequest(r *gin.Engine, method, path, userID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	if userID != "" {
		req.Header.Set("Token", userID)
	}
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)
	return w
}

func TestAuthorizeTaskAccess_NonMember(t *testing.T) {
	db, imDB := setupTestDBs(t)
	taskID := seedTask(t, db, imDB)
	h := NewTaskHandler(db, imDB, "")
	r := setupRouter(h)

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", taskID), "stranger1")
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-member, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthorizeTaskAccess_NoAuth(t *testing.T) {
	db, imDB := setupTestDBs(t)
	taskID := seedTask(t, db, imDB)
	h := NewTaskHandler(db, imDB, "")
	r := setupRouter(h)

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", taskID), "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthenticated, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthorizeTaskAccess_Creator(t *testing.T) {
	db, imDB := setupTestDBs(t)
	taskID := seedTask(t, db, imDB)
	h := NewTaskHandler(db, imDB, "")
	r := setupRouter(h)

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", taskID), "creator1")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for creator, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthorizeTaskAccess_Participant(t *testing.T) {
	db, imDB := setupTestDBs(t)
	taskID := seedTask(t, db, imDB)
	h := NewTaskHandler(db, imDB, "")
	r := setupRouter(h)

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", taskID), "participant1")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for participant, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthorizeTaskAccess_GroupMemberDenied(t *testing.T) {
	db, imDB := setupTestDBs(t)
	taskID := seedTask(t, db, imDB)
	h := NewTaskHandler(db, imDB, "")
	r := setupRouter(h)

	// groupmember1 is only a source-group member, neither creator nor participant → 403
	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", taskID), "groupmember1")
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for source-group member, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthorizeTaskAccess_DeletedGroupMember(t *testing.T) {
	db, imDB := setupTestDBs(t)
	taskID := seedTask(t, db, imDB)
	h := NewTaskHandler(db, imDB, "")
	r := setupRouter(h)

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", taskID), "deletedmember1")
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for deleted group member, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeleteSummary_RequiresAuth(t *testing.T) {
	db, imDB := setupTestDBs(t)
	taskID := seedTask(t, db, imDB)
	h := NewTaskHandler(db, imDB, "")
	r := setupRouter(h)

	w := doRequest(r, "DELETE", fmt.Sprintf("/api/v1/summaries/%d", taskID), "stranger1")
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for delete by stranger, got %d: %s", w.Code, w.Body.String())
	}

	// Creator can delete
	w = doRequest(r, "DELETE", fmt.Sprintf("/api/v1/summaries/%d", taskID), "creator1")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for delete by creator, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCancelSummary_RequiresAuth(t *testing.T) {
	db, imDB := setupTestDBs(t)
	h := NewTaskHandler(db, imDB, "")

	// Create a pending task for cancellation
	task := model.SummaryTask{
		TaskNo:      "TST-002",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusPending,
	}
	db.Create(&task)

	r := setupRouter(h)

	w := doRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/cancel", task.ID), "stranger1")
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for cancel by stranger, got %d: %s", w.Code, w.Body.String())
	}

	w = doRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/cancel", task.ID), "creator1")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for cancel by creator, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeleteSummary_GroupMemberDenied(t *testing.T) {
	db, imDB := setupTestDBs(t)
	taskID := seedTask(t, db, imDB)
	h := NewTaskHandler(db, imDB, "")
	r := setupRouter(h)

	// Source-group member must not be able to delete another user's summary.
	w := doRequest(r, "DELETE", fmt.Sprintf("/api/v1/summaries/%d", taskID), "groupmember1")
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for delete by source-group member, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeleteSummary_NonCreatorForbidden(t *testing.T) {
	// A4: a NON-creator participant must not be able to delete the whole
	// multi-person task -- they can only LEAVE it. authorizeTaskAccess lets the
	// participant through (read/cancel access), but DeleteSummary now rejects any
	// caller != task.CreatorID with 403 code 40006. participant1 (seeded as a
	// non-creator participant) is the case under test. Fail-before: participant1's
	// delete soft-deleted the task (200). Pass-after: 403 + task still live.
	db, imDB := setupTestDBs(t)
	taskID := seedTask(t, db, imDB)
	h := NewTaskHandler(db, imDB, "")
	r := setupRouter(h)

	w := doRequest(r, "DELETE", fmt.Sprintf("/api/v1/summaries/%d", taskID), "participant1")
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for delete by non-creator participant, got %d: %s", w.Code, w.Body.String())
	}
	var resp apiResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Code != 40006 {
		t.Errorf("expected biz code 40006, got %d", resp.Code)
	}
	// The task must remain live (not soft-deleted).
	var task model.SummaryTask
	if err := db.Where("id = ? AND deleted_at IS NULL", taskID).First(&task).Error; err != nil {
		t.Errorf("task must stay live after a rejected non-creator delete: %v", err)
	}
}

func TestCancelSummary_GroupMemberDenied(t *testing.T) {
	db, imDB := setupTestDBs(t)
	h := NewTaskHandler(db, imDB, "")

	// Create a pending task whose source group contains groupmember1.
	task := model.SummaryTask{
		TaskNo:      "TST-003",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusPending,
	}
	db.Create(&task)
	db.Create(&model.SummarySource{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "grp_abc"})
	imDB.Exec("INSERT INTO group_member (group_no, uid, is_deleted) VALUES (?, ?, 0)", "grp_abc", "groupmember1")

	r := setupRouter(h)

	// Source-group member must not be able to cancel another user's summary.
	w := doRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/cancel", task.ID), "groupmember1")
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for cancel by source-group member, got %d: %s", w.Code, w.Body.String())
	}
}

// setupListTestDBs migrates the extra tables ListSummaries touches (summary_result)
// to avoid no-such-table noise during list rendering.
func setupListTestDBs(t *testing.T) (db *gorm.DB, imDB *gorm.DB) {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open summary db: %v", err)
	}
	db.AutoMigrate(&model.SummaryTask{}, &model.SummarySource{}, &model.SummaryParticipant{}, &model.SummaryResult{})

	imDB, err = gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open im db: %v", err)
	}
	imDB.Exec("CREATE TABLE group_member (group_no TEXT NOT NULL, uid TEXT NOT NULL, is_deleted INTEGER DEFAULT 0)")

	return db, imDB
}

func setupListRouter(h *TaskHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.GET("/api/v1/summaries", h.ListSummaries)
	return r
}

type listResponse struct {
	Code int `json:"code"`
	Data struct {
		Total int           `json:"total"`
		Items []interface{} `json:"items"`
	} `json:"data"`
}

func parseListResponse(t *testing.T, w *httptest.ResponseRecorder) listResponse {
	t.Helper()
	var resp listResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v\nbody: %s", err, w.Body.String())
	}
	return resp
}

func seedListTask(t *testing.T, db *gorm.DB, imDB *gorm.DB) {
	t.Helper()

	task := model.SummaryTask{
		TaskNo:      "TST-LIST-001",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
	}
	db.Create(&task)
	db.Create(&model.SummarySource{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "grp_abc"})
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "participant1", UserName: "P1"})
	imDB.Exec("INSERT INTO group_member (group_no, uid, is_deleted) VALUES (?, ?, 0)", "grp_abc", "groupmember1")
}

func TestListSummaries_GroupMemberSeesNothing(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	seedListTask(t, db, imDB)
	h := NewTaskHandler(db, imDB, "")
	r := setupListRouter(h)

	w := doRequest(r, "GET", "/api/v1/summaries", "groupmember1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := parseListResponse(t, w)
	if resp.Data.Total != 0 {
		t.Errorf("expected total 0 for source-group member, got %d", resp.Data.Total)
	}
	if len(resp.Data.Items) != 0 {
		t.Errorf("expected 0 items for source-group member, got %d", len(resp.Data.Items))
	}
}

func TestListSummaries_CreatorSeesTask(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	seedListTask(t, db, imDB)
	h := NewTaskHandler(db, imDB, "")
	r := setupListRouter(h)

	w := doRequest(r, "GET", "/api/v1/summaries", "creator1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := parseListResponse(t, w)
	if resp.Data.Total != 1 {
		t.Errorf("expected total 1 for creator, got %d", resp.Data.Total)
	}
}

func TestListSummaries_ParticipantSeesTask(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	seedListTask(t, db, imDB)
	h := NewTaskHandler(db, imDB, "")
	r := setupListRouter(h)

	w := doRequest(r, "GET", "/api/v1/summaries", "participant1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := parseListResponse(t, w)
	if resp.Data.Total != 1 {
		t.Errorf("expected total 1 for participant, got %d", resp.Data.Total)
	}
}

// --- need2/3/4/7: GetSummary permissions matrix ---

func getPermissions(t *testing.T, w *httptest.ResponseRecorder) map[string]bool {
	t.Helper()
	var resp struct {
		Data struct {
			Permissions map[string]bool `json:"permissions"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal permissions: %v; body=%s", err, w.Body.String())
	}
	return resp.Data.Permissions
}

func assertPerm(t *testing.T, perms map[string]bool, key string, want bool) {
	t.Helper()
	if perms[key] != want {
		t.Errorf("permission %s = %v, want %v", key, perms[key], want)
	}
}

// seedTask gives creator1 (creator) + participant1 (one participant row).
// Build an explicit MULTI-person task (creator1 + p1 + p2 as participant rows)
// to verify the creator's permission vector with the <=1 legacy gate exercised.
func TestGetSummary_PermissionsMultiPersonCreator(t *testing.T) {
	db, imDB := setupTestDBs(t)
	task := model.SummaryTask{
		TaskNo:      "TST-PERM-MP",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
	}
	db.Create(&task)
	db.Create(&model.SummarySource{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "grp_abc"})
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "creator1", UserName: "Creator", Status: model.ParticipantCompleted})
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "participant1", UserName: "P1", Status: model.ParticipantCompleted})
	h := NewTaskHandler(db, imDB, "")
	r := setupRouter(h)

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", task.ID), "creator1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	perms := getPermissions(t, w)
	// creator + multi-person:
	assertPerm(t, perms, "can_edit_team", true)     // need4: creator, no <=1 gate
	assertPerm(t, perms, "can_schedule", true)      // creator
	assertPerm(t, perms, "can_add_member", true)    // need7: creator
	assertPerm(t, perms, "can_view_schedule", true) // creator IS a participant row here
	assertPerm(t, perms, "can_edit_personal", true) // creator IS a participant row here
	// legacy can_edit keeps old semantics: multi-person => false.
	assertPerm(t, perms, "can_edit", false)
}

// can_edit_team must also require StatusCompleted (matches PUT /edit which 400s
// on non-completed tasks); otherwise creator sees an edit button whose save 400s.
func TestGetSummary_PermissionsCreatorNonCompletedNoEditTeam(t *testing.T) {
	db, imDB := setupTestDBs(t)
	task := model.SummaryTask{
		TaskNo:      "TST-PERM-NONCOMP",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusProcessing,
	}
	db.Create(&task)
	db.Create(&model.SummarySource{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "grp_abc"})
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "creator1", UserName: "Creator", Status: model.ParticipantAccepted})
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "participant1", UserName: "P1", Status: model.ParticipantAccepted})
	h := NewTaskHandler(db, imDB, "")
	r := setupRouter(h)

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", task.ID), "creator1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	perms := getPermissions(t, w)
	// creator but task not completed => can_edit_team must be false (PUT /edit would 400).
	assertPerm(t, perms, "can_edit_team", false)
	// scheduling/add-member are task-level configs, still allowed for creator.
	assertPerm(t, perms, "can_schedule", true)
	assertPerm(t, perms, "can_add_member", true)
}

// A plain (non-creator) participant of a multi-person task.
func TestGetSummary_PermissionsMultiPersonParticipant(t *testing.T) {
	db, imDB := setupTestDBs(t)
	task := model.SummaryTask{
		TaskNo:      "TST-PERM-MP2",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
	}
	db.Create(&task)
	db.Create(&model.SummarySource{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "grp_abc"})
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "creator1", UserName: "Creator", Status: model.ParticipantCompleted})
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "participant1", UserName: "P1", Status: model.ParticipantCompleted})
	h := NewTaskHandler(db, imDB, "")
	r := setupRouter(h)

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", task.ID), "participant1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	perms := getPermissions(t, w)
	assertPerm(t, perms, "can_edit_team", false)    // need4: not creator
	assertPerm(t, perms, "can_schedule", false)     // not creator
	assertPerm(t, perms, "can_add_member", false)   // need7: not creator
	assertPerm(t, perms, "can_view_schedule", true) // need2: any participant
	assertPerm(t, perms, "can_edit_personal", true) // need3: participant edits own
	assertPerm(t, perms, "can_edit", false)
}

// Single-person task where the creator is also the sole participant.
func TestGetSummary_PermissionsSinglePersonCreator(t *testing.T) {
	db, imDB := setupTestDBs(t)
	task := model.SummaryTask{
		TaskNo:      "TST-PERM-SINGLE",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
	}
	db.Create(&task)
	db.Create(&model.SummarySource{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "grp_abc"})
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "creator1", UserName: "Creator", Status: model.ParticipantCompleted})

	h := NewTaskHandler(db, imDB, "")
	r := setupRouter(h)

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", task.ID), "creator1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	perms := getPermissions(t, w)
	assertPerm(t, perms, "can_edit_team", true)
	assertPerm(t, perms, "can_schedule", true)
	assertPerm(t, perms, "can_add_member", true)
	assertPerm(t, perms, "can_view_schedule", true)  // creator is also the participant
	assertPerm(t, perms, "can_edit_personal", true)  // creator is also the participant
	assertPerm(t, perms, "can_edit", true)           // legacy: single-person creator completed
}

// --- P1-A: plain-citation stripping must be gated on TRUE multi-person
//     (participantCount>1), not on SummaryMode==ModeByPerson. ---
//
// Background: the codebase's single "multi-person" measure is participantCount>1
// (Count(task_id=?)>1; see edit.go:107 / personal.go:226,548 / schedule.go:1503),
// and EVERY task is ModeByPerson (there is no ModeByGroup in this DB). The old
// GetSummary/GetResult code stripped plain citations whenever
// task.SummaryMode==model.ModeByPerson, which therefore wiped the [n] source
// citations of 100% of summaries -- including SINGLE-person ones whose plain
// citations only reference the caller's OWN messages (no cross-member privacy
// concern). The fix strips only when participantCount>1.

// setupResultRouter wires GetSummary + GetResult and AutoMigrates SummaryResult
// (setupTestDBs does not migrate it). Returns the migrated db + router.
func setupResultRouter(t *testing.T) (*gorm.DB, *gorm.DB, *gin.Engine) {
	t.Helper()
	db, imDB := setupTestDBs(t)
	// PersonalResult is needed too: the citation-privacy gate now identifies the
	// producer of a plain-citation set by the PersonalResult that owns those exact
	// citations (callerOwnsPlainCitations).
	if err := db.AutoMigrate(&model.SummaryResult{}, &model.PersonalResult{}); err != nil {
		t.Fatalf("migrate SummaryResult/PersonalResult: %v", err)
	}
	h := NewTaskHandler(db, imDB, "")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.GET("/api/v1/summaries/:id", h.GetSummary)
	r.GET("/api/v1/summaries/:id/result", h.GetResult)
	return db, imDB, r
}

// seedResultTask creates a Completed BY_PERSON task with `nParticipants`
// participant rows (creator1 is always the first) and a SummaryResult that
// carries one plain citation. The plain citations are attributed to a PRODUCER
// PersonalResult so the ownership-based privacy gate behaves like production:
//   - single-person: producer == creator1 (the sole participant), so creator1
//     keeps the citations (their own messages).
//   - multi-person: producer is participant2 (NOT the creator). The team
//     SummaryResult in this fixture still carries plain citations to model the
//     leak scenario; creator1 must be redacted because they are not the producer.
// Returns the task ID.
func seedResultTask(t *testing.T, db *gorm.DB, nParticipants int) int64 {
	t.Helper()
	task := model.SummaryTask{
		TaskNo:      fmt.Sprintf("TST-P1A-%d", nParticipants),
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson, // every task is ModeByPerson in this DB
		Status:      model.StatusCompleted,
	}
	db.Create(&task)
	db.Create(&model.SummarySource{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "grp_abc"})

	uids := []string{"creator1", "participant2", "participant3"}
	parts := make([]model.SummaryParticipant, 0, nParticipants)
	for i := 0; i < nParticipants; i++ {
		p := model.SummaryParticipant{
			TaskID: task.ID, UserID: uids[i], UserName: uids[i], Status: model.ParticipantCompleted,
		}
		db.Create(&p)
		parts = append(parts, p)
	}

	cits := []model.Citation{{Index: 1, Sender: "s", Content: "原始消息", ChannelID: "grp_abc"}}
	res := model.SummaryResult{
		TaskID:      task.ID,
		Content:     "team/solo summary [1]",
		Version:     1,
		GeneratedAt: timezone.Now(),
	}
	res.SetCitations(cits)
	db.Create(&res)

	// Attribute the plain citations to their producer's PersonalResult. For
	// single-person that is creator1; for multi-person we attribute them to
	// participant2 to model a result whose plain citations belong to a non-creator
	// member (the leak shape).
	producer := "creator1"
	if nParticipants > 1 {
		producer = "participant2"
	}
	var producerRefID int64
	for _, p := range parts {
		if p.UserID == producer {
			producerRefID = p.ID
		}
	}
	now := timezone.Now()
	pr := model.PersonalResult{
		TaskID:           task.ID,
		ParticipantRefID: producerRefID,
		UserID:           producer,
		Content:          "team/solo summary [1]",
		SubmittedAt:      &now,
		GeneratedAt:      &now,
	}
	pr.SetCitations(cits)
	db.Create(&pr)
	return task.ID
}

// citationsFromBody extracts data.result.citations from a GetSummary response.
func summaryResultCitations(t *testing.T, w *httptest.ResponseRecorder) []json.RawMessage {
	t.Helper()
	var resp struct {
		Data struct {
			Result struct {
				Citations []json.RawMessage `json:"citations"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal GetSummary body: %v; body=%s", err, w.Body.String())
	}
	return resp.Data.Result.Citations
}

// citationsFromResultBody extracts data.citations from a GetResult response.
func getResultCitations(t *testing.T, w *httptest.ResponseRecorder) []json.RawMessage {
	t.Helper()
	var resp struct {
		Data struct {
			Citations []json.RawMessage `json:"citations"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal GetResult body: %v; body=%s", err, w.Body.String())
	}
	return resp.Data.Citations
}

// TestGetSummary_SinglePerson_KeepsPlainCitations: a single-person task
// (participantCount==1) with citations must KEEP its plain citations.
// FAIL-BEFORE: the old `if task.SummaryMode == model.ModeByPerson` branch fired
// for this task too (every task is ModeByPerson), so citations were cleared and
// this assertion failed. PASS-AFTER: len(participants)>1 is false -> kept.
func TestGetSummary_SinglePerson_KeepsPlainCitations(t *testing.T) {
	db, _, r := setupResultRouter(t)
	taskID := seedResultTask(t, db, 1) // single participant (creator1 only)

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", taskID), "creator1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	cits := summaryResultCitations(t, w)
	if len(cits) == 0 {
		t.Errorf("single-person GetSummary must KEEP plain citations for [n] sourcing; got none. body=%s", w.Body.String())
	}
}

// TestGetSummary_MultiPerson_StripsPlainCitations: a true multi-person task
// (participantCount>1) must STRIP plain citations (they would leak other
// members' raw chat messages).
func TestGetSummary_MultiPerson_StripsPlainCitations(t *testing.T) {
	db, _, r := setupResultRouter(t)
	taskID := seedResultTask(t, db, 2) // creator1 + participant2

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", taskID), "creator1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	cits := summaryResultCitations(t, w)
	if len(cits) != 0 {
		t.Errorf("multi-person GetSummary must STRIP plain citations (privacy); got %d. body=%s", len(cits), w.Body.String())
	}
}

// TestGetResult_SinglePerson_KeepsPlainCitations: same single-person rule for
// GetResult, which now counts participants via loadTaskParticipantCount.
// FAIL-BEFORE: old `task.SummaryMode == model.ModeByPerson` cleared them for the
// single-person task too. PASS-AFTER: pc==1 -> kept.
func TestGetResult_SinglePerson_KeepsPlainCitations(t *testing.T) {
	db, _, r := setupResultRouter(t)
	taskID := seedResultTask(t, db, 1)

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d/result", taskID), "creator1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	cits := getResultCitations(t, w)
	if len(cits) == 0 {
		t.Errorf("single-person GetResult must KEEP plain citations for [n] sourcing; got none. body=%s", w.Body.String())
	}
}

// TestGetResult_MultiPerson_StripsPlainCitations: multi-person GetResult strips.
func TestGetResult_MultiPerson_StripsPlainCitations(t *testing.T) {
	db, _, r := setupResultRouter(t)
	taskID := seedResultTask(t, db, 2)

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d/result", taskID), "creator1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	cits := getResultCitations(t, w)
	if len(cits) != 0 {
		t.Errorf("multi-person GetResult must STRIP plain citations (privacy); got %d. body=%s", len(cits), w.Body.String())
	}
}

// --- P1 (yujiawei): citation leak in a single-confirmed scheduled CONFIRM round ---
//
// Leak shape: a scheduled CONFIRM round materializes exactly ONE participant row
// (memberA), memberA confirms, the creator does NOT. The single-person direct
// path then writes memberA's RAW chat citations into the team SummaryResult.
// participantCount==1, so the OLD count-based gate (len(participants)>1 /
// loadTaskParticipantCount>1) stayed open and shipped memberA's raw messages to
// the creator -- who has read access via CreatorID but is NOT the producer.
//
// seedSingleConfirmLeakTask reproduces exactly that DB shape: one participant
// (memberA, NOT the creator) with a PersonalResult whose citations were copied
// onto the team SummaryResult.
func seedSingleConfirmLeakTask(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	task := model.SummaryTask{
		TaskNo:      "TST-LEAK-001",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
		TriggerType: model.TriggerScheduled,
	}
	db.Create(&task)
	db.Create(&model.SummarySource{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "grp_abc"})

	// Exactly ONE materialized participant: memberA (a non-creator member).
	part := model.SummaryParticipant{
		TaskID: task.ID, UserID: "memberA", UserName: "memberA", Status: model.ParticipantCompleted,
	}
	db.Create(&part)

	cits := []model.Citation{{Index: 1, Sender: "memberA", Content: "memberA 的私密聊天", ChannelID: "grp_abc"}}
	res := model.SummaryResult{
		TaskID:      task.ID,
		Content:     "scheduled round summary [1]",
		Version:     1,
		GeneratedAt: timezone.Now(),
	}
	res.SetCitations(cits) // single-person direct path copied memberA's citations here
	db.Create(&res)

	now := timezone.Now()
	pr := model.PersonalResult{
		TaskID:           task.ID,
		ParticipantRefID: part.ID,
		UserID:           "memberA",
		Content:          "scheduled round summary [1]",
		SubmittedAt:      &now,
		GeneratedAt:      &now,
	}
	pr.SetCitations(cits)
	db.Create(&pr)
	return task.ID
}

// TestGetSummary_SingleConfirmLeak_CreatorRedacted is the P1 regression for
// GetSummary. The creator (read access via CreatorID, NOT the producer) must NOT
// see memberA's plain citations.
// FAIL-BEFORE: the count gate (len(participants)==1) left citations intact ->
// non-empty -> leak. PASS-AFTER: callerOwnsPlainCitations(creator1) is false
// (creator owns no PersonalResult with these citations) -> stripped to [].
func TestGetSummary_SingleConfirmLeak_CreatorRedacted(t *testing.T) {
	db, _, r := setupResultRouter(t)
	taskID := seedSingleConfirmLeakTask(t, db)

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", taskID), "creator1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	cits := summaryResultCitations(t, w)
	if len(cits) != 0 {
		t.Errorf("P1 LEAK: creator must NOT see memberA's plain citations; got %d. body=%s", len(cits), w.Body.String())
	}
}

// TestGetResult_SingleConfirmLeak_CreatorRedacted is the same P1 regression for
// GetResult.
func TestGetResult_SingleConfirmLeak_CreatorRedacted(t *testing.T) {
	db, _, r := setupResultRouter(t)
	taskID := seedSingleConfirmLeakTask(t, db)

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d/result", taskID), "creator1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	cits := getResultCitations(t, w)
	if len(cits) != 0 {
		t.Errorf("P1 LEAK: creator must NOT see memberA's plain citations via GetResult; got %d. body=%s", len(cits), w.Body.String())
	}
}

// TestGetSummary_SingleConfirmLeak_ProducerSeesOwn: the producing member
// (memberA) IS the contributor, so they keep their own plain citations -- the
// fix must not over-redact the legitimate owner.
func TestGetSummary_SingleConfirmLeak_ProducerSeesOwn(t *testing.T) {
	db, _, r := setupResultRouter(t)
	taskID := seedSingleConfirmLeakTask(t, db)

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", taskID), "memberA")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	cits := summaryResultCitations(t, w)
	if len(cits) == 0 {
		t.Errorf("producer memberA must KEEP their own plain citations; got none. body=%s", w.Body.String())
	}
}

// TestGetResult_SingleConfirmLeak_ProducerSeesOwn: same, via GetResult.
func TestGetResult_SingleConfirmLeak_ProducerSeesOwn(t *testing.T) {
	db, _, r := setupResultRouter(t)
	taskID := seedSingleConfirmLeakTask(t, db)

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d/result", taskID), "memberA")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	cits := getResultCitations(t, w)
	if len(cits) == 0 {
		t.Errorf("producer memberA must KEEP their own plain citations via GetResult; got none. body=%s", w.Body.String())
	}
}
