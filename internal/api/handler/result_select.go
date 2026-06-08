package handler

import (
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/gorm"
)

// queryDisplayResult returns the result shown by the read paths (list/detail/
// get). The display contract is "latest result wins": the highest version is
// returned regardless of whether older rows were hand-edited. A scheduled run
// (or a regenerate) inserts a strictly higher version, so its fresh output is
// shown immediately; hand-edited rows are retained in the table as queryable
// history but no longer mask a newer scheduled result.
//
// Rationale: an earlier ordering placed edited rows ahead of newer unedited
// rows, which permanently hid every future scheduled result once the user had
// edited any single result. version DESC is the single, unambiguous order.
func queryDisplayResult(db *gorm.DB, taskID int64) (model.SummaryResult, error) {
	var result model.SummaryResult
	err := db.
		Where("task_id = ?", taskID).
		Order("version DESC").
		Order("id DESC").
		Limit(1).
		First(&result).Error
	return result, err
}
