package agent

import (
	"strings"
	"testing"
)

// TestClassifyRefine covers SS-08's 3-way refine routing (缺点十二).
func TestClassifyRefine(t *testing.T) {
	cases := []struct {
		name           string
		instruction    string
		wantIntent     RefineIntent
		wantFetch      bool
		wantReuseRange bool
		wantReuseCite  bool
	}{
		// rewrite: pure text work / Q&A → no fetch, reuse citations
		{"translate", "帮我把这份总结翻译成英文", RefineRewrite, false, false, true},
		{"condense", "精简一下，太长了", RefineRewrite, false, false, true},
		{"polish", "润色排版一下", RefineRewrite, false, false, true},
		{"qa", "这份总结里到底说了什么结论", RefineRewrite, false, false, true},
		{"empty→safe rewrite", "", RefineRewrite, false, false, true},
		{"unknown→safe rewrite", "随便改改", RefineRewrite, false, false, true},

		// augment: same scope, fill gaps → fetch, reuse old range
		{"fill gaps", "补全遗漏的要点", RefineAugment, true, true, false},
		{"more detail", "把第二部分展开更详细一些", RefineAugment, true, true, false},
		{"more complete", "写得更全面些", RefineAugment, true, true, false},

		// extend: fresh/incremental → fetch, NEW window
		{"latest", "补充最新进展", RefineExtend, true, false, false}, // 补充(augment)+最新(extend) → extend wins
		{"today", "今天有什么新消息", RefineExtend, true, false, false},
		{"incremental", "做个增量更新", RefineExtend, true, false, false},
		{"recent week", "最近一周的情况", RefineExtend, true, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyRefine(tc.instruction)
			if got.Intent != tc.wantIntent || got.Fetch != tc.wantFetch ||
				got.ReuseTimeRange != tc.wantReuseRange || got.ReuseCitations != tc.wantReuseCite {
				t.Errorf("ClassifyRefine(%q) = %+v, want intent=%s fetch=%t reuseRange=%t reuseCite=%t",
					tc.instruction, got, tc.wantIntent, tc.wantFetch, tc.wantReuseRange, tc.wantReuseCite)
			}
		})
	}
}

// TestExtendBeatsAugment locks the priority: a request carrying BOTH an augment
// word and a fresh-data word must route to extend (it needs a new fetch window).
func TestExtendBeatsAugment(t *testing.T) {
	got := ClassifyRefine("补充一下最新的进展") // 补充 = augment, 最新 = extend
	if got.Intent != RefineExtend {
		t.Fatalf("expected extend to win over augment, got %s", got.Intent)
	}
}

// TestHardNoFetch verifies SS-08b: only a CONFIDENT rewrite (explicit keyword)
// is safe to strip fetch tools; the ambiguous fallback and the fetch paths are
// never HardNoFetch.
func TestHardNoFetch(t *testing.T) {
	cases := []struct {
		instruction string
		wantIntent  RefineIntent
		wantHard    bool
	}{
		{"翻译成英文", RefineRewrite, true},   // explicit rewrite keyword
		{"精简一下", RefineRewrite, true},    // explicit
		{"总结里说了什么", RefineRewrite, true}, // explicit Q&A
		{"随便改改", RefineRewrite, false},   // ambiguous fallback → keep tools
		{"", RefineRewrite, false},       // empty fallback → keep tools
		{"补全遗漏", RefineAugment, false},   // augment must keep fetch
		{"最新进展", RefineExtend, false},    // extend must keep fetch
	}
	for _, tc := range cases {
		got := ClassifyRefine(tc.instruction)
		if got.Intent != tc.wantIntent || got.HardNoFetch != tc.wantHard {
			t.Errorf("ClassifyRefine(%q) intent=%s hardNoFetch=%t, want intent=%s hardNoFetch=%t",
				tc.instruction, got.Intent, got.HardNoFetch, tc.wantIntent, tc.wantHard)
		}
		// Safety invariant: HardNoFetch implies no fetch.
		if got.HardNoFetch && got.Fetch {
			t.Errorf("ClassifyRefine(%q): HardNoFetch must imply !Fetch", tc.instruction)
		}
	}
}

// TestBuildRefineGuidance checks each route yields a non-empty block that names
// its route and respects the reuse-time-range rule.
func TestBuildRefineGuidance(t *testing.T) {
	rewrite := BuildRefineGuidance(ClassifyRefine("翻译成英文"))
	if !strings.Contains(rewrite, "rewrite") || !strings.Contains(rewrite, "沿用老 citations") {
		t.Errorf("rewrite guidance missing markers: %s", rewrite)
	}
	augment := BuildRefineGuidance(ClassifyRefine("补全遗漏"))
	if !strings.Contains(augment, "augment") || !strings.Contains(augment, "可以复用老 time_range") {
		t.Errorf("augment guidance should relax the time-range rule: %s", augment)
	}
	extend := BuildRefineGuidance(ClassifyRefine("最新进展"))
	if !strings.Contains(extend, "extend") || !strings.Contains(extend, "绝不复用老 time_range") {
		t.Errorf("extend guidance should forbid reusing the old range: %s", extend)
	}
}
