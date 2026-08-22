package worker

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/citation"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

// The single-pattern invariant. Two copies of the marker regexp is how a
// marker one stage deletes becomes a marker another stage still counts.
func TestCitationReIsTheSharedPattern(t *testing.T) {
	if citationRe != citation.MarkerRe {
		t.Fatal("worker.citationRe is no longer the shared citation.MarkerRe; " +
			"the cap and buildCitations would be matching different vocabularies")
	}
}

// escapeCitationMarkers is what makes the cap safe to run near message
// bodies: a literal [12] in content becomes (12) before it can be confused
// with a citation. This asserts the property the cap depends on, at the
// definition site.
func TestEscapeCitationMarkersDefeatsTheCap(t *testing.T) {
	body := "参考 [12] 和 [13][14][15][16][17][18] 的说明"
	escaped := escapeCitationMarkers(body)

	if strings.ContainsAny(escaped, "[]") {
		t.Fatalf("escapeCitationMarkers left bracket markers: %q", escaped)
	}
	if want := "参考 (12) 和 (13)(14)(15)(16)(17)(18) 的说明"; escaped != want {
		t.Fatalf("escaped = %q, want %q", escaped, want)
	}

	// The escaped form is invisible to the cap: nothing to count, nothing to
	// cut, byte-identical output even at the tightest cap.
	got, st := citation.CapRuns(escaped, 1)
	if got != escaped {
		t.Errorf("cap modified escaped body text:\n got: %q\nwant: %q", got, escaped)
	}
	if st.MarkersBefore != 0 {
		t.Errorf("cap saw %d markers in fully-escaped body text, want 0", st.MarkersBefore)
	}
}

// A rendered prompt line: the leading [n] is the real citation index, and
// everything after it came from the message body and was escaped. Capping
// such a line must never touch the body.
func TestCapOnRenderedPromptLineTouchesOnlyTheIndex(t *testing.T) {
	line := "[7][2026-08-22 10:00] 张三: " + escapeCitationMarkers("看 [1][2][3][4][5][6]")
	got, st := citation.CapRuns(line, 1)
	if got != line {
		t.Fatalf("cap rewrote a rendered prompt line:\n got: %q\nwant: %q", got, line)
	}
	if st.MarkersBefore != 1 {
		t.Errorf("MarkersBefore = %d, want 1 (the citation index only)", st.MarkersBefore)
	}
}

// End-to-end on the worker path, in the REAL production order. This test
// previously stopped at buildCitations and never called
// dedupCitations/stripOrphanCitations — which is exactly the gap that let a
// default-on regression ship. It now goes through finalizeCitations, the
// single function production itself calls, so the two cannot drift again.
func TestCapThenBuildCitationsStayConsistent(t *testing.T) {
	msgs := makeCapTestMessages(12)
	body := "结论一：范围已确认[1][2][3][4][5][6][7][8]\n结论二：负责人已定[9][10][11][12]"

	out, citations := finalizeCitations(body, msgs, msgs, nil, 3)

	assertBodyAndRowsAgree(t, out, citations)
	assertEveryClaimStillCited(t, body, out)

	if len(citations) != 6 {
		t.Errorf("got %d citations, want 6 (3 per claim x 2 claims)", len(citations))
	}
	for _, line := range strings.Split(out, "\n") {
		if n := len(citation.Numbers(line)); n > 3 {
			t.Errorf("claim line kept %d markers, cap is 3: %q", n, line)
		}
	}
}

// BLOCKING-1 regression. Two claims sharing sources — the shape a real
// summary has, where one decisive message supports several conclusions.
//
// Capping BEFORE dedupCitations stripped claim two to zero citations, because
// CapRuns keeps the HEAD of a run and the global dedup keeps the FIRST
// occurrence in the document: the cap preserved exactly the markers claim one
// had already consumed and deleted exactly the ones that would have survived.
//
// The invariant: enabling the cap must never leave a claim LESS cited than
// the same body with the cap off.
func TestCapNeverStripsAClaimTheUncappedPipelineWouldCite(t *testing.T) {
	msgs := makeCapTestMessages(12)
	body := "结论一：范围已确认[1][2][3]\n结论二：负责人已定[1][2][3][10][11][12]"

	uncapped, uncappedRows := finalizeCitations(body, msgs, msgs, nil, citation.Disabled)
	capped, cappedRows := finalizeCitations(body, msgs, msgs, nil, 3)

	t.Logf("cap OFF: %q (%d rows)", uncapped, len(uncappedRows))
	t.Logf("cap = 3: %q (%d rows)", capped, len(cappedRows))

	// The concrete failure: claim two lost every marker.
	for i, line := range strings.Split(capped, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !citationRe.MatchString(line) {
			t.Errorf("claim line %d has ZERO citations under the cap: %q\n"+
				"uncapped body was: %q", i, line, uncapped)
		}
	}

	// The general invariant, line by line: the cap may shorten a claim's
	// evidence list, never empty one the uncapped pipeline kept.
	off := strings.Split(uncapped, "\n")
	on := strings.Split(capped, "\n")
	if len(off) != len(on) {
		t.Fatalf("line count changed under the cap: %d -> %d", len(off), len(on))
	}
	for i := range off {
		offN, onN := len(citation.Numbers(off[i])), len(citation.Numbers(on[i]))
		if offN > 0 && onN == 0 {
			t.Errorf("line %d: cap off -> %d citations, cap on -> 0. The cap "+
				"destroyed a citation that exists without it.\n  off: %q\n  on:  %q",
				i, offN, off[i], on[i])
		}
	}

	assertBodyAndRowsAgree(t, capped, cappedRows)
}

