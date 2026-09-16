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
//
// Only non-derived rows are inherited. Derived rows are worker-selected
// provenance (auto-chosen channels under the creator's membership), which every
// other summary_source reader deliberately excludes: inheriting them would turn
// auto-selected channels into hard schedule constraints, expose creator-only
// channels to all participants, and — because the detail API only ever returns
// non-derived rows — lock schedule create/update behind a 409 for any task the
// worker has backfilled (PR#248 review P1). hadStored reports whether the task
// has any inheritable source at all, so the caller can fall back to the request
// for an Agent-saved summary whose sources are backfilled later.
func scheduleTaskSources(tx *gorm.DB, task model.SummaryTask, requested []sourceReq) (config model.JSON, hadStored bool, err error) {
	var stored []model.SummarySource
	if err := tx.Where("task_id = ? AND derived = 0", task.ID).Order("id").Find(&stored).Error; err != nil {
		return nil, false, err
	}
	if len(stored) == 0 {
		return nil, false, nil
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
				return nil, true, service.NewBizError(40005, "定时更新沿用当前总结来源，不能更换群聊", http.StatusConflict)
			}
			actual[key] = true
		}
		if len(actual) != len(expected) {
			return nil, true, service.NewBizError(40005, "定时更新沿用当前总结来源，不能更换群聊", http.StatusConflict)
		}
	}
	encoded, err := json.Marshal(sources)
	return model.JSON(encoded), true, err
}
