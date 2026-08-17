package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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

// errBotIdempotencyBodyMismatch signals that (space_id, bot_id, idempotency_key)
// matched an existing binding, but the current request's canonical hash does
// not match the one persisted with it. This is HTTP 409 territory: the client
// reused an idempotency key across two different intents, and returning the
// original task would silently discard the new intent. Mirrors the contract
// summary_share_snapshot enforces with the same-key/different-hash case
// (share.go:244). See issue #181 P1-2.
var errBotIdempotencyBodyMismatch = errors.New("bot summary idempotency body mismatch")

// canonicalBotCreateRequestHash returns a deterministic sha256 fingerprint of
// the semantic content of a create request. Fields are drawn from the
// resolved (canonicalized) sources, the persisted origin channel, the title,
// topic, the exact time range, and include_archived — everything the worker
// would actually use. The client's raw --data ordering, whitespace, or key
// order does not affect the hash: only the resolved values do, so a client
// that re-sends the same intent with cosmetic differences replays cleanly.
func canonicalBotCreateRequestHash(req createBotSummaryReq, sources []canonicalBotSource, originID string, originType int) string {
	// Sort a copy of the resolved sources so map iteration order or client
	// ordering never breaks replay determinism.
	sorted := make([]canonicalBotSource, len(sources))
	copy(sorted, sources)
	// Simple string-key sort keeps the dependency footprint small; sources
	// are capped at maxSourceCount so O(n^2) is fine.
	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			ki := fmt.Sprintf("%d:%s", sorted[i].SourceType, sorted[i].SourceID)
			kj := fmt.Sprintf("%d:%s", sorted[j].SourceType, sorted[j].SourceID)
			if ki > kj {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	payload := struct {
		Title           string               `json:"title"`
		Topic           string               `json:"topic"`
		TimeRangeStart  string               `json:"time_range_start"`
		TimeRangeEnd    string               `json:"time_range_end"`
		Sources         []canonicalBotSource `json:"sources"`
		OriginChannelID string               `json:"origin_channel_id"`
		OriginChannelTp int                  `json:"origin_channel_type"`
		IncludeArchived bool                 `json:"include_archived"`
	}{
		Title: strings.TrimSpace(req.Title),
		Topic: strings.TrimSpace(req.Topic),
		// Round the time range to whole minutes so a normal client retry
		// that recomputes a relative window ("summarize the last hour")
		// replays cleanly instead of colliding on nanosecond drift. A
		// genuinely different window (measured in minutes or more) still
		// mismatches. See PR #181 discussion — the previous
		// RFC3339Nano formatting turned every network-timeout retry into
		// a spurious 40009 conflict.
		TimeRangeStart:  req.TimeRange.Start.UTC().Truncate(time.Minute).Format(time.RFC3339),
		TimeRangeEnd:    req.TimeRange.End.UTC().Truncate(time.Minute).Format(time.RFC3339),
		Sources:         sorted,
		OriginChannelID: originID,
		OriginChannelTp: originType,
		IncludeArchived: req.IncludeArchived,
	}
	b, _ := json.Marshal(payload)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

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

	// Compute the canonical request hash once, from the resolved sources +
	// persisted origin channel. This is what will be stored in the binding
	// on first-write and compared against on replay. Reused for the "second
	// call, same key, different body" 409 case.
	originID := ""
	originType := 0
	if req.OriginChannelID != "" {
		originID = canonicalBotSourceID(sourceReq{SourceType: req.OriginChannelType, SourceID: req.OriginChannelID}, ownerUID)
		originType = req.OriginChannelType
	}
	requestHash := canonicalBotCreateRequestHash(req, sources, originID, originType)

	if existing, mismatched, ok, err := h.findBotIdempotentTaskWithHash(spaceID, botUID, idempotencyKey, requestHash); err != nil {
		// A DB failure here must not silently proceed to insert — that
		// path would collide on the unique key and then re-read (with the
		// same failing DB), returning 500 for a request whose replay
		// actually exists. Surface the read failure.
		c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "failed to resolve idempotency binding"})
		return
	} else if ok {
		if mismatched {
			// Same idempotency key, different intent. Do not silently return
			// the original task — surface the collision so the caller can
			// pick a new key. Echo the existing task_id so an accidental
			// mismatched retry is still recoverable from the response.
			c.JSON(http.StatusConflict, apiResponse{
				Code:    40009,
				Message: "idempotency key already used for a different request",
				Data:    gin.H{"existing_task_id": existing.ID},
			})
			return
		}
		c.JSON(http.StatusOK, apiResponse{Code: 0, Message: "ok", Data: botCreateResponse(existing)})
		return
	}

	task, participantID, err := h.persistBotSummary(ownerUID, botUID, spaceID, idempotencyKey, requestHash, req, sources)
	if errors.Is(err, errBotIdempotencyConflict) {
		// Racing writer beat us to the insert. Re-fetch and treat as a replay
		// — but honour the hash check so a concurrent replay that happens to
		// carry a mismatched body still surfaces as 409.
		if existing, mismatched, ok, ferr := h.findBotIdempotentTaskWithHash(spaceID, botUID, idempotencyKey, requestHash); ferr != nil {
			c.JSON(http.StatusInternalServerError, apiResponse{Code: 50000, Message: "failed to resolve idempotency binding"})
			return
		} else if ok {
			if mismatched {
				c.JSON(http.StatusConflict, apiResponse{
					Code:    40009,
					Message: "idempotency key already used for a different request",
					Data:    gin.H{"existing_task_id": existing.ID},
				})
				return
			}
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
			// DM sources need special handling: pipeline.GetUserChannels
			// returns DMs with no space filter (their ChannelInfo.SpaceID is
			// empty), so the channel-side space comparison below cannot
			// authorize them. Instead, verify that the DM peer is a member
			// of the token's space via space_member, mirroring the DM path
			// candidates.go:231 already uses for exactly this data. Without
			// this check a space-scoped bot token can reach the owner's DMs
			// with peers outside that space (issue #181 P1-1).
			if imType == model.ChannelTypeDM {
				// P1-1 (PR #181): use the owner-anchored
				// derivation as the authorization primitive. deriveDMPeer
				// returns "" when neither half of the canonical id is the
				// owner, which is the only correct behaviour for a channel
				// the owner is not a party to. channel.PeerUID (populated
				// by pipeline.getPeerUID, which does NOT re-check that the
				// remaining half is the owner) is used purely as a
				// cross-check: any disagreement rejects rather than picks
				// one.
				peer := deriveDMPeer(canonicalID, ownerUID)
				if peer == "" {
					// Not a DM the owner is a party to — reject as
					// unauthorized rather than probing a third party.
					continue
				}
				if channel.PeerUID != "" && channel.PeerUID != peer {
					// Owner-anchored derivation disagrees with the pipeline
					// value. Refuse to guess which one is correct — this
					// signals an inconsistency in the canonical id or a
					// malformed ChannelInfo, either of which is safer to
					// reject than to authorize.
					continue
				}
				// Self-DM ("note to self", peer == owner) is by construction
				// inside the owner's own space membership set. The peerInSpace
				// probe below finds the owner's own row and passes, so no
				// special-case branch is needed here — see PR #181 P2-4.
				inSpace, ierr := peerInSpace(ctx, h.imDB, spaceID, peer)
				if ierr != nil {
					// Return a static message: apiResponse surfaces
					// this err.Error() to the caller (see task.go:219
					// and safeErrorDetail in agent_chat.go:679). The
					// wrapped detail stays here for logs only.
					log.Printf("[bot-summary-create] DM peer space check failed peer=%s space=%s: %v", peer, spaceID, ierr)
					return nil, 50000, errors.New("failed to resolve DM peer space membership")
				}
				if !inSpace {
					wrongSpace = true
					continue
				}
			} else if channel.SpaceID != spaceID {
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

// deriveDMPeer is a fallback that extracts the peer UID from a canonicalized
// DM channel id of the form "a@b" (WuKongIM's larger-CRC32-first layout).
// Only used when pipeline.GetUserChannels did not populate ChannelInfo.PeerUID
// on the matching row — the normal path prefers channel.PeerUID directly.
// Returns "" for an unparseable id (no @ separator); the self-DM case
// (owner@owner) is handled at the call site so the ambiguity can be resolved
// with knowledge of the owner UID.
func deriveDMPeer(canonicalID, ownerUID string) string {
	parts := strings.SplitN(canonicalID, "@", 2)
	if len(parts) != 2 {
		return ""
	}
	if parts[0] == ownerUID {
		return parts[1]
	}
	if parts[1] == ownerUID {
		return parts[0]
	}
	// Neither half is the owner: either a malformed id or a canonical form
	// this handler does not understand. Let the caller decide.
	return ""
}

// peerInSpace reports whether the given peer uid holds an active space_member
// row for spaceID. Uses a single-row COUNT probe so it is cheap enough to
// run in the source-authorization loop without a batch-friendly query — DM
// sources per request are already capped at maxSourceCount.
func peerInSpace(ctx context.Context, imDB *gorm.DB, spaceID, peerUID string) (bool, error) {
	if imDB == nil || spaceID == "" || peerUID == "" {
		return false, nil
	}
	var count int64
	err := imDB.WithContext(ctx).
		Table("space_member").
		Where("space_id = ? AND uid = ? AND status = 1", spaceID, peerUID).
		Limit(1).
		Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
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

func (h *TaskHandler) persistBotSummary(ownerUID, botUID, spaceID, key, requestHash string, req createBotSummaryReq, sources []canonicalBotSource) (model.SummaryTask, int64, error) {
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
		binding := model.SummaryBotCreateIdempotency{SpaceID: spaceID, BotID: botUID, IdempotencyKey: key, RequestHash: requestHash, TaskID: task.ID, CreatedAt: now}
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

// findBotIdempotentTaskWithHash resolves an existing idempotency binding and
// verifies that the caller's current request hash matches the one persisted
// with it. Return semantics:
//
//   - (task, false, true,  nil):  binding found AND hashes match. Replay.
//   - (task, true,  true,  nil):  binding found but hashes differ. Callers
//     should answer HTTP 409 and include existing.task_id in the payload so
//     a mismatched retry is recoverable (PR #181).
//   - ({},   false, false, nil):  no binding for this (space, bot, key) tuple.
//   - ({},   false, false, err):  DB error other than record-not-found. Callers
//     must fail loudly rather than silently proceeding to insert — mirroring
//     share.go:232-241's distinction between "not found" and "read failed".
func (h *TaskHandler) findBotIdempotentTaskWithHash(spaceID, botID, key, requestHash string) (model.SummaryTask, bool, bool, error) {
	var binding model.SummaryBotCreateIdempotency
	if err := h.db.Where("space_id = ? AND bot_id = ? AND idempotency_key = ?", spaceID, botID, key).First(&binding).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return model.SummaryTask{}, false, false, nil
		}
		return model.SummaryTask{}, false, false, err
	}
	// Load the referenced task so callers can echo task_id in either the
	// replay or the mismatch case. When the binding survives but the task
	// row does not (only possible if a manual DELETE ran), treat as "no
	// replay" and let the caller proceed with a fresh create.
	var task model.SummaryTask
	if err := h.db.Where("id = ? AND space_id = ? AND creator_bot_id = ?", binding.TaskID, spaceID, botID).First(&task).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return model.SummaryTask{}, false, false, nil
		}
		return model.SummaryTask{}, false, false, err
	}
	if binding.RequestHash != requestHash {
		return task, true, true, nil
	}
	return task, false, true, nil
}

func botCreateResponse(task model.SummaryTask) gin.H {
	creatorBotName := ""
	if task.CreatorBotID != "" {
		creatorBotName = service.ResolveUserName(task.CreatorBotID)
	}
	return gin.H{"task_id": task.ID, "task_no": task.TaskNo, "status": task.Status, "trigger_type": task.TriggerType, "creator_id": task.CreatorID, "creator_bot_id": task.CreatorBotID, "creator_bot_name": creatorBotName, "created_at": task.CreatedAt.Format(time.RFC3339)}
}