// Mutation evidence for BLOCKING-1: the OLD order (cap first, then the
// citation pipeline) must FAIL the assertion the new order satisfies. Without
// this, the test above could be passing for an unrelated reason.
func TestMutationCapBeforeDedupProducesUncitedClaims(t *testing.T) {
	msgs := makeCapTestMessages(12)
	body := "结论一：范围已确认[1][2][3]\n结论二：负责人已定[1][2][3][10][11][12]"

	// The pre-fix production order, reproduced inline.
	capFirst := func(text string, maxCites int) (string, []model.Citation) {
		if maxCites > 0 {
			text, _ = citation.CapRuns(text, maxCites)
		}
		cits := buildCitations(text, msgs, msgs, nil)
		text, cits = dedupCitations(text, cits)
		return stripOrphanCitations(text, cits), cits
	}

	uncitedLines := func(text string) int {
		n := 0
		for _, line := range strings.Split(text, "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			if !citationRe.MatchString(line) {
				n++
			}
		}
		return n
	}

	oldBody, oldRows := capFirst(body, 3)
	newBody, newRows := finalizeCitations(body, msgs, msgs, nil, 3)

	if uncitedLines(oldBody) == 0 {
		t.Fatal("MUTATION CHECK FAILED: the pre-fix order left no uncited claim, " +
			"so this test does not actually pin the bug")
	}
	if got := uncitedLines(newBody); got != 0 {
		t.Fatalf("fixed order still left %d uncited claim(s): %q", got, newBody)
	}
	t.Logf("MUTATION EVIDENCE:\n  cap-first (pre-fix): %q  rows=%d  uncited_claims=%d\n"+
		"  cap-last  (fixed):   %q  rows=%d  uncited_claims=%d",
		oldBody, len(oldRows), uncitedLines(oldBody),
		newBody, len(newRows), uncitedLines(newBody))
}

// The orphan-Citation-row concern that originally motivated capping FIRST.
// Re-deriving after the cap is only safe if buildCitations is pure and if
// dedupCitations' index REMAP does not make a second derivation disagree.
// Messages 3 and 7 share (sender, content) here, so remap fires.
func TestCapLastSurvivesCitationRemap(t *testing.T) {
	msgs := makeCapTestMessages(12)
	// Make message 7 a duplicate of message 3 -> dedupCitations remaps 7 -> 3.
	msgs[6].SenderUID = msgs[2].SenderUID
	msgs[6].SenderName = msgs[2].SenderName
	msgs[6].Content = msgs[2].Content

	body := "结论一：范围已确认[1][2][3]\n结论二：负责人已定[7][8][9][10][11][12]"

	// buildCitations must be idempotent over an already-deduped body, or
	// re-deriving after the cap would silently change the rows.
	pre := buildCitations(body, msgs, msgs, nil)
	dedupedBody, dedupedRows := dedupCitations(body, pre)
	dedupedBody = stripOrphanCitations(dedupedBody, dedupedRows)
	rederived := buildCitations(dedupedBody, msgs, msgs, nil)
	if !reflect.DeepEqual(dedupedRows, rederived) {
		t.Fatalf("buildCitations is not idempotent over a deduped body; "+
			"re-deriving after the cap is unsafe.\n  dedup rows: %v\n  re-derived: %v",
			rowIndexes(dedupedRows), rowIndexes(rederived))
	}

	out, rows := finalizeCitations(body, msgs, msgs, nil, 3)
	t.Logf("remap+cap: body=%q rows=%v", out, rowIndexes(rows))

	assertBodyAndRowsAgree(t, out, rows)

	// Every surviving row must be a subset of what the uncapped pipeline
	// produced: the cap removes, it never invents.
	_, uncappedRows := finalizeCitations(body, msgs, msgs, nil, citation.Disabled)
	allowed := map[int]bool{}
	for _, c := range uncappedRows {
		allowed[c.Index] = true
	}
	for _, c := range rows {
		if !allowed[c.Index] {
			t.Errorf("cap introduced Citation row [%d] the uncapped pipeline never had", c.Index)
		}
	}
}

