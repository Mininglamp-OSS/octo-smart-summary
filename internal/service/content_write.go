package service

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ContentBaseline is mandatory even when the current version has not changed:
// editing/restoring a version changes its revision without changing its ID.
type ContentBaseline struct {
	ExpectedCurrentVersionID string `json:"expected_current_version_id"`
	ExpectedContentRevision  int64  `json:"expected_content_revision"`
}

type EditContentRequest struct {
	ContentBaseline
	Content string `json:"content"`
}

type RestoreContentRequest struct {
	ContentBaseline
	SourceVersionID string `json:"source_version_id"`
}

type lockedContent struct {
	service *ContentService
	access  contentAccess
	target  ContentTarget
	repo    contentRepository
	current *FormalContentVersion
	pr      *model.PersonalResult
}

// Always lock task -> membership -> materialized content -> run. The task lock
// is held only for admission/commit, never for retrieval or model execution.
// All legacy writers must adopt this order before the write rollout is enabled.
func (s *ContentService) lockContent(ctx context.Context, space string, taskID int64, actor, contentID string) (*lockedContent, error) {
	var task model.SummaryTask
	if err := s.db.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ? AND space_id = ? AND deleted_at IS NULL", taskID, space).First(&task).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, contentError("content_not_found", 404)
		}
		return nil, err
	}
	if task.SpaceID != space {
		return nil, contentError("content_not_found", 404)
	}
	var members []model.SummaryParticipant
	if err := s.db.Clauses(clause.Locking{Strength: "UPDATE"}).Where("task_id = ?", taskID).Find(&members).Error; err != nil {
		return nil, err
	}
	a, target, repo, err := s.resolve(ctx, space, taskID, actor, contentID)
	if err != nil {
		return nil, err
	}
	if target.Kind == ContentResult && a.task.CreatorID != actor {
		return nil, contentError("content_forbidden", 403)
	}
	l := &lockedContent{service: s, access: a, target: target, repo: repo}
	if target.Kind == ContentPersonal {
		var rows []model.PersonalResult
		if err := s.db.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("task_id = ? AND user_id = ?", taskID, target.UserID).Limit(2).Find(&rows).Error; err != nil {
			return nil, err
		}
		if len(rows) != 1 || rows[0].UserID != target.UserID {
			return nil, contentError("content_repair_required", 409)
		}
		l.pr = &rows[0]
	}
	current, _, integrity, err := repo.current()
	if err != nil {
		return nil, err
	}
	if integrity == "repair_required" {
		return nil, contentError("content_repair_required", 409)
	}
	if target.Kind == ContentResult {
		var duplicate int64
		if err := s.db.Model(&model.SummaryResult{}).Select("COUNT(*)").
			Where("task_id = ?", taskID).Group("version").Having("COUNT(*) > 1").Limit(1).Scan(&duplicate).Error; err != nil {
			return nil, err
		}
		if duplicate > 0 {
			return nil, contentError("version_repair_required", 409)
		}
		// A missing pointer plus several rows cannot prove a former restore's
		// selection. Never normalize it by guessing the highest version.
		if integrity == "normalization_required" {
			var count int64
			if err := s.db.Model(&model.SummaryResult{}).Where("task_id = ?", taskID).Count(&count).Error; err != nil {
				return nil, err
			}
			if count != 1 {
				return nil, contentError("content_repair_required", 409)
			}
		}
	}
	l.current = current
	return l, nil
}

func (l *lockedContent) checkBaseline(base ContentBaseline) error {
	if base.ExpectedCurrentVersionID == "" || base.ExpectedContentRevision <= 0 {
		return contentError("content_baseline_required", 400)
	}
	if l.current == nil || l.current.VersionID != base.ExpectedCurrentVersionID ||
		l.current.ContentRevision != base.ExpectedContentRevision {
		return contentError("content_conflict", 409)
	}
	return nil
}

