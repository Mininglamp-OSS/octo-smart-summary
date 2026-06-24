// Package pipeline provides intent recognition for the summary workflow.
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

// IntentResult is the unified output of intent recognition.
// It consolidates results from time parsing, channel scope, and target person resolution.
type IntentResult struct {
	// Skipped indicates whether LLM intent recognition was skipped (short-circuited).
	Skipped    bool
	SkipReason string // "pure_generic_topic", "simple_channel_constraint", or ""

	// TimeRange holds the resolved time constraints.
	TimeRange struct {
		Start       time.Time
		End         time.Time
		HasTimeExpr bool
		Narrowed    bool   // true if time was narrowed from original range
		Reasoning   string // LLM's explanation
	}

	// ChannelScope holds the resolved channel constraints.
	ChannelScope struct {
		HasConstraint bool
		ChannelIDs    []string // specified channel IDs
		ChannelType   []string // channel type constraints (dm/group/thread)
		Persons       []string // person UIDs for channel filtering
		PersonMode    string   // intersection/union
		IncludeSelf   bool
		Reasoning     string
	}

	// TargetPersons holds the resolved target person constraints.
	TargetPersons struct {
		UIDs        []string
		IncludeSelf bool
		HasTarget   bool
		Reasoning   string
	}
}

// IntentRecognitionOptions configures intent recognition behavior.
type IntentRecognitionOptions struct {
	EnableShortcut bool   // whether to enable short-circuit detection
	CreatorUID     string // UID of the user who created the summary task
}

// recognizeIntentResult is the raw LLM output structure.
type recognizeIntentResult struct {
	// Time
	HasTimeExpr bool   `json:"has_time_expr"`
	TimeStart   string `json:"time_start"`
	TimeEnd     string `json:"time_end"`

	// Channel
	HasChannelConstraint bool     `json:"has_channel_constraint"`
	ChannelIDs           []string `json:"channel_ids"`
	ChannelType          []string `json:"channel_type"`
	ChannelPersons       []string `json:"channel_persons"`
	ChannelPersonMode    string   `json:"channel_person_mode"`
	ChannelIncludeSelf   bool     `json:"channel_include_self"`

	// Target person
	HasTarget   bool     `json:"has_target"`
	TargetUIDs  []string `json:"target_uids"`
	IncludeSelf bool     `json:"include_self"`

	// Reasoning
	Reasoning string `json:"reasoning"`
}

var recognizeIntentTool = service.Tool{
	Type: "function",
	Function: service.ToolFunction{
		Name:        "recognize_intent",
		Description: "解析用户查询意图，包括时间范围、频道范围和目标人物",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				// 时间相关
				"has_time_expr": map[string]interface{}{
					"type":        "boolean",
					"description": "主题中是否包含时间表达式",
				},
				"time_start": map[string]interface{}{
					"type":        "string",
					"description": "时间范围起始，RFC3339 格式。无时间表达式时为空字符串",
				},
				"time_end": map[string]interface{}{
					"type":        "string",
					"description": "时间范围结束，RFC3339 格式。无时间表达式时为空字符串",
				},

				// 频道相关
				"has_channel_constraint": map[string]interface{}{
					"type":        "boolean",
					"description": "主题中是否包含频道范围约束（人物、频道名、频道类型等）。如果主题是通用的（如'项目进度'、'最近在忙什么'），则为 false",
				},
				"channel_ids": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "主题中提到的频道对应的 channel_id。只能从候选频道列表中选取，不得编造。无频道约束时为空数组",
				},
				"channel_type": map[string]interface{}{
					"type":  "array",
					"items": map[string]interface{}{"type": "string", "enum": []string{"group", "dm", "thread"}},
					"description": "限定频道类型。'私聊'/'DM'→[\"dm\"]；'群'/'群组'→[\"group\"]；'子区'/'thread'→[\"thread\"]；无限定时为空数组",
				},
				"channel_persons": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "用于频道筛选的人物 UID（如'我和Alice的聊天'→需找包含这两人的频道）。只能从成员列表中选取 UID",
				},
				"channel_person_mode": map[string]interface{}{
					"type":        "string",
					"enum":        []string{"intersection", "union"},
					"description": "channel_persons 的组合模式。'intersection'：所有人需同时出现；'union'：任一人出现即可。默认 'intersection'",
				},
				"channel_include_self": map[string]interface{}{
					"type":        "boolean",
					"description": "频道筛选中'我'是否作为参与方。如'我和Bob聊了什么'→true",
				},

				// 人物相关
				"has_target": map[string]interface{}{
					"type":        "boolean",
					"description": "主题是否指向特定成员的发言（用于消息过滤）",
				},
				"target_uids": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "目标成员的 UID 列表（用于过滤只看这些人的消息）。只能从成员列表中选取 UID 原文，不得编造",
				},
				"include_self": map[string]interface{}{
					"type":        "boolean",
					"description": "仅当主题中【显式出现第一人称\"我/我的/我说/我发\"】且\"我\"是【对话参与者/发言人】时为 true。\"这个群/本群\"等范围词不触发",
				},

				// 推理过程
				"reasoning": map[string]interface{}{
					"type":        "string",
					"description": "一句话解释判断依据",
				},
			},
			"required": []string{
				"has_time_expr",
				"has_channel_constraint",
				"has_target",
				"include_self",
				"reasoning",
			},
		},
	},
}

