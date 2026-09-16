package timing

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/metrics"
)

func TestRecord(t *testing.T) {
	// Use temp file for timing log
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "timing.log")

	// Reset global state for test isolation
	mu.Lock()
	filePath = tmpFile
	file = nil
	openOnce = sync.Once{}
	openErr = nil
	mu.Unlock()

	// Record a stage
	Record("ST20260617test001", "fetch_messages", 1234*time.Millisecond)

	// Read and verify
	content, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatalf("failed to read timing log: %v", err)
	}

	s := string(content)
	if !strings.Contains(s, "task_no=ST20260617test001") {
		t.Errorf("missing task_no in timing log: %s", s)
	}
	if !strings.Contains(s, "stage=fetch_messages") {
		t.Errorf("missing stage in timing log: %s", s)
	}
	if !strings.Contains(s, "took_ms=1234") {
		t.Errorf("missing took_ms in timing log: %s", s)
	}
}

func TestStage(t *testing.T) {
	// Use temp file
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "timing.log")

	mu.Lock()
	filePath = tmpFile
	file = nil
	openOnce = sync.Once{}
	openErr = nil
	mu.Unlock()

	// Use Stage helper
	done := Stage("ST20260617test002", "llm_summary")
	time.Sleep(10 * time.Millisecond)
	done()

	// Verify
	content, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatalf("failed to read timing log: %v", err)
	}

	s := string(content)
	if !strings.Contains(s, "stage=llm_summary") {
		t.Errorf("missing stage in timing log: %s", s)
	}
}

func TestRecordLLM(t *testing.T) {
	// Clear previous state
	acctMu.Lock()
	acct = map[string][]LLMCall{}
	acctMu.Unlock()

	taskNo := "ST20260617test003"

	// Record multiple LLM calls
	RecordLLM(taskNo, "意图识别", 500*time.Millisecond, 1000)
	RecordLLM(taskNo, "Map: 分块总结 chunk#1", 2000*time.Millisecond, 5000)
	RecordLLM(taskNo, "Map: 分块总结 chunk#2", 1800*time.Millisecond, 4500)
	RecordLLM(taskNo, "Reduce: 最终总结", 3000*time.Millisecond, 8000)

	// Verify
	acctMu.Lock()
	calls := acct[taskNo]
	acctMu.Unlock()

	if len(calls) != 4 {
		t.Errorf("expected 4 LLM calls, got %d", len(calls))
	}

	if calls[0].Purpose != "意图识别" {
		t.Errorf("first call purpose = %q, want '意图识别'", calls[0].Purpose)
	}
	if calls[0].TookMs != 500 {
		t.Errorf("first call TookMs = %d, want 500", calls[0].TookMs)
	}
	if calls[0].Tokens != 1000 {
		t.Errorf("first call Tokens = %d, want 1000", calls[0].Tokens)
	}
}

func TestTaskContext(t *testing.T) {
	taskNo := "ST20260617test004"

	// Clear previous state
	ctxMu.Lock()
	taskCtx = map[string]*TaskContext{}
	ctxMu.Unlock()

	// Get context (creates new)
	ctx := GetContext(taskNo)
	if ctx.TaskNo != taskNo {
		t.Errorf("TaskNo = %q, want %q", ctx.TaskNo, taskNo)
	}

	// Set some values
	ctx.IntentSkipped = true
	ctx.IntentSkipReason = "pure_generic_topic"
	ctx.ChannelCount = 5
	ctx.MessagesRetrieved = 1000
	ctx.MessagesFinal = 800

	// Get again (should return same)
	ctx2 := GetContext(taskNo)
	if ctx2 != ctx {
		t.Error("GetContext returned different instance")
	}
	if !ctx2.IntentSkipped {
		t.Error("IntentSkipped not preserved")
	}

	// Clear
	ClearContext(taskNo)
	ctxMu.Lock()
	_, exists := taskCtx[taskNo]
	ctxMu.Unlock()
	if exists {
		t.Error("ClearContext did not remove task")
	}
}

