// Package citation owns the `[n]` citation-marker vocabulary shared by the
// summary pipeline: the canonical marker pattern and the per-claim cap that
// bounds how many markers a single claim may carry.
//
// Why a separate package: the marker regexp already existed in
// internal/worker (citationRe), but the cap has to run on the AGENT Map path
// (internal/agent), on the worker Map/final path (internal/worker) and on the
// agent save path (internal/api/handler). internal/agent must not import
// internal/worker, so the shared vocabulary moved down here where all three
// can depend on it. There is still exactly ONE pattern in the process —
// worker.citationRe is now an alias of MarkerRe.
package citation

import (
	"regexp"
	"strconv"
	"strings"
)

// MarkerRe matches a citation marker `[n]`, n = 1..5 digits.
//
// Byte-identical to the pattern that lived at internal/worker/citation.go:14
// since the feature shipped. Deliberately NOT re-derived: buildCitations,
// dedupCitations, stripOrphanCitations and this cap must agree on what a
// marker IS, or a marker one of them deletes is one another still counts.
var MarkerRe = regexp.MustCompile(`\[(\d{1,5})\]`)

// Disabled is the sentinel meaning "no cap". Mirrors the convention the
// sibling knobs in internal/config already use (MAP_MAX_TOKENS,
// SKIP_MAP_REDUCE_THRESHOLD, MAX_MESSAGES_PER_CHANNEL: a non-positive value
// means "not in effect").
const Disabled = 0

// Stats reports what CapRuns did. Every field is a count or a byte size —
// never content — so it is safe to log.
type Stats struct {
	// Runs is how many marker runs were found (see CapRuns for the
	// definition of a run).
	Runs int
	// CappedRuns is how many runs lost at least one DISTINCT marker to the
	// cap. Runs that only lost duplicates are not counted here.
	CappedRuns int
	// MarkersBefore / MarkersAfter count citable markers in the whole text.
	MarkersBefore int
	MarkersAfter  int
	// RemovedByDedup counts markers dropped because the same [n] already
	// appeared earlier in the SAME run. Lossless: the evidence is still cited.
	RemovedByDedup int
	// RemovedByCap counts distinct markers dropped because the run still
	// exceeded the cap after dedup. This is the lossy half.
	RemovedByCap int
	// LongestRunBefore / LongestRunAfter are marker counts, not bytes.
	LongestRunBefore int
	LongestRunAfter  int
	// LongestRunCharsBefore / LongestRunCharsAfter are the byte lengths of the
	// longest unbroken marker run — the "wall of [3][7][12]…" shape that
	// motivated the cap (production peak: 1026 chars).
	LongestRunCharsBefore int
	LongestRunCharsAfter  int
	// BytesBefore / BytesAfter are the sizes of the whole text.
	BytesBefore int
	BytesAfter  int
	// NonCitable counts `[n]` occurrences deliberately left alone because
	// they are syntactically not citations (markdown link / reference heads —
	// see isCitable).
	NonCitable int
}

// Changed reports whether CapRuns actually rewrote the text.
func (s Stats) Changed() bool { return s.RemovedByDedup > 0 || s.RemovedByCap > 0 }

// CapRuns bounds the number of `[n]` markers in each marker RUN to max,
// returning the rewritten text and what it did.
//
// # What a "claim" is
//
// The unit capped here is a maximal run of consecutive markers — `[3][7][12]`,
// tolerating spaces/tabs between them — NOT a sentence, bullet or paragraph.
// Three reasons:
//
//  1. It is exactly the pathological shape production produced: a single
//     1026-character unbroken wall of markers. A paragraph-level cap would
//     have to decide WHICH of a paragraph's runs to shrink; a run-level cap
//     shrinks the wall and leaves a well-behaved paragraph of four
//     three-marker bullets completely untouched.
//  2. It is unambiguous to detect with no natural-language parsing. A
//     sentence splitter over mixed zh/en markdown is a second problem with
//     its own failure modes, and every one of those failures silently
//     deletes evidence.
//  3. It matches how the prompt asks the model to cite: markers are appended
//     at the end of the claim they support, so one run == one claim's
//     evidence list.
//
// A NEWLINE ends a run even though it is whitespace. Markers on two different
// lines belong to two different claims (bullets, list items); merging them
// would let the cap delete the second claim's only citation while the first
// claim's list looks intact. This is stricter than
// worker.collapseConsecutiveMarkers (which does span newlines) and
// deliberately so: that function collapses IDENTICAL markers, which is
// lossless across lines; this one discards distinct markers, which is not.
//
// # Invariants
//
//   - Never drops the last marker of a claim: max >= 1 always keeps the run's
//     first marker, and max < 1 disables capping entirely. An uncited claim
//     is worse than an over-cited one.
//   - Dedup runs first and is free: a repeated `[n]` inside one run carries
//     no information. Only if the run is STILL over the cap after dedup does
//     it lose real evidence, and Stats reports the two separately.
//   - Order is preserved. Surviving markers keep their original relative
//     order so downstream BuildCitations and the frontend citation cards see
//     the numbering the model emitted, minus deletions.
//   - Idempotent: CapRuns(CapRuns(t, n), n) == CapRuns(t, n).
//   - max < 1 (Disabled) returns the input byte-identical. This is the kill
//     switch: setting the env var to 0 restores pre-change behavior exactly,
//     including the absence of dedup. Stats is still populated, so a run with
//     the cap off is still measurable.
//
// # Which markers survive
//
// The first max distinct markers of the run, in emitted order. The prompt
// asks the model to pick the most representative / most recent sources itself
// and to write them in order, so a run that arrives over the cap is one where
// the model ignored the instruction — at which point there is no ranking
// signal left to exploit and the head is the neutral, stable choice. Keeping
// the head also makes the operation idempotent and keeps the earliest
// (chunk-order-first, topically primary) anchor rather than whatever the
// model happened to append last.
func CapRuns(text string, max int) (string, Stats) {
	st := Stats{BytesBefore: len(text), BytesAfter: len(text)}
	runs, citable, nonCitable := findRuns(text)
	st.NonCitable = nonCitable
	st.MarkersBefore = citable
	st.MarkersAfter = citable
	st.Runs = len(runs)
	st.LongestRunBefore, st.LongestRunCharsBefore = longest(runs)
	st.LongestRunAfter, st.LongestRunCharsAfter = st.LongestRunBefore, st.LongestRunCharsBefore

	if max < 1 || citable == 0 {
		return text, st
	}

	// Plan removals. Within a run: keep the first occurrence of each distinct
	// number (dedup), then keep only the first max of those (cap).
	type span struct{ start, end int }
	var remove []span

	for _, r := range runs {
		seen := make(map[string]bool, len(r))
		kept := 0
		capped := false
		for i, m := range r {
			num := text[m[0]:m[1]]
			dup := seen[num]
			seen[num] = true

			switch {
			case dup:
				st.RemovedByDedup++
			case kept >= max:
				st.RemovedByCap++
				capped = true
			default:
				kept++
				continue
			}
			// i > 0 is guaranteed: the first marker of a run is never a
			// duplicate and never exceeds max (max >= 1). So there is always
			// a preceding marker whose trailing gap we absorb along with the
			// marker, and a run can never be emptied.
			remove = append(remove, span{start: r[i-1][1], end: m[1]})
		}
		if capped {
			st.CappedRuns++
		}
	}

	if len(remove) == 0 {
		return text, st
	}

	var b strings.Builder
	b.Grow(len(text))
	prev := 0
	for _, r := range remove {
		b.WriteString(text[prev:r.start])
		prev = r.end
	}
	b.WriteString(text[prev:])
	out := b.String()

	// Re-measure the OUTPUT rather than predicting it. The removal spans
	// absorb the whitespace between markers, so the surviving run's rendered
	// length is not a simple sum of what was kept; measuring the real result
	// also means Stats can never claim a reduction the text did not get.
	outRuns, outCitable, _ := findRuns(out)
	st.BytesAfter = len(out)
	st.MarkersAfter = outCitable
	st.LongestRunAfter, st.LongestRunCharsAfter = longest(outRuns)
	return out, st
}

