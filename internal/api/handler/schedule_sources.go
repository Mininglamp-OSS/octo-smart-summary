package handler

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"gorm.io/gorm"
)

// The caller holds the task lock. A schedule inherits the summary's sources;
// resending those same identifiers is compatible, but replacing them is not.
func scheduleTaskSources(tx *gorm.DB, task model.SummaryTask, requested []sourceReq) (model.JSON, error) {
	var stored []model.SummarySource
	if err := tx.Where("task_id = ?", task.ID).Order("id").Find(&stored).Error; err != nil {
		return nil, err
	}
	sources := make([]sourceReq, 0, len(stored))
	expected := make(map[string]bool, len(stored))
	for _, src := range stored {
		sources = append(sources, sourceReq{SourceType: src.SourceType, SourceID: src.SourceID, SourceName: src.SourceName})
		expected[fmt.Sprintf("%d:%s", src.SourceType, src.SourceID)] = true
	}
	if requested != nil {
		actual := make(map[string]bool, len(requested))
		for _, src := range requested {
			key := fmt.Sprintf("%d:%s", src.SourceType, src.SourceID)
			if !expected[key] {
				return nil, service.NewBizError(40005, "定时更新沿用当前总结来源，不能更换群聊", http.StatusConflict)
			}
			actual[key] = true
		}
		if len(actual) != len(expected) {
			return nil, service.NewBizError(40005, "定时更新沿用当前总结来源，不能更换群聊", http.StatusConflict)
		}
	}
	encoded, err := json.Marshal(sources)
	return model.JSON(encoded), err
}
