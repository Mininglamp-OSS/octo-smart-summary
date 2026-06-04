package service

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

// Interval bounds. These guard against overflow / pathological values that
// would push next_run far into the future or, via overflow, into the past.
const (
	// MaxIntervalDays caps day/week intervals at ~10 years.
	MaxIntervalDays = 3650
	// MaxIntervalMonths caps month intervals at 10 years.
	MaxIntervalMonths = 120
)

// NextRun computes the next run time for a cron expression.
func NextRun(cronExpr string, from time.Time) (time.Time, error) {
	schedule, err := cron.ParseStandard(cronExpr)
	if err != nil {
		return time.Time{}, err
	}
	return schedule.Next(from), nil
}

// NextRunWithInterval computes the next run time. Scheduling sources are
// mutually exclusive and evaluated with a single, global priority order so
// that create/update/toggle all behave identically:
//
//  1. intervalMonths > 0  -> natural-month stepping via AddDate(0, n, 0),
//     which keeps the same day-of-month / time-of-day and respects variable
//     month lengths (no fixed-day approximation).
//  2. intervalDays   > 0  -> fixed N*24h interval (day = N*1, week = N*7).
//  3. otherwise           -> standard cron expression.
//
// For the two interval modes runTime ("HH:MM", UTC) anchors the time-of-day so
// the run hour stays stable regardless of when the scheduler actually fired.
// An empty runTime keeps the time-of-day of `from`. runTime is ignored for cron
// (the cron expression already encodes the time).
//
// Callers should enforce mutual exclusivity at the API boundary; this function
// only fixes the precedence so a stray field can never silently change meaning.
func NextRunWithInterval(cronExpr string, intervalDays int, intervalMonths int, runTime string, from time.Time) (time.Time, error) {
	if err := ValidateInterval(cronExpr, intervalDays, intervalMonths); err != nil {
		return time.Time{}, err
	}
	if intervalMonths > 0 {
		return applyRunTime(from.AddDate(0, intervalMonths, 0), runTime), nil
	}
	if intervalDays > 0 {
		return applyRunTime(from.Add(time.Duration(intervalDays)*24*time.Hour), runTime), nil
	}
	return NextRun(cronExpr, from)
}

// applyRunTime snaps t's hour/minute to runTime ("HH:MM"), zeroing seconds and
// below, in t's own location. Invalid/empty runTime returns t unchanged.
func applyRunTime(t time.Time, runTime string) time.Time {
	h, m, ok := parseRunTime(runTime)
	if !ok {
		return t
	}
	return time.Date(t.Year(), t.Month(), t.Day(), h, m, 0, 0, t.Location())
}

// parseRunTime parses an "HH:MM" (24h) string. Returns ok=false on any error.
func parseRunTime(runTime string) (hour, minute int, ok bool) {
	if runTime == "" {
		return 0, 0, false
	}
	var h, m int
	if _, err := fmt.Sscanf(runTime, "%d:%d", &h, &m); err != nil {
		return 0, 0, false
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// ValidateInterval enforces bounds and exactly-one-source semantics for a
// schedule's recurrence definition. It is the single source of truth used by
// create, update and toggle paths.
func ValidateInterval(cronExpr string, intervalDays int, intervalMonths int) error {
	if intervalDays < 0 {
		return fmt.Errorf("interval_days 不能为负")
	}
	if intervalMonths < 0 {
		return fmt.Errorf("interval_months 不能为负")
	}
	if intervalDays > MaxIntervalDays {
		return fmt.Errorf("interval_days 超出上限 %d", MaxIntervalDays)
	}
	if intervalMonths > MaxIntervalMonths {
		return fmt.Errorf("interval_months 超出上限 %d", MaxIntervalMonths)
	}
	// Mutual exclusivity: at most one recurrence source may be active.
	active := 0
	if intervalMonths > 0 {
		active++
	}
	if intervalDays > 0 {
		active++
	}
	if cronExpr != "" {
		active++
	}
	if active == 0 {
		return fmt.Errorf("cron_expr / interval_days / interval_months 至少提供一个")
	}
	if active > 1 {
		return fmt.Errorf("cron_expr / interval_days / interval_months 互斥, 只能提供一个")
	}
	return nil
}

// ComputeTimeRange returns (start, end) based on time_range_type.
func ComputeTimeRange(rangeType int, now time.Time) (time.Time, time.Time) {
	end := now
	var start time.Time
	switch rangeType {
	case 1:
		start = now.Add(-24 * time.Hour)
	case 2:
		start = now.Add(-7 * 24 * time.Hour)
	case 3:
		start = now.Add(-30 * 24 * time.Hour)
	default: // type 4 — since last run, fallback to 24h
		start = now.Add(-24 * time.Hour)
	}
	return start, end
}
