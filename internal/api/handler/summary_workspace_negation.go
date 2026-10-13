package handler

import (
	"strings"
	"unicode"
	"unicode/utf8"
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
	result := summaryWorkspaceNoNegation
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
				if clause.hasReportedBytes(start, end) {
					continue // Reported words are data, not a cancellation verb.
				}
				if verb[0] < unicode.MaxASCII && start > 0 && isSummaryASCIIWord(clause.text[start-1]) {
					continue // English verbs must start on a word boundary.
				}
				object := clause.text[end:]
				if clause.isMeaningQuestion(start, end) {
					continue // "不要生成总结是什么意思" is not cancellation.
				}
				if normalizeSummaryWorkspaceNegationObject(object) == "" {
					// A known fronted object is a content restriction, not an
					// empty-object cancellation. Never allow arbitrary suffixes.
					if clause.hasFrontedContentObject(start) {
						continue
					}
				}
				switch summaryWorkspaceNegationObject(object) {
				case summaryWorkspaceCancelRun:
					return summaryWorkspaceCancelRun
				case summaryWorkspaceUncertainNegation:
					result = summaryWorkspaceUncertainNegation
				}
			}
		}
	}
	return result
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

func normalizeSummaryWorkspaceNegationObject(object string) string {
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
	return object
}

func summaryWorkspaceNegationObject(object string) summaryWorkspaceNegation {
	object = normalizeSummaryWorkspaceNegationObject(object)
	if isSummaryWorkspaceContentObject(object) {
		return summaryWorkspaceNoNegation
	}
	switch object {
	case "", "总结", "总结任务", "summary", "a summary", "the summary":
		return summaryWorkspaceCancelRun
	default:
		return summaryWorkspaceUncertainNegation
	}
}

var summaryWorkspaceContentObjects = []string{
	"表格", "行动项", "附件内容", "已关闭的任务", "图表", "总结表格",
	"正式总结表格", "完整总结附件",
	"table", "a table", "the table", "tables", "action items",
	"a summary table", "summary table", "charts", "a chart", "chart",
	"attachment content", "closed tasks",
}

var summaryWorkspaceContentMemberLimit = func() int {
	limit := 0
	for _, object := range summaryWorkspaceContentObjects {
		limit = max(limit, len(object))
	}
	return limit
}()

func isSummaryWorkspaceContentObject(object string) bool {
	// Scan one member at a time, rejecting an impossible member immediately.
	// Splitting the entire suffix for every verb match is quadratic. This
	// bounds each unknown candidate without imposing a total-list length cap.
	for {
		object = strings.TrimSpace(object)
		split, separatorLength := -1, 0
		for i, r := range object {
			for _, separator := range []string{"以及", "和", "与", "、", " and "} {
				if strings.HasPrefix(object[i:], separator) {
					split, separatorLength = i, len(separator)
					break
				}
			}
			if split >= 0 {
				break
			}
			if !unicode.IsSpace(r) && i+utf8.RuneLen(r) > summaryWorkspaceContentMemberLimit {
				return false
			}
		}
		if split < 0 {
			return isSummaryWorkspaceSingleContentObject(object)
		}
		if !isSummaryWorkspaceSingleContentObject(strings.TrimSpace(object[:split])) {
			return false
		}
		object = object[split+separatorLength:]
	}
}

func isSummaryWorkspaceSingleContentObject(object string) bool {
	for _, candidate := range summaryWorkspaceContentObjects {
		if object == candidate {
			return true
		}
	}
	return false
}

func (c summaryWorkspaceNegationClause) hasFrontedContentObject(verbStart int) bool {
	// Do not normalize a separator away and reinterpret separated text as
	// an attached content object. Whitespace removed during CJK normalization
	// remains observable through boundaries. Ambiguous forms clarify.
	if strings.HasSuffix(c.text[:verbStart], "、") {
		return false
	}
	for _, boundary := range c.boundaries {
		if boundary == verbStart {
			return false
		}
		if boundary > verbStart {
			break
		}
	}
	prefix := strings.TrimSpace(c.text[:verbStart])
	if summaryWorkspaceNegationObject(prefix) == summaryWorkspaceNoNegation {
		return true
	}
	// Only a preserved whitespace boundary after an affirmative request can
	// separate its text from a fronted object. Do not guess a noun suffix from
	// an uninterrupted string such as "未知表格" or "总结和表格".
	for _, boundary := range c.boundaries {
		if boundary >= verbStart {
			break
		}
		request := strings.TrimSpace(c.text[:boundary])
		switch request {
		case "总结这份文档", "总结一下", "请总结", "请总结文档", "帮我总结", "开始总结", "请生成总结":
			if summaryWorkspaceNegationObject(c.text[boundary:verbStart]) == summaryWorkspaceNoNegation {
				return true
			}
		}
	}
	return false
}

