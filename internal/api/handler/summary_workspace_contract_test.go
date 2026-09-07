package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/gin-gonic/gin"
)

func TestSummaryWorkspaceHistoryPreservesEveryPreview(t *testing.T) {
	scopeJSON, _, err := marshalSummaryWorkspaceContext(emptySummaryWorkspaceContext())
	if err != nil {
		t.Fatal(err)
	}
	previewPayload := func(content string, version int) *string {
		data, marshalErr := json.Marshal(agent.SummaryResponsePayload{
			ResultType:      workspaceResultAgentRevision,
			ExecutionTarget: "agent_preview",
			Preview: &agent.SummaryResponsePreview{
				Content: content,
				Version: version,
			},
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		value := string(data)
		return &value
	}
	messages := []model.AgentMessage{
		{
			ID: 11, Role: "assistant", Content: "第一版", ResultType: workspaceResultAgentRevision,
			ScopeVersion: 1, ArtifactVersion: 1, SnapshotVersion: workspaceSnapshotVersion,
			ResponsePayload: previewPayload("# 总结 V1", 1),
		},
		{
			ID: 22, Role: "assistant", Content: "第二版", ResultType: workspaceResultAgentRevision,
			ScopeVersion: 1, ArtifactVersion: 2, SnapshotVersion: workspaceSnapshotVersion,
			ResponsePayload: previewPayload("# 总结 V2", 2),
		},
	}
	snapshot := WorkspaceSnapshot{
		Session: model.AgentSummarySession{
			ContractVersion:        summaryWorkspaceContractVersion,
			ScopeVersion:           1,
			ScopeJSON:              string(scopeJSON),
			ArtifactVersion:        2,
			LatestPreviewMessageID: 22,
		},
		Messages:       messages,
		CurrentPreview: &messages[1],
	}

	history, err := (&summaryWorkspaceCoordinator{}).historyFromSnapshot(context.Background(), "session-1", snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(history.Messages) != 2 || history.Messages[0].Preview == nil || history.Messages[1].Preview == nil {
		t.Fatalf("history previews = %#v, want both versions", history.Messages)
	}
	if history.Messages[0].Preview.Content != "# 总结 V1" || len(history.Messages[0].Preview.AvailableActions) != 0 {
		t.Fatalf("old preview = %#v, want read-only V1", history.Messages[0].Preview)
	}
	if history.Messages[1].Preview.Content != "# 总结 V2" ||
		!reflect.DeepEqual(history.Messages[1].Preview.AvailableActions, []string{workspaceActionSavePreview, workspaceActionContinueChat}) {
		t.Fatalf("latest preview = %#v, want actionable V2", history.Messages[1].Preview)
	}
}

func TestSummaryWorkspaceCapabilitiesAdvertisesTimeRangeLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	handler := &AgentChatHandler{
		workspaceEntryEnabled: true,
		workspace:             &summaryWorkspaceCoordinator{store: &AgentWorkspaceStore{}},
	}

	handler.SummaryWorkspaceCapabilities(context)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	var payload struct {
		Data struct {
			Enabled            bool   `json:"enabled"`
			ContractVersion    string `json:"contract_version"`
			MaxTimeRangeDays   int    `json:"max_time_range_days"`
			DirectTeamWorkflow bool   `json:"direct_team_workflow"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode capabilities: %v", err)
	}
	if !payload.Data.Enabled || payload.Data.ContractVersion != summaryWorkspaceContractVersion {
		t.Fatalf("unexpected capabilities: %#v", payload.Data)
	}
	if payload.Data.MaxTimeRangeDays != 90 {
		t.Fatalf("max_time_range_days = %d, want 90", payload.Data.MaxTimeRangeDays)
	}
	if !payload.Data.DirectTeamWorkflow {
		t.Fatal("direct_team_workflow = false, want true")
	}
}

func TestSummaryWorkspaceCapabilitiesFailsClosedWhenRolloutDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	handler := &AgentChatHandler{
		workspace: &summaryWorkspaceCoordinator{store: &AgentWorkspaceStore{}},
	}

	handler.SummaryWorkspaceCapabilities(context)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	var payload struct {
		Data struct {
			Enabled            bool `json:"enabled"`
			DirectTeamWorkflow bool `json:"direct_team_workflow"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode capabilities: %v", err)
	}
	if payload.Data.Enabled || payload.Data.DirectTeamWorkflow {
		t.Fatalf("disabled rollout advertised enabled capabilities: %#v", payload.Data)
	}
	if !handler.summaryWorkspaceConfigured() {
		t.Fatal("disabled rollout should keep dependencies configured for later enablement")
	}
}

func TestSummaryWorkspaceChatFailsClosedWhenRolloutDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	handler := &AgentChatHandler{
		workspace: &summaryWorkspaceCoordinator{store: &AgentWorkspaceStore{}},
	}
	handler.handleSummaryWorkspaceChat(context, agentChatRequest{}, false)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
}

func TestNormalizeSummaryWorkspaceContext(t *testing.T) {
	got, err := normalizeSummaryWorkspaceContext(summaryWorkspaceContext{
		SelectedChannels: []summaryWorkspaceChannel{
			{ChatID: " group-1 ", ChatType: " GROUP ", Name: " 项目群 "},
			{ChatID: "group-1", ChatType: "group", Name: "重复"},
		},
		Participants: []summaryWorkspaceParticipant{
			{UserID: " u2 ", UserName: " 张三 "},
			{UserID: "u2", UserName: "重复"},
		},
		ReferencedTaskIDs: []int64{9, 9, 10},
	})
	if err != nil {
		t.Fatalf("normalize context: %v", err)
	}
	if len(got.SelectedChannels) != 1 || got.SelectedChannels[0].ChatID != "group-1" || got.SelectedChannels[0].ChatType != "group" {
		t.Fatalf("unexpected channels: %#v", got.SelectedChannels)
	}
	if len(got.Participants) != 1 || got.Participants[0].UserID != "u2" || got.Participants[0].UserName != "张三" {
		t.Fatalf("unexpected participants: %#v", got.Participants)
	}
	if !reflect.DeepEqual(got.ReferencedTaskIDs, []int64{9, 10}) {
		t.Fatalf("unexpected references: %#v", got.ReferencedTaskIDs)
	}
	if got.Template != nil || got.TimeRange != nil {
		t.Fatalf("nil optional values changed: %#v", got)
	}
}

func TestNormalizeSummaryWorkspaceContextRejectsInvalidRange(t *testing.T) {
	_, err := normalizeSummaryWorkspaceContext(summaryWorkspaceContext{
		TimeRange: &summaryWorkspaceTimeRange{
			Start: "2026-08-27T10:00:00Z",
			End:   "2026-08-26T10:00:00Z",
			Label: "昨天",
		},
	})
	if err == nil {
		t.Fatal("expected invalid time range")
	}
}

func TestNormalizeSummaryWorkspaceContextDefaultsAndValidatesRangeSource(t *testing.T) {
	got, err := normalizeSummaryWorkspaceContext(summaryWorkspaceContext{
		TimeRange: &summaryWorkspaceTimeRange{
			Start: "2026-08-27T10:00:00Z",
			End:   "2026-09-04T10:00:00Z",
			Label: "最近 7 天",
		},
	})
	if err != nil {
		t.Fatalf("normalize context: %v", err)
	}
	if got.TimeRange == nil || got.TimeRange.Source != summaryWorkspaceTimeRangeSourcePicker {
		t.Fatalf("time range=%#v, want picker source", got.TimeRange)
	}

	_, err = normalizeSummaryWorkspaceContext(summaryWorkspaceContext{
		TimeRange: &summaryWorkspaceTimeRange{
			Start: "2026-08-27T10:00:00Z",
			End:   "2026-09-04T10:00:00Z", Label: "最近 7 天", Source: "unknown",
		},
	})
	if err == nil {
		t.Fatal("expected invalid time range source")
	}
}

func TestNormalizeSummaryWorkspaceContextRejectsRangeOverNinetyDays(t *testing.T) {
	_, err := normalizeSummaryWorkspaceContext(summaryWorkspaceContext{
		TimeRange: &summaryWorkspaceTimeRange{
			Start: "2026-05-01T00:00:00Z",
			End:   "2026-08-01T00:00:01Z",
			Label: "超过 90 天",
		},
	})
	if err == nil {
		t.Fatal("expected over-limit time range to be rejected")
	}
}

func TestNormalizeSummaryWorkspaceContextEnforcesWorkspaceSelectionLimits(t *testing.T) {
	channels := make([]summaryWorkspaceChannel, maxSummaryWorkspaceSelectedChannels+1)
	for index := range channels {
		channels[index] = summaryWorkspaceChannel{ChatID: fmt.Sprintf("group-%d", index), ChatType: "group", Name: "群聊"}
	}
	if _, err := normalizeSummaryWorkspaceContext(summaryWorkspaceContext{SelectedChannels: channels}); err == nil {
		t.Fatal("expected selected channel limit error")
	}

	participants := make([]summaryWorkspaceParticipant, maxSummaryWorkspaceParticipants+1)
	for index := range participants {
		participants[index] = summaryWorkspaceParticipant{UserID: fmt.Sprintf("user-%d", index)}
	}
	if _, err := normalizeSummaryWorkspaceContext(summaryWorkspaceContext{Participants: participants}); err == nil {
		t.Fatal("expected participant limit error")
	}
}

func TestWorkspaceActionsOnlyPreviewCanSave(t *testing.T) {
	for _, resultType := range []string{
		workspaceResultClarification,
		workspaceResultExplanation,
		workspaceResultWorkflowConfirm,
		workspaceResultWorkflowStarted,
		workspaceResultWorkflowCompleted,
		workspaceResultError,
	} {
		for _, action := range workspaceActionsForResult(resultType, false) {
			if action == workspaceActionSavePreview {
				t.Fatalf("result %s unexpectedly saveable", resultType)
			}
		}
	}
	for _, resultType := range []string{workspaceResultAgentPreview, workspaceResultAgentRevision} {
		if !reflect.DeepEqual(workspaceActionsForResult(resultType, false), []string{workspaceActionSavePreview, workspaceActionContinueChat}) {
			t.Fatalf("result %s must be saveable", resultType)
		}
		if reflect.DeepEqual(workspaceActionsForResult(resultType, true), []string{workspaceActionSavePreview, workspaceActionContinueChat}) {
			t.Fatalf("saved result %s must not stay saveable", resultType)
		}
	}
}

func TestClassifySummaryWorkspaceIntentKeepsQuestionsReadOnly(t *testing.T) {
	tests := []struct {
		name       string
		message    string
		hasPreview bool
		want       service.SummaryIntent
	}{
		{name: "template question before preview", message: "这个模板包含什么内容？", want: service.SummaryIntentExplain},
		{name: "explicit generation", message: "请根据当前选择生成总结", want: service.SummaryIntentGenerate},
		{name: "polite generation question", message: "能帮我总结一下吗？", want: service.SummaryIntentGenerate},
		{name: "negated generation", message: "先不要生成总结，我还要补充要求", want: service.SummaryIntentExplain},
		{name: "explicit preview overrides formal negation", message: "不要生成正式总结，先给预览", want: service.SummaryIntentGenerate},
		{name: "free form requirement", message: "重点关注风险和行动项", want: service.SummaryIntentGenerate},
		{name: "preview follow-up", message: "把行动项放到最前面", hasPreview: true, want: service.SummaryIntentRevise},
		{name: "explicit regeneration updates preview", message: "请重新生成总结", hasPreview: true, want: service.SummaryIntentRevise},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifySummaryWorkspaceIntent(tt.message, tt.hasPreview); got != tt.want {
				t.Fatalf("classifySummaryWorkspaceIntent() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHasExplicitSummaryRunIntent(t *testing.T) {
	for _, message := range []string{
		"请根据当前选择生成总结",
		"请根据当前选择准备多人总结任务",
		"Generate a summary from the current selection",
		"Prepare a team summary task from the current selection",
		"Summarize the selected conversation",
	} {
		if !hasExplicitSummaryRunIntent(message) {
			t.Errorf("message %q must authorize an execution route", message)
		}
	}

	for _, message := range []string{
		"这个模板包含什么内容？",
		"重点关注风险和行动项",
		"为什么会得到这个结论",
		"先不要生成总结，我还要补充要求",
		"不要生成正式总结，先给预览",
	} {
		if hasExplicitSummaryRunIntent(message) {
			t.Errorf("message %q must remain free of workflow side effects", message)
		}
	}
}

func TestExplicitExecutionCommandOverridesExistingPreviewWithoutHijackingRevision(t *testing.T) {
	if !hasExplicitSummaryExecutionCommand("开始总结") ||
		!hasExplicitSummaryExecutionCommand("发起多人总结：按风险排序") ||
		!hasExplicitSummaryExecutionCommand("请根据当前选择生成总结") ||
		!hasExplicitSummaryExecutionCommand("请根据当前选择准备多人总结任务") ||
		!hasExplicitSummaryExecutionCommand("Generate a summary from the current selection") ||
		!hasExplicitSummaryExecutionCommand("Prepare a team summary task from the current selection") {
		t.Fatal("fixed execution commands must be recognized")
	}
	if hasExplicitSummaryExecutionCommand("请总结得更简洁") {
		t.Fatal("revision wording must not be treated as an execution command")
	}
	if got := classifySummaryWorkspaceIntent("请总结得更简洁", true); got != service.SummaryIntentRevise {
		t.Fatalf("revision intent=%q", got)
	}
}

func TestSummaryWorkspaceRequirementSeparatesInputOrigin(t *testing.T) {
	template := &summaryWorkspaceTemplate{TemplateID: "weekly", Label: "周报", Requirement: "按周报结构输出"}
	if got := summaryWorkspaceExecutionRequirement(summaryWorkspaceContext{Template: template}, template.Requirement, summaryWorkspaceInputTemplate); got != template.Requirement {
		t.Fatalf("template autofill duplicated requirement: %q", got)
	}
	if got := summaryWorkspaceExecutionRequirement(summaryWorkspaceContext{}, "开始总结", summaryWorkspaceInputUser); got != "" {
		t.Fatalf("pure execution command became requirement: %q", got)
	}
	if got := summaryWorkspaceExecutionRequirement(summaryWorkspaceContext{}, "开始总结：重点关注延期风险", summaryWorkspaceInputUser); got != "重点关注延期风险" {
		t.Fatalf("command remainder = %q", got)
	}
	if got := summaryWorkspaceExecutionRequirement(summaryWorkspaceContext{Template: template}, "额外关注客户反馈", summaryWorkspaceInputUser); got != template.Requirement+"\n\n额外关注客户反馈" {
		t.Fatalf("template + user requirement = %q", got)
	}
}

func TestNormalizeSummaryWorkspaceInputOriginCompatibility(t *testing.T) {
	template := &summaryWorkspaceTemplate{TemplateID: "weekly", Label: "周报", Requirement: "按周报结构输出"}
	if got, err := normalizeSummaryWorkspaceInputOrigin("", summaryWorkspaceContext{Template: template}, template.Requirement); err != nil || got != summaryWorkspaceInputTemplate {
		t.Fatalf("legacy template origin = %q err=%v", got, err)
	}
	if got, err := normalizeSummaryWorkspaceInputOrigin("", summaryWorkspaceContext{}, "请根据当前选择生成总结"); err != nil || got != summaryWorkspaceInputSystemIntent {
		t.Fatalf("legacy generated origin = %q err=%v", got, err)
	}
	if _, err := normalizeSummaryWorkspaceInputOrigin("model", summaryWorkspaceContext{}, "总结"); err == nil {
		t.Fatal("invalid input origin accepted")
	}
}

func TestCanonicalizeSummaryWorkspaceContextForActor(t *testing.T) {
	got := canonicalizeSummaryWorkspaceContextForActor(summaryWorkspaceContext{
		SelectedChannels: []summaryWorkspaceChannel{
			{ChatID: "peer", ChatType: "direct", Name: "私聊"},
			{ChatID: "peer@actor", ChatType: "direct", Name: "重复私聊"},
		},
		Participants: []summaryWorkspaceParticipant{
			{UserID: "actor", UserName: "自己"},
			{UserID: "other", UserName: "成员"},
		},
		ReferencedTaskIDs: []int64{},
	}, "actor")
	if len(got.SelectedChannels) != 1 || got.SelectedChannels[0].ChatID != pipeline.NormalizeDMChannelID("peer", "actor", model.ChannelTypeDM) {
		t.Fatalf("direct channels were not canonicalized and deduplicated: %#v", got.SelectedChannels)
	}
	if len(got.Participants) != 1 || got.Participants[0].UserID != "other" {
		t.Fatalf("actor was not removed from participants: %#v", got.Participants)
	}
}

func TestEmptySummaryWorkspaceHistoryUsesV1Shape(t *testing.T) {
	history := emptySummaryWorkspaceHistory("session-1")
	if history.ContractVersion != summaryWorkspaceContractVersion || history.SessionID != "session-1" || history.State.ScopeVersion != 1 {
		t.Fatalf("unexpected empty history: %#v", history)
	}
	if history.Messages == nil || history.State.SummaryContext.SelectedChannels == nil || history.State.SummaryContext.Participants == nil || history.State.SummaryContext.ReferencedTaskIDs == nil {
		t.Fatalf("empty workspace collections must encode as arrays: %#v", history)
	}
	if history.State.CurrentPreview != nil || history.State.PendingProposal != nil || history.State.Workflow != nil {
		t.Fatalf("empty workspace state contains artifacts: %#v", history.State)
	}
}

func TestSummaryWorkspaceRequestHashIncludesScopeVersion(t *testing.T) {
	v1 := summaryWorkspaceRequestHash("chat", "请总结", 1, "same-scope")
	v2 := summaryWorkspaceRequestHash("chat", "请总结", 2, "same-scope")
	if v1 == v2 {
		t.Fatal("request hash must change when scope_version changes")
	}
}

func TestSummaryWorkspaceRequestHashIncludesInputOrigin(t *testing.T) {
	user := summaryWorkspaceRequestHash("chat", "同一段文本", 1, "same-scope", summaryWorkspaceInputUser)
	template := summaryWorkspaceRequestHash("chat", "同一段文本", 1, "same-scope", summaryWorkspaceInputTemplate)
	if user == template {
		t.Fatal("request hash must distinguish input_origin")
	}
}

func TestDeriveWorkspaceRouteFinalMatrix(t *testing.T) {
	channel := summaryWorkspaceChannel{ChatID: "g1", ChatType: "group", Name: "项目群"}
	template := &summaryWorkspaceTemplate{TemplateID: "weekly", Label: "周报", Requirement: "总结进展"}
	participant := summaryWorkspaceParticipant{UserID: "u2"}
	tests := []struct {
		name                   string
		action                 service.SummaryAction
		context                summaryWorkspaceContext
		selectedSourceExplicit bool
		hasRequirement         bool
		openScopeAgent         bool
		sourcesValid           bool
		want                   service.SummaryRoute
	}{
		{name: "C only previews", context: summaryWorkspaceContext{SelectedChannels: []summaryWorkspaceChannel{channel}}, selectedSourceExplicit: true, sourcesValid: true, want: service.SummaryRouteAgentPreview},
		{name: "T only inferred C previews", context: summaryWorkspaceContext{SelectedChannels: []summaryWorkspaceChannel{channel}, Template: template}, hasRequirement: true, sourcesValid: true, want: service.SummaryRouteAgentPreview},
		{name: "C plus T runs personal workflow", context: summaryWorkspaceContext{SelectedChannels: []summaryWorkspaceChannel{channel}, Template: template}, selectedSourceExplicit: true, hasRequirement: true, sourcesValid: true, want: service.SummaryRoutePersonalWorkflow},
		{name: "P only clarifies", context: summaryWorkspaceContext{Participants: []summaryWorkspaceParticipant{participant}}, want: service.SummaryRouteClarification},
		{name: "C plus P without requirement clarifies", context: summaryWorkspaceContext{SelectedChannels: []summaryWorkspaceChannel{channel}, Participants: []summaryWorkspaceParticipant{participant}}, selectedSourceExplicit: true, sourcesValid: true, want: service.SummaryRouteClarification},
		{name: "P plus T requires team confirmation", context: summaryWorkspaceContext{Participants: []summaryWorkspaceParticipant{participant}, Template: template}, hasRequirement: true, want: service.SummaryRouteTeamConfirmation},
		{name: "C plus P plus T requires team confirmation", context: summaryWorkspaceContext{SelectedChannels: []summaryWorkspaceChannel{channel}, Participants: []summaryWorkspaceParticipant{participant}, Template: template}, selectedSourceExplicit: true, hasRequirement: true, sourcesValid: true, want: service.SummaryRouteTeamConfirmation},
		{name: "explicit team start skips confirmation", action: service.SummaryActionStartTeamWorkflow, context: summaryWorkspaceContext{SelectedChannels: []summaryWorkspaceChannel{channel}, Participants: []summaryWorkspaceParticipant{participant}, Template: template}, selectedSourceExplicit: true, hasRequirement: true, sourcesValid: true, want: service.SummaryRouteTeamWorkflow},
		{name: "U only enters open-scope agent", context: summaryWorkspaceContext{}, hasRequirement: true, openScopeAgent: true, want: service.SummaryRouteAgentPreview},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			action := tt.action
			if action == "" {
				action = service.SummaryActionChat
			}
			got := deriveWorkspaceRoute(tt.context, action, service.SummaryIntentGenerate, true, tt.selectedSourceExplicit, tt.hasRequirement, tt.openScopeAgent, WorkspaceSnapshot{}, true, tt.sourcesValid, true)
			if got != tt.want {
				t.Fatalf("route = %q, want %q", got, tt.want)
			}
		})
	}
}
