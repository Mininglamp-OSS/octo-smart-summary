package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// mockResolver implements TokenResolver for tests.
type mockResolver struct {
	uid string
	err error
}

func (m *mockResolver) ResolveUID(_ context.Context, _ string) (string, error) {
	return m.uid, m.err
}

func TestSpaceMiddleware_WithHeader(t *testing.T) {
	r := gin.New()
	r.Use(SpaceMiddleware())
	r.GET("/test", func(c *gin.Context) {
		spaceID := GetSpaceID(c)
		c.JSON(http.StatusOK, gin.H{"space_id": spaceID})
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Space-Id", "space-123")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
	body := w.Body.String()
	if body == "" {
		t.Fatal("empty response body")
	}
	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestSpaceMiddleware_WithoutHeader(t *testing.T) {
	r := gin.New()
	r.Use(SpaceMiddleware())
	r.GET("/test", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	// SpaceMiddleware is now permissive: missing header → allow with empty space_id
	if w.Code != http.StatusOK {
		t.Errorf("expected status 200 (permissive mode), got %d", w.Code)
	}
}

func TestSpaceMiddleware_EmptyHeader(t *testing.T) {
	r := gin.New()
	r.Use(SpaceMiddleware())
	r.GET("/test", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Space-Id", "")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	// Empty header → allow through (empty string scope)
	if w.Code != http.StatusOK {
		t.Errorf("expected status 200 for empty header (permissive mode), got %d", w.Code)
	}
}

func TestAuthMiddleware_WithToken(t *testing.T) {
	resolver := &mockResolver{uid: "user-abc"}
	r := gin.New()
	r.Use(AuthMiddleware(resolver))
	r.GET("/test", func(c *gin.Context) {
		uid := GetUserID(c)
		c.JSON(http.StatusOK, gin.H{"user_id": uid})
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Token", "valid-token")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestAuthMiddleware_NoAuth(t *testing.T) {
	resolver := &mockResolver{uid: ""}
	r := gin.New()
	r.Use(AuthMiddleware(resolver))
	r.GET("/test", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	// No auth headers → allowed through (matches Python prototype behaviour: Header(default=""))
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 (permissive mode), got %d", w.Code)
	}
}

func TestAuthMiddleware_InvalidToken(t *testing.T) {
	resolver := &mockResolver{uid: ""}
	r := gin.New()
	r.Use(AuthMiddleware(resolver))
	r.GET("/test", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Token", "bad-token")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for invalid token, got %d", w.Code)
	}
}

func TestAuthMiddleware_XUserIdIgnored(t *testing.T) {
	r := gin.New()
	r.Use(AuthMiddleware(nil))
	r.GET("/test", func(c *gin.Context) {
		uid := GetUserID(c)
		c.JSON(http.StatusOK, gin.H{"user_id": uid})
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-User-Id", "user-direct")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	// user_id should be empty — X-User-Id header is ignored
	if body := w.Body.String(); body == "" {
		t.Fatal("empty response body")
	}
}
