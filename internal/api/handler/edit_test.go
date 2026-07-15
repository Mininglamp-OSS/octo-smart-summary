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

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupEditDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.AutoMigrate(
		&model.SummaryTask{},
		&model.SummarySource{},
		&model.SummaryParticipant{},
		&model.PersonalResult{},
		&model.SummaryResult{},
		&model.SummaryChunk{},
	)
	return db
}

func setupEditRouter(h *EditHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.PUT("/api/v1/summaries/:id/edit", h.EditSummary)
	r.POST("/api/v1/summaries/:id/refine", h.RefineSummary)
	r.GET("/api/v1/summaries/:id/versions/:result_id", h.GetSummaryVersion)
	return r
}

func seedEditableTask(t *testing.T, db *gorm.DB) (taskID int64, resultID int64, prID int64) {
	t.Helper()
	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:      "TST-EDIT-001",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
	}
	db.Create(&task)

	participant := model.SummaryParticipant{
		TaskID:   task.ID,
		UserID:   "creator1",
		UserName: "Creator",
		Status:   model.ParticipantCompleted,
	}
	db.Create(&participant)

	pr := model.PersonalResult{
		TaskID:           task.ID,
		ParticipantRefID: participant.ID,
		UserID:           "creator1",
		WorkerStatus:     model.PersonalStatusCompleted,
		Content:          "original content with [1] citation",
		CitationsJSON:    `[{"index":1,"sender":"Alice","content":"hello","sent_at":"2026-01-01T00:00:00Z","source":"grp","channel_id":"ch1","channel_type":2,"message_seq":100}]`,
		GeneratedAt:      &now,
	}
	db.Create(&pr)

	result := model.SummaryResult{
		TaskID:         task.ID,
		Content:        "original content with [1] citation",
		CitationsJSON:  `[{"index":1,"sender":"Alice","content":"hello","sent_at":"2026-01-01T00:00:00Z","source":"grp","channel_id":"ch1","channel_type":2,"message_seq":100}]`,
		TotalMsgCount:  10,
		TotalTokenUsed: 200,
		ModelVersion:   "test-v1",
		Version:        1,
		GeneratedAt:    now,
	}
	db.Create(&result)

	return task.ID, result.ID, pr.ID
}

func doEditRequest(r *gin.Engine, taskID int64, userID string, body interface{}) *httptest.ResponseRecorder {
	var bodyBytes []byte
	if body != nil {
		bodyBytes, _ = json.Marshal(body)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", fmt.Sprintf("/api/v1/summaries/%d/edit", taskID), bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	if userID != "" {
		req.Header.Set("Token", userID)
	}
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)
	return w
}

func doEditRequestWithSpace(r *gin.Engine, taskID int64, userID, spaceID string, body interface{}) *httptest.ResponseRecorder {
	var bodyBytes []byte
	if body != nil {
		bodyBytes, _ = json.Marshal(body)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", fmt.Sprintf("/api/v1/summaries/%d/edit", taskID), bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	if userID != "" {
		req.Header.Set("Token", userID)
	}
	req.Header.Set("X-Space-Id", spaceID)
	r.ServeHTTP(w, req)
	return w
}

func TestEditSummary_WrongSpaceID(t *testing.T) {
	db := setupEditDB(t)
	taskID, resultID, _ := seedEditableTask(t, db)

	h := NewEditHandler(db)
	r := setupEditRouter(h)

	body := map[string]interface{}{
		"content":        "new content",
		"base_result_id": resultID,
	}
	w := doEditRequestWithSpace(r, taskID, "creator1", "wrong_space", body)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for wrong space_id, got %d: %s", w.Code, w.Body.String())
	}
}

func TestEditSummary_WhitespaceOnlyContent(t *testing.T) {
	db := setupEditDB(t)
	taskID, resultID, _ := seedEditableTask(t, db)

	h := NewEditHandler(db)
	r := setupEditRouter(h)

	cases := []string{"   ", "\n\n", "\t\t", "  \n  \t  "}
	for _, content := range cases {
		body := map[string]interface{}{
			"content":        content,
			"base_result_id": resultID,
		}
		w := doEditRequest(r, taskID, "creator1", body)

		if w.Code != http.StatusBadRequest {
			t.Errorf("expected 400 for whitespace-only content %q, got %d: %s", content, w.Code, w.Body.String())
		}
	}
}

