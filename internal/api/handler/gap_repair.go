package handler

// SS-12-b — bounded self-repair for the agent chat loop.
//
// The finish gate records a coverage gap at SAVE time: by then the run is over
// and the user is looking at a summary that silently missed a channel they
// picked. This closes that loop one step earlier — right after the agent
// answers, while the runner is still available — by diffing the closed-scope
// Spec's expected channels against the channels fetch_channel was actually
// called on, and giving the agent one more round to go get what it missed.
//
// The diff comes from finishgate.MissingChannels, the same function the gate
// uses, so "what the repair loop re-fetches" and "what the verdict calls a gap"
// cannot drift apart.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/finishgate"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/summaryspec"
)

// historyRunner is the minimal runner surface the repair loop needs, so the
// loop is testable without building a real LLM-backed Runner.
type historyRunner interface {
	RunWithHistory(ctx context.Context, system string, history []agent.Message, userMessage string) (string, []agent.Message, error)
}

// runWithRepair runs the agent once, then applies up to SummaryRepairMaxRounds
// repair rounds while an expected channel remains unfetched.
//
// Guardrails, in the order they fire:
//
//   - cheap gates first (v2 off / no run_id / open scope / rounds<=0) return
//     before touching summary deps, so the flag-off path and unit tests never
//     enter the loop and never reach GetSummaryDeps (which panics when unset);
//   - a round that does not SHRINK the missing set stops the loop — a channel
//     that stays unreachable must not be retried forever;
//   - MaxRounds is the hard ceiling regardless.
//
// Returns the final reply plus every round's new messages accumulated, so the
// caller persists the whole conversation once, exactly as it does today.
func (h *AgentChatHandler) runWithRepair(
	ctx context.Context,
	runner historyRunner,
	system string,
	history []agent.Message,
	userMsg string,
	uid, runID string,
	closedScope bool,
	emit func(round int, missing []summaryspec.Channel),
) (string, []agent.Message, error) {
	reply, allNew, err := runner.RunWithHistory(ctx, system, history, userMsg)
	if err != nil {
		return reply, allNew, err
	}

	if !agent.SummaryV2Enabled() || runID == "" || !closedScope || h.runStore == nil {
		return reply, allNew, nil
	}
	_, _, _, cfg := agent.GetSummaryDeps()
	maxRounds := cfg.SummaryRepairMaxRounds
	if maxRounds <= 0 {
		return reply, allNew, nil
	}

	spec, ok, serr := h.runStore.GetLatestSpec(ctx, uid, runID)
	if serr != nil || !ok || len(spec.Channels) == 0 {
		return reply, allNew, nil
	}
	expected := make([]string, 0, len(spec.Channels))
	for _, c := range spec.Channels {
		if c.ChannelID != "" {
			expected = append(expected, c.ChannelID)
		}
	}

	prevMissing := -1
	for round := 1; round <= maxRounds; round++ {
		missingIDs := finishgate.MissingChannels(expected, h.attemptedChannels(ctx, uid, runID))
		if len(missingIDs) == 0 {
			break // full coverage — nothing to repair
		}
		if prevMissing >= 0 && len(missingIDs) >= prevMissing {
			// No progress: the previous round asked for these and got nowhere.
			// Retrying cannot help and would burn a full agent round each time.
			log.Printf("[agent] SS-12 repair stopped after round %d: %d channel(s) still unreachable run=%s", round-1, len(missingIDs), runID)
			break
		}
		prevMissing = len(missingIDs)

		missing := channelsForIDs(spec.Channels, missingIDs)
		if emit != nil {
			emit(round, missing)
		}
		log.Printf("[agent] SS-12 repair round %d/%d: %d expected channel(s) unfetched run=%s", round, maxRounds, len(missingIDs), runID)

		// Feed the whole conversation so far (caller history + every round's new
		// messages) so the agent sees its own prior answer and repairs it rather
		// than starting over.
		priorHistory := append(append([]agent.Message(nil), history...), allNew...)
		roundReply, roundNew, rerr := runner.RunWithHistory(ctx, system, priorHistory, buildGapRepairPrompt(missing, spec.TimeRange))
		if rerr != nil {
			// A failed repair round must not lose the good answer we already have:
			// keep the pre-repair reply and messages, drop this round.
			log.Printf("[agent] SS-12 repair round %d failed run=%s: %v; keeping pre-repair answer", round, runID, rerr)
			return reply, allNew, nil
		}
		reply = roundReply
		allNew = append(allNew, roundNew...)
	}

	return reply, allNew, nil
}

