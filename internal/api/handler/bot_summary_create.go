package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const maxBotIdempotencyKeyLen = 128

var botIdempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)

var errBotIdempotencyConflict = errors.New("bot summary idempotency conflict")

type createBotSummaryReq struct {
	Title             string      `json:"title"`
	Topic             string      `json:"topic"`
	TimeRange         *timeRange  `json:"time_range"`
	Sources           []sourceReq `json:"sources"`
	OriginChannelID   string      `json:"origin_channel_id"`
	OriginChannelType int         `json:"origin_channel_type"`
	IncludeArchived   bool        `json:"include_archived"`
}

type canonicalBotSource struct {
	SourceType int
	SourceID   string
}

// CreateBotSummary creates an owner-only asynchronous summary for a personal
// bot. Authentication supplies owner, bot and space; none are client fields.
func (h *TaskHandler) CreateBotSummary(c *gin.Context) {
	if !botSummaryCreateEnabled() {
		c.JSON(http.StatusServiceUnavailable, apiResponse{Code: 50301, Message: "bot summary creation is disabled"})
		return
	}

	ownerUID := middleware.GetUserID(c)
	spaceID := middleware.GetSpaceID(c)
	botID, _ := c.Get("bot_id")
	botUID, _ := botID.(string)
	if ownerUID == "" || spaceID == "" || botUID == "" {
		c.JSON(http.StatusUnauthorized, apiResponse{Code: 40100, Message: "missing bot auth context"})
		return
	}

	idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if !validBotIdempotencyKey(idempotencyKey) {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40005, Message: "valid Idempotency-Key header is required"})
		return
	}

	var req createBotSummaryReq
	decoder := json.NewDecoder(io.LimitReader(c.Request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40004, Message: "invalid or unsupported request field"})
		return
	}
	if err := ensureJSONEOF(decoder); err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: "request body must contain one JSON object"})
		return
	}
	if err := validateBotSummaryRequest(req); err != nil {
		c.JSON(http.StatusBadRequest, apiResponse{Code: 40001, Message: err.Error()})
		return
	}

	sources, code, err := h.resolveAuthorizedBotSources(c.Request.Context(), ownerUID, spaceID, req.Sources, req.IncludeArchived)
	if err != nil {
		status := http.StatusForbidden
		if code == 50000 {
			status = http.StatusInternalServerError
		}
		c.JSON(status, apiResponse{Code: code, Message: err.Error()})
		return
	}
	if req.OriginChannelID != "" && !canonicalSourcesContain(sources, req.OriginChannelType, req.OriginChannelID, ownerUID) {
		c.JSON(http.StatusForbidden, apiResponse{Code: 40301, Message: "origin channel must be an authorized source"})
		return
	}

	if existing, ok := h.findBotIdempotentTask(spaceID, botUID, idempotencyKey); ok {
		c.JSON(http.StatusOK, apiResponse{Code: 0, Message: "ok", Data: botCreateResponse(existing)})
		return
	}

	task, participantID, err := h.persistBotSummary(ownerUID, botUID, spaceID, idempotencyKey, req, sources)
	if errors.Is(err, errBotIdempotencyConflict) {
		if existing, ok := h.findBotIdempotentTask(spaceID, botUID, idempotencyKey); ok {
			c.JSON(http.StatusOK, apiResponse{Code: 0, Message: "ok", Data: botCreateResponse(existing)})
			return
		}
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "failed to create summary"})
		return
	}

	go h.triggerWorker(model.WorkerTriggerRequest{Type: "personal_summary", TaskID: task.ID, ParticipantRefID: participantID})
	c.JSON(http.StatusCreated, apiResponse{Code: 0, Message: "ok", Data: botCreateResponse(task)})
}

func botSummaryCreateEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("BOT_SUMMARY_CREATE_ENABLED"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func validBotIdempotencyKey(key string) bool {
	return len(key) > 0 && len(key) <= maxBotIdempotencyKeyLen && botIdempotencyKeyPattern.MatchString(key)
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra interface{}
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("extra JSON value")
		}
		return err
	}
	return nil
}

