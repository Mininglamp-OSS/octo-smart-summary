package citationtext

import (
	"strings"
	"testing"
	"time"
)

func TestNormalizeSetextHeadingsInsertsBlankBeforeRule(t *testing.T) {
	in := "根据提供的 45 条混合证据（含聊天记录与文档），现将项目当前进展按 **已完成、进行中、风险阻塞、下一步计划** 四类整理如下：\n---\n### 一、已完成事项\n"
	want := "根据提供的 45 条混合证据（含聊天记录与文档），现将项目当前进展按 **已完成、进行中、风险阻塞、下一步计划** 四类整理如下：\n\n---\n### 一、已完成事项\n"
	if got := NormalizeSetextHeadings(in); got != want {
		t.Fatalf("NormalizeSetextHeadings mismatch:\n got=%q\nwant=%q", got, want)
	}
}

// B-1 (yujiawei §1 / mocha P1 / Jerry-Xin B-4, round-1 PR#268): '=' is NOT a
// thematic-break character in CommonMark, so neutralizing a '===' underline
// surfaces it as literal visible text and destroys a correctly-rendering H1.
// The dash rule stays normalizable; equals underlines must stay byte-identical.
func TestNormalizeSetextHeadingsEqualsUnderlineUntouched(t *testing.T) {
	in := "总结标题行\n===\n后续内容\n"
	if got := NormalizeSetextHeadings(in); got != in {
		t.Fatalf("equals underline must stay byte-identical:\n got=%q\nwant=%q", got, in)
	}
}

// B-3 over-fire leg (yujiawei 2.2 / Jerry-Xin B-3): a 4-space-indented closing
// fence is code CONTENT in CommonMark, so the block stays open and the trailing
// '---' after 'text' must NOT get a blank line inserted inside the code block.
func TestNormalizeSetextHeadingsNoBlankInsideOpenCodeBlock(t *testing.T) {
	in := "```\ncode\n    ```\ntext\n---\nmore\n"
	if got := NormalizeSetextHeadings(in); got != in {
		t.Fatalf("4-space-indented closer is code content; blank line must not be inserted:\n got=%q\nwant=%q", got, in)
	}
}

// B-3 under-fire leg: a 4-space-indented fence-lookalike is indented code, not
// a fence. opensFence must not flip fence state on it, so later real setext
// shapes are still normalized.
func TestNormalizeSetextHeadingsIndentedCodeBeforeFenceLookalike(t *testing.T) {
	in := "para\n\n    ```text\n段落B\n---\n尾部\n"
	want := "para\n\n    ```text\n段落B\n\n---\n尾部\n"
	if got := NormalizeSetextHeadings(in); got != want {
		t.Fatalf("indented fence-lookalike must not suppress later normalization:\n got=%q\nwant=%q", got, want)
	}
}

// P2-2 (yujiawei 2.5 / Jerry-Xin P2-2): CRLF content must normalize; '\r'
// left by the '\n' split must not defeat the underline/blank-line checks.
func TestNormalizeSetextHeadingsCRLF(t *testing.T) {
	in := "段落文字\r\n---\r\n### 后续\r\n"
	want := "段落文字\r\n\r\n---\r\n### 后续\r\n"
	if got := NormalizeSetextHeadings(in); got != want {
		t.Fatalf("CRLF input must be normalized:\n got=%q\nwant=%q", got, want)
	}
}

// P2-1 (yujiawei 2.4 / mocha / Jerry-Xin P2-1): write sites accept up to
// maxContentBytes (500KB); the neutralizer's own bound must cover that range.
// 135KB of valid body must not bypass the fix.
func TestNormalizeSetextHeadingsLargeBodyWithinContentCap(t *testing.T) {
	var b strings.Builder
	b.WriteString("# 报告\n\n")
	// 300KB: above the old 200000-byte guard, below maxContentBytes (512000)
	// that every write site accepts — exactly the bypass band the reviewers
	// flagged (yujiawei 2.4 executed probe shape).
	for b.Len() < 300*1024 {
		b.WriteString("这是一段足够长的正文内容，用于把文档体量推过旧的 200KB 字节守卫。\n\n")
	}
	b.WriteString("结论段落\n---\n后续\n")
	in := b.String()
	want := strings.Replace(in, "结论段落\n---", "结论段落\n\n---", 1)
	if got := NormalizeSetextHeadings(in); got != want {
		t.Fatalf("body within maxContentBytes must still be normalized (got len=%d)", len(got))
	}
}

