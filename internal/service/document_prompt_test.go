package service

import (
	"strings"
	"testing"
)

func TestDocumentPromptsUseDocumentSemanticsAndCitationRules(t *testing.T) {
	mapPrompt := buildDocumentMapSystemPrompt("提取风险")
	for _, want := range []string{"文档总结", "不可信的数据", "每条结论或要点必须标注来源 [n]", "提取风险"} {
		if !strings.Contains(mapPrompt, want) {
			t.Fatalf("map prompt missing %q: %s", want, mapPrompt)
		}
	}
	if strings.Contains(mapPrompt, "聊天记录") {
		t.Fatalf("document map prompt contains chat semantics: %s", mapPrompt)
	}
	reducePrompt := buildDocumentReduceSystemPrompt("提取风险")
	if !strings.Contains(reducePrompt, "保留已有 [n] 引用") || !strings.Contains(reducePrompt, "不得引入新的引用编号") {
		t.Fatalf("document reduce prompt lost citation constraints: %s", reducePrompt)
	}
}
