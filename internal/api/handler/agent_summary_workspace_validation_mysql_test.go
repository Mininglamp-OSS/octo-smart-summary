//go:build cgo

package handler

import (
	"context"
	"os"
	"testing"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// This test pins the production MySQL dialect for the participant-eligibility
// union query. Personal-result source access is narrowed separately by the
// worker.
// SQLite accepted the previous derived-table query without an alias, masking a
// production-only failure. A connection-local temporary table keeps the test
// isolated from the real IM schema.
func TestSummaryWorkspaceValidateTeamScope_MySQLParticipantUnion(t *testing.T) {
	dsn := os.Getenv("SUMMARY_MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("SUMMARY_MYSQL_TEST_DSN is not set")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatalf("open mysql: %v", err)
	}

	err = db.Connection(func(tx *gorm.DB) error {
		if err := tx.Exec(`CREATE TEMPORARY TABLE group_member (
			group_no VARCHAR(64) NOT NULL,
			uid VARCHAR(64) NOT NULL,
			status TINYINT NOT NULL,
			is_deleted TINYINT NOT NULL
		)`).Error; err != nil {
			return err
		}
		if err := tx.Exec(`INSERT INTO group_member (group_no, uid, status, is_deleted) VALUES
			('group-a', 'member-a', 1, 0),
			('group-b', 'member-b', 1, 0),
			('group-a', 'inactive', 0, 0)`).Error; err != nil {
			return err
		}
		coordinator := &summaryWorkspaceCoordinator{imDB: tx}
		valid, reason, validateErr := coordinator.validateTeamScope(
			context.Background(),
			[]summaryWorkspaceChannel{
				{ChatID: "group-a", ChatType: "group"},
				{ChatID: "group-b", ChatType: "group"},
			},
			[]summaryWorkspaceParticipant{
				{UserID: "member-a"},
				{UserID: "member-b"},
			},
		)
		if validateErr != nil {
			return validateErr
		}
		if !valid || reason != teamScopeReasonNone {
			t.Fatalf("union validation valid=%t reason=%q", valid, reason)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("mysql participant-union validation: %v", err)
	}
}