func validateBotSummaryRequest(req createBotSummaryReq) error {
	if len(req.Sources) == 0 {
		return errors.New("sources is required")
	}
	if len(req.Sources) > maxSourceCount {
		return fmt.Errorf("sources cannot exceed %d", maxSourceCount)
	}
	if utf8.RuneCountInString(req.Title) > maxSummaryTopicRunes || utf8.RuneCountInString(req.Topic) > maxSummaryTopicRunes {
		return fmt.Errorf("title and topic cannot exceed %d characters", maxSummaryTopicRunes)
	}
	if req.TimeRange == nil || req.TimeRange.Start.IsZero() || req.TimeRange.End.IsZero() {
		return errors.New("time_range with start and end is required")
	}
	if !req.TimeRange.Start.Before(req.TimeRange.End) {
		return errors.New("time_range.start must be before time_range.end")
	}
	if req.TimeRange.End.Sub(req.TimeRange.Start) > time.Duration(pipeline.DefaultTimeRangeDays)*24*time.Hour {
		return fmt.Errorf("time range cannot exceed %d days", pipeline.DefaultTimeRangeDays)
	}
	if req.OriginChannelID == "" && req.OriginChannelType != 0 {
		return errors.New("origin_channel_id is required when origin_channel_type is set")
	}
	if req.OriginChannelID != "" && !validFrontendSourceType(req.OriginChannelType) {
		return errors.New("origin_channel_type must be 1, 2, or 3")
	}
	for _, source := range req.Sources {
		if strings.TrimSpace(source.SourceID) == "" || !validFrontendSourceType(source.SourceType) {
			return errors.New("each source requires source_id and source_type 1, 2, or 3")
		}
	}
	return nil
}

func validFrontendSourceType(sourceType int) bool { return sourceType >= 1 && sourceType <= 3 }

func frontendToIMChannelType(sourceType int) int {
	switch sourceType {
	case model.SourceGroup:
		return model.ChannelTypeGroup
	case model.SourceThread:
		return model.ChannelTypeThread
	case model.SourceDirect:
		return model.ChannelTypeDM
	default:
		return 0
	}
}

func canonicalBotSourceID(source sourceReq, ownerUID string) string {
	return pipeline.NormalizeDMChannelID(strings.TrimSpace(source.SourceID), ownerUID, frontendToIMChannelType(source.SourceType))
}

func (h *TaskHandler) resolveAuthorizedBotSources(ctx context.Context, ownerUID, spaceID string, requested []sourceReq, includeArchived bool) ([]canonicalBotSource, int, error) {
	var opts []pipeline.ChannelQueryOption
	if includeArchived {
		opts = append(opts, pipeline.WithIncludeArchived(true))
	}
	allowed, err := pipeline.GetUserChannels(ctx, ownerUID, h.imDB, opts...)
	if err != nil {
		return nil, 50000, errors.New("failed to resolve owner channel access")
	}

	result := make([]canonicalBotSource, 0, len(requested))
	seen := make(map[string]struct{}, len(requested))
	for _, source := range requested {
		canonicalID := canonicalBotSourceID(source, ownerUID)
		imType := frontendToIMChannelType(source.SourceType)
		matched, wrongSpace := false, false
		for _, channel := range allowed {
			if channel.ChannelID != canonicalID || channel.ChannelType != imType {
				continue
			}
			if imType != model.ChannelTypeDM && channel.SpaceID != spaceID {
				wrongSpace = true
				continue
			}
			matched = true
			break
		}
		if !matched {
			if wrongSpace {
				return nil, 40302, errors.New("source belongs to a different space")
			}
			return nil, 40301, errors.New("owner does not have access to a requested source")
		}
		key := fmt.Sprintf("%d:%s", source.SourceType, canonicalID)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, canonicalBotSource{SourceType: source.SourceType, SourceID: canonicalID})
	}
	return result, 0, nil
}

