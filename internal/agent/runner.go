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
	// TerminalTool makes successful completion require one registered terminal
	// tool call. Empty preserves the legacy free-text completion behavior.
	TerminalTool string
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
	result, _, err := r.RunWithHistoryOutcome(ctx, system, nil, userInput)
	return result.Reply, err
}

// RunWithHistory 在给定历史之上驱动多轮"思考→调工具→回喂"回环，直到模型收敛或触顶。
// 起始上下文 = [system] + history + [user]；system/history 由调用方拼好（滑窗在上层做）。
// 返回最终回复 + 本回合新产生的消息（user + assistant(含 tool_calls) + tool），供上层落库；
// 新消息不含 system，也不含传入的 history。
func (r *Runner) RunWithHistory(ctx context.Context, system string, history []Message, userInput string) (string, []Message, error) {
	result, newMsgs, err := r.RunWithHistoryOutcome(ctx, system, history, userInput)
	return result.Reply, newMsgs, err
}

// RunWithHistoryOutcome is the structured completion API. Legacy profiles
// return RunResult{Reply: ...}; profiles with Policy.TerminalTool also return
// the validated terminal payload without conflating it with the visible reply.
func (r *Runner) RunWithHistoryOutcome(ctx context.Context, system string, history []Message, userInput string) (RunResult, []Message, error) {
	// Keep Map output out of the planner transcript. Every run gets an isolated,
	// request-scoped store shared by summarize_chunk and merge_summaries through
	// the tool context. It deliberately shadows any store on the parent context so
	// a reused context cannot leak handles or pending state across runner calls.
	ctx = withSummaryHandleStore(ctx)
	ctx, truncationTracker := withOutputTruncationTracker(ctx)

	// A replay can see an already-persisted answer from an earlier attempt in
	// session history. If that answer belongs to this same run and was truncated,
	// conservatively carry the taint forward: the planner may reuse its partial
	// text. If the earlier attempt never persisted a message, there is nothing to
	// inherit and a clean replay remains clean.
	runID, _ := ctx.Value(ContextKeyRunID).(string)
	if runID != "" {
		for i := range history {
			if history[i].RunID == runID && history[i].OutputTruncated {
				truncationTracker.mark()
				break
			}
		}
	}

	userMsg := Message{Role: "user", Content: userInput}

	msgs := make([]Message, 0, len(history)+2)
	msgs = append(msgs, Message{Role: "system", Content: system})
	// Older deployments persisted terminal tool calls, including the full draft
	// payload in their arguments. Never feed those arguments back to the model.
	// New turns avoid persisting them below; this filter makes the migration safe
	// for already-written history as well.
	historyForPlanner := stripTerminalToolHistory(history, r.policy.TerminalTool)
	if r.policy.TerminalTool != "" {
		historyForPlanner = sanitizeToolProtocolHistory(historyForPlanner)
	}
	msgs = append(msgs, compactSummaryToolHistory(historyForPlanner)...)
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
		// Measure the planner turn and the size of what it was handed. The
		// message list grows by every tool result, so a late hop can spend
		// more wall clock re-reading accumulated output than the tools took
		// to produce it; without both numbers that cost is invisible. The
		// measurement itself is skipped when tracing is off.
		trace := TraceFromContext(ctx)
		var promptChars, promptMsgs int
		if trace.Active() {
			promptChars, promptMsgs = measurePrompt(msgs)
		}
		planStart := time.Now()
		turn, err := r.client.Chat(stepCtx, msgs, r.reg.Schemas())
		planMs := time.Since(planStart).Milliseconds()
		cancel()
		trace.AddStep(step+1, planMs, promptChars, promptMsgs, turnCompletionTokens(turn, err))
		if err != nil {
			return RunResult{}, nil, err
		}
		totalTokens += turn.Tokens

		terminalOnly := len(turn.ToolCalls) == 1 &&
			r.policy.TerminalTool != "" &&
			turn.ToolCalls[0].Function.Name == r.policy.TerminalTool &&
			r.reg.IsTerminal(r.policy.TerminalTool)

		// A final answer is not valid while this request still has Map outputs that
		// have not passed through one successful Reduce covering every handle. The
		// model can otherwise skip merge_summaries (or ignore its parse error) and
		// confidently answer from partial Map text. Nudge it without persisting the
		// rejected draft; at the final step fail closed.
		if len(turn.ToolCalls) == 0 || terminalOnly {
			store, storeErr := summaryHandleStoreFromContext(ctx)
			if storeErr == nil && (store.PendingMapFailures() > 0 || store.NeedsReduce()) {
				pendingMaps := store.PendingMapFailures()
				anonymousMaps := store.PendingAnonymousMapFailures()
				log.Printf("[agent] step %d/%d: final answer attempted with pending summary work (map_failures=%d anonymous_map_failures=%d reduce_needed=%t)",
					step+1, r.policy.MaxSteps, pendingMaps, anonymousMaps, store.NeedsReduce())
				if step >= r.policy.MaxSteps-1 {
					return RunResult{}, nil, errors.New("successful Map retries and Reduce required before final answer")
				}
				instruction := "本次请求仍有未合并的 Map 结果。请先调用 merge_summaries，并在 summary_handles 中原样传入本次请求产生的全部 summary_handle；不要复制摘要正文，也不要直接输出最终答案。"
				if pendingMaps > 0 {
					instruction = "本次请求仍有失败的 summarize_chunk。请先使用原 messages_handle 重试每个失败的 Map 调用。不同 handle 不能证明覆盖同一批消息；若原 handle 已失效，本轮必须失败并由用户重新发起请求。全部 Map 成功后，再调用 merge_summaries 合并全部 summary_handle；不要直接输出最终答案。"
					if anonymousMaps > 0 {
						instruction = "本次请求仍有 summarize_chunk 参数无效或缺少 messages_handle。请修正参数，使用本次请求中正确的 messages_handle 逐一重试失败的 Map 调用；每个失败调用都需要一次后续成功。全部 Map 成功后，再调用 merge_summaries 合并全部 summary_handle；不要直接输出最终答案。"
					}
				}
				// A terminal call still needs an assistant/tool pair in the transient
				// transcript, even though the completeness gate rejected it.
				// The handler is deliberately not invoked: a pending Map/Reduce is an
				// earlier, authoritative gate. Failed terminal attempts are never durable:
				// their arguments may contain the complete draft payload.
				if terminalOnly {
					tc := turn.ToolCalls[0]
					assistantMsg := Message{Role: "assistant", Content: turn.Content, ToolCalls: turn.ToolCalls}
					msgs = append(msgs, assistantMsg)
					toolStart := time.Now()
					r.emitToolEvent("tool_start", tc.Function.Name, step+1, r.policy.MaxSteps, 0)
					result := "错误: 本次请求仍有未完成的 Map/Reduce，emit_summary_response 未被接受。"
					elapsed := time.Since(toolStart).Milliseconds()
					if trace.Active() {
						trace.AddTool(tc.Function.Name, elapsed)
						trace.CloseStep(step+1, elapsed, []string{tc.Function.Name})
					}
					r.emitToolEvent("tool_end", tc.Function.Name, step+1, r.policy.MaxSteps, elapsed)
					toolMsg := Message{Role: "tool", ToolCallID: tc.ID, Name: tc.Function.Name, Content: result}
					msgs = append(msgs, toolMsg)
					r.emitStepEnd(stepStart, step+1, true)
				}
				msgs = append(msgs, Message{
					Role:    "user",
					Content: instruction,
				})
				continue
			}
		}

		// A terminal-policy profile cannot finish with model-authored free text.
		// Reject it in-memory and ask for the registered terminal tool; neither the
		// rejected content nor this runtime nudge is persisted.
		if len(turn.ToolCalls) == 0 && r.policy.TerminalTool != "" {
			log.Printf("[agent] step %d/%d: free-text final rejected; terminal tool %s is required",
				step+1, r.policy.MaxSteps, r.policy.TerminalTool)
			if step >= r.policy.MaxSteps-1 {
				return RunResult{}, nil, errors.New("terminal tool required before final answer")
			}
			msgs = append(msgs, Message{
				Role:    "user",
				Content: "请仅通过 " + r.policy.TerminalTool + " 工具提交本轮最终结果，不要直接输出文本；该工具必须是本轮唯一的工具调用。",
			})
			continue
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
				return RunResult{}, nil, errors.New("LLM returned empty response with no tool_calls at final step")
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
			// A planner turn cut off at its token ceiling is recorded only once it
			// has passed every acceptance gate and will actually be delivered. A
			// premature answer rejected above is discarded rather than fed back into
			// the conversation, so latching it would falsely mark a later complete
			// Reduce/final answer as partial. The client has already appended the
			// inline notice; this persisted fact is the model-proof disclosure channel.
			if turn.Truncated {
				recordOutputTruncatedFromContext(ctx)
			}
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
			finalMsg := Message{
				Role:            "assistant",
				Content:         turn.Content,
				RunID:           runID,
				OutputTruncated: runID != "" && truncationTracker.value(),
			}
			newMsgs = append(newMsgs, finalMsg)
			if runID != "" {
				for i := range newMsgs {
					newMsgs[i].RunID = runID
				}
			}
			return RunResult{Reply: turn.Content}, newMsgs, nil
		}

		if terminalOnly {
			tc := turn.ToolCalls[0]
			assistantMsg := Message{Role: "assistant", Content: turn.Content, ToolCalls: turn.ToolCalls}
			msgs = append(msgs, assistantMsg)

			toolStart := time.Now()
			r.emitToolEvent("tool_start", tc.Function.Name, step+1, r.policy.MaxSteps, 0)
			outcome, terminalErr := r.reg.DispatchTerminal(ctx, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
			toolElapsed := time.Since(toolStart).Milliseconds()
			if trace.Active() {
				trace.AddTool(tc.Function.Name, toolElapsed)
				trace.CloseStep(step+1, toolElapsed, []string{tc.Function.Name})
			}
			r.emitToolEvent("tool_end", tc.Function.Name, step+1, r.policy.MaxSteps, toolElapsed)

			toolContent := terminalOutcomeToolResult(outcome)
			if terminalErr != nil {
				toolContent = "错误: " + terminalErr.Error()
			}
			toolMsg := Message{Role: "tool", ToolCallID: tc.ID, Name: tc.Function.Name, Content: toolContent}
			msgs = append(msgs, toolMsg)
			r.emitStepEnd(stepStart, step+1, true)

			if terminalErr != nil {
				if step >= r.policy.MaxSteps-1 {
					return RunResult{}, nil, errors.New("terminal tool failed at final step: " + terminalErr.Error())
				}
				continue
			}

			finalMsg := Message{
				Role:            "assistant",
				Content:         outcome.VisibleContent,
				RunID:           runID,
				OutputTruncated: runID != "" && truncationTracker.value(),
			}
			newMsgs = append(newMsgs, finalMsg)
			if runID != "" {
				for i := range newMsgs {
					newMsgs[i].RunID = runID
				}
			}
			return RunResult{Reply: outcome.VisibleContent, Terminal: &outcome}, newMsgs, nil
		}

		// 回喂 assistant 轮次（必须携带原始 tool_calls，否则下游 tool 消息无处挂靠）。
		assistantMsg := Message{
			Role:      "assistant",
			Content:   turn.Content,
			ToolCalls: turn.ToolCalls,
		}
		msgs = append(msgs, assistantMsg)
		if durableAssistant, ok := withoutTerminalToolCalls(assistantMsg, r.policy.TerminalTool); ok {
			newMsgs = append(newMsgs, durableAssistant)
		}

		// 单跳内多工具并发执行；结果按原索引回填以保证顺序稳定、无数据竞争。
		// Use the outer request ctx (300s ChatStream backstop) — NOT stepCtx.
		// See the stepCtx setup comment above for why: LLM-backed tools like
		// summarize_chunk run their own bounded-concurrent LLM calls that legitimately
		// exceed the 60s per-step planning budget.
		toolsStart := time.Now()
		results := r.runTools(ctx, turn.ToolCalls, step+1, r.policy.MaxSteps)
		if trace.Active() {
			toolNames := make([]string, 0, len(turn.ToolCalls))
			for _, tc := range turn.ToolCalls {
				if r.reg.Has(tc.Function.Name) {
					toolNames = append(toolNames, tc.Function.Name)
				}
			}
			trace.CloseStep(step+1, time.Since(toolsStart).Milliseconds(), toolNames)
		}
		for i, tc := range turn.ToolCalls {
			toolMsg := Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    results[i],
			}
			msgs = append(msgs, toolMsg)
			if !r.reg.IsTerminal(tc.Function.Name) {
				newMsgs = append(newMsgs, toolMsg)
			}
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
			} else if r.policy.TerminalTool != "" {
				instruction = "已达token预算，请基于现有信息仅调用 " + r.policy.TerminalTool + " 提交最终结果；不要直接输出文本，也不要再调用其他工具。"
			}
			msgs = append(msgs, Message{
				Role:    "user",
				Content: instruction,
			})
		}
	}
	return RunResult{}, nil, errors.New("max steps exceeded")
}

