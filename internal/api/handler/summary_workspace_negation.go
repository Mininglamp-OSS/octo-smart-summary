package handler

import (
	"strings"
	"unicode"
)

type summaryWorkspaceNegation int

const (
	summaryWorkspaceNoNegation summaryWorkspaceNegation = iota
	summaryWorkspaceCancelRun
	summaryWorkspaceUncertainNegation
)

// Both cancellation and uncertain negative intent require clarification. An
// unknown object must never silently authorize a document fetch or task.
func hasNegatedSummaryRunIntent(message string) bool {
	return summaryWorkspaceRunNegation(message) != summaryWorkspaceNoNegation
}

func summaryWorkspaceRunNegation(message string) summaryWorkspaceNegation {
	for _, clause := range summaryWorkspaceNegationClauses(message) {
		for _, verb := range []string{
			"不要总结", "不要生成", "别总结", "别生成", "暂不总结", "暂不生成",
			"不总结", "不生成", "无需总结", "无需生成", "不需要总结", "不需要生成",
			"不用总结", "不用生成", "不要再总结", "不要再生成",
			"do not summarize", "don't summarize", "do not generate", "don't generate", "without generating",
		} {
			for from := 0; from < len(clause.text); {
				offset := strings.Index(clause.text[from:], verb)
				if offset < 0 {
					break
				}
				start := from + offset
				end := start + len(verb)
				from = end
				if clause.hasQuotedBytes(start, end) {
					continue // Reported words are data, not a cancellation verb.
				}
				if verb[0] < unicode.MaxASCII && start > 0 && isSummaryASCIIWord(clause.text[start-1]) {
					continue // English verbs must start on a word boundary.
				}
				object := clause.text[end:]
				if clause.isMeaningQuestion(start, end) {
					continue // "不要生成总结是什么意思" is not cancellation.
				}
				switch summaryWorkspaceNegationObject(object) {
				case summaryWorkspaceCancelRun:
					return summaryWorkspaceCancelRun
				case summaryWorkspaceUncertainNegation:
					return summaryWorkspaceUncertainNegation
				}
			}
		}
	}
	return summaryWorkspaceNoNegation
}

func isSummaryASCIIWord(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_'
}

func (c summaryWorkspaceNegationClause) isMeaningQuestion(verbStart, verbEnd int) bool {
	text := strings.TrimRightFunc(c.text, unicode.IsSpace)
	for _, suffix := range []string{
		"是什么意思", "是什么含义", "有什么含义", "的意思是什么",
		" mean",
	} {
		start := len(text) - len(suffix)
		if start < verbEnd || !strings.HasSuffix(text, suffix) || c.hasQuotedBytes(start, len(text)) {
			continue
		}
		if suffix == " mean" && !strings.HasPrefix(strings.TrimLeftFunc(c.text[:verbStart], unicode.IsSpace), "what does ") {
			continue
		}
		return true
	}
	return false
}

func summaryWorkspaceNegationObject(object string) summaryWorkspaceNegation {
	// Strip discourse decoration, not semantic nouns. These transforms apply
	// equally to cancellation objects and recognized content restrictions.
	object = strings.TrimSpace(object)
	for {
		before := object
		object = strings.Trim(object, " \t()（）[]【】")
		object = strings.TrimRightFunc(object, func(r rune) bool {
			return unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) ||
				strings.ContainsRune("了吧呢哦啦喔噢呀啊嘛哈呗咯", r)
		})
		if object == "please" || object == "thanks" || object == "ok" {
			object = ""
		}
		for _, tail := range []string{"谢谢", "好吗", "行吗", "可以吗", "ok", " please", " thanks"} {
			object = strings.TrimSpace(strings.TrimSuffix(object, tail))
		}
		if before == object {
			break
		}
	}
	// Match the whole content object, never a substring: "表格和总结" must
	// not become execution permission. Unknown objects deliberately clarify.
	switch object {
	case "表格", "行动项", "附件内容", "已关闭的任务", "图表", "总结表格",
		"正式总结表格", "完整总结附件",
		"table", "a table", "the table", "tables", "action items",
		"a summary table", "summary table", "charts", "a chart", "chart",
		"attachment content", "closed tasks":
		return summaryWorkspaceNoNegation
	case "", "总结", "总结任务", "summary", "a summary", "the summary":
		return summaryWorkspaceCancelRun
	default:
		return summaryWorkspaceUncertainNegation
	}
}

