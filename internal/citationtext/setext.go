// Package citationtext — setext heading neutralization.
//
// LLM-generated summary content occasionally ends a lead-in paragraph with a
// horizontal-rule line, e.g.
//
//	根据提供的 45 条混合证据（含聊天记录与文档），现将项目进展整理如下：
//	---
//	### 一、已完成事项
//
// In CommonMark a line of dashes immediately following a non-blank line is the
// SETEXT H2 HEADING syntax, not a thematic break — the whole lead-in paragraph
// becomes an <h2>. Consumers that render headings with special layout (the web
// summary view numbers h2 elements) then mis-render the paragraph as a section
// heading. The model never intends a heading here: it means a visual
// separator, and only '-' has that reading once blank-separated. '=' has NO
// thematic-break reading in CommonMark, so an underline of equals signs is
// left byte-identical: rewriting "标题\n===\n正文" to "标题\n\n===\n正文"
// would destroy a correctly-rendering H1 and leak the '=' run as literal body
// text (PR#268 round-1, yujiawei §1 / mocha P1 / Jerry-Xin B-4).
//
// The fix is deterministic post-processing at every content write site: insert
// a blank line between the paragraph and the dash rule so the renderer keeps
// them as paragraph + thematic break. This is intentionally conservative:
// only lines consisting solely of 3+ dashes (plus surrounding whitespace) are
// treated. Lines inside fenced code blocks and indented code blocks are left
// byte-identical. Known, accepted limitations (documented, not asserted away):
// 1-2 character underlines ("-"/"--" are setext H2 in CommonMark) are out of
// scope, raw HTML blocks are not tracked so inserting a blank line can
// terminate one early, and lazy-continuation paragraphs keep their hijack —
// when a paragraph's last line is indented 4+ spaces ("para\n    more\n---")
// the normalizer treats that line as indented code and skips the rule, even
// though CommonMark lets indented text continue a paragraph (PR#268 round-1
// P2-4 / round-2 A-3). Hand-edit and restore paths (EditSummary, PersonalEdit,
// PersonalDraft, RestorePersonalVersion, failure-restore) persist
// client-/snapshot-supplied bytes verbatim by design — they are deliberately
// out of scope (PR#268 round-3: normalizing them would mutate what the user
// or a version snapshot authored; the asymmetry with normalized team rows is
// accepted).
package citationtext

import (
	"strings"
	"unicode/utf8"
)

const (
	// maxSetextContentRunes caps the paragraph line considered for a setext
	// underline; longer "paragraphs" are treated as noise and left alone.
	maxSetextContentRunes = 10000
	// setextMaxContentBytes bounds the scanned body in BYTES. It is sized to
	// be >= the largest raw input the ENFORCING refine write sites accept:
	// maxContentBytes is 500*1024 = 512000 bytes
	// (internal/api/handler/edit.go), and the refine gates measure the
	// STRIPPED, PRE-NORMALIZE length (PR#268 round-4/5 A-13) — so the
	// normalizer's input at every refine call site is bounded by 512000
	// bytes, matching this guard exactly. Re-normalizing an already-
	// normalized cap+growth persisted row is an idempotent no-op that never
	// re-enters this path. (PR#268 round-1 P2-1: the old 200000-byte guard
	// silently bypassed accepted 200KB-500KB summaries.)
	// Uncapped write surfaces that DO route through this normalizer:
	//   - CreateAgentSummary (mochashanyao round-3 P2): no content cap on
	//     the HTTP write path, so an agent deliverable above 512000 bytes
	//     is returned unmodified by this guard — silently skipping
	//     normalization rather than being rejected. Tracked as A-11 in the
	//     PR#268 follow-up list.
	//   - Worker write sites (meta_processor / personal_processor): LLM
	//     output bounded only by the token config, not by maxContentBytes
	//     at the HTTP layer — so this guard is an arbitrary DoS bound
	//     rather than "above the enforced cap" on those surfaces. Tracked
	//     with A-11 in the same follow-up.
	// Non-normalizing write surfaces (do NOT route through this normalizer;
	// listed for completeness, not enforced by this guard):
	//   - Hand-edit paths (EditSummary, PersonalEdit, personal draft):
	//     reject `len(req.Content) > maxContentBytes` on the RAW input
	//     basis, so a client that round-trips a refine's persisted (cap +
	//     normalizeGrowth) bytes back through a hand-edit gets 40010. This
	//     asymmetry is a consequence of the A-13 strip-basis prescription
	//     (PR#268 A-17: mocha r5 P2-2 origin + this seat's executable
	//     confirmation); the invariant decision is deferred.
	//   - Restore paths (RestoreSummaryVersion, RestorePersonalVersion):
	//     take no content input and copy the stored version's content
	//     server-side, so no cap check applies at all — restore is not on
	//     the reject side of the A-17 asymmetry.
	setextMaxContentBytes = 512000
)