func TestFlushReport(t *testing.T) {
	// Use temp file for report
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "report.log")

	// Reset global state
	acctMu.Lock()
	reportPath = tmpFile
	reportFile = nil
	reportOnce = sync.Once{}
	reportErr = nil
	acct = map[string][]LLMCall{}
	acctMu.Unlock()

	ctxMu.Lock()
	taskCtx = map[string]*TaskContext{}
	ctxMu.Unlock()

	taskNo := "ST20260617test005"

	// Set up context
	ctx := GetContext(taskNo)
	ctx.IntentSkipped = false
	ctx.IntentLLMCalls = 1
	ctx.ChannelCount = 3
	ctx.MessagesRetrieved = 500
	ctx.MessagesFinal = 450
	ctx.UsedMapReduce = true
	ctx.ChunkCount = 2

	// Record LLM calls
	RecordLLM(taskNo, "意图识别", 500*time.Millisecond, 1000)
	RecordLLM(taskNo, "Map: chunk#1", 2000*time.Millisecond, 5000)
	RecordLLM(taskNo, "Reduce", 3000*time.Millisecond, 8000)

	// Flush report
	FlushReport(taskNo, 10000, []StageMs{
		{"fetch", 1000},
		{"postprocess", 200},
	})

	// Verify
	content, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatalf("failed to read report: %v", err)
	}

	s := string(content)

	// Check required elements
	checks := []string{
		"智能总结汇总报告",
		"task_no=" + taskNo,
		"意图识别: 短路=否 LLM调用=1次",
		"消息获取: 频道=3 召回=500条 最终=450条",
		"总结生成: Map-Reduce=是 分块=2",
		"LLM 调用次数: 3",
		"用途=意图识别",
		"用途=Map: chunk#1",
		"用途=Reduce",
		"环节耗时:",
		"fetch=1000ms",
		"postprocess=200ms",
		"全流程合计: 10000ms",
	}

	for _, check := range checks {
		if !strings.Contains(s, check) {
			t.Errorf("report missing: %q\nGot:\n%s", check, s)
		}
	}
}

func TestFlushReport_Shortcut(t *testing.T) {
	// Use temp file for report
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "report.log")

	// Reset global state
	acctMu.Lock()
	reportPath = tmpFile
	reportFile = nil
	reportOnce = sync.Once{}
	reportErr = nil
	acct = map[string][]LLMCall{}
	acctMu.Unlock()

	ctxMu.Lock()
	taskCtx = map[string]*TaskContext{}
	ctxMu.Unlock()

	taskNo := "ST20260617test006"

	// Set up context with shortcut
	ctx := GetContext(taskNo)
	ctx.IntentSkipped = true
	ctx.IntentSkipReason = "pure_generic_topic"
	ctx.ChannelCount = 1
	ctx.MessagesRetrieved = 100
	ctx.MessagesFinal = 100

	// No LLM calls for intent (shortcutted)
	RecordLLM(taskNo, "单次总结", 2000*time.Millisecond, 5000)

	// Flush report
	FlushReport(taskNo, 3000, nil)

	// Verify
	content, err := os.ReadFile(tmpFile)
	if err != nil {
		t.Fatalf("failed to read report: %v", err)
	}

	s := string(content)

	if !strings.Contains(s, "意图识别: 短路=是 原因=pure_generic_topic") {
		t.Errorf("report missing shortcut info:\n%s", s)
	}
	if !strings.Contains(s, "LLM 调用次数: 1") {
		t.Errorf("report should show 1 LLM call:\n%s", s)
	}
}

func TestRecordSkip(t *testing.T) {
	ctxMu.Lock()
	taskCtx = map[string]*TaskContext{}
	ctxMu.Unlock()

	taskNo := "ST20260617test007"

	// Record skip
	RecordSkip(taskNo, "intent_recognition", "simple_channel_constraint")

	// Verify
	ctx := GetContext(taskNo)
	if !ctx.IntentSkipped {
		t.Error("IntentSkipped should be true")
	}
	if ctx.IntentSkipReason != "simple_channel_constraint" {
		t.Errorf("IntentSkipReason = %q, want 'simple_channel_constraint'", ctx.IntentSkipReason)
	}

	ClearContext(taskNo)
}

