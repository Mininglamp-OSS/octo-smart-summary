package handler

import (
	"strings"
)

// stripAgentPreamble removes conversational preamble that agents sometimes
// generate before the actual deliverable content. This is a defense-in-depth
// layer on top of prompt discipline (see internal/agent/prompts/summary_refine.md
// "输出格式硬规则") — even with tight prompts, models occasionally leak an
// opener like「好的。根据引用的老总结内容,我现在将其转化为...」that
// pollutes the summary when saved verbatim.
//
// Heuristic (owner decision Q3 = A, see
// CHAT-REFERENCE-PREVIEW-AND-RANGE-SAVE-v1):
//
//   Find the first markdown heading (# / ## / ### / ...) or horizontal rule (---).
//   If the text before it is < 500 chars AND is not itself a heading/rule,
//   treat it as preamble and strip.
//
// Deliberately conservative: pure prose summaries with no heading pass through
// unchanged (the 500-char cap ensures we don't strip a real intro paragraph).
//
// Contract:
//   - input never nil (empty string returns empty string)
//   - never touches the tail (closing "希望..." style padding is dropped by
//     the prompt rule alone; if not, add stripAgentPostscript later)
//   - preserves surrounding whitespace of the retained portion (trimming the
//     leading newlines that follow the heading is caller's job if desired)
const stripPreambleMaxLen = 500

func stripAgentPreamble(content string) string {
	if content == "" {
		return content
	}

	// Scan line by line, locate first heading or rule line.
	lines := strings.SplitAfter(content, "\n") // keep trailing \n so we can rebuild losslessly
	var (
		firstStructIdx = -1 // line index of first heading / rule
		preambleLen    = 0  // byte length of everything before that line
	)

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if isHeadingOrRule(trimmed) {
			firstStructIdx = i
			break
		}
		preambleLen += len(line)
	}

	// No structure marker found anywhere → don't touch (pure prose summary,
	// possibly a valid intro-only agent reply).
	if firstStructIdx == -1 {
		return content
	}

	// Preamble too long → likely real content, not conversational opener.
	if preambleLen >= stripPreambleMaxLen {
		return content
	}

	// Preamble is empty → nothing to strip.
	if firstStructIdx == 0 {
		return content
	}

	return strings.Join(lines[firstStructIdx:], "")
}

// isHeadingOrRule returns true if the line is a markdown heading (# .. ######)
// or horizontal rule (---, ***, ___). Called with an already-trimmed line.
func isHeadingOrRule(trimmed string) bool {
	if trimmed == "" {
		return false
	}
	// Horizontal rule: 3+ of the same char, only allowed chars in the line.
	if len(trimmed) >= 3 {
		if allSame(trimmed, '-') || allSame(trimmed, '*') || allSame(trimmed, '_') {
			return true
		}
	}
	// Heading: 1-6 '#' followed by space (matches markdown spec).
	if trimmed[0] == '#' {
		i := 0
		for i < len(trimmed) && i < 6 && trimmed[i] == '#' {
			i++
		}
		if i > 0 && i < len(trimmed) && trimmed[i] == ' ' {
			return true
		}
	}
	return false
}

func allSame(s string, c byte) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != c {
			return false
		}
	}
	return true
}
