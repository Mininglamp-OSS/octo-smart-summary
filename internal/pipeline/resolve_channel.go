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
	"gorm.io/gorm"
)

// ChannelScopeOptions controls Layer 1.7 behavior.
type ChannelScopeOptions struct {
	Enabled bool
}

// ChannelScopeRule represents a single channel scope constraint rule.
type ChannelScopeRule struct {
	Persons     []string `json:"persons,omitempty"`
	PersonMode  string   `json:"person_mode,omitempty"`
	IncludeSelf bool     `json:"include_self,omitempty"`
	ChannelIDs  []string `json:"channel_ids,omitempty"`
	ChannelType []string `json:"channel_type,omitempty"`
	Ownership   []string `json:"ownership,omitempty"`
}

// ChannelScopeResult is the structured output from resolve_channel_scope Function Call.
type ChannelScopeResult struct {
	HasConstraint bool               `json:"has_constraint"`
	Rules         []ChannelScopeRule `json:"rules"`
	Reasoning     string             `json:"reasoning"`
}

var resolveChannelScopeTool = service.Tool{
	Type: "function",
	Function: service.ToolFunction{
		Name:        "resolve_channel_scope",
		Description: "从用户的总结主题中提取频道范围约束，判断用户想看哪些频道的内容",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"has_constraint": map[string]interface{}{
					"type":        "boolean",
					"description": "主题中是否包含频道范围约束（人物、频道名、频道类型等）。如果主题是通用的（如'项目进度'、'最近在忙什么'），则为 false",
				},
				"rules": map[string]interface{}{
					"type": "array",
					"items": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"persons": map[string]interface{}{
								"type":        "array",
								"items":       map[string]interface{}{"type": "string"},
								"description": "主题中提到的人物对应的 UID。只能从成员列表中选取 UID 原文，不得编造。无人物时为空数组或省略",
							},
							"person_mode": map[string]interface{}{
								"type":        "string",
								"enum":        []string{"intersection", "union"},
								"description": "persons 之间的组合模式。\"intersection\"：所有人需同时出现在频道中（如'我和Alice聊了什么'）；\"union\"：任一人出现即可（如'我、Alice、Bob在搞什么'）。默认 \"intersection\"",
							},
							"include_self": map[string]interface{}{
								"type":        "boolean",
								"description": "主题中'我'是否作为对话参与方出现。如'我和Bob聊了什么'→true；'Alice最近在忙什么'→false",
							},
							"channel_ids": map[string]interface{}{
								"type":        "array",
								"items":       map[string]interface{}{"type": "string"},
								"description": "主题中提到的频道对应的 channel_id。只能从候选频道列表中选取，不得编造。支持模糊匹配（如'dev相关的群'可选取所有名称含dev的频道）。无频道名时为空数组或省略",
							},
							"channel_type": map[string]interface{}{
								"type":  "array",
								"items": map[string]interface{}{"type": "string", "enum": []string{"group", "dm", "thread"}},
								"description": "限定频道类型。'私聊'/'DM'→[\"dm\"]；'群'/'群组'→[\"group\"]；'群聊和私聊'→[\"group\",\"dm\"]；无限定时为空数组或省略",
							},
							"ownership": map[string]interface{}{
								"type":  "array",
								"items": map[string]interface{}{"type": "string", "enum": []string{"creator", "admin", "member"}},
								"description": "限定频道所有权角色。'我建的群'→[\"creator\"]；'我管理的群'→[\"creator\",\"admin\"]；'我参与但不管理的群'→[\"member\"]；无限定时为空数组或省略",
							},
						},
					},
					"description": "频道约束规则列表。规则之间为 OR（并集）关系；同一规则内各维度为 AND（交集）关系。当 has_constraint=false 时为空数组",
				},
				"reasoning": map[string]interface{}{
					"type":        "string",
					"description": "一句话解释判断依据",
				},
			},
			"required": []string{"has_constraint", "rules", "reasoning"},
		},
	},
}

