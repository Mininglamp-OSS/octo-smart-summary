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

// LLMClient handles calls to the OpenAI-compatible LLM gateway.
type LLMClient struct {
	apiURL    string
	apiKey    string
	model     string
	timeout   time.Duration
	maxTokens int
	client    *http.Client
}

// NewLLMClient creates a new LLM client.
func NewLLMClient(apiURL, apiKey, model string, timeoutSec, maxTokens int) *LLMClient {
	return &LLMClient{
		apiURL:    strings.TrimRight(apiURL, "/"),
		apiKey:    apiKey,
		model:     model,
		timeout:   time.Duration(timeoutSec) * time.Second,
		maxTokens: maxTokens,
		client:    &http.Client{Timeout: time.Duration(timeoutSec) * time.Second},
	}
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
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
func (c *LLMClient) Call(ctx context.Context, messages []chatMessage, temperature float64) (string, int, error) {
	reqBody := chatRequest{
		Model:       c.model,
		Messages:    messages,
		Temperature: temperature,
		MaxTokens:   c.maxTokens,
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
	content, _, err := c.Call(ctx, []chatMessage{{Role: "user", Content: prompt}}, 0.3)
	if err != nil {
		log.Printf("[llm] CallRaw failed: %v", err)
		return "[]", nil
	}
	return content, nil
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

	sb.WriteString(`
## 任务
`)
	sb.WriteString(fmt.Sprintf("从以下聊天记录中，围绕「%s」进行总结。", topic))
	sb.WriteString(fmt.Sprintf("当主题中出现\"我\"、\"自己\"等人称代词时，指的是「%s」。\n", userName))

	sb.WriteString(`
## 输出要求
- 紧密围绕主题，与主题无关的闲聊、表情、寒暄等直接跳过
- 提炼关键信息：讨论了什么、达成了什么结论、有什么待办、谁负责什么
- 如果聊天记录中没有明确结论，如实说明"尚未达成共识"，不要编造
- 有待办事项时，用 ` + "`- [ ] 内容（负责人）`" + ` 格式列出
- 根据实际内容自行组织结构，不需要套用固定模板
- 保持简洁，不要复述原文，用自己的话归纳

## 引用规则
- 每个要点末尾标注来源消息编号，格式为 [n] 或 [n1][n2]
- 只引用带编号的消息（即 [数字] 开头的行），不要引用无编号的上下文消息
- 不要捏造不存在的编号
- 多条消息支持同一要点时，列出所有相关编号
- 如果多条消息内容完全相同（如用户重复发送），只引用其中一条

## 格式规范
- 用显示名称指代人（如"张三"），绝对不要输出 UID 或用户 ID
- 输出语言与聊天记录的语言保持一致
`)
	return sb.String()
}

func buildReduceSystemPrompt(topic string) string {
	var sb strings.Builder
	sb.WriteString(`你是一个专业的工作内容整理助手。请将以下多个分片总结合并为一份完整的总结报告。

要求：
- 合并相同主题，去除重复
- 保留所有待办事项和责任人
- 如有冲突信息，保留最新的
- 保留所有 [n] 引用标记，不要删除或修改
- 合并相同要点时，合并其引用编号
- 根据实际内容自行组织结构，不需要套用固定模板
- 用显示名称指代人，绝对不要输出 UID 或用户 ID
- 输出语言与输入语言保持一致
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
		content, tokens, err := c.Call(ctx, []chatMessage{
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
	return fmt.Sprintf("(分片 %d 总结失败)", chunkIndex), 0, nil
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

	return c.Call(ctx, []chatMessage{
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

	return c.Call(ctx, []chatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: fmt.Sprintf("时间范围：%s ~ %s\n\n%s", startTime, endTime, text)},
	}, 0.1)
}
