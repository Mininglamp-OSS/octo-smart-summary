package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
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
)

type refinePersonalReq struct {
	Feedback     string `json:"feedback"`
	BaseResultID int64  `json:"base_result_id"`
	BaseVersion  int    `json:"base_version"`
}

type personalVersionItem struct {
	VersionID       int64  `json:"result_id"`
	Version         int    `json:"version"`
	OperationType   string `json:"operation_type"`
	OperationNote   string `json:"operation_note"`
	ParentVersionID *int64 `json:"parent_result_id,omitempty"`
	CreatedBy       string `json:"created_by"`
	GeneratedAt     string `json:"generated_at"`
	EditedAt        any    `json:"edited_at"`
}

func (h *PersonalHandler) RefinePersonalSummary(c *gin.Context) {
	if h.llm == nil {
		c.JSON(http.StatusServiceUnavailable, apiResponse{Code: 50001, Message: "refine service is not configured"})
		return
	}
	userID := middleware.GetUserID(c)
	taskID, valid := h.parseTaskID(c)
	if !valid {
		return
	}
	var req refinePersonalReq
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

	task, taskOK := h.requireTaskInSpace(c, taskID)
	if !taskOK {
		return
	}
	if task.SummaryMode != model.ModeByPerson {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40005, Message: "该任务不支持个人调整"})
		return
	}
	if task.Status != model.StatusCompleted {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40005, Message: "仅已完成的任务可调整"})
		return
	}

	var pr model.PersonalResult
	if err := h.db.Where("task_id = ? AND user_id = ?", taskID, userID).First(&pr).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "个人总结不存在"})
		return
	}
	if req.BaseResultID > 0 && req.BaseResultID != pr.ID {
		c.JSON(http.StatusConflict, apiResponse{Code: 40009, Message: "内容已更新，请刷新后重试"})
		return
	}
	if pr.WorkerStatus != model.PersonalStatusCompleted || strings.TrimSpace(pr.Content) == "" {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40005, Message: "个人总结未完成，无法调整"})
		return
	}

	currentVersion, err := currentPersonalVersion(h.db, taskID, userID, pr)
	if err != nil {
		log.Printf("[personal-refine] query current version task=%d user=%s: %v", taskID, userID, err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "internal error"})
		return
	}
	if req.BaseVersion > 0 && req.BaseVersion != currentVersion {
		c.JSON(http.StatusConflict, apiResponse{Code: 40009, Message: "内容已更新，请刷新后重试"})
		return
	}

	llmCtx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()
	newContent, tokens, err := h.llm.Call(llmCtx, []service.ChatMessage{
		{Role: "system", Content: buildRefineSystemPrompt()},
		{Role: "user", Content: fmt.Sprintf("当前总结：\n%s\n\n用户修改意见：\n%s", pr.Content, feedback)},
	}, 0.1)
	if err != nil {
		log.Printf("[personal-refine] llm error task=%d personal_result=%d: %v", taskID, pr.ID, err)
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

	cleanedCitations := service.CleanUnreferencedCitations(newContent, pr.GetCitations())
	tmp := &model.PersonalResult{}
	tmp.SetCitations(cleanedCitations)
	citationsJSON := tmp.CitationsJSON

	now := timezone.Now()
	var newVersion model.PersonalResultVersion
	err = h.db.Transaction(func(tx *gorm.DB) error {
		var latestPR model.PersonalResult
		if err := tx.Where("id = ? AND task_id = ? AND user_id = ?", pr.ID, taskID, userID).First(&latestPR).Error; err != nil {
			return err
		}
		if latestPR.WorkerStatus != model.PersonalStatusCompleted || strings.TrimSpace(latestPR.Content) == "" {
			return service.NewBizError(40005, "个人总结状态已变更", http.StatusBadRequest)
		}
		latestVersion, err := ensurePersonalVersionBaseline(tx, latestPR)
		if err != nil {
			return err
		}
		if req.BaseVersion > 0 && req.BaseVersion != latestVersion.Version {
			return service.NewBizError(40009, "内容已更新，请刷新后重试", http.StatusConflict)
		}

		nextVer, err := service.GetNextPersonalVersion(tx, taskID, userID)
		if err != nil {
			return err
		}
		newVersion = model.PersonalResultVersion{
			TaskID:           taskID,
			ParticipantRefID: latestPR.ParticipantRefID,
			UserID:           userID,
			Content:          newContent,
			CitationsJSON:    citationsJSON,
			MsgCount:         latestPR.MsgCount,
			TotalTokenUsed:   latestPR.TotalTokenUsed + tokens,
			ModelVersion:     h.llm.ModelVersion(),
			Version:          nextVer,
			OperationType:    "refine",
			OperationNote:    feedback,
			ParentVersionID:  &latestVersion.ID,
			CreatedBy:        userID,
			GeneratedAt:      now,
		}
		if err := tx.Create(&newVersion).Error; err != nil {
			return err
		}
		res := tx.Model(&model.PersonalResult{}).
			Where("id = ? AND task_id = ? AND user_id = ?", latestPR.ID, taskID, userID).
			Updates(map[string]interface{}{
				"content":            newContent,
				"citations_json":     citationsJSON,
				"total_token_used":   latestPR.TotalTokenUsed + tokens,
				"model_version":      h.llm.ModelVersion(),
				"current_version_id": newVersion.ID,
				"generated_at":       now,
				"edited_at":          now,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errPersonalResultGone
		}
		return service.PrunePersonalResultVersions(tx, taskID, userID, service.PersonalResultVersionKeepLimit)
	})
	if err != nil {
		if bizError, isBiz := err.(*service.BizError); isBiz {
			bizErr(c, bizError)
			return
		}
		if err == gorm.ErrRecordNotFound || err == errPersonalResultGone {
			c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "个人总结不存在"})
			return
		}
		log.Printf("[personal-refine] transaction error task=%d user=%s: %v", taskID, userID, err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "internal error"})
		return
	}

	ok(c, gin.H{
		"task_id":          taskID,
		"result_id":        pr.ID,
		"version_id":       newVersion.ID,
		"version":          newVersion.Version,
		"content":          newContent,
		"citations":        newVersion.GetCitations(),
		"msg_count":        newVersion.MsgCount,
		"total_token_used": newVersion.TotalTokenUsed,
		"model_version":    newVersion.ModelVersion,
		"operation_type":   newVersion.OperationType,
		"operation_note":   newVersion.OperationNote,
		"parent_result_id": newVersion.ParentVersionID,
		"generated_at":     newVersion.GeneratedAt.Format(time.RFC3339),
	})
}

