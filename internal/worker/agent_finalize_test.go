package worker

import (
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

func TestBuildFinalizeConsolidationPrompt(t *testing.T) {
	replies := []model.AgentMessage{
		{Content: "第一段:讨论了 A [1]"},
		{Content: "第二段:结论是 B [2]"},
	}
	p := buildFinalizeConsolidationPrompt("会议纪要", replies)

	// Fragments appear, in order.
	iA := strings.Index(p, "讨论了 A [1]")
	iB := strings.Index(p, "结论是 B [2]")
	if iA < 0 || iB < 0 {
		t.Fatalf("prompt missing fragment content:\n%s", p)
	}
	if iA > iB {
		t.Fatalf("fragments out of order (片段1 must precede 片段2)")
	}
	// Title woven in.
	if !strings.Contains(p, "会议纪要") {
		t.Fatalf("prompt missing the confirmed title")
	}
	// The load-bearing instruction: citation markers must be preserved.
	if !strings.Contains(p, "严格保留引用") {
		t.Fatalf("prompt must instruct verbatim [n] preservation")
	}
	// It must be a MERGE task, not a re-summarize-from-raw task.
	if !strings.Contains(p, "合并") {
		t.Fatalf("prompt must frame the task as consolidation/merge")
	}
}

func TestBuildFinalizeConsolidationPrompt_NoTitle(t *testing.T) {
	p := buildFinalizeConsolidationPrompt("   ", []model.AgentMessage{{Content: "只有一段"}})
	if strings.Contains(p, "用户确认的标题") {
		t.Fatalf("blank title must not emit the title section")
	}
	if !strings.Contains(p, "只有一段") {
		t.Fatalf("prompt missing the single fragment")
	}
}
