package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestStrictAuthMiddleware_NoToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(StrictAuthMiddleware(nil))
	r.GET("/test", func(c *gin.Context) {
		c.JSON(200, gin.H{"user_id": GetUserID(c)})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestStrictAuthMiddleware_WithUserIDHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(StrictAuthMiddleware(nil))
	r.GET("/test", func(c *gin.Context) {
		c.JSON(200, gin.H{"user_id": GetUserID(c)})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-User-Id", "user123")
	r.ServeHTTP(w, req)

	// X-User-Id header is ignored — should return 401
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}

func TestStrictAuthMiddleware_WithValidToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resolver := &mockResolver{uid: "resolved_user"}
	r := gin.New()
	r.Use(StrictAuthMiddleware(resolver))
	r.GET("/test", func(c *gin.Context) {
		c.JSON(200, gin.H{"user_id": GetUserID(c)})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Token", "valid_token")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestStrictAuthMiddleware_WithInvalidToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resolver := &mockResolver{uid: ""}
	r := gin.New()
	r.Use(StrictAuthMiddleware(resolver))
	r.GET("/test", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Token", "bad_token")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
}