func TestNormalizeSetextHeadingsLeavesStandaloneRule(t *testing.T) {
	// Blank line before --- already breaks setext; must stay byte-identical.
	in := "段落一\n\n---\n\n段落二\n"
	if got := NormalizeSetextHeadings(in); got != in {
		t.Fatalf("expected byte-identical output, got %q", got)
	}
}

func TestNormalizeSetextHeadingsLeavesFirstLineUnderline(t *testing.T) {
	in := "---\n内容\n"
	if got := NormalizeSetextHeadings(in); got != in {
		t.Fatalf("expected byte-identical output, got %q", got)
	}
}

func TestNormalizeSetextHeadingsSkipsFencedCode(t *testing.T) {
	in := "```\ntext\n---\nmore\n```\n段落\n---\n"
	want := "```\ntext\n---\nmore\n```\n段落\n\n---\n"
	if got := NormalizeSetextHeadings(in); got != want {
		t.Fatalf("mismatch:\n got=%q\nwant=%q", got, want)
	}
}

func TestNormalizeSetextHeadingsSkipsTildeFence(t *testing.T) {
	in := "~~~go\ncode\n---\n~~~\n段落\n---\n"
	want := "~~~go\ncode\n---\n~~~\n段落\n\n---\n"
	if got := NormalizeSetextHeadings(in); got != want {
		t.Fatalf("mismatch:\n got=%q\nwant=%q", got, want)
	}
}

func TestNormalizeSetextHeadingsSkipsIndentedCode(t *testing.T) {
	in := "段落:\n\n    indented\n    ---\n\n后续\n---\n"
	want := "段落:\n\n    indented\n    ---\n\n后续\n\n---\n"
	if got := NormalizeSetextHeadings(in); got != want {
		t.Fatalf("mismatch:\n got=%q\nwant=%q", got, want)
	}
}

func TestNormalizeSetextHeadingsMixedDashesNotUnderline(t *testing.T) {
	// "- 4 -" is not a bare rule; must not be treated as underline.
	in := "标题文字\n- 4 -\n"
	if got := NormalizeSetextHeadings(in); got != in {
		t.Fatalf("expected byte-identical output, got %q", got)
	}
}

func TestNormalizeSetextHeadingsMultipleRules(t *testing.T) {
	in := "甲\n---\n乙\n---\n丙\n"
	want := "甲\n\n---\n乙\n\n---\n丙\n"
	if got := NormalizeSetextHeadings(in); got != want {
		t.Fatalf("mismatch:\n got=%q\nwant=%q", got, want)
	}
}

func TestNormalizeSetextHeadingsNoNewline(t *testing.T) {
	in := "single line no rule"
	if got := NormalizeSetextHeadings(in); got != in {
		t.Fatalf("expected byte-identical output, got %q", got)
	}
}

// P2-7 (Jerry-Xin): the old Oversized test was vacuous — its 200,010-rune body
// tripped the prev-line cap independently, so removing the size guard left the
// suite green. Rewrite with a short-prev-line body above the guard: removing
// or lowering the guard must fail this test (the body then normalizes).
// B-5 acceptance (Jerry-Xin): a 20k-rule-pair body must normalize in <500ms;
// the quadratic in-place insertion measured 2.10s at 120KB and ~40s near the
// size guard. Kills mutant M-Q (restore in-place insertion).
func TestNormalizeSetextHeadingsLinearPerformance(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 20000; i++ {
		b.WriteString("para\n---\ntext\n\n")
	}
	in := b.String()
	start := time.Now()
	got := NormalizeSetextHeadings(in)
	elapsed := time.Since(start)
	want := strings.ReplaceAll(in, "para\n---", "para\n\n---")
	if got != want {
		t.Fatalf("normalization output mismatch under perf probe")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("normalization took %s for a %dKB body; quadratic insertion regressed (B-5)", elapsed, len(in)/1024)
	}
}

