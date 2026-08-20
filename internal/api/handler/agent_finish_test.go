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
// path: a rewrite turn (fetch_expected=false, no build) produces no tool traces,
// so savedCitations is nil while the rewritten content still carries the source
// summary's markers. Nothing of its own was claimed, so nothing can dangle — this
// used to be an automatic FAILED.
func TestCitationsValidWithNoCitationsIsVacuouslyTrue(t *testing.T) {
	if !citationsValid("重写后的总结 [1][2]", nil, false /* no build expected: refine-borrowed */) {
		t.Error("a rewrite turn with no built citations must not be FAILED")
	}
}

// TestCitationsValidEmptySliceScopedToRefine pins the round-4 P1-4 (yujiawei):
// the empty-slice exemption is vacuously-valid ONLY on the refine-borrowed path.
// When a build WAS expected (a fetch turn) and produced zero citations, content
// still carrying a [1]-anchored citation sequence is a failed/expired citation
// build — it must not pass as valid and report COMPLETE.
func TestCitationsValidEmptySliceScopedToRefine(t *testing.T) {
	// Fetch turn, build expected, zero citations, but [1][2][3] left in the text.
	if citationsValid("结论一 [1]，结论二 [2][3]", nil, true) {
		t.Error("a failed citation build (build expected, zero cits) with a [1] sequence must be invalid")
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
