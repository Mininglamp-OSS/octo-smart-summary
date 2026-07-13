package service

import (
	"fmt"
	"log"
	"math/rand"
	"strings"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"gorm.io/gorm"
)

// BizError represents a business logic error.
type BizError struct {
	Code       int    `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"-"`
}

func (e *BizError) Error() string {
	return fmt.Sprintf("[%d] %s", e.Code, e.Message)
}

// NewBizError creates a new BizError.
func NewBizError(code int, message string, httpStatus int) *BizError {
	return &BizError{Code: code, Message: message, HTTPStatus: httpStatus}
}

// GenerateTaskNo creates a unique task number.
func GenerateTaskNo() string {
	now := timezone.Now()
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 8)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return fmt.Sprintf("ST%s%s", now.Format("20060102"), string(b))
}

// ResolveSourceNameWithType returns source name from IM DB with type suffix.
// sourceType: 1=group, 2=thread, 3=DM (private chat)
func ResolveSourceNameWithType(sourceID string, sourceType int, imDB *gorm.DB) string {
	if imDB == nil {
		return fallbackSourceName(sourceID, sourceType)
	}

	switch sourceType {
	case 1: // group
		// Strip space_id suffix if present (e.g., "uuid____spaceid" -> "uuid")
		// Note: For group, ____ separates group_no and space_id; space_id is discarded
		groupNo := sourceID
		if idx := strings.Index(sourceID, "____"); idx > 0 {
			groupNo = sourceID[:idx]
		}
		var name string
		err := imDB.Table("group").Where("group_no = ?", groupNo).Pluck("name", &name).Error
		if err == nil && name != "" {
			return name + "(群聊)"
		}
	case 2: // thread
		// 子区身份是 (group_no, short_id) 联合键；source_id 形如 {group_no}____{short_id}。
		// 解析严格依赖 source_type==2，不靠 "____" 的出现与否猜测是否为子区
		// （群 source_id 也可能含 "____{space_id}" 后缀，见 case 1）。
		parts := strings.SplitN(sourceID, "____", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			// 非法子区 source_id：走确定性兜底，绝不退化成父群名。
			log.Printf("[service] ResolveSourceNameWithType: malformed thread source_id=%q", sourceID)
			return fallbackSourceName(sourceID, sourceType)
		}
		groupNo := parts[0]
		shortID := parts[1]

		// 一条 JOIN 同时取子区名与父群名；group_no 跨表比较必须加 COLLATE，
		// 与 internal/pipeline/fetch.go:141、internal/api/handler/candidates.go:177/190 保持一致。
		var row struct {
			ThreadName string `gorm:"column:thread_name"`
			GroupName  string `gorm:"column:group_name"`
		}
		err := imDB.Raw(
			"SELECT t.name AS thread_name, g.name AS group_name "+
				"FROM thread t "+
				"LEFT JOIN `group` g ON g.group_no COLLATE utf8mb4_unicode_ci = t.group_no "+
				"WHERE t.group_no = ? AND t.short_id = ? "+
				"LIMIT 1",
			groupNo, shortID,
		).Scan(&row).Error
		if err != nil {
			// 不再吞错：打日志便于线上定位；返回确定性可区分兜底（含 short_id）。
			log.Printf("[service] ResolveSourceNameWithType: thread query failed source_id=%q: %v", sourceID, err)
			return threadFallbackName("", shortID)
		}

		threadName := strings.TrimSpace(row.ThreadName)
		groupName := strings.TrimSpace(row.GroupName)

		// 兜底顺序：子区名优先；其次“父群名-占位”；最后纯占位。
		// 关键：未命名子区永远带 short_id 片段，保证同群多子区互不相同。
		if threadName != "" {
			if groupName != "" {
				return groupName + "-" + threadName + "(子区)"
			}
			return threadName + "(子区)"
		}
		return threadFallbackName(groupName, shortID)
	case 3: // DM (private chat)
		var name string
		err := imDB.Table("user").Where("uid = ?", sourceID).Pluck("name", &name).Error
		if err == nil && name != "" {
			return name + "(私聊)"
		}
	}
	return fallbackSourceName(sourceID, sourceType)
}

