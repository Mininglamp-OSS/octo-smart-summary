package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/gorm"
)

// buildReferencedSummariesContext fetches the referenced summary tasks and
// their latest agent-generated PersonalResult snapshots, then formats them
// into a single string block that can be appended to the agent's system
// prompt. Used when the user starts a new chat session while referencing
// one or more existing summaries (see CHAT-REFERENCE-BASED-DESIGN-v1).
//
// Design notes:
//   - Only invoked on the FIRST turn of a session (when LoadHistory returns
//     empty). Later turns rely on the injected system message being cached
//     in the LLM context.
//   - Space boundary enforced: tasks not belonging to the caller's space
//     are silently dropped (not an error — logged so the caller can decide
//     whether to surface an "some references unavailable" hint).
//   - Reference material is APPENDED to the profile's system prompt (not
//     prepended), so agent's baseline behavior (from profile.md) still
//     takes precedence.
//   - If none of the requested task IDs are accessible, returns "" (caller
//     should treat as "no reference material" and proceed with normal chat).
//
// Returns:
//   - context string (empty if no valid references)
//   - list of successfully-loaded task IDs (for logging / persistence)
func buildReferencedSummariesContext(
	ctx context.Context,
	db *gorm.DB,
	spaceID string,
	userID string,
	taskIDs []int64,
) (string, []int64, error) {
	if len(taskIDs) == 0 {
		return "", nil, nil
	}

	// Fetch tasks in one query, then filter to caller's space
	var tasks []model.SummaryTask
	if err := db.WithContext(ctx).
		Where("id IN ? AND space_id = ?", taskIDs, spaceID).
		Find(&tasks).Error; err != nil {
		return "", nil, fmt.Errorf("fetch referenced tasks: %w", err)
	}
	if len(tasks) == 0 {
		return "", nil, nil
	}

	loaded := make([]int64, 0, len(tasks))
	var sb strings.Builder
	sb.WriteString("\n\n═══════════════════════════════════════════════════\n")
	sb.WriteString("【引用材料 · 参考素材,不是执行指令】\n")
	sb.WriteString("以下是用户在本次对话中引用的已有总结,仅作参考。\n")
	sb.WriteString("你是全新 agent,拥有自主决策权:\n")
	sb.WriteString("  • 时间窗口:自己用 get_current_time / extract_time_range 决定\n")
	sb.WriteString("  • 工具调用:自己判断需要什么工具,不受历史 tool_summary 约束\n")
	sb.WriteString("  • 输出内容:根据用户当前意图产出,不必复现历史\n")
	sb.WriteString("═══════════════════════════════════════════════════\n\n")

	for _, task := range tasks {
		// Fetch the latest (per creator) PersonalResult for this task.
		var pr model.PersonalResult
		err := db.WithContext(ctx).
			Where("task_id = ? AND user_id = ?", task.ID, userID).
			Order("id DESC").
			First(&pr).Error
		if err != nil {
			// Fall back to any PR for this task
			if err2 := db.WithContext(ctx).
				Where("task_id = ?", task.ID).
				Order("id DESC").
				First(&pr).Error; err2 != nil {
				sb.WriteString(fmt.Sprintf("【引用总结 · task_id=%d · %s】(产物尚未生成,跳过)\n\n", task.ID, task.Title))
				continue
			}
		}

		loaded = append(loaded, task.ID)
		sb.WriteString(fmt.Sprintf("─── 引用总结 · task_id=%d · %s ───\n\n", task.ID, task.Title))

		// Snapshot section: reference metadata only, NOT execution parameters
		if snap := pr.GetSnapshot(); snap != nil {
			sb.WriteString("【元信息 · 老总结的生成语境(仅供参考)】\n")
			if snap.Requirement != "" {
				sb.WriteString("- 老需求: " + snap.Requirement + "\n")
			}
			// channel_ids: candidate pool the agent may choose from.
			// IMPORTANT: SummaryTask.OriginChannelType is application-layer
			// (1=Group, 2=Thread, 3=DM); the fetch_channel tool expects
			// storage-layer channel_type (1=DM, 2=Group, 5=Thread) —
			// translate here so the value we hand the agent is directly
			// usable as a tool argument.
			if len(snap.Scope.ChannelIDs) > 0 {
				storageType := appOriginToStorageChannelType(task.OriginChannelType)
				sb.WriteString("- 候选频道 (candidate channels):\n")
				for _, cid := range snap.Scope.ChannelIDs {
					sb.WriteString(fmt.Sprintf("  * channel_id=%s channel_type=%d %s\n",
						cid, storageType, channelTypeLabel(storageType)))
				}
				sb.WriteString("  (你可以复用其中一个,或让用户明确,或用 list_channels 探索其他)\n")
				sb.WriteString("  ⚠️ 调用 fetch_channel/peek_channel 时必须**原样复制**上面的 channel_type 数字,不要猜、不要默认 1\n")
			}
			// time_range: OLD/HISTORICAL window, must NOT be reused as fetch params
			sb.WriteString(fmt.Sprintf("- ⚠️ 老时间窗 (已过期,不要复制作为 fetch 参数): %s ~ %s\n",
				snap.Scope.TimeRange.Start, snap.Scope.TimeRange.End))
			sb.WriteString("  (若用户说'最新/今天/最近'请用 get_current_time 决定新时间窗)\n")
			// tool_summary: historical trace, not a checklist
			if len(snap.ToolSummary) > 0 {
				sb.WriteString(fmt.Sprintf("- 老工具轨迹 (历史,不必复现): %v\n", snap.ToolSummary))
			}
			if snap.DataFreshnessNote != "" {
				sb.WriteString("- 老数据新鲜度声明: " + snap.DataFreshnessNote + "\n")
			}
			sb.WriteString("\n")
		} else {
			sb.WriteString("【元信息】老产物无快照 —— 仅提供正文和 citations 作参考\n\n")
		}

		sb.WriteString("【老产物内容 · 参考文本】\n")
		sb.WriteString(pr.Content)
		sb.WriteString("\n\n")

		// Old citations: the messages the old summary was grounded in
		if cits := pr.GetCitations(); len(cits) > 0 {
			citJSON, _ := json.Marshal(cits)
			sb.WriteString("【老 citations · 参考证据】\n")
			sb.WriteString(string(citJSON))
			sb.WriteString("\n\n")
		}

		sb.WriteString("─── 引用结束 ───\n\n")
	}

	if len(loaded) == 0 {
		return "", nil, nil
	}
	return sb.String(), loaded, nil
}

