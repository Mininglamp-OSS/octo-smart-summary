package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
)

// ChannelInfo holds discovered channel metadata.
type ChannelInfo struct {
	ChannelID   string `json:"channel_id"`
	ChannelType int    `json:"channel_type"`
	ChannelName string `json:"channel_name"`
	SpaceID     string `json:"space_id,omitempty"`
}

// Message represents a fetched chat message.
type Message struct {
	MessageSeq    int64  `json:"message_seq"`
	SenderUID     string `json:"sender_uid"`
	SenderName    string `json:"sender_name"`
	ChannelID     string `json:"channel_id"`
	ChannelType   int    `json:"channel_type"`
	Timestamp     int64  `json:"timestamp"`
	SendTime      string `json:"send_time"`
	Content       string `json:"content"`
	SourceName    string `json:"source_name"`
	CitationIndex int    `json:"citation_index"`
	IsTargetUser  bool   `json:"is_target_user"`
}

// LLMCallFn is the type for the LLM topic-narrowing function.
type LLMCallFn func(ctx context.Context, prompt string) (string, error)

// GetUserChannels discovers all channels (group + DM) for a user. (Layer 1)
func GetUserChannels(ctx context.Context, uid string, imDB *gorm.DB) ([]ChannelInfo, error) {
	if imDB == nil {
		return nil, fmt.Errorf("IM database not available")
	}

	var channels []ChannelInfo

	// Groups
	type groupRow struct {
		ChannelID   string `gorm:"column:channel_id"`
		ChannelType int    `gorm:"column:channel_type"`
		ChannelName string `gorm:"column:channel_name"`
		SpaceID     string `gorm:"column:space_id"`
	}
	var groups []groupRow
	err := imDB.WithContext(ctx).Raw(`
		SELECT g.group_no AS channel_id,
		       2 AS channel_type,
		       g.name AS channel_name,
		       COALESCE(g.space_id, '') AS space_id
		FROM `+"`group`"+` g
		INNER JOIN group_member gm ON g.group_no = gm.group_no
		WHERE gm.uid = ?
		  AND gm.is_deleted = 0
		  AND g.status = 1
		ORDER BY g.updated_at DESC
	`, uid).Scan(&groups).Error
	if err != nil {
		return nil, fmt.Errorf("query groups: %w", err)
	}
	for _, g := range groups {
		channels = append(channels, ChannelInfo{
			ChannelID:   g.ChannelID,
			ChannelType: g.ChannelType,
			ChannelName: g.ChannelName,
			SpaceID:     g.SpaceID,
		})
	}

	// DM channels
	type dmRow struct {
		ChannelID string `gorm:"column:channel_id"`
	}
	var dms []dmRow
	err = imDB.WithContext(ctx).Raw(`
		SELECT channel_id
		FROM conversation_extra
		WHERE uid = ? AND channel_type = 1
		GROUP BY channel_id
		ORDER BY MAX(updated_at) DESC
		LIMIT 200
	`, uid).Scan(&dms).Error
	if err != nil {
		log.Printf("[pipeline] query DM channels error: %v", err)
	}
	for _, d := range dms {
		peerUID := getPeerUID(d.ChannelID, uid)
		normalized := NormalizeDMChannelID(d.ChannelID, uid, 1)
		channels = append(channels, ChannelInfo{
			ChannelID:   normalized,
			ChannelType: 1,
			ChannelName: fmt.Sprintf("私聊-%s", peerUID),
		})
	}

	// Thread channels (channelType=5)
	type threadRow struct {
		ChannelID   string `gorm:"column:channel_id"`
		ChannelType int    `gorm:"column:channel_type"`
		ChannelName string `gorm:"column:channel_name"`
		SpaceID     string `gorm:"column:space_id"`
	}
	var threadChannels []threadRow
	err = imDB.WithContext(ctx).Raw(`
		SELECT CONCAT(t.group_no, '____', t.short_id) AS channel_id,
		       5 AS channel_type,
		       CONCAT(t.name, ' · ', g.name) AS channel_name,
		       COALESCE(g.space_id, '') AS space_id
		FROM thread t
		INNER JOIN `+"`group`"+` g ON g.group_no COLLATE utf8mb4_unicode_ci = t.group_no
		INNER JOIN thread_member tm ON tm.thread_id = t.id
		WHERE tm.uid = ?
		  AND t.status = 1
		  AND g.status = 1
		ORDER BY t.updated_at DESC
	`, uid).Scan(&threadChannels).Error
	if err != nil {
		log.Printf("[pipeline] query thread channels error: %v", err)
	}
	for _, tc := range threadChannels {
		channels = append(channels, ChannelInfo{
			ChannelID:   tc.ChannelID,
			ChannelType: 5,
			ChannelName: tc.ChannelName,
			SpaceID:     tc.SpaceID,
		})
	}

	return channels, nil
}

