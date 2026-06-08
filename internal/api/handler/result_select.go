package handler

import (
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/gorm"
)

func queryDisplayResult(db *gorm.DB, taskID int64) (model.SummaryResult, error) {
	var result model.SummaryResult
	err := db.
		Where("task_id = ?", taskID).
		Order("CASE WHEN edited_at IS NULL THEN 1 ELSE 0 END ASC").
		Order("edited_at DESC").
		Order("version DESC").
		Limit(1).
		First(&result).Error
	return result, err
}
