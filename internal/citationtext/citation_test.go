package citationtext

import (
	"strings"
	"testing"
)

func TestCanonicalizeBoundsTotalGrowth(t *testing.T) {
	// A long untouched tail must not hide the expansion of earlier groups.
	content := strings.Repeat("[1-128]", 1000) + strings.Repeat("x", maxExpansionBytes)
	got, err := Canonicalize(content, func(n int) bool { return n >= 1 && n <= 128 })
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
			got, err := Canonicalize(tc.in, valid)
			if got != tc.out || (err != nil) != tc.invalid {
				t.Fatalf("got %q err=%v; want %q invalid=%v", got, err, tc.out, tc.invalid)
			}
		})
	}
}

func TestValidationCannotSkipCompoundReferences(t *testing.T) {
	valid := func(n int) bool { return n == 9 || n == 73 || n == 88 }
	if !Valid("[9,73] then [88]", valid, true) {
		t.Fatal("valid group rejected")
	}
	for _, text := range []string{"[9,74] then [88]", "[73–88]", "[0]", "[9,]", "`[9]`", "```\n[9]"} {
		if Valid(text, valid, true) {
			t.Fatalf("invalid evidence accepted: %q", text)
		}
	}
}
