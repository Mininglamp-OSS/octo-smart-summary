package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
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
	// OnToolError is an optional hook (SS-07b) called when a tool returns an
	// error. It receives the tool name, the per-call TARGET (channel/chunk id
	// extracted from the arguments, "" for single-target tools) and the classified
	// envelope so the handler can record fatal failures against the run (→ finish
	// gate FAILED). The target is passed because fetch_channel / summarize_chunk
	// run once per channel/chunk through the worker pool: keying the fatal set on
	// the tool name alone let one chunk's success clear a different chunk's fatal
	// marker (verdict-by-scheduling). Nil-safe. runTools reports one completed
	// step as a batch: errors first, then successes, so a successful sibling call
	// for the same target deterministically wins over a same-step failure.
	OnToolError func(toolName, target string, env ToolErrorEnvelope)

	// OnToolSuccess is the counterpart, called when a tool call SUCCEEDS. It
	// receives the same (toolName, target) key so a success clears only the
	// matching target's fatal marker.
	//
	// Without it the failed marker is one-way: the model retries the same tool,
	// succeeds, produces a perfect summary, and the run is still reported FAILED
	// because nothing ever clears the flag. Growing the classifier's list of
	// transient error strings cannot fix that — the same string has been judged
	// wrong in both directions across review rounds ("invalid connection" was too
	// lenient, then too strict) — so recoverability is derived from an OBSERVED
	// fact ("that tool worked on a later call") instead of guessed from text.
	// A retry of the same target is a new tool_call with a new id but the SAME
	// target, so the target (not the call id) is what makes recovery observable.
	// Nil-safe; emitted after the step's error callbacks.
	OnToolSuccess func(toolName, target string)
}

func NewRunner(client chatter, reg *Registry, pool *Pool, policy Policy) *Runner {
	return &Runner{client: client, reg: reg, pool: pool, policy: policy}
}

