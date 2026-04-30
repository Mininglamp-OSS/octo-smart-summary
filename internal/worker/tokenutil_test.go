package worker

import (
	"strings"
	"testing"
)

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		name               string
		content            string
		charsPerTokenCJK   int
		charsPerTokenASCII int
		wantAtLeast        int
	}{
		{"empty string", "", 1, 4, 50},
		{"pure ascii short", "hello world", 1, 4, 50 + 2},
		{"pure cjk", "你好世界", 1, 4, 50 + 4},
		{"mixed cjk and ascii", "hello 你好", 1, 4, 50 + 1 + 2},
		{"defensive zero cjk ratio", "你好", 0, 4, 50 + 2},
		{"defensive zero ascii ratio", "abcd", 1, 0, 50 + 1},
		{"defensive negative cjk ratio", "你好", -1, 4, 50 + 2},
		{"long cjk", strings.Repeat("字", 1000), 1, 4, 50 + 1000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := estimateTokens(tt.content, tt.charsPerTokenCJK, tt.charsPerTokenASCII)
			if got < tt.wantAtLeast {
				t.Errorf("estimateTokens(%q, %d, %d) = %d, want >= %d",
					tt.content, tt.charsPerTokenCJK, tt.charsPerTokenASCII, got, tt.wantAtLeast)
			}
		})
	}
}

func TestSanitizeErrorForUser(t *testing.T) {
	tests := []struct {
		name   string
		errMsg string
		want   string
	}{
		{"llm api error", "LLM API error: status=401 body=invalid key", "AI 服务暂时不可用，请稍后重试"},
		{"context deadline", "context deadline exceeded", "AI 处理超时，请稍后重试"},
		{"all chunks failed", "all 3 chunk(s) failed during Map phase (LLM unreachable)", "AI 服务暂时不可用，所有分片处理失败"},
		{"unknown short", "some random error", "AI 处理失败，请稍后重试"},
		{"unknown dsn leak", "dial tcp 10.0.0.5:3306: user=root password=secret", "AI 处理失败，请稍后重试"},
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