func canonicalSourcesContain(sources []canonicalBotSource, sourceType int, sourceID, ownerUID string) bool {
	want := canonicalBotSourceID(sourceReq{SourceType: sourceType, SourceID: sourceID}, ownerUID)
	for _, source := range sources {
		if source.SourceType == sourceType && source.SourceID == want {
			return true
		}
	}
	return false
}

func (h *TaskHandler) persistBotSummary(ownerUID, botUID, spaceID, key string, req createBotSummaryReq, sources []canonicalBotSource) (model.SummaryTask, int64, error) {
	taskNo := service.GenerateTaskNo()
	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = strings.TrimSpace(req.Topic)
	}
	if title == "" {
		title = "总结-" + taskNo[len(taskNo)-8:]
	}
	task := model.SummaryTask{
		TaskNo: taskNo, SpaceID: spaceID, CreatorID: ownerUID, CreatorBotID: botUID,
		Title: title, Topic: req.Topic, SummaryMode: model.ModeByPerson,
		TimeRangeStart: req.TimeRange.Start, TimeRangeEnd: req.TimeRange.End,
		Status: model.StatusPending, TriggerType: model.TriggerBot,
		OriginChannelID:   canonicalBotSourceID(sourceReq{SourceType: req.OriginChannelType, SourceID: req.OriginChannelID}, ownerUID),
		OriginChannelType: req.OriginChannelType,
	}
	var participantID int64
	err := h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&task).Error; err != nil {
			return err
		}
		for _, source := range sources {
			row := model.SummarySource{TaskID: task.ID, SourceType: source.SourceType, SourceID: source.SourceID, SourceName: service.ResolveSourceNameWithType(source.SourceID, source.SourceType, h.imDB)}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
		}
		now := timezone.Now()
		participant := model.SummaryParticipant{TaskID: task.ID, UserID: ownerUID, UserName: service.ResolveUserName(ownerUID), Status: model.ParticipantAccepted, ConfirmedAt: &now}
		if err := tx.Create(&participant).Error; err != nil {
			return err
		}
		personal := model.PersonalResult{TaskID: task.ID, ParticipantRefID: participant.ID, UserID: ownerUID, WorkerStatus: model.PersonalStatusPending, CreatedAt: now, UpdatedAt: now}
		if err := tx.Create(&personal).Error; err != nil {
			return err
		}
		if err := tx.Model(&participant).Update("personal_result_id", personal.ID).Error; err != nil {
			return err
		}
		participantID = participant.ID
		binding := model.SummaryBotCreateIdempotency{SpaceID: spaceID, BotID: botUID, IdempotencyKey: key, TaskID: task.ID, CreatedAt: now}
		insert := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&binding)
		if insert.Error != nil {
			return insert.Error
		}
		if insert.RowsAffected == 0 {
			return errBotIdempotencyConflict
		}
		return nil
	})
	return task, participantID, err
}

func (h *TaskHandler) findBotIdempotentTask(spaceID, botID, key string) (model.SummaryTask, bool) {
	var binding model.SummaryBotCreateIdempotency
	if err := h.db.Where("space_id = ? AND bot_id = ? AND idempotency_key = ?", spaceID, botID, key).First(&binding).Error; err != nil {
		return model.SummaryTask{}, false
	}
	var task model.SummaryTask
	if err := h.db.Where("id = ? AND space_id = ? AND creator_bot_id = ?", binding.TaskID, spaceID, botID).First(&task).Error; err != nil {
		return model.SummaryTask{}, false
	}
	return task, true
}

func botCreateResponse(task model.SummaryTask) gin.H {
	return gin.H{"task_id": task.ID, "task_no": task.TaskNo, "status": task.Status, "trigger_type": task.TriggerType, "creator_id": task.CreatorID, "creator_bot_id": task.CreatorBotID, "created_at": task.CreatedAt.Format(time.RFC3339)}
}
