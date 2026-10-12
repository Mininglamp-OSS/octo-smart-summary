package handler

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

func TestDocumentSubmissionOriginValidation(t *testing.T) {
	template := &summaryWorkspaceTemplate{TemplateID: "risk", Label: "风险", Requirement: "请说明一下本周风险"}
	for _, tc := range []struct {
		name, origin, message, want string
		template                    *summaryWorkspaceTemplate
		reject                      bool
	}{
		{name: "inferred template", message: template.Requirement, template: template, want: summaryWorkspaceInputTemplate},
		{name: "declared template", origin: summaryWorkspaceInputTemplate, message: template.Requirement, template: template, want: summaryWorkspaceInputTemplate},
		{name: "missing template", origin: summaryWorkspaceInputTemplate, message: template.Requirement, reject: true},
		{name: "mismatched template", origin: summaryWorkspaceInputTemplate, message: "不同的要求", template: template, reject: true},
		{name: "unverified system", origin: summaryWorkspaceInputSystemIntent, message: template.Requirement, reject: true},
		{name: "verified system", origin: summaryWorkspaceInputSystemIntent, message: "请根据当前选择生成总结", want: summaryWorkspaceInputSystemIntent},
		{name: "inferred system", message: "请根据当前选择生成总结", want: summaryWorkspaceInputSystemIntent},
		{name: "ordinary user", message: "这些内容有什么区别？", want: summaryWorkspaceInputUser},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := summaryWorkspaceContext{Template: tc.template, Documents: []summaryWorkspaceDocument{{DocumentID: "d1"}}}
			origin, err := normalizeSummaryWorkspaceInputOrigin(tc.origin, scope, tc.message)
			if tc.reject {
				if err == nil {
					t.Fatal("unverified origin accepted")
				}
				return
			}
			if err != nil || origin != tc.want {
				t.Fatalf("origin=%s err=%v want=%s", origin, err, tc.want)
			}
			if got := summaryWorkspaceExecutionRequirement(scope, tc.message, origin); got != tc.message {
				t.Fatalf("requirement lost or duplicated: %q", got)
			}
		})
	}
}

func TestDocumentInputRoutingCompatibility(t *testing.T) {
	questions := []string{
		"开始总结是什么意思？", "生成总结的来源是什么？", "帮我总结的依据是什么？",
		"开始生成是什么意思？", "发起总结的依据是什么？", "总结下是什么意思？",
		"总结一下是什么意思", "创建总结有什么限制？", "输出总结包含什么内容？",
		"start summary? 来源是什么", "summarize? what is in the docs", "“开始总结”是什么意思？",
		"这些内容有什么区别？",
	}
	affirmatives := []string{
		"请帮我总结一下群里有什么重要决定", "麻烦总结一下这些文档和群聊有什么风险",
		"你能总结一下文档和会话的区别吗", "先总结一下有什么风险", "帮忙总结一下有什么风险",
		"今天的会议请总结一下，有什么决议", "能不能帮我总结一下有什么风险",
		"please summarize what is in these documents", "could you summarize what is in the chat",
		"ok, summarize what is in these docs", "summarize这份文档和群聊有什么风险",
		"帮我总结一下群里有什么重要决定？", "总结一下本周有哪些内容需要跟进？",
	}
	for _, mixed := range []bool{false, true} {
		scope := summaryWorkspaceContext{Documents: []summaryWorkspaceDocument{{DocumentID: "d1"}}}
		if mixed {
			scope.SelectedChannels = []summaryWorkspaceChannel{{ChatID: "g1", ChatType: "group"}}
		}
		for _, message := range append(questions, affirmatives...) {
			intent := classifySummaryWorkspaceIntent(message, false)
			// Documents-only Explain semantics stay unchanged; mixed selection
			// is the compatibility execution surface, regardless of word order.
			want := service.SummaryRouteClarification
			if mixed || intent != service.SummaryIntentExplain {
				want = service.SummaryRoutePersonalWorkflow
			}
			got := deriveWorkspaceRoute(scope, service.SummaryActionChat, intent, false, false, hasNegatedSummaryRunIntent(message), true, true, false, WorkspaceSnapshot{}, true, true, true)
			if got != want {
				t.Fatalf("mixed=%t message=%q route=%s want=%s", mixed, message, got, want)
			}
		}
		for _, structured := range []bool{false, true} {
			for _, intent := range []service.SummaryIntent{service.SummaryIntentGenerate, service.SummaryIntentExplain, service.SummaryIntentRevise} {
				got := deriveWorkspaceRoute(scope, service.SummaryActionChat, intent, true, structured, true, true, true, false, WorkspaceSnapshot{}, true, true, true)
				if got != service.SummaryRouteClarification {
					t.Fatalf("negation bypassed: mixed=%t structured=%t intent=%s", mixed, structured, intent)
				}
			}
		}
	}
}

func TestDocumentClarificationReportsHardBlockersFirst(t *testing.T) {
	scope := summaryWorkspaceContext{
		Documents:        []summaryWorkspaceDocument{{DocumentID: "d1"}},
		SelectedChannels: []summaryWorkspaceChannel{{ChatID: "g1", ChatType: "group"}},
	}
	const message = "开始总结：先不要生成，先说明一下来源"
	if got := summaryWorkspaceClarificationReply(scope, message, false, true, true, "", true); got != "部分所选会话不可用，请调整后重试。" {
		t.Fatalf("permission blocker masked: %s", got)
	}
	if got := summaryWorkspaceClarificationReply(scope, message, true, true, true, "", true); got != "已按你的要求暂不生成总结。你可以继续调整所选文档、会话或总结要求。" {
		t.Fatalf("negation reply: %s", got)
	}
	scope.Participants = []summaryWorkspaceParticipant{{UserID: "other"}}
	if got := summaryWorkspaceClarificationReply(scope, message, true, true, false, "", true); got != "文档总结暂不支持多人协作，请移除参与者后再生成。" {
		t.Fatalf("participant blocker masked: %s", got)
	}
}
