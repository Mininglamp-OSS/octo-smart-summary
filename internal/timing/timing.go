// Package timing provides a lightweight, append-only stage timer that records
// the duration of each major step of the smart-summary pipeline both to stdout
// (via the standard logger, alongside the existing "took %dms" lines) and to a
// dedicated timing log file.
//
// Each line in the file is a single, self-describing record:
//
//	2026-06-04T17:00:00+08:00 task_no=ST20260604abcd1234 stage=fetch_messages took_ms=1234
//
// The timestamp is in Asia/Shanghai (Beijing time) so the timing log agrees
// with the rest of the system's wall clock. The target directory is created on
// first use (os.MkdirAll) and the file is opened in append mode so concurrent
// workers and process restarts never truncate prior records.
package timing

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
)

// DefaultLogPath is the in-container path of the timing log. The directory is
// created automatically; mount it to the host (see deploy compose) if the log
// must survive container restarts.
const DefaultLogPath = "/var/log/smart-summary/timing.log"

var (
	mu       sync.Mutex
	file     *os.File
	filePath = DefaultLogPath
	openErr  error
	openOnce sync.Once
)

// SetLogPath overrides the timing log file path. Must be called before the first
// Record/Observe; safe no-op if the path is empty.
func SetLogPath(p string) {
	if p == "" {
		return
	}
	mu.Lock()
	filePath = p
	mu.Unlock()
}

// ensureFile lazily opens (creating parent dirs) the timing log file. Failures
// are logged once and degrade gracefully to stdout-only timing.
func ensureFile() *os.File {
	openOnce.Do(func() {
		mu.Lock()
		p := filePath
		mu.Unlock()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			openErr = err
			log.Printf("[timing] cannot create dir for %s: %v (timing file disabled)", p, err)
			return
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			openErr = err
			log.Printf("[timing] cannot open %s: %v (timing file disabled)", p, err)
			return
		}
		file = f
	})
	return file
}

// Record writes one stage timing record to both stdout and the timing file.
// taskNo identifies the summary task; stage is the pipeline step name; d is the
// measured duration.
func Record(taskNo, stage string, d time.Duration) {
	ms := d.Milliseconds()
	// Always echo to stdout so existing log-based observability still works.
	log.Printf("[timing] task=%s stage=%s took=%dms", taskNo, stage, ms)

	f := ensureFile()
	if f == nil {
		return
	}
	line := fmt.Sprintf("%s task_no=%s stage=%s took_ms=%d\n",
		timezone.Now().Format(time.RFC3339), taskNo, stage, ms)
	mu.Lock()
	_, _ = f.WriteString(line)
	mu.Unlock()
}

// Stage starts a timer for `stage`. Call the returned func (typically deferred)
// to record the elapsed duration. Example:
//
//	done := timing.Stage(taskNo, "llm_summary")
//	... work ...
//	done()
func Stage(taskNo, stage string) func() {
	start := time.Now()
	return func() {
		Record(taskNo, stage, time.Since(start))
	}
}

// Observe records a stage whose start time the caller already holds. Useful when
// the start instant predates the decision to measure.
func Observe(taskNo, stage string, start time.Time) {
	Record(taskNo, stage, time.Since(start))
}

// ---------------------------------------------------------------------------
// Per-task LLM call accounting + human-readable summary report.
//
// Goal (per Boss request): for every smart-summary run, produce one consolidated
// report showing HOW MANY times the LLM was called, WHAT each call was for, and
// how long / how many tokens each took. Records are accumulated in memory keyed
// by task_no, then flushed as a single multi-line block to a dedicated report
// file when the run finishes.
// ---------------------------------------------------------------------------

// DefaultReportPath is the in-container path of the per-run summary report.
const DefaultReportPath = "/var/log/smart-summary/summary-report.log"

// LLMCall is one recorded LLM invocation within a task.
type LLMCall struct {
	Purpose string // human-readable: what this call was for
	TookMs  int64
	Tokens  int
}

var (
	acctMu     sync.Mutex
	acct       = map[string][]LLMCall{}
	reportPath = DefaultReportPath
	reportFile *os.File
	reportOnce sync.Once
	reportErr  error
)

// SetReportPath overrides the summary-report file path. Call before first use.
func SetReportPath(p string) {
	if p == "" {
		return
	}
	acctMu.Lock()
	reportPath = p
	acctMu.Unlock()
}

// RecordLLM appends one LLM call record for taskNo. purpose describes what the
// call did (e.g. "Map: 分块总结 chunk#2"); tokens is the total tokens reported by
// the gateway for that call (0 if unknown). It is safe for concurrent callers
// (Map runs chunks in parallel).
func RecordLLM(taskNo, purpose string, took time.Duration, tokens int) {
	if taskNo == "" {
		return
	}
	acctMu.Lock()
	acct[taskNo] = append(acct[taskNo], LLMCall{Purpose: purpose, TookMs: took.Milliseconds(), Tokens: tokens})
	acctMu.Unlock()
}