// isValidMessageTable validates the table name against known shard names.
func isValidMessageTable(table string, tableCount int) bool {
	if tableCount <= 0 {
		tableCount = 5
	}
	if table == "message" {
		return true
	}
	for i := 1; i < tableCount; i++ {
		if table == fmt.Sprintf("message%d", i) {
			return true
		}
	}
	return false
}

func getPeerUID(channelID, selfUID string) string {
	parts := strings.SplitN(channelID, "@", 2)
	if len(parts) != 2 {
		return channelID
	}
	if parts[0] == selfUID {
		return parts[1]
	}
	return parts[0]
}

// NormalizeDMChannelID converts a logical DM channel id (peerUID or peer@self)
// into the storage-layer format: max(uid1,uid2)@min(uid1,uid2).
// For non-DM channels (channelType != 1), returns input unchanged.
func NormalizeDMChannelID(channelID string, selfUID string, channelType int) string {
	if channelType != 1 {
		return channelID
	}
	var a, b string
	if idx := strings.IndexByte(channelID, '@'); idx >= 0 {
		a = channelID[:idx]
		b = channelID[idx+1:]
	} else {
		a = channelID
		b = selfUID
	}
	if a < b {
		a, b = b, a
	}
	return a + "@" + b
}

// mapFrontendSourceType maps frontend source_type to backend channelType.
// Frontend: 1=group, 3=DM; Backend WuKongIM: 1=DM, 2=group
func mapFrontendSourceType(frontendType int) int {
	switch frontendType {
	case 1: // frontend group -> backend group
		return 2
	case 2: // frontend thread -> backend thread
		return 5
	case 3: // frontend DM -> backend DM
		return 1
	default:
		return frontendType
	}
}

// ApplySourceConstraints filters channels to only those specified. (Layer 2)
// selfUID is used to normalize DM source IDs from the frontend.
func ApplySourceConstraints(userChannels []ChannelInfo, specifiedSources []map[string]interface{}, selfUID string) []ChannelInfo {
	if len(specifiedSources) == 0 {
		return userChannels
	}
	allowed := make(map[string]bool, len(userChannels))
	for _, ch := range userChannels {
		allowed[ch.ChannelID] = true
	}
	specified := make(map[string]bool, len(specifiedSources))
	for _, s := range specifiedSources {
		if id, ok := s["source_id"].(string); ok {
			chType := 0
			if st, ok := s["source_type"].(int); ok {
				chType = st
			} else if st, ok := s["source_type"].(float64); ok {
				chType = int(st)
			}
			// Map frontend source_type to backend channelType
			backendChType := mapFrontendSourceType(chType)
			specified[NormalizeDMChannelID(id, selfUID, backendChType)] = true
		}
	}
	var result []ChannelInfo
	for _, ch := range userChannels {
		if specified[ch.ChannelID] && allowed[ch.ChannelID] {
			result = append(result, ch)
		}
	}
	return result
}