func (h *PersonalHandler) ListPersonalVersions(c *gin.Context) {
	userID := middleware.GetUserID(c)
	taskID, valid := h.parseTaskID(c)
	if !valid {
		return
	}
	limit := 3
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= service.PersonalResultVersionKeepLimit {
			limit = n
		}
	}
	task, taskOK := h.requireTaskInSpace(c, taskID)
	if !taskOK {
		return
	}
	if task.SummaryMode != model.ModeByPerson {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40005, Message: "该任务不支持个人版本"})
		return
	}
	var participantCount int64
	if err := h.db.Model(&model.SummaryParticipant{}).Where("task_id = ? AND user_id = ?", taskID, userID).Count(&participantCount).Error; err != nil {
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "internal error"})
		return
	}
	if participantCount == 0 {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "你不是该任务的参与者"})
		return
	}

	var rows []model.PersonalResultVersion
	if err := h.db.Where("task_id = ? AND user_id = ?", taskID, userID).Order("version DESC").Order("id DESC").Limit(limit).Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "internal error"})
		return
	}
	items := make([]personalVersionItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, personalVersionItem{
			VersionID:       row.ID,
			Version:         row.Version,
			OperationType:   row.OperationType,
			OperationNote:   row.OperationNote,
			ParentVersionID: row.ParentVersionID,
			CreatedBy:       row.CreatedBy,
			GeneratedAt:     row.GeneratedAt.Format(time.RFC3339),
			EditedAt:        nil,
		})
	}
	ok(c, gin.H{"versions": items, "keep_limit": service.PersonalResultVersionKeepLimit})
}

func (h *PersonalHandler) GetPersonalVersion(c *gin.Context) {
	userID := middleware.GetUserID(c)
	taskID, valid := h.parseTaskID(c)
	if !valid {
		return
	}
	versionID, err := strconv.ParseInt(c.Param("version_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid version id"})
		return
	}
	task, taskOK := h.requireTaskInSpace(c, taskID)
	if !taskOK {
		return
	}
	if task.SummaryMode != model.ModeByPerson {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40005, Message: "该任务不支持个人版本"})
		return
	}
	var row model.PersonalResultVersion
	if err := h.db.Where("id = ? AND task_id = ? AND user_id = ?", versionID, taskID, userID).First(&row).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "版本不存在"})
		return
	}
	ok(c, gin.H{
		"result_id":        row.ID,
		"version_id":       row.ID,
		"version":          row.Version,
		"operation_type":   row.OperationType,
		"operation_note":   row.OperationNote,
		"parent_result_id": row.ParentVersionID,
		"created_by":       row.CreatedBy,
		"generated_at":     row.GeneratedAt.Format(time.RFC3339),
		"edited_at":        nil,
		"content":          row.Content,
		"citations":        row.GetCitations(),
	})
}