const resolveChannelScopeSystemPrompt = `你是一个频道范围解析器。根据用户的总结主题、成员列表和频道列表，判断用户想查看哪些频道的内容。

输出结构采用 DNF（析取范式）：rules 数组中的规则之间为 OR（并集），同一规则内各字段为 AND（交集）。

规则：
- persons：从成员列表中选取与主题中提到的人物语义匹配的 UID
  - 名字匹配支持语义关联：昵称、简称、姓氏称呼、职位、中英文对照等
  - 必须与成员姓名存在明确的语义关联（如包含关系、谐音、常见缩写）
  - 两个名字之间没有语义关联时，不算匹配
  - "我" 通过 include_self=true 标识，不放入 persons 数组
  - 只能从成员列表中选取 UID，不得编造或推测不存在的 UID
- person_mode：控制 persons 之间的组合逻辑
  - "intersection"（默认）：所有 persons 必须同时出现在频道中。用于"我和A聊了什么"、"A和B讨论了什么"
  - "union"：任一 person 出现在频道中即可。用于"我、A、B在搞什么"（列举相关人，不要求共现）
- channel_ids：从候选频道列表中选取与主题匹配的频道 ID
  - 精确匹配：主题明确指定频道名，选取对应 ID
  - 模糊匹配：主题含关键词（如"dev相关"），选取所有名称中包含该关键词的频道
  - 只能从候选频道列表中选取 ID，不得编造
- channel_type：数组形式，仅当主题明确限定频道类型时设置
  - "私聊"、"DM" → ["dm"]
  - "群"、"群组"、"群聊" → ["group"]
  - "子区"、"thread"、"话题" → ["thread"]
  - "群聊和私聊" → ["group", "dm"]
  - 没有明确限定 → 省略或空数组
- ownership：数组形式，仅当主题明确提到创建者/管理员/成员身份时设置
  - "我建的群"、"我创建的" → ["creator"]
  - "我管理的群" → ["creator", "admin"]（创建者也算管理）
  - "我参与但不管理的群" → ["member"]
  - 没有明确提到 → 省略或空数组
- 当主题包含多个独立约束条件需要并集时，拆分为多条 rules
  - 例："总结下我的全部私聊，以及我和Alice所在群聊" → 两条规则
- 如果主题不包含任何频道范围约束（如"项目进度"、"最近在聊什么"），has_constraint 为 false，rules 为空数组

示例：
- "我和Alice聊了什么"
  → has_constraint=true, rules=[{persons:[AliceUID], include_self:true, person_mode:"intersection"}]
- "我、Alice、Bob在搞什么"
  → has_constraint=true, rules=[{persons:[AliceUID, BobUID], include_self:true, person_mode:"union"}]
- "octo-dev群最近在聊什么"
  → has_constraint=true, rules=[{channel_ids:[octo-dev的ID]}]
- "私聊里Bob说了什么"
  → has_constraint=true, rules=[{persons:[BobUID], channel_type:["dm"], person_mode:"intersection"}]
- "dev相关的群"
  → has_constraint=true, rules=[{channel_ids:[所有含dev的频道ID], channel_type:["group"]}]
- "最近大家在讨论什么"
  → has_constraint=false, rules=[]
- "我建的群在聊什么"
  → has_constraint=true, rules=[{ownership:["creator"]}]
- "我参与但不管理的群"
  → has_constraint=true, rules=[{ownership:["member"]}]
- "总结下我的全部私聊，以及我和Alice所在群聊的全部信息"
  → has_constraint=true, rules=[{channel_type:["dm"], include_self:true}, {persons:[AliceUID], include_self:true, channel_type:["group"], person_mode:"intersection"}]

你必须调用 resolve_channel_scope 工具来返回结果，不要以文本形式回复。`

