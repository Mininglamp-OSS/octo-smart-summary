package handler

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

func TestCitationsValid(t *testing.T) {
	cits := []model.Citation{{Index: 1}, {Index: 2}, {Index: 3}}

	if !citationsValid("要点一 [1]，要点二 [2][3]", cits) {
		t.Error("all markers resolve → should be valid")
	}
	if !citationsValid("没有任何引用标记的正文", cits) {
		t.Error("no markers → vacuously valid")
	}

	// In-range miss: with citations 1-4 built, a [3] that resolves to nothing is
	// genuine citation corruption and must still fail.
	sparse := []model.Citation{{Index: 1}, {Index: 2}, {Index: 4}}
	if citationsValid("结论 [1]，幽灵引用 [3]", sparse) {
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
		if !citationsValid(content, cits) {
			t.Errorf("ordinary bracketed integer must not fail the run: %q", content)
		}
	}
}

// TestCitationsValidWithNoCitationsIsVacuouslyTrue pins the reachable refine
// path: a rewrite turn produces no tool traces, so savedCitations is nil while
// the rewritten content still carries the source summary's markers. Nothing was
// claimed, so nothing can dangle — this used to be an automatic FAILED.
func TestCitationsValidWithNoCitationsIsVacuouslyTrue(t *testing.T) {
	if !citationsValid("重写后的总结 [1][2]", nil) {
		t.Error("a rewrite turn with no built citations must not be FAILED")
	}
}
