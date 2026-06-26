package pipeline

import (
	"testing"
)

func TestHasTimeWord(t *testing.T) {
	tests := []struct {
		topic    string
		expected bool
	}{
		// 应该命中
		{"总结本周工作汇报", true},
		{"总结上周项目进展", true},
		{"总结最近7天的工作", true},
		{"总结昨天日报", true},
		{"周报", true},
		{"总结5.6至今的进展", true},
		{"总结过去一周的工作", true},
		
		// 不应该命中
		{"总结", false},
		{"总结本群的关键内容", false},
		{"总结这个聊天的关键内容", false},
		{"总结项目进展", false},
	}
	
	for _, tt := range tests {
		t.Run(tt.topic, func(t *testing.T) {
			got := hasTimeWord(tt.topic)
			if got != tt.expected {
				t.Errorf("hasTimeWord(%q) = %v, want %v", tt.topic, got, tt.expected)
			}
		})
	}
}

func TestHasPersonWord(t *testing.T) {
	tests := []struct {
		topic    string
		expected bool
	}{
		// 应该命中
		{"总结我的工作内容", true},
		{"帮我总结一下最近一周我的主要工作", true},
		{"总结大家的周报内容", true},
		{"总结团队工作", true},
		{"mengxi发给我的信息", true},
		{"和我有关的信息", true},
		{"@李晓同 透视分析", true},
		
		// 裸第一人称动词（应该命中，防止 shortcut 跳过）
		{"我说了什么", true},
		{"我发的内容", true},
		{"我讲了啥", true},
		{"我写的文档", true},
		{"我提的问题", true},
		{"我分享的内容", true},
		
		// 不应该命中
		{"总结", false},
		{"总结本群的关键内容", false},
		{"总结项目进展", false},
	}
	
	for _, tt := range tests {
		t.Run(tt.topic, func(t *testing.T) {
			got := hasPersonWord(tt.topic)
			if got != tt.expected {
				t.Errorf("hasPersonWord(%q) = %v, want %v", tt.topic, got, tt.expected)
			}
		})
	}
}

func TestHasComplexChannelWord(t *testing.T) {
	tests := []struct {
		topic    string
		expected bool
	}{
		// 应该命中
		{"总结这些群的工作", true},
		{"总结这几个群里的讨论", true},
		{"总结私聊消息", true},
		{"帮我总结下我和辉哥私信最近都聊了什么", true},
		
		// 不应该命中（简单频道词）
		{"总结本群的关键内容", false},
		{"总结这个群最近都讨论什么", false},
		{"总结群里的内容", false},
		{"总结", false},
	}
	
	for _, tt := range tests {
		t.Run(tt.topic, func(t *testing.T) {
			got := hasComplexChannelWord(tt.topic)
			if got != tt.expected {
				t.Errorf("hasComplexChannelWord(%q) = %v, want %v", tt.topic, got, tt.expected)
			}
		})
	}
}

func TestIsPureGenericTopic(t *testing.T) {
	tests := []struct {
		topic    string
		expected bool
	}{
		// 应该命中
		{"总结", true},
		{"总结一下", true},
		{"总结主题", true},
		{"概要", true},
		
		// 不应该命中
		{"周报", false},  // "周报" implies "本周", needs LLM to parse time
		{"总结本周工作", false},
		{"总结本群的关键内容", false},
		{"总结项目进展", false},
		{"总结我的工作", false},
	}
	
	for _, tt := range tests {
		t.Run(tt.topic, func(t *testing.T) {
			got := isPureGenericTopic(tt.topic)
			if got != tt.expected {
				t.Errorf("isPureGenericTopic(%q) = %v, want %v", tt.topic, got, tt.expected)
			}
		})
	}
}

func TestShouldSkipIntentRecognition(t *testing.T) {
	tests := []struct {
		name             string
		topic            string
		specifiedSources []string
		enableShortcut   bool
		expected         bool
	}{
		// 开关关闭
		{
			name:             "开关关闭",
			topic:            "总结",
			specifiedSources: nil,
			enableShortcut:   false,
			expected:         false,
		},
		
		// 条件1: 纯泛主题
		{
			name:             "纯泛主题-总结",
			topic:            "总结",
			specifiedSources: nil,
			enableShortcut:   true,
			expected:         true,
		},
		{
			name:             "纯泛主题-周报-不短路",
			topic:            "周报",
			specifiedSources: nil,
			enableShortcut:   true,
			expected:         false,  // "周报" has time_word, needs LLM
		},
		
		// 条件2: 简单频道约束
		{
			name:             "简单频道约束-有来源无时间人物",
			topic:            "总结本群的关键内容",
			specifiedSources: []string{"group_123"},
			enableShortcut:   true,
			expected:         true,
		},
		{
			name:             "简单频道约束-这个聊天",
			topic:            "总结这个聊天的关键内容",
			specifiedSources: []string{"group_123"},
			enableShortcut:   true,
			expected:         true,
		},
		
		// 不应该短路
		{
			name:             "有时间词-不短路",
			topic:            "总结本周工作",
			specifiedSources: []string{"group_123"},
			enableShortcut:   true,
			expected:         false,
		},
		{
			name:             "有人物词-不短路",
			topic:            "总结我的工作",
			specifiedSources: []string{"group_123"},
			enableShortcut:   true,
			expected:         false,
		},
		{
			name:             "有复杂频道词-不短路",
			topic:            "总结这些群的工作",
			specifiedSources: []string{"group_123"},
			enableShortcut:   true,
			expected:         false,
		},
		{
			name:             "无来源非泛主题-不短路",
			topic:            "总结项目进展",
			specifiedSources: nil,
			enableShortcut:   true,
			expected:         false,
		},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ShouldSkipIntentRecognition(tt.topic, tt.specifiedSources, tt.enableShortcut)
			if got != tt.expected {
				t.Errorf("ShouldSkipIntentRecognition(%q, %v, %v) = %v, want %v",
					tt.topic, tt.specifiedSources, tt.enableShortcut, got, tt.expected)
			}
		})
	}
}

func TestGetSkipReason(t *testing.T) {
	tests := []struct {
		topic            string
		specifiedSources []string
		expected         string
	}{
		{"总结", nil, "pure_generic_topic"},
		{"周报", nil, ""},  // "周报" has time_word, not skipped
		{"总结本群的关键内容", []string{"group_123"}, "simple_channel_constraint"},
		{"总结本周工作", []string{"group_123"}, ""},
		{"总结项目进展", nil, ""},
	}
	
	for _, tt := range tests {
		t.Run(tt.topic, func(t *testing.T) {
			got := GetSkipReason(tt.topic, tt.specifiedSources)
			if got != tt.expected {
				t.Errorf("GetSkipReason(%q, %v) = %q, want %q",
					tt.topic, tt.specifiedSources, got, tt.expected)
			}
		})
	}
}