func (l *lockedContent) checkEditableStage() error {
	if l.target.Kind == ContentResult {
		if l.access.task.Status != model.StatusCompleted {
			return contentError("content_not_editable", 409)
		}
	} else if l.pr.WorkerStatus != model.PersonalStatusCompleted {
		return contentError("content_not_editable", 409)
	} else {
		for _, participant := range l.access.participants {
			if participant.UserID == l.target.UserID && participant.Status == model.ParticipantPending {
				return contentError("content_not_editable", 409)
			}
		}
	}
	return nil
}

// normalize runs only after authorization and CAS, inside the eventual write
// transaction. It never adds an "edit version" for a legacy materialized edit.
func (l *lockedContent) normalize(actor string, now time.Time) error {
	if l.current == nil {
		return contentError("content_not_found", 404)
	}
	db, v := l.service.db, l.current
	normalized := false
	if l.pr != nil {
		if !validMessageEvidence(l.pr.CitationsJSON) {
			return contentError("content_repair_required", 409)
		}
		if v.Provisional {
			row := model.PersonalResultVersion{
				TaskID: l.target.TaskID, UserID: l.target.UserID, ParticipantRefID: l.pr.ParticipantRefID,
				Content: l.pr.Content, CitationsJSON: l.pr.CitationsJSON, Version: 1,
				ContentRevision: v.ContentRevision, OperationType: "generate",
				MsgCount: l.pr.MsgCount, TotalTokenUsed: l.pr.TotalTokenUsed, ModelVersion: l.pr.ModelVersion,
				GeneratedAt: v.GeneratedAt, CreatedBy: l.target.UserID, EditedAt: l.pr.EditedAt,
			}
			if err := db.Create(&row).Error; err != nil {
				return err
			}
			v.VersionID = (ContentVersionIdentity{Target: l.target, RowID: row.ID}).ID()
			v.Provisional = false
			normalized = true
		} else {
			id, _ := ParseContentVersionID(v.VersionID, l.target)
			var row model.PersonalResultVersion
			if err := db.First(&row, id.RowID).Error; err != nil {
				return err
			}
			if !validMessageEvidence(row.CitationsJSON) {
				return contentError("content_repair_required", 409)
			}
			normalized = l.pr.CurrentVersionID == nil || row.Content != l.pr.Content ||
				!sameContentJSON(row.GetCitations(), l.pr.GetCitations())
			updates := map[string]any{"content_revision": v.ContentRevision}
			if normalized {
				updates["content"], updates["citations_json"], updates["edited_at"] = l.pr.Content, l.pr.CitationsJSON, l.pr.EditedAt
			}
			if err := db.Model(&row).Updates(updates).Error; err != nil {
				return err
			}
		}
	} else {
		id, _ := ParseContentVersionID(v.VersionID, l.target)
		var row model.SummaryResult
		if err := db.First(&row, id.RowID).Error; err != nil {
			return err
		}
		if !validMessageEvidence(row.CitationsJSON) || !validTeamEvidence(row.TeamCitationsJSON) {
			return contentError("content_repair_required", 409)
		}
		normalized = l.access.task.CurrentResultID == nil
		if err := db.Model(&row).Update("content_revision", v.ContentRevision).Error; err != nil {
			return err
		}
	}
	if err := l.setCurrent(v, false); err != nil {
		return err
	}
	if l.access.task.ContentProtocolVersion < ContentContractVersion {
		if err := db.Model(&model.SummaryTask{}).Where("id = ?", l.target.TaskID).
			Update("content_protocol_version", ContentContractVersion).Error; err != nil {
			return err
		}
		l.access.task.ContentProtocolVersion = ContentContractVersion
	}
	if normalized {
		return l.audit(actor, "normalize", "", 0, now)
	}
	return nil
}

func validMessageEvidence(raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return true
	}
	var rows []model.Citation
	if json.Unmarshal([]byte(raw), &rows) != nil || rows == nil {
		return false
	}
	seen := map[int]bool{}
	for _, row := range rows {
		if row.Index <= 0 || seen[row.Index] {
			return false
		}
		seen[row.Index] = true
	}
	return true
}

