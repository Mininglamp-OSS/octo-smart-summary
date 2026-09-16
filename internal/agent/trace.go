package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/metrics"
)

// Per-request latency instrumentation for the agent path.
//
// Why this exists: the worker path has had structured stage timing for a long
// time (internal/timing, ~26 call sites in personal_processor.go); the agent
// path had none. Diagnosing a slow agent summary meant hand-correlating
// "[llm] calling" log lines by wall-clock timestamp, which cannot separate a
// planning turn from tool execution and cannot see how large a prompt the
// planner was handed. This instrumentation makes prompt-growth and tool
// stragglers measurable without logging prompt or message content.
//
// The trace is deliberately NOT internal/timing: that package writes per-task
// report files keyed by task_no, which an interactive chat turn does not have.
// This is an in-memory, per-request accumulator that emits scrapeable metrics
// always, and one structured log block when AGENT_TRACE is on.
//
// PRIVACY: this file must never log message content, prompt text, tool
// arguments, user names, or channel names. Sizes, counts, durations, step
// numbers, tool names and the session id only. The session id is an opaque
// identifier already logged by the citation and handler paths; everything
// else here is a number. Adding a field that carries content is a privacy
// regression, not a debugging improvement.
//
// The metric side is ALWAYS on: the accumulator is allocated for every run
// regardless of AGENT_TRACE (the measurement is a few time diffs, slice
// appends and one length-sum per hop — negligible against an LLM round trip),
// so per-request latency percentiles are scrapeable without flipping a flag
// during the incident. AGENT_TRACE gates only the verbose per-step LOG block.
// A nil trace is still a no-op for defensiveness, so a caller that never
// started one changes no control flow.

// TraceEnvVar gates the verbose per-step LOG block. Off by default.
//
// It does NOT gate the metrics: those are always emitted (see the package
// comment). It gates only the log output — a run emits one line per planner
// step plus a few summary lines, useful when diagnosing one request and noise
// when multiplied by production traffic. Read straight from the environment
// rather than through config.Config, following agentStepTimeoutOverride's
// precedent in profile.go: tracing must work in unit tests that build a runner
// without initializing the whole deps container.
const TraceEnvVar = "AGENT_TRACE"

// TraceEnabled reports whether the verbose per-request agent LOG is on.
// Accepts the standard strconv.ParseBool spellings (1/t/true/T/TRUE...).
func TraceEnabled() bool {
	enabled, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(TraceEnvVar)))
	return err == nil && enabled
}

// agentDurationBuckets mirror llmobs/timing's second-boundaries so agent, LLM
// and worker latencies read against one scale. agentStepBuckets and
// agentPromptCharBuckets are shaped for their own quantities: a handful of
// planner turns, and prompt sizes from a small refine to a very large
// long-context turn.
var (
	agentDurationBuckets   = []float64{0.5, 1, 2, 5, 10, 20, 30, 60, 90, 120, 180, 300}
	agentStepBuckets       = []float64{1, 2, 3, 5, 8, 13, 21}
	agentPromptCharBuckets = []float64{1e3, 5e3, 1e4, 5e4, 1e5, 2.5e5, 5e5, 1e6}
)

// Agent-run metrics (#242 S3-b), always emitted from Report regardless of
// AGENT_TRACE. Every label is a closed set (outcome ok|error|panic, phase
// planning|tools) or a fixed registry vocabulary (tool name) — never request
// data, so series count stays bounded.
var (
	agentRequestDur = metrics.NewHistogramVec("agent_request_duration_seconds",
		"Wall-clock of a whole agent run, by outcome (ok|error|panic). The end-to-end latency a chat user feels; the llm_* histograms only cover the model round-trips inside it.",
		agentDurationBuckets)
	agentPhaseDur = metrics.NewHistogramVec("agent_phase_duration_seconds",
		"Wall-clock per agent phase per run: planning (planner LLM turns) vs tools (tool-hop execution). Planning-dominated runs need prompt-size work; tool-dominated runs need tool parallelism.",
		agentDurationBuckets)
	agentToolDur = metrics.NewHistogramVec("agent_tool_duration_seconds",
		"Wall-clock of individual agent tool invocations, by tool name (a fixed registry vocabulary). Names the straggler tool across all traffic.",
		agentDurationBuckets)
	agentSteps = metrics.NewHistogramVec("agent_steps",
		"Distribution of planner turns per agent run. A growing tail means the agent is looping more to reach an answer.",
		agentStepBuckets)
	agentPromptChars = metrics.NewHistogramVec("agent_prompt_chars",
		"Distribution of the largest planner prompt per agent run, in characters. Tracks prompt growth across releases; it is a LENGTH, never content.",
		agentPromptCharBuckets)
)

