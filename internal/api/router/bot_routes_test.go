package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
)

type fixedBotResolver struct {
	identity middleware.BotIdentity
}

func (r fixedBotResolver) ResolveBot(context.Context, string) (middleware.BotIdentity, error) {
	return r.identity, nil
}

func TestBotSummaryRoutesAreMountedBehindBotAuth(t *testing.T) {
	r := SetupPublic(nil, nil, nil, nil, fixedBotResolver{}, "", 0, false, false, 0, nil, "", "", "", 0, 0, nil)
	for _, path := range []string{
		"/api/v1/bot/summaries",
		"/api/v1/bot/summaries/42",
		"/api/v1/bot/summaries/42/result",
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s: got %d, want 401 (route mounted behind bot auth)", path, w.Code)
		}
	}
}

func TestBotSummaryRoutesRejectHumanTokenAndClientSpace(t *testing.T) {
	r := SetupPublic(nil, nil, nil, nil, fixedBotResolver{}, "", 0, false, false, 0, nil, "", "", "", 0, 0, nil)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/bot/summaries", nil)
	req.Header.Set("Token", "human-session-token")
	req.Header.Set("X-Space-Id", "spoofed-space")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401: %s", w.Code, w.Body.String())
	}
}