func validTeamEvidence(raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return true
	}
	var rows []model.TeamCitation
	if json.Unmarshal([]byte(raw), &rows) != nil || rows == nil {
		return false
	}
	seen := map[int]bool{}
	for _, row := range rows {
		if row.Index <= 0 || seen[row.Index] || row.UserID == "" {
			return false
		}
		seen[row.Index] = true
	}
	return true
}

func (l *lockedContent) validateStoredEvidence(id int64) error {
	var row struct{ CitationsJSON, TeamCitationsJSON string }
	table := "summary_result"
	if l.pr != nil {
		table = "summary_personal_result_version"
	}
	if err := l.service.db.Table(table).Where("id = ?", id).First(&row).Error; err != nil {
		return err
	}
	if !validMessageEvidence(row.CitationsJSON) || !validTeamEvidence(row.TeamCitationsJSON) {
		return contentError("content_repair_required", 409)
	}
	return nil
}

func (l *lockedContent) audit(actor, operation, source string, sourceRevision int64, now time.Time) error {
	return l.service.db.Create(&model.SummaryContentAudit{
		SpaceID: l.target.SpaceID, TaskID: l.target.TaskID, ContentID: l.target.ID(),
		VersionID: l.current.VersionID, ContentRevision: l.current.ContentRevision,
		OperationType: operation, ActorID: actor, SourceVersionID: source,
		SourceContentRevision: sourceRevision, CreatedAt: now,
	}).Error
}

// setCurrent synchronizes the pointer/revision and personal materialization.
// It deliberately does not reset collaboration/submission state or a schedule.
func (l *lockedContent) setCurrent(v *FormalContentVersion, contentChanged bool) error {
	id, err := ParseContentVersionID(v.VersionID, l.target)
	if err != nil || id.Provisional {
		return contentError("content_repair_required", 409)
	}
	if l.pr == nil {
		return l.service.db.Model(&model.SummaryTask{}).Where("id = ?", l.target.TaskID).
			Updates(map[string]any{"current_result_id": id.RowID, "content_revision": v.ContentRevision}).Error
	}
	updates := map[string]any{"current_version_id": id.RowID, "content_revision": v.ContentRevision}
	if contentChanged {
		citations, _ := json.Marshal(v.Citations)
		updates["content"], updates["citations_json"], updates["edited_at"] = v.Content, string(citations), v.EditedAt
	}
	return l.service.db.Model(&model.PersonalResult{}).Where("id = ?", l.pr.ID).Updates(updates).Error
}

func (l *lockedContent) overwrite(actor, operation string, source *FormalContentVersion, now time.Time) error {
	v := l.current
	v.Content, v.Citations, v.TeamCitations = source.Content, source.Citations, source.TeamCitations
	v.GenerationSpecSnapshot = source.GenerationSpecSnapshot
	v.ContentRevision++
	v.EditedAt, v.EditedBy = &now, actor
	citations, _ := json.Marshal(v.Citations)
	team, _ := json.Marshal(v.TeamCitations)
	updates := map[string]any{
		"content": v.Content, "citations_json": string(citations), "content_revision": v.ContentRevision,
		"generation_spec_snapshot": v.GenerationSpecSnapshot, "edited_at": now, "edited_by": actor,
	}
	sourceID, sourceRevision := "", int64(0)
	if operation == "restore" {
		sourceID, sourceRevision = source.VersionID, source.ContentRevision
		id, _ := ParseContentVersionID(sourceID, l.target)
		updates["restored_from_version_id"], updates["restored_at"] = id.RowID, now
		v.RestoredFromVersionID, v.RestoredAt = sourceID, &now
	} else {
		updates["restored_from_version_id"], updates["restored_at"] = nil, nil
		v.RestoredFromVersionID, v.RestoredAt = "", nil
	}
	id, _ := ParseContentVersionID(v.VersionID, l.target)
	var table any = &model.PersonalResultVersion{}
	if l.target.Kind == ContentResult {
		table = &model.SummaryResult{}
		updates["team_citations_json"] = string(team)
	}
	if err := l.service.db.Model(table).Where("id = ?", id.RowID).Updates(updates).Error; err != nil {
		return err
	}
	if err := l.setCurrent(v, true); err != nil {
		return err
	}
	return l.audit(actor, operation, sourceID, sourceRevision, now)
}

