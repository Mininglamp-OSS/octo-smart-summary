package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// 一期真实工具：验证多跳 + 结构化 args 回喂。一期不接真实下游，重点跑通回环。

type timeRangeArgs struct {
	HasTimeExpr bool   `json:"has_time_expr"`
	Start       string `json:"start"`
	End         string `json:"end"`
	Reasoning   string `json:"reasoning"`
}

// ExtractTimeRangeTool 返回 extract_time_range 的 schema + handler。
func ExtractTimeRangeTool() (Tool, Handler) {
	schema := Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "extract_time_range",
			Description: "从用户表述中抽取时间范围。若无明确时间表达，将 has_time_expr 置为 false。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"has_time_expr": map[string]any{"type": "boolean", "description": "是否存在时间表达"},
					"start":         map[string]any{"type": "string", "description": "起始时间(RFC3339)"},
					"end":           map[string]any{"type": "string", "description": "结束时间(RFC3339)"},
					"reasoning":     map[string]any{"type": "string", "description": "推理说明"},
				},
				"required": []string{"has_time_expr"},
			},
		},
	}

	handler := func(ctx context.Context, args json.RawMessage) (string, error) {
		var a timeRangeArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		if !a.HasTimeExpr {
			return "未检测到明确的时间表达。", nil
		}
		return fmt.Sprintf("已确认时间范围: %s ~ %s (依据: %s)", a.Start, a.End, a.Reasoning), nil
	}
	return schema, handler
}

// GetCurrentTimeTool 返回 get_current_time 的 schema + handler（无参）。
// 与 extract_time_range 组合可构造"先问当前时间再算范围"的两跳场景。
func GetCurrentTimeTool() (Tool, Handler) {
	schema := Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "get_current_time",
			Description: "获取当前时间(RFC3339)。当需要相对时间(如'昨天''上周')时先调用它。",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}

	handler := func(ctx context.Context, args json.RawMessage) (string, error) {
		return time.Now().Format(time.RFC3339), nil
	}
	return schema, handler
}
