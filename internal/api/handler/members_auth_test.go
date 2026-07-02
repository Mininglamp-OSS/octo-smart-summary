//go:build cgo

package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// assertBizCode unmarshals the response body and asserts resp.Code, instead of
// a brittle bytes.Contains substring match on the raw JSON (which can falsely
// match a code appearing anywhere -- e.g. inside a message, id, or timestamp).
// Mirrors the resp.Code style already used in auth_test.go.
func assertBizCode(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	var resp apiResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response body: %v; body=%s", err, w.Body.String())
	}
	if resp.Code != want {
		t.Errorf("expected biz code %d, got %d; body=%s", want, resp.Code, w.Body.String())
	}
}

func setupMembersTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open summary db: %v", err)
	}
	db.AutoMigrate(&model.SummaryTask{}, &model.SummarySource{}, &model.SummaryParticipant{}, &model.PersonalResult{}, &model.SummarySchedule{}, &model.SummaryResult{})
	// Mirror the production MySQL unique constraints AutoMigrate doesn't create on
	// sqlite, so handler ON CONFLICT upserts resolve:
	//   uk_part(task_id,user_id)            -- AddMembers participant upsert (F2)
	//   uk_task_participant(task_id,part_ref) -- Accept personal_result upsert
	db.Exec("CREATE UNIQUE INDEX uk_part_task_user ON summary_participant(task_id, user_id)")
	db.Exec("CREATE UNIQUE INDEX uk_task_participant ON summary_personal_result(task_id, participant_ref_id)")
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