func TestGetContext_Empty(t *testing.T) {
	// Empty taskNo should return dummy
	ctx := GetContext("")
	if ctx == nil {
		t.Error("GetContext('') should not return nil")
	}
	// Should not panic
	ctx.IntentSkipped = true
}

func TestClearContext_Empty(t *testing.T) {
	// Empty taskNo should not panic
	ClearContext("")
}

func TestRecordLLM_Empty(t *testing.T) {
	// Empty taskNo should not record
	acctMu.Lock()
	acct = map[string][]LLMCall{}
	acctMu.Unlock()

	RecordLLM("", "test", time.Second, 100)

	acctMu.Lock()
	count := len(acct)
	acctMu.Unlock()

	if count != 0 {
		t.Error("RecordLLM with empty taskNo should not record")
	}
}

// productionPurposes is the census of every literal (or literal-shaped) value
// passed to RecordLLM/RecordLLMSince in non-test code at head, paired with the
// class it MUST map to. Keep this in sync with the call-site census when a new
// RecordLLM* site is added — the retrieval-prep entries are "检索预处理: " + the
// closed forceFn set plus the no-force "(tool-call)" case.
var productionPurposes = []struct {
	purpose string
	want    string
}{
	{"检索预处理(tool-call)", "retrieval_prep"},
	{"检索预处理: recognize_intent", "intent"},
	{"检索预处理: extract_time_range", "extract_time_range"},
	{"检索预处理: resolve_channel_scope", "resolve_channel_scope"},
	{"检索预处理: resolve_topic_target", "resolve_topic_target"},
	{"检索后裁剪 PostRetrievalNarrow", "post_retrieval_narrow"},
	{"Map: 单次总结(跳过Map-Reduce)", "map_single"},
	{"Map: 分块总结 chunk#0", "map_chunk"},
	{"Map: 分块总结 chunk#7", "map_chunk"},
	{"Reduce: 合并分块总结", "reduce"},
	{"团队汇总: 合并各成员总结", "team_reduce"},
}

// TestPurposeClass_RealCallSites pins every production purpose value to its
// class. Built from the RecordLLM* census rather than the report-rendering
// strings, so misclassifying any real family fails here.
func TestPurposeClass_RealCallSites(t *testing.T) {
	for _, c := range productionPurposes {
		if got := PurposeClass(c.purpose); got != c.want {
			t.Errorf("PurposeClass(%q) = %q, want %q", c.purpose, got, c.want)
		}
	}
}

// TestPurposeClass_NoProductionPurposeIsOther is the anti-vacuity guard the
// review asked for: no real call-site value may land in the "other" junk drawer.
// Misclassifying any current production family (e.g. dropping the retrieval-prep
// cases) turns this red, where a table of speculative literals would not.
func TestPurposeClass_NoProductionPurposeIsOther(t *testing.T) {
	for _, c := range productionPurposes {
		if got := PurposeClass(c.purpose); got == "other" {
			t.Errorf("production purpose %q classified as %q — real latency would be buried in the unclassified bucket", c.purpose, got)
		}
	}
}

// TestPurposeClass_UnknownFallsThrough documents the closed-set contract: a
// value no call site emits, including a would-be-new forced function, lands in
// "other" rather than silently widening the label space.
func TestPurposeClass_UnknownFallsThrough(t *testing.T) {
	for _, p := range []string{
		"",
		"something new nobody classified",
		"检索预处理: brand_new_forced_fn",
		"ReducerDebug", // must NOT be caught by the reduce rule
	} {
		if got := PurposeClass(p); got != "other" {
			t.Errorf("PurposeClass(%q) = %q, want other", p, got)
		}
	}
}

