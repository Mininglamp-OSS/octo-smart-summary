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