// ResolveChannelScope extracts channel constraints from topic and filters candidates (Layer 1.7).
func ResolveChannelScope(
	ctx context.Context,
	topic string,
	candidates []ChannelInfo,
	creatorUID string,
	memberMap map[string]string,
	imDB *gorm.DB,
	toolCallFn LLMToolCallFn,
) []ChannelInfo {
	if topic == "" || len(candidates) == 0 || toolCallFn == nil {
		return candidates
	}

	parsed, err := callResolveChannelScope(ctx, topic, candidates, memberMap, toolCallFn)
	if err != nil || !parsed.HasConstraint || len(parsed.Rules) == 0 {
		return candidates
	}

	unionSet := make(map[string]bool)
	for _, rule := range parsed.Rules {
		ruleResult := executeRule(ctx, rule, candidates, creatorUID, memberMap, imDB)
		for _, ch := range ruleResult {
			unionSet[ch.ChannelID] = true
		}
	}

	var result []ChannelInfo
	for _, ch := range candidates {
		if unionSet[ch.ChannelID] {
			result = append(result, ch)
		}
	}

	if len(result) == 0 {
		log.Printf("[pipeline] ResolveChannelScope: all rules resulted in 0 channels, fallback to all %d candidates",
			len(candidates))
		return candidates
	}

	log.Printf("[pipeline] ResolveChannelScope: narrowed %d → %d channels (%d rules), reason=%s",
		len(candidates), len(result), len(parsed.Rules), parsed.Reasoning)
	return result
}

// executeRule executes a single rule with AND across dimensions.
func executeRule(
	ctx context.Context,
	rule ChannelScopeRule,
	candidates []ChannelInfo,
	creatorUID string,
	memberMap map[string]string,
	imDB *gorm.DB,
) []ChannelInfo {
	result := candidates

	if len(rule.ChannelType) > 0 {
		result = filterByChannelTypes(result, rule.ChannelType)
	}

	if len(rule.ChannelIDs) > 0 {
		result = filterByChannelIDs(result, rule.ChannelIDs, candidates)
	}

	if len(rule.Ownership) > 0 {
		result = filterByOwnership(ctx, result, creatorUID, rule.Ownership, imDB)
	}

	if len(rule.Persons) > 0 || rule.IncludeSelf {
		personMode := rule.PersonMode
		if personMode == "" {
			personMode = "intersection"
		}
		result = filterByPersonUIDs(ctx, result, rule.Persons,
			rule.IncludeSelf, creatorUID, personMode, memberMap, imDB)
	}

	return result
}

func filterByChannelTypes(candidates []ChannelInfo, channelTypes []string) []ChannelInfo {
	typeMap := map[string]int{
		"dm":     1,
		"group":  2,
		"thread": 5,
	}

	targetTypes := make(map[int]bool)
	for _, t := range channelTypes {
		if code, ok := typeMap[t]; ok {
			targetTypes[code] = true
		}
	}
	if len(targetTypes) == 0 {
		return candidates
	}

	var result []ChannelInfo
	for _, ch := range candidates {
		if targetTypes[ch.ChannelType] {
			result = append(result, ch)
		}
	}
	return result
}

func filterByChannelIDs(current []ChannelInfo, selectedIDs []string, allCandidates []ChannelInfo) []ChannelInfo {
	validSet := make(map[string]bool, len(allCandidates))
	for _, ch := range allCandidates {
		validSet[ch.ChannelID] = true
	}

	selectedSet := make(map[string]bool, len(selectedIDs))
	for _, id := range selectedIDs {
		if validSet[id] {
			selectedSet[id] = true
		}
	}

	if len(selectedSet) == 0 {
		return current
	}

	var result []ChannelInfo
	for _, ch := range current {
		if selectedSet[ch.ChannelID] {
			result = append(result, ch)
		}
	}
	return result
}