func TestEditSummary_Success(t *testing.T) {
	db := setupEditDB(t)
	taskID, resultID, prID := seedEditableTask(t, db)

	h := NewEditHandler(db)
	r := setupEditRouter(h)

	body := map[string]interface{}{
		"content":        "updated content with [1] citation",
		"base_result_id": resultID,
	}
	w := doEditRequest(r, taskID, "creator1", body)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	data := resp["data"].(map[string]interface{})
	if data["edited_at"] == nil {
		t.Error("expected edited_at to be set")
	}

	var sr model.SummaryResult
	db.First(&sr, resultID)
	if sr.Content != "updated content with [1] citation" {
		t.Errorf("SummaryResult content not updated: %q", sr.Content)
	}
	if sr.EditedAt == nil {
		t.Error("SummaryResult edited_at should be set")
	}

	var pr model.PersonalResult
	db.First(&pr, prID)
	if pr.Content != "updated content with [1] citation" {
		t.Errorf("PersonalResult content not updated: %q", pr.Content)
	}
	if pr.EditedAt == nil {
		t.Error("PersonalResult edited_at should be set")
	}
}

func TestEditSummary_CitationCleanup(t *testing.T) {
	db := setupEditDB(t)
	taskID, resultID, _ := seedEditableTask(t, db)

	h := NewEditHandler(db)
	r := setupEditRouter(h)

	body := map[string]interface{}{
		"content":        "updated content without any citation references",
		"base_result_id": resultID,
	}
	w := doEditRequest(r, taskID, "creator1", body)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var sr model.SummaryResult
	db.First(&sr, resultID)
	citations := sr.GetCitations()
	if len(citations) != 0 {
		t.Errorf("expected 0 citations after cleanup, got %d", len(citations))
	}
}

func TestEditSummary_NonCreatorForbidden(t *testing.T) {
	db := setupEditDB(t)
	taskID, resultID, _ := seedEditableTask(t, db)

	db.Create(&model.SummaryParticipant{
		TaskID:   taskID,
		UserID:   "other_user",
		UserName: "Other",
	})

	h := NewEditHandler(db)
	r := setupEditRouter(h)

	body := map[string]interface{}{
		"content":        "hacked content",
		"base_result_id": resultID,
	}
	w := doEditRequest(r, taskID, "other_user", body)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestEditSummary_NonCompletedStatus(t *testing.T) {
	db := setupEditDB(t)
	now := time.Now().UTC()

	task := model.SummaryTask{
		TaskNo:      "TST-EDIT-002",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusProcessing,
	}
	db.Create(&task)

	db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "creator1", UserName: "Creator"})

	result := model.SummaryResult{
		TaskID:      task.ID,
		Content:     "some content",
		Version:     1,
		GeneratedAt: now,
	}
	db.Create(&result)

	h := NewEditHandler(db)
	r := setupEditRouter(h)

	body := map[string]interface{}{
		"content":        "new content",
		"base_result_id": result.ID,
	}
	w := doEditRequest(r, task.ID, "creator1", body)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestEditSummary_BaseResultIDMismatch(t *testing.T) {
	db := setupEditDB(t)
	taskID, resultID, _ := seedEditableTask(t, db)

	h := NewEditHandler(db)
	r := setupEditRouter(h)

	body := map[string]interface{}{
		"content":        "new content",
		"base_result_id": resultID + 999,
	}
	w := doEditRequest(r, taskID, "creator1", body)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
}

func TestEditSummary_Idempotent(t *testing.T) {
	db := setupEditDB(t)
	taskID, resultID, _ := seedEditableTask(t, db)

	h := NewEditHandler(db)
	r := setupEditRouter(h)

	body := map[string]interface{}{
		"content":        "original content with [1] citation",
		"base_result_id": resultID,
	}
	w := doEditRequest(r, taskID, "creator1", body)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for idempotent call, got %d: %s", w.Code, w.Body.String())
	}

	var sr model.SummaryResult
	db.First(&sr, resultID)
	if sr.EditedAt != nil {
		t.Error("edited_at should remain nil for idempotent (no-change) call")
	}
}

func TestEditSummary_EmptyContent(t *testing.T) {
	db := setupEditDB(t)
	taskID, resultID, _ := seedEditableTask(t, db)

	h := NewEditHandler(db)
	r := setupEditRouter(h)

	body := map[string]interface{}{
		"content":        "",
		"base_result_id": resultID,
	}
	w := doEditRequest(r, taskID, "creator1", body)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty content, got %d: %s", w.Code, w.Body.String())
	}
}

