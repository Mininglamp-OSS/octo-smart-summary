package handler

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

func TestMaterializeWorkspaceAgentContextKeepsExplicitPickerRange(t *testing.T) {
	now := time.Date(2026, 9, 4, 16, 30, 0, 0, time.UTC)
	coordinator := &summaryWorkspaceCoordinator{now: func() time.Time { return now }}
	contextValue := emptySummaryWorkspaceContext()
	contextValue.SelectedChannels = []summaryWorkspaceChannel{{ChatID: "group-a", ChatType: "group", Name: "A群"}}
	contextValue.TimeRange = &summaryWorkspaceTimeRange{
		Start:  now.AddDate(0, 0, -2).Format(time.RFC3339),
		End:    now.Format(time.RFC3339),
		Label:  "2026-09-02 至 2026-09-04",
		Source: summaryWorkspaceTimeRangeSourcePicker,
	}

	got, _, err := coordinator.materializeWorkspaceAgentContext(
		t.Context(), "space-a", "actor", contextValue, WorkspaceSnapshot{},
		"最近一个月的背景我知道，但只总结我选的这三天", service.SummaryIntentGenerate, summaryWorkspaceInputUser,
	)
	if err != nil {
		t.Fatalf("materialize context: %v", err)
	}
	if got.TimeRange == nil || got.TimeRange.Label != contextValue.TimeRange.Label {
		t.Fatalf("time range=%#v, want explicit picker range %#v", got.TimeRange, contextValue.TimeRange)
	}
}

func TestMaterializeWorkspaceAgentContextDefersExplicitCommandToAgentScopeTool(t *testing.T) {
	now := time.Date(2026, 9, 4, 16, 30, 0, 0, time.UTC)
	coordinator := &summaryWorkspaceCoordinator{now: func() time.Time { return now }}
	contextValue := emptySummaryWorkspaceContext()
	contextValue.SelectedChannels = []summaryWorkspaceChannel{{ChatID: "group-a", ChatType: "group", Name: "A群"}}
	contextValue.TimeRange = &summaryWorkspaceTimeRange{
		Start: now.AddDate(0, 0, -2).Format(time.RFC3339), End: now.Format(time.RFC3339),
		Label: "2026-09-02 至 2026-09-04", Source: summaryWorkspaceTimeRangeSourcePicker,
	}

	got, _, err := coordinator.materializeWorkspaceAgentContext(
		t.Context(), "space-a", "actor", contextValue, WorkspaceSnapshot{},
		"把时间范围扩大到最近一个月", service.SummaryIntentRevise, summaryWorkspaceInputUser,
	)
	if err != nil {
		t.Fatalf("materialize context: %v", err)
	}
	if got.TimeRange == nil || got.TimeRange.Label != contextValue.TimeRange.Label || got.TimeRange.Source != summaryWorkspaceTimeRangeSourcePicker {
		t.Fatalf("time range=%#v, want picker range until Agent declaration", got.TimeRange)
	}
}

func TestMaterializeWorkspaceAgentContextDefersDirectRequestToAgentScopeTool(t *testing.T) {
	now := time.Date(2026, 9, 4, 16, 30, 0, 0, time.UTC)
	coordinator := &summaryWorkspaceCoordinator{now: func() time.Time { return now }}
	contextValue := emptySummaryWorkspaceContext()
	contextValue.SelectedChannels = []summaryWorkspaceChannel{{ChatID: "group-a", ChatType: "group", Name: "A群"}}
	contextValue.TimeRange = &summaryWorkspaceTimeRange{
		Start: now.AddDate(0, 0, -2).Format(time.RFC3339), End: now.Format(time.RFC3339),
		Label: "2026-09-02 至 2026-09-04", Source: summaryWorkspaceTimeRangeSourcePicker,
	}

	got, _, err := coordinator.materializeWorkspaceAgentContext(
		t.Context(), "space-a", "actor", contextValue, WorkspaceSnapshot{},
		"总结最近一个月的进展", service.SummaryIntentGenerate, summaryWorkspaceInputUser,
	)
	if err != nil {
		t.Fatalf("materialize context: %v", err)
	}
	if got.TimeRange == nil || got.TimeRange.Label != contextValue.TimeRange.Label || got.TimeRange.Source != summaryWorkspaceTimeRangeSourcePicker {
		t.Fatalf("time range=%#v, want picker range until Agent declaration", got.TimeRange)
	}
}

