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

// coverageGateTTL bounds how long a run's process-local gate bookkeeping is
// remembered. It matches the message cache TTL: after this window the handles
// needed to retry summarize_chunk are stale too. This is a best-effort
// per-process bound, not a durable per-run guarantee across restarts.
const coverageGateTTL = 30 * time.Minute

// coverageRepairReservedSteps is the downstream budget after all possible
// coverage rounds: retry Map, run Reduce, tolerate one correction/nudge, and
// emit the final answer. Each still-available coverage round additionally
// reserves two planner steps (fetch + retried summarize). This prevents the
// gate's own bounded repair overhead from pushing an otherwise deliverable
// PARTIAL summary into the runner's hard step ceiling.
const coverageRepairReservedSteps = 4

type coverageStepContext struct {
	step     int
	maxSteps int
}

type contextKeyCoverageStep struct{}

func withCoverageGateStep(ctx context.Context, step, maxSteps int) context.Context {
	return context.WithValue(ctx, contextKeyCoverageStep{}, coverageStepContext{step: step, maxSteps: maxSteps})
}

func coverageGateStep(ctx context.Context) coverageStepContext {
	if v, ok := ctx.Value(contextKeyCoverageStep{}).(coverageStepContext); ok && v.step > 0 && v.maxSteps > 0 {
		return v
	}
	// Direct tool-handler tests and non-runner callers do not carry step data.
	// Give them the normal summary profile budget; production Runner paths always
	// inject the real values above.
	return coverageStepContext{step: 1, maxSteps: profiles["summary"].Policy.MaxSteps}
}

type coverageGateState struct {
	// signature identifies the coverage round: the run's attempted+missing sets.
	// It changes exactly when the planner actually fetched something.
	signature string
	// rounds counts DISTINCT signatures blocked so far — the value bounded by
	// SUMMARY_REPAIR_MAX_ROUNDS.
	rounds int
	// decisionStep is the 1-indexed planner step for which block/reason was
	// decided. Every concurrent call from that step reuses the same decision.
	decisionStep int
	block        bool
	reason       string
	satisfied    bool
	updatedAt    time.Time
}

var (
	coverageGateMu    sync.Mutex
	coverageGateRuns  = map[string]*coverageGateState{}
	coverageGateClock = time.Now // indirected for tests
)

func sweepCoverageGateLocked(now time.Time) {
	for id, st := range coverageGateRuns {
		if now.Sub(st.updatedAt) > coverageGateTTL {
			delete(coverageGateRuns, id)
		}
	}
}

// admitCoverageBlock makes one decision per planner step. All summarize_chunk
// calls fanned out by that step reuse it, so no concurrency width can bypass the
// gate. A later step with an unchanged signature means the planner made no fetch
// progress; it is allowed through immediately instead of burning another step.
// A changed signature may consume another configured round, provided enough
// steps remain for fetch + Map + Reduce + final answer.
func admitCoverageBlock(runID, signature string, step, maxSteps, maxRounds int) (bool, int, string) {
	if runID == "" || maxRounds <= 0 {
		return false, 0, "disabled"
	}
	coverageGateMu.Lock()
	defer coverageGateMu.Unlock()
	now := coverageGateClock()
	sweepCoverageGateLocked(now)

	st, ok := coverageGateRuns[runID]
	if ok && st.satisfied {
		st.updatedAt = now
		return false, st.rounds, "coverage_satisfied"
	}
	if ok && st.decisionStep == step {
		st.updatedAt = now
		return st.block, st.rounds, st.reason
	}

	prevRounds := 0
	if ok {
		prevRounds = st.rounds
		if st.signature == signature {
			st.decisionStep = step
			st.block = false
			st.reason = "no_coverage_progress"
			st.updatedAt = now
			return false, st.rounds, st.reason
		}
	}

	reason := "blocked"
	block := true
	if prevRounds >= maxRounds {
		block = false
		reason = "round_budget_exhausted"
	} else if maxSteps-step < coverageRepairReservedSteps+2*(maxRounds-prevRounds) {
		block = false
		reason = "step_budget_reserved"
	}
	if block {
		prevRounds++
	}
	coverageGateRuns[runID] = &coverageGateState{
		signature: signature, rounds: prevRounds, decisionStep: step,
		block: block, reason: reason, updatedAt: now,
	}
	return block, prevRounds, reason
}

