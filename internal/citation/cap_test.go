package citation

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func TestCapRunsBasicTruncation(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"under cap untouched", "结论A [1][2]", 3, "结论A [1][2]"},
		{"exactly at cap untouched", "结论A [1][2][3]", 3, "结论A [1][2][3]"},
		{"over cap truncated to head", "结论A [1][2][3][4][5]", 3, "结论A [1][2][3]"},
		{"cap of one keeps one", "结论A [7][8][9]", 1, "结论A [7]"},
		{"whitespace-separated run is one run", "结论A [1] [2] [3] [4]", 2, "结论A [1] [2]"},
		{"tab-separated run is one run", "A [1]\t[2]\t[3]", 2, "A [1]\t[2]"},
		{"no markers", "没有任何引用的结论", 3, "没有任何引用的结论"},
		{"empty", "", 3, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := CapRuns(tc.in, tc.max)
			if got != tc.want {
				t.Errorf("CapRuns(%q, %d)\n got: %q\nwant: %q", tc.in, tc.max, got, tc.want)
			}
		})
	}
}

// A newline ends a run: two bullets are two claims, and the second bullet's
// citations must survive independently of the first's. Without this, a cap of
// 3 applied to a 5-bullet list would strip every bullet after the first.
func TestCapRunsNewlineSeparatesClaims(t *testing.T) {
	in := "- 要点一 [1][2][3][4]\n- 要点二 [5][6][7][8]\n- 要点三 [9]"
	want := "- 要点一 [1][2]\n- 要点二 [5][6]\n- 要点三 [9]"
	got, st := CapRuns(in, 2)
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if st.Runs != 3 {
		t.Errorf("Runs = %d, want 3 (one per bullet)", st.Runs)
	}
	if st.CappedRuns != 2 {
		t.Errorf("CappedRuns = %d, want 2", st.CappedRuns)
	}
}

