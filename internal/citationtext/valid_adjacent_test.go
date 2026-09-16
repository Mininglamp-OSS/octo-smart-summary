package citationtext

import "testing"

// The reviewer's empirical probes (PR#251 review P1-1), pinned as tests.
func TestValidAdjacentProbes(t *testing.T) {
	valid := func(n int) bool { return n >= 1 && n <= 5 }
	// Real citation plus ordinary bracketed-number prose must pass.
	for _, content := range []string{
		"依据 GB/T [50011-2010] 抗震规范。参考 [2]。",
		"预算区间 [3-5] 万元。参考 [2]。",
		"排期 [2026-09] 确认。结论见 [2] [3]。",
		"编号 [7;9] 待确认。依据 [2]。",
	} {
		if !ValidAdjacent(content, valid, true) {
			t.Fatalf("legitimate draft rejected: %q", content)
		}
	}
	// Isolated prose alone is still not citation evidence (requireMarker).
	for _, prose := range []string{
		"预算区间 [3-5] 万元。",
		"依据 GB/T [50011-2010] 抗震规范。",
		"排期 [2026-09] 确认。",
	} {
		if ValidAdjacent(prose, valid, true) {
			t.Fatalf("prose counted as citation evidence: %q", prose)
		}
	}
	// Inside a real cluster every member must resolve; holes fail closed.
	for _, corrupt := range []string{
		"见 [1][3-9] 的讨论。",
		"结论 [1][2,6]。",
		"见 [1][9,] 的讨论。",
	} {
		if ValidAdjacent(corrupt, valid, true) {
			t.Fatalf("corrupt cluster accepted: %q", corrupt)
		}
	}
	// Single markers remain strictly validated even when isolated.
	if ValidAdjacent("见 [9] 的讨论。", valid, true) {
		t.Fatal("out-of-window single marker accepted")
	}
	if !ValidAdjacent("见 [2] 的讨论。", valid, true) {
		t.Fatal("in-window single marker rejected")
	}
}
