# Issue #34 设计文档 V5：Function Call 统一架构 + 按主题目标人物过滤

## 概述

本文档覆盖两个目标：
1. **Issue #34 修复**：`FilterWithContext` 按错误的 UID 过滤 → 用 LLM 语义匹配解析 topic 中的目标人物
2. **统一 Function Call 基础设施**：将结构化 LLM 输出从「Prompt 要求返回 JSON → 手动清理 markdown fence → json.Unmarshal」改为「OpenAI Function Calling 强制输出 → 协议保证 valid JSON → json.Unmarshal」

---

## 1. 根因分析（Issue #34）

### 问题链路

```
用户请求 topic="辉哥的主要发言内容"
    ↓
handler/task.go:187  — participants 为空 → 默认填充 creator UID
    ↓
personal_processor.go:79  — executePersonalPipeline(ctx, task, participant.UserID)
                             其中 participant.UserID = creator UID
    ↓
personal_processor.go:256 — pipeline.FilterWithContext(messages, userID, contextWindow)
                             userID = creator UID（而非"辉哥"）
    ↓
filter.go:11 — 仅保留 m.SenderUID == userID 的消息
    ↓
结果：creator 在该频道从未发言 → 374 条消息过滤为 0 → 空输出
```

### 根因

`FilterWithContext` 假设 participant = 需要过滤的目标人物。但当 topic 指向他人发言时（"辉哥的主要发言内容"），系统仍用 creator UID 过滤，与主题意图矛盾。

---

## 2. 方案概述

### 核心思路

新增 `ResolveTopicTarget` 调用，通过 LLM Function Call 语义解析 topic 中指向的目标人物，将解析结果（`targetUIDs`）传入 `FilterWithContext`，替代硬编码的 creator UID。

### 两个 Function Call 调用点

| # | 函数 | 文件 | 用途 | 当前方式 |
|---|------|------|------|---------|
| 1 | `PreRetrievalNarrow` | `internal/pipeline/narrow.go` | 从 topic 中提取时间表达式 | prompt→`trimMarkdownCodeFence`→`json.Unmarshal` |
| 2 | `ResolveTopicTarget` | **新增** `internal/pipeline/resolve_topic.go` | 语义解析 topic 中的目标人物 | N/A（新函数直接用 Function Call） |

> **注**：`NarrowByTopic` 存在于代码中但当前从未被调用（Layer 3 在 personal pipeline 中跳过），**不在本 PR 范围内**。后续如果启用该函数，可作为第三个 Function Call 调用点改造（见第 13 节 Future Work）。

### 为什么用 Function Call

| 问题 | 表现 |
|------|------|
| Markdown fence 污染 | LLM 偶尔返回 ` ```json ... ``` `，需要 `trimMarkdownCodeFence` 手动清理 |
| 格式不稳定 | 不同模型/温度下可能返回带注释的 JSON 或附加解释文字 |
| 无 schema 验证 | 缺字段或类型错误只能在 runtime unmarshal 时发现 |

OpenAI Function Calling（`tools` + `tool_choice`）规范保证：
- `choices[0].message.tool_calls[0].function.arguments` 是合法 JSON 字符串
- JSON 结构严格遵循 `parameters` 中声明的 JSON Schema
- 无 markdown fence、无多余文本

### 双保险：Prompt 指令 + `tool_choice` 协议强制

即使 `tool_choice` 已在协议层强制 LLM 调用指定函数，每个结构化调用的 system prompt **末尾** 仍须追加一条明确指令要求 LLM 调用 tool：

| 调用点 | Prompt 末尾追加指令 |
|--------|-------------------|
| `PreRetrievalNarrow` | `你必须调用 extract_time_range 工具来返回结果，不要以文本形式回复。` |
| `ResolveTopicTarget` | `你必须调用 resolve_topic_target 工具来返回结果，不要以文本形式回复。` |

**原因**：Belt-and-suspenders 策略。`tool_choice` 是协议级约束，prompt 指令是语义级约束，二者配合可以在不同 LLM 网关实现中最大化成功率。

### 统一日志规范

所有 `CallWithTools` 调用必须记录三要素：**Input**（工具名 + 关键参数）、**Output**（解析结果或错误）、**Duration**（耗时 ms）。

**格式规范**：

成功：
```
[pipeline] CallWithTools: tool=<tool_name> input={<key_params>} took <ms>ms result={<parsed_fields>}
```

失败：
```
[pipeline] CallWithTools: tool=<tool_name> input={<key_params>} took <ms>ms error=<error_message>, fallback to <behavior>
```

**示例**：
```
[pipeline] CallWithTools: tool=extract_time_range input={topic:"辉哥的发言"} took 1523ms result={has_time_expr:false}
[pipeline] CallWithTools: tool=resolve_topic_target input={topic:"辉哥的发言", members:5} took 2100ms result={has_target:true, uids:["uid_xxx"]}
[pipeline] CallWithTools: tool=resolve_topic_target input={topic:"辉哥的发言", members:5} took 15000ms error=context deadline exceeded, fallback to creator
```

**原则**：
- 不记录完整 prompt（避免日志膨胀），只记录能唯一标识调用的关键参数
- Duration 在 `CallWithTools` 底层和调用方都记录（底层记录含重试的总耗时，调用方记录业务侧视角耗时）
- 错误日志必须包含 fallback 行为描述

---

## 3. 新增基础设施：`CallWithTools` 方法

### 3.1 导出 `ChatMessage` 类型

**文件**：`internal/service/llm.go`

将 `chatMessage` 重命名为 `ChatMessage` 以便跨包使用：