func filterByOwnership(ctx context.Context, candidates []ChannelInfo, creatorUID string, ownership []string, imDB *gorm.DB) []ChannelInfo {
	if imDB == nil || len(ownership) == 0 {
		return candidates
	}

	var groupIDs []string
	var threadChannelIDs []string

	for _, ch := range candidates {
		switch ch.ChannelType {
		case 2:
			groupIDs = append(groupIDs, ch.ChannelID)
		case 5:
			parts := strings.Split(ch.ChannelID, "____")
			if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
				threadChannelIDs = append(threadChannelIDs, ch.ChannelID)
			}
		}
	}

	if len(groupIDs) == 0 && len(threadChannelIDs) == 0 {
		return candidates
	}

	matchedGroupSet := make(map[string]bool)
	matchedThreadSet := make(map[string]bool)
	var anyQuerySucceeded bool

	for _, role := range ownership {
		switch role {
		case "creator":
			if len(groupIDs) > 0 {
				var ids []string
				if err := imDB.WithContext(ctx).Raw(
					"SELECT group_no FROM `group` WHERE creator = ? AND group_no IN ? AND status = 1",
					creatorUID, groupIDs,
				).Pluck("group_no", &ids).Error; err != nil {
					log.Printf("[pipeline] filterByOwnership: query creator group error: %v", err)
				} else {
					anyQuerySucceeded = true
					for _, id := range ids {
						matchedGroupSet[id] = true
					}
				}
			}
			if len(threadChannelIDs) > 0 {
				var ids []string
				if err := imDB.WithContext(ctx).Raw(
					"SELECT CONCAT(group_no, '____', short_id) FROM thread WHERE creator_uid = ? AND CONCAT(group_no, '____', short_id) IN ? AND status = 1",
					creatorUID, threadChannelIDs,
				).Pluck("CONCAT(group_no, '____', short_id)", &ids).Error; err != nil {
					log.Printf("[pipeline] filterByOwnership: query creator thread error: %v", err)
				} else {
					anyQuerySucceeded = true
					for _, id := range ids {
						matchedThreadSet[id] = true
					}
				}
			}
		case "admin":
			if len(groupIDs) > 0 {
				var ids []string
				if err := imDB.WithContext(ctx).Raw(
					"SELECT group_no FROM group_member WHERE uid = ? AND role IN (1,2) AND group_no IN ? AND is_deleted = 0",
					creatorUID, groupIDs,
				).Pluck("group_no", &ids).Error; err != nil {
					log.Printf("[pipeline] filterByOwnership: query admin group error: %v", err)
				} else {
					anyQuerySucceeded = true
					for _, id := range ids {
						matchedGroupSet[id] = true
					}
				}
			}
		case "member":
			if len(groupIDs) > 0 {
				var ids []string
				if err := imDB.WithContext(ctx).Raw(
					"SELECT gm.group_no FROM group_member gm "+
						"INNER JOIN `group` g ON g.group_no = gm.group_no "+
						"WHERE gm.uid = ? AND gm.role = 0 AND g.creator != ? AND gm.group_no IN ? AND gm.is_deleted = 0 AND g.status = 1",
					creatorUID, creatorUID, groupIDs,
				).Pluck("group_no", &ids).Error; err != nil {
					log.Printf("[pipeline] filterByOwnership: query member group error: %v", err)
				} else {
					anyQuerySucceeded = true
					for _, id := range ids {
						matchedGroupSet[id] = true
					}
				}
			}
			if len(threadChannelIDs) > 0 {
				var ids []string
				if err := imDB.WithContext(ctx).Raw(
					"SELECT CONCAT(t.group_no, '____', t.short_id) FROM thread t "+
						"INNER JOIN thread_member tm ON tm.thread_id = t.id "+
						"WHERE tm.uid = ? AND t.creator_uid != ? AND CONCAT(t.group_no, '____', t.short_id) IN ? AND t.status = 1",
					creatorUID, creatorUID, threadChannelIDs,
				).Pluck("CONCAT(t.group_no, '____', t.short_id)", &ids).Error; err != nil {
					log.Printf("[pipeline] filterByOwnership: query member thread error: %v", err)
				} else {
					anyQuerySucceeded = true
					for _, id := range ids {
						matchedThreadSet[id] = true
					}
				}
			}
		}
	}

	if !anyQuerySucceeded {
		log.Printf("[pipeline] filterByOwnership: all queries failed, skipping ownership filter")
		return candidates
	}

	var result []ChannelInfo
	for _, ch := range candidates {
		switch ch.ChannelType {
		case 1:
			result = append(result, ch)
		case 2:
			if matchedGroupSet[ch.ChannelID] {
				result = append(result, ch)
			}
		case 5:
			if matchedThreadSet[ch.ChannelID] {
				result = append(result, ch)
			}
		default:
			result = append(result, ch)
		}
	}
	return result
}

