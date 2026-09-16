package citationtext

import (
	"strings"
	"testing"
)

func TestCanonicalizeBoundsTotalGrowth(t *testing.T) {
	// A long untouched tail must not hide the expansion of earlier groups.
	content := strings.Repeat("[1-128]", 1000) + strings.Repeat("x", maxExpansionBytes)
	got, err := Canonicalize(content, func(n int) bool { return n >= 1 && n <= 128 }, 128)
	if err == nil || got != content {
		t.Fatal("oversized expansion must fail atomically")
	}
}

func TestCanonicalize(t *testing.T) {
	valid := func(n int) bool { return n >= 1 && n <= 129 && n != 40 }
	for _, tc := range []struct {
		in, out string
		invalid bool
	}{
		{"first [9, 73], next [88], repeat [9]", "first [9][73], next [88], repeat [9]", false},
		{"[93–95] [23-24] [30，123]", "[93][94][95] [23][24] [30][123]", false},
		{"[9,9,73]", "[9][73]", false},
		{"[ 9,73 ] [ 88 ]", "[9][73] [88]", false},
		{"[9 73]", "[9 73]", true},
		{"[37–41]", "[37–41]", true},
		{"[9,999] [1]", "[9,999] [1]", true},
		{"[4–2] [1]", "[4–2] [1]", true},
		{"[1–99999]", "[1–99999]", true},
		{"[1,] [1]", "[1,] [1]", true},
		{"[1;2]", "[1;2]", true},
		{"[1][2] [P1]", "[1][2] [P1]", false},
		{"`[9,73]` [9,73]", "`[9,73]` [9][73]", false},
		{"~~~md\n[9,73]\n~~~\n[9,73]", "~~~md\n[9,73]\n~~~\n[9][73]", false},
		{"````\n```\n[9,73]\n````\n[9,73]", "````\n```\n[9,73]\n````\n[9][73]", false},
		{"[9,73](https://example.test/[1,2])", "[9,73](https://example.test/[1,2])", false},
		{"\\[9,73] ![9,73](img)", "\\[9,73] ![9,73](img)", false},
		{"[2026] [P1]", "[2026] [P1]", false},
		{"1. Heading\n    - nested [9,73]", "1. Heading\n    - nested [9][73]", false},
		{"    code [9,73]\n\nactual [9,73]", "    code [9,73]\n\nactual [9][73]", false},
		{"[See [9,73]](https://example.test) [9,73]", "[See [9,73]](https://example.test) [9][73]", false},
		{"[9,73][ref]\n\n[ref]: https://example.test\n\nactual [9,73]", "[9,73][ref]\n\n[ref]: https://example.test\n\nactual [9][73]", false},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := Canonicalize(tc.in, valid, 129)
			if got != tc.out || (err != nil) != tc.invalid {
				t.Fatalf("got %q err=%v; want %q invalid=%v", got, err, tc.out, tc.invalid)
			}
		})
	}
}

func TestValidationCannotSkipCompoundReferences(t *testing.T) {
	valid := func(n int) bool { return n == 9 || n == 73 || n == 88 }
	if !Valid("[9,73] then [88]", valid, 88, true) {
		t.Fatal("valid group rejected")
	}
	for _, text := range []string{"[9,74] then [88]", "[73–88]", "[0]", "[9,]", "`[9]`", "```\n[9]"} {
		if Valid(text, valid, 88, true) {
			t.Fatalf("invalid evidence accepted: %q", text)
		}
	}
}

func TestNumericProseDoesNotBecomeCitationCorruption(t *testing.T) {
	valid := func(n int) bool { return n == 1 || n == 3 }
	for _, prose := range []string{"[2024-2025]", "[2024–2025]", "[2026-09-14]", "[100-120]"} {
		t.Run(prose, func(t *testing.T) {
			content := "Evidence [1]. Date/page " + prose + "."
			got, err := Canonicalize(content, valid, 3)
			if err != nil || got != content {
				t.Fatalf("prose changed: %q %v", got, err)
			}
			if !Valid(content, valid, 3, true) || Valid(prose, valid, 3, true) {
				t.Fatal("prose rejected or counted as supporting evidence")
			}
		})
	}
	// An in-window gap is corruption even when none of the group members is
	// authorized. Testing only whether ANY member is authorized would miss it.
	for _, content := range []string{"[2,2]", "[1,999]", "[1-3]", "[1;2]", "[1-2-3]", "[0,999]"} {
		if _, err := Canonicalize(content, valid, 3); err == nil || Valid(content, valid, 3, false) {
			t.Fatalf("accepted corrupt group %q", content)
		}
	}
}

func TestFullyAuthorizedGroupLimit(t *testing.T) {
	valid := func(n int) bool { return n >= 1 && n <= 129 }
	if _, err := Canonicalize("[1-129]", valid, 129); err == nil {
		t.Fatal("group larger than 128 accepted")
	}
	if _, err := Canonicalize("[1-128]", valid, 129); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownEvidenceCannotExcuseMissingGroups(t *testing.T) {
	if _, err := Canonicalize("[1,2]", nil, -1); err == nil {
		t.Fatal("unknown evidence accepted a group")
	}
	prose := "Planning [2024-2025]"
	if got, err := Canonicalize(prose, nil, 0); err != nil || got != prose {
		t.Fatalf("known citation-free prose rejected: %q %v", got, err)
	}
	if Valid(prose, nil, 0, true) {
		t.Fatal("citation-free prose counted as supporting evidence")
	}
}

// CanonicalizeAdjacent backs the Workflow generation path, whose evidence window
// is a contiguous 1..N range. It must never rewrite isolated bracketed-number
// prose (PR#248 review B-2) and never fail on an unresolvable group (B-1); it
// expands only groups that abut another citation marker.
func TestCanonicalizeAdjacent(t *testing.T) {
	// window 1..30 — every small number "resolves", exactly the worker case.
	valid := func(n int) bool { return n >= 1 && n <= 30 }
	for _, tc := range []struct{ in, out string }{
		// Isolated prose ranges/lists must stay byte-identical, not become citations.
		{"预算区间 [3-5] 万元。", "预算区间 [3-5] 万元。"},
		{"涉及 [1,2] 两类问题。", "涉及 [1,2] 两类问题。"},
		{"参见附录 [12-14]。", "参见附录 [12-14]。"},
		// Unresolvable / out-of-window shapes must not abort — left as prose.
		{"依据 GB/T [50011-2010] 抗震规范执行。", "依据 GB/T [50011-2010] 抗震规范执行。"},
		{"编号 [7;9] 待确认。", "编号 [7;9] 待确认。"},
		{"排期 [2026-09] 确认。", "排期 [2026-09] 确认。"},
		// A genuine citation cluster (compound abutting another marker) expands.
		{"见消息 [1][3-5] 的讨论。", "见消息 [1][3][4][5] 的讨论。"},
		{"结论 [3-5][7]。", "结论 [3][4][5][7]。"},
		// A single marker adjacent with only spaces still counts as a cluster.
		{"参考 [2] [4,6]。", "参考 [2] [4][6]。"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			if got := CanonicalizeAdjacent(tc.in, valid); got != tc.out {
				t.Fatalf("got %q; want %q", got, tc.out)
			}
		})
	}
}
