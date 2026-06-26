package timing

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
