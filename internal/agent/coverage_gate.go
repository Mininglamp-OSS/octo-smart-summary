package agent

// SS-12-b — pre-freeze coverage gate.
//
// WHY THIS LIVES HERE AND NOT AROUND THE RUN
//
// SS-05 freezes the run's citation manifest on the FIRST summarize_chunk call,
// and applyFrozenManifest is explicit that "messages fetched AFTER the freeze
// are not in the manifest and are dropped from the citable pool". That single
// fact decides where a coverage check can possibly work:
//
//   - AFTER the answer (the loop this replaces): a repair fetch is post-freeze
//     by construction. Its messages all miss the manifest, assignCitationIndexes
//     drops them, summarize_chunk reports 无可总结内容 + dropped_count, and the
//     model dutifully tells the user that channel was empty. The gap is not
//     closed, it is replaced by a false statement — and by a vaguer verdict,
//     because the channel is now in succeeded_channels so the gate's precise
//     "expected channel was not fetched" becomes a dropped-messages gap.
//   - BEFORE the freeze (here): the missing fetch is still free. Everything the
//     planner fetches in response is in the pool the manifest is frozen FROM, so
//     the repaired channel's messages are citable — which is the entire point.
//
// So the gate does not weaken the freeze-once invariant; it only chooses a
// better MOMENT to freeze. The mechanism is SS-07b's structured tool error: a
// retryable, non-fatal envelope naming what is missing, which the SS-12 §5
// prompt already teaches the planner to act on ("retryable → fix the args and
// retry the same tool").

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/artifact"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/finishgate"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/summaryrun"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/summaryspec"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// ToolChannelType maps the Spec/UI channel type string to the INTEGER
// fetch_channel actually declares (1=DM, 2=Group, 5=Thread). 0 means "not
// representable" — the caller must not print it as an argument value.
//
// This is the ONE definition. The handler's selected-channels prompt calls it
// too (as tool_channel_type=%d); the previous repair prompt shipped the Spec's
// STRING type with "copy it verbatim", which produced {"channel_type":"group"}
// — rejected during argument decoding, BEFORE recordFetch runs, so the channel
// stayed "never attempted" and the next round repeated the same instruction.
// Two spellings of one mapping is how that happens, so there is only one.
func ToolChannelType(chatType string) int {
	switch strings.ToLower(strings.TrimSpace(chatType)) {
	case "direct":
		return 1
	case "group":
		return 2
	case "thread":
		return 5
	default:
		return 0
	}
}

// CoverageGateError is the structured, RETRYABLE, NON-fatal error summarize_chunk
// returns when the run still owes an expected-channel fetch.
//
// It is a typed error rather than a recognisable error string on purpose:
// classifyToolError keys on it with errors.As, so the classification cannot be
// steered by a channel NAME that happens to contain "permission" / "timeout" /
// any other pattern in that switch. summarize_chunk is a criticalTool — a
// misclassification would mark the run FAILED, i.e. would trade "incomplete
// summary" for "no summary".
type CoverageGateError struct {
	// Missing are the expected channels no fetch was ever attempted on.
	Missing []summaryspec.Channel
	// Instruction is the model-facing text (channel ids, integer channel_type,
	// RFC3339 window).
	Instruction string
}

func (e *CoverageGateError) Error() string { return e.Instruction }

// coverageGateTTL bounds how long a run's gate bookkeeping is remembered. It
// exceeds any plausible run wall-clock (StepTimeout 240s × MaxSteps 20) and
// matches the message cache's own TTL, so state is evicted long after the run
// it belongs to is gone but never while that run is still executing.
const coverageGateTTL = 30 * time.Minute

// maxBlocksPerCoverageRound is how many blocked summarize_chunk calls may share
// ONE coverage round.
//
// A round is keyed on what the run had attempted at the time, so a single
// planner step that fans out N parallel summarize_chunk calls sees one identical
// coverage state and must produce ONE round, not N — otherwise a 3-way fan-out
// would exhaust the default 2-round budget in a single step and the gate would
// effectively never fire.
//
// The value is chosen against the runner's step budget, not by taste. A planner
// that re-calls summarize_chunk SERIALLY without ever fetching burns one step
// per block, so the serial worst case is maxRounds × this constant: 2 × 4 = 8,
// inside the summary profile's MaxSteps of 15. It has to be, because exceeding
// MaxSteps makes RunWithHistory return "max steps exceeded" — an error, i.e. NO
// summary. Trading an incomplete summary for no summary is the one thing this
// feature must never do, so the bound protects the step budget first.
//
// A fan-out WIDER than this cap lets the surplus calls through unblocked, and
// one of them freezes. That degrades to exactly the cap-reached behaviour
// (freeze proceeds, finish gate discloses the gap) — weaker enforcement, never a
// broken run.
const maxBlocksPerCoverageRound = 4

