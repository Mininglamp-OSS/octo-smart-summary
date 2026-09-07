package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

func TestSummaryWorkspaceScopeAgentUsesTrustedOriginInsteadOfKeywords(t *testing.T) {
	tests := []struct {
		name   string
		action service.SummaryAction
		origin string
		intent service.SummaryIntent
		want   bool
	}{
		{name: "ordinary user edit", action: service.SummaryActionChat, origin: summaryWorkspaceInputUser, intent: service.SummaryIntentRevise, want: true},
		{name: "user range request", action: service.SummaryActionChat, origin: summaryWorkspaceInputUser, intent: service.SummaryIntentGenerate, want: true},
		{name: "explanation", action: service.SummaryActionChat, origin: summaryWorkspaceInputUser, intent: service.SummaryIntentExplain, want: false},
		{name: "template action", action: service.SummaryActionChat, origin: summaryWorkspaceInputTemplate, intent: service.SummaryIntentGenerate, want: false},
		{name: "direct team action", action: service.SummaryActionStartTeamWorkflow, origin: summaryWorkspaceInputSystemIntent, intent: service.SummaryIntentGenerate, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := summaryWorkspaceShouldOpenScopeAgent(test.action, test.origin, test.intent); got != test.want {
				t.Fatalf("open scope agent = %t, want %t", got, test.want)
			}
		})
	}
}

func TestMaterializeWorkspaceAgentContextKeepsOldChannelsUntilReplacementIsValidated(t *testing.T) {
	preview := workspacePreviewMessage(t, []agent.SummaryResponseChannel{{ChannelID: "old-group", ChannelType: 2, ChannelName: "旧群"}})
	contextValue := emptySummaryWorkspaceContext()
	contextValue.SelectedChannels = []summaryWorkspaceChannel{{ChatID: "ui-group", ChatType: "group", Name: "页面群"}}

	got, _, err := (&summaryWorkspaceCoordinator{}).materializeWorkspaceAgentContext(
		t.Context(), "space-a", "actor", contextValue, WorkspaceSnapshot{CurrentPreview: preview},
		"改成项目群重新总结", service.SummaryIntentRevise, summaryWorkspaceInputUser,
	)
	if err != nil {
		t.Fatalf("materialize context: %v", err)
	}
	if len(got.SelectedChannels) != 1 || got.SelectedChannels[0].ChatID != "ui-group" {
		t.Fatalf("replacement mutated authoritative channels before validation: %#v", got.SelectedChannels)
	}
}

func TestApplyDiscoveredWorkspaceScopeRejectsOversizedReplacementWithoutMutatingOldScope(t *testing.T) {
	current := emptySummaryWorkspaceContext()
	current.SelectedChannels = []summaryWorkspaceChannel{{ChatID: "old-group", ChatType: "group", Name: "旧群"}}
	discovered := make([]summaryWorkspaceChannel, maxSummaryWorkspaceSelectedChannels+1)
	for i := range discovered {
		discovered[i] = summaryWorkspaceChannel{ChatID: "group-" + string(rune('a'+i)), ChatType: "group", Name: "群" + string(rune('a'+i))}
	}

	got, err := (&summaryWorkspaceCoordinator{}).applyDiscoveredWorkspaceScope(
		t.Context(), "space-a", "actor", current, discovered, summaryWorkspaceSourceReplace,
	)
	if err == nil {
		t.Fatal("oversized replacement must be rejected")
	}
	var bizErr *service.BizError
	if !errors.As(err, &bizErr) || bizErr.Code != 40022 || bizErr.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("oversized replacement error=%v, want 40022 BizError", err)
	}
	if len(got.SelectedChannels) != 1 || got.SelectedChannels[0].ChatID != "old-group" {
		t.Fatalf("rejected replacement mutated old scope: %#v", got.SelectedChannels)
	}
}

func TestApplyDiscoveredWorkspaceScopeReturnsActionableErrorWhenNothingResolves(t *testing.T) {
	current := emptySummaryWorkspaceContext()
	current.SelectedChannels = []summaryWorkspaceChannel{{ChatID: "old-group", ChatType: "group", Name: "旧群"}}

	got, err := (&summaryWorkspaceCoordinator{}).applyDiscoveredWorkspaceScope(
		t.Context(), "space-a", "actor", current, nil, summaryWorkspaceSourceReplace,
	)
	var bizErr *service.BizError
	if !errors.As(err, &bizErr) || bizErr.Code != 40021 || bizErr.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("empty replacement error=%v, want 40021 BizError", err)
	}
	if len(got.SelectedChannels) != 1 || got.SelectedChannels[0].ChatID != "old-group" {
		t.Fatalf("empty replacement mutated old scope: %#v", got.SelectedChannels)
	}
}

func TestMaterializeWorkspaceAgentContextKeepsChannelsForExtension(t *testing.T) {
	preview := workspacePreviewMessage(t, []agent.SummaryResponseChannel{{ChannelID: "old-group", ChannelType: 2, ChannelName: "旧群"}})
	contextValue := emptySummaryWorkspaceContext()

	got, _, err := (&summaryWorkspaceCoordinator{}).materializeWorkspaceAgentContext(
		t.Context(), "space-a", "actor", contextValue, WorkspaceSnapshot{CurrentPreview: preview},
		"再加上运营群", service.SummaryIntentRevise, summaryWorkspaceInputUser,
	)
	if err != nil {
		t.Fatalf("materialize context: %v", err)
	}
	if len(got.SelectedChannels) != 1 || got.SelectedChannels[0].ChatID != "old-group" {
		t.Fatalf("extension lost hydrated channels: %#v", got.SelectedChannels)
	}
}

func workspacePreviewMessage(t *testing.T, channels []agent.SummaryResponseChannel) *model.AgentMessage {
	t.Helper()
	payload, err := json.Marshal(agent.SummaryResponsePayload{
		ResultType: agent.SummaryResultAgentPreview,
		Preview: &agent.SummaryResponsePreview{
			Content:        "preview",
			EffectiveScope: &agent.SummaryResponseEffectiveScope{Channels: channels},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	value := string(payload)
	return &model.AgentMessage{ResponsePayload: &value}
}
