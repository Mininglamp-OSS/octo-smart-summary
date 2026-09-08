package service

import (
	"errors"
	"os"
	"sort"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Old clients receive a conflict, never a false success or internal error.
var ErrContentProtocolRequired = NewBizError(40901, "content_protocol_required", 409)

// ContentWriteSpaces is shared by API, retention and Worker. Read enrollment
// never grants write capability. Empty and wildcard values grant no access.
func ContentWriteSpaces() []string {
	parsed := ParseContentReadSpaces(os.Getenv("SUMMARY_CONTENT_WRITE_SPACES"))
	spaces := make([]string, 0, len(parsed))
	for space := range parsed {
		spaces = append(spaces, space)
	}
	sort.Strings(spaces)
	return spaces
}

func ContentProtocolRequired(task model.SummaryTask) bool {
	if task.ContentProtocolVersion > 0 {
		return true
	}
	for _, space := range ContentWriteSpaces() {
		if space == task.SpaceID {
			return true
		}
	}
	return false
}

// CheckLegacyContentWrite must follow the task lock and precede mutations.
// Checking only before an LLM call leaves a migration/commit race.
func CheckLegacyContentWrite(task model.SummaryTask) error {
	if ContentProtocolRequired(task) {
		return ErrContentProtocolRequired
	}
	return nil
}

// LegacyTaskScope also fences single-statement task claims/failure transitions.
// UPDATE evaluates the marker while holding the same task row lock as enrollment.
// This scope is only for queries whose model is SummaryTask, not joined tables.
func LegacyTaskScope(db *gorm.DB) *gorm.DB {
	db = db.Where("content_protocol_version = 0")
	if spaces := ContentWriteSpaces(); len(spaces) > 0 {
		column := "space_id"
		if db.Dialector.Name() == "mysql" {
			column = "BINARY space_id"
		}
		db = db.Where(column+" NOT IN ?", spaces)
	}
	return db
}

// LockLegacyContentTask is for callers holding no content/member locks yet.
// Legacy handlers also change schedules, so lock schedule -> task, matching the
// scheduler and membership handlers. Coordinated content writes only lock
// task -> content and never acquire a schedule lock.
func LockLegacyContentTask(tx *gorm.DB, taskID int64) error {
	var hint model.SummaryTask
	if err := tx.First(&hint, taskID).Error; err != nil {
		return err
	}
	if hint.ScheduleID != nil {
		var schedule model.SummarySchedule
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&schedule, *hint.ScheduleID).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
	}
	var task model.SummaryTask
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&task, taskID).Error; err != nil {
		return err
	}
	if task.DeletedAt != nil {
		return gorm.ErrRecordNotFound
	}
	if (hint.ScheduleID == nil) != (task.ScheduleID == nil) ||
		(hint.ScheduleID != nil && task.ScheduleID != nil && *hint.ScheduleID != *task.ScheduleID) {
		return NewBizError(40901, "summary_configuration_changed", 409)
	}
	return CheckLegacyContentWrite(task)
}

// WithLegacyContentWrite holds the compatibility fence until commit. It can be
// composed inside a transaction, but must precede content/member row locks.
func WithLegacyContentWrite(db *gorm.DB, taskID int64, write func(*gorm.DB) error) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := LockLegacyContentTask(tx, taskID); err != nil {
			return err
		}
		return write(tx)
	})
}

// Cross-round membership removal must check every affected task, not just the
// clicked round. Caller already holds the schedule lock.
func CheckLegacyScheduleContent(tx *gorm.DB, space string, scheduleID int64) error {
	var tasks []model.SummaryTask
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("schedule_id = ? AND space_id = ? AND deleted_at IS NULL", scheduleID, space).
		Order("id ASC").Find(&tasks).Error; err != nil {
		return err
	}
	for _, task := range tasks {
		if err := CheckLegacyContentWrite(task); err != nil {
			return err
		}
	}
	return nil
}

// Decision and DELETE serialize with enrollment, so a stale cleaner cannot
// prune new data. Retention never acquires a schedule lock.
func preserveContentHistory(db *gorm.DB, taskID int64, prune func(*gorm.DB) error) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var task model.SummaryTask
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&task, taskID).Error; err != nil {
			return err
		}
		if ContentProtocolRequired(task) {
			return nil
		}
		return prune(tx)
	})
}

// CancelContentGenerationsForDeletion requires the task row lock. Call before
// soft-deleting that task, in the same transaction, so late model callbacks
// cannot commit and pending runs do not keep their active slots indefinitely.
func CancelContentGenerationsForDeletion(tx *gorm.DB, task model.SummaryTask) error {
	if task.ContentProtocolVersion == 0 {
		return nil
	}
	return tx.Model(&model.SummaryGenerationRun{}).
		Where("task_id = ? AND active_slot IS NOT NULL", task.ID).
		Updates(map[string]any{
			"status": "cancelled", "stage": "finished", "cancel_requested": true,
			"active_slot": nil, "lease_until": nil, "error_code": "summary_deleted",
			"updated_at": timezone.Now(),
		}).Error
}