// NormalizeSetextHeadings returns content with a blank line inserted between a
// non-blank paragraph line and a following setext underline (3+ dashes only —
// see the package comment for why '=' is excluded). Lines inside fenced (```
// or ~~~) or indented (4-space/tab) code blocks are never modified. ATX
// headings (## …), list items, quotes and table rows are not underlines for
// this purpose. The insertion rule is unconditional on shape: it applies after
// any non-blank, non-indented, non-rule line, including lines that close or
// continue list/quote/table containers. For container predecessors the insert
// is behaviorally inert — a bare rule after those constructs already parses as
// a thematic break in CommonMark, so the blank line changes the rendered HTML
// not at all (verified through a reference implementation, PR#268 round-4
// P2-3); the insertion only matters for the plain-paragraph case, which is
// the shape the model actually emits.
func NormalizeSetextHeadings(content string) string {
	if !strings.Contains(content, "\n") {
		return content
	}
	if len(content) > setextMaxContentBytes {
		// Defensive bound: bodies above the raw input cap
		// (maxContentBytes = 512000) short-circuit scanning rather than
		// modifying pathological inputs. This bound must stay >=
		// maxContentBytes; the refine gates measure the stripped input
		// (A-13), so the normalizer never sees more than maxContentBytes
		// bytes on the refine path (re-normalizing an already-normalized
		// cap+growth persisted row is an idempotent no-op). See the
		// constant's doc-comment above for the full write-surface matrix
		// (uncapped agent-save / worker surfaces; non-normalizing
		// hand-edit / restore).
		return content
	}
	lines := strings.Split(content, "\n")
	// Single-pass rebuild into a fresh slice (PR#268 round-1 B-5: the old
	// in-place append-per-match copied the remaining tail each match, giving
	// Θ(lines²) — measured 40s at ~195KB inside request handlers).
	out := make([]string, 0, len(lines)+8)
	// inFence tracks fenced code state; fenceMarker is the opening sequence
	// (at least 3 backticks or tildes). A closing fence must be at least as
	// long and use the same character.
	inFence := false
	fenceChar := byte(0)
	fenceLen := 0
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if inFence {
			if closesFence(line, fenceChar, fenceLen) {
				inFence = false
			}
			out = append(out, line)
			continue
		}
		// Indented-code check must run BEFORE opensFence (PR#268 round-1
		// B-3 under-fire leg): a 4-space-indented ```text line is indented
		// code content in CommonMark, not a fence opener. Evaluating the
		// fence opener first flipped fence state on it and suppressed
		// normalization for the entire remainder of the document.
		if isIndentedCode(line) {
			out = append(out, line)
			continue
		}
		if open, ch, n := opensFence(line); open {
			inFence = true
			fenceChar = ch
			fenceLen = n
			out = append(out, line)
			continue
		}
		if !isSetextUnderline(line) || i == 0 {
			out = append(out, line)
			continue
		}
		prev := lines[i-1]
		prevBlank := strings.TrimRight(prev, "\r") == ""
		if prevBlank || isIndentedCode(prev) || isSetextUnderline(prev) {
			// A blank line, indented code, or a chain of rules before the
			// underline already breaks the setext interpretation; leave as-is.
			out = append(out, line)
			continue
		}
		if runeCount(prev) > maxSetextContentRunes {
			out = append(out, line)
			continue
		}
		// Insert a blank line before the underline: paragraph + thematic
		// break. The blank follows the document's line-ending convention
		// (a CRLF file gets a "\r" line, keeping the file self-consistent).
		if strings.HasSuffix(line, "\r") {
			out = append(out, "\r")
		} else {
			out = append(out, "")
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// isSetextUnderline reports whether the line is exactly 3+ '-' characters with
// optional surrounding whitespace. CommonMark also accepts 1-2 dash runs and
// '=' runs as setext underlines; those shapes are deliberately out of scope
// here (documented limitation), and '=' must not be neutralized at all because
// it has no thematic-break reading (package comment).
func isSetextUnderline(line string) bool {
	trimmed := strings.TrimRight(strings.TrimLeft(line, " "), " \t\r")
	if len(trimmed) < 3 {
		return false
	}
	if trimmed[0] != '-' {
		return false
	}
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] != '-' {
			return false
		}
	}
	return true
}

