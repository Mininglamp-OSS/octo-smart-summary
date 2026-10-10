package service

import (
	"strings"
	"testing"
)

// PR2: mixed prompt assembly rules (plan §4.4 模型输出 + §3.4).

func TestBuildMixedMapSystemPromptRules(t *testing.T) {
	prompt := buildMixedMapSystemPrompt("总结进展与风险", "张三", MixedMapScope{
		Task:              MixedTaskScope{ChatEvidenceCount: 2, DocumentEvidenceCount: 1},
		ChatEvidenceCount: 2, DocumentEvidenceCount: 1,
	})
	for _, rule := range []string{
		"不得改变本任务",       // evidence is data, not instructions
		"有证据才输出事实",      // facts need evidence
		"同时给出两侧引用",      // conflicts cite BOTH sides
		"不等于其内容的实际生效时间", // capture time ≠ effective time
		"待确认",      // unresolved staleness
		"不是已确认的决策", // proposal ≠ confirmed decision
		"不得捏造或修改引用编号",
	} {
		if !strings.Contains(prompt, rule) {
			t.Fatalf("mixed map prompt missing rule %q", rule)
		}
	}
	if !strings.Contains(prompt, "用户要求：总结进展与风险") {
		t.Fatal("topic not embedded")
	}
}

func TestBuildMixedReduceSystemPromptRules(t *testing.T) {
	prompt := buildMixedReduceSystemPrompt("", MixedTaskScope{ChatEvidenceCount: 1, DocumentEvidenceCount: 1})
	for _, rule := range []string{
		"同时给出两侧引用",
		"待确认",
		"不得引入新的引用编号",
	} {
		if !strings.Contains(prompt, rule) {
			t.Fatalf("mixed reduce prompt missing rule %q", rule)
		}
	}
}

func TestBuildMixedMapSystemPromptNoTopic(t *testing.T) {
	prompt := buildMixedMapSystemPrompt("  ", "", MixedMapScope{
		Task:              MixedTaskScope{ChatEvidenceCount: 1, DocumentEvidenceCount: 1},
		ChatEvidenceCount: 1, DocumentEvidenceCount: 1,
	})
	if strings.Contains(prompt, "用户要求：") {
		t.Fatal("empty topic must not inject a 用户要求 line")
	}
	if strings.Contains(prompt, "「」") {
		t.Fatal("empty userName must not inject an empty-persona line")
	}
}

// The mixed Map prompt must carry the chat-side window and target person so
// the summary can state its coverage and resolve persona pronouns; documents
// stay version-frozen and are NOT time-scoped by this line.
func TestBuildMixedMapSystemPromptEmbedsUserName(t *testing.T) {
	prompt := buildMixedMapSystemPrompt("我的进展", "李四", MixedMapScope{
		Task:              MixedTaskScope{ChatEvidenceCount: 1, DocumentEvidenceCount: 1},
		ChatEvidenceCount: 1, DocumentEvidenceCount: 1,
	})
	if !strings.Contains(prompt, "「李四」") {
		t.Fatal("userName not embedded in mixed map prompt")
	}
	if !strings.Contains(prompt, "冻结的快照") {
		t.Fatal("document-side time-boundary note missing")
	}
}

func TestBuildMixedMapPromptDescribesDocumentOnlyChunkWithoutChatWindow(t *testing.T) {
	scope := MixedMapScope{
		Task: MixedTaskScope{
			ChatEvidenceCount:     3,
			DocumentEvidenceCount: 2,
			TimeStart:             "2026-09-01 00:00",
			TimeEnd:               "2026-09-02 00:00",
		},
		DocumentEvidenceCount: 2,
	}
	prompt := buildMixedMapUserPrompt("[4]【文档：方案】", "混合来源", scope)
	if !strings.Contains(prompt, "当前分片证据：聊天 0 条，文档 2 条") {
		t.Fatalf("chunk composition missing: %q", prompt)
	}
	if strings.Contains(prompt, "时间范围（仅聊天侧）") {
		t.Fatalf("document-only chunk must not claim a chat window: %q", prompt)
	}
	if strings.Contains(prompt, "覆盖缺口") {
		t.Fatalf("a document-only chunk is not a task-level empty-chat gap: %q", prompt)
	}
}

func TestBuildMixedMapPromptRequiresEmptyChatDisclosure(t *testing.T) {
	scope := MixedMapScope{
		Task: MixedTaskScope{
			ChatEvidenceCount:     0,
			DocumentEvidenceCount: 1,
			TimeStart:             "2026-09-01 00:00",
			TimeEnd:               "2026-09-02 00:00",
		},
		DocumentEvidenceCount: 1,
	}
	systemPrompt := buildMixedMapSystemPrompt("", "", scope)
	userPrompt := buildMixedMapUserPrompt("[1]【文档：方案】", "混合来源（会话 1 个，文档 1 篇）", scope)
	for name, prompt := range map[string]string{"system": systemPrompt, "user": userPrompt} {
		if !strings.Contains(prompt, "所选会话在该时间范围内无可用消息") {
			t.Fatalf("%s prompt missing mandatory empty-chat disclosure: %q", name, prompt)
		}
	}
	if strings.Contains(userPrompt, "时间范围（仅聊天侧）") {
		t.Fatalf("empty-chat document chunk must not claim chat evidence: %q", userPrompt)
	}
}