func coverageGateSatisfied(runID string) bool {
	coverageGateMu.Lock()
	defer coverageGateMu.Unlock()
	now := coverageGateClock()
	sweepCoverageGateLocked(now)
	st := coverageGateRuns[runID]
	if st == nil || !st.satisfied {
		return false
	}
	st.updatedAt = now
	return true
}

func markCoverageGateSatisfied(runID string) {
	coverageGateMu.Lock()
	defer coverageGateMu.Unlock()
	now := coverageGateClock()
	sweepCoverageGateLocked(now)
	st := coverageGateRuns[runID]
	if st == nil {
		st = &coverageGateState{}
		coverageGateRuns[runID] = st
	}
	st.satisfied = true
	st.block = false
	st.reason = "coverage_satisfied"
	st.updatedAt = now
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
	if coverageGateSatisfied(runID) {
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
	if err != nil {
		log.Printf("[coverage_gate] run=%s session=%s: load run failed: %v", runID, sessionID, err)
		return nil
	}
	if run == nil {
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
		markCoverageGateSatisfied(runID)
		return nil
	}

	spec, found, err := runStore.GetLatestSpec(ctx, uid, runID)
	if err != nil {
		log.Printf("[coverage_gate] run=%s session=%s: load latest spec failed: %v", runID, sessionID, err)
		return nil
	}
	declared := decodeAttemptedChannels(run.DiscoveredChannels, runID)
	expected := declared
	if len(expected) == 0 {
		// Without a declaration, only a closed picker scope has an authoritative
		// expected set. Open-scope discovery candidates are not sufficient.
		if run.ScopePolicy != model.ScopePolicyClosed {
			markCoverageGateSatisfied(runID)
			return nil
		}
		if !found || len(spec.Channels) == 0 {
			return nil
		}
		expected = make([]string, 0, len(spec.Channels))
		for _, c := range spec.Channels {
			if c.ChannelID != "" {
				expected = append(expected, c.ChannelID)
			}
		}
	}
	attempted := decodeAttemptedChannels(run.AttemptedChannels, runID)
	missingIDs := finishgate.MissingChannels(expected, attempted)
	if len(missingIDs) == 0 {
		markCoverageGateSatisfied(runID)
		return nil
	}

	// Already frozen (a previous summarize_chunk in this run, or an idempotent
	// replay reusing the original run's manifest): the moment this gate protects
	// has passed. Blocking now could only cost turns to fetch messages that the
	// manifest can no longer make citable.
	if _, _, frozen, ferr := artifact.NewStore(db).GetFrozenManifestByRun(ctx, uid, runID); ferr != nil {
		log.Printf("[coverage_gate] run=%s session=%s: read frozen manifest failed: %v", runID, sessionID, ferr)
		// Fail open, consistent with the run/spec reads above. The manifest may
		// already be frozen; blocking on an uncertain read could make the planner
		// fetch messages that applyFrozenManifest must then drop. The finish gate
		// remains the honest disclosure fallback.
		return nil
	} else if frozen {
		markCoverageGateSatisfied(runID)
		return nil
	}

	stepInfo := coverageGateStep(ctx)
	block, round, reason := admitCoverageBlock(runID, coverageSignature(attempted, missingIDs), stepInfo.step, stepInfo.maxSteps, maxRounds)
	if !block {
		log.Printf("[coverage_gate] run=%s session=%s step=%d/%d: %d expected channel(s) still unfetched after %d blocked round(s); allowing freeze reason=%s (finish gate will disclose the gap)",
			runID, sessionID, stepInfo.step, stepInfo.maxSteps, len(missingIDs), round, reason)
		return nil
	}

	missing := channelsForIDs(spec.Channels, missingIDs)
	log.Printf("[coverage_gate] run=%s session=%s step=%d/%d round=%d/%d: blocking the citation freeze, %d expected channel(s) never fetched",
		runID, sessionID, stepInfo.step, stepInfo.maxSteps, round, maxRounds, len(missingIDs))
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
			fmt.Fprintf(&b, "  - channel_id=%q (%s; channel_type 请使用系统提示中该频道对应的整数)\n", c.ChannelID, name)
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