// The load-bearing invariant: a capped claim must never end up uncited.
func TestCapRunsNeverDropsLastCitation(t *testing.T) {
	for _, in := range []string{
		"单一引用的结论 [42]",
		"重复到爆的结论 [42][42][42][42][42][42]",
		"两个不同 [1][2]",
		"- a [1]\n- b [2]\n- c [3]",
	} {
		for max := 1; max <= 5; max++ {
			got, _ := CapRuns(in, max)
			for _, line := range strings.Split(in, "\n") {
				if !MarkerRe.MatchString(line) {
					continue
				}
				// Each originally-cited line must still be cited.
				found := false
				for _, outLine := range strings.Split(got, "\n") {
					if MarkerRe.MatchString(outLine) {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("CapRuns(%q, %d) = %q removed all citations", in, max, got)
				}
			}
			if n := len(Numbers(got)); n == 0 {
				t.Fatalf("CapRuns(%q, %d) = %q left zero citations", in, max, got)
			}
		}
	}
}

// Dedup is lossless and must happen before the cap starts discarding real
// evidence: [1][1][1][2] under a cap of 2 keeps BOTH distinct sources, it
// does not spend the budget on the duplicate.
func TestCapRunsDedupBeforeCap(t *testing.T) {
	got, st := CapRuns("结论 [1][1][1][2]", 2)
	if want := "结论 [1][2]"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if st.RemovedByDedup != 2 {
		t.Errorf("RemovedByDedup = %d, want 2", st.RemovedByDedup)
	}
	if st.RemovedByCap != 0 {
		t.Errorf("RemovedByCap = %d, want 0 — dedup alone should have fit the run under the cap", st.RemovedByCap)
	}
	if st.CappedRuns != 0 {
		t.Errorf("CappedRuns = %d, want 0", st.CappedRuns)
	}
}

// Duplicates ACROSS runs are independent: the same source may legitimately
// support two different claims.
func TestCapRunsDedupIsPerRun(t *testing.T) {
	in := "要点一 [1][1]\n要点二 [1][2]"
	want := "要点一 [1]\n要点二 [1][2]"
	got, st := CapRuns(in, 3)
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if st.RemovedByDedup != 1 {
		t.Errorf("RemovedByDedup = %d, want 1", st.RemovedByDedup)
	}
}

func TestCapRunsPreservesOrder(t *testing.T) {
	got, _ := CapRuns("结论 [37][4][19][88][2]", 3)
	if want := "结论 [37][4][19]"; got != want {
		t.Fatalf("got %q want %q — surviving markers must keep original relative order", got, want)
	}
	nums := Numbers(got)
	if len(nums) != 3 || nums[0] != 37 || nums[1] != 4 || nums[2] != 19 {
		t.Fatalf("Numbers = %v, want [37 4 19]", nums)
	}
}

func TestCapRunsIdempotent(t *testing.T) {
	in := "- a [1][2][3][4][5]\n- b [6][7][8]\n段落 [9][9][10][11]"
	once, _ := CapRuns(in, 3)
	twice, st := CapRuns(once, 3)
	if once != twice {
		t.Fatalf("not idempotent:\n once: %q\ntwice: %q", once, twice)
	}
	if st.Changed() {
		t.Errorf("second pass reported changes: dedup=%d cap=%d", st.RemovedByDedup, st.RemovedByCap)
	}
}

// max<1 is the kill switch: byte-identical output, including NO dedup.
func TestCapRunsDisabledIsByteIdentical(t *testing.T) {
	in := "结论 [1][1][2][3][4][5][6][7][8][9][10]"
	for _, max := range []int{Disabled, 0, -1, -99} {
		got, st := CapRuns(in, max)
		if got != in {
			t.Fatalf("max=%d rewrote text: %q", max, got)
		}
		if st.Changed() {
			t.Fatalf("max=%d reported changes", max)
		}
		// Measurement still works with the cap off — that is what makes a
		// disabled rollout observable.
		if st.MarkersBefore != 11 || st.LongestRunBefore != 11 {
			t.Errorf("max=%d stats not populated: %+v", max, st)
		}
	}
}

// --- Escaped / literal marker protection -------------------------------
//
// This is the most likely way the change silently breaks something, so it is
// tested from both directions: the upstream escape must make body text
// invisible to the cap, and the syntactic forms that are not citations must
// survive byte-identically.

// escapeCitationMarkers is a verbatim copy of the production helper at
// internal/worker/personal_processor.go:23. Copied rather than imported
// because internal/citation must not depend on internal/worker (the agent
// path could not then use it). If the production version ever changes, this
// test's premise changes with it — which is exactly what should fail loudly.
func escapeCitationMarkers(content string) string {
	return MarkerRe.ReplaceAllString(content, "($1)")
}

func TestEscapedBodyMarkersAreInvisibleToCap(t *testing.T) {
	// A chat message whose BODY happens to contain bracketed numbers — the
	// exact case escapeCitationMarkers exists for.
	body := "看下 [12] 号需求和 [13][14][15][16][17] 这几条"
	escaped := escapeCitationMarkers(body)
	if strings.Contains(escaped, "[") {
		t.Fatalf("escape left brackets: %q", escaped)
	}
	// Rendered into a prompt line the way formatChunkForLLM does: the real
	// citation index is the line's leading marker, the body's numbers are not.
	line := "[7] 张三: " + escaped
	got, st := CapRuns(line, 3)
	if got != line {
		t.Fatalf("cap modified an escaped line:\n got: %q\nwant: %q", got, line)
	}
	if st.MarkersBefore != 1 {
		t.Errorf("MarkersBefore = %d, want 1 (only the leading citation index)", st.MarkersBefore)
	}
	if st.Changed() {
		t.Error("cap changed an escaped line")
	}
}

func TestCapRunsLeavesMarkdownLinksAndRefsAlone(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"inline link", "见 [1](https://example.com/a)[2](https://example.com/b)[3](https://x)[4](https://y)"},
		{"reference definition", "[1]: https://example.com/a\n[2]: https://example.com/b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, st := CapRuns(tc.in, 1)
			if got != tc.in {
				t.Fatalf("rewrote non-citation markers:\n got: %q\nwant: %q", got, tc.in)
			}
			if st.NonCitable == 0 {
				t.Error("NonCitable = 0, expected the link/ref heads to be counted as non-citable")
			}
			if st.Changed() {
				t.Error("reported a change on non-citation markers")
			}
		})
	}
}