// RecognizeIntent performs unified intent recognition in a single LLM call.
// It extracts time range, channel scope, and target persons from the topic.
//
// This replaces the previous three separate calls:
//   - PreRetrievalNarrow (time)
//   - ResolveChannelScope (channel)
//   - ResolveTopicTarget (person)
func RecognizeIntent(
	ctx context.Context,
	topic string,
	originalStart, originalEnd time.Time,
	channels []ChannelInfo,
	memberMap map[string]string, // UID → name
	creatorUID string,
	toolCallFn LLMToolCallFn,
) (*IntentResult, error) {
	result := &IntentResult{}
	result.TimeRange.Start = originalStart
	result.TimeRange.End = originalEnd

	if topic == "" || toolCallFn == nil {
		return result, nil
	}

	// Truncate topic if too long
	if utf8.RuneCountInString(topic) > 1000 {
		runes := []rune(topic)
		topic = string(runes[:1000])
	}
	topic = sanitizeTopic(topic)

	// Build prompt
	now := time.Now()
	currentDate := now.Format("2006-01-02")
	weekdays := [...]string{"日", "一", "二", "三", "四", "五", "六"}
	weekday := weekdays[now.Weekday()]

	// Build member list for prompt
	var memberLines []string
	if len(memberMap) > 0 {
		type member struct {
			UID  string
			Name string
		}
		var members []member
		for uid, name := range memberMap {
			if name != "" {
				members = append(members, member{UID: uid, Name: name})
			}
		}
		sort.Slice(members, func(i, j int) bool {
			return members[i].UID < members[j].UID
		})
		for _, m := range members {
			memberLines = append(memberLines, fmt.Sprintf("- UID: %s, 姓名: %s", m.UID, m.Name))
		}
	}

	// Build channel list for prompt
	var channelLines []string
	if len(channels) > 0 {
		for _, ch := range channels {
			chType := "group"
			switch ch.ChannelType {
			case 1:
				chType = "dm"
			case 5:
				chType = "thread"
			}
			channelLines = append(channelLines, fmt.Sprintf("- ID: %s, 名称: %s, 类型: %s", ch.ChannelID, ch.ChannelName, chType))
		}
	}

	systemPrompt := buildIntentSystemPrompt(currentDate, weekday, now.Format("15:04"), memberLines, channelLines, creatorUID)

	userMsg := fmt.Sprintf(`请分析以下总结主题的意图。

总结主题："%s"
创建者 UID：%s`, topic, creatorUID)

	messages := []service.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userMsg},
	}

	start := time.Now()
	argsJSON, err := toolCallFn(ctx, messages, []service.Tool{recognizeIntentTool}, "recognize_intent")
	elapsed := time.Since(start).Milliseconds()

	if err != nil {
		log.Printf("[intent] RecognizeIntent: tool call error=%v (took %dms), using defaults", err, elapsed)
		return result, nil // fallback to defaults, not error
	}

	var parsed recognizeIntentResult
	if err := json.Unmarshal([]byte(argsJSON), &parsed); err != nil {
		log.Printf("[intent] RecognizeIntent: parse error=%v, args=%s", err, argsJSON)
		return result, nil
	}

	log.Printf("[intent] RecognizeIntent: topic=%q took=%dms result={time:%v, channel:%v, target:%v}",
		truncateForLog(topic), elapsed, parsed.HasTimeExpr, parsed.HasChannelConstraint, parsed.HasTarget)

	// Apply time range
	if parsed.HasTimeExpr && parsed.TimeStart != "" && parsed.TimeEnd != "" {
		parsedStart, err1 := time.Parse(time.RFC3339, parsed.TimeStart)
		parsedEnd, err2 := time.Parse(time.RFC3339, parsed.TimeEnd)
		if err1 == nil && err2 == nil {
			// Clamp to original range
			if parsedStart.Before(originalStart) {
				parsedStart = originalStart
			}
			if parsedEnd.After(originalEnd) {
				parsedEnd = originalEnd
			}
			if parsedStart.Before(parsedEnd) {
				result.TimeRange.Start = parsedStart
				result.TimeRange.End = parsedEnd
				result.TimeRange.HasTimeExpr = true
				result.TimeRange.Narrowed = true
				log.Printf("[intent] time narrowed: [%s ~ %s] → [%s ~ %s]",
					originalStart.Format("01-02"), originalEnd.Format("01-02"),
					parsedStart.Format("01-02 15:04"), parsedEnd.Format("01-02 15:04"))
			}
		}
	}
	result.TimeRange.Reasoning = parsed.Reasoning

	// Apply channel scope
	result.ChannelScope.HasConstraint = parsed.HasChannelConstraint
	if parsed.HasChannelConstraint {
		// Validate channel IDs against candidates
		validChannelIDs := make(map[string]bool)
		for _, ch := range channels {
			validChannelIDs[ch.ChannelID] = true
		}
		for _, id := range parsed.ChannelIDs {
			if validChannelIDs[id] {
				result.ChannelScope.ChannelIDs = append(result.ChannelScope.ChannelIDs, id)
			}
		}
		result.ChannelScope.ChannelType = parsed.ChannelType

		// Validate person UIDs
		for _, uid := range parsed.ChannelPersons {
			if _, ok := memberMap[uid]; ok {
				result.ChannelScope.Persons = append(result.ChannelScope.Persons, uid)
			}
		}
		result.ChannelScope.PersonMode = parsed.ChannelPersonMode
		if result.ChannelScope.PersonMode == "" {
			result.ChannelScope.PersonMode = "intersection"
		}
		result.ChannelScope.IncludeSelf = parsed.ChannelIncludeSelf
	}
	result.ChannelScope.Reasoning = parsed.Reasoning

	// Apply target persons
	result.TargetPersons.HasTarget = parsed.HasTarget
	result.TargetPersons.IncludeSelf = parsed.IncludeSelf
	if parsed.HasTarget {
		// Validate UIDs
		for _, uid := range parsed.TargetUIDs {
			if _, ok := memberMap[uid]; ok {
				result.TargetPersons.UIDs = append(result.TargetPersons.UIDs, uid)
			} else {
				log.Printf("[intent] unknown target UID %q, skipping", uid)
			}
		}
		// Handle include_self
		if parsed.IncludeSelf && creatorUID != "" {
			hasCreator := false
			for _, uid := range result.TargetPersons.UIDs {
				if uid == creatorUID {
					hasCreator = true
					break
				}
			}
			if !hasCreator {
				result.TargetPersons.UIDs = append(result.TargetPersons.UIDs, creatorUID)
			}
		}
	}
	result.TargetPersons.Reasoning = parsed.Reasoning

	return result, nil
}

