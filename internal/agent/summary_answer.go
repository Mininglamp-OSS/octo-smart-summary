package agent

import (
	"log"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/citation"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
)

// CapFinalAnswer enforces the per-claim citation cap on a SUMMARY PROFILE
// planner's final user-facing body. Callers must not apply it to general chat,
// where bracketed expressions are ordinary text rather than citations.
//
// # Why this exists
//
// The argument that justifies capping the worker's final body applies verbatim
// here, and until now it was unaddressed on this path. A Map-only cap is
// guaranteed to be re-exceeded by merging: summarize_chunk caps each chunk
// summary at N, then the planner merges those chunk summaries in its own
// context and "合并相同要点时,合并其引用编号" turns two capped N-marker claims
// into one 2N-marker claim. So the planner's own output is exactly where the
// wall of markers reappears.
//
// Worse, internal/agent/prompts/summary.md told the model "超出上限的标记会在
// 服务端被截断" while NO call site capped the planner's final body. That is a
// prompt promising an enforcement that did not exist. This function is what
// makes the sentence true; the alternative (deleting the claim from the
// prompt) would have left the planner path as the one place a 1026-character
// marker wall can still reach a user.
//
// # What it does NOT do
//
// It does not rebuild citation rows. The agent path has no equivalent of
// worker.buildCitations at this point — the frontend resolves `[n]` against
// the fetched message pool by ordinal — so there is no row/body consistency
// obligation to maintain here, and therefore no re-derivation step. The cap
// only ever deletes markers, so every surviving marker still resolves against
// the same pool it did before.
//
// It runs on MODEL OUTPUT only, never on rendered prompt input. Note that
// unlike the worker path, agent message rendering (renderMessageLine) does not
// escape literal `[12]` in message bodies, so a marker the model copied out of
// a chat message does consume a budget slot here. Nothing is corrupted, but
// see citation.CapRuns' isCitable doc for why that is stated rather than
// silently assumed away.
//
// maxCites <= 0 returns the input byte-identical: the kill switch disables
// this path along with every other.
//
// The persisted assistant message is capped alongside the returned reply, so
// the stored history is what the user was actually shown. Leaving the two
// different would make the next turn's context cite markers the user never
// saw.
func CapFinalAnswer(sessionID, reply string, newMsgs []Message) (string, []Message) {
	maxCites := config.MaxCitationsPerClaim()
	if maxCites < 1 || reply == "" {
		return reply, newMsgs
	}
	capped, st := citation.CapRuns(reply, maxCites)
	if !st.Changed() {
		return reply, newMsgs
	}
	log.Printf("[agent] final-answer citation cap session=%s max=%d runs=%d capped_runs=%d markers=%d->%d dedup=%d cap=%d longest_run=%d->%d marks (%d->%d chars) bytes=%d->%d",
		sessionID, maxCites, st.Runs, st.CappedRuns, st.MarkersBefore, st.MarkersAfter,
		st.RemovedByDedup, st.RemovedByCap,
		st.LongestRunBefore, st.LongestRunAfter,
		st.LongestRunCharsBefore, st.LongestRunCharsAfter,
		st.BytesBefore, st.BytesAfter)

	// Rewrite the assistant turn that produced this reply. Matched on content
	// rather than position: the runner appends tool messages after assistant
	// messages, and only the message whose content IS the reply is the one
	// the user saw.
	for i := len(newMsgs) - 1; i >= 0; i-- {
		if newMsgs[i].Role == "assistant" && newMsgs[i].Content == reply {
			newMsgs[i].Content = capped
			break
		}
	}
	return capped, newMsgs
}
