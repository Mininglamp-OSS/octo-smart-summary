package service

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/gorm"
)

// These two storage adapters intentionally retain the existing physical tables.
// Neither GET adapter materializes a baseline, fixes a pointer or creates an
// edit version. Normalization belongs to a subsequent audited write transaction.
type contentRepository interface {
	current() (*FormalContentVersion, int64, string, error)
	version(id int64) (*FormalContentVersion, error)
	page(before int, limit int) ([]FormalContentVersion, error)
}

type resultContentRepository struct {
	db     *gorm.DB
	target ContentTarget
	task   model.SummaryTask
}

func (r resultContentRepository) current() (*FormalContentVersion, int64, string, error) {
	var count int64
	if err := r.db.Model(&model.SummaryResult{}).Where("task_id = ?", r.target.TaskID).Count(&count).Error; err != nil {
		return nil, 0, "", err
	}
	revision := r.task.ContentRevision
	if revision == 0 {
		revision = count
	}
	if r.task.CurrentResultID != nil {
		v, err := r.version(*r.task.CurrentResultID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, revision, "repair_required", nil
		}
		if err != nil {
			return nil, 0, "", err
		}
		v.ContentRevision, v.IsCurrent = revision, true
		return v, revision, "consistent", nil
	}
	if count == 0 {
		return nil, revision, "consistent", nil
	}
	// Match the legacy read fallback, but explicitly mark the absent pointer.
	// Write activation cannot silently promote MAX(version) to current.
	rows, err := r.page(0, 1)
	if err != nil {
		return nil, 0, "", err
	}
	if len(rows) == 0 {
		return nil, revision, "repair_required", nil
	}
	rows[0].ContentRevision, rows[0].IsCurrent = revision, true
	return &rows[0], revision, "normalization_required", nil
}

func (r resultContentRepository) version(id int64) (*FormalContentVersion, error) {
	var row model.SummaryResult
	if err := r.db.Where("id = ? AND task_id = ?", id, r.target.TaskID).First(&row).Error; err != nil {
		return nil, err
	}
	v := r.dto(row)
	return &v, nil
}