func init() {
	metrics.Default.MustRegister(agentRequestDur, agentPhaseDur, agentToolDur, agentSteps, agentPromptChars)
}

// stepSpan records one planner turn plus the tool hop it triggered.
type stepSpan struct {
	Step int
	// PlanningMs is the planner LLM round-trip.
	PlanningMs int64
	// PromptChars is the size of the message list handed to the planner. This
	// is the number that explains a slow planning turn: tool results are
	// appended to the list every hop, so hop N pays for every result from
	// hops 1..N-1. It is a LENGTH, never the text.
	PromptChars int
	// PromptMsgs is how many messages were in that list.
	PromptMsgs int
	// OutTokens is the turn's reported completion tokens.
	OutTokens int
	// ToolsMs is the wall-clock of the tool hop (0 when the turn was the
	// final answer).
	ToolsMs int64
	// Tools names the tools called in the hop, in dispatch order. Tool names
	// are a fixed vocabulary from the registry, not user input.
	Tools []string
}

// toolSpan is one named span with a duration.
type toolSpan struct {
	Name string
	Ms   int64
}

// RunTrace accumulates one agent run's phase timings.
//
// Tool spans are recorded from the runner's worker pool, so the mutex is not
// optional; step spans are appended from the single runner goroutine but
// share the same lock for simplicity.
type RunTrace struct {
	mu        sync.Mutex
	start     time.Time
	sessionID string
	// logVerbose is TraceEnabled() captured at StartTrace: it gates the
	// per-step log block in Report, not the metrics.
	logVerbose bool
	steps      []stepSpan
	tools      []toolSpan
	// subPhases holds named sub-timings reported by tools themselves (e.g.
	// the Map phase inside summarize_chunk), so a tool that is itself an LLM
	// pipeline can explain its own cost instead of appearing as one opaque
	// span.
	subPhases []toolSpan
}

type contextKeyTrace struct{}

// ContextKeyTrace carries the *RunTrace for the in-flight request.
var ContextKeyTrace = contextKeyTrace{}

// StartTrace attaches a fresh RunTrace to ctx. It is allocated for every run,
// on or off: the metrics in Report are always-on and need the accumulator.
// AGENT_TRACE only decides whether Report also writes the verbose log block.
//
// Every method is nil-safe, so a caller that passes a nil *RunTrace (or never
// starts one) changes no control flow.
func StartTrace(ctx context.Context, sessionID string) (context.Context, *RunTrace) {
	t := &RunTrace{start: time.Now(), sessionID: sessionID, logVerbose: TraceEnabled()}
	return context.WithValue(ctx, ContextKeyTrace, t), t
}

// TraceFromContext returns the request's trace, or nil when untraced.
// Callers must tolerate nil; every RunTrace method is nil-safe.
func TraceFromContext(ctx context.Context) *RunTrace {
	if ctx == nil {
		return nil
	}
	t, _ := ctx.Value(ContextKeyTrace).(*RunTrace)
	return t
}

// Active reports whether this trace will record anything. Call sites use it
// to skip the measurement work itself (not just the recording) when tracing
// is off.
func (t *RunTrace) Active() bool { return t != nil }

// AddStep records a planner turn. promptChars/promptMsgs describe the message
// list the planner was given, which is the input side of the cost.
func (t *RunTrace) AddStep(step int, planningMs int64, promptChars, promptMsgs, outTokens int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.steps = append(t.steps, stepSpan{
		Step:        step,
		PlanningMs:  planningMs,
		PromptChars: promptChars,
		PromptMsgs:  promptMsgs,
		OutTokens:   outTokens,
	})
}

// CloseStep attaches the tool-hop wall clock to the most recent step.
func (t *RunTrace) CloseStep(step int, toolsMs int64, tools []string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := len(t.steps) - 1; i >= 0; i-- {
		if t.steps[i].Step == step {
			t.steps[i].ToolsMs = toolsMs
			t.steps[i].Tools = tools
			return
		}
	}
}

// AddTool records one tool invocation. Safe to call concurrently.
func (t *RunTrace) AddTool(name string, ms int64) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tools = append(t.tools, toolSpan{Name: name, Ms: ms})
}