// fenceIndent returns the leading-space count of the original line, before any
// trimming. CommonMark allows at most 3 spaces of indentation for a fence
// line; 4+ spaces make it indented-code content. (PR#268 round-1 B-3: the old
// guard measured indentation AFTER TrimLeft had already stripped every
// leading space, so it only ever counted tabs and never enforced the rule.)
func fenceIndent(line string) int {
	ws := line[:len(line)-len(strings.TrimLeft(line, " 	"))]
	if strings.Contains(ws, "	") {
		return 4 // any tab in leading whitespace => visual column >= 4
	}
	return len(ws)
}

// opensFence detects a fenced code block opener: 3+ backticks or tildes,
// optionally indented up to 3 spaces, with an optional info string.
func opensFence(line string) (bool, byte, int) {
	if fenceIndent(line) > 3 {
		return false, 0, 0
	}
	trimmed := strings.TrimLeft(line, " \t")
	if len(trimmed) < 3 {
		return false, 0, 0
	}
	c := trimmed[0]
	if c != '`' && c != '~' {
		return false, 0, 0
	}
	n := 0
	for n < len(trimmed) && trimmed[n] == c {
		n++
	}
	if n < 3 {
		return false, 0, 0
	}
	// Backtick fences must not have backticks in the info string; tildes may.
	if c == '`' && strings.Contains(trimmed[n:], "`") {
		return false, 0, 0
	}
	return true, c, n
}

// closesFence reports whether line closes the current fence: same character,
// length at least the opener's, nothing but spaces after. A closing fence
// indented 4+ spaces is code content in CommonMark and must NOT close the
// block (PR#268 round-1 B-3 over-fire leg: accepting it inserted a blank line
// inside an open code block, breaking the byte-identity contract).
func closesFence(line string, c byte, n int) bool {
	if fenceIndent(line) > 3 {
		return false
	}
	trimmed := strings.TrimLeft(line, " \t")
	i := 0
	for i < len(trimmed) && trimmed[i] == c {
		i++
	}
	if i < n {
		return false
	}
	return strings.TrimSpace(trimmed[i:]) == ""
}

// isIndentedCode reports whether the line starts with 4 spaces or a tab
// (indented code block content in CommonMark, when not inside a list item —
// list continuation lines are rare in generated summaries and the
// conservative behavior of leaving them untouched is safe).
func isIndentedCode(line string) bool {
	return strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t")
}

func runeCount(s string) int {
	return utf8.RuneCountInString(s)
}
