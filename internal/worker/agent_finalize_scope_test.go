package worker

import (
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

// R4 BLOCKING 1 (Jerry-Xin + yujiawei, independently): remapFinalizeCitations
// runs citationRe.ReplaceAllStringFunc over the WHOLE fragment body, so every
// bracketed 1-5 digit token takes one of the two mutating branches.
func TestRemapFinalizeCitations_PreservesNonCitationBrackets(t *testing.T) {
	at := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	rows := []model.AgentMessageEvidence{
		evidenceRow(t, "msg_u1_1", at, []pipeline.Message{
			poolMsg("alpha", 1, 1000, "a"),
			poolMsg("alpha", 2, 1001, "b"),
			poolMsg("alpha", 3, 1002, "c"),
		}),
	}
	finalPool := buildPoolFromEvidenceRows(rows)

	frag := "待办共 [3] 项\n```go\nitems[0] = x\n```\n按 GB/T 7714 [2020] 执行,见 [1]。\n链接 [2](https://example.com/doc)"
	got, _ := remapFinalizeCitations([]model.AgentMessage{{ID: 1, CreatedAt: at, Content: frag}}, rows, finalPool)
	out := got[0].Content

	for _, want := range []string{"待办共 [3] 项", "items[0] = x", "GB/T 7714 [2020]", "[2](https://example.com/doc)"} {
		if !strings.Contains(out, want) {
			t.Errorf("non-citation text destroyed: %q missing from output\n--- out ---\n%s", want, out)
		}
	}
	if !strings.Contains(out, "[1]") {
		t.Errorf("the real marker [1] was lost\n--- out ---\n%s", out)
	}
}
