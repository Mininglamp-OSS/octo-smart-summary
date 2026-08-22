package worker

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/citation"
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

// End-to-end on the worker path: cap the body, then build citations from it.
// The two must agree — every surviving marker resolves, and no Citation row
// exists for a marker the cap removed.
func TestCapThenBuildCitationsStayConsistent(t *testing.T) {
	msgs := makeCapTestMessages(12)
	body := "结论一：范围已确认[1][2][3][4][5][6][7][8]\n结论二：负责人已定[9][10][11][12]"

	capped, st := citation.CapRuns(body, 3)
	if !st.Changed() {
		t.Fatal("test body did not exceed the cap")
	}

	citations := BuildCitations(capped, msgs, msgs, nil)

	present := map[int]bool{}
	for _, n := range citation.Numbers(capped) {
		present[n] = true
	}
	built := map[int]bool{}
	for _, c := range citations {
		built[c.Index] = true
		if !present[c.Index] {
			t.Errorf("Citation row [%d] has no marker in the capped body (orphan row)", c.Index)
		}
	}
	for n := range present {
		if !built[n] {
			t.Errorf("marker [%d] survived the cap but produced no Citation row", n)
		}
	}
	if len(citations) != 6 {
		t.Errorf("got %d citations, want 6 (3 per claim x 2 claims)", len(citations))
	}
	// Both claims must still be cited.
	for i, line := range strings.Split(capped, "\n") {
		if !citationRe.MatchString(line) {
			t.Errorf("claim line %d lost every citation: %q", i, line)
		}
	}
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