// RecordLLMSince is RecordLLM with a start instant the caller already holds.
func RecordLLMSince(taskNo, purpose string, start time.Time, tokens int) {
	RecordLLM(taskNo, purpose, time.Since(start), tokens)
}

func ensureReportFile() *os.File {
	reportOnce.Do(func() {
		acctMu.Lock()
		p := reportPath
		acctMu.Unlock()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			reportErr = err
			log.Printf("[timing] cannot create dir for %s: %v (report file disabled)", p, err)
			return
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			reportErr = err
			log.Printf("[timing] cannot open %s: %v (report file disabled)", p, err)
			return
		}
		reportFile = f
	})
	return reportFile
}

// FlushReport writes one consolidated, human-readable report block for taskNo to
// the summary-report file and clears the in-memory records for that task. It is
// called once when a summary run finishes (success or failure). totalStage is
// the measured end-to-end duration of the run; extraStages is an ordered list of
// (name,ms) non-LLM stage timings to include for context.
func FlushReport(taskNo string, totalMs int64, extraStages []StageMs) {
	if taskNo == "" {
		return
	}
	acctMu.Lock()
	calls := acct[taskNo]
	delete(acct, taskNo)
	acctMu.Unlock()

	// Get TaskContext snapshot for safe reading, then clear
	ctxPtr := GetContext(taskNo)
	ctx := ctxPtr.Snapshot()
	defer ClearContext(taskNo)

	var llmTotalMs int64
	var llmTotalTokens int
	for _, c := range calls {
		llmTotalMs += c.TookMs
		llmTotalTokens += c.Tokens
	}

	var b strings.Builder
	ts := timezone.Now().Format(time.RFC3339)
	fmt.Fprintf(&b, "==== 智能总结汇总报告 task_no=%s time=%s ====\n", taskNo, ts)

	if ctx.TaskCreatedToWorkerStartMs > 0 || ctx.PersonalResultCreatedToWorkerStartMs > 0 || ctx.ParticipantCreatedToWorkerStartMs > 0 {
		fmt.Fprintf(&b, "前置等待: 任务创建→worker开始=%dms 个人结果创建→worker开始=%dms 参与者创建→worker开始=%dms\n",
			ctx.TaskCreatedToWorkerStartMs, ctx.PersonalResultCreatedToWorkerStartMs, ctx.ParticipantCreatedToWorkerStartMs)
	}
	if len(ctx.WorkflowStages) > 0 {
		b.WriteString("Workflow阶段上报:\n")
		for _, ws := range ctx.WorkflowStages {
			fmt.Fprintf(&b, "  stage=%s  距worker开始=%dms  距上一阶段=%dms\n", ws.Stage, ws.SinceWorkerStartMs, ws.DeltaMs)
		}
	}

	// Intent recognition section
	if ctx.IntentSkipped {
		fmt.Fprintf(&b, "意图识别: 短路=是 原因=%s\n", ctx.IntentSkipReason)
	} else {
		fmt.Fprintf(&b, "意图识别: 短路=否 LLM调用=%d次\n", ctx.IntentLLMCalls)
	}

	// Message retrieval section
	if ctx.ChannelCount > 0 || ctx.MessagesRetrieved > 0 {
		fmt.Fprintf(&b, "消息获取: 频道=%d 召回=%d条 最终=%d条\n",
			ctx.ChannelCount, ctx.MessagesRetrieved, ctx.MessagesFinal)
	}

	// Post-processing section
	if ctx.TargetPersonCount > 0 || ctx.FilteredCount > 0 {
		fmt.Fprintf(&b, "后处理: 目标人物=%d人 过滤后=%d条\n",
			ctx.TargetPersonCount, ctx.FilteredCount)
	}

	// Summary generation section
	if ctx.UsedMapReduce {
		fmt.Fprintf(&b, "总结生成: Map-Reduce=是 分块=%d\n", ctx.ChunkCount)
	} else if ctx.MessagesFinal > 0 {
		fmt.Fprintf(&b, "总结生成: Map-Reduce=否 (单次调用)\n")
	}

	// LLM calls detail
	fmt.Fprintf(&b, "LLM 调用次数: %d  | LLM 累计耗时: %dms  | LLM 累计 tokens: %d\n", len(calls), llmTotalMs, llmTotalTokens)
	if len(calls) == 0 {
		b.WriteString("  (本次没有产生 LLM 调用)\n")
	}
	for i, c := range calls {
		fmt.Fprintf(&b, "  #%d  用途=%s  耗时=%dms  tokens=%d\n", i+1, c.Purpose, c.TookMs, c.Tokens)
	}
	if len(extraStages) > 0 {
		b.WriteString("环节耗时:\n")
		for _, s := range extraStages {
			fmt.Fprintf(&b, "  %s=%dms\n", s.Name, s.Ms)
		}
	}
	fmt.Fprintf(&b, "全流程合计: %dms\n", totalMs)
	b.WriteString("================================================\n")

	// Always echo to stdout for docker logs observability.
	log.Printf("[summary-report]\n%s", b.String())

	f := ensureReportFile()
	if f == nil {
		return
	}
	acctMu.Lock()
	_, _ = f.WriteString(b.String())
	acctMu.Unlock()
}