```go
// ChatMessage represents a single message in a chat completion request.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
```

同步更新 `llm.go` 内所有引用（`chatRequest`、`Call`、`CallMap` 等方法签名中的 `[]chatMessage` → `[]ChatMessage`）。

### 3.2 新增类型定义

**文件**：`internal/service/llm.go`

```go
// ToolFunction describes an OpenAI function calling tool definition.
type ToolFunction struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  interface{} `json:"parameters"`
}

// Tool wraps ToolFunction in the OpenAI tool format.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolChoice forces the LLM to call a specific function.
type ToolChoice struct {
	Type     string             `json:"type"`
	Function ToolChoiceFunction `json:"function"`
}

// ToolChoiceFunction specifies the function name for tool_choice.
type ToolChoiceFunction struct {
	Name string `json:"name"`
}

type chatRequestWithTools struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
	Tools       []Tool        `json:"tools"`
	ToolChoice  ToolChoice    `json:"tool_choice"`
}

type chatResponseWithTools struct {
	Choices []struct {
		Message struct {
			ToolCalls []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}
```

### 3.3 `CallWithTools` 方法

```go
// CallWithTools makes a chat completion request with forced function calling.
// Returns the raw JSON string from tool_calls[0].function.arguments and token count.
// Logs input (tool name, key params), output/error, and duration for every call.
func (c *LLMClient) CallWithTools(ctx context.Context, messages []ChatMessage, tools []Tool, forceFn string, temperature float64) (string, int, error) {
	log.Printf("[llm] CallWithTools: tool=%s temperature=%.2f model=%s", forceFn, temperature, c.model)
	start := time.Now()

	reqBody := chatRequestWithTools{
		Model:       c.model,
		Messages:    messages,
		Temperature: temperature,
		MaxTokens:   c.maxTokens,
		Tools:       tools,
		ToolChoice: ToolChoice{
			Type:     "function",
			Function: ToolChoiceFunction{Name: forceFn},
		},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", 0, fmt.Errorf("marshal request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(1<<uint(attempt-1)) * time.Second)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return "", 0, fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.client.Do(req)
		if err != nil {
			lastErr = err
			log.Printf("[llm] CallWithTools attempt %d network error: %v", attempt+1, err)
			continue
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("read response body: %w", err)
			continue
		}

		// Only retry on 5xx and 429; 4xx (except 429) are permanent errors
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("LLM API error: status=%d body=%s", resp.StatusCode, string(respBody))
			log.Printf("[llm] CallWithTools attempt %d: %v", attempt+1, lastErr)
			if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 429 {
				return "", 0, lastErr
			}
			continue
		}

		var chatResp chatResponseWithTools
		if err := json.Unmarshal(respBody, &chatResp); err != nil {
			lastErr = fmt.Errorf("unmarshal response: %w", err)
			continue
		}

		if len(chatResp.Choices) == 0 {
			lastErr = fmt.Errorf("LLM returned no choices")
			continue
		}
		if len(chatResp.Choices[0].Message.ToolCalls) == 0 {
			lastErr = fmt.Errorf("LLM returned no tool_calls")
			continue
		}

		args := chatResp.Choices[0].Message.ToolCalls[0].Function.Arguments
		if args == "" {
			lastErr = fmt.Errorf("LLM returned empty arguments")
			continue
		}

		log.Printf("[llm] CallWithTools: tool=%s took %dms tokens=%d", forceFn, time.Since(start).Milliseconds(), chatResp.Usage.TotalTokens)
		return args, chatResp.Usage.TotalTokens, nil
	}
	elapsed := time.Since(start).Milliseconds()
	log.Printf("[llm] CallWithTools: tool=%s took %dms error=%v", forceFn, elapsed, lastErr)
	return "", 0, fmt.Errorf("CallWithTools failed after 3 attempts: %w", lastErr)
}
```

**重试策略说明**（Fix M1）：
- 网络错误：重试
- HTTP 5xx：重试（服务端临时故障）
- HTTP 429：重试（限流，指数退避可生效）
- HTTP 4xx（非 429）：**不重试**，立即返回错误（如 400 Bad Request、401 Unauthorized、403 Forbidden 均为永久性错误）

### 3.4 `CallRaw` 保留

`PostRetrievalNarrow` 仍通过 `LLMCallFn`（包装 `CallRaw`）调用。**`CallRaw` 不删除**，保持两种调用方式共存：

| 方法 | 用途 | 调用方 |
|------|------|--------|
| `CallRaw` | 简单文本输入/输出 | `PostRetrievalNarrow`（通过 `LLMCallFn`） |
| `CallWithTools` | 结构化 Function Call 输出 | `PreRetrievalNarrow`、`ResolveTopicTarget` |
| `Call` | Map/Reduce 多消息对话 | `CallMap`、`CallReduce` |

---

## 4. Tool 定义与改造

### 4.1 `PreRetrievalNarrow` → Function Call

#### Tool Schema

```go
var extractTimeRangeTool = service.Tool{
	Type: "function",
	Function: service.ToolFunction{
		Name:        "extract_time_range",
		Description: "从用户输入的主题中提取时间表达式并转换为精确的时间范围",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"has_time_expr": map[string]interface{}{
					"type":        "boolean",
					"description": "主题中是否包含时间表达式",
				},
				"start": map[string]interface{}{
					"type":        "string",
					"description": "时间范围起始，RFC3339 格式（如 2026-05-01T00:00:00+08:00）。无时间表达式时为空字符串",
				},
				"end": map[string]interface{}{
					"type":        "string",
					"description": "时间范围结束，RFC3339 格式。无时间表达式时为空字符串",
				},
				"reasoning": map[string]interface{}{
					"type":        "string",
					"description": "一句话解释判断依据",
				},
			},
			"required": []string{"has_time_expr", "start", "end", "reasoning"},
		},
	},
}
```

