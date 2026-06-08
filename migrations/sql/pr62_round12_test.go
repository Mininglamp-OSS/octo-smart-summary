package sql

import (
	"strings"
	"testing"

	migrate "github.com/rubenv/sql-migrate"
)

func TestPR62Round12_RepairMigrationUsesMySQLIdempotentDDL(t *testing.T) {
	bodyBytes, err := FS.ReadFile("20260608-07-heal-partial-ddl-state.sql")
	if err != nil {
		t.Fatalf("read 07: %v", err)
	}
	body := string(bodyBytes)

	for _, want := range []string{
		"information_schema.COLUMNS",
		"information_schema.STATISTICS",
		"PREPARE stmt FROM @pr62_r12_sql;",
		"EXECUTE stmt;",
		"DEALLOCATE PREPARE stmt;",
		"TABLE_NAME = 'summary_task'",
		"COLUMN_NAME = 'live_schedule_id'",
		"INDEX_NAME = 'uk_live_schedule_binding'",
		"TABLE_NAME = 'summary_schedule'",
		"COLUMN_NAME = 'anchor_dom'",
		"TABLE_NAME = 'summary_participant'",
		"INDEX_NAME = 'uk_summary_participant_task_user'",
		"ADD COLUMN live_schedule_id BIGINT",
		"ADD UNIQUE KEY uk_live_schedule_binding (live_schedule_id)",
		"ADD COLUMN anchor_dom TINYINT NOT NULL DEFAULT 0 AFTER day_of_month",
		"CREATE UNIQUE INDEX `uk_summary_participant_task_user` ON `summary_participant` (`task_id`, `user_id`)",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("07 missing %q", want)
		}
	}

	upper := strings.ToUpper(body)
	for _, banned := range []string{
		"ADD COLUMN IF NOT EXISTS",
		"ADD UNIQUE KEY IF NOT EXISTS",
		"CREATE UNIQUE INDEX IF NOT EXISTS",
		"CREATE INDEX IF NOT EXISTS",
		"DROP INDEX IF EXISTS",
	} {
		if strings.Contains(upper, banned) {
			t.Fatalf("07 must not use MySQL-unsupported conditional DDL %q", banned)
		}
	}
}

func TestPR62Round12_RepairMigrationSplitsIntoFlatStatements(t *testing.T) {
	bodyBytes, err := FS.ReadFile("20260608-07-heal-partial-ddl-state.sql")
	if err != nil {
		t.Fatalf("read 07: %v", err)
	}

	mig, err := migrate.ParseMigration("20260608-07-heal-partial-ddl-state.sql", strings.NewReader(string(bodyBytes)))
	if err != nil {
		t.Fatalf("parse 07: %v", err)
	}
	if len(mig.Up) != 20 {
		t.Fatalf("up statement count=%d want 20 (4 guarded objects x 5 flat statements)", len(mig.Up))
	}
	if len(mig.Down) != 1 {
		t.Fatalf("down statement count=%d want 1", len(mig.Down))
	}
	if mig.DisableTransactionUp || mig.DisableTransactionDown {
		t.Fatalf("07 should use default transaction mode")
	}

	for _, want := range []string{
		"SET @pr62_r12_has_live_schedule_id = (",
		"SET @pr62_r12_sql = IF(",
		"PREPARE stmt FROM @pr62_r12_sql;\n",
		"EXECUTE stmt;\n",
		"DEALLOCATE PREPARE stmt;\n",
		"SET @pr62_r12_has_anchor_dom = (",
		"SET @pr62_r12_has_uk_summary_participant_task_user = (",
	} {
		found := false
		for _, stmt := range mig.Up {
			if strings.Contains(stmt, want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("parsed up statements missing %q", want)
		}
	}
}