func (r *Runner) emitToolEvent(eventType, toolName string, step, ofSteps int, elapsedMs int64) {
	if r.OnEvent == nil {
		return
	}
	r.OnEvent(Event{
		Type:      eventType,
		Tool:      toolName,
		Step:      step,
		OfSteps:   ofSteps,
		ElapsedMs: elapsedMs,
	})
}

func (r *Runner) emitStepEnd(stepStart time.Time, step int, hasTools bool) {
	if r.OnEvent == nil {
		return
	}
	r.OnEvent(Event{
		Type:         "step_end",
		Step:         step,
		OfSteps:      r.policy.MaxSteps,
		ElapsedMs:    time.Since(stepStart).Milliseconds(),
		StepHasTools: hasTools,
	})
}

func terminalOutcomeToolResult(outcome TerminalOutcome) string {
	data, err := json.Marshal(map[string]interface{}{
		"accepted":    true,
		"result_type": outcome.ResultType,
	})
	if err != nil {
		return `{"accepted":true}`
	}
	return string(data)
}

// runTools 分发一跳内的全部 tool_calls，各自独立 ctx，错误转结果字符串（不中断）。
//
// Most turns keep full fan-out concurrency. The one causality-sensitive shape
// is fetch_channel + summarize_chunk in the SAME turn: fetch_channel records
// coverage and persists evidence only at the end, while summarize_chunk can
// freeze the run's citation manifest. Running them concurrently lets the gate
// read a stale coverage signature and freeze without the in-flight channel.
// Therefore all fetch_channel calls form phase 1 and must finish before the
// remaining calls start. Results still occupy their original indexes.
func (r *Runner) runTools(ctx context.Context, calls []ToolCall, step, ofSteps int) []string {
	results := make([]string, len(calls))
	hookOutcomes := make([]toolHookOutcome, len(calls))
	// Every tool call chosen by one planner turn shares the same immutable step
	// metadata. The pre-freeze coverage gate uses this to make one decision for
	// the whole fan-out, regardless of worker width or completion order.
	//
	// withSummaryToolStep (#208) carries the same step for Map failure/success
	// bookkeeping. Both are loop-invariant, so they are composed once here rather
	// than rebuilt per worker.
	toolCtx := withCoverageGateStep(withSummaryToolStep(ctx, step), step, ofSteps)
	all := make([]int, 0, len(calls))
	fetches := make([]int, 0, len(calls))
	rest := make([]int, 0, len(calls))
	discoveries := make([]int, 0, len(calls))
	scopeSetters := make([]int, 0, 1)
	afterScope := make([]int, 0, len(calls))
	hasSummarize := false
	for i, tc := range calls {
		all = append(all, i)
		phase := r.reg.Phase(tc.Function.Name)
		if phase == ToolPhaseFetch {
			fetches = append(fetches, i)
		} else {
			rest = append(rest, i)
		}
		if phase == ToolPhaseSummarize {
			hasSummarize = true
		}
		switch phase {
		case ToolPhaseScopePreparation:
			discoveries = append(discoveries, i)
		case ToolPhaseScopeCommit:
			scopeSetters = append(scopeSetters, i)
		default:
			afterScope = append(afterScope, i)
		}
	}
	// Hook outcomes are reported once, after every phase has finished, so the
	// fatal-marker verdict stays independent of both worker completion order AND
	// phase boundaries. Reporting per phase would let a phase-1 failure latch
	// before a phase-2 success for the same (tool, target) could clear it.
	defer r.reportToolHookOutcomes(hookOutcomes)
	if len(scopeSetters) > 0 {
		if len(discoveries) > 0 {
			r.runToolBatch(toolCtx, calls, discoveries, results, hookOutcomes, step, ofSteps)
		}
		// A successful scope declaration is final for the turn. Execute setters
		// serially in model order so a duplicate deterministically receives the
		// repairable "already declared" tool error instead of racing for the lock.
		for _, index := range scopeSetters {
			r.runToolBatch(toolCtx, calls, []int{index}, results, hookOutcomes, step, ofSteps)
		}
		remainingFetches := make([]int, 0, len(afterScope))
		remainingRest := make([]int, 0, len(afterScope))
		remainingHasSummarize := false
		for _, index := range afterScope {
			phase := r.reg.Phase(calls[index].Function.Name)
			if phase == ToolPhaseFetch {
				remainingFetches = append(remainingFetches, index)
			} else {
				remainingRest = append(remainingRest, index)
			}
			if phase == ToolPhaseSummarize {
				remainingHasSummarize = true
			}
		}
		if len(remainingFetches) > 0 && remainingHasSummarize {
			r.runToolBatch(toolCtx, calls, remainingFetches, results, hookOutcomes, step, ofSteps)
			r.runToolBatch(toolCtx, calls, remainingRest, results, hookOutcomes, step, ofSteps)
		} else if len(afterScope) > 0 {
			r.runToolBatch(toolCtx, calls, afterScope, results, hookOutcomes, step, ofSteps)
		}
		return results
	}
	if len(fetches) > 0 && hasSummarize {
		r.runToolBatch(toolCtx, calls, fetches, results, hookOutcomes, step, ofSteps)
		r.runToolBatch(toolCtx, calls, rest, results, hookOutcomes, step, ofSteps)
		return results
	}
	r.runToolBatch(toolCtx, calls, all, results, hookOutcomes, step, ofSteps)
	return results
}