#### 改造后完整实现

**文件**：`internal/pipeline/narrow.go`

```go
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

// TimeNarrowResult represents the structured output from time-range extraction.
type TimeNarrowResult struct {
	HasTimeExpr bool   `json:"has_time_expr"`
	Start       string `json:"start"`
	End         string `json:"end"`
	Reasoning   string `json:"reasoning"`
}

var extractTimeRangeTool = service.Tool{
	Type: "function",
	Function: service.ToolFunction{
		Name:        "extract_time_range",
		Description: "从用户输入的主题中提取时间表达式并转换为精确的时间范围",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"has_time_expr": map[string]interface{}{
					"type":        "boolean",
					"description": "主题中是否包含时间表达式",
				},
				"start": map[string]interface{}{
					"type":        "string",
					"description": "时间范围起始，RFC3339 格式。无时间表达式时为空字符串",
				},
				"end": map[string]interface{}{
					"type":        "string",
					"description": "时间范围结束，RFC3339 格式。无时间表达式时为空字符串",
				},
				"reasoning": map[string]interface{}{
					"type":        "string",
					"description": "一句话解释判断依据",
				},
			},
			"required": []string{"has_time_expr", "start", "end", "reasoning"},
		},
	},
}

// LLMToolCallFn is the callback type for function-call based LLM invocations.
// Returns raw JSON arguments string from tool_calls[0].function.arguments.
type LLMToolCallFn func(ctx context.Context, messages []service.ChatMessage, tools []service.Tool, forceFn string) (string, error)

// PreRetrievalNarrow uses LLM Function Call to extract time expressions from topic
// and narrow the query window. Falls back to original range on any failure.
func PreRetrievalNarrow(ctx context.Context, topic string, originalStart, originalEnd time.Time, toolCallFn LLMToolCallFn) (time.Time, time.Time) {
	if topic == "" || toolCallFn == nil {
		return originalStart, originalEnd
	}
	if utf8.RuneCountInString(topic) > 500 {
		runes := []rune(topic)
		topic = string(runes[:500])
	}
	topic = sanitizeTopic(topic)

	now := time.Now()
	currentDate := now.Format("2006-01-02")
	weekdays := [...]string{"日", "一", "二", "三", "四", "五", "六"}
	weekday := weekdays[now.Weekday()]

	systemPrompt := fmt.Sprintf(`你是一个时间表达式解析器。

当前日期：%s（星期%s）
当前时间：%s

规则：
- "今天" = 当天 00:00:00 ~ 23:59:59
- "昨天" = 前一天 00:00:00 ~ 23:59:59
- "本周" = 本周一 00:00:00 ~ 当前时间
- "上周" = 上周一 00:00:00 ~ 上周日 23:59:59
- "最近N天" = N天前 00:00:00 ~ 当前时间
- "这几天" = 最近3天
- 时区统一使用 +08:00
- 如果没有任何时间表达式，has_time_expr 设为 false

你必须调用 extract_time_range 工具来返回结果，不要以文本形式回复。`, currentDate, weekday, now.Format("15:04"))

	userMsg := fmt.Sprintf(`判断以下主题中是否包含时间表达式，如果有，解析出精确的时间范围。

主题："%s"`, topic)

	messages := []service.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userMsg},
	}

	narrowStart := time.Now()
	argsJSON, err := toolCallFn(ctx, messages, []service.Tool{extractTimeRangeTool}, "extract_time_range")
	elapsed := time.Since(narrowStart).Milliseconds()

	if err != nil {
		log.Printf("[pipeline] CallWithTools: tool=extract_time_range input={topic:%q} took %dms error=%v, fallback to original range", topic, elapsed, err)
		return originalStart, originalEnd
	}

	var parsed TimeNarrowResult
	if err := json.Unmarshal([]byte(argsJSON), &parsed); err != nil {
		log.Printf("[pipeline] CallWithTools: tool=extract_time_range input={topic:%q} took %dms parse_error=%v, args=%s", topic, elapsed, err, argsJSON)
		return originalStart, originalEnd
	}

	log.Printf("[pipeline] CallWithTools: tool=extract_time_range input={topic:%q} took %dms result={has_time_expr:%v}", topic, elapsed, parsed.HasTimeExpr)

	if !parsed.HasTimeExpr {
		return originalStart, originalEnd
	}

	parsedStart, err1 := time.Parse(time.RFC3339, parsed.Start)
	parsedEnd, err2 := time.Parse(time.RFC3339, parsed.End)
	if err1 != nil || err2 != nil {
		log.Printf("[pipeline] PreRetrievalNarrow: time parse error start=%v end=%v", err1, err2)
		return originalStart, originalEnd
	}

	if parsedStart.Before(originalStart) {
		parsedStart = originalStart
	}
	if parsedEnd.After(originalEnd) {
		parsedEnd = originalEnd
	}
	if !parsedStart.Before(parsedEnd) {
		log.Printf("[pipeline] PreRetrievalNarrow: invalid range start >= end")
		return originalStart, originalEnd
	}

	log.Printf("[pipeline] PreRetrievalNarrow: narrowed [%s ~ %s] → [%s ~ %s] reason=%s",
		originalStart.Format("01-02"), originalEnd.Format("01-02"),
		parsedStart.Format("01-02 15:04"), parsedEnd.Format("01-02 15:04"),
		parsed.Reasoning)

	return parsedStart, parsedEnd
}

// PostRetrievalNarrow remains unchanged — uses LLMCallFn (not Function Call).
func PostRetrievalNarrow(ctx context.Context, messages []Message, topic string, llmFn LLMCallFn) []Message {
	return messages
}
```

