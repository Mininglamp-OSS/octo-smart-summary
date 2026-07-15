package handler

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const maxContentBytes = 500 * 1024
const maxFeedbackRunes = 2000

type EditHandler struct {
	db  *gorm.DB
	llm *service.LLMClient
}

func NewEditHandler(db *gorm.DB, llm ...*service.LLMClient) *EditHandler {
	h := &EditHandler{db: db}
	if len(llm) > 0 {
		h.llm = llm[0]
	}
	return h
}

type editSummaryReq struct {
	Content      string `json:"content"`
	BaseResultID int64  `json:"base_result_id"`
}

func (h *EditHandler) EditSummary(c *gin.Context) {
	userID := middleware.GetUserID(c)
	taskID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid task id"})
		return
	}

	var req editSummaryReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid request body"})
		return
	}

	if strings.TrimSpace(req.Content) == "" {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40010, Message: "content cannot be empty"})
		return
	}
	if len(req.Content) > maxContentBytes {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40010, Message: "content 超过 500KB 限制"})
		return
	}

	spaceID := middleware.GetSpaceID(c)

	var task model.SummaryTask
	if err := h.db.Where("id = ? AND space_id = ? AND deleted_at IS NULL", taskID, spaceID).First(&task).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "任务不存在"})
		return
	}

	if task.CreatorID != userID {
		c.JSON(http.StatusForbidden, apiResponse{Code: 40003, Message: "仅创建者可编辑"})
		return
	}

	if task.Status != model.StatusCompleted {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40005, Message: "仅已完成的任务可编辑"})
		return
	}

	// need4: multi-person tasks now allow the CREATOR to edit the *team*
	// SummaryResult. The old participantCount>1 -> 400 rejection is removed; the
	// creator-only + Completed gates above remain the sole authorization. Editing
	// the team draft for a multi-person task does NOT write back into each member's
	// PersonalResult (R4: team draft and personal contents are intentionally
	// allowed to diverge once the creator hand-edits the team summary).
	summaryResult, err := queryDisplayResult(h.db, taskID)
	if err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "总结结果不存在"})
		return
	}

	if summaryResult.ID != req.BaseResultID {
		c.JSON(http.StatusConflict, apiResponse{Code: 40009, Message: "内容已被重新生成，请刷新后重试"})
		return
	}

	// In a single-person task the creator's PersonalResult mirrors the team
	// SummaryResult and is kept in sync. In a multi-person task the creator may
	// not even have a PersonalResult (creator-only edits the *team* draft); a
	// missing row must NOT 500 -- we simply skip the personal write-back.
	// F1: only a SINGLE-person task mirrors the team edit back into the creator's
	// PersonalResult. In a multi-person task the creator is usually also a
	// participant WITH their own PersonalResult; mirroring the team draft into it
	// would clobber the creator's personal summary. So compute participantCount and
	// only mirror when <=1 (and a row actually exists). Multi-person team edits
	// update the team SummaryResult ONLY -- never any PersonalResult (R4).
	var participantCount int64
	if err := h.db.Model(&model.SummaryParticipant{}).Where("task_id = ?", taskID).Count(&participantCount).Error; err != nil {
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "internal error"})
		return
	}
	var personalResult model.PersonalResult
	hasPersonal := participantCount <= 1 &&
		h.db.Where("task_id = ? AND user_id = ?", taskID, task.CreatorID).First(&personalResult).Error == nil

	if req.Content == summaryResult.Content {
		var editedAt interface{}
		if summaryResult.EditedAt != nil {
			editedAt = summaryResult.EditedAt.Format(time.RFC3339)
		}
		ok(c, gin.H{"edited_at": editedAt})
		return
	}

	citations := summaryResult.GetCitations()
	cleanedCitations := service.CleanUnreferencedCitations(req.Content, citations)
	var citationsJSON string
	tempResult := &model.SummaryResult{}
	tempResult.SetCitations(cleanedCitations)
	citationsJSON = tempResult.CitationsJSON

	now := timezone.Now()

	err = h.db.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&model.SummaryResult{}).
			Where("id = ?", req.BaseResultID).
			Updates(map[string]interface{}{
				"content":        req.Content,
				"citations_json": citationsJSON,
				"edited_at":      now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}

		var taskCheck model.SummaryTask
		if err := tx.Where("id = ?", taskID).First(&taskCheck).Error; err != nil {
			return err
		}
		if taskCheck.Status != model.StatusCompleted {
			return service.NewBizError(40005, "任务状态已变更", http.StatusBadRequest)
		}

		// F1: mirror into the creator's PersonalResult ONLY for a single-person task
		// (hasPersonal already encodes participantCount<=1 && row exists). Multi-person
		// team edits touch the team SummaryResult only, never a PersonalResult (R4).
		if hasPersonal {
			if err := tx.Model(&model.PersonalResult{}).
				Where("id = ?", personalResult.ID).
				Updates(map[string]interface{}{
					"content":        req.Content,
					"citations_json": citationsJSON,
					"edited_at":      now,
				}).Error; err != nil {
				return err
			}
		}

		if taskCheck.ScheduleID != nil {
			pauseResult := tx.Model(&model.SummarySchedule{}).
				Where("id = ? AND deleted_at IS NULL AND is_active = 1", *taskCheck.ScheduleID).
				Update("is_active", 0)
			if pauseResult.Error != nil {
				return pauseResult.Error
			}
			if pauseResult.RowsAffected > 0 {
				log.Printf("[edit] EditSummary: task %d edit auto-paused schedule %d", taskID, *taskCheck.ScheduleID)
			}
		}

		return nil
	})

	if err != nil {
		if bizError, isBiz := err.(*service.BizError); isBiz {
			bizErr(c, bizError)
			return
		}
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusConflict, apiResponse{Code: 40009, Message: "内容已被重新生成，请刷新后重试"})
			return
		}
		log.Printf("[edit] transaction error task=%d: %v", taskID, err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "internal error"})
		return
	}

	ok(c, gin.H{"edited_at": now.Format(time.RFC3339)})
}