type coverageGateState struct {
	// signature identifies the coverage round: the run's attempted+missing sets.
	// It changes exactly when the planner actually fetched something.
	signature string
	// rounds counts DISTINCT signatures blocked so far — the value bounded by
	// SUMMARY_REPAIR_MAX_ROUNDS.
	rounds int
	// blocks counts calls blocked at the current signature (the fan-out width).
	blocks    int
	updatedAt time.Time
}

var (
	coverageGateMu    sync.Mutex
	coverageGateRuns  = map[string]*coverageGateState{}
	coverageGateClock = time.Now // indirected for tests
)

// admitCoverageBlock decides whether this call may be blocked, and books the
// decision. Returns (block, round).
//
// Termination is structural, not hopeful: a NEW signature costs a round and
// rounds are capped at maxRounds; a REPEATED signature costs a slot and slots
// are capped at maxBlocksPerCoverageRound. Once either cap is hit the gate stops
// blocking forever and the freeze proceeds — the run still delivers a summary
// with the gap honestly disclosed by the finish gate. Never "no summary".
func admitCoverageBlock(runID, signature string, maxRounds int) (bool, int) {
	if runID == "" || maxRounds <= 0 {
		return false, 0
	}
	now := coverageGateClock()

	coverageGateMu.Lock()
	defer coverageGateMu.Unlock()
	for id, st := range coverageGateRuns {
		if now.Sub(st.updatedAt) > coverageGateTTL {
			delete(coverageGateRuns, id)
		}
	}

	st, ok := coverageGateRuns[runID]
	if !ok || st.signature != signature {
		prevRounds := 0
		if ok {
			prevRounds = st.rounds
		}
		if prevRounds >= maxRounds {
			return false, prevRounds
		}
		coverageGateRuns[runID] = &coverageGateState{
			signature: signature, rounds: prevRounds + 1, blocks: 1, updatedAt: now,
		}
		return true, prevRounds + 1
	}
	if st.blocks >= maxBlocksPerCoverageRound {
		return false, st.rounds
	}
	st.blocks++
	st.updatedAt = now
	return true, st.rounds
}

// forgetCoverageGateRun drops a run's gate bookkeeping. Test-only hygiene; the
// TTL sweep above is what production relies on.
func forgetCoverageGateRun(runID string) {
	coverageGateMu.Lock()
	defer coverageGateMu.Unlock()
	delete(coverageGateRuns, runID)
}

// checkCoverageBeforeFreeze returns a *CoverageGateError when this run has a
// closed scope whose expected channels were not all attempted yet, and the
// citation manifest has not frozen. Any other situation returns nil.
//
// The cheap gates come first and RETURN BEFORE GetSummaryDeps, which panics when
// unset: flag-off / no run keeps the pre-SS-12 path byte-identical and unit
// tests never trip the panic.
func checkCoverageBeforeFreeze(ctx context.Context, uid, sessionID, runID string) error {
	if !SummaryV2Enabled() || uid == "" || runID == "" {
		return nil
	}
	db, _, _, cfg := GetSummaryDeps()
	if db == nil {
		return nil
	}
	maxRounds := cfg.SummaryRepairMaxRounds
	if maxRounds <= 0 {
		// 0 disables coverage enforcement entirely (the documented kill switch).
		return nil
	}

	runStore := summaryrun.NewStore(db)
	run, err := runStore.GetByID(ctx, uid, runID)
	if err != nil || run == nil {
		return nil
	}
	// Open scope has no authoritative expected list — nothing can be proven
	// missing, so there is nothing to demand.
	if run.ScopePolicy != model.ScopePolicyClosed {
		return nil
	}
	// FetchExpected=false means this turn was never owed a fetch: SS-08b strips
	// the fetch tools from a confident rewrite, and refineFetchExpected persists
	// 0 for a refine turn that will not gather data. The finish gate deliberately
	// skips its own absence audit in that case ("would make PARTIAL the standing
	// verdict for every correct rewrite"); demanding a fetch here would be the
	// same mistake, and worse — it would demand it from a model whose fetch tools
	// were physically removed, which cannot succeed.
	if !run.FetchExpected {
		return nil
	}

	spec, found, err := runStore.GetLatestSpec(ctx, uid, runID)
	if err != nil || !found || len(spec.Channels) == 0 {
		return nil
	}
	expected := make([]string, 0, len(spec.Channels))
	for _, c := range spec.Channels {
		if c.ChannelID != "" {
			expected = append(expected, c.ChannelID)
		}
	}
	attempted := decodeAttemptedChannels(run.AttemptedChannels, runID)
	missingIDs := finishgate.MissingChannels(expected, attempted)
	if len(missingIDs) == 0 {
		return nil
	}

	// Already frozen (a previous summarize_chunk in this run, or an idempotent
	// replay reusing the original run's manifest): the moment this gate protects
	// has passed. Blocking now could only cost turns to fetch messages that the
	// manifest can no longer make citable.
	if _, _, frozen, ferr := artifact.NewStore(db).GetFrozenManifestByRun(ctx, uid, runID); ferr == nil && frozen {
		return nil
	}

	block, round := admitCoverageBlock(runID, coverageSignature(attempted, missingIDs), maxRounds)
	if !block {
		log.Printf("[coverage_gate] run=%s session=%s: %d expected channel(s) still unfetched after %d round(s); allowing the freeze so the run still delivers a summary (gap will be disclosed)",
			runID, sessionID, len(missingIDs), maxRounds)
		return nil
	}

	missing := channelsForIDs(spec.Channels, missingIDs)
	log.Printf("[coverage_gate] run=%s session=%s round=%d/%d: blocking the citation freeze, %d expected channel(s) never fetched",
		runID, sessionID, round, maxRounds, len(missingIDs))
	return &CoverageGateError{
		Missing:     missing,
		Instruction: buildCoverageGateInstruction(missing, spec.TimeRange),
	}
}

