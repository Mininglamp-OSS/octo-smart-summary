package sql

import (
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// TestCleanDirtySchedules_OverboundMonthEndsSingleSource is a Bug3 (PR#62 r4
// Jerry-Xin) logic test. It pins the STEP ORDER of
// 20260604-03-clean-dirty-schedules-backfill-runtime.sql: normalization
// (clamp/zero, 2a/2b) must run BEFORE the month>day>cron precedence cleanup
// (1a/1b). Otherwise an over-bound month row like
// (interval_months=999, interval_days=1) slips past 1a's `interval_months <= 120`
// guard, then 2b clamps 999 -> 120, leaving a double-source row
// (interval_months=120 AND interval_days=1) the scheduler treats as invalid.
//
// We run the portable interval-affecting UPDATEs from the migration file (in the
// exact order they appear) against sqlite and assert the dirty 999,1 row ends up
// clean single-source: interval_months=120, interval_days=0, cron_expr=''.
func TestCleanDirtySchedules_OverboundMonthEndsSingleSource(t *testing.T) {
	raw, err := FS.ReadFile("20260604-03-clean-dirty-schedules-backfill-runtime.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	// Take only the Up section.
	body := string(raw)
	if idx := strings.Index(body, "-- +migrate Down"); idx >= 0 {
		body = body[:idx]
	}

	// Extract the interval-affecting UPDATE statements in document order. We
	// deliberately skip the run_time backfill (it uses MySQL DATE_FORMAT which
	// sqlite does not implement); Bug3 is purely about the interval cleanup
	// ordering, so those statements are the ones under test.
	stmts := extractIntervalUpdates(body)
	if len(stmts) < 4 {
		t.Fatalf("expected >=4 interval update statements, got %d: %#v", len(stmts), stmts)
	}

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}

	if err := db.Exec(`CREATE TABLE summary_schedule (
		id INTEGER PRIMARY KEY,
		interval_days INTEGER NOT NULL DEFAULT 0,
		interval_months INTEGER NOT NULL DEFAULT 0,
		cron_expr TEXT NOT NULL DEFAULT '',
		run_time TEXT,
		next_run_at TEXT
	)`).Error; err != nil {
		t.Fatalf("create table: %v", err)
	}

	// Seed the exact dirty row Jerry-Xin called out, plus a couple of friends.
	seed := []struct {
		id     int
		days   int
		months int
		cron   string
	}{
		{1, 1, 999, ""},      // over-bound month + day -> month wins, single source
		{2, 9999, 0, ""},     // over-bound day only -> clamp to 3650, still single
		{3, 3, 0, "0 9 * *"}, // day + cron -> day wins, cron cleared
		{4, 0, 5, "0 9 * *"}, // month + cron -> month wins, cron cleared
	}
	for _, s := range seed {
		if err := db.Exec(
			`INSERT INTO summary_schedule (id, interval_days, interval_months, cron_expr) VALUES (?,?,?,?)`,
			s.id, s.days, s.months, s.cron).Error; err != nil {
			t.Fatalf("seed %d: %v", s.id, err)
		}
	}

	for i, stmt := range stmts {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("exec stmt %d (%q): %v", i, stmt, err)
		}
	}

	check := func(id, wantDays, wantMonths int, wantCron string) {
		var row struct {
			IntervalDays   int
			IntervalMonths int
			CronExpr       string
		}
		if err := db.Raw(
			`SELECT interval_days, interval_months, cron_expr FROM summary_schedule WHERE id=?`, id,
		).Scan(&row).Error; err != nil {
			t.Fatalf("query id %d: %v", id, err)
		}
		d, m, cron := row.IntervalDays, row.IntervalMonths, row.CronExpr
		if d != wantDays || m != wantMonths || cron != wantCron {
			t.Fatalf("id %d: got days=%d months=%d cron=%q; want days=%d months=%d cron=%q",
				id, d, m, cron, wantDays, wantMonths, wantCron)
		}
		// Single-source invariant: exactly one of (days>0, months>0, cron!='').
		active := 0
		if d > 0 {
			active++
		}
		if m > 0 {
			active++
		}
		if cron != "" {
			active++
		}
		if active != 1 {
			t.Fatalf("id %d not single-source: days=%d months=%d cron=%q (active=%d)", id, d, m, cron, active)
		}
	}

	// The headline Bug3 case: 999,1 -> month wins, clamped to 120, day cleared.
	check(1, 0, 120, "")
	check(2, 3650, 0, "")
	check(3, 3, 0, "")
	check(4, 0, 5, "")
}

// extractIntervalUpdates pulls the interval-affecting UPDATE statements (those
// touching interval_days / interval_months / cron_expr but NOT run_time) from
// the migration body, preserving document order. Each returned string is one
// executable statement (trailing semicolon stripped).
func extractIntervalUpdates(body string) []string {
	var out []string
	for _, chunk := range strings.Split(body, ";") {
		// Drop SQL comments line-by-line.
		var lines []string
		for _, ln := range strings.Split(chunk, "\n") {
			trimmed := strings.TrimSpace(ln)
			if strings.HasPrefix(trimmed, "--") || trimmed == "" {
				continue
			}
			lines = append(lines, ln)
		}
		stmt := strings.TrimSpace(strings.Join(lines, "\n"))
		if stmt == "" {
			continue
		}
		upper := strings.ToUpper(stmt)
		if !strings.HasPrefix(upper, "UPDATE") {
			continue
		}
		// Only the interval-cleanup statements; skip run_time backfill (MySQL
		// DATE_FORMAT, not portable / not relevant to Bug3 ordering).
		if strings.Contains(upper, "RUN_TIME") {
			continue
		}
		out = append(out, stmt)
	}
	return out
}