// runToolBatch executes one causally-independent phase concurrently.
func (r *Runner) runToolBatch(toolCtx context.Context, calls []ToolCall, indexes []int, results []string, hookOutcomes []toolHookOutcome, step, ofSteps int) {
	var wg sync.WaitGroup
	for _, i := range indexes {
		wg.Add(1)
		i, tc := i, calls[i]
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

			// A terminal tool is accepted only when it is the sole call in the
			// planner turn. The unique-call path is handled by
			// RunWithHistoryOutcome before runTools; reaching here therefore means
			// it was mixed with another call (or duplicated). Do not invoke the
			// terminal handler, but still return a paired tool result so the next
			// planner turn can repair the protocol.
			terminalRejected := r.reg.IsTerminal(tc.Function.Name)
			var out string
			var err error
			if terminalRejected {
				err = errors.New("terminal tool must be the only tool call in its step")
			} else {
				out, err = r.reg.Dispatch(toolCtx, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
			}
			if tc.Function.Name == "summarize_chunk" {
				// toolCtx, not the caller's ctx: the store lives on the request-scoped
				// context that toolCtx wraps, and using it keeps every lookup on this
				// path reading the same context chain.
				if store, storeErr := summaryHandleStoreFromContext(toolCtx); storeErr == nil {
					target := toolCallTarget(tc.Function.Arguments)
					if target == "" {
						target = anonymousMapFailurePrefix + tc.ID
					}
					if err != nil {
						// The coverage gate rejects the call before Map reads or
						// summarizes any messages. Recording that precondition as a
						// failed Map would require a later success on the exact same
						// messages_handle, even when the repair fetch legitimately
						// gives the planner new handles. That can strand Reduce and the
						// final-answer gate at the step ceiling.
						var gateErr *CoverageGateError
						if !errors.As(err, &gateErr) {
							store.MarkMapFailed(target, step)
						}
					} else {
						store.MarkMapSucceeded(target, step)
					}
				}
			}

			toolElapsed := time.Since(toolStart).Milliseconds()
			// Tools in a hop run concurrently, so the hop's wall clock is set
			// by the slowest one; record each so the straggler can be named.
			if trace := TraceFromContext(toolCtx); trace.Active() && r.reg.Has(tc.Function.Name) {
				trace.AddTool(tc.Function.Name, toolElapsed)
			}

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
				if terminalRejected {
					results[i] = "错误: " + err.Error()
					return
				}
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

// turnCompletionTokens reports a turn's completion tokens, tolerating the error path
// (a failed turn has no usable token count, but its latency still matters and
// the step is still recorded).
func turnCompletionTokens(turn AssistantTurn, err error) int {
	if err != nil {
		return 0
	}
	return turn.CompletionTokens
}
