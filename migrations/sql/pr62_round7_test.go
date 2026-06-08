package sql

import (
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// PR#62 r7 Blocker1: the unique-binding migration self-heals double-bound rows
// BEFORE ADD UNIQUE so it never fails on historical dirty data. The MySQL UPDATE
// uses a JOIN+derived table (not portable to sqlite), so we pin the cleanup
// SEMANTICS with the equivalent portable statement and assert the file actually
// performs the cleanup ahead of ADD UNIQUE.

func TestPR62Round7_UniqueBinding_SelfHealKeepsOneRow(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.Exec(`CREATE TABLE summary_task (
		id INTEGER PRIMARY KEY,
		schedule_id INTEGER,
		deleted_at DATETIME
	)`).Error; err != nil {
		t.Fatalf("create table: %v", err)
	}
	// schedule 1: triple-bound (ids 1,2,3) -> keep min id 1.
	// schedule 2: single-bound (id 4) -> untouched.
	// schedule 3: double-bound but one soft-deleted (id 6 dead) -> already single live.
	db.Exec(`INSERT INTO summary_task (id, schedule_id, deleted_at) VALUES
		(1, 1, NULL), (2, 1, NULL), (3, 1, NULL),
		(4, 2, NULL),
		(5, 3, NULL), (6, 3, '2026-01-01')`)

	// Portable equivalent of the migration's keep-min-id unbind.
	heal := `UPDATE summary_task SET schedule_id = NULL
		WHERE deleted_at IS NULL AND schedule_id IS NOT NULL
		  AND id NOT IN (
			SELECT MIN(id) FROM summary_task
			WHERE deleted_at IS NULL AND schedule_id IS NOT NULL
			GROUP BY schedule_id
		  )`
	if err := db.Exec(heal).Error; err != nil {
		t.Fatalf("heal: %v", err)
	}
	// Idempotent: second run is a no-op.
	if err := db.Exec(heal).Error; err != nil {
		t.Fatalf("heal rerun: %v", err)
	}

	assertLive := func(scheduleID int64, want int64) {
		var n int64
		db.Raw(`SELECT COUNT(*) FROM summary_task WHERE deleted_at IS NULL AND schedule_id = ?`, scheduleID).Scan(&n)
		if n != want {
			t.Fatalf("schedule %d: live bound=%d want %d", scheduleID, n, want)
		}
	}
	assertLive(1, 1)
	assertLive(2, 1)
	assertLive(3, 1)
	// The kept row for schedule 1 is the min id.
	var kept int64
	db.Raw(`SELECT id FROM summary_task WHERE deleted_at IS NULL AND schedule_id = 1`).Scan(&kept)
	if kept != 1 {
		t.Fatalf("schedule 1 kept id=%d want 1 (min)", kept)
	}

	// File assertion: the unbind UPDATE precedes ADD UNIQUE.
	raw, err := FS.ReadFile("20260608-01-unique-live-schedule-binding.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	body := string(raw)
	updIdx := strings.Index(body, "UPDATE summary_task")
	uniqIdx := strings.Index(body, "ADD UNIQUE KEY uk_live_schedule_binding")
	if updIdx < 0 || uniqIdx < 0 || updIdx > uniqIdx {
		t.Fatalf("cleanup UPDATE must precede ADD UNIQUE (upd=%d uniq=%d)", updIdx, uniqIdx)
	}
}

// PR#62 r7 Blocker3: clean dirty participant_config. The MySQL migration uses
// JSON_TABLE (not portable); we pin the dirty-detection口径 (a non-empty user_id
// != creator_id makes the row dirty) and assert the file targets the right
// table/column.
func TestPR62Round7_CleanParticipantConfig_Predicate(t *testing.T) {
	creator := "creator1"
	cases := []struct {
		name      string
		cfg       []map[string]string
		wantDirty bool
	}{
		{"nil/empty", nil, false},
		{"creator only", []map[string]string{{"user_id": creator}}, false},
		{"empty user_id only", []map[string]string{{"user_id": ""}}, false},
		{"creator + empty", []map[string]string{{"user_id": creator}, {"user_id": ""}}, false},
		{"stranger", []map[string]string{{"user_id": "stranger"}}, true},
		{"creator + stranger", []map[string]string{{"user_id": creator}, {"user_id": "x"}}, true},
	}
	isDirty := func(cfg []map[string]string) bool {
		for _, m := range cfg {
			uid := m["user_id"]
			if uid != "" && uid != creator {
				return true
			}
		}
		return false
	}
	for _, tc := range cases {
		if got := isDirty(tc.cfg); got != tc.wantDirty {
			t.Fatalf("%s: dirty=%v want %v", tc.name, got, tc.wantDirty)
		}
	}

	raw, err := FS.ReadFile("20260608-02-clean-dirty-participant-config.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	body := string(raw)
	for _, want := range []string{"UPDATE summary_schedule", "participant_config = NULL", "creator_id", "JSON_TABLE"} {
		if !strings.Contains(body, want) {
			t.Fatalf("migration missing %q", want)
		}
	}
}