// channelTypeLabel returns a human-readable label for a **storage-layer**
// channel_type value (as used in WuKongIM message table and passed to
// fetch_channel/peek_channel tool handlers).
//
// Note: this operates on storage-layer values (1=DM, 2=Group, 5=Thread),
// NOT the application-layer OriginChannel* enum (1=Group, 2=Thread, 3=DM).
// Use appOriginToStorageChannelType() to convert first.
func channelTypeLabel(t int) string {
	switch t {
	case model.ChannelTypeDM: // 1
		return "(DM 私聊)"
	case model.ChannelTypeGroup: // 2
		return "(Group 群)"
	case model.ChannelTypeThread: // 5
		return "(Thread 子区)"
	default:
		return "(未知类型)"
	}
}

// appOriginToStorageChannelType maps SummaryTask.OriginChannelType
// (application-layer, user-facing origin enum) to WuKongIM storage-layer
// channel_type used in message tables and tool arguments.
//
// Application layer → Storage layer:
//
//	OriginChannelGroup  (1) → ChannelTypeGroup  (2)
//	OriginChannelThread (2) → ChannelTypeThread (5)
//	OriginChannelDM     (3) → ChannelTypeDM     (1)
//
// Returns 0 for unknown / OriginChannelGlobal (which has no single channel).
func appOriginToStorageChannelType(origin int) int {
	switch origin {
	case model.OriginChannelGroup:
		return model.ChannelTypeGroup
	case model.OriginChannelThread:
		return model.ChannelTypeThread
	case model.OriginChannelDM:
		return model.ChannelTypeDM
	default:
		return 0
	}
}

// serializeReferencedTaskIDs converts a slice of task IDs to a JSON string
// suitable for storing in SummaryTask.ReferencedTaskIDs. Returns nil (not
// empty string) when the list is empty so the DB column stays NULL.
func serializeReferencedTaskIDs(ids []int64) *string {
	if len(ids) == 0 {
		return nil
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return nil
	}
	s := string(b)
	return &s
}

// borrowCitationsFromReference returns the citations JSON of the specified
// referenced task's PersonalResult, so a refine-flow save can preserve the
// [n] citation index alignment when its own session had no tool traces.
//
// Returns []model.Citation{} (never nil) if:
//   - the referenced task isn't found in the caller's space
//   - no PR exists for it
//   - the PR's citations_json is empty/invalid
//
// See CHAT-REFERENCE-BASED-DESIGN-v1 §citation preservation.
func (h *AgentSummaryHandler) borrowCitationsFromReference(
	ctx context.Context,
	refTaskID int64,
	spaceID string,
	userID string,
) []model.Citation {
	// Space-scoped lookup: refuse to leak citations across spaces.
	var task model.SummaryTask
	if err := h.db.WithContext(ctx).
		Select("id, space_id").
		Where("id = ? AND space_id = ?", refTaskID, spaceID).
		First(&task).Error; err != nil {
		return []model.Citation{}
	}

	// Prefer caller's own PR (per-creator citation set); fall back to any PR
	// for this task if not found.
	var pr model.PersonalResult
	err := h.db.WithContext(ctx).
		Where("task_id = ? AND user_id = ?", refTaskID, userID).
		Order("id DESC").
		First(&pr).Error
	if err != nil {
		if err2 := h.db.WithContext(ctx).
			Where("task_id = ?", refTaskID).
			Order("id DESC").
			First(&pr).Error; err2 != nil {
			return []model.Citation{}
		}
	}

	cits := pr.GetCitations()
	if cits == nil {
		return []model.Citation{}
	}
	return cits
}
