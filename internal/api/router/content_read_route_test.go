package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestContentReadRoutesAreAdditiveAndHumanOnly(t *testing.T) {
	t.Setenv("SUMMARY_CONTENT_EXECUTION_SPACES", "")
	r := SetupPublic(nil, nil, nil, nil, fixedBotResolver{}, "", 0, false, false, 0, nil, "", "", "", 0, 0, nil, nil)
	expected := map[string]bool{
		"/api/v1/summaries/:id/contents":                                        false,
		"/api/v1/summaries/:id/contents/:content_id/versions":                   false,
		"/api/v1/summaries/:id/contents/:content_id/versions/:version_id":       false,
		"/api/v1/summaries/:id/contents/:content_id/generations/:generation_id": false,
	}
	for _, route := range r.Routes() {
		if strings.Contains(route.Handler, "ContentCommandHandler") {
			t.Fatalf("commands activated before legacy writer integration: %+v", route)
		}
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

func TestContentExecutionRoutesRequireExactOptIn(t *testing.T) {
	for _, spaces := range []string{"", "*", "fixture-space"} {
		t.Run(spaces, func(t *testing.T) {
			t.Setenv("SUMMARY_CONTENT_READ_SPACES", "fixture-space")
			t.Setenv("SUMMARY_CONTENT_EXECUTION_SPACES", spaces)
			r := SetupPublic(nil, nil, nil, nil, fixedBotResolver{}, "", 0, false, false, 0, nil, "", "", "", 0, 0, nil, nil)
			count := 0
			for _, route := range r.Routes() {
				if !strings.Contains(route.Handler, "ContentCommandHandler") {
					continue
				}
				count++
				if strings.Contains(route.Path, "/bot/") || spaces != "fixture-space" {
					t.Fatalf("unexpected execution route: %+v", route)
				}
				path := strings.ReplaceAll(strings.ReplaceAll(route.Path, ":id", "1"), ":content_id", "sc1_fixture")
				path = strings.ReplaceAll(path, ":generation_id", "11111111-1111-4111-8111-111111111111")
				rec := httptest.NewRecorder()
				r.ServeHTTP(rec, httptest.NewRequest(route.Method, path, nil))
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("anonymous command %s = %d", route.Path, rec.Code)
				}
			}
			expected := 0
			if spaces == "fixture-space" {
				expected = 8
			}
			if count != expected {
				t.Fatalf("commands = %d, want %d", count, expected)
			}
		})
	}
}
