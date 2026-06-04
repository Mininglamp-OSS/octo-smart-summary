package service

import (
	"fmt"
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
		// Thread source_id format: {group_no}____{short_id}
		// Note: For thread, ____ separates group_no and short_id; both are used
		parts := strings.SplitN(sourceID, "____", 2)
		if len(parts) == 2 {
			groupNo := parts[0]
			shortID := parts[1]
			
			var groupName string
			_ = imDB.Table("group").Where("group_no = ?", groupNo).Pluck("name", &groupName).Error
			
			var threadName string
			_ = imDB.Table("thread").Where("short_id = ?", shortID).Pluck("name", &threadName).Error
			
			if groupName != "" && threadName != "" {
				return groupName + "-" + threadName + "(子区)"
			}
			if threadName != "" {
				return threadName + "(子区)"
			}
			if groupName != "" {
				return groupName + "(子区)"
			}
		}
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