func filterByPersonUIDs(
	ctx context.Context,
	candidates []ChannelInfo,
	personUIDs []string,
	includeSelf bool,
	creatorUID string,
	personMode string,
	memberMap map[string]string,
	imDB *gorm.DB,
) []ChannelInfo {
	var validUIDs []string
	for _, uid := range personUIDs {
		if _, ok := memberMap[uid]; ok {
			validUIDs = append(validUIDs, uid)
		} else {
			log.Printf("[pipeline] filterByPersonUIDs: LLM returned unknown UID %q, skipping", uid)
		}
	}

	if len(validUIDs) == 0 && !includeSelf {
		return candidates
	}

	var filterUIDs []string
	if includeSelf {
		filterUIDs = append(filterUIDs, creatorUID)
	}
	filterUIDs = append(filterUIDs, validUIDs...)

	if len(filterUIDs) == 0 {
		return candidates
	}

	candidateSet := make(map[string]bool, len(candidates))
	for _, ch := range candidates {
		candidateSet[ch.ChannelID] = true
	}

	if personMode == "union" {
		return filterByPersonUnion(ctx, candidates, filterUIDs, creatorUID, candidateSet, imDB)
	}
	return filterByPersonIntersection(ctx, candidates, filterUIDs, creatorUID, candidateSet, imDB)
}

func filterByPersonIntersection(
	ctx context.Context,
	candidates []ChannelInfo,
	filterUIDs []string,
	creatorUID string,
	candidateSet map[string]bool,
	imDB *gorm.DB,
) []ChannelInfo {
	intersection := make(map[string]bool)
	for k, v := range candidateSet {
		intersection[k] = v
	}

	for _, uid := range filterUIDs {
		if uid == creatorUID {
			continue
		}
		pChannels, err := GetUserChannels(ctx, uid, imDB)
		if err != nil {
			log.Printf("[pipeline] filterByPersonIntersection: GetUserChannels(%s) error: %v", uid, err)
			continue
		}
		pSet := make(map[string]bool, len(pChannels))
		for _, ch := range pChannels {
			pSet[ch.ChannelID] = true
		}
		for chID := range intersection {
			if !pSet[chID] {
				delete(intersection, chID)
			}
		}
	}

	var result []ChannelInfo
	for _, ch := range candidates {
		if intersection[ch.ChannelID] {
			result = append(result, ch)
		}
	}
	return result
}

func filterByPersonUnion(
	ctx context.Context,
	candidates []ChannelInfo,
	filterUIDs []string,
	creatorUID string,
	candidateSet map[string]bool,
	imDB *gorm.DB,
) []ChannelInfo {
	unionSet := make(map[string]bool)

	for _, uid := range filterUIDs {
		if uid == creatorUID {
			for chID := range candidateSet {
				unionSet[chID] = true
			}
			continue
		}
		pChannels, err := GetUserChannels(ctx, uid, imDB)
		if err != nil {
			log.Printf("[pipeline] filterByPersonUnion: GetUserChannels(%s) error: %v", uid, err)
			continue
		}
		for _, ch := range pChannels {
			if candidateSet[ch.ChannelID] {
				unionSet[ch.ChannelID] = true
			}
		}
	}

	var result []ChannelInfo
	for _, ch := range candidates {
		if unionSet[ch.ChannelID] {
			result = append(result, ch)
		}
	}
	return result
}

// BuildCandidateMemberMap derives a UID→Name mapping from Layer 1 candidates.
func BuildCandidateMemberMap(ctx context.Context, candidates []ChannelInfo, imDB *gorm.DB) (map[string]string, error) {
	if imDB == nil || len(candidates) == 0 {
		return nil, nil
	}

	var groupNos []string
	var dmPeerUIDs []string

	for _, ch := range candidates {
		switch ch.ChannelType {
		case 2:
			groupNos = append(groupNos, ch.ChannelID)
		case 1:
			if ch.PeerUID != "" {
				dmPeerUIDs = append(dmPeerUIDs, ch.PeerUID)
			}
		}
	}

	result := make(map[string]string)

	if len(groupNos) > 0 {
		type memberRow struct {
			UID  string `gorm:"column:uid"`
			Name string `gorm:"column:name"`
		}
		var rows []memberRow
		err := imDB.WithContext(ctx).Raw(`
			SELECT DISTINCT gm.uid, u.name
			FROM group_member gm
			INNER JOIN `+"`user`"+` u ON u.uid = gm.uid
			WHERE gm.group_no IN (?) AND gm.is_deleted = 0 AND u.name != ''
		`, groupNos).Scan(&rows).Error
		if err != nil {
			return nil, fmt.Errorf("query group members: %w", err)
		}
		for _, r := range rows {
			result[r.UID] = r.Name
		}
	}

	if len(dmPeerUIDs) > 0 {
		type userRow struct {
			UID  string `gorm:"column:uid"`
			Name string `gorm:"column:name"`
		}
		var rows []userRow
		err := imDB.WithContext(ctx).Raw(
			"SELECT uid, name FROM `user` WHERE uid IN (?) AND name != ''",
			dmPeerUIDs,
		).Scan(&rows).Error
		if err != nil {
			log.Printf("[pipeline] BuildCandidateMemberMap: query DM peer names error: %v", err)
		} else {
			for _, r := range rows {
				result[r.UID] = r.Name
			}
		}
	}

	return result, nil
}

