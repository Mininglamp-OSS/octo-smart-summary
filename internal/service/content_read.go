package service

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/gorm"
)

// ContentService is the authenticated formal-content boundary. In the
// compatibility phase it advertises read capabilities only: no UI can enable
// new mutations before API, worker and scheduler share the write protocol.
type ContentService struct{ db *gorm.DB }

func NewContentService(db *gorm.DB) *ContentService { return &ContentService{db: db} }

func sameContentJSON(a, b any) bool { return reflect.DeepEqual(a, b) }

type contentAccess struct {
	task         model.SummaryTask
	participants []model.SummaryParticipant
	main         ContentTarget
}

func (s *ContentService) access(ctx context.Context, spaceID string, taskID int64, actorID string) (contentAccess, error) {
	var a contentAccess
	if spaceID == "" || actorID == "" || taskID <= 0 {
		return a, contentError("content_not_found", 404)
	}
	db := s.db.WithContext(ctx)
	if err := db.Where("id = ? AND space_id = ? AND deleted_at IS NULL", taskID, spaceID).First(&a.task).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return a, contentError("content_not_found", 404)
		}
		return a, err
	}
	// Legacy MySQL tables use a case-insensitive collation. Tenant identity is
	// still exact, even if the SQL equality matched a differently-cased value.
	if a.task.SpaceID != spaceID {
		return a, contentError("content_not_found", 404)
	}
	if err := db.Where("task_id = ?", taskID).Find(&a.participants).Error; err != nil {
		return a, err
	}
	authorized := a.task.CreatorID == actorID
	for _, p := range a.participants {
		if p.UserID == actorID && p.Status != model.ParticipantDeclined {
			authorized = true
		}
	}
	if !authorized {
		return a, contentError("content_forbidden", 403)
	}
	a.main = ContentTarget{SpaceID: spaceID, TaskID: taskID, Kind: ContentResult}
	if a.task.SummaryMode != model.ModeByPerson {
		return a, nil
	}
	// A normalized, explicitly confirmed configuration is authoritative. Never
	// infer collaboration type from how many members happened to finish a run.
	if len(a.task.GenerationSpecJSON) > 0 {
		var spec struct {
			Collaboration string   `json:"collaboration"`
			Participants  []string `json:"participants"`
		}
		if json.Unmarshal(a.task.GenerationSpecJSON, &spec) != nil {
			return a, contentError("configuration_repair_required", 409)
		}
		if spec.Collaboration == "single" {
			if len(spec.Participants) != 1 || spec.Participants[0] != a.task.CreatorID {
				return a, contentError("configuration_repair_required", 409)
			}
			a.main.Kind, a.main.UserID = ContentPersonal, a.task.CreatorID
		} else if spec.Collaboration != "team" {
			return a, contentError("configuration_repair_required", 409)
		}
		return a, nil
	}
	if a.task.ScheduleID != nil {
		var schedule model.SummarySchedule
		err := db.Where("id = ? AND space_id = ? AND deleted_at IS NULL", *a.task.ScheduleID, spaceID).First(&schedule).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return a, err
		}
		if err == nil {
			config := model.ParseScheduleParticipantConfig(schedule.ParticipantConfig)
			roster := config.EffectiveUserIDs(a.task.CreatorID)
			if len(roster) > 1 {
				return a, nil
			}
		}
	}
	// Legacy Agent saves and single-person workflows have exactly the creator
	// in the configured roster. A creator + one other author remains a team.
	if len(a.participants) == 1 && a.participants[0].UserID == a.task.CreatorID {
		a.main.Kind, a.main.UserID = ContentPersonal, a.task.CreatorID
	}
	return a, nil
}

func (s *ContentService) resolve(ctx context.Context, spaceID string, taskID int64, actorID, contentID string) (contentAccess, ContentTarget, contentRepository, error) {
	a, err := s.access(ctx, spaceID, taskID, actorID)
	if err != nil {
		return a, ContentTarget{}, nil, err
	}
	target, err := ParseContentID(contentID)
	if err != nil || target.SpaceID != spaceID || target.TaskID != taskID {
		return a, target, nil, contentError("content_not_found", 404)
	}
	if target.Kind == ContentPersonal {
		// Other members' submitted bodies remain available through the existing
		// member API. A creator role never grants another member's version pool.
		if target.UserID != actorID || a.task.SummaryMode != model.ModeByPerson {
			return a, target, nil, contentError("content_forbidden", 403)
		}
		member := false
		for _, p := range a.participants {
			if p.UserID == actorID && p.Status != model.ParticipantDeclined {
				member = true
			}
		}
		if !member {
			return a, target, nil, contentError("content_forbidden", 403)
		}
	} else if a.main.Kind != ContentResult {
		// Do not expose the legacy single-person mirror as a second main content.
		return a, target, nil, contentError("content_not_found", 404)
	}
	return a, target, s.repository(ctx, a.task, target), nil
}