**关键变化**：
- 回调类型从 `LLMCallFn` 改为 `LLMToolCallFn`
- 消息类型使用导出的 `service.ChatMessage`
- 删除 `trimMarkdownCodeFence` 调用
- Prompt 中删除 "只返回 JSON，不要其他内容" 指令
- **新增**：Prompt 末尾追加 "你必须调用 extract_time_range 工具来返回结果，不要以文本形式回复。"（双保险：`tool_choice` 协议级强制 + prompt 指令级强制）
- **新增**：统一日志格式 `[pipeline] CallWithTools: tool=<name> input={...} took <ms> result={...}` / `error=<err>`
- `PostRetrievalNarrow` 保留原始签名（仍用 `LLMCallFn`）

---

### 4.2 `ResolveTopicTarget`（新增）→ Function Call

#### Tool Schema

```go
var resolveTopicTargetTool = service.Tool{
	Type: "function",
	Function: service.ToolFunction{
		Name:        "resolve_topic_target",
		Description: "判断总结主题是否指向特定成员，如果是则返回对应成员的 UID",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"has_target": map[string]interface{}{
					"type":        "boolean",
					"description": "主题是否指向特定成员",
				},
				"uids": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "目标成员的 UID 列表。has_target 为 false 时为空数组",
				},
				"reasoning": map[string]interface{}{
					"type":        "string",
					"description": "一句话解释判断依据",
				},
			},
			"required": []string{"has_target", "uids", "reasoning"},
		},
	},
}
```

#### 完整实现

**文件**：`internal/pipeline/resolve_topic.go`（新文件）

```go
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

// TopicResolveResult is the structured output from topic target resolution.
type TopicResolveResult struct {
	HasTarget bool     `json:"has_target"`
	UIDs      []string `json:"uids"`
	Reasoning string   `json:"reasoning"`
}

var resolveTopicTargetTool = service.Tool{
	Type: "function",
	Function: service.ToolFunction{
		Name:        "resolve_topic_target",
		Description: "判断总结主题是否指向特定成员，如果是则返回对应成员的 UID",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"has_target": map[string]interface{}{
					"type":        "boolean",
					"description": "主题是否指向特定成员",
				},
				"uids": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "目标成员的 UID 列表。has_target 为 false 时为空数组",
				},
				"reasoning": map[string]interface{}{
					"type":        "string",
					"description": "一句话解释判断依据",
				},
			},
			"required": []string{"has_target", "uids", "reasoning"},
		},
	},
}

// ResolveTopicTarget uses LLM Function Call to semantically resolve the target person
// referenced in the topic. Returns targetUIDs for FilterWithContext.
// Falls back to []string{defaultUID} when topic has no target or on any failure.
func ResolveTopicTarget(ctx context.Context, topic string, nameMap map[string]string, defaultUID string, toolCallFn LLMToolCallFn) []string {
	if topic == "" || toolCallFn == nil {
		return []string{defaultUID}
	}

	// Build sorted member list for deterministic prompts (Fix M3)
	type member struct {
		UID  string
		Name string
	}
	var members []member
	for uid, name := range nameMap {
		if name != "" {
			members = append(members, member{UID: uid, Name: name})
		}
	}
	if len(members) == 0 {
		return []string{defaultUID}
	}

	sort.Slice(members, func(i, j int) bool {
		return members[i].UID < members[j].UID
	})

	var memberLines []string
	for _, m := range members {
		memberLines = append(memberLines, fmt.Sprintf("- UID: %s, 姓名: %s", m.UID, m.Name))
	}
	memberList := strings.Join(memberLines, "\n")
	topicSafe := sanitizeTopic(topic)

	systemPrompt := `你是一个人物指代解析器。根据总结主题和成员列表，判断主题是否指向特定成员。

规则：
- 主题是关于某个特定成员的内容（如"辉哥的发言"、"CTO的观点"），返回该成员 UID
- 主题包含多个人（如"辉哥和小明的讨论"），返回所有相关成员的 UID
- 主题是自我指代（如"我的工作"），has_target 为 false
- 主题不涉及特定人物（如"项目进度"），has_target 为 false
- 主题中提到的人物不在成员列表中，has_target 为 false
- 只返回确定的匹配，不要猜测
- 支持昵称、简称、职位等间接引用

你必须调用 resolve_topic_target 工具来返回结果，不要以文本形式回复。`

	userMsg := fmt.Sprintf(`总结主题："%s"
创建者 UID：%s

成员列表：
%s

