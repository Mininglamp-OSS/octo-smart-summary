package service

import "github.com/Mininglamp-OSS/octo-smart-summary/internal/model"

// The caller first checks target ownership and executor support. Keep the
// remaining capability policy identical for list and detail; neither is a
// substitute for command-time authorization and revision checks.
func singleContentCapabilities(task model.SummaryTask, hasCurrent bool, integrity string, rows []model.PersonalResult, busy bool, config ContentGenerationConfig) ContentCapabilities {
	if !hasCurrent || integrity == "repair_required" || len(rows) != 1 ||
		rows[0].WorkerStatus != model.PersonalStatusCompleted || task.Status != model.StatusCompleted {
		return compatibilityContentCapabilities()
	}
	caps := ContentCapabilities{
		CanViewVersions: true, CanEdit: !busy, CanRefine: !busy,
		CanConfigureSchedule: true, CanSchedule: config.State == "complete",
		CanRegenerateDirect:     config.State == "complete" && !busy,
		CanRegenerateWithConfig: true,
		UnavailableReasons:      map[string]string{"save_as_new": "not_supported", "delete": "use_summary_delete"},
	}
	if busy {
		for _, action := range []string{"edit", "refine", "regenerate_direct"} {
			caps.UnavailableReasons[action] = "generation_busy"
		}
	}
	return caps
}
