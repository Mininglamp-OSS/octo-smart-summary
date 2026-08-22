package worker

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/citation"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

// sharedPoolBody models the property internal/citation's generator
// structurally CANNOT produce: cross-claim source reuse.
//
// citation.syntheticPathologicalBody inserts each repeated marker near its
// original (`at := src + 1 + rng.Intn(6)`), so repeats land inside the same
// claim's run and two claims almost never share a source. That is why it
// could not surface the bug where the cap plus the global dedup strips a
// claim to zero citations — that bug REQUIRES two claims citing the same
// message.
//
// This generator draws every claim's evidence from one shared pool of
// `poolSize` messages, which is what a real summary looks like: a handful of
// decisive messages support many conclusions. Sorted ascending per claim,
// matching how the prompt asks the model to emit them.
func sharedPoolBody(poolSize, claims int) string {
	rng := rand.New(rand.NewSource(int64(poolSize)*1000 + int64(claims)))
	var b strings.Builder
	b.WriteString("# 项目周会总结\n\n## 关键结论\n\n")
	for i := 1; i <= claims; i++ {
		n := 4 + rng.Intn(13) // 4..16 sources per claim
		picked := map[int]bool{}
		for len(picked) < n && len(picked) < poolSize {
			picked[1+rng.Intn(poolSize)] = true
		}
		ordered := make([]int, 0, len(picked))
		for k := range picked {
			ordered = append(ordered, k)
		}
		// ascending, as the prompt asks
		for a := 0; a < len(ordered); a++ {
			for c := a + 1; c < len(ordered); c++ {
				if ordered[c] < ordered[a] {
					ordered[a], ordered[c] = ordered[c], ordered[a]
				}
			}
		}
		fmt.Fprintf(&b, "- 讨论要点%d：明确了范围与负责人", i)
		for _, m := range ordered {
			fmt.Fprintf(&b, "[%d]", m)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func poolMessages(n int) []pipeline.Message {
	msgs := make([]pipeline.Message, 0, n)
	for i := 1; i <= n; i++ {
		msgs = append(msgs, pipeline.Message{
			MessageSeq:    int64(i),
			SenderUID:     fmt.Sprintf("u%d", i%17),
			SenderName:    fmt.Sprintf("用户%d", i%17),
			ChannelID:     "ch-1",
			ChannelType:   2,
			Timestamp:     int64(1000 + i),
			SendTime:      "2026-08-22 10:00:00",
			Content:       fmt.Sprintf("第 %d 条消息内容，讨论了具体的范围与排期", i),
			CitationIndex: i,
		})
	}
	return msgs
}

func uncitedClaimCount(text string) (uncited, claims int) {
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "- 讨论要点") {
			continue
		}
		claims++
		if len(citation.Numbers(line)) == 0 {
			uncited++
		}
	}
	return uncited, claims
}

// TestMeasuredUncitedClaims is the quality metric the byte counts do not
// capture: how many claims end up with ZERO citations. The cap can regress
// this — that is the whole of BLOCKING-1 — so it belongs next to the byte
// counts, measured through the REAL three-stage pipeline.
//
// Run with -v to see the table.
func TestMeasuredUncitedClaims(t *testing.T) {
	const claims = 110
	msgs := poolMessages(400)

	t.Log("uncited claims / total, through the real finalizeCitations pipeline:")
	t.Log("pool | cap OFF |   cap=5 |   cap=3 |   cap=1 | bytes OFF -> cap=5 / cap=3")

	for _, pool := range []int{400, 200, 100, 60} {
		body := sharedPoolBody(pool, claims)
		row := fmt.Sprintf("%4d |", pool)
		var offBytes, b5, b3 int
		for _, max := range []int{citation.Disabled, 5, 3, 1} {
			out, _ := finalizeCitations(body, msgs, msgs, nil, max)
			uncited, total := uncitedClaimCount(out)
			row += fmt.Sprintf(" %3d/%3d |", uncited, total)
			switch max {
			case citation.Disabled:
				offBytes = len(out)
			case 5:
				b5 = len(out)
			case 3:
				b3 = len(out)
			}
		}
		t.Logf("%s %d -> %d / %d", row, offBytes, b5, b3)
	}

	// The invariant, asserted as EQUALITY rather than "no worse".
	//
	// After the reordering the cap cannot change citation COVERAGE at all,
	// and that is a proof, not a measurement: the cap now runs on a body the
	// global dedup has already reduced to first-occurrences, so every marker
	// in a surviving run is distinct and appears for the first time in the
	// document. CapRuns always keeps a run's head. Therefore a claim that had
	// a marker before the cap has one after, at EVERY cap value.
	//
	// This is what the pre-fix ordering destroyed, and it is why the choice
	// of default is now purely a bytes-vs-verifiability tradeoff and no
	// longer a citation-quality risk.
	for _, pool := range []int{400, 200, 100, 60} {
		body := sharedPoolBody(pool, claims)
		offOut, _ := finalizeCitations(body, msgs, msgs, nil, citation.Disabled)
		offUncited, _ := uncitedClaimCount(offOut)
		for _, max := range []int{1, 2, 3, 5, 8, 16} {
			onOut, _ := finalizeCitations(body, msgs, msgs, nil, max)
			onUncited, _ := uncitedClaimCount(onOut)
			if onUncited != offUncited {
				t.Errorf("pool=%d cap=%d: %d uncited claims vs %d with the cap OFF — "+
					"the cap must not change citation coverage in either direction",
					pool, max, onUncited, offUncited)
			}
		}
	}
}

// Mutation evidence for the measurement itself: run the same table through
// the PRE-FIX order (cap first) and it must violate the invariant above.
func TestMutationCapFirstRegressesUncitedClaims(t *testing.T) {
	const claims = 110
	msgs := poolMessages(400)

	capFirst := func(text string, max int) string {
		if max > 0 {
			text, _ = citation.CapRuns(text, max)
		}
		cits := buildCitations(text, msgs, msgs, nil)
		text, cits = dedupCitations(text, cits)
		return stripOrphanCitations(text, cits)
	}

	t.Log("PRE-FIX (cap before dedup) vs FIXED (cap after dedup), uncited claims:")
	t.Log("pool | cap OFF | pre-fix c=5 | fixed c=5 | pre-fix c=3 | fixed c=3")

	regressions := 0
	for _, pool := range []int{400, 200, 100, 60} {
		body := sharedPoolBody(pool, claims)
		offUncited, _ := uncitedClaimCount(capFirst(body, citation.Disabled))

		row := fmt.Sprintf("%4d |     %3d |", pool, offUncited)
		for _, max := range []int{5, 3} {
			oldU, _ := uncitedClaimCount(capFirst(body, max))
			newOut, _ := finalizeCitations(body, msgs, msgs, nil, max)
			newU, _ := uncitedClaimCount(newOut)
			row += fmt.Sprintf("         %3d |       %3d |", oldU, newU)
			if oldU > offUncited {
				regressions++
			}
			if newU > offUncited {
				t.Errorf("pool=%d cap=%d: the FIXED order still regressed (%d > %d)",
					pool, max, newU, offUncited)
			}
		}
		t.Log(row)
	}

	if regressions == 0 {
		t.Fatal("MUTATION CHECK FAILED: the pre-fix order never regressed uncited-claim " +
			"count on this corpus, so the table does not demonstrate the bug")
	}
	t.Logf("MUTATION EVIDENCE: the pre-fix (cap-first) order regressed uncited-claim "+
		"count vs cap-OFF in %d of 8 measured configurations; the fixed order regresses "+
		"in 0.", regressions)
}