// The kill switch must be exactly the pre-cap pipeline.
func TestDisabledCapIsThePreCapPipelineByteForByte(t *testing.T) {
	msgs := makeCapTestMessages(12)
	body := "结论一：范围已确认[1][2][3][4][5][6][7][8]\n结论二：负责人已定[9][10][11][12]"

	legacy := buildCitations(body, msgs, msgs, nil)
	legacyBody, legacy := dedupCitations(body, legacy)
	legacyBody = stripOrphanCitations(legacyBody, legacy)

	for _, off := range []int{citation.Disabled, 0, -1, -999} {
		gotBody, gotRows := finalizeCitations(body, msgs, msgs, nil, off)
		if gotBody != legacyBody {
			t.Errorf("maxCites=%d body differs from the pre-cap pipeline:\n got: %q\nwant: %q",
				off, gotBody, legacyBody)
		}
		if !reflect.DeepEqual(gotRows, legacy) {
			t.Errorf("maxCites=%d rows differ: %v vs %v", off, rowIndexes(gotRows), rowIndexes(legacy))
		}
	}
}

// assertBodyAndRowsAgree: every marker in the body has a Citation row and
// every Citation row has a marker in the body. This is the property the
// original cap-first ordering existed to protect, so it is asserted on every
// path that reorders anything.
func assertBodyAndRowsAgree(t *testing.T, body string, citations []model.Citation) {
	t.Helper()
	present := map[int]bool{}
	for _, n := range citation.Numbers(body) {
		present[n] = true
	}
	built := map[int]bool{}
	for _, c := range citations {
		built[c.Index] = true
		if !present[c.Index] {
			t.Errorf("Citation row [%d] has no marker in the body (orphan row): %q", c.Index, body)
		}
	}
	for n := range present {
		if !built[n] {
			t.Errorf("marker [%d] is in the body but produced no Citation row: %q", n, body)
		}
	}
}

func assertEveryClaimStillCited(t *testing.T, before, after string) {
	t.Helper()
	for i, line := range strings.Split(after, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !citationRe.MatchString(line) {
			t.Errorf("claim line %d lost every citation: %q (input was %q)", i, line, before)
		}
	}
}

func rowIndexes(cs []model.Citation) []int {
	out := make([]int, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Index)
	}
	return out
}

// Mutation evidence at the worker layer: with the cap disabled, the same
// assertion the enabled path satisfies must fail.
func TestWorkerCapMutationEvidence(t *testing.T) {
	body := "结论[1][2][3][4][5][6][7][8][9][10]"

	overCap := func(text string, limit int) int {
		n := 0
		for _, line := range strings.Split(text, "\n") {
			if c := len(citation.Numbers(line)); c > limit {
				n++
			}
		}
		return n
	}

	enabled, _ := citation.CapRuns(body, 3)
	if v := overCap(enabled, 3); v != 0 {
		t.Fatalf("cap=3 left %d over-cap claims — enforcement broken", v)
	}

	disabled, _ := citation.CapRuns(body, citation.Disabled)
	if v := overCap(disabled, 3); v == 0 {
		t.Fatal("MUTATION CHECK FAILED: with the cap disabled the assertion still passed, " +
			"so it does not actually test the cap")
	} else {
		t.Logf("MUTATION EVIDENCE: cap disabled -> %d claim(s) exceed the 3-marker contract; cap=3 -> 0", v)
	}
}

// makeCapTestMessages builds n messages with CitationIndex 1..n.
func makeCapTestMessages(n int) []pipeline.Message {
	msgs := make([]pipeline.Message, 0, n)
	for i := 1; i <= n; i++ {
		msgs = append(msgs, pipeline.Message{
			MessageSeq:    int64(i),
			SenderUID:     fmt.Sprintf("u%d", i),
			SenderName:    fmt.Sprintf("用户%d", i),
			ChannelID:     "ch-1",
			ChannelType:   2,
			Timestamp:     int64(1000 + i),
			SendTime:      "2026-08-22 10:00:00",
			Content:       fmt.Sprintf("第 %d 条消息内容", i),
			CitationIndex: i,
		})
	}
	return msgs
}