type refineSummaryReq struct {
	Feedback     string `json:"feedback"`
	BaseResultID int64  `json:"base_result_id"`
}

type summaryVersionItem struct {
	ResultID       int64  `json:"result_id"`
	Version        int    `json:"version"`
	OperationType  string `json:"operation_type"`
	OperationNote  string `json:"operation_note"`
	ParentResultID *int64 `json:"parent_result_id,omitempty"`
	CreatedBy      string `json:"created_by"`
	GeneratedAt    string `json:"generated_at"`
	EditedAt       any    `json:"edited_at"`
}

func (h *EditHandler) RefineSummary(c *gin.Context) {
	if h.llm == nil {
		c.JSON(http.StatusServiceUnavailable, apiResponse{Code: 50001, Message: "refine service is not configured"})
		return
	}
	userID := middleware.GetUserID(c)
	taskID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid task id"})
		return
	}
	var req refineSummaryReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid request body"})
		return
	}
	feedback := strings.TrimSpace(req.Feedback)
	if feedback == "" {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40010, Message: "feedback cannot be empty"})
		return
	}
	if utf8.RuneCountInString(feedback) > maxFeedbackRunes {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40010, Message: "feedback 不能超过 2000 字符"})
		return
	}

	spaceID := middleware.GetSpaceID(c)
	var task model.SummaryTask
	if err := h.db.Where("id = ? AND space_id = ? AND deleted_at IS NULL", taskID, spaceID).First(&task).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "任务不存在"})
		return
	}
	if task.CreatorID != userID {
		c.JSON(http.StatusForbidden, apiResponse{Code: 40003, Message: "仅创建者可调整"})
		return
	}
	if task.Status != model.StatusCompleted {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40005, Message: "仅已完成的任务可调整"})
		return
	}

	baseResult, err := queryDisplayResult(h.db, taskID)
	if err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "总结结果不存在"})
		return
	}
	if baseResult.ID != req.BaseResultID {
		c.JSON(http.StatusConflict, apiResponse{Code: 40009, Message: "内容已更新，请刷新后重试"})
		return
	}

	llmCtx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()
	newContent, tokens, err := h.llm.Call(llmCtx, []service.ChatMessage{
		{Role: "system", Content: buildRefineSystemPrompt()},
		{Role: "user", Content: fmt.Sprintf("当前总结：\n%s\n\n用户修改意见：\n%s", baseResult.Content, feedback)},
	}, 0.1)
	if err != nil {
		log.Printf("[refine] llm error task=%d result=%d: %v", taskID, baseResult.ID, err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "调整失败，请稍后重试"})
		return
	}
	newContent = strings.TrimSpace(stripMarkdownFence(newContent))
	if newContent == "" {
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "调整结果为空"})
		return
	}
	if len(newContent) > maxContentBytes {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40010, Message: "调整结果超过 500KB 限制"})
		return
	}

	basePlainCitations := baseResult.GetCitations()
	if !callerPlainCitationsVisible(h.db, &task, userID, &baseResult) {
		basePlainCitations = []model.Citation{}
	}
	cleanedCitations := service.CleanUnreferencedCitations(newContent, basePlainCitations)
	cleanedTeamCitations := cleanUnreferencedTeamCitations(newContent, baseResult.GetTeamCitations())
	newResult := model.SummaryResult{
		TaskID:         taskID,
		Content:        newContent,
		TotalMsgCount:  baseResult.TotalMsgCount,
		TotalTokenUsed: baseResult.TotalTokenUsed + tokens,
		ModelVersion:   h.llm.ModelVersion(),
		OperationType:  "refine",
		OperationNote:  feedback,
		ParentResultID: &baseResult.ID,
		CreatedBy:      userID,
		GeneratedAt:    timezone.Now(),
	}
	newResult.SetCitations(cleanedCitations)
	newResult.SetTeamCitations(cleanedTeamCitations)

	err = h.db.Transaction(func(tx *gorm.DB) error {
		var taskCheck model.SummaryTask
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", taskID).First(&taskCheck).Error; err != nil {
			return err
		}
		latest, err := queryDisplayResult(tx, taskID)
		if err != nil {
			return err
		}
		if latest.ID != baseResult.ID {
			return service.NewBizError(40009, "内容已更新，请刷新后重试", http.StatusConflict)
		}
		if taskCheck.Status != model.StatusCompleted {
			return service.NewBizError(40005, "任务状态已变更", http.StatusBadRequest)
		}
		nextVer, err := service.GetNextVersion(tx, taskID)
		if err != nil {
			return err
		}
		newResult.Version = nextVer
		if err := tx.Create(&newResult).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.SummaryTask{}).Where("id = ?", taskID).Update("current_result_id", newResult.ID).Error; err != nil {
			return err
		}
		if err := service.PruneSummaryResultVersions(tx, taskID, service.SummaryResultVersionKeepLimit); err != nil {
			return err
		}
		return appendBoundScheduleGenerationInstruction(tx, taskCheck, feedback)
	})
	if err != nil {
		if bizError, isBiz := err.(*service.BizError); isBiz {
			bizErr(c, bizError)
			return
		}
		log.Printf("[refine] transaction error task=%d: %v", taskID, err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "internal error"})
		return
	}

	ok(c, gin.H{
		"task_id":          taskID,
		"result_id":        newResult.ID,
		"version":          newResult.Version,
		"content":          newResult.Content,
		"citations":        newResult.GetCitations(),
		"team_citations":   newResult.GetTeamCitations(),
		"total_msg_count":  newResult.TotalMsgCount,
		"total_token_used": newResult.TotalTokenUsed,
		"model_version":    newResult.ModelVersion,
		"operation_type":   newResult.OperationType,
		"operation_note":   newResult.OperationNote,
		"parent_result_id": newResult.ParentResultID,
		"generated_at":     newResult.GeneratedAt.Format(time.RFC3339),
	})
}

