package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/gorm"
)

// Package-level configurable limits (set by config.Load at startup).
// These replace the old hardcoded constants.
var (
	// MaxSafetyLimit is the maximum messages per channel before safety truncation.
	// Default: 100000. Set via config.Config.MaxSafetyLimit.
	MaxSafetyLimit = 100000

	// DefaultTimeRangeDays preserves the legacy fallback when no time range is
	// supplied. It is intentionally separate from the selectable upper bound.
	DefaultTimeRangeDays = 31

	// MaxTimeRangeDays is the largest time range accepted by summary entry
	// points and the fetch pipeline.
	MaxTimeRangeDays = 90

	// EnableIntentShortcut controls whether to enable short-circuit detection.
	// Default: true. Set via config.Config.EnableIntentShortcut.
	EnableIntentShortcut = true
)

// ChannelInfo holds discovered channel metadata.
type ChannelInfo struct {
	ChannelID   string `json:"channel_id"`
	ChannelType int    `json:"channel_type"`
	ChannelName string `json:"channel_name"`
	SpaceID     string `json:"space_id,omitempty"`
	PeerUID     string `json:"peer_uid,omitempty"`
	// IsArchived is true only for Archived threads (thread status=2). Groups
	// and DMs are always false. Exposed so callers/agents can tell an archived
	// 子区 apart from an active one; the raw status enum is deliberately not
	// surfaced (a bare int invites the same mis-read that bit channel_type).
	IsArchived bool `json:"is_archived,omitempty"`
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

// channelQuery holds the resolved discovery options for GetUserChannels.
type channelQuery struct {
	selectedThreadIDs []string
	includeArchived   bool
	spaceID           string
}

// ChannelQueryOption configures GetUserChannels discovery.
type ChannelQueryOption func(*channelQuery)

// WithSelectedThreads relaxes the archived-thread filter for the given thread
// channel ids (group_no____short_id): a thread listed here survives discovery
// even when its status is Archived (2). Scopes the relaxation to exactly the
// threads the caller explicitly picked.
func WithSelectedThreads(ids []string) ChannelQueryOption {
	return func(q *channelQuery) { q.selectedThreadIDs = ids }
}

// WithIncludeArchived, when true, discovers ALL Archived threads (status=2)
// the user belongs to — not just explicitly-selected ones. Deleted threads
// (status=3) are still never returned. Default (unset) keeps discovery to
// Active threads only, so auto/background summaries stay archived-free.
func WithIncludeArchived(v bool) ChannelQueryOption {
	return func(q *channelQuery) { q.includeArchived = v }
}

// WithSpaceID constrains channel discovery to one Space. Groups and threads
// are filtered by their parent group's space_id. DMs do not carry a space_id,
// so they are retained only when the peer is an active member of this Space.
func WithSpaceID(spaceID string) ChannelQueryOption {
	return func(q *channelQuery) { q.spaceID = strings.TrimSpace(spaceID) }
}

// GetUserChannels discovers all channels (group + DM + thread) for a user. (Layer 1)
//
// Thread (子区) discovery follows this precedence:
//   - WithIncludeArchived(true): Active + all Archived threads (status IN 1,2).
//   - else WithSelectedThreads(ids): Active + the listed Archived threads only.
//   - else: Active threads only (status=1).
//
// Deleted threads (status=3) are never returned. This keeps auto/background
// summaries (no options) archived-free while letting the caller opt in either
// per-thread (selection) or wholesale (include-archived).
func GetUserChannels(ctx context.Context, uid string, imDB *gorm.DB, opts ...ChannelQueryOption) ([]ChannelInfo, error) {
	if imDB == nil {
		return nil, fmt.Errorf("IM database not available")
	}

	var q channelQuery
	for _, opt := range opts {
		opt(&q)
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
	groupSpaceCond := ""
	groupArgs := []interface{}{uid}
	if q.spaceID != "" {
		groupSpaceCond = " AND g.space_id = ?"
		groupArgs = append(groupArgs, q.spaceID)
	}
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
		  `+groupSpaceCond+`
		ORDER BY g.updated_at DESC
	`, groupArgs...).Scan(&groups).Error
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
	activeSpacePeers := make(map[string]bool)
	if q.spaceID != "" && len(dms) > 0 {
		peerSet := make(map[string]struct{}, len(dms))
		for _, d := range dms {
			normalized := NormalizeDMChannelID(d.ChannelID, uid, model.ChannelTypeDM)
			if peerUID := strings.TrimSpace(getPeerUID(normalized, uid)); peerUID != "" {
				peerSet[peerUID] = struct{}{}
			}
		}
		peers := make([]string, 0, len(peerSet))
		for peerUID := range peerSet {
			peers = append(peers, peerUID)
		}
		if len(peers) > 0 {
			var rows []struct {
				UID string `gorm:"column:uid"`
			}
			if err := imDB.WithContext(ctx).
				Table("space_member").
				Select("uid").
				Where("space_id = ? AND status = 1 AND uid IN ?", q.spaceID, peers).
				Scan(&rows).Error; err != nil {
				return nil, fmt.Errorf("query active DM peers in space %s: %w", q.spaceID, err)
			}
			for _, row := range rows {
				activeSpacePeers[row.UID] = true
			}
		}
	}
	for _, d := range dms {
		normalized := NormalizeDMChannelID(d.ChannelID, uid, model.ChannelTypeDM)
		peerUID := getPeerUID(normalized, uid)
		if q.spaceID != "" && !activeSpacePeers[peerUID] {
			continue
		}
		channels = append(channels, ChannelInfo{
			ChannelID:   normalized,
			ChannelType: model.ChannelTypeDM,
			ChannelName: fmt.Sprintf("私聊-%s", peerUID),
			PeerUID:     peerUID,
		})
	}

	// Thread channels (channelType=5)
	type threadRow struct {
		ChannelID   string `gorm:"column:channel_id"`
		ChannelType int    `gorm:"column:channel_type"`
		ChannelName string `gorm:"column:channel_name"`
		SpaceID     string `gorm:"column:space_id"`
		Status      int    `gorm:"column:status"`
	}
	var threadChannels []threadRow
	// Active threads (status=1) are always discovered. Archived threads
	// (status=2) surface either wholesale (WithIncludeArchived) or per-thread
	// (WithSelectedThreads). Deleted threads (status=3) are never returned
	// (enumerating allowed statuses always excludes it).
	threadStatusCond := "t.status = 1"
	threadArgs := []interface{}{uid}
	switch {
	case q.includeArchived:
		threadStatusCond = "t.status IN (1, 2)"
	case len(q.selectedThreadIDs) > 0:
		threadStatusCond = "(t.status = 1 OR (t.status = 2 AND CONCAT(t.group_no, '____', t.short_id) IN ?))"
		threadArgs = append(threadArgs, q.selectedThreadIDs)
	}
	threadSpaceCond := ""
	if q.spaceID != "" {
		threadSpaceCond = " AND g.space_id = ?"
		threadArgs = append(threadArgs, q.spaceID)
	}
	threadQuery := `
		SELECT CONCAT(t.group_no, '____', t.short_id) AS channel_id,
		       5 AS channel_type,
		       CONCAT(t.name, ' · ', g.name) AS channel_name,
		       COALESCE(g.space_id, '') AS space_id,
		       t.status AS status
		FROM thread t
		INNER JOIN ` + "`group`" + ` g ON g.group_no COLLATE utf8mb4_unicode_ci = t.group_no
		WHERE EXISTS (
			SELECT 1
			FROM group_member gm
			WHERE gm.group_no COLLATE utf8mb4_unicode_ci = t.group_no
			  AND gm.uid = ?
			  AND gm.is_deleted = 0
		)
		  AND ` + threadStatusCond + `
		  ` + threadSpaceCond + `
		  AND g.status = 1
		ORDER BY t.updated_at DESC
	`
	err = imDB.WithContext(ctx).Raw(threadQuery, threadArgs...).Scan(&threadChannels).Error
	if err != nil {
		log.Printf("[pipeline] query thread channels error: %v", err)
	}
	for _, tc := range threadChannels {
		channels = append(channels, ChannelInfo{
			ChannelID:   tc.ChannelID,
			ChannelType: 5,
			ChannelName: tc.ChannelName,
			SpaceID:     tc.SpaceID,
			IsArchived:  tc.Status == 2,
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
		return ""
	}
	if parts[0] == selfUID {
		return parts[1]
	}
	if parts[1] == selfUID {
		return parts[0]
	}
	return ""
}

// NormalizeDMChannelID canonicalises the WuKongIM DM channel_id ("uid_a@uid_b")
// into the WuKongIM storage format: the UID with the larger CRC32 hash comes
// first. For non-DM channels (channelType != model.ChannelTypeDM), returns
// input unchanged. The channel id may be logical (peerUID or peer@self) on
// input.
func NormalizeDMChannelID(channelID string, selfUID string, channelType int) string {
	if channelType != model.ChannelTypeDM {
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
	if crc32.ChecksumIEEE([]byte(a)) > crc32.ChecksumIEEE([]byte(b)) {
		return a + "@" + b
	}
	return b + "@" + a
}

// mapFrontendSourceType maps smart-summary's frontend source_type
// (see model.SourceGroup / SourceThread / SourceDirect) to octo-server's
// WuKongIM channel_type (see model.ChannelTypeDM / ChannelTypeGroup /
// ChannelTypeThread). The two enums intentionally use different numbers
// — see the discussion in each const block in model/model.go and the
// PR that introduced these constants (#181).
func mapFrontendSourceType(frontendType int) int {
	switch frontendType {
	case model.SourceGroup:
		return model.ChannelTypeGroup
	case model.SourceThread:
		return model.ChannelTypeThread
	case model.SourceDirect:
		return model.ChannelTypeDM
	default:
		return frontendType
	}
}

// StorageChannelTypeToSourceType is the reverse mapping of
// mapFrontendSourceType: WuKongIM storage-layer channel_type (as carried by
// fetched Message.ChannelType) back to smart-summary's summary_source.source_type.
// Used by the worker to back-fill summary_source rows from the channels a task
// actually fetched messages from (the "generation-time write-back" fix for
// auto-selected-channel tasks that never got source rows at creation).
//
// Storage layer → Source layer:
//
//	ChannelTypeDM     (1) → SourceDirect (3)
//	ChannelTypeGroup  (2) → SourceGroup  (1)
//	ChannelTypeThread (5) → SourceThread (2)
//
// Returns (0, false) for unrecognized storage values so callers skip rather
// than persist garbage source_type.
func StorageChannelTypeToSourceType(storage int) (int, bool) {
	switch storage {
	case model.ChannelTypeDM:
		return model.SourceDirect, true
	case model.ChannelTypeGroup:
		return model.SourceGroup, true
	case model.ChannelTypeThread:
		return model.SourceThread, true
	default:
		return 0, false
	}
}

// sourceType extracts the integer source_type from a specifiedSources entry,
// handling both int and float64 (JSON-decoded) representations.
func sourceType(s map[string]interface{}) int {
	if st, ok := s["source_type"].(int); ok {
		return st
	}
	if st, ok := s["source_type"].(float64); ok {
		return int(st)
	}
	return 0
}

// selectedThreadChannelIDs returns the channel ids of explicitly-selected thread
// sources (frontend source_type == model.SourceThread). These scope the
// archived-thread relaxation in GetUserChannels so that only threads the user
// actually picked can be archived; auto/background summaries (no explicit
// sources) get an empty slice and never surface archived threads.
func selectedThreadChannelIDs(specifiedSources []map[string]interface{}) []string {
	var ids []string
	for _, s := range specifiedSources {
		if mapFrontendSourceType(sourceType(s)) != model.ChannelTypeThread {
			continue
		}
		if id, ok := s["source_id"].(string); ok && id != "" {
			ids = append(ids, id)
		}
	}
	return ids
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

func filterAvailableSpecifiedSources(userChannels []ChannelInfo, specifiedSources []map[string]interface{}, selfUID string) []map[string]interface{} {
	if len(specifiedSources) == 0 {
		return specifiedSources
	}
	available := make(map[string]struct{}, len(userChannels))
	for _, channel := range userChannels {
		available[channel.ChannelID] = struct{}{}
	}
	filtered := make([]map[string]interface{}, 0, len(specifiedSources))
	for _, source := range specifiedSources {
		id, _ := source["source_id"].(string)
		if strings.TrimSpace(id) == "" {
			continue
		}
		id = NormalizeDMChannelID(id, selfUID, mapFrontendSourceType(sourceType(source)))
		if _, ok := available[id]; ok {
			filtered = append(filtered, source)
		}
	}
	return filtered
}

func validateExplicitSourceCoverage(userChannels []ChannelInfo, specifiedSources []map[string]interface{}, selfUID string) error {
	return validateExplicitSourceCoverageWithMessage(userChannels, specifiedSources, selfUID, "explicit summary source is unavailable in the current space")
}

func validateExplicitParticipantSourceCoverage(userChannels []ChannelInfo, specifiedSources []map[string]interface{}, selfUID string) error {
	return validateExplicitSourceCoverageWithMessage(userChannels, specifiedSources, selfUID, "explicit summary source is not shared by every participant")
}

func validateExplicitSourceCoverageWithMessage(userChannels []ChannelInfo, specifiedSources []map[string]interface{}, selfUID, message string) error {
	if len(specifiedSources) == 0 {
		return nil
	}
	requested := make(map[string]struct{}, len(specifiedSources))
	for _, source := range specifiedSources {
		id, _ := source["source_id"].(string)
		if strings.TrimSpace(id) == "" {
			continue
		}
		requested[NormalizeDMChannelID(id, selfUID, mapFrontendSourceType(sourceType(source)))] = struct{}{}
	}
	available := make(map[string]struct{}, len(userChannels))
	for _, channel := range userChannels {
		available[channel.ChannelID] = struct{}{}
	}
	for id := range requested {
		if _, ok := available[id]; !ok {
			return errors.New(message)
		}
	}
	return nil
}

// Deprecated: use ResolveChannelScope instead.
// NarrowByTopic uses LLM to filter channels relevant to the topic. (Layer 3)
//
// It returns candidates unchanged on four paths (no topic / no candidates / no
// llmFn, an LLM error, unparseable model output, and zero matches). Callers that
// need to tell a real narrowing from one of those pass-throughs must use
// NarrowByTopicReport — the difference is not observable from the result alone,
// and treating a pass-through as a deliberate scope choice is how a transient LLM
// blip persisted the entire candidate list as the run's scope.
func NarrowByTopic(ctx context.Context, topic string, candidates []ChannelInfo, llmFn LLMCallFn) []ChannelInfo {
	out, _ := NarrowByTopicReport(ctx, topic, candidates, llmFn)
	return out
}

// NarrowByTopicReport is NarrowByTopic plus the one fact the result cannot carry:
// whether the topic filter actually ran and selected a subset (narrowed=true), or
// whether the input was passed through untouched (narrowed=false).
func NarrowByTopicReport(ctx context.Context, topic string, candidates []ChannelInfo, llmFn LLMCallFn) (result []ChannelInfo, narrowed bool) {
	if topic == "" || len(candidates) == 0 || llmFn == nil {
		return candidates, false
	}

	topic = sanitizeTopic(topic)

	var lines []string
	for _, c := range candidates {
		lines = append(lines, fmt.Sprintf("- %s: %s", c.ChannelID, c.ChannelName))
	}
	prompt := fmt.Sprintf(
		"用户想总结的主题是:\"%s\"\n\n候选频道列表:\n%s\n\n请从上面列表中选出与主题最相关的频道,返回 JSON 数组(只包含 channel_id):\n[\"id1\", \"id2\", ...]\n只返回 JSON,不要其他内容。",
		topic, strings.Join(lines, "\n"),
	)

	raw, err := llmFn(ctx, prompt)
	if err != nil {
		return candidates, false
	}

	var selectedIDs []string
	if err := json.Unmarshal([]byte(raw), &selectedIDs); err != nil {
		return candidates, false
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
		return candidates, false
	}
	return filtered, true
}

// FetchMessagesFromChannel fetches text messages from a sharded table. (Layer 4)
// selfUID is used to normalize DM channel IDs to the storage format.
// maxPerChannel: <=0 means fetch up to maxSafetyLimit; >0 = fetch latest N.
func FetchMessagesFromChannel(ctx context.Context, channelID string, channelType int, startTS, endTS int64, imDB *gorm.DB, tableCount int, selfUID string, maxPerChannel int) ([]Message, error) {
	msgs, _, err := FetchMessagesFromChannelWithCoverage(ctx, channelID, channelType, startTS, endTS, imDB, tableCount, selfUID, maxPerChannel)
	return msgs, err
}

// FetchCoverage describes how completely a single-channel fetch covered the
// requested window (缺点八: fetch/peek 覆盖不透明). It lets a caller tell "this
// channel had exactly N messages" apart from "we hit the cap and there may be
// more", instead of guessing from a bare count.
type FetchCoverage struct {
	// RequestedMax is the effective per-channel cap actually applied (after the
	// <=0 default and the safety-limit clamp).
	RequestedMax int
	// RowsScanned is how many rows the LIMIT query returned before text
	// extraction (after the probe row, if any, is dropped).
	RowsScanned int
	// Returned is how many text messages were returned (RowsScanned minus rows
	// that carried no extractable text).
	Returned int
	// Truncated reports the cap was hit, i.e. the probe found one more row
	// beyond the cap — older messages in the window exist beyond what was
	// returned. False when the window held exactly RequestedMax or fewer
	// messages.
	Truncated bool
	// FirstTS / LastTS are the actual timestamps of the first/last returned
	// message (ascending). Zero when nothing was returned.
	FirstTS int64
	LastTS  int64
}

// FetchMessagesFromChannelWithCoverage is FetchMessagesFromChannel plus an
// honest coverage report. The message slice is byte-identical to the legacy
// function (which now delegates here), so existing callers are unaffected; only
// callers that want the coverage read the second return value.
func FetchMessagesFromChannelWithCoverage(ctx context.Context, channelID string, channelType int, startTS, endTS int64, imDB *gorm.DB, tableCount int, selfUID string, maxPerChannel int) ([]Message, FetchCoverage, error) {
	if imDB == nil {
		return nil, FetchCoverage{}, fmt.Errorf("IM database not available")
	}
	channelID = NormalizeDMChannelID(channelID, selfUID, channelType)
	table := MessageTable(channelID, tableCount)
	if !isValidMessageTable(table, tableCount) {
		return nil, FetchCoverage{}, fmt.Errorf("invalid table name: %s", table)
	}

	// Use package-level configurable limit
	maxSafetyLimit := MaxSafetyLimit

	effectiveMax := maxPerChannel
	if effectiveMax <= 0 {
		effectiveMax = maxSafetyLimit
	}
	if effectiveMax > maxSafetyLimit {
		log.Printf("[pipeline] WARN: maxPerChannel=%d exceeds safety limit, capping to %d", effectiveMax, maxSafetyLimit)
		effectiveMax = maxSafetyLimit
	}

	type msgRow struct {
		MessageSeq int64  `gorm:"column:message_seq"`
		FromUID    string `gorm:"column:from_uid"`
		ChannelID  string `gorm:"column:channel_id"`
		Timestamp  int64  `gorm:"column:timestamp"`
		Payload    []byte `gorm:"column:payload"`
	}

	var allRows []msgRow

	// Fetch effectiveMax+1 rows so Truncated can tell "exactly effectiveMax
	// messages exist in the window" apart from "the cap cut the window off":
	// with LIMIT effectiveMax alone both return effectiveMax rows and the cap
	// hit was indistinguishable from a fully-covered window (false has_more).
	// The probe row is dropped before building messages, so Returned is capped
	// at effectiveMax exactly as before.
	query := fmt.Sprintf(
		"SELECT message_seq, from_uid, channel_id, `timestamp`, payload FROM `%s` WHERE channel_id = ? AND channel_type = ? AND `timestamp` BETWEEN ? AND ? AND is_deleted = 0 ORDER BY message_seq DESC LIMIT ?",
		table,
	)
	if err := imDB.WithContext(ctx).Raw(query, channelID, channelType, startTS, endTS, effectiveMax+1).Scan(&allRows).Error; err != nil {
		return nil, FetchCoverage{}, fmt.Errorf("fetch messages from %s: %w", table, err)
	}
	truncated := len(allRows) > effectiveMax
	if len(allRows) > effectiveMax {
		allRows = allRows[:effectiveMax]
	}
	for i, j := 0, len(allRows)-1; i < j; i, j = i+1, j-1 {
		allRows[i], allRows[j] = allRows[j], allRows[i]
	}

	var messages []Message
	for _, r := range allRows {
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

	cov := FetchCoverage{
		RequestedMax: effectiveMax,
		RowsScanned:  len(allRows),
		Returned:     len(messages),
		Truncated:    truncated,
	}
	if len(messages) > 0 {
		cov.FirstTS = messages[0].Timestamp
		cov.LastTS = messages[len(messages)-1].Timestamp
	}

	log.Printf("[pipeline-personal] FetchMessagesFromChannel %s: %d rows fetched (maxPerChannel=%d, truncated=%t)",
		channelID, len(messages), maxPerChannel, cov.Truncated)
	return messages, cov, nil
}

func fetchMessagesByBackend(ctx context.Context, backend string, octoClient octoSearchClient, candidates []ChannelInfo, creatorUID string, startTS, endTS int64, imDB *gorm.DB, tableCount int, maxPerChannel int, fetchConcurrency int, octoSearchPollSec int) ([]Message, error) {
	selected := strings.ToLower(strings.TrimSpace(backend))
	if selected == "" {
		selected = "batch"
	}
	switch selected {
	case "batch":
		if octoClient == nil {
			return nil, fmt.Errorf("octo-search client is not configured")
		}
		return fetchViaBatch(ctx, octoClient, candidates, creatorUID, startTS, endTS, fetchConcurrency, time.Duration(octoSearchPollSec)*time.Second)
	case "mysql":
		return fetchViaMySQL(ctx, candidates, creatorUID, startTS, endTS, imDB, tableCount, maxPerChannel, fetchConcurrency)
	default:
		return nil, fmt.Errorf("unsupported message fetch backend %q", backend)
	}
}

func fetchViaMySQL(ctx context.Context, candidates []ChannelInfo, creatorUID string, startTS, endTS int64, imDB *gorm.DB, tableCount int, maxPerChannel int, fetchConcurrency int) ([]Message, error) {
	if imDB == nil {
		return nil, fmt.Errorf("IM database not available")
	}
	normIDs, infoByID := normalizeAndIndexCandidates(candidates, creatorUID)
	if len(normIDs) == 0 {
		return nil, nil
	}
	if fetchConcurrency <= 0 {
		fetchConcurrency = octoSearchDefaultConc
	}

	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		all []Message
		sem = make(chan struct{}, fetchConcurrency)
	)
	for _, id := range normIDs {
		info := infoByID[id]
		wg.Add(1)
		go func(ch ChannelInfo) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()

			msgs, err := FetchMessagesFromChannel(ctx, ch.ChannelID, ch.ChannelType, startTS, endTS, imDB, tableCount, creatorUID, maxPerChannel)
			if err != nil {
				log.Printf("[pipeline-personal] mysql message fetch skipped channel=%s: %v", ch.ChannelID, err)
				return
			}
			for i := range msgs {
				msgs[i].SourceName = ch.ChannelName
				msgs[i].ChannelType = ch.ChannelType
			}
			mu.Lock()
			all = append(all, msgs...)
			mu.Unlock()
		}(info)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].ChannelID != all[j].ChannelID {
			return all[i].ChannelID < all[j].ChannelID
		}
		return all[i].MessageSeq < all[j].MessageSeq
	})
	return all, nil
}

// IntersectParticipantChannels filters channels to only those where both
// the creator and all participants are members. (Layer 1.5)
//
// selectedThreadIDs is threaded into each participant's GetUserChannels call with
// the same scope as Layer 1, so an explicitly-selected archived thread that the
// creator can see is not dropped just because the participant lookup defaulted to
// status=1-only discovery.
func IntersectParticipantChannels(ctx context.Context, creatorChannels []ChannelInfo, participantUIDs []string, imDB *gorm.DB, opts ...ChannelQueryOption) ([]ChannelInfo, error) {
	if len(participantUIDs) == 0 {
		return creatorChannels, nil
	}

	// Start with creator's channel IDs
	intersection := make(map[string]bool, len(creatorChannels))
	for _, ch := range creatorChannels {
		intersection[ch.ChannelID] = true
	}

	// For each participant, get their channels and intersect. The same
	// discovery options (selected threads / include-archived) are forwarded so
	// an archived thread the creator surfaced is not dropped by a status=1-only
	// participant lookup.
	for _, uid := range participantUIDs {
		pChannels, err := GetUserChannels(ctx, uid, imDB, opts...)
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

func applyParticipantChannelScope(ctx context.Context, creatorChannels []ChannelInfo, participantUIDs []string, imDB *gorm.DB, scope *ChannelScopeOptions, opts ...ChannelQueryOption) ([]ChannelInfo, error) {
	if scope != nil && scope.ParticipantSourceUnion {
		return creatorChannels, nil
	}
	return IntersectParticipantChannels(ctx, creatorChannels, participantUIDs, imDB, opts...)
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
//
// Intent recognition is consolidated into a single LLM call that extracts:
// - Time range (Layer 0)
// - Channel scope (Layer 1.7)
// - Target persons (for post-fetch filtering)
//
// Returns messages, intent result (for target person filtering), and error.
func ResolveAndFetchMessagesForPersonal(ctx context.Context, creatorUID string, participantUIDs []string, participantNames []string, specifiedSources []map[string]interface{}, topic string, timeStart, timeEnd time.Time, imDB *gorm.DB, octoClient octoSearchClient, messageFetchBackend string, toolCallFn LLMToolCallFn, llmFn LLMCallFn, tableCount int, maxPerChannel int, fetchConcurrency int, octoSearchPollSec int, channelScopeOpts *ChannelScopeOptions, reportStage func(string)) ([]Message, *IntentResult, error) {
	maxDays := DefaultTimeRangeDays
	if channelScopeOpts != nil && channelScopeOpts.WorkspaceTask {
		maxDays = MaxTimeRangeDays
	}
	if timeEnd.Sub(timeStart) > time.Duration(maxDays)*24*time.Hour {
		return nil, nil, fmt.Errorf("时间范围不能超过 %d 天", maxDays)
	}
	if reportStage != nil {
		reportStage(model.WorkflowStageUnderstandQuestion)
	}

	pipelineStart := time.Now()
	originalStart, originalEnd := timeStart, timeEnd

	// Layer 1: channel discovery (needed before intent recognition for memberMap)
	l1Start := time.Now()
	selectedThreads := selectedThreadChannelIDs(specifiedSources)
	channelQueryOpts := []ChannelQueryOption{WithSelectedThreads(selectedThreads)}
	if channelScopeOpts != nil && strings.TrimSpace(channelScopeOpts.SpaceID) != "" {
		channelQueryOpts = append(channelQueryOpts, WithSpaceID(channelScopeOpts.SpaceID))
	}
	userChannels, err := GetUserChannels(ctx, creatorUID, imDB, channelQueryOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("channel discovery: %w", err)
	}
	log.Printf("[pipeline-personal] Layer 1 (channel discovery) took %dms (%d channels)",
		time.Since(l1Start).Milliseconds(), len(userChannels))
	if channelScopeOpts != nil && channelScopeOpts.ParticipantSourceSubset {
		requestedSourceCount := len(specifiedSources)
		specifiedSources = filterAvailableSpecifiedSources(userChannels, specifiedSources, creatorUID)
		if requestedSourceCount > 0 && len(specifiedSources) == 0 {
			return nil, nil, errors.New("no selected summary source is available to this participant")
		}
		if len(specifiedSources) != requestedSourceCount {
			log.Printf("[pipeline-personal] participant source scope reduced from %d to %d source(s)", requestedSourceCount, len(specifiedSources))
		}
	} else if channelScopeOpts != nil && channelScopeOpts.WorkspaceTask {
		if err := validateExplicitSourceCoverage(userChannels, specifiedSources, creatorUID); err != nil {
			return nil, nil, err
		}
	} else {
		requestedSourceCount := len(specifiedSources)
		specifiedSources = filterAvailableSpecifiedSources(userChannels, specifiedSources, creatorUID)
		if requestedSourceCount > 0 && len(specifiedSources) == 0 {
			return nil, nil, errors.New("explicit summary source is unavailable in the current space")
		}
		if len(specifiedSources) != requestedSourceCount {
			log.Printf("[pipeline-personal] legacy source scope reduced from %d to %d source(s)", requestedSourceCount, len(specifiedSources))
		}
	}

	// Convert the effective specifiedSources to a string slice for shortcut
	// detection. Participant-subset mode may have removed inaccessible sources.
	var sourceIDs []string
	for _, src := range specifiedSources {
		if id, ok := src["source_id"].(string); ok && id != "" {
			sourceIDs = append(sourceIDs, id)
		}
	}

	// Layer 1.5: legacy workflows require every participant to share every
	// channel. Unified-workspace team workflows instead authorise participants
	// against the selected-group member union before task creation, so their
	// explicit creator-authorised sources must remain intact here.
	l15Start := time.Now()
	userChannels, err = applyParticipantChannelScope(ctx, userChannels, participantUIDs, imDB, channelScopeOpts, channelQueryOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("intersect participant channels: %w", err)
	}
	if channelScopeOpts == nil || !channelScopeOpts.ParticipantSourceUnion {
		if channelScopeOpts != nil && channelScopeOpts.WorkspaceTask {
			if err := validateExplicitParticipantSourceCoverage(userChannels, specifiedSources, creatorUID); err != nil {
				return nil, nil, err
			}
		} else {
			requestedSourceCount := len(specifiedSources)
			specifiedSources = filterAvailableSpecifiedSources(userChannels, specifiedSources, creatorUID)
			if requestedSourceCount > 0 && len(specifiedSources) == 0 {
				return nil, nil, errors.New("explicit summary source is not shared by every participant")
			}
			if len(specifiedSources) != requestedSourceCount {
				log.Printf("[pipeline-personal] legacy participant scope reduced from %d to %d source(s)", requestedSourceCount, len(specifiedSources))
			}
		}
	}
	log.Printf("[pipeline-personal] Layer 1.5 (participant scope) took %dms (%d channels)",
		time.Since(l15Start).Milliseconds(), len(userChannels))

	// Build memberMap for intent recognition (before LLM call)
	// When explicit sources are specified, build memberMap only from those sources
	// to avoid resolving target persons from unrelated channels
	channelsForMemberMap := userChannels
	if len(specifiedSources) > 0 {
		channelsForMemberMap = ApplySourceConstraints(userChannels, specifiedSources, creatorUID)
		log.Printf("[pipeline-personal] Building memberMap from %d specified source(s)", len(channelsForMemberMap))
	}
	memberMap, memberMapErr := BuildCandidateMemberMap(ctx, channelsForMemberMap, imDB)
	if memberMapErr != nil {
		log.Printf("[pipeline-personal] WARN: BuildCandidateMemberMap failed: %v (proceeding with empty memberMap)", memberMapErr)
	}

	// === UNIFIED INTENT RECOGNITION (1 LLM call for time + channel + person) ===
	intentStart := time.Now()
	intentResult, intentErr := RecognizeIntentWithShortcut(
		ctx, topic, sourceIDs, originalStart, originalEnd,
		userChannels, memberMap, creatorUID,
		EnableIntentShortcut, toolCallFn,
	)
	if intentErr != nil {
		log.Printf("[pipeline-personal] Intent recognition error: %v", intentErr)
	}
	// Guard against nil intentResult (should not happen but defensive)
	if intentResult == nil {
		intentResult = &IntentResult{Skipped: true, SkipReason: "error"}
	}
	log.Printf("[pipeline-personal] Intent recognition took %dms (skipped=%v)",
		time.Since(intentStart).Milliseconds(), intentResult.Skipped)
	if reportStage != nil {
		reportStage(model.WorkflowStageFindRelevantChats)
	}

	// Apply time range from intent
	if intentResult.TimeRange.Narrowed {
		timeStart = intentResult.TimeRange.Start
		timeEnd = intentResult.TimeRange.End
		log.Printf("[pipeline-personal] Time narrowed to [%s ~ %s]",
			timeStart.Format("01-02 15:04"), timeEnd.Format("01-02 15:04"))
	}

	// Apply channel scope (Layer 1.7) via the full DNF resolver.
	// The unified intent's flat ChannelScope cannot represent ownership filters
	// or multi-rule OR (DNF) cases, so when a channel constraint is present we
	// delegate to ResolveChannelScope, which preserves ownership and OR rules.
	channelScopeEnabled := channelScopeOpts != nil && channelScopeOpts.Enabled
	if channelScopeEnabled && !intentResult.Skipped && intentResult.ChannelScope.HasConstraint && len(specifiedSources) == 0 {
		l17Start := time.Now()
		userChannels = ResolveChannelScope(ctx, topic, userChannels, creatorUID, memberMap, imDB, toolCallFn)
		log.Printf("[pipeline-personal] Layer 1.7 (channel scope) took %dms (%d channels)",
			time.Since(l17Start).Milliseconds(), len(userChannels))
	}

	startTS := timeStart.Unix()
	endTS := timeEnd.Unix()

	// Layer 2: source constraints
	l2Start := time.Now()
	candidates := ApplySourceConstraints(userChannels, specifiedSources, creatorUID)
	log.Printf("[pipeline-personal] Layer 2 (source constraints) took %dms (%d → %d candidates)",
		time.Since(l2Start).Milliseconds(), len(userChannels), len(candidates))

	// Layer 4: fetch message content from the configured backend.
	fetchStart := time.Now()
	allMessages, err := fetchMessagesByBackend(ctx, messageFetchBackend, octoClient, candidates, creatorUID, startTS, endTS, imDB, tableCount, maxPerChannel, fetchConcurrency, octoSearchPollSec)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch messages via %s: %w", messageFetchBackend, err)
	}
	log.Printf("[pipeline-personal] Layer 4 (%s): fetched %d messages from %d candidates in %dms",
		messageFetchBackend, len(allMessages), len(candidates), time.Since(fetchStart).Milliseconds())
	if reportStage != nil {
		reportStage(model.WorkflowStageFilterUsefulContent)
	}

	// Layer 4.5: mutual activity filter
	l45Start := time.Now()
	onlyDMSources := len(specifiedSources) > 0
	for _, s := range specifiedSources {
		if sourceType(s) != 3 { // not DM
			onlyDMSources = false
			break
		}
	}
	if onlyDMSources {
		log.Printf("[pipeline-personal] Layer 4.5: skipped (DM-only sources)")
	} else {
		allMessages = FilterByMutualActivity(allMessages, creatorUID, participantUIDs)
		log.Printf("[pipeline-personal] Layer 4.5 (mutual activity) took %dms (%d messages)",
			time.Since(l45Start).Milliseconds(), len(allMessages))
	}

	// Layer 5: Post-Retrieval Narrow
	allMessages = PostRetrievalNarrow(ctx, allMessages, topic, llmFn)

	log.Printf("[pipeline-personal] Total pipeline took %dms (%d messages final)",
		time.Since(pipelineStart).Milliseconds(), len(allMessages))

	return allMessages, intentResult, nil
}
