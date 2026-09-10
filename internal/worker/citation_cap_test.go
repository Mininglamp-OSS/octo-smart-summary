package worker

import (
	"fmt"
	"reflect"
	"strconv"
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

func TestCollapseConsecutiveMarkersAcceptsUnicodeWhitespace(t *testing.T) {
	for _, in := range []string{"[7]　[7]", "[7]\u00a0[7]"} {
		if got := collapseConsecutiveMarkers(in); got != "[7]" {
			t.Errorf("collapseConsecutiveMarkers(%q) = %q, want [7]", in, got)
		}
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

// P2-2. citation.isCitable exempts markdown links and reference definitions
// from the cap, but stripOrphanCitations ran ~15 lines later with a bare
// citationRe and no guard, doing exactly the damage the guard exists to
// prevent. Pre-existing, not a cap regression — fixed because a guard that
// protects one of the two paths that need it is not a guard.
func TestStripOrphanCitationsPreservesMarkdownLinks(t *testing.T) {
	cases := []struct {
		name string
		in   string
		rows []model.Citation
		want string
	}{
		{
			name: "inline link with a numeric label",
			in:   "结论一[1][2]，详见 [999](https://example.com/doc)",
			rows: []model.Citation{{Index: 1}, {Index: 2}},
			want: "结论一[1][2]，详见 [999](https://example.com/doc)",
		},
		{
			name: "reference definition",
			in:   "结论一[1]\n\n[999]: https://example.com/doc",
			rows: []model.Citation{{Index: 1}},
			want: "结论一[1]\n\n[999]: https://example.com/doc",
		},
		{
			name: "genuine orphans are still stripped",
			in:   "结论一[1][2][99]",
			rows: []model.Citation{{Index: 1}, {Index: 2}},
			want: "结论一[1][2]",
		},
		{
			name: "orphan stripped, link preserved, in one body",
			in:   "结论一[1][99]，详见 [42](https://example.com/x)",
			rows: []model.Citation{{Index: 1}},
			want: "结论一[1]，详见 [42](https://example.com/x)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripOrphanCitations(tc.in, tc.rows); got != tc.want {
				t.Errorf("stripOrphanCitations:\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// Mutation evidence for P2-2: the pre-fix implementation, reproduced inline,
// must destroy the link.
func TestMutationUnguardedStripDestroysMarkdownLinks(t *testing.T) {
	const in = "结论一[1][2]，详见 [999](https://example.com/doc)"
	rows := []model.Citation{{Index: 1}, {Index: 2}}

	preFix := func(text string, citations []model.Citation) string {
		valid := map[int]bool{}
		for _, c := range citations {
			valid[c.Index] = true
		}
		out := citationRe.ReplaceAllStringFunc(text, func(match string) string {
			sub := citationRe.FindStringSubmatch(match)
			n, _ := strconv.Atoi(sub[1])
			if valid[n] {
				return match
			}
			return ""
		})
		return strings.TrimSpace(multiSpaceRe.ReplaceAllString(out, " "))
	}

	old := preFix(in, rows)
	if strings.Contains(old, "[999](") {
		t.Fatal("MUTATION CHECK FAILED: the unguarded strip preserved the link, " +
			"so this test does not pin the bug")
	}
	got := stripOrphanCitations(in, rows)
	if !strings.Contains(got, "[999](https://example.com/doc)") {
		t.Fatalf("guarded strip still damaged the link: %q", got)
	}
	t.Logf("MUTATION EVIDENCE: unguarded strip -> %q (link destroyed); guarded strip -> %q", old, got)
}

// dedupCitations must apply the SAME citation.IsCitableAt guard as CapRuns,
// Numbers, stripOrphanCitations and extractCitationIndexes.
//
// It did not, and the global first-occurrence dedup made that far worse than
// the strip-path bug fixed above. A numeric markdown link label occupying the
// FIRST occurrence slot caused the dedup to delete the genuine citation that
// followed it:
//
//	in:  See [1](https://example.com). Conclusion[1]
//	out: See [1](https://example.com). Conclusion      <- citation lost
//
// The body then ends with a backed citation row and zero citable markers, so
// the claim silently loses its provenance — and dedup runs BEFORE strip and
// cap in finalizeCitations, so the damage reaches the persisted body.
func TestDedupCitationsPreservesMarkdownLinks(t *testing.T) {
	cases := []struct {
		name string
		in   string
		rows []model.Citation
		want string
	}{
		{
			name: "link label precedes the genuine citation with the same number",
			in:   "See [1](https://example.com). Conclusion[1]",
			rows: []model.Citation{{Index: 1, Sender: "a", Content: "x"}},
			want: "See [1](https://example.com). Conclusion[1]",
		},
		{
			name: "link label follows the genuine citation with the same number",
			in:   "Conclusion[1]. See [1](https://example.com)",
			rows: []model.Citation{{Index: 1, Sender: "a", Content: "x"}},
			want: "Conclusion[1]. See [1](https://example.com)",
		},
		{
			// The blank line collapses via the pre-existing emptyLineRe pass
			// (pinned by TestDedupCitations_GlobalDuplicate_EmptyLineCleanup),
			// which is unrelated to the guard. What matters here is that the
			// reference definition did not consume the first-occurrence slot
			// and both `[1]` forms survive.
			name: "reference definition does not consume the first-occurrence slot",
			in:   "Conclusion[1]\n\n[1]: https://example.com/doc",
			rows: []model.Citation{{Index: 1, Sender: "a", Content: "x"}},
			want: "Conclusion[1]\n[1]: https://example.com/doc",
		},
		{
			name: "genuine repeats are still deduplicated",
			in:   "结论一[1]，结论二[1]",
			rows: []model.Citation{{Index: 1, Sender: "a", Content: "x"}},
			want: "结论一[1]，结论二",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := dedupCitations(tc.in, tc.rows)
			if got != tc.want {
				t.Errorf("dedupCitations:\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// The remap pass must not renumber a markdown link label. Rewriting
// `[1](url)` to `[2](url)` does not merely mangle syntax — it repoints the
// reader at a different source, which is worse than deleting the link.
func TestDedupCitationsRemapDoesNotRenumberLinkLabels(t *testing.T) {
	const in = "See [1](https://example.com) and [2] plus [1]"
	// Identical (sender, content) => index 2 is remapped onto index 1.
	rows := []model.Citation{
		{Index: 1, Sender: "a", Content: "same"},
		{Index: 2, Sender: "a", Content: "same"},
	}

	got, _ := dedupCitations(in, rows)

	if !strings.Contains(got, "[1](https://example.com)") {
		t.Errorf("remap pass rewrote the markdown link label: %q", got)
	}
	// The genuine markers: [2] remaps to [1] and is the first CITABLE
	// occurrence, so it survives; the later duplicate [1] is deduplicated.
	if want := "See [1](https://example.com) and [1] plus"; got != want {
		t.Errorf("dedupCitations:\n got: %q\nwant: %q", got, want)
	}
	if n := citation.Numbers(got); len(n) != 1 || n[0] != 1 {
		t.Errorf("expected exactly citable marker [1] to survive, got %v in %q", n, got)
	}
}

// collapseConsecutiveMarkers must only collapse CITABLE runs. `[1] [1](url)`
// is a citation followed by a link that happens to share the number, not a
// repeated marker, so neither occurrence may be removed.
func TestCollapseConsecutiveMarkersSkipsLinkLabels(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			name: "citation followed by a same-numbered link label",
			in:   "结论[1] [1](https://example.com)",
			want: "结论[1] [1](https://example.com)",
		},
		{
			name: "link label followed by a same-numbered citation",
			in:   "[1](https://example.com) [1]",
			want: "[1](https://example.com) [1]",
		},
		{
			name: "genuine consecutive repeats still collapse",
			in:   "结论[1][1][1]",
			want: "结论[1]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := collapseConsecutiveMarkers(tc.in); got != tc.want {
				t.Errorf("collapseConsecutiveMarkers:\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// Mutation evidence: the pre-fix dedup, reproduced inline, must destroy the
// genuine citation. Without this the tests above could pass against an
// implementation that never had the bug.
func TestMutationUnguardedDedupEatsGenuineCitation(t *testing.T) {
	const in = "See [1](https://example.com). Conclusion[1]"

	// Pre-fix global dedup: keyed on the raw match, no IsCitableAt guard.
	preFix := func(text string) string {
		seen := map[string]bool{}
		return citationRe.ReplaceAllStringFunc(text, func(match string) string {
			if seen[match] {
				return ""
			}
			seen[match] = true
			return match
		})
	}

	old := preFix(in)
	if len(citation.Numbers(old)) != 0 {
		t.Fatalf("MUTATION CHECK FAILED: the unguarded dedup kept a citable marker "+
			"in %q, so this test does not pin the bug", old)
	}

	got, _ := dedupCitations(in, []model.Citation{{Index: 1, Sender: "a", Content: "x"}})
	if n := citation.Numbers(got); len(n) != 1 || n[0] != 1 {
		t.Fatalf("guarded dedup still lost the citation: %q (citable=%v)", got, n)
	}
	t.Logf("MUTATION EVIDENCE: unguarded dedup -> %q (citable markers %v); guarded dedup -> %q (citable markers %v)",
		old, citation.Numbers(old), got, citation.Numbers(got))
}
