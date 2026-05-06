package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const MapFailedMarker = "总结失败"

// LLMClient handles calls to the OpenAI-compatible LLM gateway.
type LLMClient struct {
	apiURL         string
	apiKey         string
	model          string
	timeout        time.Duration
	maxTokens      int
	enableThinking bool
	client         *http.Client
}

// NewLLMClient creates a new LLM client.
func NewLLMClient(apiURL, apiKey, model string, timeoutSec, maxTokens int, enableThinking bool) *LLMClient {
	return &LLMClient{
		apiURL:         strings.TrimRight(apiURL, "/"),
		apiKey:         apiKey,
		model:          model,
		timeout:        time.Duration(timeoutSec) * time.Second,
		maxTokens:      maxTokens,
		enableThinking: enableThinking,
		client:         &http.Client{Timeout: time.Duration(timeoutSec) * time.Second},
	}
}

// ChatMessage represents a single message in a chat completion request.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

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
	Model              string                 `json:"model"`
	Messages           []ChatMessage          `json:"messages"`
	Temperature        float64                `json:"temperature"`
	MaxTokens          int                    `json:"max_tokens"`
	Tools              []Tool                 `json:"tools"`
	ToolChoice         ToolChoice             `json:"tool_choice"`
	ChatTemplateKwargs map[string]interface{} `json:"chat_template_kwargs,omitempty"`
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