func (h *EditHandler) ListSummaryVersions(c *gin.Context) {
	userID := middleware.GetUserID(c)
	taskID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid task id"})
		return
	}
	limit := 3
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= service.SummaryResultVersionKeepLimit {
			limit = n
		}
	}
	spaceID := middleware.GetSpaceID(c)
	var task model.SummaryTask
	if err := h.db.Where("id = ? AND space_id = ? AND deleted_at IS NULL", taskID, spaceID).First(&task).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "任务不存在"})
		return
	}
	if task.CreatorID != userID {
		var participantCount int64
		if err := h.db.Model(&model.SummaryParticipant{}).Where("task_id = ? AND user_id = ?", taskID, userID).Count(&participantCount).Error; err != nil {
			c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "internal error"})
			return
		}
		if participantCount == 0 {
			c.JSON(http.StatusForbidden, apiResponse{Code: 40003, Message: "无权查看版本"})
			return
		}
	}
	var rows []model.SummaryResult
	if err := h.db.Where("task_id = ?", taskID).Order("version DESC").Order("id DESC").Limit(limit).Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "internal error"})
		return
	}
	items := make([]summaryVersionItem, 0, len(rows))
	for _, row := range rows {
		var editedAt any
		if row.EditedAt != nil {
			editedAt = row.EditedAt.Format(time.RFC3339)
		}
		items = append(items, summaryVersionItem{
			ResultID:       row.ID,
			Version:        row.Version,
			OperationType:  row.OperationType,
			OperationNote:  row.OperationNote,
			ParentResultID: row.ParentResultID,
			CreatedBy:      row.CreatedBy,
			GeneratedAt:    row.GeneratedAt.Format(time.RFC3339),
			EditedAt:       editedAt,
		})
	}
	ok(c, gin.H{"versions": items, "keep_limit": service.SummaryResultVersionKeepLimit})
}