请判断主题是否指向特定成员。`, topicSafe, defaultUID, memberList)

	messages := []service.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userMsg},
	}

	resolveCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	start := time.Now()
	argsJSON, err := toolCallFn(resolveCtx, messages, []service.Tool{resolveTopicTargetTool}, "resolve_topic_target")
	elapsed := time.Since(start).Milliseconds()

	if err != nil {
		log.Printf("[pipeline] CallWithTools: tool=resolve_topic_target input={topic:%q, members:%d} took %dms error=%v, fallback to creator", topicSafe, len(members), elapsed, err)
		return []string{defaultUID}
	}

	var parsed TopicResolveResult
	if err := json.Unmarshal([]byte(argsJSON), &parsed); err != nil {
		log.Printf("[pipeline] CallWithTools: tool=resolve_topic_target input={topic:%q, members:%d} took %dms parse_error=%v, args=%s, fallback to creator", topicSafe, len(members), elapsed, err, argsJSON)
		return []string{defaultUID}
	}

	log.Printf("[pipeline] CallWithTools: tool=resolve_topic_target input={topic:%q, members:%d} took %dms result={has_target:%v, uids:%v}", topicSafe, len(members), elapsed, parsed.HasTarget, parsed.UIDs)

	if !parsed.HasTarget || len(parsed.UIDs) == 0 {
		log.Printf("[pipeline] ResolveTopicTarget: no target in topic, reason=%s", parsed.Reasoning)
		return []string{defaultUID}
	}

	var validUIDs []string
	for _, uid := range parsed.UIDs {
		if _, ok := nameMap[uid]; ok {
			validUIDs = append(validUIDs, uid)
		} else {
			log.Printf("[pipeline] ResolveTopicTarget: LLM returned unknown UID %q, skipping", uid)
		}
	}

	if len(validUIDs) == 0 {
		log.Printf("[pipeline] ResolveTopicTarget: all UIDs invalid, fallback to creator")
		return []string{defaultUID}
	}

	log.Printf("[pipeline] ResolveTopicTarget: resolved %d target(s) %v, reason=%s",
		len(validUIDs), validUIDs, parsed.Reasoning)
	return validUIDs
}
```

---

## 5. `FilterWithContext` 签名修改

**文件**：`internal/pipeline/filter.go`

**修改前**：
```go
func FilterWithContext(messages []Message, userID string, contextWindow int) []Message
```

**修改后**：
```go
func FilterWithContext(messages []Message, targetUIDs []string, contextWindow int) []Message
```

**完整实现**：

```go
package pipeline

// FilterWithContext keeps target users' messages plus N context messages before/after each.
func FilterWithContext(messages []Message, targetUIDs []string, contextWindow int) []Message {
	if contextWindow < 0 {
		contextWindow = 0
	}
	if len(targetUIDs) == 0 {
		return nil
	}

	targetSet := make(map[string]bool, len(targetUIDs))
	for _, uid := range targetUIDs {
		targetSet[uid] = true
	}

	var targetIndices []int
	for i, m := range messages {
		if targetSet[m.SenderUID] {
			targetIndices = append(targetIndices, i)
		}
	}

	if len(targetIndices) == 0 {
		return nil
	}

	keep := make(map[int]bool)
	for _, idx := range targetIndices {
		for j := idx - contextWindow; j <= idx+contextWindow; j++ {
			if j >= 0 && j < len(messages) {
				keep[j] = true
			}
		}
	}

	var result []Message
	for i, m := range messages {
		if keep[i] {
			m.IsTargetUser = targetSet[m.SenderUID]
			result = append(result, m)
		}
	}
	return result
}
```

---

## 6. `executePersonalPipeline` 集成

**文件**：`internal/worker/personal_processor.go`

### 核心修改区域

```go
func (p *Processor) executePersonalPipeline(ctx context.Context, task model.SummaryTask, userID string) (string, []model.Citation, int, int, string, error) {
	totalStart := time.Now()

	// Load sources
	var sources []model.SummarySource
	if err := p.db.Where("task_id = ?", task.ID).Find(&sources).Error; err != nil {
		return "", nil, 0, 0, "", fmt.Errorf("load sources: %w", err)
	}

	specifiedSources := make([]map[string]interface{}, 0, len(sources))
	for _, s := range sources {
		specifiedSources = append(specifiedSources, map[string]interface{}{
			"source_id":   s.SourceID,
			"source_type": s.SourceType,
			"source_name": s.SourceName,
		})
	}

	// Unified LLM tool-call callback (shared by all Function Call sites)
	toolCallFn := func(ctx context.Context, messages []service.ChatMessage, tools []service.Tool, forceFn string) (string, error) {
		args, _, err := p.llm.CallWithTools(ctx, messages, tools, forceFn, p.cfg.LLMTemperature)
		return args, err
	}

	// Legacy callback for PostRetrievalNarrow (still uses CallRaw)
	llmFn := func(ctx context.Context, prompt string) (string, error) {
		return p.llm.CallRaw(ctx, prompt)
	}

	// Fetch messages via personal pipeline (Layer 0-5)
	messages, err := pipeline.ResolveAndFetchMessagesForPersonal(
		ctx, userID, nil, nil, specifiedSources, task.Title,
		task.TimeRangeStart, task.TimeRangeEnd,
		p.imDB, toolCallFn, llmFn,
		p.cfg.MsgTableCount, p.cfg.MaxMessagesPerChannel, p.cfg.FetchConcurrency,
	)
	if err != nil {
		return "", nil, 0, 0, "", fmt.Errorf("fetch messages: %w", err)
	}

	// ===== Resolve user names (moved before FilterWithContext for ResolveTopicTarget) =====
	resolveStart := time.Now()
	nameMap := p.batchResolveUserNames(messages)
	log.Printf("[personal-worker] batchResolveUserNames took %dms (%d names)",
		time.Since(resolveStart).Milliseconds(), len(nameMap))

	// ===== NEW: Resolve topic target via LLM Function Call =====
	targetUIDs := pipeline.ResolveTopicTarget(ctx, task.Title, nameMap, userID, toolCallFn)
	log.Printf("[personal-worker] topic target resolved: %v (creator=%s)", targetUIDs, userID)

	// ===== Apply context window filter (signature changed: userID → targetUIDs) =====
	filterStart := time.Now()
	userMessages := pipeline.FilterWithContext(messages, targetUIDs, p.cfg.ContextWindow)
	log.Printf("[personal-worker] FilterWithContext took %dms (%d → %d messages, targets=%v)",
		time.Since(filterStart).Milliseconds(), len(messages), len(userMessages), targetUIDs)
	if len(userMessages) == 0 {
		return noRelevantContentMessage, nil, 0, 0, p.llm.ModelVersion(), nil
	}

	// ===== Determine userName: use target's name when topic points to someone else =====
	var userName string
	if len(targetUIDs) == 1 && targetUIDs[0] != userID {
		userName = nameMap[targetUIDs[0]]
	}
	if userName == "" {
		userName = nameMap[userID]
	}
	if userName == "" {
		userName = userID
	}

	// ... (remainder unchanged: name assignment, CitationIndex, chunking, Map/Reduce) ...
}
```

