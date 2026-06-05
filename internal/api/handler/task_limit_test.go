package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/gin-gonic/gin"
)

// setupCreateRouter wires only the CreateSummary route with the same auth/space
// middleware stack used by the other handler tests.
func setupCreateRouter(h *TaskHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.POST("/api/v1/summaries", h.CreateSummary)
	return r
}

// doCreateSummary issues a POST /api/v1/summaries with the given JSON body,
// authenticated as userID.
func doCreateSummary(r *gin.Engine, body map[string]interface{}, userID string) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/summaries", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if userID != "" {
		req.Header.Set("Token", userID)
	}
	req.Header.Set("X-Space-Id", "space1")
	r.ServeHTTP(w, req)
	return w
}

// makeSources builds n source entries matching the sourceReq shape
// (source_type + source_id) the handler expects.
func makeSources(n int) []map[string]interface{} {
	sources := make([]map[string]interface{}, 0, n)
	for i := 0; i < n; i++ {
		sources = append(sources, map[string]interface{}{
			"source_type": 1,
			"source_id":   fmt.Sprintf("grp_%d", i),
		})
	}
	return sources
}

// respCode extracts the business code from a JSON response body.
func respCode(t *testing.T, w *httptest.ResponseRecorder) int {
	t.Helper()
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v (body=%s)", err, w.Body.String())
	}
	c, ok := resp["code"].(float64)
	if !ok {
		t.Fatalf("response has no numeric code: %s", w.Body.String())
	}
	return int(c)
}

// TestCreateSummary_SourceCountAtLimit verifies that exactly maxSourceCount
// sources are NOT rejected by the count-limit check (code 40003).
func TestCreateSummary_SourceCountAtLimit(t *testing.T) {
	db, imDB := setupTestDBs(t)
	h := NewTaskHandler(db, imDB, "")
	r := setupCreateRouter(h)

	body := map[string]interface{}{
		"title":   "limit-test",
		"sources": makeSources(maxSourceCount),
	}
	w := doCreateSummary(r, body, "creator1")

	if code := respCode(t, w); code == 40003 {
		t.Errorf("expected %d sources to pass the count limit, but got count-limit rejection 40003 (http %d): %s",
			maxSourceCount, w.Code, w.Body.String())
	}
}

// TestCreateSummary_SourceCountExceedsLimit verifies that maxSourceCount+1
// sources are rejected with HTTP 400 and code 40003.
func TestCreateSummary_SourceCountExceedsLimit(t *testing.T) {
	db, imDB := setupTestDBs(t)
	h := NewTaskHandler(db, imDB, "")
	r := setupCreateRouter(h)

	body := map[string]interface{}{
		"title":   "limit-test",
		"sources": makeSources(maxSourceCount + 1),
	}
	w := doCreateSummary(r, body, "creator1")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 for %d sources, got %d: %s", maxSourceCount+1, w.Code, w.Body.String())
	}
	if code := respCode(t, w); code != 40003 {
		t.Errorf("expected code 40003 for exceeding source limit, got %d: %s", code, w.Body.String())
	}
}
