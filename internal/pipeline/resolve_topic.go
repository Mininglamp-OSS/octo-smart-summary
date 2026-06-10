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
	HasTarget   bool     `json:"has_target"`
	UIDs        []string `json:"uids"`
	IncludeSelf bool     `json:"include_self"`
	Reasoning   string   `json:"reasoning"`
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
					"description": "目标成员的 UID 列表。只能从成员列表中选取 UID 原文，不得编造或推测。has_target 为 false 时为空数组",
				},
				"reasoning": map[string]interface{}{
					"type":        "string",
					"description": "一句话解释判断依据",
				},
				"include_self": map[string]interface{}{
					"type":        "boolean",
					"description": "仅当主题中【显式出现第一人称\"我/我的/我说/我发\"】且\"我\"是【对话参与者/发言人】时为 true。判定铁律：①\"我\"必须真实出现在主题文字中，不得由\"群/会话/这里\"等词推断；②\"这个群/这里/本群/本群聊/这个子区/这个频道/这些群\"等词是【信息来源范围】的指代，不是人物，绝不触发 include_self；③只关注他人或泛主题时为 false。",
				},
			},
			"required": []string{"has_target", "uids", "include_self", "reasoning"},
		},
	},
}

// ResolveTopicTarget uses LLM Function Call to semantically resolve the target person
// referenced in the topic. Returns targetUIDs for FilterWithContext.
// Returns nil when topic has no specific target or on any failure (caller skips filter).
func ResolveTopicTarget(ctx context.Context, topic string, nameMap map[string]string, defaultUID string, toolCallFn LLMToolCallFn) []string {
	if topic == "" || toolCallFn == nil {
		return nil
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
		return nil
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
- 主题是关于某个特定成员的内容（如"老王的发言"、"CTO的观点"），返回该成员 UID
- 主题包含多个人（如"老王和小李的讨论"），返回所有相关成员的 UID
- 主题不涉及特定人物（如"项目进度"、"最近在聊什么"），has_target 为 false
- uids 只能从成员列表中选取，不得编造或推测不存在的 UID
- 主题中提到的人物在成员列表中找不到匹配时，忽略该人物；所有人物都找不到时，has_target 为 false（此规则不影响"我"的处理——"我"始终通过 include_self 标识，不需要出现在成员列表中）
- 名字匹配支持语义关联：昵称、简称、姓氏称呼、职位等，但必须与成员姓名存在明确的语义关联（如包含关系、谐音、常见缩写）
- 两个名字之间没有语义关联时，不算匹配

关于 include_self 的严格判定（按顺序逐条检查）：

1. 范围词 ≠ 人物：主题里的"这个群 / 这里 / 本群 / 本群聊 / 这个子区 / 这个频道 / 这个会话 / 这些群"一律视为【要总结的来源范围】，不是任何人物。它们【绝不】使 include_self 为 true。
   → 只要主题没有显式的第一人称"我"，include_self 必为 false。
2. 第一人称必须显式出现："我 / 我的 / 我说 / 我发的 / 我参与的"在主题文字中【字面出现】，且"我"是对话的参与者/发言人时，include_self 才为 true。
3. 同时含范围词和"我"时（如"我在这个群说了什么"）：以"我"为准 → include_self = true，范围词只缩小检索范围，不改变"我是人物目标"这一事实。
4. 既无第一人称"我"、也只是范围指代或泛主题 → include_self = false，且若无任何具名他人 → has_target = false。

示例：
  ✅ "老王" → "王明"（老+姓氏 = 常见称呼方式，语义关联明确）
  ✅ "老王" → "王建国"（老+姓氏 = 常见称呼方式）
  ✅ "Tom" → "汤姆"（同一名字的中英文形式）
  ❌ "Alice" → "Carol"（两个名字之间没有任何语义关联）
  ❌ "小李" → "张伟"（没有语义关联，即使只有这一个成员）

include_self 判定示例：
  ✅ "我说了什么" / "我的工作"          → include_self=true（显式"我"）
  ✅ "我在这个子区说了什么"            → include_self=true（含"我"，范围词不影响）
  ✅ "我和老王聊了啥"                  → include_self=true + uids=[王明]
  ✅ "总结这个群的信息"               → include_self=false（"这个群"是范围，无"我"）
  ✅ "总结这里的内容" / "看看本群聊了啥" → include_self=false
  ✅ "总结这个子区"                    → include_self=false（"子区"作范围）
  ✅ "老王的发言"                      → include_self=false + uids=[王明]
  ✅ "项目进度"                        → include_self=false, has_target=false
  ❌ 把"总结这个群"判成 include_self=true（错：没有"我"，范围词不算第一人称）
  ❌ 把"我在这个群说了啥"判成 false（错：有显式"我"）

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

	start := time.Now()
	argsJSON, err := toolCallFn(ctx, messages, []service.Tool{resolveTopicTargetTool}, "resolve_topic_target")
	elapsed := time.Since(start).Milliseconds()

	if err != nil {
		log.Printf("[pipeline] CallWithTools: tool=resolve_topic_target input={topic:%q, members:%d} took %dms error=%v, fallback to no target", topicSafe, len(members), elapsed, err)
		return nil
	}

	var parsed TopicResolveResult
	if err := json.Unmarshal([]byte(argsJSON), &parsed); err != nil {
		log.Printf("[pipeline] CallWithTools: tool=resolve_topic_target input={topic:%q, members:%d} took %dms parse_error=%v, args=%s, fallback to no target", topicSafe, len(members), elapsed, err, argsJSON)
		return nil
	}

	log.Printf("[pipeline] CallWithTools: tool=resolve_topic_target input={topic:%q, members:%d} took %dms result={has_target:%v, uids:%v}", topicSafe, len(members), elapsed, parsed.HasTarget, parsed.UIDs)

	if !parsed.HasTarget {
		log.Printf("[pipeline] ResolveTopicTarget: no target in topic, reason=%s", parsed.Reasoning)
		return nil
	}

	if len(parsed.UIDs) == 0 && parsed.IncludeSelf {
		log.Printf("[pipeline] ResolveTopicTarget: self-reference topic, reason=%s", parsed.Reasoning)
		return []string{defaultUID}
	}

	if len(parsed.UIDs) == 0 {
		log.Printf("[pipeline] ResolveTopicTarget: has_target but no UIDs, reason=%s", parsed.Reasoning)
		return nil
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
		if parsed.IncludeSelf {
			log.Printf("[pipeline] ResolveTopicTarget: all UIDs invalid but include_self=true, using creator")
			return []string{defaultUID}
		}
		log.Printf("[pipeline] ResolveTopicTarget: all UIDs invalid, fallback to no target")
		return nil
	}

	if parsed.IncludeSelf {
		hasCreator := false
		for _, uid := range validUIDs {
			if uid == defaultUID {
				hasCreator = true
				break
			}
		}
		if !hasCreator {
			validUIDs = append(validUIDs, defaultUID)
		}
	}

	log.Printf("[pipeline] ResolveTopicTarget: resolved %d target(s) %v, reason=%s",
		len(validUIDs), validUIDs, parsed.Reasoning)
	return validUIDs
}
