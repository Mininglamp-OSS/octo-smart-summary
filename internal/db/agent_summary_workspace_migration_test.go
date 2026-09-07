package db

import (
	"strings"
	"testing"

	migrationsql "github.com/Mininglamp-OSS/octo-smart-summary/migrations/sql"
)

func TestAgentSummaryWorkspaceMigrationDefinesDurableContract(t *testing.T) {
	compatRaw, err := migrationsql.FS.ReadFile("20260827-01-agent-summary-workspace.sql")
	if err != nil {
		t.Fatalf("read compatibility migration: %v", err)
	}
	if !strings.Contains(string(compatRaw), "Compatibility marker") {
		t.Fatal("workspace migration ID must remain as a compatibility marker")
	}

	messageRaw, err := migrationsql.FS.ReadFile("20260827-02-agent-summary-message-columns.sql")
	if err != nil {
		t.Fatalf("read message migration: %v", err)
	}
	messageSQL := string(messageRaw)
	for _, required := range []string{
		"ALTER TABLE `agent_message`",
		"ADD COLUMN `space_id`",
		"ADD KEY `idx_agent_message_workspace_created`",
		"ALGORITHM=INPLACE",
		"LOCK=NONE",
		"information_schema.COLUMNS",
	} {
		if !strings.Contains(messageSQL, required) {
			t.Fatalf("message migration is missing %q", required)
		}
	}
	if strings.Contains(messageSQL, "MODIFY COLUMN `session_id`") {
		t.Fatal("message migration must not rebuild the hot table to change session_id collation")
	}

	sessionRaw, err := migrationsql.FS.ReadFile("20260827-03-agent-summary-session.sql")
	if err != nil {
		t.Fatalf("read session migration: %v", err)
	}
	sessionSQL := string(sessionRaw)
	for _, required := range []string{
		"CREATE TABLE IF NOT EXISTS `agent_summary_session`",
		"UNIQUE KEY `uk_agent_summary_session` (`space_id`, `user_id`, `session_id`)",
		"KEY `idx_agent_summary_session_agent_identity` (`user_id`, `agent_session_id`)",
		"`agent_session_id` VARCHAR(80)",
		"`expires_at` DATETIME(3) NULL",
		"COLLATE utf8mb4_0900_bin",
	} {
		if !strings.Contains(sessionSQL, required) {
			t.Fatalf("session migration is missing %q", required)
		}
	}

	turnRaw, err := migrationsql.FS.ReadFile("20260827-04-agent-summary-turn.sql")
	if err != nil {
		t.Fatalf("read turn migration: %v", err)
	}
	turnSQL := string(turnRaw)
	for _, required := range []string{
		"CREATE TABLE IF NOT EXISTS `agent_summary_turn`",
		"UNIQUE KEY `uk_agent_summary_turn_request` (`space_id`, `user_id`, `session_id`, `request_id`)",
		"`response_json` JSON NULL",
		"KEY `idx_agent_summary_turn_owner_session` (`space_id`, `user_id`, `session_id`, `status`)",
		"COLLATE utf8mb4_0900_bin",
		"information_schema.STATISTICS",
	} {
		if !strings.Contains(turnSQL, required) {
			t.Fatalf("turn migration is missing %q", required)
		}
	}
}
