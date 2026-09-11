package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const maxToolArgumentRepairs = 2

// InvalidToolArgumentsError is terminal at the request boundary. The runner
// first attempts bounded, in-place repair; replaying the whole request is not
// appropriate if the model still cannot produce a valid tool call.
type InvalidToolArgumentsError struct {
	Reason       string
	SyntaxOffset int64
}

func (e *InvalidToolArgumentsError) Error() string {
	if e.Reason == "" {
		return "model tool arguments must be a valid JSON object"
	}
	return fmt.Sprintf("model tool arguments must be a valid JSON object (reason=%s syntax_offset=%d)", e.Reason, e.SyntaxOffset)
}

func validToolArguments(calls []ToolCall) bool {
	return invalidToolArguments(calls) == nil
}

func invalidToolArguments(calls []ToolCall) *InvalidToolArgumentsError {
	for _, call := range calls {
		args := strings.TrimSpace(call.Function.Arguments)
		if args == "" {
			return &InvalidToolArgumentsError{Reason: "empty"}
		}
		var raw json.RawMessage
		if err := json.Unmarshal([]byte(args), &raw); err != nil {
			var syntax *json.SyntaxError
			if errors.As(err, &syntax) {
				return &InvalidToolArgumentsError{Reason: "invalid_json", SyntaxOffset: syntax.Offset}
			}
			return &InvalidToolArgumentsError{Reason: "invalid_json"}
		}
		if args[0] != '{' {
			return &InvalidToolArgumentsError{Reason: "non_object"}
		}
	}
	return nil
}

// This instruction is transient. Do not include the rejected call or draft:
// replaying malformed arguments prevents the gateway from running the model
// that could repair them. Previously completed tool results remain in context.
const toolArgumentRepairInstruction = `上一轮工具调用的 arguments 不是合法的 JSON 对象，该轮工具均未执行。请基于已有工具结果重新提交合法的工具调用参数，不要重复已经成功的读取或分析。参数必须是完整 JSON 对象，字符串中的换行、双引号和反斜杠必须正确转义；不要使用 Markdown 代码围栏。`
