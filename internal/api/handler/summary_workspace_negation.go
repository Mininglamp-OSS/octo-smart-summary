package handler

import (
	"strings"
	"unicode"
)

// hasNegatedSummaryRunIntent recognizes cancellation of the summary act, not
// negative content requirements. Keep this bounded grammar paired with the
// literal request corpus: a wider substring match revives content false positives.
func hasNegatedSummaryRunIntent(message string) bool {
	for _, clause := range summaryWorkspaceUnquotedClauses(strings.ToLower(message)) {
		// Collapse spacing within English commands without splitting their words.
		clause = strings.Join(strings.Fields(clause), " ")
		if summaryWorkspaceCancellationClause(clause) {
			return true
		}
		// Chinese users also separate an affirmative request and cancellation
		// with a space. Only inspect such suffixes after an affirmative lead;
		// arbitrary reported speech must not become a cancellation.
		if startsWithAny(clause, "总结一下", "请总结", "帮我总结", "summarize ", "please summarize ") {
			for i, r := range clause {
				if r == ' ' && summaryWorkspaceCancellationClause(clause[i+1:]) {
					return true
				}
			}
		}
	}
	return false
}

func summaryWorkspaceCancellationClause(clause string) bool {
	// A leading emoji is presentation, but the quoted-span marker is data.
	clause = strings.TrimLeftFunc(clause, func(r rune) bool {
		return r != '\uFFFC' && (unicode.IsSpace(r) || unicode.IsSymbol(r) || r == '\uFE0F' || r == '\u200D')
	})
	for {
		before := clause
		// Longest alternatives precede their prefixes (请你 before 请, etc.).
		for _, prefix := range []string{
			"这次的总结", "本周的总结", "文档总结", "the summary ", "the summary",
			"请你", "请您", "能不能", "我希望", "麻烦", "帮我", "建议", "可否", "最好",
			"但是", "不过", "请", "先", "暂时", "现在", "目前", "还是", "这次", "但",
			"总结", "周报", "日报", "月报", "but ", "please ", "for now ",
		} {
			clause = strings.TrimSpace(strings.TrimPrefix(clause, prefix))
		}
		if clause == before {
			break
		}
	}
	for _, verb := range []string{
		"不要总结", "不要生成", "别总结", "别生成", "暂不总结", "暂不生成",
		"不总结", "不生成", "无需总结", "无需生成", "不需要总结", "不需要生成",
		"不用总结", "不用生成", "不要再总结", "不要再生成",
		"do not summarize", "don't summarize", "do not generate", "don't generate", "without generating",
	} {
		if strings.HasPrefix(clause, verb) && summaryWorkspaceCancellationObject(strings.TrimSpace(strings.TrimPrefix(clause, verb))) {
			return true
		}
	}
	return false
}

func summaryWorkspaceCancellationObject(object string) bool {
	// Modifiers qualify the summary itself, not an arbitrary artifact ending
	// in similar text. "正式总结表格" must remain a content constraint.
	for _, modifier := range []string{"正式", "完整", "最终"} {
		if strings.HasPrefix(object, modifier) {
			object = strings.TrimPrefix(object, modifier)
			break
		}
	}
	switch object {
	case "", "总结", "总结了", "总结吧", "总结任务", "了", "吧",
		"这些内容", "这些文档", "所选内容", "所选文档", "内容", "文档", "全部", "这些",
		"这些内容的总结", "这些文档的总结", "所选内容的总结", "所选文档的总结",
		"summary", "a summary", "the summary", "yet", "now",
		"summary yet", "a summary yet", "the summary yet", "a summary now", "the summary now",
		"these documents", "these docs", "this content", "the selected documents", "the selected content":
		return true
	default:
		return false
	}
}

func startsWithAny(value string, prefixes ...string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

// Mask embedded quotations before splitting clauses, so punctuation INSIDE
// a quote cannot turn reported text into a standalone execution instruction.
// A wholly quoted submission is treated as the user's own instruction.
func summaryWorkspaceUnquotedClauses(message string) []string {
	runes := []rune(strings.TrimSpace(message))
	for len(runes) > 1 {
		end := summaryWorkspaceQuoteEnd(runes, 0)
		if end != len(runes)-1 {
			break
		}
		runes = []rune(strings.TrimSpace(string(runes[1:end])))
	}
	var unquoted strings.Builder
	for i := 0; i < len(runes); i++ {
		if end := summaryWorkspaceQuoteEnd(runes, i); end > i {
			unquoted.WriteRune('\uFFFC')
			i = end
		} else {
			unquoted.WriteRune(runes[i])
		}
	}
	return strings.FieldsFunc(unquoted.String(), func(r rune) bool {
		return strings.ContainsRune("，,、。.!！？?；;：:\n\r—–", r)
	})
}

func summaryWorkspaceQuoteEnd(runes []rune, start int) int {
	if start >= len(runes) {
		return -1
	}
	var close rune
	switch runes[start] {
	case '“':
		close = '”'
	case '‘':
		close = '’'
	case '「':
		close = '」'
	case '『':
		close = '』'
	case '"', '`', '\'':
		close = runes[start]
	default:
		return -1
	}
	// An apostrophe in don't is not a quote delimiter.
	if close == '\'' && start > 0 && start+1 < len(runes) &&
		runes[start-1] < unicode.MaxASCII && unicode.IsLetter(runes[start-1]) &&
		runes[start+1] < unicode.MaxASCII && unicode.IsLetter(runes[start+1]) {
		return -1
	}
	for i := start + 1; i < len(runes); i++ {
		if runes[i] == '\\' {
			i++
			continue
		}
		if runes[i] == close {
			if close == '\'' && i+1 < len(runes) &&
				runes[i-1] < unicode.MaxASCII && unicode.IsLetter(runes[i-1]) &&
				runes[i+1] < unicode.MaxASCII && unicode.IsLetter(runes[i+1]) {
				continue
			}
			return i
		}
	}
	return -1
}
