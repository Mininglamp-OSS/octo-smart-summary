package handler

import (
	"fmt"
	"slices"
	"strings"
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
		{name: "missing template", origin: summaryWorkspaceInputTemplate, message: template.Requirement, want: summaryWorkspaceInputUser},
		{name: "mismatched template", origin: summaryWorkspaceInputTemplate, message: "不同的要求", template: template, want: summaryWorkspaceInputUser},
		{name: "unverified system", origin: summaryWorkspaceInputSystemIntent, message: template.Requirement, want: summaryWorkspaceInputUser},
		{name: "unknown origin", origin: "model", message: "总结", reject: true},
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
			wantRequirement := tc.message
			if tc.template != nil && origin == summaryWorkspaceInputUser {
				wantRequirement = tc.template.Requirement + "\n\n" + tc.message
			}
			if got := summaryWorkspaceExecutionRequirement(scope, tc.message, origin); got != wantRequirement {
				t.Fatalf("requirement lost or duplicated: %q", got)
			}
		})
	}
}

func TestDocumentOriginCaseAndCommandCompatibility(t *testing.T) {
	template := &summaryWorkspaceTemplate{TemplateID: "weekly", Requirement: "Weekly Risk Review"}
	for _, raw := range []string{"", summaryWorkspaceInputTemplate} {
		for _, message := range []string{"Weekly Risk Review", "weekly risk review", "WEEKLY RISK REVIEW"} {
			got, err := normalizeSummaryWorkspaceInputOrigin(raw, summaryWorkspaceContext{Template: template}, message)
			if err != nil || got != summaryWorkspaceInputTemplate {
				t.Fatalf("raw=%q message=%q got=%q err=%v", raw, message, got, err)
			}
			if requirement := summaryWorkspaceExecutionRequirement(summaryWorkspaceContext{Template: template}, message, got); requirement != template.Requirement {
				t.Fatalf("case-normalized template duplicated: %q", requirement)
			}
		}
	}
	for _, command := range summaryWorkspaceExecutionCommands {
		for _, raw := range []string{"", summaryWorkspaceInputSystemIntent} {
			got, err := normalizeSummaryWorkspaceInputOrigin(raw, summaryWorkspaceContext{}, strings.ToUpper(command))
			want := summaryWorkspaceInputSystemIntent
			if raw == "" && !isSummaryWorkspaceLegacyButtonMessage(command) {
				want = summaryWorkspaceInputUser
			}
			if err != nil || got != want {
				t.Fatalf("command=%q raw=%q got=%q err=%v", command, raw, got, err)
			}
		}
	}
	for _, message := range []string{"新文案：总结选中内容", "Résumez les documents sélectionnés", "開始總結所選內容"} {
		got, err := normalizeSummaryWorkspaceInputOrigin(summaryWorkspaceInputSystemIntent, summaryWorkspaceContext{}, message)
		if err != nil || got != summaryWorkspaceInputUser {
			t.Fatalf("copy drift must downgrade, not reject: message=%q got=%q err=%v", message, got, err)
		}
	}
}

func TestTypedChatExecutionCommandRemainsConversational(t *testing.T) {
	for _, message := range []string{"开始总结", "start summary", "generate summary now"} {
		origin, err := normalizeSummaryWorkspaceInputOrigin("", summaryWorkspaceContext{}, message)
		if err != nil || origin != summaryWorkspaceInputUser ||
			!summaryWorkspaceShouldOpenScopeAgent(service.SummaryActionChat, origin, classifySummaryWorkspaceIntent(message, false)) {
			t.Fatalf("typed chat command changed route: message=%q origin=%q err=%v", message, origin, err)
		}
	}
}

func TestTypedChatContentConstraintOriginBoundary(t *testing.T) {
	for _, message := range []string{"不要生成表格", "请总结会议要点，不要生成行动项"} {
		for _, tc := range []struct {
			origin string
			want   service.SummaryRoute
		}{
			{summaryWorkspaceInputUser, service.SummaryRouteAgentPreview},
			{summaryWorkspaceInputTemplate, service.SummaryRoutePersonalWorkflow},
			// A non-command system declaration downgrades to conversational user.
			{summaryWorkspaceInputSystemIntent, service.SummaryRouteAgentPreview},
		} {
			scope := summaryWorkspaceContext{
				SelectedChannels: []summaryWorkspaceChannel{{ChatID: "group-a", ChatType: "group"}},
				Template:         &summaryWorkspaceTemplate{TemplateID: "content", Requirement: message},
			}
			origin, err := normalizeSummaryWorkspaceInputOrigin(tc.origin, scope, message)
			if err != nil {
				t.Fatal(err)
			}
			intent := classifySummaryWorkspaceIntent(message, false)
			open := summaryWorkspaceShouldOpenScopeAgent(service.SummaryActionChat, origin, intent)
			route := deriveWorkspaceRoute(workspaceRouteInput{
				Context: scope, Action: service.SummaryActionChat, Intent: intent,
				SelectedSourceExplicit: true, HasRequirement: true, OpenScopeAgent: open,
				ParticipantsValid: true, SourcesValid: true, ReferencesValid: true,
			})
			route = workspaceRouteAfterOpenScopeAgentOverride(scope, route, open, WorkspaceSnapshot{})
			if route != tc.want {
				t.Fatalf("message=%q origin=%s route=%s want=%s", message, tc.origin, route, tc.want)
			}
		}
	}
}

