//go:build cgo

package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

func postCreateSummaryWithKey(t *testing.T, router http.Handler, body map[string]interface{}, key string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/summaries", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Token", "creator1")
	req.Header.Set("X-Space-Id", "space1")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func createSummaryTaskID(t *testing.T, w *httptest.ResponseRecorder) int64 {
	t.Helper()
	var response struct {
		Data struct {
			TaskID int64 `json:"task_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, w.Body.String())
	}
	return response.Data.TaskID
}

func existingSummaryTaskID(t *testing.T, w *httptest.ResponseRecorder) int64 {
	t.Helper()
	var response struct {
		Data struct {
			TaskID int64 `json:"existing_task_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, w.Body.String())
	}
	return response.Data.TaskID
}

func TestCreateSummaryWorkflowIdempotencyHTTPContract(t *testing.T) {
	db, imDB := setupTestDBs(t)
	if err := db.AutoMigrate(&model.SummaryWorkflowIdempotency{}); err != nil {
		t.Fatalf("migrate workflow idempotency: %v", err)
	}
	router := setupCreateRouter(NewTaskHandler(db, imDB, ""))
	body := map[string]interface{}{
		"title": "weekly",
		"sources": []map[string]interface{}{
			{"source_type": model.SourceGroup, "source_id": "group-1"},
		},
	}

	first := postCreateSummaryWithKey(t, router, body, "workflow-http-001")
	second := postCreateSummaryWithKey(t, router, body, "workflow-http-001")
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("first/second status = %d/%d; bodies=%s / %s", first.Code, second.Code, first.Body.String(), second.Body.String())
	}
	firstID := createSummaryTaskID(t, first)
	secondID := createSummaryTaskID(t, second)
	if firstID == 0 || secondID != firstID {
		t.Fatalf("task ids = %d/%d, want same non-zero id", firstID, secondID)
	}
	var count int64
	db.Model(&model.SummaryTask{}).Count(&count)
	if count != 1 {
		t.Fatalf("task count = %d, want 1", count)
	}

	changed := map[string]interface{}{
		"title": "different",
		"sources": []map[string]interface{}{
			{"source_type": model.SourceGroup, "source_id": "group-1"},
		},
	}
	mismatch := postCreateSummaryWithKey(t, router, changed, "workflow-http-001")
	if mismatch.Code != http.StatusConflict || respCode(t, mismatch) != 40009 {
		t.Fatalf("mismatch status/code = %d/%d, want 409/40009; body=%s", mismatch.Code, respCode(t, mismatch), mismatch.Body.String())
	}
	if existingID := existingSummaryTaskID(t, mismatch); existingID != firstID {
		t.Fatalf("existing_task_id = %d, want %d", existingID, firstID)
	}
}

func TestCreateSummaryWorkflowRejectsMalformedIdempotencyHeader(t *testing.T) {
	db, imDB := setupTestDBs(t)
	if err := db.AutoMigrate(&model.SummaryWorkflowIdempotency{}); err != nil {
		t.Fatalf("migrate workflow idempotency: %v", err)
	}
	router := setupCreateRouter(NewTaskHandler(db, imDB, ""))
	body := map[string]interface{}{
		"title": "weekly",
		"sources": []map[string]interface{}{
			{"source_type": model.SourceGroup, "source_id": "group-1"},
		},
	}

	blank := postCreateSummaryWithKey(t, router, body, "   ")
	if blank.Code != http.StatusBadRequest || respCode(t, blank) != 40005 {
		t.Fatalf("blank key status/code = %d/%d, want 400/40005", blank.Code, respCode(t, blank))
	}

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/summaries", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Token", "creator1")
	req.Header.Set("X-Space-Id", "space1")
	req.Header[http.CanonicalHeaderKey("Idempotency-Key")] = []string{"key-one", "key-two"}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || respCode(t, w) != 40005 {
		t.Fatalf("multi-value key status/code = %d/%d, want 400/40005", w.Code, respCode(t, w))
	}
}