func TestMaterializeWorkspaceAgentContextKeepsPersistedRangeUntilAgentScopeTool(t *testing.T) {
	now := time.Date(2026, 9, 4, 16, 30, 0, 0, time.UTC)
	coordinator := &summaryWorkspaceCoordinator{now: func() time.Time { return now }}
	contextValue := emptySummaryWorkspaceContext()
	contextValue.SelectedChannels = []summaryWorkspaceChannel{{ChatID: "group-a", ChatType: "group", Name: "A群"}}
	contextValue.TimeRange = &summaryWorkspaceTimeRange{
		Start: now.AddDate(0, 0, -29).Format(time.RFC3339),
		End:   now.Format(time.RFC3339),
		Label: "最近一个月",
	}

	got, _, err := coordinator.materializeWorkspaceAgentContext(
		t.Context(), "space-a", "actor", contextValue, WorkspaceSnapshot{},
		"时间范围改成最近七天", service.SummaryIntentRevise, summaryWorkspaceInputUser,
	)
	if err != nil {
		t.Fatalf("materialize context: %v", err)
	}
	if got.TimeRange == nil || got.TimeRange.Label != "最近一个月" {
		t.Fatalf("time range=%#v, want persisted range until Agent declaration", got.TimeRange)
	}
}

func TestMaterializeWorkspaceAgentContextMaterializesTeamDefaultRange(t *testing.T) {
	now := time.Date(2026, 9, 4, 16, 30, 0, 0, time.UTC)
	coordinator := &summaryWorkspaceCoordinator{now: func() time.Time { return now }}
	contextValue := emptySummaryWorkspaceContext()
	contextValue.SelectedChannels = []summaryWorkspaceChannel{{ChatID: "group-a", ChatType: "group", Name: "A群"}}
	contextValue.Participants = []summaryWorkspaceParticipant{{UserID: "member", UserName: "成员"}}

	got, _, err := coordinator.materializeWorkspaceAgentContext(
		t.Context(), "space-a", "actor", contextValue, WorkspaceSnapshot{},
		"总结本周进展", service.SummaryIntentGenerate, summaryWorkspaceInputUser,
	)
	if err != nil {
		t.Fatalf("materialize context: %v", err)
	}
	if got.TimeRange == nil || got.TimeRange.Label != "最近 30 天（默认）" {
		t.Fatalf("time range=%#v, want materialized team default", got.TimeRange)
	}
	start, err := time.Parse(time.RFC3339, got.TimeRange.Start)
	if err != nil {
		t.Fatalf("parse default start: %v", err)
	}
	end, err := time.Parse(time.RFC3339, got.TimeRange.End)
	if err != nil {
		t.Fatalf("parse default end: %v", err)
	}
	if got := end.Sub(start); got != 30*24*time.Hour {
		t.Fatalf("default range=%s, want 30 days", got)
	}
}

func TestMaterializeWorkspaceAgentContextDoesNotHeuristicallyApplyFollowUpTimeRange(t *testing.T) {
	now := time.Date(2026, 9, 4, 16, 30, 0, 0, time.UTC)
	coordinator := &summaryWorkspaceCoordinator{now: func() time.Time { return now }}
	contextValue := emptySummaryWorkspaceContext()
	contextValue.SelectedChannels = []summaryWorkspaceChannel{{ChatID: "group-a", ChatType: "group", Name: "A群"}}
	contextValue.TimeRange = &summaryWorkspaceTimeRange{
		Start: now.Add(-7 * 24 * time.Hour).Format(time.RFC3339),
		End:   now.Format(time.RFC3339),
		Label: "最近 7 天（默认）",
	}

	got, inferred, err := coordinator.materializeWorkspaceAgentContext(
		t.Context(), "space-a", "actor", contextValue, WorkspaceSnapshot{},
		"把时间范围扩大到一个月", service.SummaryIntentRevise, summaryWorkspaceInputUser,
	)
	if err != nil {
		t.Fatalf("materialize context: %v", err)
	}
	if inferred {
		t.Fatal("explicit source must not be inferred")
	}
	if got.TimeRange == nil || got.TimeRange.Label != "最近 7 天（默认）" {
		t.Fatalf("time range=%#v, want existing range until Agent declaration", got.TimeRange)
	}
}