var documentScopedNegationRequests = []string{
	"总结一下这些文档和群聊的风险，不要生成行动项",
	"帮我总结文档和会话，不需要总结附件内容",
	"请总结本周进展，无需总结已关闭的任务",
	"总结一下，不要生成表格",
	"summarize the docs and chat, but do not generate action items",
	"不要生成表格，请总结本周进展",
	"请总结，不要总结附件内容",
	"please summarize the docs without generating a table",
	"请总结，不要生成总结表格",
	"summarize the docs, don't generate a summary table",
	"文档里写着“不要生成总结”，请总结文档",
	"文档里写着\"不要生成总结。先不要总结\"，请总结文档",
	"文档里写着'不要总结这些内容'，请总结文档",
	"文档里写着'不要总结。不要生成总结'，请总结文档",
	"文档里写着「不要总结这些内容」，请总结文档",
	"文档里写着`不要总结这些内容`，请总结文档",
	"请总结，文档里写着“不要生成总结”",
	"总结一下，不要生成正式总结表格",
	"总结一下，不要生成完整总结附件",
	"请你总结这些文档 不要生成行动项",
	"summarize these docs, please do  not  generate  action items",
}

func TestDocumentRouteRejectsUnsupportedCombinations(t *testing.T) {
	for _, scope := range []summaryWorkspaceContext{
		{Documents: []summaryWorkspaceDocument{{DocumentID: "d1"}}, Participants: []summaryWorkspaceParticipant{{UserID: "other"}}},
		{Documents: []summaryWorkspaceDocument{{DocumentID: "d1"}}, ReferencedTaskIDs: []int64{1}},
	} {
		for _, intent := range []service.SummaryIntent{service.SummaryIntentGenerate, service.SummaryIntentExplain} {
			in := workspaceRouteInput{
				Context: scope, Action: service.SummaryActionChat, Intent: intent,
				VerifiedDocumentTemplate: true, SelectedSourceExplicit: true, HasRequirement: true,
				ParticipantsValid: true, SourcesValid: true, ReferencesValid: true,
			}
			// Defensive helper contract, separate from the real-handler tests:
			// even valid references/participants cannot widen document execution.
			if got := deriveWorkspaceRoute(in); got != service.SummaryRouteClarification {
				t.Fatalf("unsupported scope executed: %+v intent=%s got=%s", scope, intent, got)
			}
		}
	}
}

var documentNegationQuestions = []string{
	"不要生成总结是什么意思？",
	"“先不要总结”是什么意思？",
	`what does "do not summarize" mean?`,
}

var documentExecutionNegations = []string{
	"先不要生成总结", "开始总结：先不要生成，先说明一下来源",
	"请先不要生成总结", "暂时不要总结", "现在别生成总结了",
	"总结一下，不要生成总结", "请总结，但现在先不生成",
	"无需总结", "不需要生成总结", "请不要总结。",
	"do not summarize", "please don't generate a summary yet",
	"start summary: please do not generate a summary",
	"summarize the docs, but do not generate a summary",
	"不要总结这些内容", "不要总结这些文档", "别总结这些内容",
	"不需要总结这些内容", "不要生成这些内容的总结",
	"请不要总结这些内容", "先不要总结这些内容",
	"总结先不要生成", "这次的总结不要生成", "文档总结不要生成",
	"本周的总结暂不生成", "周报先不要总结", "the summary — do not generate",
	"总结一下这些文档、不要生成总结", "总结一下这些文档 不要生成总结",
	"总结一下这些文档　不要生成总结", "do  not  generate  a  summary",
	"请你先不要总结", "请您先不要生成总结", "麻烦先别总结",
	"帮我先别生成总结", "能不能先不要生成总结", "我希望暂时不要总结",
	"建议先别总结", "可否先不要总结", "最好先不要生成总结",
	"总结一下这些文档，麻烦别生成", "先不要生成正式总结",
	"先不要生成完整总结", "🙏 先不要总结", "“先不要总结”",
	`"先不要总结"`, "'先不要总结'", "不用生成总结", "不要再生成总结",
	"please do not summarize these documents",
	"不要总结哦", "不要总结啦", "不要总结了吧", "请先不要总结哦",
	"不要总结呢", "不要总结呀", "不要总结啊", "不要总结~", "不要总结……",
	"不要生成总结哦", "别总结这些内容啦", "不要总结谢谢", "不要总结好吗",
	"不要总结行吗", "不要总结可以吗", "不要总结ok", "don't summarize please",
	"劳驾先不要总结", "拜托先别总结", "烦请不要生成总结", "眼下不要总结",
	"我的意思是不要总结", "结论是不要生成总结", "所以不要总结这些文档",
	"【先不要总结】", "-- 先不要总结",
	"\uFFFC不要总结", "不要总结\uFFFC", "不要总结\uFFFC这些内容",
	"不要总结全部", "不要总结全部内容", "不要总结全部文档",
	"不要总结嘛", "不要总结哈", "不要总结呗", "不要总结咯", "不要总结喔",
	"don't summarize thanks", "don't summarize ok",
}