func TestEditSummary_TaskNotFound(t *testing.T) {
	db := setupEditDB(t)

	h := NewEditHandler(db)
	r := setupEditRouter(h)

	body := map[string]interface{}{
		"content":        "some content",
		"base_result_id": 999,
	}
	w := doEditRequest(r, 99999, "creator1", body)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestEditSummary_MultiParticipantCreatorAllowedLegacyName(t *testing.T) {
	// need4: this used to assert multi-person edits are rejected (400). The product
	// requirement changed: a multi-person task's CREATOR may now edit the team
	// SummaryResult. Updated to assert the new pass-after behavior (200).
	db := setupEditDB(t)
	taskID, resultID, _ := seedEditableTask(t, db)

	db.Create(&model.SummaryParticipant{
		TaskID:   taskID,
		UserID:   "participant2",
		UserName: "P2",
	})

	h := NewEditHandler(db)
	r := setupEditRouter(h)

	body := map[string]interface{}{
		"content":        "new content",
		"base_result_id": resultID,
	}
	w := doEditRequest(r, taskID, "creator1", body)

	if w.Code != http.StatusOK {
		t.Errorf("need4: multi-person creator edit should now be 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestEditSummary_MultiParticipantCreatorAllowed(t *testing.T) {
	// need4: a multi-person task's CREATOR may now edit the team SummaryResult.
	// Fail-before: edit.go rejected participantCount>1 with 400. Pass-after: 200.
	db := setupEditDB(t)
	taskID, resultID, _ := seedEditableTask(t, db)

	// Make it a genuine multi-person task.
	db.Create(&model.SummaryParticipant{
		TaskID:   taskID,
		UserID:   "participant2",
		UserName: "P2",
		Status:   model.ParticipantCompleted,
	})

	h := NewEditHandler(db)
	r := setupEditRouter(h)

	body := map[string]interface{}{
		"content":        "creator edited the TEAM draft for a multi-person task",
		"base_result_id": resultID,
	}
	w := doEditRequest(r, taskID, "creator1", body)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for multi-person creator team edit, got %d: %s", w.Code, w.Body.String())
	}

	var sr model.SummaryResult
	db.First(&sr, resultID)
	if sr.Content != "creator edited the TEAM draft for a multi-person task" {
		t.Errorf("team SummaryResult content not updated: %q", sr.Content)
	}
}

func TestEditSummary_MultiParticipantNonCreatorForbidden(t *testing.T) {
	// need4: in a multi-person task a non-creator participant must NOT be able to
	// edit the team SummaryResult.
	db := setupEditDB(t)
	taskID, resultID, _ := seedEditableTask(t, db)

	db.Create(&model.SummaryParticipant{
		TaskID:   taskID,
		UserID:   "participant2",
		UserName: "P2",
		Status:   model.ParticipantCompleted,
	})

	h := NewEditHandler(db)
	r := setupEditRouter(h)

	body := map[string]interface{}{
		"content":        "non-creator tries to edit team draft",
		"base_result_id": resultID,
	}
	w := doEditRequest(r, taskID, "participant2", body)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-creator multi-person edit, got %d: %s", w.Code, w.Body.String())
	}
}

func TestEditSummary_MultiParticipantCreatorNoPersonal(t *testing.T) {
	// need4 / R4: a multi-person creator who has NO PersonalResult of their own must
	// still be able to edit the team draft (no 500). Team edit does not write back
	// to anyone's personal report.
	db := setupEditDB(t)
	now := time.Now().UTC()

	task := model.SummaryTask{
		TaskNo:      "TST-EDIT-MP-NOPR",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
	}
	db.Create(&task)

	// Two non-creator participants, each with a personal result. Creator has none.
	p1 := model.SummaryParticipant{TaskID: task.ID, UserID: "member_a", UserName: "A", Status: model.ParticipantCompleted}
	p2 := model.SummaryParticipant{TaskID: task.ID, UserID: "member_b", UserName: "B", Status: model.ParticipantCompleted}
	db.Create(&p1)
	db.Create(&p2)
	db.Create(&model.PersonalResult{TaskID: task.ID, ParticipantRefID: p1.ID, UserID: "member_a", Content: "a", WorkerStatus: model.PersonalStatusCompleted, GeneratedAt: &now})
	db.Create(&model.PersonalResult{TaskID: task.ID, ParticipantRefID: p2.ID, UserID: "member_b", Content: "b", WorkerStatus: model.PersonalStatusCompleted, GeneratedAt: &now})

	result := model.SummaryResult{TaskID: task.ID, Content: "team draft", Version: 1, GeneratedAt: now}
	db.Create(&result)

	h := NewEditHandler(db)
	r := setupEditRouter(h)

	body := map[string]interface{}{
		"content":        "creator-edited team draft, no personal of own",
		"base_result_id": result.ID,
	}
	w := doEditRequest(r, task.ID, "creator1", body)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (no 500) for multi-person creator without personal, got %d: %s", w.Code, w.Body.String())
	}

	// Members' personal results must be untouched (no write-back).
	var pra model.PersonalResult
	db.Where("task_id = ? AND user_id = ?", task.ID, "member_a").First(&pra)
	if pra.Content != "a" || pra.EditedAt != nil {
		t.Errorf("member_a personal result must be untouched, got content=%q edited_at=%v", pra.Content, pra.EditedAt)
	}
}

