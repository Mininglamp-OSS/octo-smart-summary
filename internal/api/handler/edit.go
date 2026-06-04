package handler

import (
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const maxContentBytes = 500 * 1024

type EditHandler struct {
	db *gorm.DB
}

func NewEditHandler(db *gorm.DB) *EditHandler {
	return &EditHandler{db: db}
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

	var participantCount int64
	if err := h.db.Model(&model.SummaryParticipant{}).Where("task_id = ?", taskID).Count(&participantCount).Error; err != nil {
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "internal error"})
		return
	}
	if participantCount > 1 {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40005, Message: "多人任务不支持编辑"})
		return
	}

	var summaryResult model.SummaryResult
	if err := h.db.Where("task_id = ?", taskID).Order("version DESC").First(&summaryResult).Error; err != nil {
		c.JSON(http.StatusNotFound, apiResponse{Code: 40008, Message: "总结结果不存在"})
		return
	}

	if summaryResult.ID != req.BaseResultID {
		c.JSON(http.StatusConflict, apiResponse{Code: 40009, Message: "内容已被重新生成，请刷新后重试"})
		return
	}

	var personalResult model.PersonalResult
	if err := h.db.Where("task_id = ? AND user_id = ?", taskID, task.CreatorID).First(&personalResult).Error; err != nil {
		log.Printf("[edit] PersonalResult not found for task=%d creator=%s: %v", taskID, task.CreatorID, err)
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "internal error"})
		return
	}

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

		if err := tx.Model(&model.PersonalResult{}).
			Where("id = ?", personalResult.ID).
			Updates(map[string]interface{}{
				"content":        req.Content,
				"citations_json": citationsJSON,
				"edited_at":      now,
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