// StageMs is a named non-LLM stage duration for the report's context section.
type StageMs struct {
	Name string
	Ms   int64
}

// WorkflowStageMs records when a user-visible workflow stage was reported.
type WorkflowStageMs struct {
	Stage              string
	SinceWorkerStartMs int64
	DeltaMs            int64
}

// ---------------------------------------------------------------------------
// TaskContext: unified task context for enhanced reporting.
//
// Captures metrics across all stages for a single summary task, enabling
// richer reports that include message counts, filtering stats, and intent
// recognition decisions alongside timing data.
// ---------------------------------------------------------------------------

// TaskContext holds per-task metrics collected during pipeline execution.
// All field access must go through Update() or Snapshot() to avoid data races
// when multiple goroutines work on the same task (e.g. multi-person summaries).
type TaskContext struct {
	mu     sync.Mutex // guards all fields below
	TaskNo string

	// Pre-worker waiting
	TaskCreatedToWorkerStartMs           int64 // summary_task.created_at -> personal worker start
	PersonalResultCreatedToWorkerStartMs int64 // summary_personal_result.created_at -> personal worker start
	ParticipantCreatedToWorkerStartMs    int64 // summary_participant.created_at -> personal worker start

	// User-visible workflow stages
	WorkflowStages []WorkflowStageMs

	// Intent recognition
	IntentSkipped    bool   // true if short-circuited (no LLM)
	IntentSkipReason string // e.g. "pure_generic_topic", "simple_channel_constraint"
	IntentLLMCalls   int    // number of LLM calls for intent (0 if skipped)

	// Message retrieval
	ChannelCount      int // number of channels in scope
	MessagesRetrieved int // messages fetched before filtering
	MessagesFinal     int // messages after post-processing

	// Post-processing
	TargetPersonCount int // number of target persons
	FilteredCount     int // messages after FilterWithContext

	// Summary generation
	UsedMapReduce bool // true if Map-Reduce was used
	ChunkCount    int  // number of chunks in Map phase
	TotalTokens   int  // total tokens across all LLM calls
}

// Update acquires the context lock and calls fn with the context pointer,
// allowing safe concurrent writes to TaskContext fields.
func (c *TaskContext) Update(fn func(*TaskContext)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn(c)
}

// Snapshot returns a copy of all fields for safe reading. Use this when
// multiple fields need to be read together consistently.
func (c *TaskContext) Snapshot() TaskContext {
	c.mu.Lock()
	defer c.mu.Unlock()
	workflowStages := append([]WorkflowStageMs(nil), c.WorkflowStages...)
	return TaskContext{
		TaskNo:                               c.TaskNo,
		TaskCreatedToWorkerStartMs:           c.TaskCreatedToWorkerStartMs,
		PersonalResultCreatedToWorkerStartMs: c.PersonalResultCreatedToWorkerStartMs,
		ParticipantCreatedToWorkerStartMs:    c.ParticipantCreatedToWorkerStartMs,
		WorkflowStages:                       workflowStages,
		IntentSkipped:                        c.IntentSkipped,
		IntentSkipReason:                     c.IntentSkipReason,
		IntentLLMCalls:                       c.IntentLLMCalls,
		ChannelCount:                         c.ChannelCount,
		MessagesRetrieved:                    c.MessagesRetrieved,
		MessagesFinal:                        c.MessagesFinal,
		TargetPersonCount:                    c.TargetPersonCount,
		FilteredCount:                        c.FilteredCount,
		UsedMapReduce:                        c.UsedMapReduce,
		ChunkCount:                           c.ChunkCount,
		TotalTokens:                          c.TotalTokens,
	}
}

var (
	ctxMu   sync.Mutex
	taskCtx = map[string]*TaskContext{}
)

// GetContext returns the TaskContext for taskNo, creating it if needed.
// Thread-safe for concurrent access.
func GetContext(taskNo string) *TaskContext {
	if taskNo == "" {
		return &TaskContext{} // return dummy to avoid nil checks
	}
	ctxMu.Lock()
	defer ctxMu.Unlock()
	if ctx, ok := taskCtx[taskNo]; ok {
		return ctx
	}
	ctx := &TaskContext{TaskNo: taskNo}
	taskCtx[taskNo] = ctx
	return ctx
}

// ClearContext removes the TaskContext for taskNo (call after FlushReport).
func ClearContext(taskNo string) {
	if taskNo == "" {
		return
	}
	ctxMu.Lock()
	delete(taskCtx, taskNo)
	ctxMu.Unlock()
}

// RecordSkip records that a stage was skipped (short-circuited).
func RecordSkip(taskNo, stage, reason string) {
	if taskNo == "" {
		return
	}
	ctx := GetContext(taskNo)
	ctx.Update(func(c *TaskContext) {
		if stage == "intent_recognition" {
			c.IntentSkipped = true
			c.IntentSkipReason = reason
		}
	})
	log.Printf("[timing] task=%s skipped %s (%s)", taskNo, stage, reason)
}
