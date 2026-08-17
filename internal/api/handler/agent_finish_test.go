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
	if citationsValid("要点 [1] 与幽灵引用 [9]", cits) {
		t.Error("marker [9] has no citation → should be invalid")
	}
	if !citationsValid("没有任何引用标记的正文", cits) {
		t.Error("no markers → vacuously valid")
	}
	if citationsValid("越界 [0]", cits) {
		t.Error("marker [0] has no citation → invalid")
	}
}
