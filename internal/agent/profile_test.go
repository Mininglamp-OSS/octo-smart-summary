package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGetProfile_SummaryRefine 验证 summary_refine profile 能正确装配
func TestGetProfile_SummaryRefine(t *testing.T) {
	profile, err := GetProfile("summary_refine")
	if err != nil {
		t.Fatalf("GetProfile(summary_refine) failed: %v", err)
	}

	if profile.PromptFile != "summary_refine" {
		t.Errorf("PromptFile = %q, want %q", profile.PromptFile, "summary_refine")
	}

	// 验证工具白名单：应该有 11 项
	expectedTools := []string{
		"list_channels", "narrow_channels_by_topic", "find_shared_channels",
		"peek_channel", "fetch_channel", "search_messages",
		"filter_relevant", "summarize_chunk", "merge_summaries",
		"get_current_time", "extract_time_range",
	}

	if len(profile.Tools) != len(expectedTools) {
		t.Errorf("Tools count = %d, want %d", len(profile.Tools), len(expectedTools))
	}

	// 验证每个期望的工具都在白名单里
	toolSet := make(map[string]bool)
	for _, tool := range profile.Tools {
		toolSet[tool] = true
	}

	for _, expected := range expectedTools {
		if !toolSet[expected] {
			t.Errorf("missing expected tool %q in summary_refine profile", expected)
		}
	}

	// 验证 Policy
	if profile.Policy.MaxSteps != 15 {
		t.Errorf("Policy.MaxSteps = %d, want 15", profile.Policy.MaxSteps)
	}
	if profile.Policy.MaxTokens != 120000 {
		t.Errorf("Policy.MaxTokens = %d, want 120000", profile.Policy.MaxTokens)
	}
	if profile.Policy.StepTimeout != 240e9 {
		t.Errorf("Policy.StepTimeout = %d, want 240e9", profile.Policy.StepTimeout)
	}
}

// TestLoadPrompt_SummaryRefine 验证 summary_refine 提示词文件能正常加载
func TestLoadPrompt_SummaryRefine(t *testing.T) {
	content, err := LoadPrompt("summary_refine")
	if err != nil {
		t.Fatalf("LoadPrompt(summary_refine) failed: %v", err)
	}

	if content == "" {
		t.Fatal("LoadPrompt returned empty content")
	}

	// 验证关键文本是否存在（对齐当前引用式 summary_refine 提示词）
	expectedPhrases := []string{
		"引用 ≠ 增量修改",
		"参考素材",
		"自主决策权",
		"get_current_time",
	}

	for _, phrase := range expectedPhrases {
		if !strings.Contains(content, phrase) {
			t.Errorf("prompt content missing expected phrase %q", phrase)
		}
	}
}

// TestLoadPrompt_ExternalOverride 验证 AGENT_PROMPT_DIR 覆盖机制生效
func TestLoadPrompt_ExternalOverride(t *testing.T) {
	// 创建临时目录和测试提示词文件
	tmpDir := t.TempDir()
	testPromptPath := filepath.Join(tmpDir, "summary_refine.md")
	testContent := "THIS IS A TEST OVERRIDE PROMPT"

	if err := os.WriteFile(testPromptPath, []byte(testContent), 0644); err != nil {
		t.Fatalf("failed to write test prompt: %v", err)
	}

	// 设置环境变量
	oldVal := os.Getenv("AGENT_PROMPT_DIR")
	os.Setenv("AGENT_PROMPT_DIR", tmpDir)
	defer os.Setenv("AGENT_PROMPT_DIR", oldVal)

	// 清空缓存,确保重新读取
	promptCacheMu.Lock()
	delete(promptCache, "summary_refine")
	promptCacheMu.Unlock()

	content, err := LoadPrompt("summary_refine")
	if err != nil {
		t.Fatalf("LoadPrompt with AGENT_PROMPT_DIR failed: %v", err)
	}

	if content != testContent {
		t.Errorf("LoadPrompt with override: got %q, want %q", content, testContent)
	}
}

// TestBuildRegistry_SummaryRefine 验证 summary_refine 的工具白名单能正确构造 Registry
func TestBuildRegistry_SummaryRefine(t *testing.T) {
	profile, err := GetProfile("summary_refine")
	if err != nil {
		t.Fatalf("GetProfile failed: %v", err)
	}

	registry, err := BuildRegistry(profile.Tools)
	if err != nil {
		t.Fatalf("BuildRegistry failed: %v", err)
	}

	schemas := registry.Schemas()
	if len(schemas) != 11 {
		t.Errorf("BuildRegistry produced %d schemas, want 11", len(schemas))
	}

	// 验证每个工具都有对应的 schema
	schemaNames := make(map[string]bool)
	for _, schema := range schemas {
		if schema.Function.Name != "" {
			schemaNames[schema.Function.Name] = true
		}
	}

	expectedTools := []string{
		"list_channels", "narrow_channels_by_topic", "find_shared_channels",
		"peek_channel", "fetch_channel", "search_messages",
		"filter_relevant", "summarize_chunk", "merge_summaries",
		"get_current_time", "extract_time_range",
	}

	for _, tool := range expectedTools {
		if !schemaNames[tool] {
			t.Errorf("BuildRegistry missing schema for tool %q", tool)
		}
	}
}

// TestGetProfile_UnknownProfile 验证未知 profile 报错
func TestGetProfile_UnknownProfile(t *testing.T) {
	_, err := GetProfile("nonexistent_profile")
	if err == nil {
		t.Fatal("GetProfile for unknown profile should return error")
	}

	if !strings.Contains(err.Error(), "unknown agent profile") {
		t.Errorf("error message should mention unknown profile, got: %v", err)
	}
}
