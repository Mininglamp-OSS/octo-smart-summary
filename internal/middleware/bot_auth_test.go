package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

type mockBotResolver struct {
	identity BotIdentity
	err      error
	token    string
}

func (r *mockBotResolver) ResolveBot(_ context.Context, token string) (BotIdentity, error) {
	r.token = token
	return r.identity, r.err
}

func TestStrictBotAuthUsesVerifiedOwnerAndSpace(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resolver := &mockBotResolver{identity: BotIdentity{BotUID: "bot-1", OwnerUID: "owner-1", SpaceID: "trusted-space"}}
	r := gin.New()
	r.Use(StrictBotAuthMiddleware(resolver))
	r.GET("/test", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"user_id": GetUserID(c), "space_id": GetSpaceID(c), "bot_id": c.GetString("bot_id")})
	})
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer bf_secret")
	req.Header.Set("X-Space-Id", "spoofed-space")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK || resolver.token != "bf_secret" {
		t.Fatalf("status=%d token=%q body=%s", w.Code, resolver.token, w.Body.String())
	}
	if got := w.Body.String(); got != `{"bot_id":"bot-1","space_id":"trusted-space","user_id":"owner-1"}` {
		t.Fatalf("unexpected identity: %s", got)
	}
}

func TestStrictBotAuthRejectsMissingOrIncompleteIdentity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		header   string
		resolver BotTokenResolver
		want     int
	}{
		{"missing bearer", "", &mockBotResolver{}, http.StatusUnauthorized},
		{"app bot is outside personal agent scope", "Bearer app_x", &mockBotResolver{identity: BotIdentity{BotUID: "bot", OwnerUID: "owner", SpaceID: "space"}}, http.StatusUnauthorized},
		{"missing owner", "Bearer bf_x", &mockBotResolver{identity: BotIdentity{BotUID: "bot", SpaceID: "space"}}, http.StatusUnauthorized},
		{"resolver failure", "Bearer bf_x", &mockBotResolver{err: errors.New("down")}, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := gin.New()
			r.Use(StrictBotAuthMiddleware(tc.resolver))
			r.GET("/test", func(c *gin.Context) { c.Status(http.StatusNoContent) })
			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			req.Header.Set("Authorization", tc.header)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("got %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}