func (s *ContentService) repository(ctx context.Context, task model.SummaryTask, target ContentTarget) contentRepository {
	db := s.db.WithContext(ctx)
	if target.Kind == ContentPersonal {
		return personalContentRepository{db: db, target: target}
	}
	return resultContentRepository{db: db, target: target, task: task}
}

func (s *ContentService) Catalog(ctx context.Context, spaceID string, taskID int64, actorID string) (FormalContentCatalog, error) {
	a, err := s.access(ctx, spaceID, taskID, actorID)
	if err != nil {
		return FormalContentCatalog{}, err
	}
	out := FormalContentCatalog{
		ContractVersion: ContentContractVersion, SummaryID: taskID,
		CreatedVia: a.task.CreatedVia, MainContentID: a.main.ID(), Contents: []FormalContent{},
	}
	switch out.CreatedVia {
	case "agent", "workflow", "unknown":
	default:
		out.CreatedVia = "unknown"
	}
	targets := []ContentTarget{a.main}
	if a.main.Kind == ContentResult && a.task.SummaryMode == model.ModeByPerson {
		for _, p := range a.participants {
			if p.UserID == actorID && p.Status != model.ParticipantDeclined {
				targets = append(targets, ContentTarget{SpaceID: spaceID, TaskID: taskID, Kind: ContentPersonal, UserID: actorID})
				break
			}
		}
	}
	for _, target := range targets {
		if target.Kind == ContentPersonal && target.UserID != actorID {
			return out, contentError("content_forbidden", 403)
		}
		current, revision, integrity, err := s.repository(ctx, a.task, target).current()
		if err != nil {
			return out, err
		}
		if current != nil {
			s.filterVersion(a, target, actorID, current)
		}
		out.Contents = append(out.Contents, FormalContent{
			ContentID: target.ID(), Kind: target.Kind, OwnerID: target.UserID,
			IsMain: target == a.main, ContentRevision: revision, CurrentVersion: current,
			Capabilities: compatibilityContentCapabilities(), Integrity: integrity,
			GenerationConfig: compatibilityGenerationConfig(a.task),
		})
	}
	return out, nil
}

func compatibilityContentCapabilities() ContentCapabilities {
	reasons := map[string]string{}
	for _, action := range []string{"edit", "refine", "save_as_new", "configure_schedule",
		"schedule", "regenerate_direct", "regenerate_with_config", "delete"} {
		reasons[action] = "write_protocol_not_enabled"
	}
	return ContentCapabilities{CanViewVersions: true, UnavailableReasons: reasons}
}

func compatibilityGenerationConfig(task model.SummaryTask) ContentGenerationConfig {
	// Until an executor validates a complete spec, do not advertise legacy
	// snapshots/title-derived requirements as replayable configuration.
	reason := "configuration_adapter_not_enabled"
	return ContentGenerationConfig{
		State: "unavailable", Revision: task.ConfigRevision,
		MissingFields: []string{}, UnavailableReason: &reason,
	}
}

func (s *ContentService) filterVersion(a contentAccess, target ContentTarget, actorID string, v *FormalContentVersion) {
	v.CitationVisibility = "visible"
	if target.Kind == ContentResult && a.task.SummaryMode == model.ModeByPerson {
		// Preserve the existing producing-member rule for legacy single-
		// contributor team rounds. Owning the task alone is never sufficient.
		var owned []model.PersonalResult
		visible := false
		if err := s.db.Where("task_id = ? AND user_id = ?", target.TaskID, actorID).Limit(2).Find(&owned).Error; err == nil &&
			len(owned) == 1 && owned[0].UserID == actorID && len(v.Citations) > 0 {
			visible = sameContentJSON(owned[0].GetCitations(), v.Citations)
		}
		if !visible {
			v.Citations = []model.Citation{}
			v.CitationVisibility = "permission_hidden"
		}
	} else if len(v.Citations) == 0 {
		v.CitationVisibility = "not_generated"
	}
	v.TeamCitationVisibility = "current_report"
	if len(v.TeamCitations) == 0 {
		v.TeamCitationVisibility = "none"
	} else if !v.IsCurrent {
		v.TeamCitationVisibility = "historical_identity_only"
		for i := range v.TeamCitations {
			v.TeamCitations[i].PersonalResultID = 0
			v.TeamCitations[i].TaskID = 0
		}
	}
}