func (r resultContentRepository) page(before, limit int) ([]FormalContentVersion, error) {
	var rows []model.SummaryResult
	q := r.db.Where("task_id = ?", r.target.TaskID)
	if before > 0 {
		q = q.Where("version < ?", before)
	}
	if err := q.Order("version DESC").Order("id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]FormalContentVersion, 0, len(rows))
	for _, row := range rows {
		out = append(out, r.dto(row))
	}
	return out, nil
}

func (r resultContentRepository) dto(row model.SummaryResult) FormalContentVersion {
	v := FormalContentVersion{
		ContentID: r.target.ID(), VersionID: (ContentVersionIdentity{Target: r.target, RowID: row.ID}).ID(),
		Version: row.Version, ContentRevision: row.ContentRevision, Content: row.Content,
		Citations: row.GetCitations(), TeamCitations: row.GetTeamCitations(),
		OperationType: row.OperationType, OperationNote: row.OperationNote,
		BaseContentRevision: row.BaseContentRevision, GenerationID: row.GenerationID,
		EditedAt: row.EditedAt, EditedBy: row.EditedBy, GeneratedAt: row.GeneratedAt,
		GenerationSpecSnapshot: row.GenerationSpecSnapshot, RestoredAt: row.RestoredAt,
	}
	if row.ParentResultID != nil {
		v.ParentVersionID = (ContentVersionIdentity{Target: r.target, RowID: *row.ParentResultID}).ID()
	}
	if row.RestoredFromVersionID != nil {
		v.RestoredFromVersionID = (ContentVersionIdentity{Target: r.target, RowID: *row.RestoredFromVersionID}).ID()
	}
	return v
}

type personalContentRepository struct {
	db     *gorm.DB
	target ContentTarget
}

func (r personalContentRepository) current() (*FormalContentVersion, int64, string, error) {
	var rows []model.PersonalResult
	if err := r.db.Where("task_id = ? AND user_id = ?", r.target.TaskID, r.target.UserID).
		Limit(2).Find(&rows).Error; err != nil {
		return nil, 0, "", err
	}
	if len(rows) == 0 {
		return nil, 0, "consistent", nil
	}
	if len(rows) > 1 {
		return nil, 0, "repair_required", nil // never pick an arbitrary run's row
	}
	pr := rows[0]
	if pr.UserID != r.target.UserID {
		return nil, 0, "repair_required", nil
	}
	var count int64
	if err := r.db.Model(&model.PersonalResultVersion{}).
		Where("task_id = ? AND user_id = ?", r.target.TaskID, r.target.UserID).Count(&count).Error; err != nil {
		return nil, 0, "", err
	}
	revision := pr.ContentRevision
	if revision == 0 {
		revision = count
		if revision == 0 && strings.TrimSpace(pr.Content) != "" {
			revision = 1
		}
	}
	var current *FormalContentVersion
	integrity := "consistent"
	if pr.CurrentVersionID != nil {
		var err error
		current, err = r.version(*pr.CurrentVersionID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, revision, "repair_required", nil
		}
		if err != nil {
			return nil, 0, "", err
		}
	} else if count == 0 {
		if strings.TrimSpace(pr.Content) == "" {
			return nil, revision, integrity, nil
		}
		// A digest additionally catches legacy writers that don't yet increment
		// revisions. It is not a persisted version or a body exposed to another user.
		digest := sha256.Sum256([]byte(pr.Content + "\x00" + pr.CitationsJSON))
		generatedAt := pr.CreatedAt
		if pr.GeneratedAt != nil {
			generatedAt = *pr.GeneratedAt
		}
		return &FormalContentVersion{
			ContentID: r.target.ID(),
			VersionID: (ContentVersionIdentity{Target: r.target, Provisional: true,
				Revision: revision, Digest: hex.EncodeToString(digest[:])}).ID(),
			Version: 1, ContentRevision: revision, Content: pr.Content,
			Citations: pr.GetCitations(), TeamCitations: []model.TeamCitation{},
			OperationType: "generate", Provisional: true, IsCurrent: true,
			EditedAt: pr.EditedAt, GeneratedAt: generatedAt,
		}, revision, "provisional", nil
	} else {
		// A missing pointer is safe to project only when exactly one retained
		// version matches the canonical body and evidence. MAX(version) is not
		// evidence of which version a previous restore selected.
		var candidates []model.PersonalResultVersion
		if err := r.db.Where("task_id = ? AND user_id = ? AND content = ? AND COALESCE(citations_json, '') = ?",
			r.target.TaskID, r.target.UserID, pr.Content, pr.CitationsJSON).
			Limit(2).Find(&candidates).Error; err != nil {
			return nil, 0, "", err
		}
		if len(candidates) != 1 {
			return nil, revision, "repair_required", nil
		}
		v := r.dto(candidates[0])
		current = &v
		integrity = "normalization_required"
	}
	if current.Content != pr.Content || !sameContentJSON(current.Citations, pr.GetCitations()) {
		// Keep the existing canonical body visible, without pretending it is
		// the historical version body or silently adding an edit version.
		current.Content, current.Citations, current.EditedAt = pr.Content, pr.GetCitations(), pr.EditedAt
		integrity = "normalization_required"
		if pr.EditedAt == nil {
			integrity = "repair_required"
		}
	}
	current.ContentRevision, current.IsCurrent = revision, true
	return current, revision, integrity, nil
}

func (r personalContentRepository) version(id int64) (*FormalContentVersion, error) {
	var row model.PersonalResultVersion
	if err := r.db.Where("id = ? AND task_id = ? AND user_id = ?", id, r.target.TaskID, r.target.UserID).
		First(&row).Error; err != nil {
		return nil, err
	}
	if row.UserID != r.target.UserID {
		return nil, gorm.ErrRecordNotFound
	}
	v := r.dto(row)
	return &v, nil
}

func (r personalContentRepository) page(before, limit int) ([]FormalContentVersion, error) {
	var rows []model.PersonalResultVersion
	q := r.db.Where("task_id = ? AND user_id = ?", r.target.TaskID, r.target.UserID)
	if before > 0 {
		q = q.Where("version < ?", before)
	}
	if err := q.Order("version DESC").Order("id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]FormalContentVersion, 0, len(rows))
	for _, row := range rows {
		out = append(out, r.dto(row))
	}
	return out, nil
}

func (r personalContentRepository) dto(row model.PersonalResultVersion) FormalContentVersion {
	v := FormalContentVersion{
		ContentID: r.target.ID(), VersionID: (ContentVersionIdentity{Target: r.target, RowID: row.ID}).ID(),
		Version: row.Version, ContentRevision: row.ContentRevision, Content: row.Content,
		Citations: row.GetCitations(), TeamCitations: []model.TeamCitation{},
		OperationType: row.OperationType, OperationNote: row.OperationNote,
		BaseContentRevision: row.BaseContentRevision, GenerationID: row.GenerationID,
		EditedAt: row.EditedAt, EditedBy: row.EditedBy, GeneratedAt: row.GeneratedAt,
		GenerationSpecSnapshot: row.GenerationSpecSnapshot, RestoredAt: row.RestoredAt,
	}
	if row.ParentVersionID != nil {
		v.ParentVersionID = (ContentVersionIdentity{Target: r.target, RowID: *row.ParentVersionID}).ID()
	}
	if row.RestoredFromVersionID != nil {
		v.RestoredFromVersionID = (ContentVersionIdentity{Target: r.target, RowID: *row.RestoredFromVersionID}).ID()
	}
	return v
}