// Quotation and attribution are separate: quoted object text is preserved,
// but only positively attributed quoted verbs are suppressed. Unattributed
// negative instructions fail toward clarification even with outer decoration.
type summaryWorkspaceNegationClause struct {
	text       string
	quoted     []bool
	reported   []bool
	boundaries []int
}

func (c summaryWorkspaceNegationClause) hasReportedBytes(start, end int) bool {
	for _, reported := range c.reported[start:end] {
		if reported {
			return true
		}
	}
	return false
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
	runes, boundaries := normalizeSummaryWorkspaceNegationText(message)
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
	var reported []bool
	var wordBoundaries []int
	appendRune := func(index int, inQuote, attributed bool) {
		if boundaries[index] {
			wordBoundaries = append(wordBoundaries, text.Len())
		}
		r := runes[index]
		n, _ := text.WriteRune(r)
		for j := 0; j < n; j++ {
			quoted = append(quoted, inQuote)
			reported = append(reported, attributed)
		}
	}
	flush := func() {
		if text.Len() > 0 {
			clauses = append(clauses, summaryWorkspaceNegationClause{text.String(), quoted, reported, wordBoundaries})
		}
		text.Reset()
		quoted = nil
		reported = nil
		wordBoundaries = nil
	}
	quoteClosers := make([]bool, len(runes))
	quoteDepth := 0
	for i := start; i < limit; i++ {
		if quoteClosers[i] {
			quoteDepth--
			continue
		}
		if end := ends[i]; end > i && end < limit {
			if boundaries[i] {
				wordBoundaries = append(wordBoundaries, text.Len())
			}
			attributed := summaryWorkspaceQuoteIsAttributed(runes[:i])
			if attributed {
				for j := i + 1; j < end; j++ {
					appendRune(j, true, true)
				}
				i = end
			} else {
				// An unattributed wrapper does not erase attribution inside
				// nested quotes. Walk its contents without recursion.
				quoteDepth++
				quoteClosers[end] = true
			}
		} else if strings.ContainsRune("，,。.!！？?；;：:\n\r—–", runes[i]) {
			flush()
		} else {
			appendRune(i, quoteDepth > 0, false)
		}
	}
	flush()
	return clauses
}

func summaryWorkspaceQuoteIsAttributed(prefix []rune) bool {
	// Local attribution only; an affirmative request elsewhere must not
	// override a conflicting quoted cancellation. Meaning questions have
	// their own independent guard in isMeaningQuestion.
	end := len(prefix)
	for end > 0 && (unicode.IsSpace(prefix[end-1]) || strings.ContainsRune("：:", prefix[end-1])) {
		end--
	}
	// Inspect only the local marker, not a growing copy of the entire input.
	start := max(0, end-32)
	text := string(prefix[start:end])
	for _, marker := range []string{"写着", "写道", "他说", "她说", "文档说", "内容是", "原文是", "said", "says", "reads"} {
		if strings.HasSuffix(text, marker) {
			before := len(text) - len(marker)
			if marker[0] < unicode.MaxASCII && before > 0 && isSummaryASCIIWord(text[before-1]) {
				continue
			}
			return true
		}
	}
	return false
}

func normalizeSummaryWorkspaceNegationText(message string) ([]rune, []bool) {
	raw := []rune(strings.ToLower(strings.TrimSpace(strings.ReplaceAll(message, "\uFFFC", ""))))
	out := make([]rune, 0, len(raw))
	boundaries := make([]bool, 0, len(raw))
	pendingBoundary := false
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
				boundaries = append(boundaries, false)
			}
			pendingBoundary = true
			i = j - 1
			continue
		}
		out = append(out, r)
		boundaries = append(boundaries, pendingBoundary)
		pendingBoundary = false
	}
	return out, boundaries
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
