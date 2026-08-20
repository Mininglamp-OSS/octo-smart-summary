package handler

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
)

// TestRefineFetchExpectedTracksRoute pins the round-4 P1-1 (yujiawei) / P2
// (mochashanyao): the finish gate's fetch_expected must reflect whether the turn
// will actually fetch (route.Fetch), not whether a confident rewrite hard-stripped
// the tools (!HardNoFetch). Keying on HardNoFetch persisted fetch_expected=1 for a
// SOFT rewrite while the guidance told the model not to fetch, so finalizeRun
// disclosed a false PARTIAL "coverage was not measured" on every soft rewrite.
func TestRefineFetchExpectedTracksRoute(t *testing.T) {
	// A soft rewrite: Fetch=false and HardNoFetch=false (tools stay available, but
	// the guidance says don't fetch). This is the exact shape that produced the
	// false PARTIAL. "把这份总结精简一下，只保留最近的结论" is one.
	softRewrite := agent.ClassifyRefine("把这份总结精简一下，只保留最近的结论")
	if softRewrite.Fetch || softRewrite.HardNoFetch {
		t.Fatalf("precondition: expected a soft rewrite (Fetch=false HardNoFetch=false), got %+v", softRewrite)
	}
	if refineFetchExpected(true, softRewrite) {
		t.Error("soft rewrite must set fetchExpected=false, else the finish gate reports a false PARTIAL")
	}

	// A confident rewrite also does not fetch.
	if refineFetchExpected(true, agent.ClassifyRefine("翻译成英文")) {
		t.Error("confident rewrite must set fetchExpected=false")
	}

	// A fetching route (augment/extend) expects coverage.
	if !refineFetchExpected(true, agent.ClassifyRefine("把客户反馈写进总结")) {
		t.Error("augment fetches, so fetchExpected must be true")
	}

	// A non-refine summary run always fetches, whatever the (zero-value) route is.
	if !refineFetchExpected(false, agent.RefineRoute{}) {
		t.Error("a non-refine run always expects to fetch")
	}
}
