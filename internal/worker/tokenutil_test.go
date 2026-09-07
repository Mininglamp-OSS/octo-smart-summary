package worker

import (
	"testing"
)

func TestSanitizeErrorForUser(t *testing.T) {
	tests := []struct {
		name   string
		errMsg string
		want   string
	}{
		{"llm api error", "LLM API error: status=401 body=invalid key", "AI 服务暂时不可用，请稍后重试"},
		{"context deadline", "context deadline exceeded", "AI 处理超时，请稍后重试"},
		{"all chunks failed", "all 3 chunk(s) failed during Map phase (LLM unreachable)", "AI 服务暂时不可用，所有分片处理失败"},
		{"source outside space", "explicit summary source is unavailable in the current space", "所选聊天不属于当前空间或你已失去访问权限，请重新选择"},
		{"source not shared", "explicit summary source is not shared by every participant", "所选聊天并非所有参与者都可访问，请调整聊天或参与者"},
		{"participant has no selected source", "no selected summary source is available to this participant", "你已无法访问任何所选聊天，无法生成个人总结"},
		{"unknown short", "some random error", "AI 处理失败，请稍后重试"},
		{"unknown dsn leak", "dial tcp 192.0.2.1:3306: user=testuser password=***", "AI 处理失败，请稍后重试"},
		{"unknown stack trace", "goroutine 12 [running]: runtime.gopanic(...)", "AI 处理失败，请稍后重试"},
		{"empty", "", "AI 处理失败，请稍后重试"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeErrorForUser(tt.errMsg); got != tt.want {
				t.Errorf("sanitizeErrorForUser(%q) = %q, want %q", tt.errMsg, got, tt.want)
			}
		})
	}
}
