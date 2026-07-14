package handler

import (
	"strings"
	"testing"
)

func TestKeepLatestRunesKeepsRecentInstruction(t *testing.T) {
	old := strings.Repeat("旧", maxScheduleInstructionRunes)
	latest := "最新修改意见"
	got := keepLatestRunes(old+"\n"+latest, maxScheduleInstructionRunes)
	if len([]rune(got)) > maxScheduleInstructionRunes {
		t.Fatalf("got %d runes, want <= %d", len([]rune(got)), maxScheduleInstructionRunes)
	}
	if !strings.Contains(got, latest) {
		t.Fatalf("latest instruction should be retained, got suffix %q", got[len(got)-len(latest):])
	}
}