// findRuns groups the citable markers of text into maximal runs, returning
// the runs (each a slice of [start,end) marker locations), the total number
// of citable markers, and the number of non-citable `[n]` occurrences.
func findRuns(text string) (runs [][][]int, citableCount, nonCitable int) {
	if text == "" {
		return nil, 0, 0
	}
	locs := MarkerRe.FindAllStringIndex(text, -1)
	if len(locs) == 0 {
		return nil, 0, 0
	}
	citable := make([][]int, 0, len(locs))
	for _, l := range locs {
		if isCitable(text, l) {
			citable = append(citable, l)
			continue
		}
		nonCitable++
	}
	if len(citable) == 0 {
		return nil, 0, nonCitable
	}

	cur := [][]int{citable[0]}
	for i := 1; i < len(citable); i++ {
		// A non-citable marker between two citable ones cannot be a run gap:
		// the text between them contains '[', so isRunGap already rejects it.
		if isRunGap(text[citable[i-1][1]:citable[i][0]]) {
			cur = append(cur, citable[i])
			continue
		}
		runs = append(runs, cur)
		cur = [][]int{citable[i]}
	}
	runs = append(runs, cur)
	return runs, len(citable), nonCitable
}

// longest reports the marker count and byte span of the longest run.
func longest(runs [][][]int) (marks, chars int) {
	for _, r := range runs {
		if len(r) > marks {
			marks = len(r)
			chars = r[len(r)-1][1] - r[0][0]
		}
	}
	return marks, chars
}

// isRunGap reports whether the text between two markers keeps them in the
// same run. Spaces and tabs only — a newline separates claims. See CapRuns.
func isRunGap(gap string) bool {
	for i := 0; i < len(gap); i++ {
		if gap[i] != ' ' && gap[i] != '\t' {
			return false
		}
	}
	return true
}

// isCitable reports whether the `[n]` at loc may be treated as a citation
// marker.
//
// This is the escaped/literal-marker guard. Two layers protect bracketed
// numbers that come from MESSAGE BODIES rather than from the citation
// vocabulary:
//
//  1. Upstream, worker.escapeCitationMarkers rewrites `[12]` inside message
//     content to `(12)` before that content is rendered into a prompt, so a
//     body's literal marker never reaches the model as `[12]`. MarkerRe does
//     not match `(12)`, so this function never sees it — and the cap runs on
//     model OUTPUT only, never on rendered prompt input (see the call-site
//     comments in tool_summarize_chunk.go / personal_processor.go).
//  2. Here, syntactic forms where `[n]` is not a citation are excluded:
//     markdown inline links `[1](https://…)` and reference/footnote
//     definitions `[1]: https://…`. Deleting the `[1]` out of `[1](url)`
//     would silently turn a link into a bare parenthesised URL. These are
//     counted (Stats.NonCitable) and left byte-identical.
func isCitable(text string, loc []int) bool {
	if loc[1] >= len(text) {
		return true
	}
	switch text[loc[1]] {
	case '(', ':':
		return false
	}
	return true
}

// Numbers returns the distinct marker numbers in text, in first-appearance
// order. Helper for callers and tests that want marker coverage without
// re-deriving the pattern.
func Numbers(text string) []int {
	var out []int
	seen := make(map[int]bool)
	for _, m := range MarkerRe.FindAllStringSubmatch(text, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}