type contentPageCursor struct {
	Target  ContentTarget `json:"c"`
	Version int           `json:"v"`
}

func (s *ContentService) Versions(ctx context.Context, spaceID string, taskID int64, actorID, contentID, cursor string, limit int) (FormalVersionPage, error) {
	a, target, repo, err := s.resolve(ctx, spaceID, taskID, actorID, contentID)
	if err != nil {
		return FormalVersionPage{}, err
	}
	if limit == 0 {
		limit = 20
	}
	if limit < 1 || limit > 100 {
		return FormalVersionPage{}, contentError("invalid_page_limit", 400)
	}
	before := 0
	if cursor != "" {
		var c contentPageCursor
		if decodeContentToken(cursor, "sp1_", &c) != nil || c.Target != target || c.Version <= 0 ||
			encodeContentToken("sp1_", c) != cursor {
			return FormalVersionPage{}, contentError("invalid_version_cursor", 400)
		}
		before = c.Version
	}
	// Duplicate legacy version numbers cannot be paged unambiguously using a
	// number-only cursor. Surface repair rather than silently dropping rows.
	var duplicates int64
	table := &model.SummaryResult{}
	if target.Kind == ContentResult {
		if err := s.db.WithContext(ctx).Model(table).Select("COUNT(*)").
			Where("task_id = ?", taskID).Group("version").Having("COUNT(*) > 1").
			Limit(1).Scan(&duplicates).Error; err != nil {
			return FormalVersionPage{}, err
		}
	}
	if duplicates > 0 {
		return FormalVersionPage{}, contentError("version_repair_required", 409)
	}
	current, _, _, err := repo.current()
	if err != nil {
		return FormalVersionPage{}, err
	}
	items, err := repo.page(before, limit+1)
	if err != nil {
		return FormalVersionPage{}, err
	}
	if len(items) == 0 && before == 0 && current != nil && current.Provisional {
		items = append(items, *current)
	}
	out := FormalVersionPage{Items: items}
	if len(items) > limit {
		out.Items = items[:limit]
		out.NextCursor = encodeContentToken("sp1_", contentPageCursor{Target: target, Version: out.Items[limit-1].Version})
	}
	for i := range out.Items {
		v := &out.Items[i]
		if current != nil && v.VersionID == current.VersionID {
			v.IsCurrent, v.ContentRevision = true, current.ContentRevision
		}
		s.filterVersion(a, target, actorID, v)
	}
	return out, nil
}

func (s *ContentService) Version(ctx context.Context, spaceID string, taskID int64, actorID, contentID, versionID string) (*FormalContentVersion, error) {
	a, target, repo, err := s.resolve(ctx, spaceID, taskID, actorID, contentID)
	if err != nil {
		return nil, err
	}
	id, err := ParseContentVersionID(versionID, target)
	if err != nil {
		return nil, contentError("version_not_found", 404)
	}
	current, _, _, err := repo.current()
	if err != nil {
		return nil, err
	}
	if id.Provisional {
		if current == nil || !current.Provisional || current.VersionID != versionID {
			return nil, contentError("content_conflict", 409)
		}
		s.filterVersion(a, target, actorID, current)
		return current, nil
	}
	v, err := repo.version(id.RowID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, contentError("version_not_found", 404)
	}
	if err != nil {
		return nil, err
	}
	if current != nil && current.VersionID == versionID {
		v.IsCurrent, v.ContentRevision = true, current.ContentRevision
	}
	s.filterVersion(a, target, actorID, v)
	return v, nil
}

// ParseContentReadSpaces accepts an exact allowlist; "*" is intentionally not a
// production-wide shortcut. API/worker configuration can reuse the same parser.
func ParseContentReadSpaces(raw string) map[string]bool {
	out := map[string]bool{}
	for _, space := range strings.Split(raw, ",") {
		if space = strings.TrimSpace(space); space != "" && space != "*" {
			out[space] = true
		}
	}
	return out
}