// ToolSchemas exposes the tool schemas this runner will offer the model, so a
// caller can assert WHICH toolset a runner was built with. SS-08b removes the
// data-fetching tools for a confident rewrite, and that decision is only
// observable through the registry the runner ended up holding. Nil-safe.
func (r *Runner) ToolSchemas() []Tool {
	if r == nil || r.reg == nil {
		return nil
	}
	return r.reg.Schemas()
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
	// Keep Map output out of the planner transcript. Every run gets an isolated,
	// request-scoped store shared by summarize_chunk and merge_summaries through
	// the tool context. It deliberately shadows any store on the parent context so
	// a reused context cannot leak handles or pending state across runner calls.
	ctx = withSummaryHandleStore(ctx)

	userMsg := Message{Role: "user", Content: userInput}

	msgs := make([]Message, 0, len(history)+2)
	msgs = append(msgs, Message{Role: "system", Content: system})
	msgs = append(msgs, compactSummaryToolHistory(history)...)
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

		// stepCtx bounds ONLY the planning LLM call (r.client.Chat below).
		// Tool execution must NOT share this budget: LLM-backed tools such
		// as summarize_chunk and merge_summaries run their own LLM calls
		// (Map calls use bounded concurrency), each with its own LLMTimeout (default 180s,
		// see config.go). Wrapping runTools in stepCtx (default 60s) — as
		// briefly attempted in commit 4f614cc — clamps every large map-reduce
		// summary to 60s and breaks the feature's primary path (byte-verified
		// by yujiawei / lml2468 / Jerry-Xin in PR #161).
		//
		// The outer ctx passed to runTools is the request-scoped ChatStream
		// context (300s backstop, see agent_chat.go), which is the correct
		// wall-clock ceiling for tool execution. If a per-tool budget is
		// ever needed, wrap it inside the tool handler itself — do NOT
		// re-widen stepCtx here.
		stepCtx, cancel := context.WithTimeout(ctx, r.policy.StepTimeout)
		turn, err := r.client.Chat(stepCtx, msgs, r.reg.Schemas())
		cancel()
		if err != nil {
			return "", nil, err
		}
		totalTokens += turn.Tokens

		// A planner turn cut off at its token ceiling is the FINAL ANSWER being
		// unfinished (llm.go only flags content-only turns; a truncated tool-call
		// turn is rejected there). The client already appended the prose notice to
		// turn.Content, but that notice is inside model-authored text: a later step
		// can rewrite it, and history compaction can drop it. Latch the fact on the
		// run row so the finish gate discloses it structurally regardless.
		//
		// Recorded here rather than in the client so the transport stays free of
		// run/DB concerns, and recorded on EVERY truncated turn rather than only the
		// terminating one because an intermediate truncated turn still feeds its
		// partial text into the conversation the final answer is built from.
		if turn.Truncated {
			recordOutputTruncatedFromContext(ctx)
		}

		// A final answer is not valid while this request still has Map outputs that
		// have not passed through one successful Reduce covering every handle. The
		// model can otherwise skip merge_summaries (or ignore its parse error) and
		// confidently answer from partial Map text. Nudge it without persisting the
		// rejected draft; at the final step fail closed.
		if len(turn.ToolCalls) == 0 {
			store, storeErr := summaryHandleStoreFromContext(ctx)
			if storeErr == nil && (store.PendingMapFailures() > 0 || store.NeedsReduce()) {
				pendingMaps := store.PendingMapFailures()
				anonymousMaps := store.PendingAnonymousMapFailures()
				log.Printf("[agent] step %d/%d: final answer attempted with pending summary work (map_failures=%d anonymous_map_failures=%d reduce_needed=%t)",
					step+1, r.policy.MaxSteps, pendingMaps, anonymousMaps, store.NeedsReduce())
				if step >= r.policy.MaxSteps-1 {
					return "", nil, errors.New("successful Map retries and Reduce required before final answer")
				}
				instruction := "本次请求仍有未合并的 Map 结果。请先调用 merge_summaries，并在 summary_handles 中原样传入本次请求产生的全部 summary_handle；不要复制摘要正文，也不要直接输出最终答案。"
				if pendingMaps > 0 {
					instruction = "本次请求仍有失败的 summarize_chunk。请先使用原 messages_handle 重试每个失败的 Map 调用。不同 handle 不能证明覆盖同一批消息；若原 handle 已失效，本轮必须失败并由用户重新发起请求。全部 Map 成功后，再调用 merge_summaries 合并全部 summary_handle；不要直接输出最终答案。"
					if anonymousMaps > 0 {
						instruction = "本次请求仍有 summarize_chunk 参数无效或缺少 messages_handle。请修正参数，使用本次请求中正确的 messages_handle 逐一重试失败的 Map 调用；每个失败调用都需要一次后续成功。全部 Map 成功后，再调用 merge_summaries 合并全部 summary_handle；不要直接输出最终答案。"
					}
				}
				msgs = append(msgs, Message{
					Role:    "user",
					Content: instruction,
				})
				continue
			}
		}

		// SUM-158 blocker follow-up: 无工具调用 且 有 content = 模型给出最终答案，正常出口。
		// 但如果 tool_calls 空 且 content 也空/空白，不能视为正常终止：
		//   1. reasoning-style 模型(kimi-k2.6 / glm / qwen)在 fan-out 多步 tool_call 后
		//      偶尔会返回 content="" tool_calls=[]，通常表示模型"卡住"而非"想通了"。
		//   2. 走静默终止路径会：(a) SSE stream 关闭时不 emit `done` 事件 → 前端表现
		//      为 "stream closed without done"；(b) 落一条 empty assistant 到
		//      agent_message 表，下次 LoadHistory 加载 session 时带毒；(c) 上层 caller
		//      看到 (content="", nil) 无从区分真·空答案 vs bug。
		// 修法：识别 empty response → log 警告 + 注入一条 nudge user message 强制模型
		// 重新给答，只有在最后一步仍空才 return error。有 content 的正常终止路径未变。
		if len(turn.ToolCalls) == 0 && strings.TrimSpace(turn.Content) == "" {
			log.Printf("[agent] step %d/%d: LLM returned empty content and no tool_calls; nudging model to produce a final answer",
				step+1, r.policy.MaxSteps)
			if step >= r.policy.MaxSteps-1 {
				return "", nil, errors.New("LLM returned empty response with no tool_calls at final step")
			}
			// Nudge lives only on the in-memory msgs slice (not newMsgs) so the
			// poison assistant + nudge don't get persisted into session history.
			msgs = append(msgs, Message{Role: "assistant", Content: ""})
			msgs = append(msgs, Message{
				Role:    "user",
				Content: "请基于以上工具返回结果给出最终答案。",
			})
			continue
		}

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
		// Use the outer request ctx (300s ChatStream backstop) — NOT stepCtx.
		// See the stepCtx setup comment above for why: LLM-backed tools like
		// summarize_chunk run their own bounded-concurrent LLM calls that legitimately
		// exceed the 60s per-step planning budget.
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
			instruction := "已达token预算，请基于现有信息直接给出最终答案，不要再调用工具。"
			if store, storeErr := summaryHandleStoreFromContext(ctx); storeErr == nil && store.PendingMapFailures() > 0 {
				instruction = "已达token预算，但仍有失败的 summarize_chunk。下一步只重试失败的 Map；全部成功后调用 merge_summaries，Reduce 成功后再直接输出最终答案。"
			} else if storeErr == nil && store.NeedsReduce() {
				// Never contradict the Reduce gate. A direct-answer instruction here
				// makes the next planner turn skip merge_summaries, wasting one full
				// LLM call before the gate can nudge it back.
				instruction = "已达token预算，但本次请求仍有未合并的 Map 结果。下一步只调用 merge_summaries，传入全部 summary_handle；Reduce 成功后再直接输出最终答案。"
			}
			msgs = append(msgs, Message{
				Role:    "user",
				Content: instruction,
			})
		}
	}
	return "", nil, errors.New("max steps exceeded")
}