func TestEditSummary_MultiPersonCreatorPersonalNotClobbered(t *testing.T) {
	// F1: in a multi-person task the creator is also a participant WITH their own
	// PersonalResult. Editing the TEAM draft must NOT overwrite the creator's
	// personal summary. Fail-before: hasPersonal mirrored unconditionally and
	// clobbered it; pass-after: multi-person edits touch the team SummaryResult only.
	db := setupEditDB(t)
	now := time.Now().UTC()

	task := model.SummaryTask{
		TaskNo:      "TST-EDIT-F1",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
	}
	db.Create(&task)

	// creator IS a participant and HAS a personal result.
	cp := model.SummaryParticipant{TaskID: task.ID, UserID: "creator1", UserName: "Creator", Status: model.ParticipantCompleted}
	db.Create(&cp)
	creatorPR := model.PersonalResult{
		TaskID:           task.ID,
		ParticipantRefID: cp.ID,
		UserID:           "creator1",
		Content:          "creator's OWN personal summary -- must survive",
		WorkerStatus:     model.PersonalStatusCompleted,
		GeneratedAt:      &now,
	}
	db.Create(&creatorPR)

	// second participant -> genuinely multi-person.
	p2 := model.SummaryParticipant{TaskID: task.ID, UserID: "member_b", UserName: "B", Status: model.ParticipantCompleted}
	db.Create(&p2)
	db.Create(&model.PersonalResult{TaskID: task.ID, ParticipantRefID: p2.ID, UserID: "member_b", Content: "b", WorkerStatus: model.PersonalStatusCompleted, GeneratedAt: &now})

	result := model.SummaryResult{TaskID: task.ID, Content: "team draft v1", Version: 1, GeneratedAt: now}
	db.Create(&result)

	h := NewEditHandler(db)
	r := setupEditRouter(h)

	body := map[string]interface{}{
		"content":        "creator edits the TEAM draft",
		"base_result_id": result.ID,
	}
	w := doEditRequest(r, task.ID, "creator1", body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// team SummaryResult updated...
	var sr model.SummaryResult
	db.First(&sr, result.ID)
	if sr.Content != "creator edits the TEAM draft" {
		t.Errorf("team SummaryResult not updated: %q", sr.Content)
	}
	// ...but the creator's PersonalResult is UNTOUCHED.
	var gotPR model.PersonalResult
	db.First(&gotPR, creatorPR.ID)
	if gotPR.Content != "creator's OWN personal summary -- must survive" {
		t.Errorf("creator's PersonalResult was clobbered by team edit: %q", gotPR.Content)
	}
	if gotPR.EditedAt != nil {
		t.Errorf("creator's PersonalResult edited_at must stay nil, got %v", gotPR.EditedAt)
	}
}

func TestEditSummary_ContentTooLarge(t *testing.T) {
	db := setupEditDB(t)
	taskID, resultID, _ := seedEditableTask(t, db)

	h := NewEditHandler(db)
	r := setupEditRouter(h)

	largeContent := make([]byte, 500*1024+1)
	for i := range largeContent {
		largeContent[i] = 'a'
	}

	body := map[string]interface{}{
		"content":        string(largeContent),
		"base_result_id": resultID,
	}
	w := doEditRequest(r, taskID, "creator1", body)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for oversized content, got %d: %s", w.Code, w.Body.String())
	}
}

func newTestRefineLLM(t *testing.T, content string) (*service.LLMClient, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected LLM path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":%q},"finish_reason":"stop"}],"usage":{"total_tokens":7,"completion_tokens":3}}`, content)
	}))
	return service.NewLLMClient(srv.URL, "test-key", "test-model", 5, 256, false, 5), srv.Close
}