// AddSubPhase records a named sub-timing inside a tool (e.g. "map").
func (t *RunTrace) AddSubPhase(name string, ms int64) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.subPhases = append(t.subPhases, toolSpan{Name: name, Ms: ms})
}

// Report emits one structured line per run plus a per-step breakdown.
//
// The headline splits total wall clock three ways — planning / tools /
// unaccounted — because that split is the actual diagnostic question: a run
// dominated by planning needs prompt-size work (fewer/smaller tool results
// fed back), a run dominated by tools needs tool-level parallelism, and a
// large unaccounted remainder means the cost is outside the runner entirely
// (persistence, citation building, the handler's own finalize work).
func (t *RunTrace) Report(outcome string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	totalMs := time.Since(t.start).Milliseconds()
	var planMs, toolMs int64
	var maxPrompt int
	for _, s := range t.steps {
		planMs += s.PlanningMs
		toolMs += s.ToolsMs
		if s.PromptChars > maxPrompt {
			maxPrompt = s.PromptChars
		}
	}

	// Metrics are emitted ALWAYS, before the AGENT_TRACE-gated log below:
	// gating them would recreate the blind spot this instrumentation exists to
	// remove (#231/#242). Labels are closed sets / a fixed tool vocabulary.
	agentRequestDur.Observe(metrics.Labels("outcome", outcome), float64(totalMs)/1000)
	agentPhaseDur.Observe(metrics.Labels("phase", "planning"), float64(planMs)/1000)
	agentPhaseDur.Observe(metrics.Labels("phase", "tools"), float64(toolMs)/1000)
	agentSteps.Observe(metrics.Labels(), float64(len(t.steps)))
	agentPromptChars.Observe(metrics.Labels(), float64(maxPrompt))
	for _, ts := range t.tools {
		agentToolDur.Observe(metrics.Labels("tool", ts.Name), float64(ts.Ms)/1000)
	}

	// AGENT_TRACE gates only the human-readable per-step log block below.
	if !t.logVerbose {
		return
	}

	log.Printf("[agent-trace] session=%s outcome=%s total=%dms planning=%dms(%s) tools=%dms(%s) other=%dms(%s) steps=%d max_prompt=%dchars",
		t.sessionID, outcome, totalMs,
		planMs, pct(planMs, totalMs),
		toolMs, pct(toolMs, totalMs),
		totalMs-planMs-toolMs, pct(totalMs-planMs-toolMs, totalMs),
		len(t.steps), maxPrompt)

	for _, s := range t.steps {
		log.Printf("[agent-trace]   step=%d planning=%dms prompt=%dchars/%dmsgs out_tokens=%d tools=%dms [%s]",
			s.Step, s.PlanningMs, s.PromptChars, s.PromptMsgs, s.OutTokens, s.ToolsMs, strings.Join(s.Tools, ","))
	}

	// Slowest tool invocations first: with concurrent tools inside a hop, the
	// hop's wall clock is set by one straggler, and this names it. Capped at
	// 8 so a wide fan-out cannot turn one run into hundreds of log lines.
	if len(t.tools) > 0 {
		tools := make([]toolSpan, len(t.tools))
		copy(tools, t.tools)
		sort.SliceStable(tools, func(i, j int) bool { return tools[i].Ms > tools[j].Ms })
		var b strings.Builder
		for i, ts := range tools {
			if i >= 8 {
				fmt.Fprintf(&b, " …(+%d more)", len(tools)-i)
				break
			}
			fmt.Fprintf(&b, " %s=%dms", ts.Name, ts.Ms)
		}
		log.Printf("[agent-trace]   tools(slowest first):%s", b.String())
	}

	if len(t.subPhases) > 0 {
		var b strings.Builder
		for _, sp := range t.subPhases {
			fmt.Fprintf(&b, " %s=%dms", sp.Name, sp.Ms)
		}
		log.Printf("[agent-trace]   sub-phases:%s", b.String())
	}

}

func pct(part, total int64) string {
	if total <= 0 {
		return "0%"
	}
	return fmt.Sprintf("%d%%", part*100/total)
}

// measurePrompt returns the total content size and message count of the list
// handed to the planner. Tool-call arguments count toward the size because
// they are part of the serialised request even though they are not in
// Content — but only their LENGTH is taken, never their text.
func measurePrompt(msgs []Message) (chars, count int) {
	for _, m := range msgs {
		chars += len(m.Content)
		for _, tc := range m.ToolCalls {
			chars += len(tc.Function.Arguments)
		}
	}
	return chars, len(msgs)
}
