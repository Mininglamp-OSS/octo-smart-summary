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
	// Quoted spans are tracked structurally, never by a user-typable sentinel.
	clause = strings.TrimLeftFunc(clause, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsSymbol(r) || unicode.IsPunct(r) || r == '\uFE0F' || r == '\u200D'
	})
	for {
		before := clause
		// Longest alternatives precede their prefixes (请你 before 请, etc.).
		for _, prefix := range []string{
			"这次的总结", "本周的总结", "文档总结", "the summary ", "the summary",
			"请你", "请您", "能不能", "我希望", "麻烦", "帮我", "建议", "可否", "最好",
			"劳驾", "拜托", "烦请", "眼下", "我的意思是", "结论是", "所以",
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
	// Normalize discourse tails before classifying the object. Do not invert
	// this into an artifact denylist: unknown content constraints must not
	// silently become cancellation.
	for {
		before := object
		if object == "please" || object == "thanks" || object == "ok" {
			object = ""
		}
		object = strings.TrimRightFunc(object, func(r rune) bool {
			return unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) ||
				strings.ContainsRune("了吧呢哦啦喔噢呀啊嘛哈呗咯", r)
		})
		for _, tail := range []string{"谢谢", "好吗", "行吗", "可以吗", "ok", " please", " thanks"} {
			object = strings.TrimSpace(strings.TrimSuffix(object, tail))
		}
		if object == before {
			break
		}
	}
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
		"这些内容", "这些文档", "所选内容", "所选文档", "内容", "文档", "全部", "这些", "全部内容", "全部文档",
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

// Exclude clauses containing quoted data from cancellation detection, so
// punctuation INSIDE a quote cannot turn reported text into cancellation.
// This does not change the separate affirmative-intent classifier.
// A wholly quoted submission is treated as the user's own instruction.
func summaryWorkspaceUnquotedClauses(message string) []string {
	// Rich-text object placeholders are presentation, not internal parser state.
	runes := []rune(strings.TrimSpace(strings.ReplaceAll(message, "\uFFFC", "")))
	ends := summaryWorkspaceQuoteEnds(runes)
	start, limit := 0, len(runes)
	for limit-start > 1 {
		if ends[start] != limit-1 {
			break
		}
		start++
		limit--
		for start < limit && unicode.IsSpace(runes[start]) {
			start++
		}
		for start < limit && unicode.IsSpace(runes[limit-1]) {
			limit--
		}
	}
	var clauses []string
	var clause strings.Builder
	quoted := false
	flush := func() {
		if !quoted && clause.Len() > 0 {
			clauses = append(clauses, clause.String())
		}
		clause.Reset()
		quoted = false
	}
	for i := start; i < limit; i++ {
		if end := ends[i]; end > i && end < limit {
			quoted = true
			i = end
		} else if strings.ContainsRune("，,、。.!！？?；;：:\n\r—–", runes[i]) {
			flush()
		} else {
			clause.WriteRune(runes[i])
		}
	}
	flush()
	return clauses
}

// Precompute nearest closing quotes in one reverse pass. Unlike searching the
// remaining input at each opener, even 8192 unmatched CJK openers take O(n).
func summaryWorkspaceQuoteEnds(runes []rune) []int {
	escaped := make([]bool, len(runes))
	for i := 0; i < len(runes); i++ {
		if runes[i] == '\\' && i+1 < len(runes) {
			i++
			escaped[i] = true
		}
	}
	ends := make([]int, len(runes))
	next := make(map[rune]int, 10)
	for i := len(runes) - 1; i >= 0; i-- {
		ends[i] = -1
		if escaped[i] {
			continue
		}
		if runes[i] == '\'' && i > 0 && i+1 < len(runes) &&
			runes[i-1] < unicode.MaxASCII && unicode.IsLetter(runes[i-1]) &&
			runes[i+1] < unicode.MaxASCII && unicode.IsLetter(runes[i+1]) {
			continue // apostrophe in don't
		}
		if close := summaryWorkspaceClosingQuote(runes[i]); close != 0 {
			if end, ok := next[close]; ok {
				ends[i] = end
			}
		}
		switch runes[i] {
		case '”', '’', '」', '』', '"', '`', '\'':
			next[runes[i]] = i
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