func (h *PersonalHandler) RestorePersonalVersion(c *gin.Context) {
	userID := middleware.GetUserID(c)
	taskID, valid := h.parseTaskID(c)
	if !valid {
		return
	}
	versionID, err := strconv.ParseInt(c.Param("version_id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid version id"})
		return
	}
	task, taskOK := h.requireTaskInSpace(c, taskID)
	if !taskOK {
		return
	}
	if task.SummaryMode != model.ModeByPerson {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40005, Message: "该任务不支持个人版本"})
		return
	}
	if task.Status != model.StatusCompleted {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40005, Message: "仅已完成的任务可恢复版本"})
		return
	}
	var source model.PersonalResultVersion
	if err := h.db.Where("id = ? AND task_id = ? AND user_id = ?", versionID, taskID, userID).First(&source).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "版本不存在"})
		return
	}
	var pr model.PersonalResult
	if err := h.db.Where("task_id = ? AND user_id = ?", taskID, userID).First(&pr).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "个人总结不存在"})
		return
	}

	now := timezone.Now()
	err = h.db.Transaction(func(tx *gorm.DB) error {
		if _, err := ensurePersonalVersionBaseline(tx, pr); err != nil {
			return err
		}
		res := tx.Model(&model.PersonalResult{}).
			Where("id = ? AND task_id = ? AND user_id = ?", pr.ID, taskID, userID).
			Updates(map[string]interface{}{
				"content":            source.Content,
				"citations_json":     source.CitationsJSON,
				"msg_count":          source.MsgCount,
				"total_token_used":   source.TotalTokenUsed,
				"model_version":      source.ModelVersion,
				"current_version_id": source.ID,
				"worker_status":      model.PersonalStatusCompleted,
				"workflow_stage":     model.WorkflowStageGenerateSummary,
				"retry_count":        0,
				"error_message":      nil,
				"generated_at":       now,
				"edited_at":          now,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errPersonalResultGone
		}
		return nil
	})
	if err != nil {
		if err == gorm.ErrRecordNotFound || err == errPersonalResultGone {
			c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "个人总结不存在"})
			return
		}
		log.Printf("[personal-restore] transaction error task=%d user=%s version=%d: %v", taskID, userID, versionID, err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "internal error"})
		return
	}
	ok(c, gin.H{"task_id": taskID, "result_id": pr.ID, "version_id": source.ID, "version": source.Version})
}

type regeneratePersonalReq struct {
	Topic string `json:"topic"`
}

func (h *PersonalHandler) RegeneratePersonalSummary(c *gin.Context) {
	userID := middleware.GetUserID(c)
	taskID, valid := h.parseTaskID(c)
	if !valid {
		return
	}
	task, taskOK := h.requireTaskInSpace(c, taskID)
	if !taskOK {
		return
	}
	if task.SummaryMode != model.ModeByPerson {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40005, Message: "该任务不支持个人重新生成"})
		return
	}
	if task.Status != model.StatusCompleted {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40005, Message: "仅已完成的任务可重新生成个人总结"})
		return
	}

	var participant model.SummaryParticipant
	if err := h.db.Where("task_id = ? AND user_id = ?", taskID, userID).First(&participant).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "你不是该任务的参与者"})
		return
	}
	var participantCount int64
	if err := h.db.Model(&model.SummaryParticipant{}).Where("task_id = ?", taskID).Count(&participantCount).Error; err != nil {
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "internal error"})
		return
	}
	if participantCount <= 1 {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40005, Message: "单人任务请使用全部重新生成"})
		return
	}
	var pr model.PersonalResult
	if err := h.db.Where("task_id = ? AND user_id = ?", taskID, userID).First(&pr).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "个人总结不存在"})
		return
	}
	if pr.WorkerStatus == model.PersonalStatusPending || pr.WorkerStatus == model.PersonalStatusProcessing {
		c.JSON(http.StatusConflict, apiResponse{Code: 40005, Message: "个人总结正在生成中"})
		return
	}

	var req regeneratePersonalReq
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid request body"})
		return
	}
	topic := strings.TrimSpace(req.Topic)
	if utf8.RuneCountInString(topic) > 1000 {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40001, Message: "topic 不能超过 1000 字符"})
		return
	}
	newTitle := task.Title
	if topic != "" && topic != task.Title {
		newTitle = topic
	}

	err := h.db.Transaction(func(tx *gorm.DB) error {
		if _, err := ensurePersonalVersionBaseline(tx, pr); err != nil && strings.TrimSpace(pr.Content) != "" {
			return err
		}
		res := tx.Model(&model.PersonalResult{}).
			Where("id = ? AND task_id = ? AND user_id = ? AND worker_status IN ?", pr.ID, taskID, userID, []int{model.PersonalStatusCompleted, model.PersonalStatusFailed}).
			Updates(map[string]interface{}{
				"worker_status":      model.PersonalStatusPending,
				"workflow_stage":     "",
				"retry_count":        0,
				"content":            "",
				"citations_json":     "",
				"msg_count":          0,
				"total_token_used":   0,
				"model_version":      "",
				"current_version_id": nil,
				"error_message":      nil,
				"submitted_at":       nil,
				"submit_source":      model.SubmitSourceNone,
				"generated_at":       nil,
				"edited_at":          nil,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return service.NewBizError(40005, "个人总结正在生成中", http.StatusConflict)
		}
		if newTitle != task.Title {
			if err := tx.Model(&model.SummaryTask{}).Where("id = ?", taskID).Update("title", newTitle).Error; err != nil {
				return err
			}
		}
		if err := tx.Model(&model.SummaryParticipant{}).
			Where("id = ? AND task_id = ?", participant.ID, taskID).
			Updates(map[string]interface{}{
				"status":            model.ParticipantAccepted,
				"worker_started_at": nil,
			}).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		if bizError, isBiz := err.(*service.BizError); isBiz {
			bizErr(c, bizError)
			return
		}
		log.Printf("[personal-regenerate] transaction error task=%d user=%s: %v", taskID, userID, err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "internal error"})
		return
	}

	go h.triggerWorker(model.WorkerTriggerRequest{
		Type:             "personal_regenerate",
		TaskID:           taskID,
		ParticipantRefID: participant.ID,
	})

	ok(c, gin.H{"task_id": taskID, "result_id": pr.ID, "status": model.PersonalStatusPending})
}