func buildIntentSystemPrompt(currentDate, weekday, currentTime string, memberLines, channelLines []string, creatorUID string) string {
	var b strings.Builder

	b.WriteString(`你是一个智能总结意图解析器。根据用户的总结主题，判断：
1. 时间范围：是否包含时间表达式，如果有则解析出精确时间
2. 频道范围：是否包含频道约束（特定频道、频道类型、特定人的聊天）
3. 目标人物：是否要筛选特定人的消息

`)

	fmt.Fprintf(&b, `当前日期：%s（星期%s）
当前时间：%s

`, currentDate, weekday, currentTime)

	b.WriteString(`=== 时间解析规则 ===
- "今天" = 当天 00:00:00 ~ 23:59:59
- "昨天" = 前一天 00:00:00 ~ 23:59:59
- "本周" = 本周一 00:00:00 ~ 当前时间
- "上周" = 上周一 00:00:00 ~ 上周日 23:59:59
- "最近N天" = N天前 00:00:00 ~ 当前时间
- "这几天" = 最近3天
- 时区统一使用 +08:00
- 如果没有任何时间表达式，has_time_expr 设为 false

`)

	b.WriteString(`=== 频道范围规则 ===
- 如果主题提到特定频道名，从候选频道列表中匹配 channel_ids
- 如果主题限定频道类型（私聊/群/子区），设置 channel_type
- 如果主题涉及特定人的聊天（如"我和Alice的对话"），设置 channel_persons + channel_include_self
- 范围词"这个群/本群/这里"等指当前 context，不算频道约束（has_channel_constraint=false）
- 通用主题（如"项目进度"、"最近在聊什么"）没有频道约束

`)

	b.WriteString(`=== 目标人物规则 ===
- 如果主题要看特定人的发言（如"老王的观点"、"Alice说了什么"），设置 target_uids
- target_uids 只能从成员列表中选取 UID，不得编造
- 名字匹配支持语义关联：昵称、简称、姓氏称呼、职位等
- 两个名字之间没有语义关联时，不算匹配

关于 include_self 的严格判定：
1. 范围词 ≠ 人物："这个群/这里/本群"是来源范围，不是人物，不触发 include_self
2. 第一人称必须显式出现："我/我的/我说/我发的"在主题中字面出现时，include_self=true
3. 同时含范围词和"我"时：以"我"为准 → include_self=true

示例：
  ✅ "我说了什么" → include_self=true
  ✅ "总结这个群" → include_self=false, has_target=false
  ✅ "老王的发言" → include_self=false, target_uids=[王明的UID]
  ❌ 把"总结这个群"判成 include_self=true（错误）

`)

	if len(memberLines) > 0 {
		b.WriteString("=== 成员列表 ===\n")
		b.WriteString(strings.Join(memberLines, "\n"))
		b.WriteString("\n\n")
	}

	if len(channelLines) > 0 {
		b.WriteString("=== 候选频道列表 ===\n")
		b.WriteString(strings.Join(channelLines, "\n"))
		b.WriteString("\n\n")
	}

	fmt.Fprintf(&b, "创建者 UID：%s\n\n", creatorUID)

	b.WriteString("你必须调用 recognize_intent 工具来返回结果，不要以文本形式回复。")

	return b.String()
}