### `ResolveAndFetchMessagesForPersonal` 签名变化

**文件**：`internal/pipeline/fetch.go`

```go
func ResolveAndFetchMessagesForPersonal(
	ctx context.Context,
	creatorUID string,
	participantUIDs []string,
	participantNames []string,
	specifiedSources []map[string]interface{},
	topic string,
	timeStart, timeEnd time.Time,
	imDB *gorm.DB,
	toolCallFn LLMToolCallFn,
	llmFn LLMCallFn,
	tableCount int,
	maxPerChannel int,
	fetchConcurrency int,
) ([]Message, error)
```

内部改动：
- `PreRetrievalNarrow` 调用传递 `toolCallFn`
- `PostRetrievalNarrow` 调用传递 `llmFn`（保持不变）
- `NarrowByTopic` **不修改、不调用**（保持当前跳过状态）

---

## 7. 配置项变更

### 新增：`LLM_TEMPERATURE` 环境变量

**文件**：`internal/config/config.go`

```go
type Config struct {
	// ... existing fields ...

	// LLM
	LLMApiURL      string
	LLMApiKey      string
	LLMModel       string
	LLMTimeout     int
	LLMMaxToken    int
	LLMTemperature float64  // NEW: default 0.3
}
```

`Load()` 函数追加：

```go
func Load() *Config {
	cfg := &Config{
		// ... existing ...
		LLMTemperature: getEnvFloat("LLM_TEMPERATURE", 0.3),
	}
	return cfg
}

func getEnvFloat(key string, defaultVal float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		log.Printf("[config] invalid %s=%q, using default %.2f", key, v, defaultVal)
		return defaultVal
	}
	return f
}
```

**使用方式**：`toolCallFn` 闭包中通过 `p.cfg.LLMTemperature` 传入 `CallWithTools`。

---

## 8. 向后兼容性

### 8.1 兼容性矩阵

| 场景 | topic | ResolveTopicTarget 返回 | FilterWithContext 行为 | 与修改前对比 |
|------|-------|--------------------------|----------------------|-------------|
| 默认（无 topic） | "" | `[creatorUID]` | 按 creatorUID 过滤 | **完全一致** |
| 自我总结 | "我的工作" | `[creatorUID]` | 按 creatorUID 过滤 | **完全一致** |
| 不含人名 | "项目进度" | `[creatorUID]` | 按 creatorUID 过滤 | **完全一致** |
| LLM 调用失败 | 任何 | `[creatorUID]` | 按 creatorUID 过滤 | **完全一致** |
| **他人总结** | "辉哥的主要发言" | `[辉哥UID]` | 按辉哥 UID 过滤 | **修复生效** |
| **多人总结** | "辉哥和小明" | `[辉哥, 小明]` | 按两人 UID 过滤 | **修复生效** |

### 8.2 无破坏性变更

- 无外部 API 变更
- 无数据库 schema 变更
- 新增环境变量 `LLM_TEMPERATURE`（可选，有默认值 0.3）
- 当 `targetUIDs = []string{userID}` 时，`FilterWithContext` 行为与旧版逻辑等价
- `CallRaw` 保留，`PostRetrievalNarrow` 调用路径不受影响

---

## 9. 边界情况处理

| # | 场景 | 行为 |
|---|------|------|
| 1 | LLM 网络超时/5xx | `CallWithTools` 指数退避重试 3 次后失败 → `ResolveTopicTarget` fallback 到 `[creatorUID]` |
| 2 | LLM 返回 4xx（非 429） | 立即返回错误，不重试 → fallback 到 `[creatorUID]` |
| 3 | LLM 返回 429 | 指数退避重试最多 3 次 |
| 4 | LLM 返回空 `tool_calls` | `CallWithTools` 视为失败并重试 |
| 5 | LLM 返回的 UID 不在 nameMap 中 | 过滤无效 UID；全部无效时 fallback 到 `[creatorUID]` |
| 6 | topic 是自我指代 | LLM 返回 `has_target=false` → 使用 creatorUID |
| 7 | nameMap 为空（无可解析成员） | 不发起 LLM 调用，直接返回 `[creatorUID]` |
| 8 | 目标人物在时间范围内无发言 | `FilterWithContext` 返回 nil → 返回 `noRelevantContentMessage` |
| 9 | 15s 独立超时 | `ResolveTopicTarget` 有独立 context timeout，不影响上游 |
| 10 | LLM 网关不支持 `tools` 参数 | 返回 HTTP 400 → 不重试，fallback 到 `[creatorUID]` |

---

## 10. 性能影响

| 操作 | 变化 | 影响 |
|------|------|------|
| `batchResolveUserNames` | 提前调用（执行顺序变化） | 零额外开销 |
| PreRetrievalNarrow | 改用 Function Call | 延迟不变（~1-3s），输出更稳定 |
| **ResolveTopicTarget** | **新增调用** | ~1-3s，~1050 tokens |
| LLM 总调用次数 | N → N+1（新增 1 次） | 增加一次轻量调用 |