// runTools 并发分发一跳内的全部 tool_calls，各自独立 ctx，错误转结果字符串（不中断）。
// 结果写入预分配 slice 的固定索引，天然无写冲突；WaitGroup 收齐。
func (r *Runner) runTools(ctx context.Context, calls []ToolCall, step, ofSteps int) []string {
	results := make([]string, len(calls))
	hookOutcomes := make([]toolHookOutcome, len(calls))
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

			toolCtx := withSummaryToolStep(ctx, step)
			out, err := r.reg.Dispatch(toolCtx, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
			if tc.Function.Name == "summarize_chunk" {
				if store, storeErr := summaryHandleStoreFromContext(ctx); storeErr == nil {
					target := toolCallTarget(tc.Function.Arguments)
					if target == "" {
						target = anonymousMapFailurePrefix + tc.ID
					}
					if err != nil {
						store.MarkMapFailed(target, step)
					} else {
						store.MarkMapSucceeded(target, step)
					}
				}
			}

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
				// SS-07b: structured error envelope when V2 is on so the planner
				// can tell retryable from fatal (defect #5); off → the exact legacy
				// "错误: <text>" string, byte-identical. Fatal failures are surfaced
				// to the finish gate via OnToolError.
				if SummaryV2Enabled() {
					env := classifyToolError(tc.Function.Name, err)
					results[i] = env.JSON()
					hookOutcomes[i] = toolHookOutcome{
						toolName: tc.Function.Name,
						target:   toolCallTarget(tc.Function.Arguments),
						err:      &env,
					}
				} else {
					results[i] = "错误: " + err.Error()
				}
				return
			}
			results[i] = out
			// SS-07b: a successful call is the evidence that whatever failed earlier on
			// this tool+target was recoverable. Reported so the run's fatal marker can
			// be cleared — see Runner.OnToolSuccess for why this is an observation
			// rather than another error-string pattern.
			if SummaryV2Enabled() {
				hookOutcomes[i] = toolHookOutcome{
					toolName: tc.Function.Name,
					target:   toolCallTarget(tc.Function.Arguments),
					success:  true,
				}
			}
		})
	}
	wg.Wait()
	r.reportToolHookOutcomes(hookOutcomes)
	return results
}

type toolHookOutcome struct {
	toolName string
	target   string
	err      *ToolErrorEnvelope
	success  bool
}

// reportToolHookOutcomes makes the fatal-marker verdict independent of worker
// completion order. All failures in one planner step are observed first, then
// all successes. A same-step success for the same (tool, target) therefore
// proves that the requested work completed despite a duplicate sibling call,
// while failures for different targets remain latched.
func (r *Runner) reportToolHookOutcomes(outcomes []toolHookOutcome) {
	if r.OnToolError != nil {
		for _, outcome := range outcomes {
			if outcome.err != nil {
				r.OnToolError(outcome.toolName, outcome.target, *outcome.err)
			}
		}
	}
	if r.OnToolSuccess != nil {
		for _, outcome := range outcomes {
			if outcome.success {
				r.OnToolSuccess(outcome.toolName, outcome.target)
			}
		}
	}
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
