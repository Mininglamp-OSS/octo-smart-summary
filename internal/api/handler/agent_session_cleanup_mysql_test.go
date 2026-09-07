package handler

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// Opt-in production-dialect coverage for the evidence-retention guard. The
// workspace session identifier is utf8mb4_0900_bin while legacy evidence uses
// utf8mb4_unicode_ci, a combination SQLite cannot validate.
func TestCleanupExpiredAgentEvidence_MySQLMixedCollations(t *testing.T) {
	dsn := os.Getenv("SUMMARY_MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("SUMMARY_MYSQL_TEST_DSN is not set")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatalf("open mysql: %v", err)
	}
	for _, table := range []string{"agent_summary_session", "agent_message_evidence"} {
		if !db.Migrator().HasTable(table) {
			t.Fatalf("mysql integration database is not migrated: missing %s", table)
		}
	}

	tx := db.Begin()
	if tx.Error != nil {
		t.Fatalf("begin transaction: %v", tx.Error)
	}
	defer tx.Rollback()

	now := timezone.Now()
	suffix := fmt.Sprintf("%d", now.UnixNano())
	userID := "cleanup-user-" + suffix
	agentSessionID := "Cleanup-Session-" + suffix
	lowercaseSessionID := strings.ToLower(agentSessionID)
	session := model.AgentSummarySession{
		SpaceID: "cleanup-space-" + suffix, UserID: userID, SessionID: "workspace-" + suffix,
		AgentSessionID: agentSessionID, ContractVersion: "1", State: "idle", StateVersion: 1,
		ScopeVersion: 1, ScopeJSON: "{}", CreatedAt: now, UpdatedAt: now,
	}
	if err := tx.Create(&session).Error; err != nil {
		t.Fatalf("create workspace session: %v", err)
	}
	old := now.Add(-cleanupAge - time.Hour)
	for _, evidence := range []model.AgentMessageEvidence{
		{UserID: userID, SessionID: agentSessionID, Handle: "exact", Evidence: "[]", CreatedAt: old, UpdatedAt: old},
		{UserID: userID, SessionID: lowercaseSessionID, Handle: "different-case", Evidence: "[]", CreatedAt: old, UpdatedAt: old},
	} {
		if err := tx.Create(&evidence).Error; err != nil {
			t.Fatalf("create evidence %s: %v", evidence.Handle, err)
		}
	}

	rows, err := cleanupExpiredAgentEvidence(tx, now.Add(-cleanupAge))
	if err != nil {
		t.Fatalf("cleanup evidence with mixed collations: %v", err)
	}
	if rows != 1 {
		t.Fatalf("deleted evidence rows = %d, want 1", rows)
	}
	// Do not count by session_id here: the legacy evidence column deliberately
	// uses a case-insensitive collation, so either spelling would match the one
	// retained row. The unique handles identify which row survived cleanup.
	var handles []string
	if err := tx.Model(&model.AgentMessageEvidence{}).
		Where("user_id = ?", userID).
		Order("handle ASC").
		Pluck("handle", &handles).Error; err != nil {
		t.Fatalf("list remaining evidence handles: %v", err)
	}
	if len(handles) != 1 || handles[0] != "exact" {
		t.Fatalf("remaining evidence handles = %v, want [exact]", handles)
	}
}