### Pipeline 整体耗时对比

| 阶段 | 原有 | 新增 | 总计 |
|------|------|------|------|
| PreRetrievalNarrow | 1-3s | — | 1-3s |
| 消息获取 | 2-5s | — | 2-5s |
| batchResolveUserNames | <100ms | — | <100ms |
| **ResolveTopicTarget** | — | **1-3s** | **1-3s** |
| FilterWithContext | <1ms | — | <1ms |
| Map/Reduce | 10-40s | — | 10-40s |
| **总计** | 15-50s | **+1-3s** | **16-53s (+5-10%)** |

---

## 11. 数据流图

```
executePersonalPipeline(task, userID=creatorUID)
    │
    │  toolCallFn = p.llm.CallWithTools(ctx, ..., p.cfg.LLMTemperature)
    │  llmFn = p.llm.CallRaw(ctx, ...)
    │
    ├── ResolveAndFetchMessagesForPersonal(ctx, ..., toolCallFn, llmFn, ...)
    │       │
    │       ├── PreRetrievalNarrow(ctx, topic, start, end, toolCallFn)
    │       │       └── CallWithTools → extract_time_range → TimeNarrowResult
    │       │
    │       ├── GetUserChannels → Layer 1
    │       ├── IntersectParticipantChannels → Layer 1.5
    │       ├── ApplySourceConstraints → Layer 2
    │       ├── [Layer 3: NarrowByTopic — 当前跳过，不修改]
    │       ├── FetchMessagesFromChannel → Layer 4 (并发)
    │       ├── FilterByMutualActivity → Layer 4.5
    │       └── PostRetrievalNarrow(ctx, messages, topic, llmFn) → Layer 5
    │
    ├── batchResolveUserNames(messages) → nameMap
    │
    ├── ResolveTopicTarget(ctx, task.Title, nameMap, userID, toolCallFn)
    │       └── CallWithTools → resolve_topic_target → TopicResolveResult
    │       → targetUIDs []string
    │
    ├── FilterWithContext(messages, targetUIDs, contextWindow)
    │       → userMessages
    │
    ├── Token-aware chunking
    ├── Map phase (并发 CallMap)
    ├── Reduce phase (CallReduce)
    └── Citation extraction → finalContent + citations
```

---

## 12. 测试计划

### 12.1 单元测试：`CallWithTools`

**文件**：`internal/service/llm_test.go`

| # | 场景 | 验证点 |
|---|------|--------|
| 1 | 正常调用 | 返回 `tool_calls[0].function.arguments`，token 数正确 |
| 2 | HTTP 500 → 重试成功 | 第 1-2 次 500，第 3 次 200 → 成功返回 |
| 3 | HTTP 429 → 重试成功 | 限流后恢复 → 成功返回 |
| 4 | HTTP 400 → 立即失败 | 不重试，直接返回错误 |
| 5 | 所有 3 次均失败（5xx） | 返回 error，error message 含 "after 3 attempts" |
| 6 | Response 无 tool_calls | 视为失败并重试 |
| 7 | Response arguments 为空 | 视为失败并重试 |
| 8 | Context cancelled | 立即返回 context error |

使用 `httptest.NewServer` mock HTTP 端点。

### 12.2 单元测试：`ResolveTopicTarget`

**文件**：`internal/pipeline/resolve_topic_test.go`

| # | topic | nameMap | mock 返回 | 期望 |
|---|-------|---------|----------|------|
| 1 | "" | any | （不调用） | `[defaultUID]` |
| 2 | "辉哥的发言" | `{"u2":"李辉"}` | `{"has_target":true,"uids":["u2"],"reasoning":"辉哥指李辉"}` | `["u2"]` |
| 3 | "辉哥和小明" | `{"u2":"李辉","u3":"小明"}` | `{"has_target":true,"uids":["u2","u3"],...}` | `["u2","u3"]` |
| 4 | "我的工作" | `{"u2":"李辉"}` | `{"has_target":false,"uids":[],...}` | `[defaultUID]` |
| 5 | "项目进度" | `{"u2":"李辉"}` | `{"has_target":false,"uids":[],...}` | `[defaultUID]` |
| 6 | "辉哥" | `{}` | （不调用，列表为空） | `[defaultUID]` |
| 7 | "辉哥" | `{"u2":"李辉"}` | 返回 error | `[defaultUID]` |
| 8 | "辉哥" | `{"u2":"李辉"}` | `{"has_target":true,"uids":["u999"],...}` | `[defaultUID]` (UID 无效) |
| 9 | nil toolCallFn | any | — | `[defaultUID]` |

### 12.3 单元测试：`FilterWithContext`（新签名）

**文件**：`internal/pipeline/filter_test.go`

| # | senders | targetUIDs | window | 期望保留 |
|---|---------|-----------|--------|---------|
| 1 | `[u1,u2,u1,u2,u3]` | `["u1"]` | 1 | `[0,1,2,3]` |
| 2 | `[u1,u2,u1,u2,u3]` | `["u2"]` | 0 | `[1,3]` |
| 3 | `[u1,u2,u3]` | `["u2","u3"]` | 0 | `[1,2]` |
| 4 | `[u1,u2,u3]` | `[]` | 2 | nil |
| 5 | `[u1,u1,u1]` | `["u2"]` | 1 | nil |

### 12.4 单元测试：`PreRetrievalNarrow`（改造后）