func buildChannelScopeUserMsg(topic string, memberLines []string, channelLines []string) string {
	return fmt.Sprintf(`请分析以下总结主题，提取频道范围约束。

主题："%s"

成员列表：
%s

候选频道列表：
%s`, sanitizeTopic(topic), strings.Join(memberLines, "\n"), strings.Join(channelLines, "\n"))
}

func callResolveChannelScope(
	ctx context.Context,
	topic string,
	candidates []ChannelInfo,
	memberMap map[string]string,
	toolCallFn LLMToolCallFn,
) (*ChannelScopeResult, error) {
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

	const maxMembers = 500
	if len(members) > maxMembers {
		log.Printf("[pipeline] WARN: callResolveChannelScope: member list truncated %d → %d", len(members), maxMembers)
		members = members[:maxMembers]
	}

	var memberLines []string
	for _, m := range members {
		memberLines = append(memberLines, fmt.Sprintf("- UID: %s, 姓名: %s", m.UID, m.Name))
	}

	typeNames := map[int]string{1: "私聊", 2: "群组", 5: "子区"}
	channelCandidates := candidates

	const maxChannels = 200
	if len(channelCandidates) > maxChannels {
		log.Printf("[pipeline] WARN: callResolveChannelScope: channel list truncated %d → %d", len(channelCandidates), maxChannels)
		channelCandidates = channelCandidates[:maxChannels]
	}

	var channelLines []string
	for _, ch := range channelCandidates {
		typeName := typeNames[ch.ChannelType]
		if typeName == "" {
			typeName = "未知"
		}
		channelLines = append(channelLines, fmt.Sprintf("- ID: %s, 名称: %s (%s)",
			ch.ChannelID, ch.ChannelName, typeName))
	}

	userMsg := buildChannelScopeUserMsg(topic, memberLines, channelLines)
	messages := []service.ChatMessage{
		{Role: "system", Content: resolveChannelScopeSystemPrompt},
		{Role: "user", Content: userMsg},
	}

	start := time.Now()
	argsJSON, err := toolCallFn(ctx, messages, []service.Tool{resolveChannelScopeTool}, "resolve_channel_scope")
	elapsed := time.Since(start).Milliseconds()

	if err != nil {
		log.Printf("[pipeline] CallWithTools: tool=resolve_channel_scope input={topic:%q, members:%d, channels:%d} took %dms error=%v",
			topic, len(members), len(candidates), elapsed, err)
		return nil, err
	}

	var parsed ChannelScopeResult
	if err := json.Unmarshal([]byte(argsJSON), &parsed); err != nil {
		log.Printf("[pipeline] CallWithTools: tool=resolve_channel_scope input={topic:%q} took %dms parse_error=%v, args=%s",
			topic, elapsed, err, argsJSON)
		return nil, fmt.Errorf("parse channel scope result: %w", err)
	}

	log.Printf("[pipeline] CallWithTools: tool=resolve_channel_scope input={topic:%q, members:%d, channels:%d} took %dms result={has_constraint:%v, rules:%d}",
		topic, len(members), len(candidates), elapsed, parsed.HasConstraint, len(parsed.Rules))

	return &parsed, nil
}
