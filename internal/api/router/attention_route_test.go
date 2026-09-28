package router

import (
	"net/http"
	"strings"
	"testing"
)

// GET /api/v1/summaries/attention is a static sibling of the /summaries/:id
// wildcard. Assert against the route table rather than response codes: the auth
// middleware returns 401 before routing matters, so a response-code test cannot
// distinguish "route exists" from "route is missing and falls through to :id".
// A route-table assertion proves the handler binding and that the endpoint is
// not mounted on the bot group.
func TestAttentionRouteCoexistsWithSummaryIDRoute(t *testing.T) {
	r := SetupPublic(nil, nil, nil, nil, fixedBotResolver{}, "", 0, false, false, 0, nil, "", "", "", 0, 0, nil)

	routes := r.Routes()

	// The attention endpoint must be bound to GetAttention on the human-token
	// group, not swallowed by the /summaries/:id wildcard.
	found := false
	for _, rt := range routes {
		if rt.Method == http.MethodGet && rt.Path == "/api/v1/summaries/attention" {
			if !strings.Contains(rt.Handler, "GetAttention") {
				t.Fatalf("GET /api/v1/summaries/attention is bound to %s, not GetAttention", rt.Handler)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("GET /api/v1/summaries/attention is not registered")
	}

	// /summaries/:id must still resolve to GetSummary, not GetAttention.
	for _, rt := range routes {
		if rt.Method == http.MethodGet && rt.Path == "/api/v1/summaries/:id" {
			if !strings.Contains(rt.Handler, "GetSummary") {
				t.Fatalf("GET /api/v1/summaries/:id is bound to %s, not GetSummary", rt.Handler)
			}
		}
	}

	// The bot surface must NOT gain the attention endpoint.
	for _, rt := range routes {
		if strings.HasPrefix(rt.Path, "/api/v1/bot/") && strings.Contains(rt.Handler, "GetAttention") {
			t.Fatalf("attention endpoint must not be mounted on the bot surface: %s", rt.Path)
		}
	}
}
