package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/citationtext"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// The target is independent of reference materials. Its revision covers in-place
// edits as well as appended/restored versions; clients must capture it BEFORE
// starting optimization, never refresh it silently at save time.
type optimizationTarget struct {
	TaskID           int64  `json:"task_id"`
	ResultKind       string `json:"result_kind"`
	ExpectedRevision string `json:"expected_revision"`
}

type optimizationBase struct {
	Task     model.SummaryTask
	Result   model.SummaryResult
	Personal model.PersonalResult
	Kind     string
}

func (b optimizationBase) revision() string {
	// Hash only the artifact being updated; unrelated team pointer changes
	// must not invalidate a personal revision. Storage-only fields are explicit.
	values := []interface{}{b.Kind, b.Result.ID, b.Result.Version, b.Result.Content, b.Result.CitationOwnerID, b.Result.CitationsJSON, b.Result.TeamCitationsJSON, b.Result.SnapshotJSON, b.Task.CurrentResultID}
	if b.Kind == "personal" {
		values = []interface{}{b.Kind, b.Personal.ID, b.Personal.CurrentVersionID, b.Personal.Content, b.Personal.CitationsJSON, b.Personal.SnapshotJSON}
	}
	data, _ := json.Marshal(values)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func loadOptimizationBase(db *gorm.DB, space, user string, id int64, kind string, lock bool) (optimizationBase, error) {
	var b optimizationBase
	unavailable := service.NewBizError(40008, "summary unavailable", http.StatusNotFound)
	if space == "" {
		return b, unavailable
	}
	q := db.Where("id = ? AND space_id = ? AND deleted_at IS NULL", id, space)
	if lock {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	if err := q.First(&b.Task).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return b, unavailable
		}
		return b, err
	}
	if !canAccessTaskDB(db, user, id, b.Task.CreatorID) {
		return b, unavailable
	}
	if b.Task.Status != model.StatusCompleted {
		return b, service.NewBizError(40009, "summary state changed", http.StatusConflict)
	}
	if kind != "" && kind != "team" && kind != "personal" {
		return b, service.NewBizError(40001, "invalid result_kind", http.StatusBadRequest)
	}
	// Keep the same team-first precedence as the generation reference resolver.
	// Explicit personal targets cannot redirect a shared task reference.
	{
		r, err := queryDisplayResult(db, id)
		if err == nil && r.Content != "" {
			if kind == "personal" {
				return b, service.NewBizError(40009, "reference resolves to team summary; personal optimization unavailable", http.StatusConflict)
			}
			if lock {
				if err := db.Clauses(clause.Locking{Strength: "UPDATE"}).First(&r, r.ID).Error; err != nil {
					return b, err
				}
			}
			b.Result = r
			b.Kind = "team"
			return b, nil
		}
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return b, err
		}
		if kind == "team" {
			return b, unavailable
		}
	}
	q = db.Where("task_id = ? AND user_id = ? AND worker_status = ?", id, user, model.PersonalStatusCompleted)
	if lock {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	if err := q.First(&b.Personal).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return b, unavailable
		}
		return b, err
	}
	if strings.TrimSpace(b.Personal.Content) == "" {
		return b, unavailable
	}
	b.Kind = "personal"
	return b, nil
}

func (h *AgentSummaryHandler) GetOptimizationTarget(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(400, apiResponse{Code: 40000, Message: "invalid task id"})
		return
	}
	b, err := loadOptimizationBase(h.db, middleware.GetSpaceID(c), middleware.GetUserID(c), id, c.Query("result_kind"), false)
	if err != nil {
		writeOptimizationError(c, err)
		return
	}
	ok(c, gin.H{"target": optimizationTarget{id, b.Kind, b.revision()}, "title": b.Task.Title, "can_save_version": b.Kind == "personal" || b.Task.CreatorID == middleware.GetUserID(c)})
}

func writeOptimizationError(c *gin.Context, err error) {
	var business *service.BizError
	if errors.As(err, &business) {
		bizErr(c, business)
		return
	}
	if errors.Is(err, errWorkspacePreviewSaveStale) || errors.Is(err, errAgentMessageRunMismatch) || errors.Is(err, errNoAgentOutput) {
		writeWorkspacePreviewSaveConflict(c)
		return
	}
	if errors.Is(err, errWorkspacePreviewCitationUnresolved) {
		writeWorkspacePreviewCitationConflict(c)
		return
	}
	c.JSON(500, apiResponse{Code: 50000, Message: "failed to save summary version"})
}

