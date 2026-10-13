package handler

import "testing"

func TestDocumentBareContentRestriction(t *testing.T) {
	cases := []struct {
		message string
		want    bool
	}{
		{"不要生成表格", true},
		{"不需要总结附件内容", true},
		{"表格不用生成", true},
		{"不要生成表格和行动项", true},
		{"不要生成表格，不需要总结附件内容", true},
		{"do not generate a table", true},
		{"不要总结“附件内容”", true},
		{"", false},
		{"不要总结", false},     // The cancellation guard owns this case.
		{"不要生成未知对象", false}, // The uncertain-negation guard owns this case.
		{"总结一下，不要生成表格", false},
		{"不要生成表格，请总结本周进展", false},
		{"总结这份文档 表格不用生成", false},
		{"开始总结，表格不用生成", false},
		{"按风险等级组织内容", false},
		{"文档写着“不要生成表格”", false},
		{"不要生成表格是什么意思？", false},
	}
	for _, tc := range cases {
		t.Run(tc.message, func(t *testing.T) {
			if got := summaryWorkspaceBareContentRestriction(tc.message); got != tc.want {
				t.Fatalf("bare restriction = %v, want %v", got, tc.want)
			}
		})
	}
}