func TestDocumentCancellationDecorations(t *testing.T) {
	// Metamorphic checks: presentation/politeness must not change the object.
	for _, tc := range []struct {
		message string
		want    bool
	}{
		{"不要总结", true}, {"不要生成总结", true}, {"别总结这些文档", true},
		{"不要生成表格", false}, {"不要生成行动项", false}, {"不需要总结附件内容", false},
		{"不要生成正式总结表格", false},
	} {
		for _, prefix := range []string{"", "请你先", "🙏 ", "-- ", "【", "\uFFFC"} {
			for _, suffix := range []string{"", "哦", "了吧", "谢谢", "可以吗", "……", "】", "\uFFFC", "哦，谢谢！"} {
				message := prefix + tc.message + suffix
				if got := hasNegatedSummaryRunIntent(message); got != tc.want {
					t.Errorf("%q: got %t want %t", message, got, tc.want)
				}
			}
		}
	}
}

func TestDocumentQuotedCancellationBoundaries(t *testing.T) {
	for _, message := range []string{
		`文档写着“说明。不要总结”，请总结`,
		`文档写着“说明。不要总结。结束”，请总结`,
		`不要总结“附件内容”，请总结正文`,
		`请总结文档，内容是"说明。不要总结"`,
	} {
		if hasNegatedSummaryRunIntent(message) {
			t.Errorf("quoted data became cancellation: %q", message)
		}
	}
}

func BenchmarkDocumentUnclosedQuotes(b *testing.B) {
	for _, size := range []int{1024, 8192} {
		message := strings.Repeat("“", size)
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				hasNegatedSummaryRunIntent(message)
			}
		})
	}
}

func TestDocumentNegationDistinguishesExecutionFromContent(t *testing.T) {
	for _, tc := range []struct {
		messages []string
		want     bool
	}{{documentScopedNegationRequests, false}, {documentNegationQuestions, false}, {documentExecutionNegations, true}} {
		for _, message := range tc.messages {
			if got := hasNegatedSummaryRunIntent(message); got != tc.want {
				t.Errorf("message=%q negated=%t want=%t", message, got, tc.want)
			}
		}
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
		// These lists have literal expectations; do not use the production
		// classifier to decide whether its own result is correct.
		clarifying := append(append([]string{}, questions...), documentNegationQuestions...)
		clarifying = append(clarifying, affirmatives...)
		for _, message := range append(clarifying, documentScopedNegationRequests...) {
			intent := classifySummaryWorkspaceIntent(message, false)
			want := service.SummaryRouteClarification
			if mixed || slices.Contains(documentScopedNegationRequests, message) {
				want = service.SummaryRoutePersonalWorkflow
			}
			got := deriveWorkspaceRoute(workspaceRouteInput{
				Context: scope, Action: service.SummaryActionChat, Intent: intent,
				NegatedRun: hasNegatedSummaryRunIntent(message), SelectedSourceExplicit: true,
				HasRequirement: true, ParticipantsValid: true, SourcesValid: true, ReferencesValid: true,
			})
			if got != want {
				t.Fatalf("mixed=%t message=%q route=%s want=%s", mixed, message, got, want)
			}
		}
		for _, structured := range []bool{false, true} {
			for _, intent := range []service.SummaryIntent{service.SummaryIntentGenerate, service.SummaryIntentExplain, service.SummaryIntentRevise} {
				got := deriveWorkspaceRoute(workspaceRouteInput{
					Context: scope, Action: service.SummaryActionChat, Intent: intent,
					VerifiedDocumentTemplate: structured, NegatedRun: true, SelectedSourceExplicit: true,
					HasRequirement: true, ParticipantsValid: true, SourcesValid: true, ReferencesValid: true,
				})
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
	in := workspaceRouteInput{Context: scope, NegatedRun: hasNegatedSummaryRunIntent(message), ReferencesValid: true, ParticipantsValid: true, HasRequirement: true}
	if got := summaryWorkspaceClarificationReply(in, ""); got != "部分所选会话不可用，请调整后重试。" {
		t.Fatalf("permission blocker masked: %s", got)
	}
	in.SourcesValid = true
	if got := summaryWorkspaceClarificationReply(in, ""); got != "要现在生成总结吗？如果要继续，请移除暂停生成的要求后重新提交。" {
		t.Fatalf("negation reply: %s", got)
	}
}