// B-N1 (Jerry-Xin round-2 🔴, PR#268): CommonMark expands a tab in leading
// indentation to the next 4-column tab stop, so ANY tab in the leading
// whitespace run makes the line indented-code content — never a valid opening
// or closing fence. The round-2 fenceIndent rewrite counted only literal
// spaces and re-opened both round-1 B-3 mechanisms on tab-indented shapes
// (over-fire: blank inserted inside a still-open code block; under-fire: a
// space+tab fence-lookalike suppresses normalization for the rest of the
// document). Pin-gap evidence: the correctness-direction fix SURVIVED the
// shipped suite — mutant M-TABC (revert fenceIndent to space-only counting)
// must turn these pins red.
func TestNormalizeSetextHeadingsTabIndentedFenceShapes(t *testing.T) {
	cases := []struct{ name, in string }{
		// F3: tab-indented closer is code CONTENT; the fence stays open and
		// the trailing para+rule must NOT get a blank line inside the block.
		{"F3 tab closer", "```\ncode\n\t```\npara\n---\nmore\n"},
		// F4: space+tab closer, same contract.
		{"F4 space+tab closer", "```\ncode\n  \t```\ntext\n---\nmore\n"},
		// F6: tilde variant.
		{"F6 tilde tab closer", "~~~\ncode\n\t~~~\ntext\n---\nmore\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeSetextHeadings(tc.in); got != tc.in {
				t.Fatalf("M-TABC: tab-indented closer must not close the fence (blank line inserted inside open code block):\n got=%q\nwant=%q", got, tc.in)
			}
		})
	}
	// F7: a space+tab fence-lookalike OPENER is indented code (visual column
	// >= 4); it must not flip fence state, so both later setext hijacks
	// normalize. (The isIndentedCode pre-check only catches literal-tab
	// prefixes, NOT space+tab — fenceIndent must carry the tab rule itself.)
	t.Run("F7 space+tab opener lookalike", func(t *testing.T) {
		in := "para\n\n  \t```\ninside\n---\npara2\n---\n"
		want := "para\n\n  \t```\ninside\n\n---\npara2\n\n---\n"
		if got := NormalizeSetextHeadings(in); got != want {
			t.Fatalf("M-TABC: space+tab lookalike must not suppress later normalization:\n got=%q\nwant=%q", got, want)
		}
	})
}

// M4 pin (Jerry-Xin r1 P2-4 / r2 A-2, PR#268): the underline-line indent
// check must stay — removing it corrupts `para\n    ---` into paragraph +
// indented code block (the 4-space-indented underline is indented-code
// content, NOT an underline). Passes today; exists to die under the mutation.
func TestNormalizeSetextHeadingsIndentedUnderlineUntouched(t *testing.T) {
	in := "para\n    ---\n"
	if got := NormalizeSetextHeadings(in); got != in {
		t.Fatalf("M4: 4-space-indented underline is indented-code content; byte-identity broken:\n got=%q\nwant=%q", got, in)
	}
}

// Nit pin (mochashanyao round-3): the documented limitations at the package
// comment currently have no assertions, so a future refactor could widen them
// silently. These three cases pin the accepted boundary — they exist to
// surface an accidental change, not to enshrine desired behavior.
func TestNormalizeSetextHeadingsDocumentedLimitations(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // "" = byte-identical
	}{
		// 1-2 dash underlines are setext H2 in CommonMark but out of scope.
		{"short underline", "para\n-\n", ""},
		// Raw HTML blocks are not tracked: the blank line IS inserted, which
		// terminates the block early — reference rendering (markdown-it,
		// commonmark preset) of the output gains an <hr/> inside the element
		// ("<div> | text | <hr /> | </div>"). That mutation is exactly the
		// documented limitation; this pin locks the current boundary.
		{"html block", "<div>\ntext\n---\n</div>\n", "<div>\ntext\n\n---\n</div>\n"},
		// Lazy continuation: the indented last paragraph line suppresses the
		// fix (A-3) — byte-identical, hijack survives by design.
		{"lazy continuation", "para\n    more\n---\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.want
			if want == "" {
				want = tc.in
			}
			if got := NormalizeSetextHeadings(tc.in); got != want {
				t.Fatalf("documented-limitation boundary changed (update the setext.go:26 block deliberately if intended):\n got=%q\nwant=%q", got, want)
			}
		})
	}
}

func TestNormalizeSetextHeadingsOversized(t *testing.T) {
	var b strings.Builder
	b.WriteString("# 报告\n\n短段落\n\n")
	for b.Len() <= setextMaxContentBytes {
		b.WriteString("这是一段足够长的正文内容，用于把文档体量推过守卫而不触发任何其他上限。\n\n")
	}
	b.WriteString("结论段落\n---\n后续\n")
	in := b.String()
	if got := NormalizeSetextHeadings(in); got != in {
		t.Fatalf("oversized input must stay byte-identical")
	}
}