// decodeAttemptedChannels reads the run row's attempted_channels JSON — the SAME
// column the finish gate reads at save time, written by RecordChannelFetch inside
// fetch_channel, so it already covers attempted-but-empty channels that no
// message-count check can see.
//
// A decode failure is logged rather than swallowed: silently returning nil makes
// a corrupted run row look like "nothing was ever fetched", which is exactly the
// state that makes this gate block, so the one diagnostic that explains a
// mysterious block must not be missing.
func decodeAttemptedChannels(raw, runID string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var attempted []string
	if err := json.Unmarshal([]byte(raw), &attempted); err != nil {
		log.Printf("[coverage_gate] run=%s: decode attempted_channels failed (%v); treating coverage as unmeasured", runID, err)
		return nil
	}
	return attempted
}

// coverageSignature canonicalises "what the run had covered when we looked".
// Order-independent, because attempted_channels order reflects fetch scheduling,
// not coverage: two parallel summarize_chunk calls in one step must produce the
// SAME signature and therefore cost ONE round.
func coverageSignature(attempted, missing []string) string {
	norm := func(in []string) string {
		out := append([]string(nil), in...)
		sort.Strings(out)
		return strings.Join(out, ",")
	}
	return norm(attempted) + "|" + norm(missing)
}

// channelsForIDs turns MissingChannels' []string back into full Spec channels,
// preserving ids' order, so the instruction can echo each channel's real name and
// type. Unknown ids degrade to a bare ChannelID rather than being dropped — a
// channel we cannot describe is still a channel that must be fetched.
func channelsForIDs(all []summaryspec.Channel, ids []string) []summaryspec.Channel {
	byID := make(map[string]summaryspec.Channel, len(all))
	for _, c := range all {
		byID[c.ChannelID] = c
	}
	out := make([]summaryspec.Channel, 0, len(ids))
	for _, id := range ids {
		if c, ok := byID[id]; ok {
			out = append(out, c)
		} else {
			out = append(out, summaryspec.Channel{ChannelID: id})
		}
	}
	return out
}

// buildCoverageGateInstruction is the model-facing half of the gate.
//
// It hands back every argument fetch_channel needs, in the form fetch_channel
// accepts: an INTEGER channel_type and an RFC3339 window. The commonest reason a
// channel went unfetched in the first place is a bad argument, so re-deriving
// them is how a retry reproduces the original failure. missing must be non-empty.
func buildCoverageGateInstruction(missing []summaryspec.Channel, tr summaryspec.TimeRange) string {
	var b strings.Builder
	b.WriteString("覆盖检查未通过:用户明确选定的以下频道还没有被 fetch_channel 抓取过,必须先抓取,否则本次总结会漏掉它们:\n")
	for _, c := range missing {
		name := c.Name
		if name == "" {
			name = c.ChannelID
		}
		if t := ToolChannelType(c.Type); t > 0 {
			fmt.Fprintf(&b, "  - channel_id=%q channel_type=%d (%s)\n", c.ChannelID, t, name)
		} else {
			// Never print channel_type=0: fetch_channel rejects it, and an
			// instruction the tool refuses is worse than no instruction.
			fmt.Fprintf(&b, "  - channel_id=%q channel_type=<用系统提示里该频道的 tool_channel_type> (%s)\n", c.ChannelID, name)
		}
	}
	b.WriteString("\n请对上面每个频道调用 fetch_channel(channel_type 必须是上面给出的整数,1=私聊 2=群 5=子区)")
	if tr.Start > 0 && tr.End > 0 {
		fmt.Fprintf(&b, ",time_start=%q、time_end=%q",
			time.Unix(tr.Start, 0).Format(time.RFC3339),
			time.Unix(tr.End, 0).Format(time.RFC3339))
	} else {
		b.WriteString(",time_start / time_end 沿用你本次已用的时间窗")
	}
	b.WriteString("。抓完后再重新调用 summarize_chunk。若某个频道确实抓不到(无权限/已删除),说明原因后继续处理其余频道,不要编造内容。")
	return b.String()
}