func currentPersonalVersion(db *gorm.DB, taskID int64, userID string, pr model.PersonalResult) (int, error) {
	var version int
	if err := db.Model(&model.PersonalResultVersion{}).
		Where("task_id = ? AND user_id = ?", taskID, userID).
		Select("COALESCE(MAX(version), 0)").Scan(&version).Error; err != nil {
		return 0, err
	}
	if version == 0 && strings.TrimSpace(pr.Content) != "" {
		return 1, nil
	}
	return version, nil
}

func ensurePersonalVersionBaseline(tx *gorm.DB, pr model.PersonalResult) (model.PersonalResultVersion, error) {
	var latest model.PersonalResultVersion
	if pr.CurrentVersionID != nil {
		if err := tx.Where("id = ? AND task_id = ? AND user_id = ?", *pr.CurrentVersionID, pr.TaskID, pr.UserID).First(&latest).Error; err == nil {
			return latest, nil
		} else if err != gorm.ErrRecordNotFound {
			return latest, err
		}
	}

	err := tx.Where("task_id = ? AND user_id = ?", pr.TaskID, pr.UserID).
		Order("version DESC").Order("id DESC").First(&latest).Error
	if err == nil {
		if pr.CurrentVersionID == nil {
			if err := tx.Model(&model.PersonalResult{}).Where("id = ? AND current_version_id IS NULL", pr.ID).Update("current_version_id", latest.ID).Error; err != nil {
				return latest, err
			}
		}
		return latest, nil
	}
	if err != gorm.ErrRecordNotFound {
		return latest, err
	}
	generatedAt := timezone.Now()
	if pr.GeneratedAt != nil {
		generatedAt = *pr.GeneratedAt
	} else if !pr.CreatedAt.IsZero() {
		generatedAt = pr.CreatedAt
	}
	latest = model.PersonalResultVersion{
		TaskID:           pr.TaskID,
		ParticipantRefID: pr.ParticipantRefID,
		UserID:           pr.UserID,
		Content:          pr.Content,
		CitationsJSON:    pr.CitationsJSON,
		MsgCount:         pr.MsgCount,
		TotalTokenUsed:   pr.TotalTokenUsed,
		ModelVersion:     pr.ModelVersion,
		Version:          1,
		OperationType:    "generate",
		CreatedBy:        pr.UserID,
		GeneratedAt:      generatedAt,
	}
	if err := tx.Create(&latest).Error; err != nil {
		return latest, err
	}
	if err := tx.Model(&model.PersonalResult{}).Where("id = ? AND current_version_id IS NULL", pr.ID).Update("current_version_id", latest.ID).Error; err != nil {
		return latest, err
	}
	return latest, nil
}
