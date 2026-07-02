package notify

import (
	"errors"
	"strings"

	"gorm.io/gorm"
)

// isDuplicateKey reports whether err is a unique-constraint violation on the
// summary_notification UNIQUE(task_id, notify_kind) key. It is driver-agnostic:
//   - gorm v2 surfaces gorm.ErrDuplicatedKey for known drivers;
//   - MySQL (go-sql-driver) reports "Error 1062" / "Duplicate entry";
//   - SQLite (used by the unit tests) reports "UNIQUE constraint failed".
//
// We match all three so the preemptive-insert dedup works in prod and in tests.
func isDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "duplicate entry"):
		return true
	case strings.Contains(msg, "error 1062"):
		return true
	case strings.Contains(msg, "unique constraint failed"):
		return true
	case strings.Contains(msg, "constraint failed: unique"):
		return true
	default:
		return false
	}
}