| # | 场景 | 验证点 |
|---|------|--------|
| 1 | 正常时间解析 | 解析 Function Call 返回的 JSON args → 正确时间范围 |
| 2 | toolCallFn 返回 error | fallback 到原始范围 |
| 3 | `has_time_expr=false` | 返回原始范围 |
| 4 | topic 为空 | 不调用，返回原始范围 |

### 12.5 集成测试场景

**场景 A：原有行为不变**
```
topic="我最近的工作总结", userID="creator1"
LLM Function Call 返回: {"has_target":false, "uids":[], "reasoning":"自我指代"}
→ FilterWithContext 按 creator1 过滤
→ 结果非空 ✓
```

**场景 B：Issue #34 核心修复**
```
topic="辉哥的主要发言内容", userID="creator1"
nameMap={"hui_uid":"李辉", "creator1":"张三"}
LLM Function Call 返回: {"has_target":true, "uids":["hui_uid"], "reasoning":"辉哥指李辉"}
→ FilterWithContext 按 hui_uid 过滤
→ 374 条消息中李辉的发言被正确保留 ✓
→ Map/Reduce userName="李辉"
```

**场景 C：LLM 降级**
```
topic="辉哥的发言", userID="creator1"
CallWithTools 重试失败 / 返回 4xx
→ ResolveTopicTarget fallback 到 [creator1]
→ 行为与修改前一致 ✓
```

---

## 12. 变更清单

| 文件 | 变更类型 | 说明 |
|------|---------|------|
| `internal/service/llm.go` | **修改** | 导出 `ChatMessage`；新增 `Tool`/`ToolChoice` 类型、`CallWithTools` 方法；**保留 `CallRaw`** |
| `internal/service/llm_test.go` | **修改** | 新增 `CallWithTools` 单元测试 |
| `internal/config/config.go` | **修改** | 新增 `LLMTemperature` 字段 + `getEnvFloat` 辅助函数 |
| `internal/pipeline/narrow.go` | **修改** | 新增 `LLMToolCallFn` 类型；`PreRetrievalNarrow` 改用 Function Call；删除 `trimMarkdownCodeFence` |
| `internal/pipeline/narrow_test.go` | **修改** | 更新测试为 Function Call 回调 |
| `internal/pipeline/resolve_topic.go` | **新增** | `ResolveTopicTarget` + tool schema 定义 |
| `internal/pipeline/resolve_topic_test.go` | **新增** | `ResolveTopicTarget` 完整单元测试 |
| `internal/pipeline/fetch.go` | **修改** | `ResolveAndFetchMessagesForPersonal` 增加 `toolCallFn LLMToolCallFn` 参数 |
| `internal/pipeline/fetch_test.go` | **修改** | 更新函数签名 |
| `internal/pipeline/filter.go` | **修改** | `FilterWithContext` 签名改为 `targetUIDs []string` |
| `internal/pipeline/filter_test.go` | **修改** | 更新已有测试 + 新增多 UID 场景 |
| `internal/worker/personal_processor.go` | **修改** | 统一 toolCallFn 回调；调整调用顺序；接入 ResolveTopicTarget |
| `internal/worker/personal_processor_test.go` | **修改** | 集成测试覆盖 Function Call 链路 |

### 不删除的代码

| 代码 | 文件 | 原因 |
|------|------|------|
| `CallRaw` 方法 | `llm.go` | `PostRetrievalNarrow` 通过 `LLMCallFn` 仍在使用 |
| `NarrowByTopic` 函数 | `fetch.go` | 已存在但未被调用，本 PR 不修改 |
| `sanitizeTopic` 函数 | `narrow.go` | 仍需对输入做安全清理 |

### 删除的代码

| 代码 | 文件 | 原因 |
|------|------|------|
| `trimMarkdownCodeFence` 函数 | `narrow.go` | Function Call 协议保证输出是纯 JSON |
| Prompt 中 "只返回 JSON" 系列指令 | `narrow.go` | 输出格式由 tool schema 约束 |

### 依赖

- 无新增外部依赖
- LLM 网关需支持 OpenAI `tools` + `tool_choice` 参数（已确认支持）

---

## 13. 实现顺序

| Phase | 内容 | 前置依赖 |
|-------|------|---------|
| 1 | `config.go`：新增 `LLMTemperature` + `getEnvFloat` | 无 |
| 2 | `llm.go`：导出 `ChatMessage`；新增 `Tool`/`ToolChoice` 类型 + `CallWithTools` 方法 + 单元测试 | Phase 1 |
| 3 | `narrow.go`：新增 `LLMToolCallFn` 类型；改造 `PreRetrievalNarrow` + 单元测试 | Phase 2 |
| 4 | `resolve_topic.go`：实现 `ResolveTopicTarget` + 单元测试 | Phase 2, 3 |
| 5 | `filter.go`：`FilterWithContext` 签名改为 `targetUIDs []string` + 更新测试 | 无 |
| 6 | `fetch.go`：`ResolveAndFetchMessagesForPersonal` 签名变更（增加 `toolCallFn` 参数） | Phase 3 |
| 7 | `personal_processor.go`：集成 toolCallFn / ResolveTopicTarget / 新 FilterWithContext | Phase 4, 5, 6 |
| 8 | 端到端集成测试 | Phase 7 |
| 9 | 清理 `trimMarkdownCodeFence` | Phase 7 通过 |

### Future Work（本 PR 之后）

- `NarrowByTopic`（`fetch.go`）：当前未被调用。如后续启用 Layer 3 频道过滤，可改造为 Function Call 调用点，使用 `select_relevant_channels` tool schema。