// fallbackSourceName returns a placeholder source name.
func fallbackSourceName(sourceID string, sourceType int) string {
	suffix := ""
	switch sourceType {
	case 1:
		suffix = "(群聊)"
	case 2:
		suffix = "(子区)"
	case 3:
		suffix = "(私聊)"
	}
	if len(sourceID) > 8 {
		return "来源-" + sourceID[:8] + suffix
	}
	return "来源-" + sourceID + suffix
}

// threadFallbackName 为“未命名 / 未命中”的子区生成确定性、可区分的占位名。
// 永远包含 short_id 的前若干位，确保同一父群下的不同子区不会撞名
// （这正是 #93 的根因：旧逻辑退化成纯父群名导致同群子区全部同名）。
func threadFallbackName(groupName, shortID string) string {
	short := shortID
	if len(short) > 6 {
		short = short[:6]
	}
	if groupName != "" {
		return groupName + "-子区" + short + "(子区)"
	}
	return "子区-" + short + "(子区)"
}

// ResolveSourceName returns source name from IM DB, fallback to placeholder.
// Deprecated: use ResolveSourceNameWithType instead.
func ResolveSourceName(sourceID string) string {
	return resolveSourceNameFn(sourceID)
}

// resolveSourceNameFn can be overridden at init time with an IM DB resolver.
var resolveSourceNameFn = func(sourceID string) string {
	if len(sourceID) > 8 {
		return "来源-" + sourceID[:8]
	}
	return "来源-" + sourceID
}

// SetSourceNameResolver injects an IM DB resolver so group names are fetched from DB.
func SetSourceNameResolver(fn func(sourceID string) string) {
	resolveSourceNameFn = fn
}

// ResolveUserName returns user display name from IM DB, fallback to uid.
func ResolveUserName(uid string) string {
	return resolveUserNameFn(uid)
}

// resolveUserNameFn can be overridden at init time with an IM DB resolver.
var resolveUserNameFn = func(uid string) string {
	return uid
}

// SetUserNameResolver injects an IM DB resolver so user names are fetched from DB.
func SetUserNameResolver(fn func(uid string) string) {
	resolveUserNameFn = fn
}

// GetNextVersion returns the next version number for a task result.
func GetNextVersion(db *gorm.DB, taskID int64) (int, error) {
	var maxVer *int
	err := db.Model(&model.SummaryResult{}).
		Where("task_id = ?", taskID).
		Select("MAX(version)").
		Scan(&maxVer).Error
	if err != nil {
		return 0, err
	}
	if maxVer == nil {
		return 1, nil
	}
	return *maxVer + 1, nil
}

// SplitIntoChunks splits messages into chunks of roughly chunkSize.

const SummaryResultVersionKeepLimit = 5
const PersonalResultVersionKeepLimit = 5

// PruneSummaryResultVersions keeps the newest auto-generated summary result versions.
// Hand-edited rows are retained as user data, and the current display pointer is
// never pruned even when it points to an older restored version.
func PruneSummaryResultVersions(db *gorm.DB, taskID int64, keep int) error {
	if keep <= 0 {
		keep = SummaryResultVersionKeepLimit
	}
	var keepIDs []int64
	if err := db.Model(&model.SummaryResult{}).
		Where("task_id = ?", taskID).
		Order("version DESC").
		Order("id DESC").
		Limit(keep).
		Pluck("id", &keepIDs).Error; err != nil {
		return err
	}
	if len(keepIDs) < keep {
		return nil
	}
	var current struct {
		CurrentResultID *int64
	}
	if err := db.Model(&model.SummaryTask{}).
		Where("id = ?", taskID).
		Select("current_result_id").
		Scan(&current).Error; err != nil {
		return err
	}
	deleteQuery := db.Where("task_id = ? AND id NOT IN ? AND edited_at IS NULL", taskID, keepIDs)
	if current.CurrentResultID != nil {
		deleteQuery = deleteQuery.Where("id <> ?", *current.CurrentResultID)
	}
	return deleteQuery.Delete(&model.SummaryResult{}).Error
}

