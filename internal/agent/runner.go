package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// chatter 是 Runner 对 LLM 的最小依赖抽象，*Client 实现它；测试可注入 fake。
type chatter interface {
	Chat(ctx context.Context, msgs []Message, tools []Tool) (AssistantTurn, error)
}

type Policy struct {
	MaxSteps    int
	MaxTokens   int
	StepTimeout time.Duration
}

// Event represents a progress event during runner execution.
// Used for SSE streaming to provide real-time progress updates.
type Event struct {
	Type         string // "step_start" | "tool_start" | "tool_end" | "step_end"
	Step         int    // Current step number (1-indexed)
	OfSteps      int    // Total max steps
	Tool         string // Tool name (internal only: used to derive the abstract phase; NOT sent over SSE)
	ElapsedMs    int64  // Elapsed time in milliseconds for this step/tool
	Count        int    // Optional safe integer count (e.g. messages processed); 0 = omit. Replaces the old free-text Detail so no tool/channel-identifying strings leak.
	StepHasTools bool   // Whether this step has tool calls (set by runner main loop)
}

type Runner struct {
	client  chatter
	reg     *Registry
	pool    *Pool
	policy  Policy
	OnEvent func(Event) // Optional callback for progress events; nil-safe
}

func NewRunner(client chatter, reg *Registry, pool *Pool, policy Policy) *Runner {
	return &Runner{client: client, reg: reg, pool: pool, policy: policy}
}

// Run 无状态单轮入口：委托 RunWithHistory（history=nil），保持旧签名零回归。
func (r *Runner) Run(ctx context.Context, system, userInput string) (string, error) {
	reply, _, err := r.RunWithHistory(ctx, system, nil, userInput)
	return reply, err
}

// RunWithHistory 在给定历史之上驱动多轮"思考→调工具→回喂"回环，直到模型收敛或触顶。
// 起始上下文 = [system] + history + [user]；system/history 由调用方拼好（滑窗在上层做）。
// 返回最终回复 + 本回合新产生的消息（user + assistant(含 tool_calls) + tool），供上层落库；
// 新消息不含 system，也不含传入的 history。
func (r *Runner) RunWithHistory(ctx context.Context, system string, history []Message, userInput string) (string, []Message, error) {
	userMsg := Message{Role: "user", Content: userInput}

	msgs := make([]Message, 0, len(history)+2)
	msgs = append(msgs, Message{Role: "system", Content: system})
	msgs = append(msgs, history...)
	msgs = append(msgs, userMsg)

	// newMsgs 只累积本回合新增（user + 各 assistant + 各 tool），供落库；不含 system/history。
	newMsgs := []Message{userMsg}
	totalTokens := 0

	for step := 0; step < r.policy.MaxSteps; step++ {
		stepStart := time.Now()

		// Emit step_start event
		if r.OnEvent != nil {
			r.OnEvent(Event{
				Type:    "step_start",
				Step:    step + 1,
				OfSteps: r.policy.MaxSteps,
			})
		}

		stepCtx, cancel := context.WithTimeout(ctx, r.policy.StepTimeout)
		turn, err := r.client.Chat(stepCtx, msgs, r.reg.Schemas())
		cancel()
		if err != nil {
			return "", nil, err
		}
		totalTokens += turn.Tokens

		// 无工具调用 = 模型给出最终答案，正常出口：把 assistant 终答并入 newMsgs 落库。
		if len(turn.ToolCalls) == 0 {
			stepElapsed := time.Since(stepStart).Milliseconds()
			if r.OnEvent != nil {
				r.OnEvent(Event{
					Type:         "step_end",
					Step:         step + 1,
					OfSteps:      r.policy.MaxSteps,
					ElapsedMs:    stepElapsed,
					StepHasTools: false, // No tool calls - final answer
				})
			}
			newMsgs = append(newMsgs, Message{Role: "assistant", Content: turn.Content})
			return turn.Content, newMsgs, nil
		}

		// 回喂 assistant 轮次（必须携带原始 tool_calls，否则下游 tool 消息无处挂靠）。
		assistantMsg := Message{
			Role:      "assistant",
			Content:   turn.Content,
			ToolCalls: turn.ToolCalls,
		}
		msgs = append(msgs, assistantMsg)
		newMsgs = append(newMsgs, assistantMsg)

		// 单跳内多工具并发执行；结果按原索引回填以保证顺序稳定、无数据竞争。
		results := r.runTools(ctx, turn.ToolCalls, step+1, r.policy.MaxSteps)
		for i, tc := range turn.ToolCalls {
			toolMsg := Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    results[i],
			}
			msgs = append(msgs, toolMsg)
			newMsgs = append(newMsgs, toolMsg)
		}

		stepElapsed := time.Since(stepStart).Milliseconds()
		if r.OnEvent != nil {
			r.OnEvent(Event{
				Type:         "step_end",
				Step:         step + 1,
				OfSteps:      r.policy.MaxSteps,
				ElapsedMs:    stepElapsed,
				StepHasTools: true, // Had tool calls
			})
		}

		// 预算触顶：注入收尾指令，逼模型下一轮直接给答案。
		// 这条纯运行时提示，不并入 newMsgs（不落库，避免污染历史）。
		if totalTokens >= r.policy.MaxTokens {
			msgs = append(msgs, Message{
				Role:    "user",
				Content: "已达token预算，请基于现有信息直接给出最终答案，不要再调用工具。",
			})
		}
	}
	return "", nil, errors.New("max steps exceeded")
}