// text retains quoted object text, but the parallel byte mask prevents quoted
// verbs from being interpreted as commands. Empty quotes erase neither text nor
// intent. This is cancellation analysis, not affirmative-intent authorization.
type summaryWorkspaceNegationClause struct {
	text   string
	quoted []bool
}

func (c summaryWorkspaceNegationClause) hasQuotedBytes(start, end int) bool {
	for _, quoted := range c.quoted[start:end] {
		if quoted {
			return true
		}
	}
	return false
}

func summaryWorkspaceNegationClauses(message string) []summaryWorkspaceNegationClause {
	runes := normalizeSummaryWorkspaceNegationText(message)
	ends := summaryWorkspaceQuoteEnds(runes)
	start, limit := 0, len(runes)
	// An entirely quoted message is the user's own submission.
	for limit-start > 1 && ends[start] == limit-1 {
		start++
		limit--
		for start < limit && unicode.IsSpace(runes[start]) {
			start++
		}
		for start < limit && unicode.IsSpace(runes[limit-1]) {
			limit--
		}
	}
	var clauses []summaryWorkspaceNegationClause
	var text strings.Builder
	var quoted []bool
	appendRune := func(r rune, inQuote bool) {
		n, _ := text.WriteRune(r)
		for j := 0; j < n; j++ {
			quoted = append(quoted, inQuote)
		}
	}
	flush := func() {
		if text.Len() > 0 {
			clauses = append(clauses, summaryWorkspaceNegationClause{text.String(), quoted})
		}
		text.Reset()
		quoted = nil
	}
	for i := start; i < limit; i++ {
		if end := ends[i]; end > i && end < limit {
			for j := i + 1; j < end; j++ {
				appendRune(runes[j], true)
			}
			i = end
		} else if strings.ContainsRune("，,、。.!！？?；;：:\n\r—–", runes[i]) {
			flush()
		} else {
			appendRune(runes[i], false)
		}
	}
	flush()
	return clauses
}

func normalizeSummaryWorkspaceNegationText(message string) []rune {
	raw := []rune(strings.ToLower(strings.TrimSpace(strings.ReplaceAll(message, "\uFFFC", ""))))
	out := make([]rune, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		r := raw[i]
		// Smart apostrophes inside English words are not quote boundaries.
		if r == '’' && i > 0 && i+1 < len(raw) && isSummaryLatinLetter(raw[i-1]) && isSummaryLatinLetter(raw[i+1]) {
			r = '\''
		}
		if unicode.IsSpace(r) && r != '\n' && r != '\r' {
			j := i + 1
			for j < len(raw) && unicode.IsSpace(raw[j]) && raw[j] != '\n' && raw[j] != '\r' {
				j++
			}
			// CJK glyph spacing is presentation; English word spacing is syntax.
			if len(out) == 0 || j == len(raw) || !unicode.Is(unicode.Han, out[len(out)-1]) || !unicode.Is(unicode.Han, raw[j]) {
				out = append(out, ' ')
			}
			i = j - 1
			continue
		}
		out = append(out, r)
	}
	return out
}

func isSummaryLatinLetter(r rune) bool {
	return r >= 'a' && r <= 'z'
}

// Pair quotes with a stack so an unmatched outer opener cannot steal the
// closer of a nested quoted object. Indexing and clause construction are O(n).
func summaryWorkspaceQuoteEnds(runes []rune) []int {
	ends := make([]int, len(runes))
	for i := range ends {
		ends[i] = -1
	}
	var stack []int
	for i := 0; i < len(runes); i++ {
		if runes[i] == '\\' {
			i++
			continue
		}
		if runes[i] == '\'' && i > 0 && i+1 < len(runes) &&
			isSummaryLatinLetter(runes[i-1]) && isSummaryLatinLetter(runes[i+1]) {
			continue
		}
		if len(stack) > 0 && summaryWorkspaceClosingQuote(runes[stack[len(stack)-1]]) == runes[i] {
			ends[stack[len(stack)-1]] = i
			stack = stack[:len(stack)-1]
		} else if summaryWorkspaceClosingQuote(runes[i]) != 0 {
			stack = append(stack, i)
		}
	}
	return ends
}

func summaryWorkspaceClosingQuote(open rune) rune {
	switch open {
	case '“':
		return '”'
	case '‘':
		return '’'
	case '「':
		return '」'
	case '『':
		return '』'
	case '"', '`', '\'':
		return open
	default:
		return 0
	}
}