func SplitIntoChunks(messages []map[string]interface{}, chunkSize int) [][]map[string]interface{} {
	if chunkSize <= 0 {
		chunkSize = 500
	}
	if len(messages) <= chunkSize {
		return [][]map[string]interface{}{messages}
	}
	var chunks [][]map[string]interface{}
	for i := 0; i < len(messages); i += chunkSize {
		end := i + chunkSize
		if end > len(messages) {
			end = len(messages)
		}
		chunks = append(chunks, messages[i:end])
	}
	return chunks
}

// InferScope infers sources and summary_mode from a topic (keyword heuristic fallback).
func InferScope(topic string) map[string]interface{} {
	type source struct {
		SourceType int    `json:"source_type"`
		SourceID   string `json:"source_id"`
		SourceName string `json:"source_name"`
	}

	topicLower := topic
	var sources []source

	switch {
	case containsAny(topicLower, []string{"项目", "project", "进展", "进度"}):
		sources = []source{
			{1, "group_product", "产品团队群"},
			{1, "group_dev", "开发团队群"},
		}
	case containsAny(topicLower, []string{"会议", "meeting", "周会"}):
		sources = []source{
			{1, "group_all", "全员群"},
		}
	case containsAny(topicLower, []string{"技术", "tech", "架构", "代码"}):
		sources = []source{
			{1, "group_dev", "开发团队群"},
			{1, "group_infra", "基础架构群"},
		}
	default:
		sources = []source{
			{1, "group_general", "综合讨论群"},
		}
	}

	return map[string]interface{}{
		"sources":      sources,
		"summary_mode": model.ModeByPerson,
		"inferred":     true,
	}
}

func containsAny(s string, keywords []string) bool {
	for _, kw := range keywords {
		if len(s) >= len(kw) {
			for i := 0; i <= len(s)-len(kw); i++ {
				if s[i:i+len(kw)] == kw {
					return true
				}
			}
		}
	}
	return false
}

func GetNextPersonalVersion(db *gorm.DB, taskID int64, userID string) (int, error) {
	var maxVer *int
	err := db.Model(&model.PersonalResultVersion{}).
		Where("task_id = ? AND user_id = ?", taskID, userID).
		Select("MAX(version)").
		Scan(&maxVer).Error
	if err != nil {
		return 0, err
	}
	if maxVer == nil {
		return 1, nil
	}
	return *maxVer + 1, nil
}

// PrunePersonalResultVersions keeps the newest personal result versions for a user.
// The current_version_id pointer is always retained, including after restoring
// an older version.
func PrunePersonalResultVersions(db *gorm.DB, taskID int64, userID string, keep int) error {
	if keep <= 0 {
		keep = PersonalResultVersionKeepLimit
	}
	var keepIDs []int64
	if err := db.Model(&model.PersonalResultVersion{}).
		Where("task_id = ? AND user_id = ?", taskID, userID).
		Order("version DESC").
		Order("id DESC").
		Limit(keep).
		Pluck("id", &keepIDs).Error; err != nil {
		return err
	}
	if len(keepIDs) < keep {
		return nil
	}
	var current struct {
		CurrentVersionID *int64
	}
	if err := db.Model(&model.PersonalResult{}).
		Where("task_id = ? AND user_id = ?", taskID, userID).
		Select("current_version_id").
		Scan(&current).Error; err != nil {
		return err
	}
	deleteQuery := db.Where("task_id = ? AND user_id = ? AND id NOT IN ?", taskID, userID, keepIDs)
	if current.CurrentVersionID != nil {
		deleteQuery = deleteQuery.Where("id <> ?", *current.CurrentVersionID)
	}
	return deleteQuery.Delete(&model.PersonalResultVersion{}).Error
}