// attemptedChannels reads the channels fetch_channel was called on for this run
// from the run row — the same column the finish gate reads at save time.
//
// Deliberately NOT an in-process registry: the SSE and non-SSE entry points, the
// save path and the gate all have to agree on this set, and the run row is the
// one place all of them can see. It is written by RecordChannelFetch inside the
// fetch tool, so it already reflects attempted-but-empty channels — which is
// exactly the case a message-count-based check cannot see.
func (h *AgentChatHandler) attemptedChannels(ctx context.Context, uid, runID string) []string {
	run, err := h.runStore.GetByID(ctx, uid, runID)
	if err != nil || run == nil {
		return nil
	}
	var attempted []string
	if run.AttemptedChannels != "" {
		if uerr := json.Unmarshal([]byte(run.AttemptedChannels), &attempted); uerr != nil {
			return nil
		}
	}
	return attempted
}

// channelsForIDs turns MissingChannels' []string back into full Spec channels,
// preserving ids' order, so the repair prompt can echo each channel's real name
// and type. Unknown ids degrade to a bare ChannelID rather than being dropped —
// a channel we cannot describe is still a channel we must ask for.
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

// buildGapRepairPrompt builds the single instruction a repair round injects.
//
// It names each missing channel with its EXACT channel_type and an RFC3339 time
// window, because the most common reason a channel went unfetched in the first
// place is a bad argument (missing time_start, wrong channel_type) — handing the
// values back verbatim keeps the retry from reproducing the original failure.
// missing must be non-empty.
func buildGapRepairPrompt(missing []summaryspec.Channel, tr summaryspec.TimeRange) string {
	var b strings.Builder
	b.WriteString("⚠️ 覆盖检查:你还没有抓取以下用户明确选定的频道,它们必须纳入本次总结:\n")
	for _, c := range missing {
		name := c.Name
		if name == "" {
			name = c.ChannelID
		}
		ctype := c.Type
		if ctype == "" {
			ctype = "见参考资料中该频道的 channel_type"
		}
		b.WriteString(fmt.Sprintf("  - channel_id=%s channel_type=%s (%s)\n", c.ChannelID, ctype, name))
	}
	b.WriteString("\n请对上面每个频道调用 fetch_channel:channel_type 原样复制上面的值,")
	if tr.Start > 0 && tr.End > 0 {
		b.WriteString(fmt.Sprintf(
			"time_start=%s、time_end=%s(RFC3339),",
			time.Unix(tr.Start, 0).Format(time.RFC3339),
			time.Unix(tr.End, 0).Format(time.RFC3339),
		))
	} else {
		b.WriteString("time_start / time_end 沿用你本次已用的时间窗,")
	}
	b.WriteString("抓到后把它们的内容并入,重新生成完整总结。若某频道确实无可总结内容,如实说明,勿编造。")
	return b.String()
}

// closedScopeForRequest reports whether this request pinned a channel set in the
// UI. Only a closed scope has a fixed expectation to repair against; an open
// scope ("总结我这周所有群") has no authoritative expected list, so there is
// nothing the loop could prove is missing.
func closedScopeForRequest(req agentChatRequest) bool {
	return len(normalizeSelectedChannels(req.SelectedChannels)) > 0
}
