package agent

import (
	"os"
	"strconv"
	"strings"
)

// DefaultHistoryWindow 是滑窗默认保留的"轮"数（env AGENT_HISTORY_WINDOW 覆盖）。
const DefaultHistoryWindow = 10

// HistoryWindow 读取 env AGENT_HISTORY_WINDOW；非法/<=0 时回落默认值。
func HistoryWindow() int {
	if v := os.Getenv("AGENT_HISTORY_WINDOW"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return DefaultHistoryWindow
}

// TruncateHistory 把历史（不含 system）按最近 n 轮截断。
//
// 一"轮"= 一条 user 消息及其后续所有 assistant/tool 消息，直到下一条 user。
// 关键约束：assistant(tool_calls) 与其对应的 tool 结果消息必须成对同去同留——
// 拆散会让 LLM 侧因缺失 tool_call_id 对应结果而返回协议 400。以 user 为轮边界、
// 整轮保留即天然保证成对性（一轮内的 tool_calls 与其 tool 结果都在同一轮）。
func TruncateHistory(history []Message, n int) []Message {
	if n <= 0 || len(history) == 0 {
		return history
	}
	// 从最新往回数 user 边界，找到第 n 个（含）轮的起点。
	starts := 0
	idx := 0 // 截断起点（保留 history[idx:]）
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "user" {
			starts++
			idx = i
			if starts == n {
				break
			}
		}
	}
	if starts < n {
		// 轮数不足 n，全量保留。
		return history
	}
	return history[idx:]
}

const (
	compactedMapHistoryResult    = `{"history_compacted":true,"note":"Map summary body omitted; historical handles are expired"}`
	compactedReduceHistoryResult = `{"history_compacted":true,"note":"Reduce result omitted; use the final assistant message from that turn"}`
	compactedReduceHistoryArgs   = `{"history_compacted":true}`
)

// compactSummaryToolHistory removes duplicate/expired summary payloads from
// PREVIOUS turns before they are sent back to the planner. Legacy sessions may
// contain tens of thousands of characters both in summarize_chunk tool results
// and in merge_summaries arguments. The final assistant message already carries
// the durable user-facing summary, so retaining those intermediate copies only
// recreates the post-Map latency this handle protocol is meant to remove.
//
// Tool call IDs and result messages are preserved so the OpenAI tool protocol
// remains paired. Structured/legacy error results stay intact for diagnostics.
func compactSummaryToolHistory(history []Message) []Message {
	if len(history) == 0 {
		return history
	}
	out := make([]Message, len(history))
	for i, message := range history {
		out[i] = message
		if len(message.ToolCalls) > 0 {
			out[i].ToolCalls = append([]ToolCall(nil), message.ToolCalls...)
			for j := range out[i].ToolCalls {
				if out[i].ToolCalls[j].Function.Name == "merge_summaries" {
					out[i].ToolCalls[j].Function.Arguments = compactedReduceHistoryArgs
				}
			}
		}
		if message.Role != "tool" || isHistoricalToolError(message.Content) {
			continue
		}
		switch message.Name {
		case "summarize_chunk":
			out[i].Content = compactedMapHistoryResult
		case "merge_summaries":
			out[i].Content = compactedReduceHistoryResult
		}
	}
	return out
}

func isHistoricalToolError(content string) bool {
	trimmed := strings.TrimSpace(content)
	return strings.HasPrefix(trimmed, "错误:") || strings.Contains(trimmed, `"ok":false`)
}
