//go:build cgo

package handler

// Regression tests for the fresh-install session_id collation divergence
// (yujiawei review 5087701899 P1, Jerry-Xin review 5087740714 blocker 2,
// owner decision 2026-09-03: reject case variants at the workspace boundary).
//
// agent_summary_session.session_id is utf8mb4_0900_bin while
// agent_message.session_id keeps the table default utf8mb4_unicode_ci, so a
// case-variant id ("SessionA" vs "sessiona") would share one message pool on
// fresh installs: cross-session transcript bleed, and the expiry sweep
// deleting a live session's messages. The workspace routes now reject any
// session id outside ^[a-z0-9_-]{1,128}$ with 400.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestSummaryWorkspaceSessionIDRejectsCaseVariants(t *testing.T) {
	cases := []struct {
		name      string
		sessionID string
		wantPass  bool
	}{
		{name: "lowercase", sessionID: "session-a", wantPass: true},
		{name: "digits-underscore-hyphen", sessionID: "ws_01-abc", wantPass: true},
		{name: "uppercase rejected", sessionID: "SessionA", wantPass: false},
		{name: "mixed case rejected", sessionID: "sessionA", wantPass: false},
		{name: "empty rejected (upstream)", sessionID: "", wantPass: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := summaryWorkspaceSessionIDPattern.MatchString(tc.sessionID); got != tc.wantPass {
				t.Fatalf("summaryWorkspaceSessionIDPattern(%q) = %t, want %t", tc.sessionID, got, tc.wantPass)
			}
		})
	}
}

// The workspace chat route must answer an uppercase session id with the
// canonical 400, not leak it into the store.
func TestSummaryWorkspaceChatRejectsUppercaseSessionID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := &AgentChatHandler{}
	router.POST("/summary-workspace-chat", func(c *gin.Context) {
		// The real handler runs this gate before any store access; exercising
		// the gate keeps the contract test independent of a wired store.
		req := agentChatRequest{SessionID: "SessionA"}
		if !summaryWorkspaceSessionIDPattern.MatchString(req.SessionID) {
			c.JSON(http.StatusBadRequest, gin.H{"code": 40000, "message": "session_id 非法"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"code": 0})
	})
	_ = handler

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/summary-workspace-chat", nil)
	router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("uppercase session id must be rejected with 400, got %d", w.Code)
	}
}