var formalCitationMarker = regexp.MustCompile(`\[(P?)([0-9]+)\]`)

func validateContentBody(body string) error {
	if strings.TrimSpace(body) == "" || len(body) > 500*1024 {
		return contentError("invalid_content", 400)
	}
	return nil
}

// Pools keep their original indices, including unused entries. Clients cannot
// inject new evidence through an edit or manufacture evidence during refine.
func validateContentMarkers(body string, citations []model.Citation, team []model.TeamCitation) error {
	messages, people := map[int]bool{}, map[int]bool{}
	for _, citation := range citations {
		messages[citation.Index] = true
	}
	for _, citation := range team {
		people[citation.Index] = true
	}
	for _, match := range formalCitationMarker.FindAllStringSubmatch(body, -1) {
		index, err := strconv.Atoi(match[2])
		if err != nil || index <= 0 || (match[1] == "P" && !people[index]) || (match[1] == "" && !messages[index]) {
			return contentError("invalid_citation_reference", 422)
		}
	}
	return nil
}

func (s *ContentService) Edit(ctx context.Context, space string, taskID int64, actor, contentID string, request EditContentRequest) (*FormalContentVersion, error) {
	if err := validateContentBody(request.Content); err != nil {
		return nil, err
	}
	return s.mutate(ctx, space, taskID, actor, contentID, request.ContentBaseline, func(l *lockedContent, now time.Time) error {
		source := *l.current
		source.Content = request.Content
		if err := validateContentMarkers(source.Content, source.Citations, source.TeamCitations); err != nil {
			return err
		}
		return l.overwrite(actor, "edit", &source, now)
	})
}

func (s *ContentService) Restore(ctx context.Context, space string, taskID int64, actor, contentID string, request RestoreContentRequest) (*FormalContentVersion, error) {
	return s.mutate(ctx, space, taskID, actor, contentID, request.ContentBaseline, func(l *lockedContent, now time.Time) error {
		id, err := ParseContentVersionID(request.SourceVersionID, l.target)
		if err != nil || id.Provisional || request.SourceVersionID == l.current.VersionID {
			return contentError("invalid_restore_source", 400)
		}
		source, err := l.repo.version(id.RowID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return contentError("version_not_found", 404)
		}
		if err != nil {
			return err
		}
		if err := l.validateStoredEvidence(id.RowID); err != nil {
			return err
		}
		if err := l.service.markCandidate(ctx, l.target, source); err != nil {
			return err
		}
		if source.PendingApplication {
			return contentError("invalid_restore_source", 409)
		}
		return l.overwrite(actor, "restore", source, now)
	})
}

func (s *ContentService) mutate(ctx context.Context, space string, taskID int64, actor, contentID string, base ContentBaseline, fn func(*lockedContent, time.Time) error) (*FormalContentVersion, error) {
	var result *FormalContentVersion
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		writer := s.withDB(tx)
		l, err := writer.lockContent(ctx, space, taskID, actor, contentID)
		if err != nil {
			return err
		}
		if err := l.checkBaseline(base); err != nil {
			return err
		}
		if err := l.checkEditableStage(); err != nil {
			return err
		}
		now := timezone.Now()
		if err := l.normalize(actor, now); err != nil {
			return err
		}
		if err := fn(l, now); err != nil {
			return err
		}
		result = l.current
		writer.filterVersion(l.access, l.target, actor, result)
		return nil
	})
	return result, err
}
