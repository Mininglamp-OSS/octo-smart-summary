package service

import (
	"time"

	"github.com/robfig/cron/v3"
)

// NextRun computes the next run time for a cron expression.
func NextRun(cronExpr string, from time.Time) (time.Time, error) {
	schedule, err := cron.ParseStandard(cronExpr)
	if err != nil {
		return time.Time{}, err
	}
	return schedule.Next(from), nil
}

// NextRunWithInterval computes the next run time supporting both fixed
// interval-day scheduling (intervalDays > 0) and standard cron (intervalDays == 0).
func NextRunWithInterval(cronExpr string, intervalDays int, from time.Time) (time.Time, error) {
	if intervalDays > 0 {
		return from.Add(time.Duration(intervalDays) * 24 * time.Hour), nil
	}
	return NextRun(cronExpr, from)
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