// TestGetMembers_NoCitationsLeak asserts the privacy fix: GetMembers must NOT
// expose any submitted member's citations (raw chat messages/context/jump info)
// to other members. content is still returned (it is meant to be cross-visible),
// but the citations field must be absent from every member object. Fail-before:
// the old GetMembers set member["citations"]=pr.GetCitations(), so this test
// failed; pass-after: the field is removed at the source.
func TestGetMembers_NoCitationsLeak(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID := seedMembersTask(t, db)

	// Give participant1 a SUBMITTED personal result that carries citations
	// (the original chat-record leak vector).
	var p model.SummaryParticipant
	if err := db.Where("task_id = ? AND user_id = ?", taskID, "participant1").First(&p).Error; err != nil {
		t.Fatalf("load participant: %v", err)
	}
	now := timezone.Now()
	pr := model.PersonalResult{
		TaskID:           taskID,
		ParticipantRefID: p.ID,
		UserID:           "participant1",
		Content:          "participant1 的个人总结正文 [1]",
		WorkerStatus:     model.PersonalStatusCompleted,
		SubmittedAt:      &now,
	}
	pr.SetCitations([]model.Citation{{
		Index:     1,
		Sender:    "someone",
		Content:   "原始聊天记录原文不应被他人看到",
		ChannelID: "grp_abc",
	}})
	if err := db.Create(&pr).Error; err != nil {
		t.Fatalf("create personal result: %v", err)
	}

	h := NewPersonalHandler(db, "", nil)
	r := setupMembersRouter(h)

	// creator reads the member list (cross-member view).
	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d/members", taskID), "creator1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Data struct {
			Members []map[string]json.RawMessage `json:"members"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v; body=%s", err, w.Body.String())
	}

	foundSubmitted := false
	for _, m := range resp.Data.Members {
		if _, hasCitations := m["citations"]; hasCitations {
			t.Errorf("member object must NOT contain 'citations' (privacy leak); body=%s", w.Body.String())
		}
		if c, ok := m["content"]; ok {
			// content stays cross-visible for submitted members.
			if string(c) != "null" {
				foundSubmitted = true
			}
		}
	}
	if !foundSubmitted {
		t.Errorf("expected at least one submitted member with content; body=%s", w.Body.String())
	}
}

// TestGetMembers_SelfGetsCitationsOthersDoNot asserts the need3 fix: when a
// member reads the member list, their OWN submitted row carries citations (so
// they can click [n] references and open the cited chat detail, identical to
// GetPersonal), while every OTHER submitted member's row still has NO citations
// (privacy collar unchanged). Fail-before: GetMembers never set citations for
// anyone, so the self row had none; pass-after: only the self row gains them.
func TestGetMembers_SelfGetsCitationsOthersDoNot(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID := seedMembersTask(t, db) // creator1 + participant1 (P1)
	now := timezone.Now()

	// participant1 submits a report carrying citations (the cross-view leak vector).
	var pP model.SummaryParticipant
	if err := db.Where("task_id = ? AND user_id = ?", taskID, "participant1").First(&pP).Error; err != nil {
		t.Fatalf("load participant1: %v", err)
	}
	prP := model.PersonalResult{
		TaskID:           taskID,
		ParticipantRefID: pP.ID,
		UserID:           "participant1",
		Content:          "participant1 正文 [1]",
		WorkerStatus:     model.PersonalStatusCompleted,
		SubmittedAt:      &now,
	}
	prP.SetCitations([]model.Citation{{Index: 1, Sender: "s", Content: "他人原始聊天记录", ChannelID: "grp_abc"}})
	if err := db.Create(&prP).Error; err != nil {
		t.Fatalf("create participant1 PR: %v", err)
	}

	// creator1 is also a participant with their OWN submitted report + citations.
	pC := model.SummaryParticipant{TaskID: taskID, UserID: "creator1", UserName: "C1", Status: model.ParticipantCompleted}
	if err := db.Create(&pC).Error; err != nil {
		t.Fatalf("create creator1 participant: %v", err)
	}
	prC := model.PersonalResult{
		TaskID:           taskID,
		ParticipantRefID: pC.ID,
		UserID:           "creator1",
		Content:          "creator1 自己的正文 [1]",
		WorkerStatus:     model.PersonalStatusCompleted,
		SubmittedAt:      &now,
	}
	prC.SetCitations([]model.Citation{{Index: 1, Sender: "self", Content: "自己的引用原文", ChannelID: "grp_abc"}})
	if err := db.Create(&prC).Error; err != nil {
		t.Fatalf("create creator1 PR: %v", err)
	}

	h := NewPersonalHandler(db, "", nil)
	r := setupMembersRouter(h)

	// creator1 reads the member list (sees self + participant1).
	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d/members", taskID), "creator1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Data struct {
			Members []map[string]json.RawMessage `json:"members"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v; body=%s", err, w.Body.String())
	}

	sawSelf, sawOther := false, false
	for _, m := range resp.Data.Members {
		uid := ""
		if raw, ok := m["user_id"]; ok {
			_ = json.Unmarshal(raw, &uid)
		}
		_, hasCitations := m["citations"]
		switch uid {
		case "creator1":
			sawSelf = true
			if !hasCitations {
				t.Errorf("self (creator1) row MUST carry citations so [n] refs open; body=%s", w.Body.String())
			}
		case "participant1":
			sawOther = true
			if hasCitations {
				t.Errorf("other member (participant1) row MUST NOT carry citations (privacy); body=%s", w.Body.String())
			}
		}
	}
	if !sawSelf {
		t.Errorf("expected creator1 (self) submitted row in members; body=%s", w.Body.String())
	}
	if !sawOther {
		t.Errorf("expected participant1 (other) submitted row in members; body=%s", w.Body.String())
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

// --- need3/need6: PUT /summaries/:id/personal-edit ---

// triggerCapture is a tiny worker-trigger sink: it records every
// WorkerTriggerRequest POSTed to its URL so tests can assert a meta_summary /
// personal_summary trigger fired.
type triggerCapture struct {
	mu   sync.Mutex
	reqs []model.WorkerTriggerRequest
	srv  *httptest.Server
}

func newTriggerCapture(t *testing.T) *triggerCapture {
	t.Helper()
	tc := &triggerCapture{}
	tc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req model.WorkerTriggerRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		tc.mu.Lock()
		tc.reqs = append(tc.reqs, req)
		tc.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(tc.srv.Close)
	return tc
}

func (tc *triggerCapture) url() string { return tc.srv.URL }

func (tc *triggerCapture) waitFor(typ string, taskID int64) bool {
	for i := 0; i < 50; i++ {
		tc.mu.Lock()
		for _, r := range tc.reqs {
			if r.Type == typ && r.TaskID == taskID {
				tc.mu.Unlock()
				return true
			}
		}
		tc.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func setupPersonalEditRouter(h *PersonalHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.PUT("/api/v1/summaries/:id/personal-edit", h.PersonalEdit)
	r.POST("/api/v1/summaries/:id/members", h.AddMembers)
	return r
}

// seedMultiPersonTask creates a completed by-person task with a creator + two
// members, each with their own PersonalResult.
func seedMultiPersonTask(t *testing.T, db *gorm.DB) (taskID int64) {
	t.Helper()
	now := timezone.Now()
	task := model.SummaryTask{
		TaskNo:      "TST-PE-001",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
	}
	db.Create(&task)
	db.Create(&model.SummarySource{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "grp_abc"})

	for _, uid := range []string{"creator1", "member_x", "member_y"} {
		p := model.SummaryParticipant{TaskID: task.ID, UserID: uid, UserName: uid, Status: model.ParticipantCompleted}
		db.Create(&p)
		db.Create(&model.PersonalResult{
			TaskID:           task.ID,
			ParticipantRefID: p.ID,
			UserID:           uid,
			Content:          uid + " original [1]",
			CitationsJSON:    `[{"index":1,"sender":"s","content":"c","channel_id":"grp_abc"}]`,
			WorkerStatus:     model.PersonalStatusCompleted,
			GeneratedAt:      &now,
		})
	}
	return task.ID
}

func doJSONRequest(r *gin.Engine, method, path, userID string, body interface{}) *httptest.ResponseRecorder {
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if userID != "" {
		req.Header.Set("Token", userID)
	}
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)
	return w
}

func TestPersonalEdit_OwnSuccessAndMetaTrigger(t *testing.T) {
	// need3 + need6: a participant edits their OWN report -> 200, only their row
	// changes, and a meta_summary trigger fires.
	db := setupMembersTestDB(t)
	taskID := seedMultiPersonTask(t, db)
	tc := newTriggerCapture(t)

	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupPersonalEditRouter(h)

	body := map[string]interface{}{"content": "member_x EDITED their own report"}
	w := doJSONRequest(r, "PUT", fmt.Sprintf("/api/v1/summaries/%d/personal-edit", taskID), "member_x", body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for own personal-edit, got %d: %s", w.Code, w.Body.String())
	}

	// Only member_x's row changed.
	var prX model.PersonalResult
	db.Where("task_id = ? AND user_id = ?", taskID, "member_x").First(&prX)
	if prX.Content != "member_x EDITED their own report" || prX.EditedAt == nil {
		t.Errorf("member_x personal result not updated: content=%q edited_at=%v", prX.Content, prX.EditedAt)
	}
	var prY model.PersonalResult
	db.Where("task_id = ? AND user_id = ?", taskID, "member_y").First(&prY)
	if prY.Content != "member_y original [1]" || prY.EditedAt != nil {
		t.Errorf("member_y personal result must be untouched: content=%q edited_at=%v", prY.Content, prY.EditedAt)
	}

	if !tc.waitFor("meta_summary", taskID) {
		t.Errorf("expected a meta_summary trigger after personal-edit")
	}
}

func TestPersonalEdit_NonParticipantForbidden(t *testing.T) {
	// BE-1 (check-order fix): an in-space non-participant now gets the SAME
	// 404/40008 ("你不是该任务的参与者") as Accept, NOT the old 403/40003. The
	// previous 403-before-space-check leaked task existence + participation across
	// spaces (40003 vs 40008 oracle); the participant check now runs AFTER
	// requireTaskInSpace and returns 40008 so participation is not leaked.
	db := setupMembersTestDB(t)
	taskID := seedMultiPersonTask(t, db)
	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalEditRouter(h)

	body := map[string]interface{}{"content": "stranger tries to edit"}
	w := doJSONRequest(r, "PUT", fmt.Sprintf("/api/v1/summaries/%d/personal-edit", taskID), "stranger1", body)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for in-space non-participant personal-edit, got %d: %s", w.Code, w.Body.String())
	}
	assertBizCode(t, w, 40008) // 40008, never the leaky differentiated 40003
}

func TestPersonalEdit_CannotEditOthers(t *testing.T) {
	// There is no way to target another member's row: editing as member_x only
	// ever touches member_x. member_y stays untouched (proven by content+edited_at).
	db := setupMembersTestDB(t)
	taskID := seedMultiPersonTask(t, db)
	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalEditRouter(h)

	body := map[string]interface{}{"content": "member_x writes"}
	w := doJSONRequest(r, "PUT", fmt.Sprintf("/api/v1/summaries/%d/personal-edit", taskID), "member_x", body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var prY model.PersonalResult
	db.Where("task_id = ? AND user_id = ?", taskID, "member_y").First(&prY)
	if prY.Content != "member_y original [1]" {
		t.Errorf("member_y must be unchanged by member_x edit, got %q", prY.Content)
	}
}

func TestPersonalEdit_EmptyContent(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID := seedMultiPersonTask(t, db)
	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalEditRouter(h)

	body := map[string]interface{}{"content": "   "}
	w := doJSONRequest(r, "PUT", fmt.Sprintf("/api/v1/summaries/%d/personal-edit", taskID), "member_x", body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty content, got %d: %s", w.Code, w.Body.String())
	}
}

// --- need7: POST /summaries/:id/members ---

func TestAddMembers_CreatorAddsMemberPendingNoDispatch(t *testing.T) {
	// need7 (corrected): a newly-added member is created as PENDING, with NO
	// PersonalResult and NO personal_summary dispatch. The member must Accept
	// themselves to generate their summary.
	db := setupMembersTestDB(t)
	taskID := seedMultiPersonTask(t, db)
	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupPersonalEditRouter(h)

	body := map[string]interface{}{"user_ids": []string{"newcomer1"}}
	w := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/members", taskID), "creator1", body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for creator add-member, got %d: %s", w.Code, w.Body.String())
	}

	// participant created as PENDING (not Accepted), no confirmed_at.
	var p model.SummaryParticipant
	if err := db.Where("task_id = ? AND user_id = ?", taskID, "newcomer1").First(&p).Error; err != nil {
		t.Fatalf("new participant not created: %v", err)
	}
	if p.Status != model.ParticipantPending {
		t.Errorf("new member must be Pending, got status=%d", p.Status)
	}
	if p.ConfirmedAt != nil {
		t.Errorf("new member must NOT be confirmed yet, got confirmed_at=%v", p.ConfirmedAt)
	}
	// NO PersonalResult created at add time.
	var prCnt int64
	db.Model(&model.PersonalResult{}).Where("task_id = ? AND user_id = ?", taskID, "newcomer1").Count(&prCnt)
	if prCnt != 0 {
		t.Errorf("add-member must NOT create a PersonalResult yet, got %d", prCnt)
	}
	// NO dispatch at add time (give the async goroutine, if any, a moment).
	if tc.waitFor("personal_summary", taskID) {
		t.Errorf("add-member must NOT dispatch personal_summary before the member accepts")
	}
}

func TestAddMembers_NewMemberAcceptGeneratesPRAndDispatch(t *testing.T) {
	// need7 (corrected): after being added (Pending), the new member's own Accept
	// flips them to Accepted, creates the PersonalResult and dispatches
	// personal_summary (existing Accept flow).
	db := setupMembersTestDB(t)
	taskID := seedMultiPersonTask(t, db)
	// Accept's idempotent upsert relies on the uk_task_participant unique
	// constraint, which setupMembersTestDB already creates on the sqlite test DB.
	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := gin.New()
	gin.SetMode(gin.TestMode)
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.POST("/api/v1/summaries/:id/members", h.AddMembers)
	r.POST("/api/v1/summaries/:id/accept", h.Accept)

	// creator adds newcomer1 (Pending)
	w := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/members", taskID), "creator1",
		map[string]interface{}{"user_ids": []string{"newcomer1"}})
	if w.Code != http.StatusOK {
		t.Fatalf("add-member: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// newcomer1 accepts -> Accepted + PR + dispatch
	w2 := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/accept", taskID), "newcomer1", nil)
	if w2.Code != http.StatusOK {
		t.Fatalf("accept: expected 200, got %d: %s", w2.Code, w2.Body.String())
	}

	var p model.SummaryParticipant
	db.Where("task_id = ? AND user_id = ?", taskID, "newcomer1").First(&p)
	if p.Status != model.ParticipantAccepted {
		t.Errorf("after accept, member must be Accepted, got %d", p.Status)
	}
	var prCnt int64
	db.Model(&model.PersonalResult{}).Where("task_id = ? AND user_id = ?", taskID, "newcomer1").Count(&prCnt)
	if prCnt != 1 {
		t.Errorf("after accept, exactly one PersonalResult expected, got %d", prCnt)
	}
	if !tc.waitFor("personal_summary", taskID) {
		t.Errorf("after accept, a personal_summary trigger is expected")
	}
}

func setupAcceptRouter(h *PersonalHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.POST("/api/v1/summaries/:id/members", h.AddMembers)
	r.POST("/api/v1/summaries/:id/accept", h.Accept)
	return r
}

// Revive fix: a member added to an ALREADY-Completed multi-person BY_PERSON task
// must, on Accept, pull the task back to Processing so the personal-worker's task
// CAS (Pending/WaitingConfirm -> Processing) does not abort and the new member's
// personal summary actually runs.
func TestAccept_RevivesCompletedMultiPersonTask(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID := seedMultiPersonTask(t, db) // BY_PERSON, Completed, creator + 2 members
	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupAcceptRouter(h)

	// creator adds newcomer1 (Pending). Task stays Completed (AddMembers never
	// touches task.status).
	w := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/members", taskID), "creator1",
		map[string]interface{}{"user_ids": []string{"newcomer1"}})
	if w.Code != http.StatusOK {
		t.Fatalf("add-member: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var before model.SummaryTask
	db.First(&before, taskID)
	if before.Status != model.StatusCompleted {
		t.Fatalf("precondition: task must still be Completed after AddMembers, got %d", before.Status)
	}

	// newcomer1 accepts -> task revived to Processing, PR(Pending) created, dispatch fired.
	w2 := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/accept", taskID), "newcomer1", nil)
	if w2.Code != http.StatusOK {
		t.Fatalf("accept: expected 200, got %d: %s", w2.Code, w2.Body.String())
	}

	var after model.SummaryTask
	db.First(&after, taskID)
	if after.Status != model.StatusProcessing {
		t.Errorf("FAIL-BEFORE/PASS-AFTER: Completed task must be revived to Processing on new-member Accept, got status=%d", after.Status)
	}
	if after.ProcessingDeadline == nil || !after.ProcessingDeadline.After(timezone.Now()) {
		t.Errorf("revived task must have a future processing_deadline, got %v", after.ProcessingDeadline)
	}

	// PersonalResult created Pending for the new member.
	var pr model.PersonalResult
	if err := db.Where("task_id = ? AND user_id = ?", taskID, "newcomer1").First(&pr).Error; err != nil {
		t.Fatalf("new member PersonalResult not created: %v", err)
	}
	if pr.WorkerStatus != model.PersonalStatusPending {
		t.Errorf("new member PersonalResult must be Pending, got worker_status=%d", pr.WorkerStatus)
	}

	// dispatch personal_summary fired.
	if !tc.waitFor("personal_summary", taskID) {
		t.Errorf("Accept must dispatch personal_summary for the revived new member")
	}
}

// Single-person and BY_GROUP tasks (and tasks already Processing) must NOT be
// disturbed by Accept's revive logic.
func TestAccept_DoesNotReviveSinglePersonOrNonCompleted(t *testing.T) {
	t.Run("single-person Completed BY_PERSON is not revived", func(t *testing.T) {
		db := setupMembersTestDB(t)
		now := timezone.Now()
		task := model.SummaryTask{
			TaskNo:      "TST-REVIVE-SINGLE",
			SpaceID:     "space1",
			CreatorID:   "creator1",
			SummaryMode: model.ModeByPerson,
			Status:      model.StatusCompleted,
		}
		db.Create(&task)
		// Exactly one participant (Pending so Accept proceeds), participantCount==1.
		p := model.SummaryParticipant{TaskID: task.ID, UserID: "solo", UserName: "solo", Status: model.ParticipantPending, CreatedAt: now}
		db.Create(&p)

		tc := newTriggerCapture(t)
		h := NewPersonalHandler(db, tc.url(), nil)
		r := setupAcceptRouter(h)

		w := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/accept", task.ID), "solo", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("accept: expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var after model.SummaryTask
		db.First(&after, task.ID)
		if after.Status != model.StatusCompleted {
			t.Errorf("single-person task must NOT be revived, status=%d", after.Status)
		}
	})

	t.Run("task still Processing is left untouched", func(t *testing.T) {
		db := setupMembersTestDB(t)
		taskID := seedMultiPersonTask(t, db)
		// Flip task to Processing (normal in-flight multi-person add scenario).
		db.Model(&model.SummaryTask{}).Where("id = ?", taskID).Update("status", model.StatusProcessing)
		// Add + accept a new member.
		tc := newTriggerCapture(t)
		h := NewPersonalHandler(db, tc.url(), nil)
		r := setupAcceptRouter(h)
		doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/members", taskID), "creator1",
			map[string]interface{}{"user_ids": []string{"newcomer1"}})
		var before model.SummaryTask
		db.First(&before, taskID)
		prevDeadline := before.ProcessingDeadline

		w := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/accept", taskID), "newcomer1", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("accept: expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var after model.SummaryTask
		db.First(&after, taskID)
		if after.Status != model.StatusProcessing {
			t.Errorf("already-Processing task must stay Processing, got %d", after.Status)
		}
		// The conditional UPDATE (WHERE status=Completed) must NOT fire on a Processing
		// task, so it must NOT rewrite processing_deadline.
		if !reflectDeadlineEqual(prevDeadline, after.ProcessingDeadline) {
			t.Errorf("Processing task's processing_deadline must be untouched by Accept; before=%v after=%v", prevDeadline, after.ProcessingDeadline)
		}
	})
}

func reflectDeadlineEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// Idempotency: a member who already Accepted must not re-dispatch nor re-toggle
// task status when they Accept again.
func TestAccept_IdempotentDoesNotRedispatchOrRevive(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID := seedMultiPersonTask(t, db)
	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupAcceptRouter(h)

	// Add + first Accept (revives the Completed task).
	doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/members", taskID), "creator1",
		map[string]interface{}{"user_ids": []string{"newcomer1"}})
	w1 := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/accept", taskID), "newcomer1", nil)
	if w1.Code != http.StatusOK {
		t.Fatalf("first accept: expected 200, got %d", w1.Code)
	}
	if !tc.waitFor("personal_summary", taskID) {
		t.Fatalf("first accept must dispatch personal_summary")
	}
	// Simulate the task having moved on (e.g. back to Completed) and the member's
	// result terminal, to assert a repeat Accept is a strict no-op.
	db.Model(&model.SummaryTask{}).Where("id = ?", taskID).Update("status", model.StatusCompleted)
	var prc model.PersonalResult
	db.Where("task_id = ? AND user_id = ?", taskID, "newcomer1").First(&prc)
	db.Model(&model.PersonalResult{}).Where("id = ?", prc.ID).
		Update("worker_status", model.PersonalStatusCompleted)

	tc.mu.Lock()
	countBefore := len(tc.reqs)
	tc.mu.Unlock()

	// Second Accept of the same (now-Accepted) member: status-only idempotent
	// fast-path returns early -> no tx, no dispatch, no revive.
	w2 := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/accept", taskID), "newcomer1", nil)
	if w2.Code != http.StatusOK {
		t.Fatalf("second accept: expected 200, got %d", w2.Code)
	}
	time.Sleep(60 * time.Millisecond) // give any (erroneous) async dispatch a chance
	tc.mu.Lock()
	countAfter := len(tc.reqs)
	tc.mu.Unlock()
	if countAfter != countBefore {
		t.Errorf("repeat Accept must NOT re-dispatch: trigger count before=%d after=%d", countBefore, countAfter)
	}
	var after model.SummaryTask
	db.First(&after, taskID)
	if after.Status != model.StatusCompleted {
		t.Errorf("repeat Accept must NOT revive an already-Accepted member's task, got status=%d", after.Status)
	}
}

func TestAddMembers_ConcurrentDuplicateNo500(t *testing.T) {
	// F2: concurrency-safe participant create. Two racing AddMembers calls can both
	// miss the existence check and both Create -> unique-key uk(task_id,user_id)
	// collision -> 500. With OnConflict DoNothing the loser is an idempotent skip.
	// We reproduce the collision deterministically: create the MySQL-equivalent
	// unique index on the sqlite test DB, pre-insert the participant row to
	// simulate the winner, then call AddMembers for the same uid (force the
	// existence snapshot to be stale by inserting AFTER the snapshot is impossible
	// here, so we assert the OnConflict path directly + the handler stays 200/idempotent).
	db := setupMembersTestDB(t)
	taskID := seedMultiPersonTask(t, db)
	// uk_part(task_id,user_id) is created by setupMembersTestDB so the OnConflict
	// participant upsert (F2) resolves on the sqlite test DB.
	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalEditRouter(h)

	// First add -> creates newcomer1 (Pending).
	w1 := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/members", taskID), "creator1",
		map[string]interface{}{"user_ids": []string{"newcomer1"}})
	if w1.Code != http.StatusOK {
		t.Fatalf("first add: expected 200, got %d: %s", w1.Code, w1.Body.String())
	}
	// Second add of the SAME uid with the unique index live -> must NOT 500 and
	// must NOT duplicate (idempotent via existence check + OnConflict backstop).
	w2 := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/members", taskID), "creator1",
		map[string]interface{}{"user_ids": []string{"newcomer1"}})
	if w2.Code != http.StatusOK {
		t.Fatalf("second add (dup) must not 500, got %d: %s", w2.Code, w2.Body.String())
	}
	var cnt int64
	db.Model(&model.SummaryParticipant{}).Where("task_id = ? AND user_id = ?", taskID, "newcomer1").Count(&cnt)
	if cnt != 1 {
		t.Errorf("duplicate AddMembers must not create a second participant row, got %d", cnt)
	}
	// The second call reports added=0 (idempotent).
	var resp struct {
		Data struct {
			Added int `json:"added"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w2.Body.Bytes(), &resp)
	if resp.Data.Added != 0 {
		t.Errorf("second (dup) add must report added=0, got %d", resp.Data.Added)
	}
}

func TestAddMembers_OnConflictRaceBackstop(t *testing.T) {
	// F2 (direct): simulate the genuine race where a participant row appears AFTER
	// the handler built its existence snapshot. We can't inject mid-transaction, so
	// instead we drive the handler's OnConflict path directly: with the unique
	// index live, inserting the same (task_id,user_id) twice via the handler's
	// upsert clause must DoNothing on the second insert (RowsAffected==0) rather
	// than error -- proving the backstop the handler relies on.
	db := setupMembersTestDB(t)
	taskID := seedMultiPersonTask(t, db)
	// uk_part(task_id,user_id) created by setupMembersTestDB.
	row := model.SummaryParticipant{TaskID: taskID, UserID: "racer1", UserName: "racer1", Status: model.ParticipantPending}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("seed racer: %v", err)
	}
	dup := model.SummaryParticipant{TaskID: taskID, UserID: "racer1", UserName: "racer1", Status: model.ParticipantPending}
	res := db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "task_id"}, {Name: "user_id"}},
		DoNothing: true,
	}).Create(&dup)
	if res.Error != nil {
		t.Fatalf("OnConflict DoNothing must not error on duplicate, got %v", res.Error)
	}
	if res.RowsAffected != 0 {
		t.Errorf("OnConflict DoNothing on existing (task,user) must affect 0 rows, got %d", res.RowsAffected)
	}
}

func TestAddMembers_NonCreatorForbidden(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID := seedMultiPersonTask(t, db)
	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalEditRouter(h)

	body := map[string]interface{}{"user_ids": []string{"newcomer1"}}
	w := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/members", taskID), "member_x", body)
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-creator add-member, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAddMembers_IdempotentDuplicate(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID := seedMultiPersonTask(t, db)
	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalEditRouter(h)

	// member_x is already a participant -> adding again must be a no-op (no error,
	// no duplicate row, no state reset).
	body := map[string]interface{}{"user_ids": []string{"member_x"}}
	w := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/members", taskID), "creator1", body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for idempotent re-add, got %d: %s", w.Code, w.Body.String())
	}
	var cnt int64
	db.Model(&model.SummaryParticipant{}).Where("task_id = ? AND user_id = ?", taskID, "member_x").Count(&cnt)
	if cnt != 1 {
		t.Errorf("re-adding member_x must not duplicate participant rows, got %d", cnt)
	}
	// member_x stays Completed (its original state), not reset to Pending.
	var p model.SummaryParticipant
	db.Where("task_id = ? AND user_id = ?", taskID, "member_x").First(&p)
	if p.Status != model.ParticipantCompleted {
		t.Errorf("existing member must not be reset, want Completed got %d", p.Status)
	}
}

func TestAddMembers_ScheduleConfigPendingNewMemberOldUnchanged(t *testing.T) {
	// need7 (corrected) / V5 Q3: when the task has a bound schedule, a newly-added
	// member is written into participant_config as confirmed=false; the existing
	// confirmed member (creator1) is left untouched. F3: AddMembers reads the
	// schedule row FOR UPDATE and does the participant_config read-modify-write
	// under that lock (serializing concurrent adds); this test verifies the locked
	// RMW path is correct single-threaded (new member appended unconfirmed, old
	// member preserved, gate recomputed).
	db := setupMembersTestDB(t)
	now := timezone.Now()
	sched := model.SummarySchedule{
		SpaceID:           "space1",
		CreatorID:         "creator1",
		SummaryMode:       model.ModeByPerson,
		ConfirmPolicy:     model.SchedConfirmRequire,
		ParticipantConfig: model.JSON(`{"participants":[{"user_id":"creator1","confirmed":true,"confirmed_at":"2026-06-01T00:00:00Z"}],"confirm_gate_passed":true}`),
		IsActive:          1,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	db.Create(&sched)
	task := model.SummaryTask{
		TaskNo:      "TST-AM-SCHED",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
		ScheduleID:  &sched.ID,
	}
	db.Create(&task)
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "creator1", UserName: "creator1", Status: model.ParticipantCompleted})

	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalEditRouter(h)

	body := map[string]interface{}{"user_ids": []string{"newcomer1"}}
	w := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/members", task.ID), "creator1", body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var got model.SummarySchedule
	db.First(&got, sched.ID)
	cfg := model.ParseScheduleParticipantConfig(got.ParticipantConfig)

	// new member: present + UNCONFIRMED.
	ne := cfg.FindParticipant("newcomer1")
	if ne == nil || ne.Confirmed || ne.ConfirmedAt != nil {
		t.Errorf("newcomer1 must be in config UNCONFIRMED, got %+v", ne)
	}
	// old member: untouched (still confirmed).
	ce := cfg.FindParticipant("creator1")
	if ce == nil || !ce.Confirmed {
		t.Errorf("creator1 confirm state must be untouched (confirmed), got %+v", ce)
	}
	// gate must drop to false because a pending member now exists.
	if cfg.ConfirmGatePassed {
		t.Errorf("confirm_gate_passed must be false after adding an unconfirmed member")
	}
}

// --- consent-bypass P1 (reviewer yujiawei): AddMembers on an AUTO multi-person
//     schedule must FLIP confirm_policy AUTO->CONFIRM when it adds an unconfirmed
//     new member. Otherwise the next scheduled run goes through
//     buildScheduledTaskParticipantsAuto, which pre-accepts the WHOLE roster
//     (EffectiveUserIDs) ignoring per-member Confirmed flags -> the new member is
//     auto-dispatched and their chat summarized without ever Accepting. ---

// helper: build an AUTO multi-person schedule + a task bound to it, with creator1
// already a member. Returns (schedule, task).
func setupAutoSchedTaskForFlip(t *testing.T, db *gorm.DB) (model.SummarySchedule, model.SummaryTask) {
	t.Helper()
	now := timezone.Now()
	sched := model.SummarySchedule{
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		// AUTO multi-person: bare-array participant_config recording the roster.
		ConfirmPolicy:     model.SchedConfirmAuto,
		ParticipantConfig: model.JSON(`[{"user_id":"creator1","user_name":"creator1"},{"user_id":"member2","user_name":"member2"}]`),
		IsActive:          1,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	db.Create(&sched)
	task := model.SummaryTask{
		TaskNo:      "TST-FLIP-AUTO",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
		ScheduleID:  &sched.ID,
	}
	db.Create(&task)
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "creator1", UserName: "creator1", Status: model.ParticipantCompleted})
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "member2", UserName: "member2", Status: model.ParticipantCompleted})
	return sched, task
}

// FAIL-BEFORE regression: adding an unconfirmed new member to an AUTO multi-person
// schedule must flip confirm_policy to CONFIRM (SchedConfirmRequire). Without the
// fix this asserts FAIL because the policy stays AUTO -> next run auto-accepts the
// whole roster incl. the un-Accepted newcomer (consent bypass).
func TestAddMembers_AutoSchedule_FlipsToConfirmOnNewMember(t *testing.T) {
	db := setupMembersTestDB(t)
	sched, task := setupAutoSchedTaskForFlip(t, db)

	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalEditRouter(h)

	body := map[string]interface{}{"user_ids": []string{"newcomer1"}}
	w := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/members", task.ID), "creator1", body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var got model.SummarySchedule
	db.First(&got, sched.ID)

	// core consent-bypass fix: policy must have flipped AUTO -> CONFIRM so the next
	// scheduled run takes the CONFIRM path and waits for the newcomer to Accept.
	if got.ConfirmPolicy != model.SchedConfirmRequire {
		t.Fatalf("confirm_policy must flip AUTO->CONFIRM (%d) after adding an unconfirmed member, got %d",
			model.SchedConfirmRequire, got.ConfirmPolicy)
	}

	// and the newcomer must be recorded UNCONFIRMED (consent still pending).
	cfg := model.ParseScheduleParticipantConfig(got.ParticipantConfig)
	ne := cfg.FindParticipant("newcomer1")
	if ne == nil || ne.Confirmed {
		t.Errorf("newcomer1 must be present and UNCONFIRMED, got %+v", ne)
	}
}

// NO-REGRESSION ①: an already-CONFIRM schedule adding a member keeps CONFIRM
// (no spurious change / no side effect from the flip logic).
func TestAddMembers_ConfirmSchedule_PolicyStaysConfirm(t *testing.T) {
	db := setupMembersTestDB(t)
	now := timezone.Now()
	sched := model.SummarySchedule{
		SpaceID:           "space1",
		CreatorID:         "creator1",
		SummaryMode:       model.ModeByPerson,
		ConfirmPolicy:     model.SchedConfirmRequire,
		ParticipantConfig: model.JSON(`{"participants":[{"user_id":"creator1","confirmed":true,"confirmed_at":"2026-06-01T00:00:00Z"}],"confirm_gate_passed":true}`),
		IsActive:          1,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	db.Create(&sched)
	task := model.SummaryTask{
		TaskNo:      "TST-FLIP-CONFIRM",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
		ScheduleID:  &sched.ID,
	}
	db.Create(&task)
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "creator1", UserName: "creator1", Status: model.ParticipantCompleted})

	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalEditRouter(h)

	body := map[string]interface{}{"user_ids": []string{"newcomer1"}}
	w := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/members", task.ID), "creator1", body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var got model.SummarySchedule
	db.First(&got, sched.ID)
	if got.ConfirmPolicy != model.SchedConfirmRequire {
		t.Errorf("already-CONFIRM schedule must stay CONFIRM (%d), got %d", model.SchedConfirmRequire, got.ConfirmPolicy)
	}
}

// NO-REGRESSION ②: a non-schedule (manual) task AddMembers must not touch any
// schedule and must succeed normally.
func TestAddMembers_NonSchedule_NoScheduleChange(t *testing.T) {
	db := setupMembersTestDB(t)
	task := model.SummaryTask{
		TaskNo:      "TST-FLIP-MANUAL",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
		// no ScheduleID -> manual task.
	}
	db.Create(&task)
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "creator1", UserName: "creator1", Status: model.ParticipantCompleted})

	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalEditRouter(h)

	body := map[string]interface{}{"user_ids": []string{"newcomer1"}}
	w := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/members", task.ID), "creator1", body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	// no schedule rows should exist at all.
	var n int64
	db.Model(&model.SummarySchedule{}).Count(&n)
	if n != 0 {
		t.Errorf("manual task AddMembers must not create/alter any schedule, found %d schedule rows", n)
	}
}

// NO-REGRESSION ③: adding an ALREADY-EXISTING member (no new member) to an AUTO
// schedule must NOT flip the policy (no addedCount/schedDirty -> no flip).
func TestAddMembers_AutoSchedule_ExistingMember_NoFlip(t *testing.T) {
	db := setupMembersTestDB(t)
	sched, task := setupAutoSchedTaskForFlip(t, db)

	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalEditRouter(h)

	// member2 already exists -> no new member added.
	body := map[string]interface{}{"user_ids": []string{"member2"}}
	w := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/members", task.ID), "creator1", body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var got model.SummarySchedule
	db.First(&got, sched.ID)
	if got.ConfirmPolicy != model.SchedConfirmAuto {
		t.Errorf("AUTO schedule must stay AUTO (%d) when no NEW member is added, got %d", model.SchedConfirmAuto, got.ConfirmPolicy)
	}
}

// --- Leave (POST /summaries/:id/leave) + RemoveMember (DELETE
//     /summaries/:id/members?uid=<uid>): physical removal of participant +
//     personal_result rows, then a meta_summary recompute. ---

// setupLeaveRemoveRouter wires the leave + remove routes (plus members for
// convenience) for the PersonalHandler under test.
func setupLeaveRemoveRouter(h *PersonalHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.POST("/api/v1/summaries/:id/leave", h.Leave)
	r.DELETE("/api/v1/summaries/:id/members", h.RemoveMember)
	return r
}

func TestLeave_NonCreatorParticipantLeaves(t *testing.T) {
	// A non-creator participant leaves: 200, their participant row AND their
	// personal_result row are physically deleted, and a meta_summary recompute is
	// dispatched. Fail-before: no Leave handler/route existed (404). Pass-after: 200
	// + both rows gone.
	db := setupMembersTestDB(t)
	taskID := seedMultiPersonTask(t, db) // creator1 + member_x + member_y
	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupLeaveRemoveRouter(h)

	w := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/leave", taskID), "member_x", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for non-creator participant leave, got %d: %s", w.Code, w.Body.String())
	}

	var pCnt int64
	db.Model(&model.SummaryParticipant{}).Where("task_id = ? AND user_id = ?", taskID, "member_x").Count(&pCnt)
	if pCnt != 0 {
		t.Errorf("leave must physically delete member_x participant row, got %d", pCnt)
	}
	var prCnt int64
	db.Model(&model.PersonalResult{}).Where("task_id = ? AND user_id = ?", taskID, "member_x").Count(&prCnt)
	if prCnt != 0 {
		t.Errorf("leave must physically delete member_x personal_result row, got %d", prCnt)
	}
	// Other members are untouched.
	var otherCnt int64
	db.Model(&model.SummaryParticipant{}).Where("task_id = ? AND user_id = ?", taskID, "member_y").Count(&otherCnt)
	if otherCnt != 1 {
		t.Errorf("leave must not touch member_y, got participant count %d", otherCnt)
	}
	if !tc.waitFor("meta_summary", taskID) {
		t.Errorf("leave must dispatch a meta_summary recompute")
	}
}

func TestLeave_CreatorCannotLeave(t *testing.T) {
	// The creator cannot leave -- they must delete the task instead -> 403.
	db := setupMembersTestDB(t)
	taskID := seedMultiPersonTask(t, db)
	h := NewPersonalHandler(db, "", nil)
	r := setupLeaveRemoveRouter(h)

	w := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/leave", taskID), "creator1", nil)
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for creator leave, got %d: %s", w.Code, w.Body.String())
	}
	// creator's participant row stays.
	var cnt int64
	db.Model(&model.SummaryParticipant{}).Where("task_id = ? AND user_id = ?", taskID, "creator1").Count(&cnt)
	if cnt != 1 {
		t.Errorf("creator participant row must remain after rejected leave, got %d", cnt)
	}
}

func TestLeave_NonParticipantForbidden(t *testing.T) {
	// A stranger (not a participant) cannot leave -> 403.
	db := setupMembersTestDB(t)
	taskID := seedMultiPersonTask(t, db)
	h := NewPersonalHandler(db, "", nil)
	r := setupLeaveRemoveRouter(h)

	w := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/leave", taskID), "stranger1", nil)
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-participant leave, got %d: %s", w.Code, w.Body.String())
	}
}

func TestRemoveMember_CreatorRemoves(t *testing.T) {
	// The creator removes member_x: 200, member_x's participant + personal_result
	// rows are physically deleted, and a meta_summary recompute is dispatched.
	db := setupMembersTestDB(t)
	taskID := seedMultiPersonTask(t, db)
	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupLeaveRemoveRouter(h)

	w := doJSONRequest(r, "DELETE", fmt.Sprintf("/api/v1/summaries/%d/members?uid=%s", taskID, "member_x"), "creator1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for creator remove-member, got %d: %s", w.Code, w.Body.String())
	}
	var pCnt int64
	db.Model(&model.SummaryParticipant{}).Where("task_id = ? AND user_id = ?", taskID, "member_x").Count(&pCnt)
	if pCnt != 0 {
		t.Errorf("remove must physically delete member_x participant row, got %d", pCnt)
	}
	var prCnt int64
	db.Model(&model.PersonalResult{}).Where("task_id = ? AND user_id = ?", taskID, "member_x").Count(&prCnt)
	if prCnt != 0 {
		t.Errorf("remove must physically delete member_x personal_result row, got %d", prCnt)
	}
	if !tc.waitFor("meta_summary", taskID) {
		t.Errorf("remove must dispatch a meta_summary recompute")
	}
}

func TestRemoveMember_NonCreatorForbidden(t *testing.T) {
	// A non-creator cannot remove members -> 403.
	db := setupMembersTestDB(t)
	taskID := seedMultiPersonTask(t, db)
	h := NewPersonalHandler(db, "", nil)
	r := setupLeaveRemoveRouter(h)

	w := doJSONRequest(r, "DELETE", fmt.Sprintf("/api/v1/summaries/%d/members?uid=%s", taskID, "member_y"), "member_x", nil)
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-creator remove-member, got %d: %s", w.Code, w.Body.String())
	}
	// member_y still present.
	var cnt int64
	db.Model(&model.SummaryParticipant{}).Where("task_id = ? AND user_id = ?", taskID, "member_y").Count(&cnt)
	if cnt != 1 {
		t.Errorf("member_y must remain after rejected remove, got %d", cnt)
	}
}

func TestRemoveMember_CannotRemoveCreator(t *testing.T) {
	// The creator cannot be removed (addressed by uid==creator) -> 400.
	db := setupMembersTestDB(t)
	taskID := seedMultiPersonTask(t, db)
	h := NewPersonalHandler(db, "", nil)
	r := setupLeaveRemoveRouter(h)

	w := doJSONRequest(r, "DELETE", fmt.Sprintf("/api/v1/summaries/%d/members?uid=%s", taskID, "creator1"), "creator1", nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 when removing the creator, got %d: %s", w.Code, w.Body.String())
	}
	// creator still present.
	var cnt int64
	db.Model(&model.SummaryParticipant{}).Where("task_id = ? AND user_id = ?", taskID, "creator1").Count(&cnt)
	if cnt != 1 {
		t.Errorf("creator participant row must remain, got %d", cnt)
	}
}

// --- FIX-SCHEDULE-ALLROUNDS: under a schedule, the same schedule_id spawns
//     multiple rounds of summary_task, each with its own participant /
//     personal_result rows. Leave/RemoveMember must purge the target member from
//     ALL rounds of that schedule (else the collapsed list still shows them and
//     "leave/remove does nothing"). Manual (schedule_id=nil) tasks stay
//     single-round. ---

// seedScheduleMultiRound seeds two rounds of one schedule (first round
// trigger_type=1 + a later scheduled round trigger_type=2) under schedID, plus,
// when otherSchedID != 0, one round of a DIFFERENT schedule. Every round gets
// participant + personal_result rows for creator1 + member_x + member_y. It
// returns the two task IDs of the primary schedule (firstRound, latestRound).
func seedScheduleMultiRound(t *testing.T, db *gorm.DB, schedID, otherSchedID int64) (firstRound, latestRound int64) {
	t.Helper()
	now := timezone.Now()

	seedRound := func(no string, sid int64, trigger int) int64 {
		sidCopy := sid
		task := model.SummaryTask{
			TaskNo:      no,
			SpaceID:     "space1",
			CreatorID:   "creator1",
			SummaryMode: model.ModeByPerson,
			Status:      model.StatusCompleted,
			TriggerType: trigger,
			ScheduleID:  &sidCopy,
		}
		db.Create(&task)
		db.Create(&model.SummarySource{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "grp_abc"})
		for _, uid := range []string{"creator1", "member_x", "member_y"} {
			p := model.SummaryParticipant{TaskID: task.ID, UserID: uid, UserName: uid, Status: model.ParticipantCompleted}
			db.Create(&p)
			db.Create(&model.PersonalResult{
				TaskID:           task.ID,
				ParticipantRefID: p.ID,
				UserID:           uid,
				Content:          uid + " " + no,
				WorkerStatus:     model.PersonalStatusCompleted,
				GeneratedAt:      &now,
			})
		}
		return task.ID
	}

	firstRound = seedRound("TST-SCH-R1", schedID, model.TriggerManual)    // first round trigger_type=1
	latestRound = seedRound("TST-SCH-R2", schedID, model.TriggerScheduled) // scheduled round trigger_type=2

	if otherSchedID != 0 {
		seedRound("TST-SCH-OTHER", otherSchedID, model.TriggerScheduled)
	}
	return firstRound, latestRound
}

func countParticipant(db *gorm.DB, taskID int64, uid string) int64 {
	var n int64
	db.Model(&model.SummaryParticipant{}).Where("task_id = ? AND user_id = ?", taskID, uid).Count(&n)
	return n
}

func countPersonalResult(db *gorm.DB, taskID int64, uid string) int64 {
	var n int64
	db.Model(&model.PersonalResult{}).Where("task_id = ? AND user_id = ?", taskID, uid).Count(&n)
	return n
}

func TestLeave_ScheduleDeletesAllRounds(t *testing.T) {
	// Fail-before: Leave deleted only the latest round's rows, leaving member_x in
	// the first round -> list still showed them. Pass-after: member_x's
	// participant + personal_result rows are gone from BOTH rounds, while
	// creator1/member_y and the OTHER schedule are untouched.
	db := setupMembersTestDB(t)
	firstRound, latestRound := seedScheduleMultiRound(t, db, 7001, 7002)
	// capture the other-schedule round id for cross-schedule assertions.
	var otherTask model.SummaryTask
	if err := db.Where("schedule_id = ?", 7002).First(&otherTask).Error; err != nil {
		t.Fatalf("load other-schedule round: %v", err)
	}
	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupLeaveRemoveRouter(h)

	w := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/leave", latestRound), "member_x", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for schedule leave, got %d: %s", w.Code, w.Body.String())
	}

	// member_x purged from BOTH rounds of THIS schedule.
	if c := countParticipant(db, firstRound, "member_x"); c != 0 {
		t.Errorf("member_x participant must be deleted from first round, got %d", c)
	}
	if c := countParticipant(db, latestRound, "member_x"); c != 0 {
		t.Errorf("member_x participant must be deleted from latest round, got %d", c)
	}
	if c := countPersonalResult(db, firstRound, "member_x"); c != 0 {
		t.Errorf("member_x personal_result must be deleted from first round, got %d", c)
	}
	if c := countPersonalResult(db, latestRound, "member_x"); c != 0 {
		t.Errorf("member_x personal_result must be deleted from latest round, got %d", c)
	}
	// creator + other member untouched in BOTH rounds.
	for _, rid := range []int64{firstRound, latestRound} {
		if c := countParticipant(db, rid, "creator1"); c != 1 {
			t.Errorf("creator1 must remain in round %d, got %d", rid, c)
		}
		if c := countParticipant(db, rid, "member_y"); c != 1 {
			t.Errorf("member_y must remain in round %d, got %d", rid, c)
		}
	}
	// OTHER schedule untouched (no cross-schedule deletion).
	if c := countParticipant(db, otherTask.ID, "member_x"); c != 1 {
		t.Errorf("member_x in a DIFFERENT schedule must NOT be deleted, got %d", c)
	}
	if c := countPersonalResult(db, otherTask.ID, "member_x"); c != 1 {
		t.Errorf("member_x personal_result in a DIFFERENT schedule must NOT be deleted, got %d", c)
	}
}

func TestRemoveMember_ScheduleDeletesAllRounds(t *testing.T) {
	// Creator removes member_x on the latest round of a schedule: member_x must be
	// purged from ALL rounds of that schedule; creator un-removable; cross-schedule
	// rows untouched. Fail-before only purged the latest round.
	db := setupMembersTestDB(t)
	firstRound, latestRound := seedScheduleMultiRound(t, db, 8001, 8002)
	var otherTask model.SummaryTask
	if err := db.Where("schedule_id = ?", 8002).First(&otherTask).Error; err != nil {
		t.Fatalf("load other-schedule round: %v", err)
	}
	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupLeaveRemoveRouter(h)

	w := doJSONRequest(r, "DELETE", fmt.Sprintf("/api/v1/summaries/%d/members?uid=%s", latestRound, "member_x"), "creator1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for schedule remove, got %d: %s", w.Code, w.Body.String())
	}

	for _, rid := range []int64{firstRound, latestRound} {
		if c := countParticipant(db, rid, "member_x"); c != 0 {
			t.Errorf("member_x participant must be deleted from round %d, got %d", rid, c)
		}
		if c := countPersonalResult(db, rid, "member_x"); c != 0 {
			t.Errorf("member_x personal_result must be deleted from round %d, got %d", rid, c)
		}
		// creator never deletable.
		if c := countParticipant(db, rid, "creator1"); c != 1 {
			t.Errorf("creator1 must remain in round %d, got %d", rid, c)
		}
	}
	// cross-schedule untouched.
	if c := countParticipant(db, otherTask.ID, "member_x"); c != 1 {
		t.Errorf("member_x in a DIFFERENT schedule must NOT be deleted, got %d", c)
	}
}

func TestLeave_ManualTaskUnchangedSingleRound(t *testing.T) {
	// Regression: a manual task (schedule_id == nil) leave deletes ONLY that task's
	// rows. We seed a manual task AND an unrelated schedule round for the same
	// member; the manual leave must not reach across into the schedule round, and
	// vice-versa it only deletes the one manual task.
	db := setupMembersTestDB(t)
	manualTaskID := seedMultiPersonTask(t, db) // schedule_id == nil
	firstRound, _ := seedScheduleMultiRound(t, db, 9001, 0)
	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupLeaveRemoveRouter(h)

	w := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/leave", manualTaskID), "member_x", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for manual leave, got %d: %s", w.Code, w.Body.String())
	}
	// manual task row deleted.
	if c := countParticipant(db, manualTaskID, "member_x"); c != 0 {
		t.Errorf("manual leave must delete member_x in the manual task, got %d", c)
	}
	// unrelated schedule round NOT touched.
	if c := countParticipant(db, firstRound, "member_x"); c != 1 {
		t.Errorf("manual leave must NOT touch an unrelated schedule round, got %d", c)
	}
}

func TestRemoveMember_ManualTaskUnchangedSingleRound(t *testing.T) {
	// Regression: a manual task remove deletes ONLY that task's rows; an unrelated
	// schedule round for the same member is untouched.
	db := setupMembersTestDB(t)
	manualTaskID := seedMultiPersonTask(t, db) // schedule_id == nil
	firstRound, _ := seedScheduleMultiRound(t, db, 9101, 0)
	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupLeaveRemoveRouter(h)

	w := doJSONRequest(r, "DELETE", fmt.Sprintf("/api/v1/summaries/%d/members?uid=%s", manualTaskID, "member_x"), "creator1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for manual remove, got %d: %s", w.Code, w.Body.String())
	}
	if c := countParticipant(db, manualTaskID, "member_x"); c != 0 {
		t.Errorf("manual remove must delete member_x in the manual task, got %d", c)
	}
	if c := countParticipant(db, firstRound, "member_x"); c != 1 {
		t.Errorf("manual remove must NOT touch an unrelated schedule round, got %d", c)
	}
}

// --- FIX1: Leave/RemoveMember must REVIVE an already-Completed multi-person
//     BY_PERSON task back to Processing so the dispatched meta_summary recompute
//     can actually write (the meta worker refuses to write a non-Processing
//     task). Core verifiable point: after the delete tx, task.status flips
//     Completed -> Processing and processing_deadline is set. ---

func TestLeave_RevivesCompletedTaskForRecompute(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID := seedMultiPersonTask(t, db) // Completed, BY_PERSON, creator1 + member_x + member_y
	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupLeaveRemoveRouter(h)

	w := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/leave", taskID), "member_x", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for leave, got %d: %s", w.Code, w.Body.String())
	}

	var task model.SummaryTask
	if err := db.First(&task, taskID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if task.Status != model.StatusProcessing {
		t.Errorf("leave on a Completed BY_PERSON task must revive status to Processing(%d), got %d", model.StatusProcessing, task.Status)
	}
	if task.ProcessingDeadline == nil {
		t.Errorf("revive must set processing_deadline, got nil")
	}
	if !tc.waitFor("meta_summary", taskID) {
		t.Errorf("leave must dispatch a meta_summary recompute")
	}
}

func TestRemoveMember_RevivesCompletedTaskForRecompute(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID := seedMultiPersonTask(t, db)
	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupLeaveRemoveRouter(h)

	w := doJSONRequest(r, "DELETE", fmt.Sprintf("/api/v1/summaries/%d/members?uid=%s", taskID, "member_x"), "creator1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for remove-member, got %d: %s", w.Code, w.Body.String())
	}

	var task model.SummaryTask
	if err := db.First(&task, taskID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if task.Status != model.StatusProcessing {
		t.Errorf("remove on a Completed BY_PERSON task must revive status to Processing(%d), got %d", model.StatusProcessing, task.Status)
	}
	if task.ProcessingDeadline == nil {
		t.Errorf("revive must set processing_deadline, got nil")
	}
	if !tc.waitFor("meta_summary", taskID) {
		t.Errorf("remove must dispatch a meta_summary recompute")
	}
}

// seedScheduledMultiPersonTask seeds a Completed BY_PERSON task BOUND to a
// schedule whose participant_config lists creator1 + member_x + member_y, so we
// can assert FIX3 strips a departed/removed member from participant_config.
func seedScheduledMultiPersonTask(t *testing.T, db *gorm.DB) (taskID int64, scheduleID int64) {
	t.Helper()
	now := timezone.Now()

	cfg := model.ScheduleParticipantConfig{
		Participants: []model.ScheduleParticipantEntry{
			{UserID: "creator1", UserName: "creator1", Confirmed: true, ConfirmedAt: &now},
			{UserID: "member_x", UserName: "member_x", Confirmed: true, ConfirmedAt: &now},
			{UserID: "member_y", UserName: "member_y", Confirmed: true, ConfirmedAt: &now},
		},
		ConfirmGatePassed: true,
	}
	raw, err := cfg.Marshal()
	if err != nil {
		t.Fatalf("marshal cfg: %v", err)
	}
	sched := model.SummarySchedule{
		SpaceID:           "space1",
		CreatorID:         "creator1",
		ParticipantConfig: raw,
	}
	db.Create(&sched)

	task := model.SummaryTask{
		TaskNo:      "TST-SCHED-001",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
		ScheduleID:  &sched.ID,
	}
	db.Create(&task)
	db.Create(&model.SummarySource{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "grp_abc"})
	for _, uid := range []string{"creator1", "member_x", "member_y"} {
		p := model.SummaryParticipant{TaskID: task.ID, UserID: uid, UserName: uid, Status: model.ParticipantCompleted}
		db.Create(&p)
	}
	return task.ID, sched.ID
}

// FIX3: leaving a schedule-bound task must strip the departing member from the
// schedule's participant_config, otherwise the next scheduled round
// re-materializes them (they "come back").
func TestLeave_StripsScheduleParticipantConfig(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID, scheduleID := seedScheduledMultiPersonTask(t, db)
	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupLeaveRemoveRouter(h)

	w := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/leave", taskID), "member_x", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for leave, got %d: %s", w.Code, w.Body.String())
	}

	var sched model.SummarySchedule
	if err := db.First(&sched, scheduleID).Error; err != nil {
		t.Fatalf("reload schedule: %v", err)
	}
	cfg := model.ParseScheduleParticipantConfig(sched.ParticipantConfig)
	if cfg.FindParticipant("member_x") != nil {
		t.Errorf("leave must remove member_x from schedule participant_config, still present: %+v", cfg.Participants)
	}
	if cfg.FindParticipant("member_y") == nil {
		t.Errorf("leave must keep member_y in schedule participant_config")
	}
	if cfg.FindParticipant("creator1") == nil {
		t.Errorf("leave must keep creator1 in schedule participant_config")
	}
}

// FIX3: creator removing a member must likewise strip them from the schedule's
// participant_config.
func TestRemoveMember_StripsScheduleParticipantConfig(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID, scheduleID := seedScheduledMultiPersonTask(t, db)
	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupLeaveRemoveRouter(h)

	w := doJSONRequest(r, "DELETE", fmt.Sprintf("/api/v1/summaries/%d/members?uid=%s", taskID, "member_x"), "creator1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for remove-member, got %d: %s", w.Code, w.Body.String())
	}

	var sched model.SummarySchedule
	if err := db.First(&sched, scheduleID).Error; err != nil {
		t.Fatalf("reload schedule: %v", err)
	}
	cfg := model.ParseScheduleParticipantConfig(sched.ParticipantConfig)
	if cfg.FindParticipant("member_x") != nil {
		t.Errorf("remove must strip member_x from schedule participant_config, still present: %+v", cfg.Participants)
	}
	if cfg.FindParticipant("creator1") == nil {
		t.Errorf("remove must keep creator1 in schedule participant_config")
	}
}

// DEADLOCK-ORDER (P1 round 4): a schedule-bound task's Leave/RemoveMember now
// acquires its row locks in the global schedule->task order (schedule row FIRST
// when task.ScheduleID != nil, THEN the task row), matching UpdateSchedule /
// DeleteSchedule / scheduler so a removal cannot deadlock against a concurrent
// schedule-side tx. This case asserts the schedule-bound happy path still works
// end to end (participant + personal_result physically removed, schedule config
// stripped). NOTE: sqlite silently drops FOR UPDATE, so this does NOT reproduce
// the deadlock itself; the lock-ORDER fix is verified by static review (see the
// task report's lock-order consistency table). This guards against a regression
// where adding the conditional schedule lock breaks the normal flow.
func TestLeave_ScheduleBound_HappyPathStillWorks(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID, _ := seedScheduledMultiPersonTask(t, db)
	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupLeaveRemoveRouter(h)

	w := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/leave", taskID), "member_x", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for schedule-bound leave, got %d: %s", w.Code, w.Body.String())
	}

	var pcount int64
	db.Model(&model.SummaryParticipant{}).
		Where("task_id = ? AND user_id = ?", taskID, "member_x").Count(&pcount)
	if pcount != 0 {
		t.Errorf("leave must physically remove member_x participant row, found %d", pcount)
	}
	var rcount int64
	db.Model(&model.PersonalResult{}).
		Where("task_id = ? AND user_id = ?", taskID, "member_x").Count(&rcount)
	if rcount != 0 {
		t.Errorf("leave must physically remove member_x personal_result row, found %d", rcount)
	}
}

// DEADLOCK-ORDER (P1 round 4): creator removing a schedule-bound member must
// likewise stay on the schedule->task lock order and complete the happy path.
// Same sqlite FOR UPDATE caveat as TestLeave_ScheduleBound_HappyPathStillWorks.
func TestRemoveMember_ScheduleBound_HappyPathStillWorks(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID, _ := seedScheduledMultiPersonTask(t, db)
	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupLeaveRemoveRouter(h)

	w := doJSONRequest(r, "DELETE", fmt.Sprintf("/api/v1/summaries/%d/members?uid=%s", taskID, "member_x"), "creator1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for schedule-bound remove-member, got %d: %s", w.Code, w.Body.String())
	}

	var pcount int64
	db.Model(&model.SummaryParticipant{}).
		Where("task_id = ? AND user_id = ?", taskID, "member_x").Count(&pcount)
	if pcount != 0 {
		t.Errorf("remove must physically remove member_x participant row, found %d", pcount)
	}
	var rcount int64
	db.Model(&model.PersonalResult{}).
		Where("task_id = ? AND user_id = ?", taskID, "member_x").Count(&rcount)
	if rcount != 0 {
		t.Errorf("remove must physically remove member_x personal_result row, found %d", rcount)
	}
}

// BE-1: editing one's own personal report on an ALREADY-Completed multi-person
// BY_PERSON task must revive the task back to Processing (inside the same
// transaction as the report Update) so the dispatched meta_summary recompute can
// actually write the new team summary. Without the revive the task stays
// Completed and the meta worker aborts ("no longer processing before result
// write"), so the edit never reaches the team summary. FAIL-before/PASS-after.
func TestPersonalEdit_CompletedTask_RevivesAndRecomputes(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID := seedMultiPersonTask(t, db) // BY_PERSON, Completed, creator + 2 members, each w/ PR
	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupPersonalEditRouter(h)

	var before model.SummaryTask
	db.First(&before, taskID)
	if before.Status != model.StatusCompleted {
		t.Fatalf("precondition: task must start Completed, got %d", before.Status)
	}

	body := map[string]interface{}{"content": "member_x EDITED their report after completion"}
	w := doJSONRequest(r, "PUT", fmt.Sprintf("/api/v1/summaries/%d/personal-edit", taskID), "member_x", body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for personal-edit, got %d: %s", w.Code, w.Body.String())
	}

	var after model.SummaryTask
	db.First(&after, taskID)
	if after.Status != model.StatusProcessing {
		t.Errorf("FAIL-BEFORE/PASS-AFTER: Completed BY_PERSON task must be revived to Processing on personal-edit, got status=%d", after.Status)
	}
	if after.ProcessingDeadline == nil || !after.ProcessingDeadline.After(timezone.Now()) {
		t.Errorf("revived task must have a future processing_deadline, got %v", after.ProcessingDeadline)
	}

	// The edit itself must have landed.
	var prX model.PersonalResult
	db.Where("task_id = ? AND user_id = ?", taskID, "member_x").First(&prX)
	if prX.Content != "member_x EDITED their report after completion" || prX.EditedAt == nil {
		t.Errorf("member_x report not updated: content=%q edited_at=%v", prX.Content, prX.EditedAt)
	}

	// The recompute must be dispatched.
	if !tc.waitFor("meta_summary", taskID) {
		t.Errorf("expected a meta_summary trigger after personal-edit revive")
	}
}

// 续修1: editing one's own report on a SINGLE-participant Completed BY_PERSON task
// must NOT revive it (no other members' content to re-aggregate). The task stays
// Completed; only the meta_summary trigger fires. Mirrors the Accept path's
// participantCount>1 guard.
func TestPersonalEdit_SingleParticipant_NoRevive(t *testing.T) {
	db := setupMembersTestDB(t)
	now := timezone.Now()
	task := model.SummaryTask{
		TaskNo:      "TST-PE-SOLO",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
	}
	db.Create(&task)
	db.Create(&model.SummarySource{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "grp_abc"})
	// Exactly ONE participant (the creator) with their own PersonalResult.
	p := model.SummaryParticipant{TaskID: task.ID, UserID: "creator1", UserName: "creator1", Status: model.ParticipantCompleted}
	db.Create(&p)
	db.Create(&model.PersonalResult{
		TaskID:           task.ID,
		ParticipantRefID: p.ID,
		UserID:           "creator1",
		Content:          "creator1 original [1]",
		CitationsJSON:    `[{"index":1,"sender":"s","content":"c","channel_id":"grp_abc"}]`,
		WorkerStatus:     model.PersonalStatusCompleted,
		GeneratedAt:      &now,
	})

	tc := newTriggerCapture(t)
	h := NewPersonalHandler(db, tc.url(), nil)
	r := setupPersonalEditRouter(h)

	body := map[string]interface{}{"content": "creator1 solo edit"}
	w := doJSONRequest(r, "PUT", fmt.Sprintf("/api/v1/summaries/%d/personal-edit", task.ID), "creator1", body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for solo personal-edit, got %d: %s", w.Code, w.Body.String())
	}

	var after model.SummaryTask
	db.First(&after, task.ID)
	if after.Status != model.StatusCompleted {
		t.Errorf("single-participant Completed task must NOT be revived by personal-edit, got status=%d", after.Status)
	}

	// The edit itself must still have landed.
	var prC model.PersonalResult
	db.Where("task_id = ? AND user_id = ?", task.ID, "creator1").First(&prC)
	if prC.Content != "creator1 solo edit" || prC.EditedAt == nil {
		t.Errorf("solo report not updated: content=%q edited_at=%v", prC.Content, prC.EditedAt)
	}
}

// BE-3: a personal-edit carrying a space_id that does not match the task's space
// must be rejected as 40008 ("任务不存在"), identical to a missing task, so task
// existence is not leaked across spaces.
func TestPersonalEdit_WrongSpace_Returns404(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID := seedMultiPersonTask(t, db) // task lives in space1
	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalEditRouter(h)

	// member_x is a real participant, but presents a different space header.
	b, _ := json.Marshal(map[string]interface{}{"content": "cross-space edit attempt"})
	w := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", fmt.Sprintf("/api/v1/summaries/%d/personal-edit", taskID), bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Token", "member_x")
	req.Header.Set("X-Space-Id", "other_space")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for cross-space personal-edit, got %d: %s", w.Code, w.Body.String())
	}
	assertBizCode(t, w, 40008)

	// The report must NOT have been modified.
	var prX model.PersonalResult
	db.Where("task_id = ? AND user_id = ?", taskID, "member_x").First(&prX)
	if prX.EditedAt != nil || prX.Content != "member_x original [1]" {
		t.Errorf("cross-space edit must not modify the report: content=%q edited_at=%v", prX.Content, prX.EditedAt)
	}
}

// ---------------------------------------------------------------------------
// FIX5 (P1 round 5): nil->non-nil rebind window on Leave/RemoveMember.
//
// The window: Leave/RemoveMember read task.ScheduleID OUTSIDE the tx (during the
// auth check). A concurrent CreateSchedule (manual->scheduled) can rebind
// schedule_id nil->non-nil in between. If the out-of-tx value (nil) were trusted,
// the conditional schedule lock + stripScheduleParticipant would be SKIPPED,
// leaving the departed member in participant_config (re-materialized next round).
//
// The fix re-peeks the LIVE schedule_id inside the tx (unlocked, preserving the
// schedule->task lock order) and bails out retryable (errRebindConcurrentModified
// -> 409/40916) on mismatch; the client's retry then observes the real binding and
// strips correctly via the round-4 "lock schedule then task then strip" path.
//
// HONEST TEST-SEAM NOTE: a gin handler test cannot deterministically interleave
// "out-of-tx read happens, THEN concurrent commit, THEN in-tx peek" because both
// reads live inside the handler with no injectable hook between them. sqlite also
// silently drops FOR UPDATE, so real concurrent timing is not reproducible in unit
// tests (same caveat as the round-4 happy-path tests). Therefore we test the LAYER
// THE FIX ADDS directly: (1) peekTaskScheduleID reads the live schedule_id inside a
// real tx; (2) int64PtrEqual against the stale out-of-tx value detects the mismatch;
// (3) errRebindConcurrentModified is classified retryable; (4) it maps to 409/40916.
// This covers the decision logic end to end except the un-injectable wall-clock race.

func TestFix5_PeekDetectsRebindMismatch_AndIsRetryable(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID, scheduleID := seedScheduledMultiPersonTask(t, db)

	// Simulate the dangerous case: the handler's out-of-tx auth read observed a
	// STALE schedule_id of nil (as it would right before a concurrent
	// CreateSchedule committed the nil->non-nil rebind). The live row, however, is
	// already bound to scheduleID.
	var staleOutOfTxScheduleID *int64 // nil == "task looked manual a moment ago"

	var (
		livePeek  *int64
		peekErr   error
		mismatch  bool
	)
	if err := db.Transaction(func(tx *gorm.DB) error {
		// This is exactly what the patched Leave/RemoveMember do at the top of the tx.
		livePeek, peekErr = peekTaskScheduleID(tx, "space1", "creator1", taskID)
		if peekErr != nil {
			return peekErr
		}
		mismatch = !int64PtrEqual(livePeek, staleOutOfTxScheduleID)
		if mismatch {
			return errRebindConcurrentModified
		}
		return nil
	}); err != nil {
		// Expected: the tx returns the retryable sentinel.
		if !isScheduleRetryableConflict(err) {
			t.Fatalf("expected retryable rebind conflict, got non-retryable: %v", err)
		}
	} else {
		t.Fatalf("expected tx to bail out on rebind mismatch, but it succeeded")
	}

	if peekErr != nil {
		t.Fatalf("peekTaskScheduleID returned error: %v", peekErr)
	}
	if livePeek == nil || *livePeek != scheduleID {
		t.Fatalf("peek must read the LIVE schedule_id %d, got %v", scheduleID, livePeek)
	}
	if !mismatch {
		t.Fatalf("int64PtrEqual must report mismatch between stale nil and live %d", scheduleID)
	}

	// And the retryable sentinel must serialize to the 409/40916 the client retries on.
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	writeRetryableRebindConflict(c)
	if w.Code != http.StatusConflict {
		t.Fatalf("writeRetryableRebindConflict must write 409, got %d", w.Code)
	}
	var resp apiResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal 409 body: %v", err)
	}
	if resp.Code != 40916 {
		t.Fatalf("rebind conflict must carry Code 40916, got %d", resp.Code)
	}
}

// Guards the OTHER direction of int64PtrEqual used by the peek-compare: when the
// out-of-tx value already MATCHES the live binding (the common case + the retry
// case where the rebind has settled), the peek must NOT bail out, so the normal
// round-4 strip path runs. This is the inverse assertion that keeps the happy path
// from regressing into a spurious 40916.
func TestFix5_PeekMatchesLiveBinding_NoBailout(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID, scheduleID := seedScheduledMultiPersonTask(t, db)

	// The out-of-tx read agrees with the live binding (e.g. the retried request, or
	// the non-racy common path).
	outOfTx := &scheduleID

	var bailed bool
	if err := db.Transaction(func(tx *gorm.DB) error {
		livePeek, err := peekTaskScheduleID(tx, "space1", "creator1", taskID)
		if err != nil {
			return err
		}
		if !int64PtrEqual(livePeek, outOfTx) {
			bailed = true
			return errRebindConcurrentModified
		}
		return nil
	}); err != nil {
		t.Fatalf("tx must not error when peek matches live binding: %v", err)
	}
	if bailed {
		t.Fatalf("peek-compare must NOT bail out when out-of-tx value matches live schedule_id %d", scheduleID)
	}
}

// ---------------------------------------------------------------------------
// P1 cross-space (space_id) isolation regression tests.
//
// Each task is seeded in space1. Calling the same task with a DIFFERENT (or
// empty) X-Space-Id must 404/40008 -- identical to a missing task -- so a task
// cannot be read, accepted, submitted, declined, or have its personal report
// fetched across spaces. FAIL-before (no space_id filter -> the (task_id,user_id)
// row matched and the call was wrongly let through) / PASS-after (space_id
// filter makes the task invisible cross-space, so 404).
// ---------------------------------------------------------------------------

// doWrongSpaceRequest issues a request carrying an explicit X-Space-Id that does
// NOT match the task's space (so we can exercise the cross-space 404 path; the
// shared doRequest/doJSONRequest helpers hardcode space1).
func doWrongSpaceRequest(r *gin.Engine, method, path, userID, spaceID string, body interface{}) *httptest.ResponseRecorder {
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if userID != "" {
		req.Header.Set("Token", userID)
	}
	if spaceID != "" {
		req.Header.Set("X-Space-Id", spaceID)
	}
	r.ServeHTTP(w, req)
	return w
}

func assertCrossSpace404(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for cross-space access, got %d: %s", w.Code, w.Body.String())
	}
	assertBizCode(t, w, 40008)
}

// setupPersonalActionRouter wires the (task_id,user_id)-only personal endpoints
// (accept/decline/submit/personal) that gained the F2 space_id pre-check.
func setupPersonalActionRouter(h *PersonalHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.POST("/api/v1/summaries/:id/accept", h.Accept)
	r.POST("/api/v1/summaries/:id/decline", h.Decline)
	r.POST("/api/v1/summaries/:id/submit", h.Submit)
	r.GET("/api/v1/summaries/:id/personal", h.GetPersonal)
	return r
}

// P1-F1: GetSummary gates through TaskHandler.authorizeTaskAccess, which now
// scopes the task load by space_id. A real participant of a space1 task who
// presents a different X-Space-Id must get 404/40008 (existence not leaked, no
// cross-space read). FAIL-before: the load was `id = ? AND deleted_at IS NULL`
// (space-blind) so the cross-space caller saw the task; PASS-after: 404.
func TestAuthorizeTaskAccess_WrongSpace_Returns404(t *testing.T) {
	db, imDB := setupTestDBs(t)
	taskID := seedTask(t, db, imDB) // space1, creator1, participant1
	h := NewTaskHandler(db, imDB, "")
	r := setupRouter(h) // wires GET /api/v1/summaries/:id -> GetSummary

	// participant1 is genuinely on the roster, but reads with the wrong space.
	w := doWrongSpaceRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", taskID), "participant1", "other_space", nil)
	assertCrossSpace404(t, w)

	// Sanity: the SAME caller with the CORRECT space still gets 200 (no regression).
	wOK := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", taskID), "participant1")
	if wOK.Code != http.StatusOK {
		t.Fatalf("in-space participant must still get 200, got %d: %s", wOK.Code, wOK.Body.String())
	}
}

// P1-F1: an EMPTY X-Space-Id now also 404s (space_id='' matches no real task),
// confirming P1-F3: the empty-space cross-space read path is sealed by F1 with no
// middleware change.
func TestAuthorizeTaskAccess_EmptySpace_Returns404(t *testing.T) {
	db, imDB := setupTestDBs(t)
	taskID := seedTask(t, db, imDB)
	h := NewTaskHandler(db, imDB, "")
	r := setupRouter(h)

	w := doWrongSpaceRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", taskID), "participant1", "", nil)
	assertCrossSpace404(t, w)
}

// P1-F2: Accept with a mismatched space must 404/40008 before touching the
// (task_id,user_id) participant row -- a cross-space caller cannot accept (and
// thereby mutate) another space's participant. FAIL-before: no space check, the
// participant lookup succeeded and the accept proceeded; PASS-after: 404 and the
// participant stays Pending.
func TestAccept_WrongSpace_Returns404(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID := seedMultiPersonTask(t, db) // space1; member_x is a participant
	// Force member_x to Pending so a (wrongly) accepted call would visibly flip it.
	db.Model(&model.SummaryParticipant{}).
		Where("task_id = ? AND user_id = ?", taskID, "member_x").
		Update("status", model.ParticipantPending)
	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalActionRouter(h)

	w := doWrongSpaceRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/accept", taskID), "member_x", "other_space", nil)
	assertCrossSpace404(t, w)

	// The participant must NOT have been accepted by the cross-space call.
	var p model.SummaryParticipant
	db.Where("task_id = ? AND user_id = ?", taskID, "member_x").First(&p)
	if p.Status != model.ParticipantPending {
		t.Errorf("cross-space accept must not mutate participant; status=%d", p.Status)
	}

	// Sanity: the correct space still accepts (200).
	wOK := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/accept", taskID), "member_x", nil)
	if wOK.Code != http.StatusOK {
		t.Fatalf("in-space accept must still 200, got %d: %s", wOK.Code, wOK.Body.String())
	}
}

// P1-F2: Decline with a mismatched space must 404/40008 and leave the
// participant untouched.
func TestDecline_WrongSpace_Returns404(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID := seedMultiPersonTask(t, db) // space1
	db.Model(&model.SummaryParticipant{}).
		Where("task_id = ? AND user_id = ?", taskID, "member_x").
		Update("status", model.ParticipantPending)
	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalActionRouter(h)

	w := doWrongSpaceRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/decline", taskID), "member_x", "other_space", nil)
	assertCrossSpace404(t, w)

	var p model.SummaryParticipant
	db.Where("task_id = ? AND user_id = ?", taskID, "member_x").First(&p)
	if p.Status != model.ParticipantPending {
		t.Errorf("cross-space decline must not mutate participant; status=%d", p.Status)
	}
}

// P1-F2: Submit with a mismatched space must 404/40008 before reading/writing
// the personal_result row -- so a cross-space caller cannot submit another
// space's report. FAIL-before: the (task_id,user_id) personal_result lookup
// succeeded and submit proceeded; PASS-after: 404 and submitted_at stays nil.
func TestSubmit_WrongSpace_Returns404(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID := seedMultiPersonTask(t, db) // space1; each member has a PR
	// Make member_x's report completed-but-unsubmitted so a wrongly-allowed submit
	// would visibly set submitted_at.
	db.Model(&model.PersonalResult{}).
		Where("task_id = ? AND user_id = ?", taskID, "member_x").
		Updates(map[string]interface{}{
			"worker_status": model.PersonalStatusCompleted,
			"submitted_at":  nil,
		})
	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalActionRouter(h)

	w := doWrongSpaceRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/submit", taskID), "member_x", "other_space", nil)
	assertCrossSpace404(t, w)

	var pr model.PersonalResult
	db.Where("task_id = ? AND user_id = ?", taskID, "member_x").First(&pr)
	if pr.SubmittedAt != nil {
		t.Errorf("cross-space submit must not mark the report submitted; submitted_at=%v", pr.SubmittedAt)
	}

	// Sanity: the correct space still submits (200).
	wOK := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/submit", taskID), "member_x", nil)
	if wOK.Code != http.StatusOK {
		t.Fatalf("in-space submit must still 200, got %d: %s", wOK.Code, wOK.Body.String())
	}
}

// P1-F2: GetPersonal with a mismatched space must 404/40008 -- the cross-space
// caller must NOT be able to coax the "no pr -> default-shaped 200" fallback out
// of another space's task. The in-space missing-pr default behaviour is verified
// separately to prove the F2 check did not break it.
func TestGetPersonal_WrongSpace_Returns404(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID := seedMultiPersonTask(t, db) // space1; member_x has a PR
	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalActionRouter(h)

	// member_x is a real participant w/ a report, but reads with the wrong space.
	w := doWrongSpaceRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d/personal", taskID), "member_x", "other_space", nil)
	assertCrossSpace404(t, w)

	// In-space, the missing-pr default fallback is preserved: a participant with NO
	// personal_result still gets a default-shaped 200 (behaviour unchanged by F2).
	now := timezone.Now()
	soloTask := model.SummaryTask{
		TaskNo:      "TST-GP-DEFAULT",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
	}
	db.Create(&soloTask)
	db.Create(&model.SummaryParticipant{TaskID: soloTask.ID, UserID: "noresult", UserName: "noresult", Status: model.ParticipantPending, CreatedAt: now})
	wDefault := doJSONRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d/personal", soloTask.ID), "noresult", nil)
	if wDefault.Code != http.StatusOK {
		t.Fatalf("in-space participant with no personal_result must still get default 200, got %d: %s", wDefault.Code, wDefault.Body.String())
	}
}

// ---------------------------------------------------------------------------
// P1-F3 (reviewer follow-up): empty-X-Space-Id fail-closed HARD gate.
//
// SummaryTask.SpaceID is `not null default ''` (model.go / baseline migration),
// so a task with space_id='' CAN exist (historical/anomalous rows). The earlier
// "empty header -> space_id='' -> 404 naturally" argument is therefore WRONG:
// without an explicit guard, an empty X-Space-Id (GetSpaceID(c) == "") makes the
// query `space_id=''` which would MATCH such a task and let the call through
// (fail-open cross-space read). authorizeTaskAccess / requireTaskInSpace now
// short-circuit to 40008/404 whenever spaceID=="" BEFORE the query, regardless
// of any data invariant.
//
// These tests SEED a real SpaceID:"" task (+ participant / personal_result) so
// the distinction is genuine: FAIL-before (no spaceID=="" short-circuit) -> the
// empty-header query `space_id=''` matches the seeded task and returns 200;
// PASS-after -> 404/40008. (Unlike TestAuthorizeTaskAccess_EmptySpace_Returns404,
// which seeds only a space1 task and so passes even without the new guard.)
// ---------------------------------------------------------------------------

// seedEmptySpaceTask seeds a task whose SpaceID is the empty string, plus a
// participant and a completed personal_result for `participant1`, mirroring the
// shape seedTask/seedMultiPersonTask use but with space_id=''.
func seedEmptySpaceTask(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	now := timezone.Now()
	task := model.SummaryTask{
		TaskNo:      "TST-EMPTYSPACE-001",
		SpaceID:     "", // <-- the anomalous space_id='' row the guard must hide
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
	}
	db.Create(&task)
	db.Create(&model.SummarySource{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "grp_abc"})

	p := model.SummaryParticipant{TaskID: task.ID, UserID: "participant1", UserName: "P1", Status: model.ParticipantCompleted}
	db.Create(&p)
	db.Create(&model.PersonalResult{
		TaskID:           task.ID,
		ParticipantRefID: p.ID,
		UserID:           "participant1",
		Content:          "participant1 original [1]",
		CitationsJSON:    `[{"index":1,"sender":"s","content":"c","channel_id":"grp_abc"}]`,
		WorkerStatus:     model.PersonalStatusCompleted,
		GeneratedAt:      &now,
	})
	return task.ID
}

// P1-F3: authorizeTaskAccess (GetSummary) — a genuine participant of a
// space_id='' task, calling with NO X-Space-Id header, must STILL get 404/40008.
// FAIL-before: the empty-header query `space_id=''` matched this seeded task and
// returned 200 (cross-space read of an anomalous row). PASS-after: the spaceID==""
// short-circuit 404s before the query.
func TestAuthorizeTaskAccess_EmptySpaceHeader_EmptySpaceTask_Returns404(t *testing.T) {
	db, imDB := setupTestDBs(t)
	taskID := seedEmptySpaceTask(t, db)
	h := NewTaskHandler(db, imDB, "")
	r := setupRouter(h)

	// participant1 is a real participant of the space_id='' task; no X-Space-Id.
	w := doWrongSpaceRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", taskID), "participant1", "", nil)
	assertCrossSpace404(t, w)
}

// P1-F3: Accept — empty X-Space-Id against a real participant of a space_id=''
// task must 404/40008 and must NOT mutate the participant. FAIL-before: the
// empty-header requireTaskInSpace query matched (space_id='') and the accept
// proceeded; PASS-after: 404, participant untouched.
func TestAccept_EmptySpaceHeader_Returns404(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID := seedEmptySpaceTask(t, db)
	db.Model(&model.SummaryParticipant{}).
		Where("task_id = ? AND user_id = ?", taskID, "participant1").
		Update("status", model.ParticipantPending)
	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalActionRouter(h)

	w := doWrongSpaceRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/accept", taskID), "participant1", "", nil)
	assertCrossSpace404(t, w)

	var p model.SummaryParticipant
	db.Where("task_id = ? AND user_id = ?", taskID, "participant1").First(&p)
	if p.Status != model.ParticipantPending {
		t.Errorf("empty-space accept must not mutate participant; status=%d", p.Status)
	}
}

// P1-F3: Submit — empty X-Space-Id against a real participant of a space_id=''
// task must 404/40008 and must NOT mark the report submitted. FAIL-before: the
// empty-header requireTaskInSpace query matched and submit set submitted_at;
// PASS-after: 404, submitted_at stays nil.
func TestSubmit_EmptySpaceHeader_Returns404(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID := seedEmptySpaceTask(t, db)
	db.Model(&model.PersonalResult{}).
		Where("task_id = ? AND user_id = ?", taskID, "participant1").
		Updates(map[string]interface{}{
			"worker_status": model.PersonalStatusCompleted,
			"submitted_at":  nil,
		})
	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalActionRouter(h)

	w := doWrongSpaceRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/submit", taskID), "participant1", "", nil)
	assertCrossSpace404(t, w)

	var pr model.PersonalResult
	db.Where("task_id = ? AND user_id = ?", taskID, "participant1").First(&pr)
	if pr.SubmittedAt != nil {
		t.Errorf("empty-space submit must not mark the report submitted; submitted_at=%v", pr.SubmittedAt)
	}
}

// P1-F3: GetPersonal — empty X-Space-Id against a real participant (with a
// personal_result) of a space_id='' task must 404/40008, not leak the report.
// FAIL-before: the empty-header requireTaskInSpace query matched and the report
// was returned; PASS-after: 404.
func TestGetPersonal_EmptySpaceHeader_Returns404(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID := seedEmptySpaceTask(t, db)
	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalActionRouter(h)

	w := doWrongSpaceRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d/personal", taskID), "participant1", "", nil)
	assertCrossSpace404(t, w)
}

// --- P1-B: AddMembers must reject TERMINAL tasks (Failed / Cancelled) ---
//
// Root cause: AddMembers loaded the task (creator gate) but never checked its
// status, so a member could be added to a Failed/Cancelled task. The new member
// is created Pending and has NO recovery path: Accept's revive CAS is
// WHERE status=Completed and the worker's task CAS also misses a terminal task,
// so the member is stuck Pending forever. The status gate only blocks the two
// TRUE terminal states (StatusFailed=4 / StatusCancelled=5); Pending /
// WaitingConfirm / Processing / Completed are still allowed (Completed is this
// PR's revive-recompute scenario and must NOT be blocked).

// seedTaskWithStatus creates a single-creator task in the given status, used to
// drive the AddMembers status gate. Returns the task ID.
func seedTaskWithStatus(t *testing.T, db *gorm.DB, status int) int64 {
	t.Helper()
	task := model.SummaryTask{
		TaskNo:      fmt.Sprintf("TST-P1B-%d", status),
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      status,
	}
	db.Create(&task)
	db.Create(&model.SummarySource{TaskID: task.ID, SourceType: model.SourceGroup, SourceID: "grp_abc"})
	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "creator1", UserName: "creator1", Status: model.ParticipantCompleted})
	return task.ID
}

func TestAddMembers_TaskFailed_Rejected(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID := seedTaskWithStatus(t, db, model.StatusFailed)
	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalEditRouter(h)

	body := map[string]interface{}{"user_ids": []string{"newcomer1"}}
	w := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/members", taskID), "creator1", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 adding to a Failed task, got %d: %s", w.Code, w.Body.String())
	}
	assertBizCode(t, w, 40005)
	// No participant must have been inserted for the new member.
	var cnt int64
	db.Model(&model.SummaryParticipant{}).Where("task_id = ? AND user_id = ?", taskID, "newcomer1").Count(&cnt)
	if cnt != 0 {
		t.Errorf("rejected add must NOT insert a participant, got %d", cnt)
	}
}

func TestAddMembers_TaskCancelled_Rejected(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID := seedTaskWithStatus(t, db, model.StatusCancelled)
	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalEditRouter(h)

	body := map[string]interface{}{"user_ids": []string{"newcomer1"}}
	w := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/members", taskID), "creator1", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 adding to a Cancelled task, got %d: %s", w.Code, w.Body.String())
	}
	assertBizCode(t, w, 40005)
	var cnt int64
	db.Model(&model.SummaryParticipant{}).Where("task_id = ? AND user_id = ?", taskID, "newcomer1").Count(&cnt)
	if cnt != 0 {
		t.Errorf("rejected add must NOT insert a participant, got %d", cnt)
	}
}

// TestAddMembers_TaskCompleted_Allowed: a Completed task must STILL accept new
// members -- this is the PR's revive-recompute scenario; the status gate must
// only block Failed/Cancelled, never Completed.
func TestAddMembers_TaskCompleted_Allowed(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID := seedMultiPersonTask(t, db) // Completed BY_PERSON, creator + 2 members
	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalEditRouter(h)

	body := map[string]interface{}{"user_ids": []string{"newcomer1"}}
	w := doJSONRequest(r, "POST", fmt.Sprintf("/api/v1/summaries/%d/members", taskID), "creator1", body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 adding to a Completed task (revive-recompute), got %d: %s", w.Code, w.Body.String())
	}
	// The new member must have been inserted (Pending).
	var p model.SummaryParticipant
	if err := db.Where("task_id = ? AND user_id = ?", taskID, "newcomer1").First(&p).Error; err != nil {
		t.Fatalf("Completed-task add must insert the new participant: %v", err)
	}
	if p.Status != model.ParticipantPending {
		t.Errorf("new member must be Pending, got status=%d", p.Status)
	}
}

// ---------------------------------------------------------------------------
// P1 (fixer-104): PersonalEdit check-order + GET empty-space fail-closed gate.
//
// Bug 1: PersonalEdit ran the (task_id,user_id) participant check BEFORE the
// space-isolation check, so a non-participant got 403/40003 ("你不是该任务的参与者")
// while a missing/cross-space task got 404/40008 -- a cross-space participant-
// enumeration + existence oracle. The fix moves requireTaskInSpace FIRST (with
// its spaceID=="" hard gate) and returns 40008 for the in-space non-participant,
// matching Accept, so neither existence nor participation leaks across spaces.
//
// Bug 2: GET endpoints are not caught by StrictSpaceMiddleware and SpaceID is
// `not null default ''`, so an empty X-Space-Id could match a space_id='' row.
// GetMembers (here) now hard-gates spaceID=="" -> 40008 before any query.
// ---------------------------------------------------------------------------

// Bug 1: empty X-Space-Id against a real participant of a space_id='' task must
// 40008 (not leak the report) -- the requireTaskInSpace empty-space hard gate now
// runs BEFORE the participant lookup. FAIL-before: the participant check ran
// first (or the empty-header query matched space_id='') and the edit proceeded;
// PASS-after: 40008 and the report stays untouched.
func TestPersonalEdit_EmptySpaceHeader_Returns404(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID := seedEmptySpaceTask(t, db) // space_id='' task, participant1 has a PR
	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalEditRouter(h)

	w := doWrongSpaceRequest(r, "PUT", fmt.Sprintf("/api/v1/summaries/%d/personal-edit", taskID),
		"participant1", "", map[string]interface{}{"content": "empty-space edit attempt"})
	assertCrossSpace404(t, w)

	// The report must NOT have been modified.
	var pr model.PersonalResult
	db.Where("task_id = ? AND user_id = ?", taskID, "participant1").First(&pr)
	if pr.EditedAt != nil || pr.Content != "participant1 original [1]" {
		t.Errorf("empty-space edit must not modify the report: content=%q edited_at=%v", pr.Content, pr.EditedAt)
	}
}

// Bug 1 (the oracle itself): a NON-participant calling cross-space must get the
// SAME 40008 as any other miss -- NOT the old 40003 ("你不是该任务的参与者").
// FAIL-before: the participant check ran first, so a cross-space stranger saw
// 403/40003 (leaking that the task exists in some other space AND that they are
// not on its roster), distinguishable from the 40008 a non-existent task gives.
// PASS-after: requireTaskInSpace 404s first -> uniform 40008, no oracle.
func TestPersonalEdit_CrossSpaceNonParticipant_Returns404NotLeak(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID := seedMultiPersonTask(t, db) // space1; stranger1 is NOT a participant
	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalEditRouter(h)

	w := doWrongSpaceRequest(r, "PUT", fmt.Sprintf("/api/v1/summaries/%d/personal-edit", taskID),
		"stranger1", "other_space", map[string]interface{}{"content": "cross-space non-participant edit"})
	assertCrossSpace404(t, w) // asserts 404 + resp.Code == 40008
	// 40008 (asserted above) already excludes the 40003 participant oracle; assert
	// on the parsed code rather than a brittle substring scan of the raw body.
	var resp apiResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response body: %v; body=%s", err, w.Body.String())
	}
	if resp.Code == 40003 {
		t.Errorf("cross-space non-participant must NOT leak the 40003 participant oracle, body=%s", w.Body.String())
	}
}

// setupMembersOnlyRouter wires ONLY GET /members so the GetMembers empty-space
// gate can be exercised in isolation (setupMembersRouter already does this; kept
// explicit here for the empty-space helper's intent).

// Bug 2: GetMembers with an EMPTY X-Space-Id must 40008 and must NOT return a
// roster -- the GET path is not behind StrictSpaceMiddleware and space_id=''
// rows exist, so without the spaceID=="" hard gate the empty-header query
// `space_id=''` would match the seeded task and leak its member list.
// FAIL-before: empty header -> roster returned (200); PASS-after: 40008, no roster.
func TestGetMembers_EmptySpaceHeader_Returns404NoRoster(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID := seedEmptySpaceTask(t, db) // space_id='' task, creator1 + participant1
	h := NewPersonalHandler(db, "", nil)
	r := setupMembersRouter(h) // GET /api/v1/summaries/:id/members

	// creator1 is a genuine creator of the space_id='' task, but sends no X-Space-Id.
	w := doWrongSpaceRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d/members", taskID), "creator1", "", nil)
	assertCrossSpace404(t, w)

	// The roster must not have leaked through the empty-space request.
	if bytes.Contains(w.Body.Bytes(), []byte("members")) ||
		bytes.Contains(w.Body.Bytes(), []byte("participant1")) {
		t.Errorf("empty-space GetMembers must NOT return the roster, body=%s", w.Body.String())
	}
}

// Sanity inverse for Bug 2: the SAME creator with the CORRECT space still gets
// the roster (200), so the empty-space gate did not over-block in-space reads.
func TestGetMembers_EmptySpaceGate_NoRegressionInSpace(t *testing.T) {
	db := setupMembersTestDB(t)
	taskID := seedMembersTask(t, db) // space1, creator1 + participant1
	h := NewPersonalHandler(db, "", nil)
	r := setupMembersRouter(h)

	w := doRequest(r, "GET", fmt.Sprintf("/api/v1/summaries/%d/members", taskID), "creator1")
	if w.Code != http.StatusOK {
		t.Fatalf("in-space creator must still get 200 roster, got %d: %s", w.Code, w.Body.String())
	}
}
