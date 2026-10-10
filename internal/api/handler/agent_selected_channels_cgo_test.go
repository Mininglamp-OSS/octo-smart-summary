//go:build cgo
// +build cgo

package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/summaryrun"
)

func TestSelectedDMPromptMatchesPersistedSpecChannelID(t *testing.T) {
	db := newFinalizeTestDB(t)
	if db == nil {
		return
	}
	const uid = "u-gate"
	selected := []selectedChannel{{ChannelID: "peer-uid@u-gate", ChannelType: "direct", Name: "同事"}}
	h := &AgentChatHandler{runStore: summaryrun.NewStore(db)}
	req := agentChatRequest{
		Message:          "总结这个聊天",
		SessionID:        "selected-dm-contract",
		RequestID:        "selected-dm-contract-request",
		SelectedChannels: selected,
	}
	runID := h.maybePersistSummaryRun(context.Background(), uid, req, true)
	if runID == "" {
		t.Fatal("maybePersistSummaryRun did not create a run")
	}
	spec, found, err := h.runStore.GetLatestSpec(context.Background(), uid, runID)
	if err != nil || !found || len(spec.Channels) != 1 {
		t.Fatalf("persisted spec = %#v found=%t err=%v, want one channel", spec, found, err)
	}

	_, prompt := applySelectedChannelContext(context.Background(), "base", selected, uid, "summary")
	if !strings.Contains(prompt, `chat_id="`+spec.Channels[0].ChannelID+`"`) {
		t.Fatalf("prompt channel id does not match persisted spec %q: %s", spec.Channels[0].ChannelID, prompt)
	}
}