func optimizationSaveHash(req createAgentSummaryReq) string {
	// Include all save semantics, including target and save mode, in replay identity.
	payload, _ := json.Marshal(req)
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func (h *AgentSummaryHandler) replayOptimization(c *gin.Context, key, hash string) bool {
	space, user := middleware.GetSpaceID(c), middleware.GetUserID(c)
	var binding model.SummaryAgentSaveIdempotency
	err := h.db.Where("space_id = ? AND user_id = ? AND idempotency_key = ?", space, user, key).First(&binding).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false
	}
	if err != nil {
		writeOptimizationError(c, err)
		return true
	}
	if binding.RequestHash != hash {
		c.JSON(409, apiResponse{Code: 40009, Message: "save request changed"})
		return true
	}
	var task model.SummaryTask
	if err := h.db.Where("id = ? AND space_id = ? AND deleted_at IS NULL", binding.TaskID, space).First(&task).Error; err != nil || !canAccessTaskDB(h.db, user, task.ID, task.CreatorID) {
		c.JSON(409, apiResponse{Code: 40009, Message: "saved summary unavailable"})
		return true
	}
	var count int64
	var resultQuery *gorm.DB
	switch binding.ResultKind {
	case "team":
		resultQuery = h.db.Model(&model.SummaryResult{})
	case "personal":
		resultQuery = h.db.Model(&model.PersonalResultVersion{}).Where("user_id = ?", user)
	default:
		c.JSON(409, apiResponse{Code: 40009, Message: "saved version unavailable"})
		return true
	}
	if err := resultQuery.Where("id = ? AND task_id = ? AND version = ?", binding.ResultID, task.ID, binding.ResultVersion).Count(&count).Error; err != nil {
		writeOptimizationError(c, err)
		return true
	}
	if count != 1 {
		c.JSON(409, apiResponse{Code: 40009, Message: "saved version unavailable"})
		return true
	}
	ok(c, gin.H{"task_id": task.ID, "task_no": task.TaskNo, "status": task.Status, "created_at": task.CreatedAt, "save_mode": "new_version", "version": binding.ResultVersion, "result_id": binding.ResultID, "result_kind": binding.ResultKind, "replayed": true})
	return true
}

