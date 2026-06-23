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

func setupMembersTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open summary db: %v", err)
	}
	db.AutoMigrate(&model.SummaryTask{}, &model.SummarySource{}, &model.SummaryParticipant{}, &model.PersonalResult{}, &model.SummarySchedule{})
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
	db := setupMembersTestDB(t)
	taskID := seedMultiPersonTask(t, db)
	h := NewPersonalHandler(db, "", nil)
	r := setupPersonalEditRouter(h)

	body := map[string]interface{}{"content": "stranger tries to edit"}
	w := doJSONRequest(r, "PUT", fmt.Sprintf("/api/v1/summaries/%d/personal-edit", taskID), "stranger1", body)
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-participant personal-edit, got %d: %s", w.Code, w.Body.String())
	}
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
	if !bytes.Contains(w.Body.Bytes(), []byte("40008")) {
		t.Errorf("cross-space personal-edit must return code 40008, body=%s", w.Body.String())
	}

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