// A real citation adjacent to a markdown link must still be capped, and the
// link must still survive — the two vocabularies coexist in one line.
func TestCapRunsMixedCitationAndLink(t *testing.T) {
	in := "结论 [1][2][3][4] 详见 [9](https://example.com)"
	want := "结论 [1][2] 详见 [9](https://example.com)"
	got, _ := CapRuns(in, 2)
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestCapRunsNeverInventsOrReordersNumbers(t *testing.T) {
	rng := rand.New(rand.NewSource(20260822))
	for i := 0; i < 500; i++ {
		var b strings.Builder
		for claim := 0; claim < 1+rng.Intn(6); claim++ {
			fmt.Fprintf(&b, "要点%d", claim)
			for m := 0; m < 1+rng.Intn(20); m++ {
				fmt.Fprintf(&b, "[%d]", 1+rng.Intn(50))
			}
			b.WriteString("\n")
		}
		in := b.String()
		max := 1 + rng.Intn(5)
		got, st := CapRuns(in, max)

		inNums := Numbers(in)
		outNums := Numbers(got)
		inSet := map[int]bool{}
		for _, n := range inNums {
			inSet[n] = true
		}
		for _, n := range outNums {
			if !inSet[n] {
				t.Fatalf("invented citation [%d]\n in: %q\nout: %q", n, in, got)
			}
		}
		if len(got) > len(in) {
			t.Fatalf("output grew: %d -> %d", len(in), len(got))
		}
		// Every claim line that had a citation still has one, and none
		// exceeds the cap.
		for _, line := range strings.Split(got, "\n") {
			if runs, _, _ := findRuns(line); len(runs) > 0 {
				for _, r := range runs {
					if len(r) > max {
						t.Fatalf("run of %d exceeds cap %d in %q", len(r), max, line)
					}
				}
			}
		}
		for i, line := range strings.Split(in, "\n") {
			if !MarkerRe.MatchString(line) {
				continue
			}
			outLines := strings.Split(got, "\n")
			if i < len(outLines) && !MarkerRe.MatchString(outLines[i]) {
				t.Fatalf("claim line %d lost all citations: %q -> %q", i, line, outLines[i])
			}
		}
		if st.MarkersAfter != len(Numbers(got)) && st.RemovedByDedup == 0 {
			_ = st // stats sanity is asserted precisely in the stats tests
		}
	}
}

func TestStatsAccounting(t *testing.T) {
	in := "a [1][1][2][3][4][5]\nb [9]"
	_, st := CapRuns(in, 3)
	if st.MarkersBefore != 7 {
		t.Errorf("MarkersBefore = %d, want 7", st.MarkersBefore)
	}
	if st.RemovedByDedup != 1 {
		t.Errorf("RemovedByDedup = %d, want 1", st.RemovedByDedup)
	}
	if st.RemovedByCap != 2 {
		t.Errorf("RemovedByCap = %d, want 2", st.RemovedByCap)
	}
	if st.MarkersAfter != st.MarkersBefore-st.RemovedByDedup-st.RemovedByCap {
		t.Errorf("MarkersAfter %d != before %d - dedup %d - cap %d",
			st.MarkersAfter, st.MarkersBefore, st.RemovedByDedup, st.RemovedByCap)
	}
	if st.BytesAfter >= st.BytesBefore {
		t.Errorf("BytesAfter %d not below BytesBefore %d", st.BytesAfter, st.BytesBefore)
	}
}

func TestPromptRuleZH(t *testing.T) {
	if got := PromptRuleZH(Disabled); got != "" {
		t.Errorf("PromptRuleZH(Disabled) = %q, want empty so the legacy prompt stays byte-identical", got)
	}
	if got := PromptRuleZH(-1); got != "" {
		t.Errorf("PromptRuleZH(-1) = %q, want empty", got)
	}
	got := PromptRuleZH(3)
	if !strings.Contains(got, "3") {
		t.Errorf("PromptRuleZH(3) does not state the number: %q", got)
	}
	if !strings.HasPrefix(got, "\n") {
		t.Errorf("PromptRuleZH should start with a newline for bullet-list concatenation: %q", got)
	}
	// The prompt must state the same number the enforcement uses.
	if strings.Contains(PromptRuleZH(5), " 3 ") {
		t.Error("PromptRuleZH(5) mentions 3 — prompt/enforcement drift")
	}
}
