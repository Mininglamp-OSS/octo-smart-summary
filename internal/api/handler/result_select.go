package handler

import (
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/gorm"
)

// queryDisplayResult returns the result shown by read paths. If a task has an
// explicit current_result_id (for restoring an older version without creating a
// new history row), that row wins. Older rows without the pointer fall back to
// the newest version for backward compatibility and test fixtures.
func queryDisplayResult(db *gorm.DB, taskID int64) (model.SummaryResult, error) {
	var task model.SummaryTask
	if err := db.Select("current_result_id").Where("id = ?", taskID).First(&task).Error; err == nil && task.CurrentResultID != nil {
		var current model.SummaryResult
		if err := db.Where("id = ? AND task_id = ?", *task.CurrentResultID, taskID).First(&current).Error; err == nil {
			return current, nil
		}
	}

	var result model.SummaryResult
	err := db.
		Where("task_id = ?", taskID).
		Order("version DESC").
		Order("id DESC").
		Limit(1).
		First(&result).Error
	return result, err
}