func TestHydrateSummaryWorkspaceContextKeepsNaturalLanguageRangeForNextTurn(t *testing.T) {
	oldRange := &summaryWorkspaceTimeRange{Start: "2026-08-29T00:00:00+08:00", End: "2026-09-04T23:59:59+08:00", Label: "最近 7 天"}
	payload := agent.SummaryResponsePayload{
		ResultType: agent.SummaryResultAgentRevision,
		Preview: &agent.SummaryResponsePreview{
			Content: "preview",
			EffectiveScope: &agent.SummaryResponseEffectiveScope{
				TimeRange: &agent.SummaryResponseTimeRange{Start: "2026-08-06T00:00:00+08:00", End: "2026-09-04T23:59:59+08:00", Label: "最近一个月"},
			},
		},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	value := string(encoded)
	contextValue := emptySummaryWorkspaceContext()
	contextValue.TimeRange = oldRange

	got, err := hydrateSummaryWorkspaceContextFromPreview(contextValue, &model.AgentMessage{ResponsePayload: &value}, "actor")
	if err != nil {
		t.Fatalf("hydrate context: %v", err)
	}
	if got.TimeRange == nil || got.TimeRange.Label != "最近一个月" {
		t.Fatalf("time range=%#v, want persisted effective month", got.TimeRange)
	}
}

func TestApplySummaryWorkspaceProposalScopeUsesConfirmedRange(t *testing.T) {
	persisted := emptySummaryWorkspaceContext()
	persisted.Participants = []summaryWorkspaceParticipant{{UserID: "user-2", UserName: "成员"}}
	persisted.TimeRange = &summaryWorkspaceTimeRange{
		Start: "2026-08-29T00:00:00+08:00",
		End:   "2026-09-04T23:59:59+08:00",
		Label: "最近 7 天",
	}
	confirmed := &summaryWorkspaceTimeRange{
		Start:  "2026-08-06T00:00:00+08:00",
		End:    "2026-09-04T23:59:59+08:00",
		Label:  "最近一个月",
		Source: summaryWorkspaceTimeRangeSourceConversation,
	}

	got, err := applySummaryWorkspaceProposalScope(persisted, summaryWorkspaceProposal{
		Participants: persisted.Participants,
		TimeRange:    confirmed,
	}, "actor")
	if err != nil {
		t.Fatalf("apply proposal scope: %v", err)
	}
	if got.TimeRange == nil || !reflect.DeepEqual(*got.TimeRange, *confirmed) {
		t.Fatalf("time range=%#v, want confirmed range %#v", got.TimeRange, confirmed)
	}
}

func TestSummaryWorkspaceAssumptionsUseEffectiveRangeLabel(t *testing.T) {
	contextValue := emptySummaryWorkspaceContext()
	contextValue.TimeRange = &summaryWorkspaceTimeRange{Start: "2026-08-06T00:00:00+08:00", End: "2026-09-04T23:59:59+08:00", Label: "最近一个月"}

	got := summaryWorkspaceAssumptions(contextValue)
	if len(got) == 0 || got[0] != "时间范围使用最近一个月" {
		t.Fatalf("assumptions=%#v, want effective month first", got)
	}
}

func TestMergeSummaryWorkspaceAssumptionsReplacesStaleTimeRange(t *testing.T) {
	contextValue := emptySummaryWorkspaceContext()
	contextValue.TimeRange = &summaryWorkspaceTimeRange{Start: "2026-08-06T00:00:00+08:00", End: "2026-09-04T23:59:59+08:00", Label: "最近一个月"}

	got := mergeSummaryWorkspaceAssumptions([]string{"时间范围使用最近 7 天", "保留重点项目"}, contextValue)
	if len(got) != 4 {
		t.Fatalf("assumptions=%#v, want four canonical assumptions", got)
	}
	if got[0] != "保留重点项目" || got[1] != "时间范围使用最近一个月" {
		t.Fatalf("assumptions=%#v, stale time range was not replaced", got)
	}
}

func TestMergeSummaryWorkspaceAssumptionsKeepsOtherTimeRangeNotes(t *testing.T) {
	contextValue := emptySummaryWorkspaceContext()
	contextValue.TimeRange = &summaryWorkspaceTimeRange{Start: "2026-08-06T00:00:00+08:00", End: "2026-09-04T23:59:59+08:00", Label: "最近一个月"}

	got := mergeSummaryWorkspaceAssumptions([]string{
		"时间范围使用最近 7 天",
		"时间范围内未发现阻塞风险",
		"时间范围较长，结论以最新为准",
	}, contextValue)
	if !reflect.DeepEqual(got, []string{
		"时间范围内未发现阻塞风险",
		"时间范围较长，结论以最新为准",
		"时间范围使用最近一个月",
		"采用通用总结结构",
		"重点覆盖结论、进展、风险和行动项",
	}) {
		t.Fatalf("assumptions=%#v", got)
	}
}
