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
