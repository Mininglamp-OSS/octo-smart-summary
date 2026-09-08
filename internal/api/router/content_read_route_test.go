package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestContentReadRoutesAreAdditiveAndHumanOnly(t *testing.T) {
	r := SetupPublic(nil, nil, nil, nil, fixedBotResolver{}, "", 0, false, false, 0, nil, "", "", "", 0, 0, nil)
	expected := map[string]bool{
		"/api/v1/summaries/:id/contents":                                  false,
		"/api/v1/summaries/:id/contents/:content_id/versions":             false,
		"/api/v1/summaries/:id/contents/:content_id/versions/:version_id": false,
	}
	for _, route := range r.Routes() {
		if strings.Contains(route.Handler, "ContentReadHandler") {
			if route.Method != http.MethodGet || strings.Contains(route.Path, "/bot/") {
				t.Fatalf("unexpected content route: %+v", route)
			}
			if _, ok := expected[route.Path]; !ok {
				t.Fatalf("unexpected content route path: %s", route.Path)
			}
			expected[route.Path] = true
		}
	}
	for path, present := range expected {
		if !present {
			t.Fatalf("missing route: %s", path)
		}
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/summaries/1/contents", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous content read=%d", rec.Code)
	}
}