func (h *EditHandler) GetSummaryVersion(c *gin.Context) {
	userID := middleware.GetUserID(c)
	taskID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid task id"})
		return
	}
	resultID, err := strconv.ParseInt(c.Param("result_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid result id"})
		return
	}
	spaceID := middleware.GetSpaceID(c)
	var task model.SummaryTask
	if err := h.db.Where("id = ? AND space_id = ? AND deleted_at IS NULL", taskID, spaceID).First(&task).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "任务不存在"})
		return
	}
	if task.CreatorID != userID {
		var participantCount int64
		if err := h.db.Model(&model.SummaryParticipant{}).Where("task_id = ? AND user_id = ?", taskID, userID).Count(&participantCount).Error; err != nil {
			c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "internal error"})
			return
		}
		if participantCount == 0 {
			c.JSON(http.StatusForbidden, apiResponse{Code: 40003, Message: "无权查看版本"})
			return
		}
	}
	var row model.SummaryResult
	if err := h.db.Where("id = ? AND task_id = ?", resultID, taskID).First(&row).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "版本不存在"})
		return
	}
	var editedAt any
	if row.EditedAt != nil {
		editedAt = row.EditedAt.Format(time.RFC3339)
	}
	plainCitations := row.GetCitations()
	if !callerPlainCitationsVisible(h.db, &task, userID, &row) {
		plainCitations = []model.Citation{}
	}
	ok(c, gin.H{
		"result_id":        row.ID,
		"version":          row.Version,
		"operation_type":   row.OperationType,
		"operation_note":   row.OperationNote,
		"parent_result_id": row.ParentResultID,
		"created_by":       row.CreatedBy,
		"generated_at":     row.GeneratedAt.Format(time.RFC3339),
		"edited_at":        editedAt,
		"content":          row.Content,
		"citations":        plainCitations,
		"team_citations":   row.GetTeamCitations(),
	})
}

func (h *EditHandler) RestoreSummaryVersion(c *gin.Context) {
	userID := middleware.GetUserID(c)
	taskID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid task id"})
		return
	}
	resultID, err := strconv.ParseInt(c.Param("result_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid result id"})
		return
	}
	spaceID := middleware.GetSpaceID(c)
	var task model.SummaryTask
	if err := h.db.Where("id = ? AND space_id = ? AND deleted_at IS NULL", taskID, spaceID).First(&task).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "任务不存在"})
		return
	}
	if task.CreatorID != userID {
		c.JSON(http.StatusForbidden, apiResponse{Code: 40003, Message: "仅创建者可恢复版本"})
		return
	}
	if task.Status != model.StatusCompleted {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40005, Message: "仅已完成的任务可恢复版本"})
		return
	}
	var source model.SummaryResult
	if err := h.db.Where("id = ? AND task_id = ?", resultID, taskID).First(&source).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "版本不存在"})
		return
	}
	err = h.db.Transaction(func(tx *gorm.DB) error {
		var taskCheck model.SummaryTask
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", taskID).First(&taskCheck).Error; err != nil {
			return err
		}
		if taskCheck.Status != model.StatusCompleted {
			return service.NewBizError(40005, "任务状态已变更", http.StatusBadRequest)
		}
		res := tx.Model(&model.SummaryTask{}).
			Where("id = ? AND status = ?", taskID, model.StatusCompleted).
			Update("current_result_id", source.ID)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return nil
	})
	if err != nil {
		if bizError, isBiz := err.(*service.BizError); isBiz {
			bizErr(c, bizError)
			return
		}
		if err == gorm.ErrRecordNotFound {
			c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "任务不存在"})
			return
		}
		log.Printf("[restore] transaction error task=%d result=%d: %v", taskID, resultID, err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "internal error"})
		return
	}
	ok(c, gin.H{"task_id": taskID, "result_id": source.ID, "version": source.Version})
}

func cleanUnreferencedTeamCitations(content string, citations []model.TeamCitation) []model.TeamCitation {
	if len(citations) == 0 {
		return []model.TeamCitation{}
	}
	kept := make([]model.TeamCitation, 0, len(citations))
	seen := make(map[int]bool, len(citations))
	for _, citation := range citations {
		if citation.Index <= 0 || seen[citation.Index] {
			continue
		}
		if strings.Contains(content, fmt.Sprintf("[P%d]", citation.Index)) {
			kept = append(kept, citation)
			seen[citation.Index] = true
		}
	}
	if kept == nil {
		return []model.TeamCitation{}
	}
	return kept
}

func buildRefineSystemPrompt() string {
	return `你是专业的工作总结编辑助手。请根据用户的修改意见，对“当前总结”做局部调整。

要求：
- 尽量保留用户没有要求修改的内容、结构和引用编号。
- 不要重新发散总结，不要补充当前总结里没有依据的新事实。
- 如果只是语气、长短、结构调整，应保持事实含义不变。
- 保留 Markdown 格式。
- 只输出修改后的完整总结正文，不要输出解释、前后缀或代码块。`
}

func stripMarkdownFence(s string) string {
	trimmed := strings.TrimSpace(s)
	if !strings.HasPrefix(trimmed, "```") {
		return trimmed
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) >= 2 && strings.HasPrefix(strings.TrimSpace(lines[0]), "```") && strings.HasPrefix(strings.TrimSpace(lines[len(lines)-1]), "```") {
		return strings.Join(lines[1:len(lines)-1], "\n")
	}
	return trimmed
}