type chatRequest struct {
	Model              string                 `json:"model"`
	Messages           []ChatMessage          `json:"messages"`
	Temperature        float64                `json:"temperature"`
	MaxTokens          int                    `json:"max_tokens"`
	ChatTemplateKwargs map[string]interface{} `json:"chat_template_kwargs,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}

// Call makes a chat completion request. Returns (content, tokenUsed, error).
func (c *LLMClient) Call(ctx context.Context, messages []ChatMessage, temperature float64) (string, int, error) {
	log.Printf("[llm] calling model=%s temperature=%.2f max_tokens=%d", c.model, temperature, c.maxTokens)
	reqBody := chatRequest{
		Model:       c.model,
		Messages:    messages,
		Temperature: temperature,
		MaxTokens:   c.maxTokens,
	}
	if !c.enableThinking && (strings.Contains(strings.ToLower(c.model), "qwen3.6") || strings.Contains(strings.ToLower(c.model), "deepseek-v4")) {
		reqBody.ChatTemplateKwargs = map[string]interface{}{"enable_thinking": false}
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("LLM API error: status=%d body=%s", resp.StatusCode, string(respBody))
	}

	var chatResp chatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return "", 0, fmt.Errorf("unmarshal LLM response: %w", err)
	}
	if len(chatResp.Choices) == 0 {
		return "", 0, fmt.Errorf("LLM returned no choices")
	}
	return chatResp.Choices[0].Message.Content, chatResp.Usage.TotalTokens, nil
}

// CallRaw is a simple single-turn call returning text only. Used for topic narrowing.
func (c *LLMClient) CallRaw(ctx context.Context, prompt string) (string, error) {
	content, _, err := c.Call(ctx, []ChatMessage{{Role: "user", Content: prompt}}, 0.3)
	if err != nil {
		log.Printf("[llm] CallRaw failed: %v", err)
		return "[]", nil
	}
	return content, nil
}

// CallWithTools makes a chat completion request with forced function calling.
// Returns the raw JSON string from tool_calls[0].function.arguments and token count.
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
	if !c.enableThinking && (strings.Contains(strings.ToLower(c.model), "qwen3.6") || strings.Contains(strings.ToLower(c.model), "deepseek-v4")) {
		reqBody.ChatTemplateKwargs = map[string]interface{}{"enable_thinking": false}
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

// ModelVersion returns the configured model name.
func (c *LLMClient) ModelVersion() string {
	return c.model
}

func buildMapSystemPrompt(userName, topic string) string {
	var sb strings.Builder
	sb.WriteString(`你是一个专业的工作内容整理助手。

## 背景信息
`)
	sb.WriteString(fmt.Sprintf("- 当前用户：%s\n", userName))
	sb.WriteString(fmt.Sprintf("- 总结主题：%s\n", topic))

	now := time.Now()
	weekdays := [...]string{"日", "一", "二", "三", "四", "五", "六"}
	sb.WriteString(fmt.Sprintf("- 当前日期：%s（星期%s）\n",
		now.Format("2006-01-02"), weekdays[now.Weekday()]))

	sb.WriteString(`
## 任务
`)
	sb.WriteString(fmt.Sprintf("从以下聊天记录中，围绕「%s」进行总结。", topic))
	sb.WriteString(fmt.Sprintf("当主题中出现\"我\"、\"自己\"等人称代词时，指的是「%s」。\n", userName))

	sb.WriteString(`
## 输出要求
- 紧密围绕主题，与主题无关的闲聊、表情、寒暄等直接跳过
- 提炼关键信息：讨论了什么、达成了什么结论、有什么待办、谁负责什么
- 【强制】输出总长度不超过 2000 token（约 1500 字），超出时优先保留关键结论和待办事项，压缩次要细节
- 如果聊天记录中没有明确结论，如实说明"尚未达成共识"，不要编造
- 有待办事项时，用 ` + "`- [ ] 内容（负责人）`" + ` 格式列出
- 根据实际内容自行组织结构，不需要套用固定模板
- 保持简洁，不要复述原文，用自己的话归纳

## 引用规则（必须严格遵守）
- 【强制】每一条结论/要点都必须标注来源引用 [n]，没有引用的结论不允许输出
- 格式：[n] 或 [n1][n2]（多个来源时）
- 仅使用消息前方的 [n] 编号来标注引用，范围为 [1] 到 [N]
- 绝对不要引用或复制消息正文内出现的任何 [数字] 标记
- 超出有效范围的标记一律不得出现在输出中
- 所有消息均带有编号（即 [数字] 开头的行），选取有意义的、相关的消息作为依据
- 不要捏造不存在的编号
- 多条消息支持同一要点时，列出所有相关编号
- 如果多条消息内容完全相同（如用户重复发送），只引用其中一条
- 如果某条信息无法找到明确来源，则不要输出该条信息

## 格式规范
- 用显示名称指代人（如"张三"），绝对不要输出 UID 或用户 ID
- 输出语言与聊天记录的语言保持一致
`)
	return sb.String()
}

func buildReduceSystemPrompt(topic string) string {
	var sb strings.Builder
	sb.WriteString(`你是一个专业的工作内容整理助手。请将以下多个分片总结合并为一份完整的总结报告。

`)
	now := time.Now()
	weekdays := [...]string{"日", "一", "二", "三", "四", "五", "六"}
	sb.WriteString(fmt.Sprintf("当前日期：%s（星期%s）\n\n",
		now.Format("2006-01-02"), weekdays[now.Weekday()]))

	sb.WriteString(`要求：
- 合并相同主题，去除重复
- 保留所有待办事项和责任人
- 输出总长度不超过 2000 token（约 1500 字），超出时合并相似要点、压缩细节
- 如有冲突信息，保留最新的
- 保留所有 [n] 引用标记，不要删除或修改
- 合并相同要点时，合并其引用编号
- 根据实际内容自行组织结构，不需要套用固定模板
- 用显示名称指代人，绝对不要输出 UID 或用户 ID
- 输出语言与输入语言保持一致

引用规则：
- 仅使用分片总结中已有的 [n] 编号，不要引入新编号
- 绝对不要引用或复制正文内出现的任何 [数字] 标记
- 超出有效范围的标记一律不得出现在输出中
`)
	if topic != "" {
		sb.WriteString(fmt.Sprintf("\n重要：总结主题是「%s」，请只保留与该主题相关的条目，移除不相关内容。\n", topic))
	}
	return sb.String()
}

// CallMap runs the Map phase for a message chunk.
func (c *LLMClient) CallMap(ctx context.Context, formattedMessages string, sourceName string, chunkIndex int, msgCount int, timeStart, timeEnd string, topic string, userName string) (string, int, error) {
	if strings.TrimSpace(formattedMessages) == "" {
		return "(该时段无文本消息)", 0, nil
	}

	systemPrompt := buildMapSystemPrompt(userName, topic)

	userPrompt := fmt.Sprintf("来源：%s\n时间范围：%s ~ %s\n消息数：%d 条\n\n聊天记录：\n%s",
		sourceName, timeStart, timeEnd, msgCount, formattedMessages)

	for attempt := 0; attempt < 3; attempt++ {
		content, tokens, err := c.Call(ctx, []ChatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		}, 0.1)
		if err == nil {
			return content, tokens, nil
		}
		log.Printf("[llm] Map chunk %d attempt %d failed: %v", chunkIndex, attempt+1, err)
		if attempt < 2 {
			time.Sleep(time.Duration(1<<uint(attempt)) * time.Second)
		}
	}
	return fmt.Sprintf("(分片 %d %s)", chunkIndex, MapFailedMarker), 0, nil
}

// CallReduce runs the Reduce phase to merge chunk summaries.
func (c *LLMClient) CallReduce(ctx context.Context, chunkSummaries []string, sourceNames string, startTime, endTime string, totalMsgCount int, topic string) (string, int, error) {
	if len(chunkSummaries) == 1 {
		return chunkSummaries[0], 0, nil
	}

	var parts []string
	for i, s := range chunkSummaries {
		parts = append(parts, fmt.Sprintf("【分片 %d】\n%s", i+1, s))
	}
	summariesText := strings.Join(parts, "\n\n---\n\n")
	system := buildReduceSystemPrompt(topic)

	userPrompt := fmt.Sprintf("信息来源：%s\n时间范围：%s ~ %s\n消息总量：%d 条\n\n以下是各分片的总结，请合并：\n\n%s",
		sourceNames, startTime, endTime, totalMsgCount, summariesText)

	return c.Call(ctx, []ChatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: userPrompt},
	}, 0.1)
}

// CallReduceByPerson merges participant-level summaries.
// Each participant is assigned a [Pn] tag that the LLM should reference in the output.
func (c *LLMClient) CallReduceByPerson(ctx context.Context, participantSummaries []struct{ Name, Summary string }, startTime, endTime string) (string, int, error) {
	var parts []string
	for i, ps := range participantSummaries {
		parts = append(parts, fmt.Sprintf("[P%d]【%s 的工作总结】\n%s", i+1, ps.Name, ps.Summary))
	}
	text := strings.Join(parts, "\n\n---\n\n")

	system := `你是专业的工作汇报整理助手，请将多位成员的工作总结合并为团队整体总结报告。

要求：
- 合并相同主题，去除重复
- 保留所有待办事项和责任人
- 每个要点末尾必须标注来源成员编号，格式为 [Pn]，例如 [P1]、[P2]
- 多位成员贡献同一要点时，列出所有编号，如 [P1][P3]
- 只引用真实存在的编号，不要捏造
- 根据实际内容自行组织结构，不需要套用固定模板
- 用显示名称指代人，绝对不要输出 UID 或用户 ID`

	return c.Call(ctx, []ChatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: fmt.Sprintf("时间范围：%s ~ %s\n\n%s", startTime, endTime, text)},
	}, 0.1)
}