// TestPurposeClass_ChunkIndexIsBounded is the cardinality guard: a thousand
// distinct chunk indices must produce exactly one class, or the label space is
// unbounded and this function has failed its only job.
func TestPurposeClass_ChunkIndexIsBounded(t *testing.T) {
	seen := map[string]struct{}{}
	for i := 0; i < 1000; i++ {
		seen[PurposeClass(fmt.Sprintf("Map: 分块总结 chunk#%d", i))] = struct{}{}
	}
	if len(seen) != 1 {
		t.Errorf("chunk-map purposes produced %d classes, want 1 — cardinality is not bounded: %v", len(seen), seen)
	}
}

// TestRecord_EmitsStageHistogram checks Record feeds the scrapeable
// summary_stage_duration_seconds histogram, labelled by the (closed-set) stage
// name — the metric side of #242 S3-b. Assertions are on presence, not exact
// counts, because the histograms are process-global and accumulate across the
// package's other tests.
func TestRecord_EmitsStageHistogram(t *testing.T) {
	Record("ST-x", "fetch_messages", 1500*time.Millisecond)

	out := scrapeDefault()
	for _, want := range []string{
		"# TYPE summary_stage_duration_seconds histogram",
		`summary_stage_duration_seconds_bucket{stage="fetch_messages",le="2"}`,
		`summary_stage_duration_seconds_count{stage="fetch_messages"}`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in exposition:\n%s", want, out)
		}
	}
}

// TestRecordLLM_EmitsPurposeClassHistogram checks RecordLLM fires the
// summary_llm_duration_seconds histogram labelled by PurposeClass — pulling the
// trigger on the classifier from #240 (which no metric consumed before). It
// also confirms the unbounded chunk index is collapsed to purpose_class="map_chunk".
func TestRecordLLM_EmitsPurposeClassHistogram(t *testing.T) {
	for i := 0; i < 5; i++ {
		RecordLLM("ST-y", fmt.Sprintf("Map: 分块总结 chunk#%d", i), 800*time.Millisecond, 100)
	}
	RecordLLM("ST-y", "检索预处理: recognize_intent", 300*time.Millisecond, 50)

	out := scrapeDefault()
	for _, want := range []string{
		"# TYPE summary_llm_duration_seconds histogram",
		`summary_llm_duration_seconds_count{purpose_class="map_chunk"}`,
		`summary_llm_duration_seconds_count{purpose_class="intent"}`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in exposition:\n%s", want, out)
		}
	}
	// The unbounded chunk index must not leak into the label space.
	if strings.Contains(out, "chunk#") {
		t.Errorf("raw chunk index leaked into a metric label:\n%s", out)
	}
}

func scrapeDefault() string {
	var buf bytes.Buffer
	metrics.Default.WritePrometheus(&buf)
	return buf.String()
}

// knownStages is the census of stage names passed to Record/Stage/Observe at
// worker call sites as of #244. `stage` is a metric label, so it must stay a
// closed set. This list pins it: if you add a Record/Stage/Observe call with a
// new stage name, add it here — and never build a stage from request data.
// (A test cannot see the call sites directly, so this is a documented census,
// not a mechanical guard; keep it in sync when touching worker timing.)
var knownStages = []string{
	"execute_pipeline_total",
	"fetch_messages",
	"persist_personal_result",
	"personal_pipeline_total",
	"resolve_user_names",
	"llm_map_summary",
	"llm_reduce_summary",
	"build_citations",
}

// TestRecord_StageCensusRenders pins the stage vocabulary: every known stage
// renders a clean summary_stage_duration_seconds series under its literal name.
func TestRecord_StageCensusRenders(t *testing.T) {
	for _, s := range knownStages {
		Record("ST-census", s, 100*time.Millisecond)
	}
	out := scrapeDefault()
	for _, s := range knownStages {
		want := `summary_stage_duration_seconds_count{stage="` + s + `"}`
		if !strings.Contains(out, want) {
			t.Errorf("stage %q did not render its metric series (%s)", s, want)
		}
	}
}
