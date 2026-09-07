package agent

import (
	"encoding/json"
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

// Compaction placeholders. They are SCHEMA-SHAPED on purpose: the planner is
// few-shot conditioned by its own transcript, and a rewritten past call that
// violates the live tool schema teaches it to emit the same malformed call
// again. merge_summaries declares additionalProperties:false with
// required:["summary_handles"], so a bare `{"history_compacted":true}` was a
// worked example of an invalid call sitting in every compacted session — and
// every merge_summaries failure latches the run fatal until a later merge
// succeeds (tool_error.go). Same reasoning for the summarize_chunk result shape.
//
// The placeholder handle values are deliberately un-resolvable: they must not
// look like a live handle the model could try to reuse. ResolveAllBefore
// rejects them with "invalid or expired summary_handle", which is the accurate
// thing for the model to learn about a previous turn's handles.
const (
	compactedMapHistoryResult    = `{"summary_handle":"__history__","chunk_count":0,"history_compacted":true,"note":"Map summary body omitted; historical handles are expired"}`
	compactedReduceHistoryResult = `{"merged_summary":"","chunk_count":0,"history_compacted":true,"note":"Reduce result omitted; use the final assistant message from that turn"}`
	compactedReduceHistoryArgs   = `{"summary_handles":["__compacted__"]}`

	// Upper bound on merge_summaries arguments kept for a failed historical call.
	// A handle-protocol call is at most ~128 short handles; anything larger is a
	// legacy call carrying inlined summary bodies.
	maxRetainedFailedReduceArgs = 8 << 10
)

// compactSummaryToolHistory removes duplicate/expired summary payloads from
// PREVIOUS turns before they are sent back to the planner. Legacy sessions may
// contain tens of thousands of characters both in summarize_chunk tool results
// and in merge_summaries arguments. The final assistant message already carries
// the durable user-facing summary, so retaining those intermediate copies only
// recreates the post-Map latency this handle protocol is meant to remove.
//
// Tool call IDs and result messages are preserved so the OpenAI tool protocol
// remains paired. Structured/legacy error results stay intact for diagnostics,
// and so do the arguments that produced them.
func compactSummaryToolHistory(history []Message) []Message {
	if len(history) == 0 {
		return history
	}
	out := make([]Message, len(history))
	// A tool_call_id whose RESULT was retained as a diagnostic error. Its
	// arguments are retained too: an error result explains what the model did
	// wrong, and the mistake IS the arguments, so pairing the complaint with
	// invented placeholder arguments both destroys the diagnostic and leaves the
	// preserved exchange internally inconsistent. Results are visited after their
	// assistant turn in transcript order, so the rewrite is deferred to a second
	// pass rather than done inline.
	//
	// Bounded by maxRetainedFailedReduceArgs: under the handle protocol these
	// arguments are a short list of handles, but a LEGACY failed merge carried
	// the full summary bodies, and retaining those would reintroduce exactly the
	// transcript bloat this function exists to remove. Oversized arguments lose
	// the diagnostic and take the placeholder — a bad diagnostic is cheaper than
	// an unbounded prompt.
	keepArgs := make(map[string]bool)
	for _, message := range history {
		if message.Role == "tool" && isHistoricalToolError(message.Content) && message.ToolCallID != "" {
			keepArgs[message.ToolCallID] = true
		}
	}
	for i, message := range history {
		out[i] = message
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
	for i := range out {
		if len(out[i].ToolCalls) == 0 {
			continue
		}
		out[i].ToolCalls = append([]ToolCall(nil), out[i].ToolCalls...)
		for j := range out[i].ToolCalls {
			call := &out[i].ToolCalls[j]
			if call.Function.Name != "merge_summaries" {
				continue
			}
			if keepArgs[call.ID] && len(call.Function.Arguments) <= maxRetainedFailedReduceArgs {
				continue
			}
			call.Function.Arguments = compactedReduceHistoryArgs
		}
	}
	return out
}

// stripTerminalToolHistory removes terminal protocol messages before an old
// transcript is sent back to the planner. Terminal arguments contain the full
// structured draft and must not become durable conversational history. Mixed
// turns keep their ordinary tool calls/results, while terminal-only attempts
// are dropped as a unit.
func stripTerminalToolHistory(history []Message, terminalTool string) []Message {
	if terminalTool == "" || len(history) == 0 {
		return history
	}
	out := make([]Message, 0, len(history))
	var terminalIDs map[string]struct{}
	for _, message := range history {
		if message.Role == "assistant" && len(message.ToolCalls) > 0 {
			terminalIDs = make(map[string]struct{})
			for _, call := range message.ToolCalls {
				if call.Function.Name == terminalTool && call.ID != "" {
					terminalIDs[call.ID] = struct{}{}
				}
			}
			filtered, keep := withoutTerminalToolCalls(message, terminalTool)
			if keep {
				out = append(out, filtered)
			}
			continue
		}
		if message.Role == "tool" {
			_, terminalID := terminalIDs[message.ToolCallID]
			if message.Name == terminalTool || terminalID {
				continue
			}
			out = append(out, message)
			continue
		}
		terminalIDs = nil
		out = append(out, message)
	}
	return out
}

// withoutTerminalToolCalls returns a copy safe for durable history. A message
// containing only terminal calls is discarded, even if it also has text: the
// whole assistant turn is one failed/accepted terminal protocol attempt.
func withoutTerminalToolCalls(message Message, terminalTool string) (Message, bool) {
	if terminalTool == "" || len(message.ToolCalls) == 0 {
		return message, true
	}
	filtered := make([]ToolCall, 0, len(message.ToolCalls))
	for _, call := range message.ToolCalls {
		if call.Function.Name != terminalTool {
			filtered = append(filtered, call)
		}
	}
	if len(filtered) == 0 {
		return Message{}, false
	}
	removedTerminal := len(filtered) != len(message.ToolCalls)
	message.ToolCalls = filtered
	// Content belongs to the whole assistant turn. If that turn attempted the
	// terminal tool, it may duplicate the draft carried in terminal arguments;
	// retain only the ordinary tool protocol, not the accompanying draft text.
	if removedTerminal {
		message.Content = ""
	}
	return message, true
}

// sanitizeToolProtocolHistory removes a truncated prefix before the first user
// turn and drops assistant/tool groups that are no longer fully paired. This is
// the final guard before a persisted workspace transcript reaches an OpenAI-
// compatible endpoint: a database LIMIT can otherwise start on an orphan tool
// result, and filtering an old terminal call can expose an incomplete group.
func sanitizeToolProtocolHistory(history []Message) []Message {
	start := -1
	for i := range history {
		if history[i].Role == "user" {
			start = i
			break
		}
	}
	if start < 0 {
		return nil
	}

	out := make([]Message, 0, len(history)-start)
	for i := start; i < len(history); {
		message := history[i]
		if message.Role == "tool" {
			i++ // orphaned by LIMIT/filtering
			continue
		}
		if message.Role != "assistant" || len(message.ToolCalls) == 0 {
			out = append(out, message)
			i++
			continue
		}

		j := i + 1
		results := make(map[string]struct{}, len(message.ToolCalls))
		for j < len(history) && history[j].Role == "tool" {
			results[history[j].ToolCallID] = struct{}{}
			j++
		}
		complete := true
		for _, call := range message.ToolCalls {
			if call.ID == "" {
				complete = false
				break
			}
			if _, ok := results[call.ID]; !ok {
				complete = false
				break
			}
		}
		if complete {
			out = append(out, message)
			allowed := make(map[string]struct{}, len(message.ToolCalls))
			for _, call := range message.ToolCalls {
				allowed[call.ID] = struct{}{}
			}
			for k := i + 1; k < j; k++ {
				if _, ok := allowed[history[k].ToolCallID]; ok {
					out = append(out, history[k])
				}
			}
		}
		i = j
	}
	return out
}

func isHistoricalToolError(content string) bool {
	trimmed := strings.TrimSpace(content)
	if strings.HasPrefix(trimmed, "错误:") {
		return true
	}
	var envelope struct {
		OK *bool `json:"ok"`
	}
	return json.Unmarshal([]byte(trimmed), &envelope) == nil && envelope.OK != nil && !*envelope.OK
}
