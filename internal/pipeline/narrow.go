package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

// TimeNarrowResult represents the structured output from time-range extraction.
type TimeNarrowResult struct {
	HasTimeExpr bool   `json:"has_time_expr"`
	Start       string `json:"start"`
	End         string `json:"end"`
	Reasoning   string `json:"reasoning"`
}

var extractTimeRangeTool = service.Tool{
	Type: "function",
	Function: service.ToolFunction{
		Name:        "extract_time_range",
		Description: "从用户输入的主题中提取时间表达式并转换为精确的时间范围",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"has_time_expr": map[string]interface{}{
					"type":        "boolean",
					"description": "主题中是否包含时间表达式",
				},
				"start": map[string]interface{}{
					"type":        "string",
					"description": "时间范围起始，RFC3339 格式。无时间表达式时为空字符串",
				},
				"end": map[string]interface{}{
					"type":        "string",
					"description": "时间范围结束，RFC3339 格式。无时间表达式时为空字符串",
				},
				"reasoning": map[string]interface{}{
					"type":        "string",
					"description": "一句话解释判断依据",
				},
			},
			"required": []string{"has_time_expr", "start", "end", "reasoning"},
		},
	},
}

func sanitizeTopic(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r < 32 {
			return ' '
		}
		return r
	}, s)
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return strings.TrimSpace(s)
}

// LLMToolCallFn is the callback type for function-call based LLM invocations.
type LLMToolCallFn func(ctx context.Context, messages []service.ChatMessage, tools []service.Tool, forceFn string) (string, error)

// PreRetrievalNarrow uses LLM Function Call to extract time expressions from topic
// and narrow the query window. Falls back to original range on any failure.
func PreRetrievalNarrow(ctx context.Context, topic string, originalStart, originalEnd time.Time, toolCallFn LLMToolCallFn) (time.Time, time.Time) {
	if topic == "" || toolCallFn == nil {
		return originalStart, originalEnd
	}
	if utf8.RuneCountInString(topic) > 1000 {
		runes := []rune(topic)
		topic = string(runes[:1000])
	}
	topic = sanitizeTopic(topic)

	now := time.Now()
	currentDate := now.Format("2006-01-02")
	weekdays := [...]string{"日", "一", "二", "三", "四", "五", "六"}
	weekday := weekdays[now.Weekday()]

	systemPrompt := fmt.Sprintf(`你是一个时间表达式解析器。

当前日期：%s（星期%s）
当前时间：%s

规则：
- "今天" = 当天 00:00:00 ~ 23:59:59
- "昨天" = 前一天 00:00:00 ~ 23:59:59
- "本周" = 本周一 00:00:00 ~ 当前时间
- "上周" = 上周一 00:00:00 ~ 上周日 23:59:59
- "最近N天" = N天前 00:00:00 ~ 当前时间
- "这几天" = 最近3天
- 时区统一使用 +08:00
- 如果没有任何时间表达式，has_time_expr 设为 false

你必须调用 extract_time_range 工具来返回结果，不要以文本形式回复。`, currentDate, weekday, now.Format("15:04"))

	userMsg := fmt.Sprintf(`判断以下主题中是否包含时间表达式，如果有，解析出精确的时间范围。

主题："%s"`, topic)

	messages := []service.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userMsg},
	}

	narrowStart := time.Now()
	argsJSON, err := toolCallFn(ctx, messages, []service.Tool{extractTimeRangeTool}, "extract_time_range")
	elapsed := time.Since(narrowStart).Milliseconds()

	if err != nil {
		log.Printf("[pipeline] CallWithTools: tool=extract_time_range input={topic:%q} took %dms error=%v, fallback to original range", topic, elapsed, err)
		return originalStart, originalEnd
	}

	var parsed TimeNarrowResult
	if err := json.Unmarshal([]byte(argsJSON), &parsed); err != nil {
		log.Printf("[pipeline] CallWithTools: tool=extract_time_range input={topic:%q} took %dms parse_error=%v, args=%s", topic, elapsed, err, argsJSON)
		return originalStart, originalEnd
	}

	log.Printf("[pipeline] CallWithTools: tool=extract_time_range input={topic:%q} took %dms result={has_time_expr:%v}", topic, elapsed, parsed.HasTimeExpr)

	if !parsed.HasTimeExpr {
		return originalStart, originalEnd
	}

	parsedStart, err1 := time.Parse(time.RFC3339, parsed.Start)
	parsedEnd, err2 := time.Parse(time.RFC3339, parsed.End)
	if err1 != nil || err2 != nil {
		log.Printf("[pipeline] PreRetrievalNarrow: time parse error start=%v end=%v", err1, err2)
		return originalStart, originalEnd
	}

	if parsedStart.Before(originalStart) {
		parsedStart = originalStart
	}
	if parsedEnd.After(originalEnd) {
		parsedEnd = originalEnd
	}
	if !parsedStart.Before(parsedEnd) {
		log.Printf("[pipeline] PreRetrievalNarrow: invalid range start >= end")
		return originalStart, originalEnd
	}

	log.Printf("[pipeline] PreRetrievalNarrow: narrowed [%s ~ %s] → [%s ~ %s] reason=%s",
		originalStart.Format("01-02"), originalEnd.Format("01-02"),
		parsedStart.Format("01-02 15:04"), parsedEnd.Format("01-02 15:04"),
		parsed.Reasoning)

	return parsedStart, parsedEnd
}

// PostRetrievalNarrow remains unchanged — uses LLMCallFn (not Function Call).
func PostRetrievalNarrow(ctx context.Context, messages []Message, topic string, llmFn LLMCallFn) []Message {
	return messages
}