func seedVersionCitationPrivacyTask(t *testing.T, db *gorm.DB) (taskID int64, resultID int64) {
	t.Helper()
	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:      "TST-VERSION-PRIVACY",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("seed task: %v", err)
	}
	participant := model.SummaryParticipant{TaskID: task.ID, UserID: "member_a", UserName: "Member A", Status: model.ParticipantCompleted}
	if err := db.Create(&participant).Error; err != nil {
		t.Fatalf("seed participant: %v", err)
	}
	citations := []model.Citation{{
		Index:       1,
		Sender:      "Member A",
		Content:     "private raw message from member A",
		SentAt:      "2026-01-01T00:00:00Z",
		Source:      "grp",
		ChannelID:   "ch1",
		ChannelType: 2,
		MessageSeq:  100,
	}}
	pr := model.PersonalResult{
		TaskID:           task.ID,
		ParticipantRefID: participant.ID,
		UserID:           "member_a",
		WorkerStatus:     model.PersonalStatusCompleted,
		Content:          "member A personal content with [1]",
		GeneratedAt:      &now,
	}
	pr.SetCitations(citations)
	if err := db.Create(&pr).Error; err != nil {
		t.Fatalf("seed personal result: %v", err)
	}
	result := model.SummaryResult{
		TaskID:         task.ID,
		Content:        "team content copied from member A with [1]",
		TotalMsgCount:  1,
		TotalTokenUsed: 10,
		ModelVersion:   "test-v1",
		Version:        1,
		CreatedBy:      "member_a",
		GeneratedAt:    now,
	}
	result.SetCitations(citations)
	if err := db.Create(&result).Error; err != nil {
		t.Fatalf("seed summary result: %v", err)
	}
	return task.ID, result.ID
}

func doGetSummaryVersionRequest(r *gin.Engine, taskID, resultID int64, userID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/summaries/%d/versions/%d", taskID, resultID), nil)
	if userID != "" {
		req.Header.Set("Token", userID)
	}
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)
	return w
}

func doRefineSummaryRequest(r *gin.Engine, taskID int64, userID string, body interface{}) *httptest.ResponseRecorder {
	var bodyBytes []byte
	if body != nil {
		bodyBytes, _ = json.Marshal(body)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/summaries/%d/refine", taskID), bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	if userID != "" {
		req.Header.Set("Token", userID)
	}
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)
	return w
}

func responseCitationsLen(t *testing.T, w *httptest.ResponseRecorder) int {
	t.Helper()
	var resp struct {
		Data struct {
			Citations []json.RawMessage `json:"citations"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%s", err, w.Body.String())
	}
	return len(resp.Data.Citations)
}

func TestGetSummaryVersion_RedactsPlainCitationsForNonProducer(t *testing.T) {
	db := setupEditDB(t)
	taskID, resultID := seedVersionCitationPrivacyTask(t, db)

	h := NewEditHandler(db)
	r := setupEditRouter(h)

	w := doGetSummaryVersionRequest(r, taskID, resultID, "creator1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := responseCitationsLen(t, w); got != 0 {
		t.Fatalf("non-producer creator must not see member plain citations, got %d: %s", got, w.Body.String())
	}

	producer := doGetSummaryVersionRequest(r, taskID, resultID, "member_a")
	if producer.Code != http.StatusOK {
		t.Fatalf("expected producer 200, got %d: %s", producer.Code, producer.Body.String())
	}
	if got := responseCitationsLen(t, producer); got == 0 {
		t.Fatalf("producer should keep own plain citations, body=%s", producer.Body.String())
	}
}

func TestRefineSummary_DoesNotCopyInvisiblePlainCitations(t *testing.T) {
	db := setupEditDB(t)
	taskID, resultID := seedVersionCitationPrivacyTask(t, db)
	llm, closeLLM := newTestRefineLLM(t, "refined team content still referencing [1]")
	defer closeLLM()

	h := NewEditHandler(db, llm)
	r := setupEditRouter(h)
	w := doRefineSummaryRequest(r, taskID, "creator1", map[string]interface{}{
		"feedback":       "make it clearer",
		"base_result_id": resultID,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := responseCitationsLen(t, w); got != 0 {
		t.Fatalf("refine response must not return invisible member citations, got %d: %s", got, w.Body.String())
	}

	var refined model.SummaryResult
	if err := db.Where("task_id = ? AND operation_type = ?", taskID, "refine").First(&refined).Error; err != nil {
		t.Fatalf("load refined result: %v", err)
	}
	if got := len(refined.GetCitations()); got != 0 {
		t.Fatalf("refined result must not persist invisible member citations, got %d", got)
	}
}
