package handler

import (
	"strings"
	"testing"
)

func TestStripAgentPreamble(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		want  string
	}{
		{
			name:  "empty input passes through",
			in:    "",
			want:  "",
		},
		{
			name: "typical bug case (task 51): opener then heading",
			in: "好的。根据引用的老总结内容,我现在将其转化为更结构化、去除口语化表达的专业版本。这是一份**迭代模式**的改写——我基于老内容的完整信息进行文字加工和结构优化,无需调用新工具。\n\n---\n\n## 📊 Summary 服务上线项目总结报告\n\n### 一、项目概览\n",
			want: "---\n\n## 📊 Summary 服务上线项目总结报告\n\n### 一、项目概览\n",
		},
		{
			name: "short opener then h1",
			in:   "好的,这是总结:\n\n# 项目上线\n内容",
			want: "# 项目上线\n内容",
		},
		{
			name: "short opener then h3",
			in:   "根据你的要求:\n\n### 章节 A\n内容",
			want: "### 章节 A\n内容",
		},
		{
			name: "opener then *** rule",
			in:   "好的:\n***\n# 标题",
			want: "***\n# 标题",
		},
		{
			name: "content already starts with heading — no strip",
			in:   "# 项目上线\n第一段内容",
			want: "# 项目上线\n第一段内容",
		},
		{
			name: "content starts with --- (front matter) — no strip",
			in:   "---\n# 项目\n内容",
			want: "---\n# 项目\n内容",
		},
		{
			name: "no heading in whole doc — no strip (pure prose)",
			in:   "这是一段没有标题的纯文字总结。所有内容都在一段里。",
			want: "这是一段没有标题的纯文字总结。所有内容都在一段里。",
		},
		{
			name: "long preamble (>= 500 chars) — no strip (looks like real content)",
			in:   strings.Repeat("这是一段很长的内容,应该被保留下来不被剥离。", 20) + "\n\n## 标题\n后续",
			want: strings.Repeat("这是一段很长的内容,应该被保留下来不被剥离。", 20) + "\n\n## 标题\n后续",
		},
		{
			name: "opener with markdown bold — still stripped",
			in:   "**明白了。** 这是一份改写:\n\n## 报告\n内容",
			want: "## 报告\n内容",
		},
		{
			name: "opener followed by heading immediately (no blank line)",
			in:   "好的:\n## 标题\n内容",
			want: "## 标题\n内容",
		},
		{
			name: "trailing space in preamble line does not break heading detection",
			in:   "好的:   \n\n## 标题\n内容",
			want: "## 标题\n内容",
		},
		{
			name: "malformed heading (# without space) does not trigger strip",
			in:   "好的:\n#not_a_heading_just_hash\ntext",
			want: "好的:\n#not_a_heading_just_hash\ntext",
		},
		{
			name: "7+ hash chars is not a valid heading",
			in:   "好的:\n####### too many\ntext",
			want: "好的:\n####### too many\ntext",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := stripAgentPreamble(tc.in)
			if got != tc.want {
				t.Errorf("stripAgentPreamble mismatch\n  input:  %q\n  want:   %q\n  got:    %q", tc.in, tc.want, got)
			}
		})
	}
}

func TestIsHeadingOrRule(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"# heading", true},
		{"## heading", true},
		{"###### heading", true},
		{"####### too many", false},
		{"#no-space", false},
		{"---", true},
		{"----", true},
		{"***", true},
		{"___", true},
		{"-- ", false}, // only 2 dashes
		{"--x", false}, // not all same
		{"plain text", false},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := isHeadingOrRule(tc.in); got != tc.want {
				t.Errorf("isHeadingOrRule(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
