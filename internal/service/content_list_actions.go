package service

import (
	"context"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// ListContentActions contains no body, evidence, configuration inputs or run
// snapshots. Commands must load a fresh catalog/baseline and authorize again.
type ListContentActions struct {
	ContractVersion   int                     `json:"contract_version"`
	Mode              string                  `json:"mode"` // formal, legacy, unavailable
	ContentID         string                  `json:"content_id,omitempty"`
	BusinessScope     string                  `json:"business_scope"` // single, team, group, unknown
	Capabilities      ContentCapabilities     `json:"capabilities"`
	GenerationConfig  ContentGenerationConfig `json:"generation_config"`
	ActiveGeneration  *ListContentRun         `json:"active_generation"`
	UnavailableReason string                  `json:"unavailable_reason,omitempty"`
}

type ListContentRun struct {
	ID        string `json:"generation_id"`
	Status    string `json:"status"`
	CanCancel bool   `json:"can_cancel"`
}

func UnavailableListContentActions(reason string) ListContentActions {
	return ListContentActions{
		ContractVersion: ContentContractVersion, Mode: "unavailable", BusinessScope: "unknown",
		Capabilities:      compatibilityContentCapabilities(),
		GenerationConfig:  ContentGenerationConfig{State: "unavailable", MissingFields: []string{}, UnavailableReason: &reason},
		UnavailableReason: reason,
	}
}

// ListActions batches a single already-paginated task list (at most 100 items).
// It still verifies exact tenant and content readership, including config-only
// invitees that can see a list row but cannot yet access its formal content.
// At most six queries independent of page size, with at most two matching history
// rows per personal result, avoid both N+1 reads and loading unlimited history.
func (s *ContentService) ListActions(ctx context.Context, space, actor string, tasks []model.SummaryTask) (map[int64]ListContentActions, error) {
	out := make(map[int64]ListContentActions, len(tasks))
	if space == "" || actor == "" || len(tasks) > 100 {
		return out, contentError("invalid_action_scope", 400)
	}
	ids, schedules := []int64{}, []int64{}
	for _, task := range tasks {
		out[task.ID] = UnavailableListContentActions("content_forbidden")
		if task.SpaceID == space && task.DeletedAt == nil {
			ids = append(ids, task.ID)
			if task.ScheduleID != nil {
				schedules = append(schedules, *task.ScheduleID)
			}
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	db := s.db.WithContext(ctx)
	var participants []model.SummaryParticipant
	if err := db.Where("task_id IN ?", ids).Find(&participants).Error; err != nil {
		return nil, err
	}
	parts := map[int64][]model.SummaryParticipant{}
	for _, row := range participants {
		parts[row.TaskID] = append(parts[row.TaskID], row)
	}
	var scheduleRows []model.SummarySchedule
	if len(schedules) > 0 {
		if err := db.Where("id IN ? AND deleted_at IS NULL", schedules).Find(&scheduleRows).Error; err != nil {
			return nil, err
		}
	}
	scheduleByID := map[int64]*model.SummarySchedule{}
	for i := range scheduleRows {
		scheduleByID[scheduleRows[i].ID] = &scheduleRows[i]
	}
	var personal []model.PersonalResult
	if err := db.Where("task_id IN ? AND user_id = ?", ids, actor).Find(&personal).Error; err != nil {
		return nil, err
	}
	byTask := map[int64][]model.PersonalResult{}
	for _, row := range personal {
		if row.UserID == actor {
			byTask[row.TaskID] = append(byTask[row.TaskID], row)
		}
	}
	var counts []struct{ TaskID, Count int64 }
	if err := db.Model(&model.PersonalResultVersion{}).Select("task_id, COUNT(*) AS count").
		Where("task_id IN ? AND user_id = ?", ids, actor).Group("task_id").Scan(&counts).Error; err != nil {
		return nil, err
	}
	countByTask := map[int64]int64{}
	for _, row := range counts {
		countByTask[row.TaskID] = row.Count
	}
	// Join only the caller's canonical row. Both the pointer and legacy
	// body/evidence matching rules mirror personalContentRepository.current.
	var versions []model.PersonalResultVersion
	ranked := db.Table("summary_personal_result_version AS v").
		Select("v.*, ROW_NUMBER() OVER (PARTITION BY v.task_id, v.user_id ORDER BY v.id DESC) AS action_rank").
		Joins("JOIN summary_personal_result AS p ON p.task_id = v.task_id AND p.user_id = v.user_id").
		Where("v.task_id IN ? AND v.user_id = ?", ids, actor).
		Where("(p.current_version_id = v.id OR (p.current_version_id IS NULL AND v.content = p.content AND COALESCE(v.citations_json, '') = COALESCE(p.citations_json, '')))")
	if err := db.Table("(?) AS matched", ranked).Where("action_rank <= 2").Find(&versions).Error; err != nil {
		return nil, err
	}
	versionByTask := map[int64][]model.PersonalResultVersion{}
	for _, row := range versions {
		if row.UserID == actor {
			versionByTask[row.TaskID] = append(versionByTask[row.TaskID], row)
		}
	}
	var runs []model.SummaryGenerationRun
	if err := db.Select("id, space_id, task_id, content_id, actor_id, scope, status").
		Where("task_id IN ? AND space_id = ? AND active_slot IS NOT NULL", ids, space).Find(&runs).Error; err != nil {
		return nil, err
	}
	for _, task := range tasks {
		if task.SpaceID != space || task.DeletedAt != nil {
			continue
		}
		a := contentAccess{task: task, participants: parts[task.ID]}
		authorized := task.CreatorID == actor
		for _, part := range a.participants {
			authorized = authorized || (part.UserID == actor && part.Status != model.ParticipantDeclined)
		}
		if !authorized {
			continue
		}
		var schedule *model.SummarySchedule
		if task.ScheduleID != nil {
			schedule = scheduleByID[*task.ScheduleID]
		}
		if schedule != nil && schedule.SpaceID != space {
			out[task.ID] = UnavailableListContentActions("configuration_forbidden")
			continue
		}
		a, err := resolveMainContent(a, schedule)
		if err != nil || (a.main.Kind == ContentPersonal && a.main.UserID != actor) {
			out[task.ID] = UnavailableListContentActions("content_unavailable")
			continue
		}
		item := ListContentActions{ContractVersion: ContentContractVersion, Mode: "legacy",
			ContentID: a.main.ID(), BusinessScope: "group", Capabilities: compatibilityContentCapabilities(),
			GenerationConfig: compatibilityGenerationConfig(task)}
		if task.SummaryMode == model.ModeByPerson {
			item.BusinessScope = "team"
			if a.main.Kind == ContentPersonal {
				item.BusinessScope = "single"
			}
		}
		if ContentProtocolRequired(task) {
			item.Mode = "formal"
		}
		if s.requireSingleGeneration(a, a.main, actor) == nil {
			config, err := s.configurationSeed(task, actor)
			if err != nil || (schedule != nil && schedule.CreatorID != actor) {
				out[task.ID] = UnavailableListContentActions("configuration_unavailable")
				continue
			}
			item.GenerationConfig = config.ContentGenerationConfig
			rows := byTask[task.ID]
			var current *FormalContentVersion
			integrity := "consistent"
			if len(rows) == 1 {
				current, _, integrity, err = (personalContentRepository{target: a.main}).currentFromLoaded(rows[0], countByTask[task.ID], versionByTask[task.ID])
				if err != nil {
					return nil, err
				}
			} else if len(rows) > 1 {
				integrity = "repair_required"
			}
			for _, run := range runs {
				if run.SpaceID == space && run.TaskID == task.ID && (run.ContentID == a.main.ID() || run.Scope == "task") {
					item.ActiveGeneration = &ListContentRun{ID: run.ID, Status: run.Status, CanCancel: run.ActorID == actor}
					break
				}
			}
			item.Capabilities = singleContentCapabilities(task, current != nil, integrity, rows, item.ActiveGeneration != nil, item.GenerationConfig)
			if item.Capabilities.CanConfigureSchedule {
				item.Mode = "formal"
			} else if integrity == "repair_required" {
				item.Mode, item.UnavailableReason = "unavailable", "content_repair_required"
			}
		}
		out[task.ID] = item
	}
	return out, nil
}