func (h *AgentSummaryHandler) saveOptimizationVersion(c *gin.Context, req createAgentSummaryReq) {
	target := req.Target
	key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if target == nil || target.TaskID <= 0 || target.ExpectedRevision == "" || (target.ResultKind != "team" && target.ResultKind != "personal") || !validAgentSaveIdempotencyKey(key) || len(c.Request.Header.Values("Idempotency-Key")) != 1 {
		c.JSON(400, apiResponse{Code: 40001, Message: "target, revision and Idempotency-Key are required"})
		return
	}
	// Version saves always use the authoritative workspace preview contract.
	// Legacy clients retain create-new behavior until they adopt this contract.
	if !isWorkspacePreviewSave(req) {
		c.JSON(400, apiResponse{Code: 40001, Message: "version save requires workspace preview"})
		return
	}
	hash := optimizationSaveHash(req)
	if h.replayOptimization(c, key, hash) {
		return
	}
	space, user := middleware.GetSpaceID(c), middleware.GetUserID(c)
	var saved model.SummaryAgentSaveIdempotency
	var task model.SummaryTask
	err := h.db.Transaction(func(tx *gorm.DB) error {
		candidate, err := loadWorkspacePreviewForSave(tx, space, user, req, true)
		if err != nil {
			return err
		}
		b, err := loadOptimizationBase(tx, space, user, target.TaskID, target.ResultKind, true)
		if err != nil {
			return err
		}
		task = b.Task
		if b.Kind == "team" && task.CreatorID != user {
			return service.NewBizError(40003, "only creator may update team summary", http.StatusForbidden)
		}
		if b.revision() != target.ExpectedRevision {
			return service.NewBizError(40009, "summary changed; draft preserved", http.StatusConflict)
		}
		// Require the target among the server-owned reference materials. Client input
		// alone cannot turn an unrelated draft into an update of an arbitrary task.
		referenced := false
		for _, id := range candidate.Scope.ReferencedTaskIDs {
			if id == task.ID {
				referenced = true
			}
		}
		if !referenced {
			return service.NewBizError(40009, "optimization reference changed; save as new summary", http.StatusConflict)
		}
		// The reference resolver currently prefers team results. Do not write a team
		// artifact into a personal result when both exist.
		artifact, err := resolveReferencedArtifact(c.Request.Context(), tx, task.ID, space, user)
		if err != nil {
			return err
		}
		if (b.Kind == "personal" && artifact.Type != "personal_result") || (b.Kind == "team" && artifact.Type != "team_result") {
			return service.NewBizError(40009, "optimization target differs from reference", http.StatusConflict)
		}
		binding, err := resolveAgentMessageRunBinding(c.Request.Context(), tx, user, req.SessionID, req.RequestID, candidate.Message)
		if err != nil {
			return err
		}
		content := citationtext.NormalizeSetextHeadings(candidate.Content)
		cits, fallback, err := h.buildWorkspacePreviewCitations(c.Request.Context(), tx, user, req.SessionID, content, candidate, binding)
		if err != nil {
			return err
		}
		if len(cits) == 0 {
			cits = artifact.Citations
			fallback = false
		}
		content, err = service.NormalizeGeneratedCitations(content, cits)
		if err != nil {
			return errWorkspacePreviewCitationUnresolved
		}
		if (fallback && contentHasCitationSequence(content)) || (contentHasCitationMarker(content) && !citationsValid(content, cits, true)) {
			return errWorkspacePreviewCitationUnresolved
		}
		if strings.TrimSpace(content) == "" || len(content) > maxContentBytes {
			return service.NewBizError(40010, "invalid preview content", http.StatusBadRequest)
		}
		cits = service.CleanUnreferencedCitations(content, cits)
		now := timezone.Now()
		snapshotTask := task
		if candidate.Scope.TimeRange != nil {
			window, err := workspaceAgentSaveTimeRange(candidate.Scope, now)
			if err != nil {
				return err
			}
			snapshotTask.TimeRangeStart, snapshotTask.TimeRangeEnd = window.Start, window.End
		}
		effective := req
		applyWorkspaceScopeToSaveRequest(&effective, candidate)
		snapshot := h.buildSnapshotV1(tx, req.SessionID, user, &snapshotTask, effective.Sources, space)
		if len(effective.Sources) == 0 {
			if artifact.Snapshot != nil {
				snapshot.Scope.ChannelIDs = artifact.Snapshot.Scope.ChannelIDs
				snapshot.Scope.ChannelNames = artifact.Snapshot.Scope.ChannelNames
			} else {
				for _, source := range artifact.Sources {
					snapshot.Scope.ChannelIDs = append(snapshot.Scope.ChannelIDs, source.SourceID)
					snapshot.Scope.ChannelNames = append(snapshot.Scope.ChannelNames, source.SourceName)
				}
			}
		}
		if len(effective.Sources) > 0 {
			for _, source := range effective.Sources {
				snapshot.Scope.Sources = append(snapshot.Scope.Sources, model.SnapshotSource{SourceID: source.SourceID, SourceType: source.SourceType, SourceName: service.ResolveSourceNameForActor(source.SourceID, source.SourceType, user, h.imDB)})
			}
		} else if artifact.Snapshot != nil {
			snapshot.Scope.Sources = artifact.Snapshot.Scope.Sources
		} else {
			for _, source := range artifact.Sources {
				snapshot.Scope.Sources = append(snapshot.Scope.Sources, model.SnapshotSource{SourceID: source.SourceID, SourceType: source.SourceType, SourceName: source.SourceName})
			}
		}
		if candidate.Scope.TimeRange == nil && artifact.Snapshot != nil {
			snapshot.Scope.TimeRange = artifact.Snapshot.Scope.TimeRange
		}
		// Preserve authorized new selections in the stored version. Reader-specific
		// filtering happens on reads; Derived provenance stays masked as before.
		snapshot = projectResultSnapshotValue(tx, h.imDB, task, snapshot, true)
		if snapshot == nil {
			return errors.New("failed to project snapshot sources")
		}
		if artifact.Snapshot != nil {
			parent := artifact.Snapshot.ContentVersion
			snapshot.ParentSnapshotVersion = &parent
		}
		snapshot.DataFreshnessNote = "本版本由优化会话生成；tool_summary 仅记录该会话可用的工具调用，不代表已重新抓取全部来源。"
		instruction := agentMessageRequirement(tx, candidate.Message, user)
		if strings.TrimSpace(instruction) != "" {
			snapshot.UserInstruction = &instruction
		}
		snapshotJSON := func(version int) *string {
			snapshot.ContentVersion = version
			data, _ := json.Marshal(snapshot)
			raw := string(data)
			return &raw
		}

		saved = model.SummaryAgentSaveIdempotency{SpaceID: space, UserID: user, IdempotencyKey: key, RequestHash: hash, TaskID: task.ID, ResultKind: b.Kind, CreatedAt: now}
		if b.Kind == "team" {
			version, err := service.GetNextVersion(tx, task.ID)
			if err != nil {
				return err
			}
			result := model.SummaryResult{TaskID: task.ID, Content: content, Version: version, OperationType: "refine", ParentResultID: &b.Result.ID, CreatedBy: user, GeneratedAt: now, TotalMsgCount: b.Result.TotalMsgCount, TotalTokenUsed: b.Result.TotalTokenUsed, ModelVersion: b.Result.ModelVersion}
			result.SnapshotJSON = snapshotJSON(version)
			result.CitationOwnerID = user
			result.OperationNote = instruction
			result.SetCitations(cits)
			result.SetTeamCitations(cleanUnreferencedTeamCitations(content, artifact.TeamCitations))
			if err := tx.Create(&result).Error; err != nil {
				return err
			}
			if err := tx.Model(&model.SummaryTask{}).Where("id = ?", task.ID).Update("current_result_id", result.ID).Error; err != nil {
				return err
			}
			saved.ResultID = result.ID
			saved.ResultVersion = version
			var count int64
			if err := tx.Model(&model.SummaryParticipant{}).Where("task_id = ?", task.ID).Count(&count).Error; err != nil {
				return err
			}
			if count <= 1 {
				var personal model.PersonalResult
				err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("task_id = ? AND user_id = ? AND worker_status = ?", task.ID, user, model.PersonalStatusCompleted).First(&personal).Error
				if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
					return err
				}
				if err == nil {
					if _, err := saveOptimizedPersonalVersion(tx, personal, content, cits, instruction, snapshot); err != nil {
						return err
					}
				}
			}
			if err := service.PruneSummaryResultVersions(tx, task.ID, service.SummaryResultVersionKeepLimit); err != nil {
				return err
			}
		} else {
			result, err := saveOptimizedPersonalVersion(tx, b.Personal, content, cits, instruction, snapshot)
			if err != nil {
				return err
			}
			saved.ResultID = result.ID
			saved.ResultVersion = result.Version
		}
		if err := createAgentSaveIdempotencyBinding(tx, &saved); err != nil {
			return err
		}
		return markWorkspacePreviewSaved(tx, space, user, req.SessionID, candidate, task.ID)
	})
	if err != nil {
		if h.replayOptimization(c, key, hash) {
			return
		}
		writeOptimizationError(c, err)
		return
	}
	ok(c, gin.H{"task_id": task.ID, "task_no": task.TaskNo, "status": task.Status, "created_at": task.CreatedAt, "save_mode": "new_version", "version": saved.ResultVersion, "result_id": saved.ResultID, "result_kind": saved.ResultKind})
}

