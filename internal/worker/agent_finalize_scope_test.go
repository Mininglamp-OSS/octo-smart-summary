package worker

import (
	"strconv"
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
	got, _ := remapFinalizeCitations([]model.AgentMessage{{ID: 1, CreatedAt: at, Content: frag}}, rows, finalPool, nil)
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

// R4 BLOCKING 2: agent_message.created_at and agent_message_evidence.created_at
// are both plain DATETIME (second resolution) and the per-turn filter uses an
// INCLUSIVE bound, so turn 2 evidence stamped in the same second as turn 1 reply
// enters turn 1 re-derived pool. If it sorts first, turn 1 [1] silently
// re-points at a message the sentence was never about.
func TestRemapFinalizeCitations_SameSecondEvidenceResolvesByID(t *testing.T) {
	sec := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)

	alpha := []pipeline.Message{
		poolMsg("alpha", 1, 1000, "alpha-1 THE ONE"),
		poolMsg("alpha", 2, 1001, "alpha-2"),
	}
	// beta fetched by turn 2, persisted in the SAME second as turn 1 reply,
	// with EARLIER timestamps so it sorts first.
	beta := []pipeline.Message{poolMsg("beta", 1, 10, "beta-1")}

	rows := []model.AgentMessageEvidence{
		evidenceRow(t, "msg_u1_1", sec, alpha),
		evidenceRow(t, "msg_u1_2", sec, beta), // same second as reply 1
	}
	finalPool := buildPoolFromEvidenceRows(rows)

	replies := []model.AgentMessage{
		{ID: 10, CreatedAt: sec, Content: "确认了 [1]"},               // turn 1: [1] means alpha-1
		{ID: 20, CreatedAt: sec.Add(time.Hour), Content: "后续 [1]"}, // turn 2: [1] means beta-1
	}
	// handleOrder is what production supplies: every handle-producing tool result
	// is persisted as a role='tool' agent_message BEFORE its turn's assistant
	// reply, so tool-row id < reply id answers "did this exist yet" without a tie.
	// msg_u1_1 (alpha, turn 1) minted at id 5 < reply 10;
	// msg_u1_2 (beta,  turn 2) minted at id 15 > reply 10.
	handleOrder := map[string]int64{"msg_u1_1": 5, "msg_u1_2": 15}
	got, dropped := remapFinalizeCitations(replies, rows, finalPool, handleOrder)

	// alpha-1 index in the merged pool
	var alpha1 int
	for _, m := range finalPool {
		if m.ChannelID == "alpha" && m.MessageSeq == 1 {
			alpha1 = m.CitationIndex
		}
	}
	if alpha1 == 1 {
		t.Fatal("precondition failed: beta must sort before alpha in the merged pool")
	}
	want := "[" + strconv.Itoa(alpha1) + "]"
	if !strings.Contains(got[0].Content, want) {
		t.Errorf("turn-1 marker did not resolve to alpha-1 %s: %q (dropped=%d) — with a same-second created_at bound it silently means beta-1",
			want, got[0].Content, dropped)
	}
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0 — the id axis resolves this exactly, no degrade needed", dropped)
	}
}

// The fallback half of the same fix: when a handle has NO tool row (orphan
// evidence) the tie is genuinely unresolvable. The fragment then degrades — it
// loses its markers and says so — instead of shipping a marker that looks valid
// and points at the wrong message. Degradation is PER FRAGMENT: the other
// fragment keeps its citations.
func TestRemapFinalizeCitations_UnresolvableTieDegradesOneFragmentOnly(t *testing.T) {
	sec := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	alpha := []pipeline.Message{
		poolMsg("alpha", 1, 1000, "alpha-1"),
		poolMsg("alpha", 2, 1001, "alpha-2"),
	}
	beta := []pipeline.Message{poolMsg("beta", 1, 10, "beta-1")}
	rows := []model.AgentMessageEvidence{
		evidenceRow(t, "msg_u1_1", sec, alpha),
		evidenceRow(t, "msg_u1_orphan", sec, beta), // no tool row: unplaceable
	}
	finalPool := buildPoolFromEvidenceRows(rows)
	replies := []model.AgentMessage{
		{ID: 10, CreatedAt: sec, Content: "确认了 [1] 项"},
		{ID: 20, CreatedAt: sec.Add(time.Hour), Content: "后续 [1]"},
	}
	// The session HAS ids (so a missing one is anomalous), but not for the orphan.
	handleOrder := map[string]int64{"msg_u1_1": 5}

	got, dropped := remapFinalizeCitations(replies, rows, finalPool, handleOrder)
	if dropped == 0 {
		t.Errorf("an unresolvable tie that changes the numbering must degrade, got dropped=0: %q", got[0].Content)
	}
	if strings.Contains(got[0].Content, "[1]") {
		t.Errorf("ambiguous fragment kept a guessed marker: %q", got[0].Content)
	}
	if !strings.Contains(got[1].Content, "[") {
		t.Errorf("degradation leaked into the unaffected fragment: %q", got[1].Content)
	}
}
