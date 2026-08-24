package router

import (
	"github.com/gin-gonic/gin"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/llmfallback"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/llmobs"
)

// TestMetricsIsInternalOnly is the security-rule acceptance from the brief: the
// scrape endpoint reports model identifiers and failure volumes, which are
// operational details. The internal router is bound to its own listener
// (API_INTERNAL_PORT / WORKER_INTERNAL_PORT) that deployments do not publish,
// so the endpoint must exist there and must NOT exist on the public engine.
func TestMetricsIsInternalOnly(t *testing.T) {
	internal, _ := SetupInternal(nil)
	if !hasRoute(internal, http.MethodGet, "/internal/metrics") {
		t.Error("/internal/metrics is not mounted on the internal router")
	}

	// Install a metric set with a recognisable series, then check no public GET
	// route can return it.
	//
	// This deliberately does NOT filter routes by name. Matching on
	// strings.Contains(path, "metrics") tests what a route is CALLED, not what it
	// serves: a handler rendering the same exposition at, say,
	// /api/v1/debug/telemetry would pass a name check untouched. Drive the routes
	// and inspect the bodies instead.
	llmobs.Install(nil)
	t.Cleanup(func() { llmfallback.SetDefaultObserver(nil) })
	llmobs.Default().ObserveResult(llmfallback.ResultEvent{
		Path: llmfallback.PathAgentChat, Model: "canary-model",
		Position: llmfallback.PositionPrimary, OK: true,
	})

	public := SetupPublic(nil, nil, nil, nil, nil, "", 0, false, 0, nil, "", "", "", 0, 0, nil)
	for _, r := range public.Routes() {
		if r.Method != http.MethodGet {
			continue
		}
		// Route patterns carry :params / *wildcards; give them a value so the
		// request actually dispatches instead of 404-ing on the pattern itself.
		path := concretePath(r.Path)
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		public.ServeHTTP(w, req)

		body := w.Body.String()
		for _, marker := range []string{"llm_attempts_total", "llm_calls_total", "canary-model"} {
			if strings.Contains(body, marker) {
				t.Errorf("public route %s %s leaked metric content (%q); the scrape must stay on the internal listener",
					r.Method, r.Path, marker)
			}
		}
	}
}

// concretePath substitutes a placeholder for gin's :param and *wildcard
// segments so a route pattern can be requested.
func concretePath(pattern string) string {
	segs := strings.Split(pattern, "/")
	for i, s := range segs {
		if strings.HasPrefix(s, ":") || strings.HasPrefix(s, "*") {
			segs[i] = "probe"
		}
	}
	return strings.Join(segs, "/")
}

// TestMetricsRendersExposition covers the wiring end to end: the handler must
// serve the installed metric set with the content type a Prometheus scraper
// expects.
func TestMetricsRendersExposition(t *testing.T) {
	// Install writes process-wide state. Without this cleanup it leaked into
	// every later test in the package — the same hazard ResetDefaultForTest was
	// added to close, and the reason TestMetricsBeforeInstallDoesNotPanic has to
	// defend itself explicitly.
	llmobs.Install(nil)
	t.Cleanup(func() {
		llmfallback.SetDefaultObserver(nil)
		llmobs.ResetDefaultForTest()
	})

	internal, _ := SetupInternal(nil)
	w := httptest.NewRecorder()
	internal.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/internal/metrics", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain exposition format", ct)
	}
	if !strings.Contains(w.Body.String(), "# TYPE llm_attempts_total counter") {
		t.Errorf("body does not carry the metric families:\n%s", w.Body.String())
	}
}

// TestMetricsBeforeInstallDoesNotPanic covers the ordering hazard: a scrape can
// arrive before (or without) llmobs.Install having run, and a 500 on the
// monitoring endpoint would itself look like an outage.
func TestMetricsBeforeInstallDoesNotPanic(t *testing.T) {
	// This used to be wrapped in `if llmobs.Default() == nil`, which made the
	// whole body dead in a full-package run: an earlier test installs a
	// process-wide default that cannot be un-installed, so under -shuffle=on the
	// assertion ran or not by coin flip. Reset the default for the duration of
	// this test instead, so the nil guard is exercised every time.
	llmfallback.SetDefaultObserver(nil)
	llmobs.ResetDefaultForTest()
	t.Cleanup(func() { llmfallback.SetDefaultObserver(nil) })

	if llmobs.Default() != nil {
		t.Fatal("precondition: the default metric set should be cleared for this test")
	}

	internal, _ := SetupInternal(nil)
	w := httptest.NewRecorder()
	internal.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/internal/metrics", nil))

	if w.Code != http.StatusOK {
		t.Errorf("status = %d before Install; want a benign empty 200 — a 500 on the monitoring endpoint reads as an outage", w.Code)
	}
	if body := w.Body.String(); body != "" {
		t.Errorf("body = %q before Install; want empty", body)
	}
}

func hasRoute(e *gin.Engine, method, path string) bool {
	for _, r := range e.Routes() {
		if r.Method == method && r.Path == path {
			return true
		}
	}
	return false
}
