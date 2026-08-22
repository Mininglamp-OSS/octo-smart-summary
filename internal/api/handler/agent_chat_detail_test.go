package handler

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// TestSafeErrorDetail 覆盖 500 response Detail 字段的白名单规则(#agent_chat.go
// safeErrorDetail)。白名单存在的意义:让客户端 F12 能区分"我请求超时"和"服务端崩了"
// 两种子情况,同时严格不透传含 URL/IP/token 的 error 到客户端。
func TestSafeErrorDetail(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil returns empty", nil, ""},
		{
			"context deadline exceeded is whitelisted",
			context.DeadlineExceeded,
			"context deadline exceeded",
		},
		{
			"wrapped context deadline exceeded is whitelisted",
			fmt.Errorf("runner step: %w", context.DeadlineExceeded),
			"context deadline exceeded",
		},
		{
			"context canceled is whitelisted",
			context.Canceled,
			"context canceled",
		},
		{
			"max steps exceeded is whitelisted",
			errors.New("max steps exceeded"),
			"max steps exceeded",
		},
		{
			"empty response guard is whitelisted",
			errors.New("LLM returned empty response with no tool_calls at final step"),
			"LLM returned empty response with no tool_calls at final step",
		},
		{
			// PR #208 round-5 P2-10. The Reduce completeness gate is a run
			// outcome the user can act on ("the model never merged its Map
			// results"), not a server fault. Collapsing it to "internal error"
			// told them nothing.
			"reduce completeness gate is whitelisted",
			errors.New("successful Map retries and Reduce required before final answer"),
			"successful Map retries and Reduce required before final answer",
		},
		{
			"wrapped reduce completeness gate is whitelisted",
			fmt.Errorf("agent run: %w", errors.New("successful Map retries and Reduce required before final answer")),
			"successful Map retries and Reduce required before final answer",
		},
		{
			"unknown profile is whitelisted",
			errors.New(`unknown agent profile "summary_x"`),
			"unknown agent profile",
		},
		{
			// SECURITY: error containing internal URL must NOT leak.
			"internal LLM gateway URL is redacted",
			errors.New("POST https://llm-gateway.internal.mlamp.cn/v1/chat/completions returned 500"),
			"internal error",
		},
		{
			// SECURITY: error containing IP must NOT leak.
			"internal IP is redacted",
			errors.New("dial tcp 10.0.0.42:8080: connection refused"),
			"internal error",
		},
		{
			// SECURITY: DB path fragments must NOT leak.
			"db error message is redacted",
			errors.New("gorm: record not found in summary_task where id=999"),
			"internal error",
		},
		{
			"unknown error collapses to internal error",
			errors.New("something weird happened"),
			"internal error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := safeErrorDetail(tc.err)
			if got != tc.want {
				t.Errorf("safeErrorDetail(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
