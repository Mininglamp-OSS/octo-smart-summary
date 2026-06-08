package sql

import (
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestPR62Round11_SummaryParticipantUniqueMigrationsAndSemantics(t *testing.T) {
	heal, err := FS.ReadFile("20260608-05-clean-duplicate-summary-participant.sql")
	if err != nil {
		t.Fatalf("read 05: %v", err)
	}
	healBody := string(heal)
	for _, want := range []string{
		"DELETE sp, pr, sc, ss",
		"GROUP BY task_id, user_id",
		"MIN(id) AS keep_id",
		"summary_personal_result",
		"summary_chunk",
		"summary_source",
	} {
		if !strings.Contains(healBody, want) {
			t.Fatalf("05 missing %q", want)
		}
	}

	addUnique, err := FS.ReadFile("20260608-06-add-unique-summary-participant-task-user.sql")
	if err != nil {
		t.Fatalf("read 06: %v", err)
	}
	addUniqueBody := string(addUnique)
	// MySQL (the production dialect, see internal/db/migrate.go) does NOT support
	// IF NOT EXISTS on CREATE INDEX; assert the plain MySQL-legal form matching the
	// existing 20260101-06 pattern. Re-run safety comes from sql-migrate's applied-
	// version tracking, not from IF NOT EXISTS.
	if !strings.Contains(addUniqueBody, "CREATE UNIQUE INDEX `uk_summary_participant_task_user` ON `summary_participant` (`task_id`, `user_id`)") {
		t.Fatalf("06 must create the unique index on (task_id, user_id) using MySQL-legal CREATE UNIQUE INDEX ... ON ...")
	}
	if strings.Contains(addUniqueBody, "IF NOT EXISTS") || strings.Contains(addUniqueBody, "IF EXISTS") {
		t.Fatalf("06 must not use IF NOT EXISTS / IF EXISTS: unsupported by MySQL CREATE/DROP INDEX")
	}

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	for _, ddl := range []string{
		`CREATE TABLE summary_participant (id INTEGER PRIMARY KEY, task_id INTEGER NOT NULL, user_id TEXT NOT NULL, personal_result_id INTEGER)`,
		`CREATE TABLE summary_personal_result (id INTEGER PRIMARY KEY, task_id INTEGER NOT NULL, participant_ref_id INTEGER NOT NULL)`,
		`CREATE TABLE summary_chunk (id INTEGER PRIMARY KEY, task_id INTEGER NOT NULL, participant_id INTEGER)`,
		`CREATE TABLE summary_source (id INTEGER PRIMARY KEY, task_id INTEGER NOT NULL, participant_id INTEGER)`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
	for _, seed := range []string{
		`INSERT INTO summary_participant (id, task_id, user_id) VALUES (10, 1, 'creator'), (11, 1, 'creator'), (12, 1, 'other')`,
		`INSERT INTO summary_personal_result (id, task_id, participant_ref_id) VALUES (100, 1, 10), (101, 1, 11), (102, 1, 12)`,
		`INSERT INTO summary_chunk (id, task_id, participant_id) VALUES (200, 1, 10), (201, 1, 11), (202, 1, 12)`,
		`INSERT INTO summary_source (id, task_id, participant_id) VALUES (300, 1, 10), (301, 1, 11), (302, 1, 12)`,
	} {
		if err := db.Exec(seed).Error; err != nil {
			t.Fatalf("seed rows: %v", err)
		}
	}

	for _, stmt := range []string{
		`DELETE FROM summary_personal_result
		 WHERE participant_ref_id IN (
			SELECT id FROM summary_participant
			WHERE id NOT IN (
				SELECT MIN(id) FROM summary_participant GROUP BY task_id, user_id
			)
		 )`,
		`DELETE FROM summary_chunk
		 WHERE participant_id IN (
			SELECT id FROM summary_participant
			WHERE id NOT IN (
				SELECT MIN(id) FROM summary_participant GROUP BY task_id, user_id
			)
		 )`,
		`DELETE FROM summary_source
		 WHERE participant_id IN (
			SELECT id FROM summary_participant
			WHERE id NOT IN (
				SELECT MIN(id) FROM summary_participant GROUP BY task_id, user_id
			)
		 )`,
		`DELETE FROM summary_participant
		 WHERE id NOT IN (
			SELECT MIN(id) FROM summary_participant GROUP BY task_id, user_id
		 )`,
		`CREATE UNIQUE INDEX uk_summary_participant_task_user ON summary_participant (task_id, user_id)`,
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("apply stmt: %v", err)
		}
	}

	type participantRow struct {
		ID     int
		TaskID int
		UserID string
	}
	var participants []participantRow
	if err := db.Raw(`SELECT id, task_id, user_id FROM summary_participant ORDER BY id`).Scan(&participants).Error; err != nil {
		t.Fatalf("load participants: %v", err)
	}
	if len(participants) != 2 {
		t.Fatalf("participant count=%d want 2", len(participants))
	}
	if participants[0].ID != 10 || participants[1].ID != 12 {
		t.Fatalf("participants after heal=%+v want keep ids 10 and 12", participants)
	}

	for _, tc := range []struct {
		table  string
		column string
		want   []int
	}{
		{table: "summary_personal_result", column: "participant_ref_id", want: []int{10, 12}},
		{table: "summary_chunk", column: "participant_id", want: []int{10, 12}},
		{table: "summary_source", column: "participant_id", want: []int{10, 12}},
	} {
		var got []int
		if err := db.Raw("SELECT " + tc.column + " FROM " + tc.table + " ORDER BY id").Scan(&got).Error; err != nil {
			t.Fatalf("load %s: %v", tc.table, err)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("%s count=%d want %d", tc.table, len(got), len(tc.want))
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Fatalf("%s[%d]=%d want %d", tc.table, i, got[i], tc.want[i])
			}
		}
	}

	if err := db.Exec(`INSERT INTO summary_participant (id, task_id, user_id) VALUES (13, 1, 'creator')`).Error; err == nil {
		t.Fatalf("duplicate (task_id, user_id) insert should fail after unique index")
	}
}