// Keep the materialized personal result and its history in the same transaction.
// The snapshot uses the personal history sequence, which can differ from team's.
func saveOptimizedPersonalVersion(tx *gorm.DB, personal model.PersonalResult, content string, citations []model.Citation, instruction string, snapshot *model.Snapshot) (model.PersonalResultVersion, error) {
	var result model.PersonalResultVersion
	baseline, err := ensurePersonalVersionBaseline(tx, personal)
	if err != nil {
		return result, err
	}
	version, err := service.GetNextPersonalVersion(tx, personal.TaskID, personal.UserID)
	if err != nil {
		return result, err
	}
	now := timezone.Now()
	snapshotCopy := *snapshot
	snapshotCopy.ContentVersion = version
	snapshotCopy.ParentSnapshotVersion = nil
	if decodeResultSnapshot(baseline.SnapshotJSON) != nil {
		snapshotCopy.ParentSnapshotVersion = &baseline.Version
	}
	raw, err := json.Marshal(snapshotCopy)
	if err != nil {
		return result, err
	}
	snapshotJSON := string(raw)
	result = model.PersonalResultVersion{TaskID: personal.TaskID, ParticipantRefID: personal.ParticipantRefID, UserID: personal.UserID, Content: content, Version: version, OperationType: "refine", ParentVersionID: &baseline.ID, CreatedBy: personal.UserID, GeneratedAt: now, MsgCount: personal.MsgCount, TotalTokenUsed: personal.TotalTokenUsed, ModelVersion: personal.ModelVersion, SnapshotJSON: &snapshotJSON, OperationNote: instruction}
	result.SetCitations(citations)
	if err := tx.Create(&result).Error; err != nil {
		return result, err
	}
	if err := tx.Model(&model.PersonalResult{}).Where("id = ?", personal.ID).Updates(map[string]interface{}{"content": content, "citations_json": result.CitationsJSON, "snapshot_json": result.SnapshotJSON, "current_version_id": result.ID, "generated_at": now, "edited_at": now}).Error; err != nil {
		return result, err
	}
	return result, service.PrunePersonalResultVersions(tx, personal.TaskID, personal.UserID, service.PersonalResultVersionKeepLimit)
}
