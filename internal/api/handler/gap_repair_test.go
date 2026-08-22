package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/summaryspec"
)

// fakeHistoryRunner records every RunWithHistory call so a test can assert both
// how many rounds ran and what the repair prompt actually said.
type fakeHistoryRunner struct {
	calls    []string
	replies  []string
	errAfter int // >0: return an error on that call number
}

func (f *fakeHistoryRunner) RunWithHistory(_ context.Context, _ string, _ []agent.Message, userMessage string) (string, []agent.Message, error) {
	f.calls = append(f.calls, userMessage)
	n := len(f.calls)
	if f.errAfter > 0 && n == f.errAfter {
		return "", nil, context.DeadlineExceeded
	}
	reply := "reply"
	if n-1 < len(f.replies) {
		reply = f.replies[n-1]
	}
	return reply, []agent.Message{{Role: "assistant", Content: reply}}, nil
}

// The cheap gates must return before the loop touches summary deps: agent
// deps are unset in unit tests and GetSummaryDeps panics when unset, so a
// regression that moves the gate below it fails loudly here rather than in prod.
func TestRunWithRepairSkipsWithoutClosedScopeOrRun(t *testing.T) {
	h := &AgentChatHandler{}
	for name, tc := range map[string]struct {
		runID       string
		closedScope bool
	}{
		"no run id":  {"", true},
		"open scope": {"run-1", false},
		"neither":    {"", false},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeHistoryRunner{}
			reply, newMsgs, err := h.runWithRepair(context.Background(), f, "sys", nil, "hi", "u1", tc.runID, tc.closedScope, nil)
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if len(f.calls) != 1 {
				t.Fatalf("runner called %d times, want exactly 1 (no repair round)", len(f.calls))
			}
			if reply != "reply" || len(newMsgs) != 1 {
				t.Fatalf("reply=%q msgs=%d, want the single run's output passed through", reply, len(newMsgs))
			}
		})
	}
}

// A repair round must hand back the EXACT channel_type and an RFC3339 window:
// the usual reason a channel went unfetched is a bad argument, so a retry that
// re-derives them reproduces the original failure.
func TestBuildGapRepairPromptEchoesTypeAndWindow(t *testing.T) {
	missing := []summaryspec.Channel{
		{ChannelID: "c-1", Name: "产品群", Type: "group"},
		{ChannelID: "c-2"}, // unknown name/type must still be asked for
	}
	got := buildGapRepairPrompt(missing, summaryspec.TimeRange{Start: 1755000000, End: 1755086400})

	for _, want := range []string{"channel_id=c-1", "channel_type=group", "产品群", "channel_id=c-2"} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "time_start=") || !strings.Contains(got, "T") {
		t.Fatalf("prompt must carry an RFC3339 window:\n%s", got)
	}
	if !strings.Contains(got, "勿编造") {
		t.Fatalf("prompt must forbid fabrication when a channel is genuinely empty:\n%s", got)
	}
}

// Without a usable window the prompt must tell the agent to reuse its own, not
// emit a bogus epoch-zero range.
func TestBuildGapRepairPromptWithoutWindow(t *testing.T) {
	got := buildGapRepairPrompt([]summaryspec.Channel{{ChannelID: "c-1"}}, summaryspec.TimeRange{})
	if strings.Contains(got, "1970") {
		t.Fatalf("empty TimeRange must not render as epoch zero:\n%s", got)
	}
	if !strings.Contains(got, "沿用") {
		t.Fatalf("prompt should tell the agent to reuse its own window:\n%s", got)
	}
}

// Unknown ids must degrade to a bare ChannelID rather than vanish: a channel we
// cannot describe is still a channel the agent owes us.
func TestChannelsForIDsPreservesOrderAndUnknowns(t *testing.T) {
	all := []summaryspec.Channel{
		{ChannelID: "a", Name: "A", Type: "group"},
		{ChannelID: "b", Name: "B", Type: "dm"},
	}
	got := channelsForIDs(all, []string{"b", "zzz", "a"})
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (unknown id must not be dropped)", len(got))
	}
	if got[0].ChannelID != "b" || got[0].Name != "B" {
		t.Fatalf("got[0] = %+v, want the full record for b in ids order", got[0])
	}
	if got[1].ChannelID != "zzz" || got[1].Name != "" {
		t.Fatalf("got[1] = %+v, want a bare ChannelID for the unknown id", got[1])
	}
	if got[2].ChannelID != "a" {
		t.Fatalf("got[2] = %+v, want ids order preserved", got[2])
	}
}

// closedScopeForRequest must agree with maybePersistSummaryRun's own
// open/closed decision, which is normalizeSelectedChannels — including its
// rejection of unrecognised channel types. A selection the run persisted as OPEN
// must not make the repair loop believe it has an expected set.
func TestClosedScopeForRequest(t *testing.T) {
	if closedScopeForRequest(agentChatRequest{}) {
		t.Fatal("no selected channels must be open scope — there is no expected set to repair against")
	}
	if closedScopeForRequest(agentChatRequest{SelectedChannels: []selectedChannel{{ChannelID: "   ", ChannelType: "group"}}}) {
		t.Fatal("blank channel ids normalize away and must not count as a closed scope")
	}
	if closedScopeForRequest(agentChatRequest{SelectedChannels: []selectedChannel{{ChannelID: "c-1", ChannelType: "wat"}}}) {
		t.Fatal("an unrecognised channel_type is dropped by normalization, so the run is OPEN scope — the repair loop must agree")
	}
	if !closedScopeForRequest(agentChatRequest{SelectedChannels: []selectedChannel{{ChannelID: "c-1", ChannelType: "group"}}}) {
		t.Fatal("a real selected channel must be closed scope")
	}
}
