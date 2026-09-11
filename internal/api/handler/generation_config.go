package handler

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Agent titles are display metadata, not evidence of the original instruction.
// Resolve only the saved instruction or the selected message's owned run.
func generationRequirement(db *gorm.DB, task model.SummaryTask) string {
	if task.GenerationRequirement != nil {
		return strings.TrimSpace(*task.GenerationRequirement)
	}
	if task.TriggerType != model.TriggerAgent {
		return task.EffectiveTopic()
	}
	if task.AgentMessageID == 0 || task.AgentSessionID == "" {
		return ""
	}
	var msg model.AgentMessage
	if err := db.Where("id = ? AND user_id = ? AND session_id = ?", task.AgentMessageID, task.CreatorID, task.AgentSessionID).
		Where("space_id = ? OR space_id = ''", task.SpaceID).First(&msg).Error; err != nil {
		return ""
	}
	return agentMessageRequirement(db, msg, task.CreatorID)
}

func agentMessageRequirement(db *gorm.DB, msg model.AgentMessage, userID string) string {
	if msg.RunID == "" {
		return ""
	}
	var spec model.AgentSummarySpec
	err := db.Table("agent_summary_spec AS spec").Select("spec.*").
		Joins("JOIN agent_summary_run AS run ON run.run_id = spec.run_id").
		Where("run.run_id = ? AND run.user_id = ? AND spec.spec_id = run.spec_id", msg.RunID, userID).
		Take(&spec).Error
	if err != nil {
		return ""
	}
	return strings.TrimSpace(spec.UserRequest)
}

// Only new/replaced source selections require validation here. Saved source
// scope is still re-authorized by the existing Workflow when reading messages.
func (h *TaskHandler) validateRegenerationConfig(c *gin.Context, task model.SummaryTask, req regenerateReq) error {
	limit := maxSummaryTopicRunes
	if task.TriggerType == model.TriggerAgent {
		limit = maxMessageLen
	}
	if utf8.RuneCountInString(strings.TrimSpace(req.Topic)) > limit {
		return service.NewBizError(40001, fmt.Sprintf("topic 不能超过 %d 字符", limit), http.StatusBadRequest)
	}
	if req.TimeRange != nil {
		r := req.TimeRange
		if r.Start.IsZero() || !r.End.After(r.Start) || r.End.Sub(r.Start).Hours() > float64(pipeline.DefaultTimeRangeDays*24) {
			return service.NewBizError(40001, "invalid time range", http.StatusBadRequest)
		}
	}
	if req.Sources != nil {
		if len(*req.Sources) == 0 || len(*req.Sources) > maxSourceCount {
			return service.NewBizError(40001, "请选择总结来源", http.StatusBadRequest)
		}
		channels := make([]summaryWorkspaceChannel, 0, len(*req.Sources))
		for _, src := range *req.Sources {
			kind := ""
			switch src.SourceType {
			case model.SourceGroup:
				kind = "group"
			case model.SourceThread:
				kind = "thread"
			case model.SourceDirect:
				kind = "direct"
			default:
				return service.NewBizError(40001, "invalid source type", http.StatusBadRequest)
			}
			channels = append(channels, summaryWorkspaceChannel{ChatID: src.SourceID, ChatType: kind})
		}
		validator := summaryWorkspaceCoordinator{imDB: h.imDB}
		valid, err := validator.validateSources(c.Request.Context(), task.SpaceID, task.CreatorID, channels)
		if err != nil {
			return service.NewBizError(50000, "source validation unavailable", http.StatusServiceUnavailable)
		}
		if !valid {
			return service.NewBizError(40004, "无权访问所选来源", http.StatusForbidden)
		}
	}
	if task.TriggerType == model.TriggerAgent {
		requirement := strings.TrimSpace(req.Topic)
		if requirement == "" {
			requirement = generationRequirement(h.db, task)
		}
		var sources int64
		if err := h.db.Model(&model.SummarySource{}).Where("task_id = ?", task.ID).Count(&sources).Error; err != nil {
			return err
		}
		if requirement == "" || (req.TimeRange == nil && !task.TimeRangeEnd.After(task.TimeRangeStart)) ||
			(req.Sources == nil && sources == 0) {
			return service.NewBizError(40001, "请补充总结要求、来源和时间范围", http.StatusBadRequest)
		}
	}
	return nil
}

// SaveGenerationConfig is the same configuration transaction used by full
// regeneration, without starting a run or creating/enabling a schedule.
func (h *TaskHandler) SaveGenerationConfig(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid task id"})
		return
	}
	task, allowed := h.authorizeTaskAccess(c, id)
	if !allowed {
		return
	}
	if task.CreatorID != middleware.GetUserID(c) {
		bizErr(c, service.NewBizError(40004, "仅创建者可修改配置", http.StatusForbidden))
		return
	}
	var req regenerateReq
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "invalid request body"})
		return
	}
	err = h.validateRegenerationConfig(c, *task, req)
	if err == nil {
		err = h.db.Transaction(func(tx *gorm.DB) error {
			var locked model.SummaryTask
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&locked, id).Error; err != nil {
				return err
			}
			if locked.DeletedAt != nil || (locked.Status != model.StatusCompleted && locked.Status != model.StatusFailed && locked.Status != model.StatusCancelled) {
				return service.NewBizError(40005, "任务处理中，暂不能修改配置", http.StatusConflict)
			}
			if topic := strings.TrimSpace(req.Topic); topic != "" {
				if err := tx.Model(&locked).Updates(map[string]interface{}{"topic": truncateRunes(topic, maxSummaryTopicRunes), "generation_requirement": topic}).Error; err != nil {
					return err
				}
			}
			return h.saveGenerationScope(tx, locked, req)
		})
	}
	if err != nil {
		var be *service.BizError
		if errors.As(err, &be) {
			bizErr(c, be)
		} else {
			c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "internal error"})
		}
		return
	}
	ok(c, gin.H{"task_id": id})
}

func (h *TaskHandler) saveGenerationScope(tx *gorm.DB, task model.SummaryTask, req regenerateReq) error {
	if req.TimeRange != nil {
		if err := tx.Model(&model.SummaryTask{}).Where("id = ?", task.ID).Updates(map[string]interface{}{
			"time_range_start": req.TimeRange.Start, "time_range_end": req.TimeRange.End,
		}).Error; err != nil {
			return err
		}
	}
	if req.Sources == nil {
		return nil
	}
	if err := tx.Where("task_id = ?", task.ID).Delete(&model.SummarySource{}).Error; err != nil {
		return err
	}
	seen := make(map[string]bool)
	for _, src := range *req.Sources {
		key := fmt.Sprintf("%d:%s", src.SourceType, src.SourceID)
		if seen[key] {
			continue
		}
		seen[key] = true
		row := model.SummarySource{TaskID: task.ID, SourceType: src.SourceType, SourceID: src.SourceID,
			SourceName: service.ResolveSourceNameForActor(src.SourceID, src.SourceType, task.CreatorID, h.imDB)}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
	}
	return nil
}