// NarrowByTopic uses LLM to filter channels relevant to the topic. (Layer 3)
func NarrowByTopic(ctx context.Context, topic string, candidates []ChannelInfo, llmFn LLMCallFn) []ChannelInfo {
	if topic == "" || len(candidates) == 0 || llmFn == nil {
		return candidates
	}

	var lines []string
	for _, c := range candidates {
		lines = append(lines, fmt.Sprintf("- %s: %s", c.ChannelID, c.ChannelName))
	}
	prompt := fmt.Sprintf(
		"用户想总结的主题是:\"%s\"\n\n候选频道列表:\n%s\n\n请从上面列表中选出与主题最相关的频道,返回 JSON 数组(只包含 channel_id):\n[\"id1\", \"id2\", ...]\n只返回 JSON,不要其他内容。",
		topic, strings.Join(lines, "\n"),
	)

	result, err := llmFn(ctx, prompt)
	if err != nil {
		return candidates
	}

	var selectedIDs []string
	if err := json.Unmarshal([]byte(result), &selectedIDs); err != nil {
		return candidates
	}

	idSet := make(map[string]bool, len(selectedIDs))
	for _, id := range selectedIDs {
		idSet[id] = true
	}

	var filtered []ChannelInfo
	for _, c := range candidates {
		if idSet[c.ChannelID] {
			filtered = append(filtered, c)
		}
	}
	if len(filtered) == 0 {
		return candidates
	}
	return filtered
}

// FetchMessagesFromChannel fetches text messages from a sharded table. (Layer 4)
// selfUID is used to normalize DM channel IDs to the storage format.
func FetchMessagesFromChannel(ctx context.Context, channelID string, channelType int, startTS, endTS int64, imDB *gorm.DB, tableCount int, selfUID string) ([]Message, error) {
	if imDB == nil {
		return nil, fmt.Errorf("IM database not available")
	}
	channelID = NormalizeDMChannelID(channelID, selfUID, channelType)
	table := MessageTable(channelID, tableCount)
	if !isValidMessageTable(table, tableCount) {
		return nil, fmt.Errorf("invalid table name: %s", table)
	}

	type msgRow struct {
		MessageSeq int64  `gorm:"column:message_seq"`
		FromUID    string `gorm:"column:from_uid"`
		ChannelID  string `gorm:"column:channel_id"`
		Timestamp  int64  `gorm:"column:timestamp"`
		Payload    []byte `gorm:"column:payload"`
	}
	var rows []msgRow

	query := fmt.Sprintf(
		"SELECT message_seq, from_uid, channel_id, `timestamp`, payload FROM `%s` WHERE channel_id = ? AND channel_type = ? AND `timestamp` BETWEEN ? AND ? AND is_deleted = 0 ORDER BY message_seq ASC LIMIT 5000",
		table,
	)
	if err := imDB.WithContext(ctx).Raw(query, channelID, channelType, startTS, endTS).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("fetch messages from %s: %w", table, err)
	}

	var messages []Message
	for _, r := range rows {
		text, ok := ExtractText(r.Payload)
		if !ok {
			continue
		}
		messages = append(messages, Message{
			MessageSeq: r.MessageSeq,
			SenderUID:  r.FromUID,
			ChannelID:  r.ChannelID,
			Timestamp:  r.Timestamp,
			SendTime:   time.Unix(r.Timestamp, 0).Format(time.RFC3339),
			Content:    text,
		})
	}
	return messages, nil
}

