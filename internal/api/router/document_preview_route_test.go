package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDocumentPreviewRouteIsMountedBehindAuth pins the route's auth wiring (P2-12).
// Every handler test calls StreamDocumentPreview with user_id pre-seeded into the
// gin context, so an edit that moved
//
//	v1.POST("/summaries/document/preview", agentSummaryH.StreamDocumentPreview)
//
// out of the authenticated v1 group would leave the whole handler suite green. This
// asserts the MOUNTED route rejects an unauthenticated request with 401, the same
// seam bot_routes_test uses for the bot group.
func TestDocumentPreviewRouteIsMountedBehindAuth(t *testing.T) {
	r := SetupPublic(nil, nil, nil, nil, fixedBotResolver{}, "", 0, false, false, 0, nil, "", "", "", 0, 0, nil)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/summaries/document/preview", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("POST /api/v1/summaries/document/preview: got %d, want 401 (route must sit behind auth)", w.Code)
	}
}

// TestRetryAfterIsExposedToCrossOriginClients pins the CORS expose-header (P2-3).
//
// Deleting the Access-Control-Expose-Headers line in router.go left the whole suite
// green. That line is what lets a cross-origin browser client READ the Retry-After
// header this endpoint returns alongside 429 — without it the front-end gets the
// status but not the advice, which is the difference between a contract and a
// suggestion. The PR description names Retry-After as a new client-visible surface
// for a module that lives in another repository, so a silent regression here breaks a
// cross-repo contract with nothing to catch it.
func TestRetryAfterIsExposedToCrossOriginClients(t *testing.T) {
	r := SetupPublic(nil, nil, nil, nil, fixedBotResolver{}, "", 0, false, false, 0, nil, "", "", "", 0, 0, nil)

	// The header must be present on the ACTUAL response, not only on the preflight —
	// Expose-Headers governs what the browser lets script read from the real response,
	// so asserting it only on OPTIONS would pin the wrong half.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/summaries/document/preview", nil)
	req.Header.Set("Origin", "https://example.test")
	r.ServeHTTP(w, req)
	if got := w.Header().Get("Access-Control-Expose-Headers"); !strings.Contains(got, "Retry-After") {
		t.Errorf("Access-Control-Expose-Headers = %q, want it to contain Retry-After; "+
			"a cross-origin client cannot read the 429 back-off advice without it", got)
	}

	// And on the preflight, so the browser does not reject the response before script
	// ever sees it.
	wp := httptest.NewRecorder()
	preflight := httptest.NewRequest(http.MethodOptions, "/api/v1/summaries/document/preview", nil)
	preflight.Header.Set("Origin", "https://example.test")
	preflight.Header.Set("Access-Control-Request-Method", "POST")
	r.ServeHTTP(wp, preflight)
	if got := wp.Header().Get("Access-Control-Expose-Headers"); !strings.Contains(got, "Retry-After") {
		t.Errorf("preflight Access-Control-Expose-Headers = %q, want it to contain Retry-After", got)
	}
}