// runTools 并发分发一跳内的全部 tool_calls，各自独立 ctx，错误转结果字符串（不中断）。
// 结果写入预分配 slice 的固定索引，天然无写冲突；WaitGroup 收齐。
func (r *Runner) runTools(ctx context.Context, calls []ToolCall, step, ofSteps int) []string {
	results := make([]string, len(calls))
	var wg sync.WaitGroup
	for i, tc := range calls {
		wg.Add(1)
		i, tc := i, tc
		r.pool.Submit(func() {
			defer wg.Done()

			toolStart := time.Now()
			if r.OnEvent != nil {
				r.OnEvent(Event{
					Type:    "tool_start",
					Tool:    tc.Function.Name,
					Step:    step,
					OfSteps: ofSteps,
				})
			}

			out, err := r.reg.Dispatch(ctx, tc.Function.Name, json.RawMessage(tc.Function.Arguments))

			toolElapsed := time.Since(toolStart).Milliseconds()

			// Extract a cheap, safe integer count from the tool result (0 = none).
			count := extractToolCount(tc.Function.Name, out, i, len(calls))

			if r.OnEvent != nil {
				r.OnEvent(Event{
					Type:      "tool_end",
					Tool:      tc.Function.Name,
					Step:      step,
					OfSteps:   ofSteps,
					ElapsedMs: toolElapsed,
					Count:     count,
				})
			}

			if err != nil {
				results[i] = "错误: " + err.Error()
				return
			}
			results[i] = out
		})
	}
	wg.Wait()
	return results
}

// extractToolCount extracts a cheap, safe integer count from a tool result.
// Returns 0 when there is no meaningful count (the SSE layer then omits it).
// It deliberately returns ONLY a number — never a tool/channel-identifying string —
// so the progress stream cannot leak internal tool semantics. summarize_chunk has
// no message count (its per-chunk index is intentionally not exposed), so it returns 0.
func extractToolCount(toolName, result string, idx, total int) int {
	switch toolName {
	case "fetch_channel", "search_messages":
		var data map[string]interface{}
		if err := json.Unmarshal([]byte(result), &data); err != nil {
			return 0
		}
		if messages, ok := data["messages"].([]interface{}); ok {
			return len(messages)
		}
		if t, ok := data["total"].(float64); ok {
			return int(t)
		}
		return 0

	case "filter_relevant":
		// filter_relevant returns {"filtered_count": N, ...}
		var data map[string]interface{}
		if err := json.Unmarshal([]byte(result), &data); err != nil {
			return 0
		}
		if filteredCount, ok := data["filtered_count"].(float64); ok {
			return int(filteredCount)
		}
		return 0

	case "merge_summaries":
		// merge_summaries returns {"chunk_count": N, ...}
		var data map[string]interface{}
		if err := json.Unmarshal([]byte(result), &data); err != nil {
			return 0
		}
		if chunkCount, ok := data["chunk_count"].(float64); ok {
			return int(chunkCount)
		}
		return 0
	}

	return 0
}
