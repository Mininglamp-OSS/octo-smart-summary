package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
)

func TestApplySelectedChannelContext_EmptyIsBackwardCompatible(t *testing.T) {
	ctx := context.Background()
	gotCtx, gotSystem := applySelectedChannelContext(ctx, "base", nil)
	if gotCtx != ctx || gotSystem != "base" {
		t.Fatalf("empty selection changed request: ctx=%v system=%q", gotCtx != ctx, gotSystem)
	}
}

func TestApplySelectedChannelContext_AddsPromptAndArchivedAllowlist(t *testing.T) {
	ctx, system := applySelectedChannelContext(context.Background(), "base", []selectedChannel{
		{ChannelID: "group-1", ChannelType: "group", Name: "品牌群"},
		{ChannelID: "group-1____thread-1", ChannelType: "thread", Name: "复盘区", IsArchived: true},
		{ChannelID: "group-1____thread-1", ChannelType: "thread", Name: "重复项", IsArchived: true},
	})

	for _, want := range []string{"品牌群", "group-1____thread-1", "tool_channel_type=5", "跳过 list_channels", "范围或能力澄清问题"} {
		if !strings.Contains(system, want) {
			t.Errorf("system prompt missing %q: %s", want, system)
		}
	}
	if strings.Contains(system, "重复项") {
		t.Errorf("duplicate channel was not removed: %s", system)
	}
	ids := agent.SelectedArchivedChannelIDs(ctx)
	if len(ids) != 1 || ids[0] != "group-1____thread-1" {
		t.Fatalf("archived allowlist = %v, want selected archived thread only", ids)
	}
}

func TestApplySelectedChannelContext_DropsUnknownChannelTypes(t *testing.T) {
	// yujiawei review PR#164 (P2#3): entries with an unrecognized chat_type
	// used to be coerced into "unknown" and still emitted into the system
	// prompt as tool_channel_type=0, which then failed fetch/peek downstream.
	// The fix drops them at normalization time so the LLM never sees a
	// broken descriptor. Valid entries in the same batch must still surface.
	ctx, system := applySelectedChannelContext(context.Background(), "base", []selectedChannel{
		{ChannelID: "bad-1", ChannelType: "chatroom", Name: "非法类型-A"},
		{ChannelID: "bad-2", ChannelType: "", Name: "空类型"},
		{ChannelID: "group-1", ChannelType: "group", Name: "正常群"},
	})

	if strings.Contains(system, "非法类型-A") || strings.Contains(system, "空类型") {
		t.Errorf("unknown-type entries leaked into system prompt: %s", system)
	}
	if strings.Contains(system, "tool_channel_type=0") {
		t.Errorf("system prompt still emits tool_channel_type=0: %s", system)
	}
	if !strings.Contains(system, "正常群") || !strings.Contains(system, "tool_channel_type=2") {
		t.Errorf("valid entry missing from system prompt: %s", system)
	}
	// Unknown entries were never threads, so no archived allowlist should exist.
	ids := agent.SelectedArchivedChannelIDs(ctx)
	if len(ids) != 0 {
		t.Fatalf("unexpected archived allowlist entries: %v", ids)
	}
}

func TestAgentChat_SelectedChannelsReachSystemPrompt(t *testing.T) {
	reg := agent.NewRegistry()
	pool := agent.NewPool(1)
	chatter := &recordingChatter{reply: "ok"}
	runner := agent.NewRunner(chatter, reg, pool, agent.Policy{MaxSteps: 2, MaxTokens: 1000, StepTimeout: time.Second})
	h := newAgentChatHandlerWithRunner(runner, "base-system", newFakeHistoryStore(), 10)
	r := setupAgentChatRouter(h)

	body := strings.NewReader(`{
		"message":"总结这个聊天",
		"session_id":"selected-context",
		"selected_channels":[{"chat_id":"g1____t1","chat_type":"thread","name":"已归档复盘","is_archived":true}]
	}`)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/chat", body)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if len(chatter.lastMsgs) == 0 || !strings.Contains(chatter.lastMsgs[0].Content, "已归档复盘") {
		t.Fatalf("selected channel missing from LLM system prompt: %+v", chatter.lastMsgs)
	}
}