// IntersectParticipantChannels filters channels to only those where both
// the creator and all participants are members. (Layer 1.5)
func IntersectParticipantChannels(ctx context.Context, creatorChannels []ChannelInfo, participantUIDs []string, imDB *gorm.DB) ([]ChannelInfo, error) {
	if len(participantUIDs) == 0 {
		return creatorChannels, nil
	}

	// Start with creator's channel IDs
	intersection := make(map[string]bool, len(creatorChannels))
	for _, ch := range creatorChannels {
		intersection[ch.ChannelID] = true
	}

	// For each participant, get their channels and intersect
	for _, uid := range participantUIDs {
		pChannels, err := GetUserChannels(ctx, uid, imDB)
		if err != nil {
			return nil, fmt.Errorf("get channels for participant %s: %w", uid, err)
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
	for _, ch := range creatorChannels {
		if intersection[ch.ChannelID] {
			result = append(result, ch)
		}
	}
	return result, nil
}

// FilterByMutualActivity keeps only messages from channels where both
// the creator and at least one participant have sent messages. (Layer 4.5)
func FilterByMutualActivity(messages []Message, creatorUID string, participantUIDs []string) []Message {
	if len(participantUIDs) == 0 {
		return messages
	}

	participantSet := make(map[string]bool, len(participantUIDs))
	for _, uid := range participantUIDs {
		participantSet[uid] = true
	}

	// Group by ChannelID and check activity
	type channelActivity struct {
		creatorActive     bool
		participantActive bool
	}
	activity := make(map[string]*channelActivity)
	for _, m := range messages {
		a, ok := activity[m.ChannelID]
		if !ok {
			a = &channelActivity{}
			activity[m.ChannelID] = a
		}
		if m.SenderUID == creatorUID {
			a.creatorActive = true
		}
		if participantSet[m.SenderUID] {
			a.participantActive = true
		}
	}

	// Keep only channels where both sides are active
	activeChannels := make(map[string]bool)
	for chID, a := range activity {
		if a.creatorActive && a.participantActive {
			activeChannels[chID] = true
		}
	}

	var filtered []Message
	for _, m := range messages {
		if activeChannels[m.ChannelID] {
			filtered = append(filtered, m)
		}
	}

	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].Timestamp < filtered[j].Timestamp
	})
	return filtered
}

// FilterMessagesByRelevance filters messages by topic keywords and participant relevance.
// Rules (any match → keep):
//  1. Message sent by a participant → keep
//  2. Message content mentions a participant (e.g. @uid) → keep
//  3. Message content contains a participant name → keep
//  4. Message content contains a topic keyword → keep
//
// When participantUIDs is empty (BY_GROUP mode), only rule 4 applies.
// When topic is empty, all messages are returned.
func FilterMessagesByRelevance(messages []Message, topic string, participantUIDs []string, participantNames []string) []Message {
	if topic == "" && len(participantUIDs) == 0 {
		return messages
	}

	// Build participant UID set
	participantSet := make(map[string]bool, len(participantUIDs))
	for _, uid := range participantUIDs {
		participantSet[uid] = true
	}

	// Build lowercase participant names for matching
	var lowerNames []string
	for _, name := range participantNames {
		n := strings.TrimSpace(name)
		if n != "" {
			lowerNames = append(lowerNames, strings.ToLower(n))
		}
	}

	// Extract topic keywords (split by common delimiters)
	var keywords []string
	if topic != "" {
		for _, kw := range strings.FieldsFunc(topic, func(r rune) bool {
			return r == ' ' || r == ',' || r == '、' || r == '，' || r == '/' || r == '|'
		}) {
			kw = strings.TrimSpace(kw)
			if len(kw) > 0 {
				keywords = append(keywords, strings.ToLower(kw))
			}
		}
	}

	// If no filter criteria at all, return everything
	if len(participantSet) == 0 && len(keywords) == 0 {
		return messages
	}

	var filtered []Message
	for _, m := range messages {
		contentLower := strings.ToLower(m.Content)

		// Rule 1: sender is a participant
		if participantSet[m.SenderUID] {
			filtered = append(filtered, m)
			continue
		}

		// Rule 2: content mentions @participant
		mentionMatch := false
		for _, uid := range participantUIDs {
			if strings.Contains(m.Content, "@"+uid) {
				mentionMatch = true
				break
			}
		}
		if mentionMatch {
			filtered = append(filtered, m)
			continue
		}

		// Rule 3: content contains participant name
		nameMatch := false
		for _, name := range lowerNames {
			if strings.Contains(contentLower, name) {
				nameMatch = true
				break
			}
		}
		if nameMatch {
			filtered = append(filtered, m)
			continue
		}

		// Rule 4: content contains topic keyword
		kwMatch := false
		for _, kw := range keywords {
			if strings.Contains(contentLower, kw) {
				kwMatch = true
				break
			}
		}
		if kwMatch {
			filtered = append(filtered, m)
			continue
		}
	}

	// If filtering removed everything, return original to avoid empty results
	if len(filtered) == 0 {
		return messages
	}
	return filtered
}

