package handler

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

func TestCitationsValid(t *testing.T) {
	cits := []model.Citation{{Index: 1}, {Index: 2}, {Index: 3}}

	if !citationsValid("要点一 [1]，要点二 [2][3]", cits, true) {
		t.Error("all markers resolve → should be valid")
	}
	if !citationsValid("没有任何引用标记的正文", cits, true) {
		t.Error("no markers → vacuously valid")
	}

	// In-range miss: with citations 1-4 built, a [3] that resolves to nothing is
	// genuine citation corruption and must still fail.
	sparse := []model.Citation{{Index: 1}, {Index: 2}, {Index: 4}}
	if citationsValid("结论 [1]，幽灵引用 [3]", sparse, true) {
		t.Error("an in-range marker with no citation is real corruption → invalid")
	}
}

// TestCitationsValidDoesNotFailOnOrdinaryBracketedIntegers pins the round-2
// finding. The penalty for failing this check is a hard FAILED verdict, which is
// far too heavy to hang on any bracketed integer in prose — and this repo
// already concedes such integers exist: stripUnresolvedCitationMarkers strips
// only the markers belonging to a referenced artifact precisely because
// "unrelated bracketed integers remain user content".
func TestCitationsValidDoesNotFailOnOrdinaryBracketedIntegers(t *testing.T) {
	cits := []model.Citation{{Index: 1}, {Index: 2}}
	for _, content := range []string{
		"根据 [2024] 年规划，本周完成 [1]。",
		"待办共 [3] 项，详见 [1]。",
		"参考 GB/T [50011] 标准 [2]。",
		"越界 [0]，但引用正常 [1]。",
	} {
		if !citationsValid(content, cits, true) {
			t.Errorf("ordinary bracketed integer must not fail the run: %q", content)
		}
	}
}

// TestCitationsValidWithNoCitationsIsVacuouslyTrue pins the reachable refine
// path: a rewrite turn that fetched nothing (coverage_measured=false) produces no
// tool traces, so savedCitations is nil while the rewritten content still carries
// the source summary's markers. Nothing of its own was claimed, so nothing can
// dangle — this used to be an automatic FAILED.
func TestCitationsValidWithNoCitationsIsVacuouslyTrue(t *testing.T) {
	if !citationsValid("重写后的总结 [1][2]", nil, false /* run fetched nothing: refine-borrowed */) {
		t.Error("a rewrite turn with no built citations must not be FAILED")
	}
}

// TestCitationsValidEmptySliceScopedToRefine pins the round-4 P1-4 (yujiawei):
// the empty-slice exemption is vacuously-valid ONLY on the refine-borrowed path.
// When the run DID fetch and the build produced zero citations, content still
// carrying a [1]-anchored citation sequence is a failed/expired citation build —
// it must not pass as valid and report COMPLETE.
func TestCitationsValidEmptySliceScopedToRefine(t *testing.T) {
	// Fetch turn, zero citations, but [1][2][3] left in the text.
	if citationsValid("结论一 [1]，结论二 [2][3]", nil, true) {
		t.Error("a failed citation build (run fetched, zero cits) with a [1] sequence must be invalid")
	}

	// The prose-integer concession must survive on a zero-citation build turn: a
	// stray year / count that never starts at [1] is not a broken citation.
	for _, content := range []string{
		"根据 [2024] 年规划完成。",
		"共 [3] 项待办。",
		"参考 GB/T [50011] 标准。",
	} {
		if !citationsValid(content, nil, true) {
			t.Errorf("a bracketed integer with no [1] sequence must not fail a zero-citation build: %q", content)
		}
	}
}

// TestCitationsValidKeysOnTheFactNotTheExpectation pins round-7 P1-3.
//
// The exemption used to be keyed on run.FetchExpected — a persisted EXPECTATION —
// which is the exact conflation finishgate.Evaluate reordered itself to eliminate,
// still live one file away. All four steps are reachable inside this PR:
//
//  1. a soft rewrite routes Fetch=false, HardNoFetch=false, so fetch_expected=0 is
//     persisted WHILE the fetch tools are kept;
//  2. the model uses them, then buildCitationsForSession (which runs on EVERY save)
//     fails and yields nil;
//  3. with no ReferencedTaskIDs the stripUnresolvedCitationMarkers fallback never
//     runs, so [1][2][3] stay in the saved content;
//  4. the exemption fires on the expectation and the run is labelled COMPLETE.
//
// A summary with dangling citation markers is exactly the confidently-wrong
// deliverable SS-07 exists to catch. Keyed on the fact (coverage_measured), the
// same state is correctly invalid.
func TestCitationsValidKeysOnTheFactNotTheExpectation(t *testing.T) {
	const dangling = "结论一 [1]，结论二 [2][3]"

	// The soft-rewrite state: the run fetched (the fact), whatever it expected.
	if citationsValid(dangling, nil, true /* coverage measured: the run DID fetch */) {
		t.Error("a run that fetched, whose citation build produced nothing, must not pass with dangling markers")
	}

	// And the genuine refine-borrowed state is still exempt.
	if !citationsValid(dangling, nil, false /* the run fetched nothing at all */) {
		t.Error("a turn that never fetched borrows its markers and must stay valid")
	}
}