// truncateForLog truncates a string for logging purposes.
func truncateForLog(s string) string {
	const maxLen = 50
	runes := []rune(s)
	if len(runes) > maxLen {
		return string(runes[:maxLen]) + "..."
	}
	return s
}

// RecognizeIntentWithShortcut performs intent recognition with optional short-circuit.
// If short-circuit conditions are met, returns default intent without LLM calls.
// Otherwise, calls RecognizeIntent for full LLM-based recognition.
func RecognizeIntentWithShortcut(
	ctx context.Context,
	topic string,
	specifiedSources []string,
	originalStart, originalEnd time.Time,
	channels []ChannelInfo,
	memberMap map[string]string,
	creatorUID string,
	enableShortcut bool,
	toolCallFn LLMToolCallFn,
) (*IntentResult, error) {
	// Check short-circuit conditions
	if ShouldSkipIntentRecognition(topic, specifiedSources, enableShortcut) {
		reason := GetSkipReason(topic, specifiedSources)
		log.Printf("[intent] short-circuit: topic=%q reason=%s", truncateForLog(topic), reason)

		return &IntentResult{
			Skipped:    true,
			SkipReason: reason,
			TimeRange: struct {
				Start       time.Time
				End         time.Time
				HasTimeExpr bool
				Narrowed    bool
				Reasoning   string
			}{
				Start:     originalStart,
				End:       originalEnd,
				Reasoning: "short-circuited: " + reason,
			},
		}, nil
	}

	// Full LLM-based recognition
	result, err := RecognizeIntent(ctx, topic, originalStart, originalEnd, channels, memberMap, creatorUID, toolCallFn)
	if err != nil {
		return nil, err
	}
	result.Skipped = false
	return result, nil
}

// BuildIntentResultFromLegacy builds an IntentResult from the legacy three-call results.
// This is a compatibility helper for the current implementation.
func BuildIntentResultFromLegacy(
	originalStart, originalEnd time.Time,
	narrowedStart, narrowedEnd time.Time,
	channelScopeResult *ChannelScopeResult,
	targetUIDs []string,
	includeSelf bool,
) *IntentResult {
	result := &IntentResult{
		Skipped:    false,
		SkipReason: "",
	}

	// Time range
	result.TimeRange.Start = narrowedStart
	result.TimeRange.End = narrowedEnd
	result.TimeRange.HasTimeExpr = !narrowedStart.Equal(originalStart) || !narrowedEnd.Equal(originalEnd)
	result.TimeRange.Narrowed = result.TimeRange.HasTimeExpr

	// Channel scope
	if channelScopeResult != nil {
		result.ChannelScope.HasConstraint = channelScopeResult.HasConstraint
		result.ChannelScope.Reasoning = channelScopeResult.Reasoning
	}

	// Target persons
	result.TargetPersons.UIDs = targetUIDs
	result.TargetPersons.IncludeSelf = includeSelf
	result.TargetPersons.HasTarget = len(targetUIDs) > 0

	return result
}