// ResolveAndFetchMessagesForPersonal runs the pipeline with participant-aware
// filtering: Layer 1.5 (channel intersection) and Layer 4.5 (mutual activity).
func ResolveAndFetchMessagesForPersonal(ctx context.Context, creatorUID string, participantUIDs []string, participantNames []string, specifiedSources []map[string]interface{}, topic string, timeStart, timeEnd time.Time, imDB *gorm.DB, llmFn LLMCallFn, tableCount int) ([]Message, error) {
	if timeEnd.Sub(timeStart) > 31*24*time.Hour {
		return nil, fmt.Errorf("时间范围不能超过 31 天")
	}

	startTS := timeStart.Unix()
	endTS := timeEnd.Unix()

	// Layer 1: channel discovery
	userChannels, err := GetUserChannels(ctx, creatorUID, imDB)
	if err != nil {
		return nil, fmt.Errorf("channel discovery: %w", err)
	}
	log.Printf("[pipeline-personal] Layer 1: discovered %d channels for creator", len(userChannels))

	// Layer 1.5: intersect with participant channels
	userChannels, err = IntersectParticipantChannels(ctx, userChannels, participantUIDs, imDB)
	if err != nil {
		return nil, fmt.Errorf("intersect participant channels: %w", err)
	}
	log.Printf("[pipeline-personal] Layer 1.5: %d channels after participant intersection", len(userChannels))

	// Layer 2: source constraints
	candidates := ApplySourceConstraints(userChannels, specifiedSources, creatorUID)

	log.Printf("[pipeline-personal] Layer 2: %d candidate channels", len(candidates))

	// Layer 4: message fetching
	var allMessages []Message
	for _, ch := range candidates {
		msgs, err := FetchMessagesFromChannel(ctx, ch.ChannelID, ch.ChannelType, startTS, endTS, imDB, tableCount, creatorUID)
		if err != nil {
			log.Printf("[pipeline-personal] fetch from %s error: %v", ch.ChannelID, err)
			continue
		}
		for i := range msgs {
			msgs[i].SourceName = ch.ChannelName
			msgs[i].ChannelType = ch.ChannelType
		}
		allMessages = append(allMessages, msgs...)
	}
	log.Printf("[pipeline-personal] Layer 4: fetched %d messages", len(allMessages))

	// Layer 4.5: mutual activity filter
	// Only skip filter for DM-only sources (source_type=3)
	// Group sources (source_type=1) still need mutual activity filtering
	onlyDMSources := len(specifiedSources) > 0
	for _, s := range specifiedSources {
		st := 0
		if v, ok := s["source_type"].(int); ok {
			st = v
		} else if v, ok := s["source_type"].(float64); ok {
			st = int(v)
		}
		if st != 3 { // not DM
			onlyDMSources = false
			break
		}
	}
	if onlyDMSources {
		log.Printf("[pipeline-personal] Layer 4.5: skipped (DM-only sources)")
	} else {
		allMessages = FilterByMutualActivity(allMessages, creatorUID, participantUIDs)
		log.Printf("[pipeline-personal] Layer 4.5: %d messages after mutual activity filter", len(allMessages))
	}

	return allMessages, nil
}
