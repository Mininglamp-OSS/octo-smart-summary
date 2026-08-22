package worker

import (
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/citation"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

// citationRe is the single process-wide citation-marker pattern, now owned by
// internal/citation so the agent Map path (which must not import
// internal/worker) enforces the cap against the very same definition
// buildCitations / dedupCitations / stripOrphanCitations match on. Aliased
// rather than re-declared: two copies of this regexp is exactly how a marker
// one stage deletes becomes a marker another stage still counts.
var citationRe = citation.MarkerRe
var multiSpaceRe = regexp.MustCompile(`[ \t]{2,}`)
var emptyLineRe = regexp.MustCompile(`(?m)^[ \t]*$\n`)

// extractCitationIndexes extracts all [n] citation indexes from text.
func extractCitationIndexes(text string) []int {
	matches := citationRe.FindAllStringSubmatch(text, -1)
	var indexes []int
	seen := make(map[int]bool)
	for _, m := range matches {
		if n, err := strconv.Atoi(m[1]); err == nil && !seen[n] {
			indexes = append(indexes, n)
			seen[n] = true
		}
	}
	sort.Ints(indexes)
	return indexes
}

// BuildCitations is the exported wrapper of buildCitations.
// Exposed so out-of-package callers can reuse the citation logic.
func BuildCitations(text string, messages []pipeline.Message, allMessages []pipeline.Message, nameMap map[string]string) []model.Citation {
	return buildCitations(text, messages, allMessages, nameMap)
}

// buildCitations builds a citation list from the summary text and original messages.
// Only messages actually referenced in the text are included.

func buildCitations(text string, messages []pipeline.Message, allMessages []pipeline.Message, nameMap map[string]string) []model.Citation {
	indexes := extractCitationIndexes(text)
	if len(indexes) == 0 {
		return []model.Citation{}
	}

	maxIdx := 0
	for _, msg := range messages {
		if msg.CitationIndex > maxIdx {
			maxIdx = msg.CitationIndex
		}
	}
	var validIndexes []int
	for _, idx := range indexes {
		if idx >= 1 && idx <= maxIdx {
			validIndexes = append(validIndexes, idx)
		}
	}
	indexes = validIndexes
	if len(indexes) == 0 {
		return []model.Citation{}
	}

	indexSet := make(map[int]bool, len(indexes))
	for _, idx := range indexes {
		indexSet[idx] = true
	}

	channelMsgMap := buildChannelMessageMap(allMessages)
	seqIndexMap := buildSeqIndexMap(channelMsgMap)

	var citations []model.Citation
	for _, msg := range messages {
		if indexSet[msg.CitationIndex] {
			content := truncateRunes(msg.Content, 200)

			sender := msg.SenderUID
			if nameMap != nil {
				if name, ok := nameMap[msg.SenderUID]; ok && name != "" {
					sender = name
				}
			}

			before, after := findContextFast(msg, channelMsgMap, seqIndexMap, nameMap, 3)

			citations = append(citations, model.Citation{
				Index:         msg.CitationIndex,
				Sender:        sender,
				Content:       content,
				SentAt:        msg.SendTime,
				Source:        msg.SourceName,
				ChannelID:     msg.ChannelID,
				ChannelType:   msg.ChannelType,
				MessageSeq:    msg.MessageSeq,
				ContextBefore: before,
				ContextAfter:  after,
			})
		}
	}
	if citations == nil {
		return []model.Citation{}
	}
	return citations
}

func truncateRunes(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}

func buildChannelMessageMap(allMessages []pipeline.Message) map[string][]pipeline.Message {
	m := make(map[string][]pipeline.Message)
	for _, msg := range allMessages {
		m[msg.ChannelID] = append(m[msg.ChannelID], msg)
	}
	return m
}

func buildSeqIndexMap(channelMsgMap map[string][]pipeline.Message) map[string]map[int64]int {
	result := make(map[string]map[int64]int, len(channelMsgMap))
	for chID, msgs := range channelMsgMap {
		idx := make(map[int64]int, len(msgs))
		for i, m := range msgs {
			idx[m.MessageSeq] = i
		}
		result[chID] = idx
	}
	return result
}

func findContextFast(target pipeline.Message, channelMsgMap map[string][]pipeline.Message, seqIndexMap map[string]map[int64]int, nameMap map[string]string, n int) ([]model.ContextMsg, []model.ContextMsg) {
	channelMsgs, ok := channelMsgMap[target.ChannelID]
	if !ok {
		return nil, nil
	}
	seqIdx, ok := seqIndexMap[target.ChannelID]
	if !ok {
		return nil, nil
	}
	targetIdx, ok := seqIdx[target.MessageSeq]
	if !ok {
		return nil, nil
	}

	var before []model.ContextMsg
	start := targetIdx - n
	if start < 0 {
		start = 0
	}
	for i := start; i < targetIdx; i++ {
		before = append(before, toContextMsg(channelMsgs[i], nameMap))
	}

	var after []model.ContextMsg
	end := targetIdx + n + 1
	if end > len(channelMsgs) {
		end = len(channelMsgs)
	}
	for i := targetIdx + 1; i < end; i++ {
		after = append(after, toContextMsg(channelMsgs[i], nameMap))
	}

	return before, after
}

func findContext(target pipeline.Message, channelMsgMap map[string][]pipeline.Message, nameMap map[string]string, n int) ([]model.ContextMsg, []model.ContextMsg) {
	channelMsgs, ok := channelMsgMap[target.ChannelID]
	if !ok {
		return nil, nil
	}

	targetIdx := -1
	for i, msg := range channelMsgs {
		if msg.MessageSeq == target.MessageSeq {
			targetIdx = i
			break
		}
	}
	if targetIdx < 0 {
		return nil, nil
	}

	var before []model.ContextMsg
	start := targetIdx - n
	if start < 0 {
		start = 0
	}
	for i := start; i < targetIdx; i++ {
		before = append(before, toContextMsg(channelMsgs[i], nameMap))
	}

	var after []model.ContextMsg
	end := targetIdx + n + 1
	if end > len(channelMsgs) {
		end = len(channelMsgs)
	}
	for i := targetIdx + 1; i < end; i++ {
		after = append(after, toContextMsg(channelMsgs[i], nameMap))
	}

	return before, after
}

func toContextMsg(msg pipeline.Message, nameMap map[string]string) model.ContextMsg {
	sender := msg.SenderUID
	if nameMap != nil {
		if name, ok := nameMap[msg.SenderUID]; ok && name != "" {
			sender = name
		}
	}
	return model.ContextMsg{
		Sender:     sender,
		Content:    truncateRunes(msg.Content, 200),
		SentAt:     msg.SendTime,
		MessageSeq: msg.MessageSeq,
	}
}

// dedupCitations merges citations that share the same (sender, content) pair.
// For each group of duplicates, the smallest index is kept as the representative.
// All occurrences of duplicate indexes in text are replaced with the representative,
// and consecutive identical markers (e.g. [1][1][1]) are collapsed to a single one.
func dedupCitations(text string, citations []model.Citation) (string, []model.Citation) {
	if len(citations) == 0 {
		return text, citations
	}

	// Group by (sender, content) — keep the smallest index as representative.
	type key struct{ sender, content string }
	mainIdx := make(map[key]int) // key -> smallest index
	remap := make(map[int]int)   // oldIdx -> mainIdx

	for _, c := range citations {
		k := key{c.Sender, c.Content}
		if existing, ok := mainIdx[k]; ok {
			if c.Index < existing {
				// New one is smaller; remap old main to new.
				remap[existing] = c.Index
				mainIdx[k] = c.Index
			} else {
				remap[c.Index] = existing
			}
		} else {
			mainIdx[k] = c.Index
		}
	}

	newText := text

	if len(remap) > 0 {
		// Replace remapped indexes in text.
		newText = citationRe.ReplaceAllStringFunc(newText, func(match string) string {
			sub := citationRe.FindStringSubmatch(match)
			n, _ := strconv.Atoi(sub[1])
			if target, ok := remap[n]; ok {
				return fmt.Sprintf("[%d]", target)
			}
			return match
		})

		// Collapse consecutive identical markers: [1][1][1] -> [1]
		newText = collapseConsecutiveMarkers(newText)
	}

	// Global dedup: for each [n], keep only the first occurrence.
	// Runs after remap so duplicates created by remap are also caught.
	seen := make(map[string]bool)
	newText = citationRe.ReplaceAllStringFunc(newText, func(match string) string {
		if seen[match] {
			return ""
		}
		seen[match] = true
		return match
	})
	newText = multiSpaceRe.ReplaceAllString(newText, " ")
	newText = emptyLineRe.ReplaceAllString(newText, "")
	newText = strings.TrimSpace(newText)

	if len(remap) == 0 {
		return newText, citations
	}

	// Build deduplicated citation list (only keep non-remapped).
	kept := make(map[int]bool)
	for _, idx := range mainIdx {
		kept[idx] = true
	}
	var result []model.Citation
	for _, c := range citations {
		if kept[c.Index] {
			result = append(result, c)
			delete(kept, c.Index) // avoid duplicates if same index appears twice
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Index < result[j].Index })

	if result == nil {
		return newText, []model.Citation{}
	}
	return newText, result
}

// collapseConsecutiveMarkers collapses runs of identical consecutive citation
// markers (optionally separated by whitespace) into a single occurrence.
// Example: "[1][1][1]" -> "[1]", "[2] [2]" -> "[2]". Different markers are
// preserved: "[1][2][1]" -> "[1][2][1]".
//
// Go's regexp engine (RE2) has no backreferences, so we process manually.
func collapseConsecutiveMarkers(text string) string {
	locs := citationRe.FindAllStringIndex(text, -1)
	if len(locs) < 2 {
		return text
	}

	type span struct{ start, end int }
	var toRemove []span

	for i := 0; i < len(locs); i++ {
		current := text[locs[i][0]:locs[i][1]]
		j := i + 1
		for j < len(locs) {
			// Only allow whitespace between markers.
			between := text[locs[j-1][1]:locs[j][0]]
			if !isOnlyWhitespace(between) {
				break
			}
			next := text[locs[j][0]:locs[j][1]]
			if next != current {
				break
			}
			// Remove whitespace + this duplicate marker.
			toRemove = append(toRemove, span{locs[j-1][1], locs[j][1]})
			j++
		}
		i = j - 1
	}

	if len(toRemove) == 0 {
		return text
	}

	// Apply removals from the end to keep earlier indexes valid.
	result := text
	for k := len(toRemove) - 1; k >= 0; k-- {
		r := toRemove[k]
		result = result[:r.start] + result[r.end:]
	}
	return result
}

func isOnlyWhitespace(s string) bool {
	for _, r := range s {
		if r != ' ' && r != '\t' && r != '\n' && r != '\r' {
			return false
		}
	}
	return true
}

// stripOrphanCitations removes `[n]` markers that have no backing Citation
// row.
//
// Skips non-citable `[n]` forms via citation.IsCitableAt, the same guard
// CapRuns applies. Without it this function undid exactly the damage that
// guard exists to prevent, one pipeline stage later:
//
//	in:  结论一[1][2]，详见 [999](https://example.com/doc)
//	out: 结论一[1][2]，详见 (https://example.com/doc)     <- link destroyed
//
// A markdown link whose text happens to be a number is not a citation, has no
// Citation row by construction, and so was deleted from every summary that
// contained one. Pre-existing, not introduced by the cap — but a guard that
// protects one of the two paths that need it is not a guard.
func stripOrphanCitations(text string, citations []model.Citation) string {
	validSet := make(map[int]bool)
	for _, c := range citations {
		validSet[c.Index] = true
	}
	var b strings.Builder
	b.Grow(len(text))
	prev := 0
	for _, m := range citationRe.FindAllStringSubmatchIndex(text, -1) {
		if !citation.IsCitableAt(text, m[0], m[1]) {
			continue
		}
		n, err := strconv.Atoi(text[m[2]:m[3]])
		if err != nil || validSet[n] {
			continue
		}
		b.WriteString(text[prev:m[0]])
		prev = m[1]
	}
	b.WriteString(text[prev:])
	return strings.TrimSpace(multiSpaceRe.ReplaceAllString(b.String(), " "))
}

// finalizeCitations is the ONE definition of the worker's final citation
// pipeline: derive rows, dedup, strip orphans, then cap. It exists as a
// function so a test cannot exercise a different order than production does —
// the previous test stopped at buildCitations, and that one-call gap between
// the test's order and production's order hid a default-on regression for a
// whole review round.
//
// maxCites <= 0 disables the cap (citation.Disabled), in which case this is
// byte-for-byte the pre-cap pipeline.
//
// # Why the cap runs LAST
//
// dedupCitations ends with a whole-document dedup: for each [n] it keeps only
// the FIRST occurrence anywhere in the body and deletes every later repeat.
// citation.CapRuns keeps the HEAD of each run. Head-keeping is the worst
// possible survivor policy against a first-occurrence global dedup — the
// markers the cap preserves are precisely the ones an earlier claim has
// already consumed, and the tail markers it deletes are the ones that would
// have survived the dedup.
//
// Two claims citing the same source is not a corner case. It is what a real
// summary looks like: one decisive message supports several conclusions.
// Capping BEFORE the dedup therefore stripped claims down to zero citations:
//
//	in:          结论一：范围已确认[1][2][3]
//	             结论二：负责人已定[1][2][3][10][11][12]
//	cap off:     结论二：负责人已定[10][11][12]     <- cited
//	cap=3 first: 结论二：负责人已定                 <- ZERO citations
//
// The cap destroyed a citation that exists without the cap. That violates
// citation.CapRuns' own stated invariant ("an uncited claim is worse than an
// over-cited one" — true inside CapRuns, false two calls later), and the Map
// prompt's "没有引用的结论不允许输出".
//
// # Why capping last does not leave orphan Citation rows
//
// That concern is real and is what put the cap first to begin with. It is
// handled by RE-DERIVING: buildCitations is a pure function of (text,
// messages, allMessages, nameMap) — it reads no state and mutates nothing —
// so running it again over the capped body returns exactly the rows whose
// markers are still present.
//
// The subtle part is dedupCitations' remap, which rewrites marker NUMBERS
// when two citations share (sender, content). Re-deriving after it is still
// correct because the remap has already been applied to the body: the
// remapped-away numbers are gone from the text, so buildCitations never sees
// them and cannot resurrect a row for one. TestCapLastSurvivesCitationRemap
// pins this by asserting the re-derived rows equal dedupCitations' rows minus
// exactly what the cap removed.
//
// And the cap only ever DELETES markers, never adds or renumbers, so it
// cannot introduce a marker with no backing row either.
//
// # What this does NOT bound
//
// It does not bound what a STREAMING client sees first. Deltas are emitted
// straight from the model (CallMapStream / CallReduceStream); every stage in
// this function runs after the last delta. The live view is reconciled by
// publishing the final body as a streaming snapshot before Done — see
// finishStreamDone in personal_processor.go.
func finalizeCitations(
	text string,
	userMessages []pipeline.Message,
	allMessages []pipeline.Message,
	nameMap map[string]string,
	maxCites int,
) (string, []model.Citation) {
	citations := buildCitations(text, userMessages, allMessages, nameMap)
	text, citations = dedupCitations(text, citations)
	text = stripOrphanCitations(text, citations)

	if maxCites <= 0 {
		return text, citations
	}

	capped, st := citation.CapRuns(text, maxCites)
	if !st.Changed() {
		// Nothing removed: the body and rows are already consistent, and
		// skipping the re-derive keeps the no-op path allocation-free and
		// provably identical to the disabled path.
		return text, citations
	}
	log.Printf("[personal-worker] citation cap max=%d runs=%d capped_runs=%d markers=%d->%d dedup=%d cap=%d longest_run=%d->%d marks (%d->%d chars) bytes=%d->%d",
		maxCites, st.Runs, st.CappedRuns, st.MarkersBefore, st.MarkersAfter,
		st.RemovedByDedup, st.RemovedByCap,
		st.LongestRunBefore, st.LongestRunAfter,
		st.LongestRunCharsBefore, st.LongestRunCharsAfter,
		st.BytesBefore, st.BytesAfter)

	return capped, buildCitations(capped, userMessages, allMessages, nameMap)
}
