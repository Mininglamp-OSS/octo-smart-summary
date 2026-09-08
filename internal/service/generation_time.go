package service

import (
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
)

// ResolvedGenerationTime is frozen once at admission. Adapters must preserve
// the half-open boundary when translating to a retrieval API.
type ResolvedGenerationTime struct {
	EffectiveAt      time.Time `json:"effective_at"`
	Start            time.Time `json:"start"`
	End              time.Time `json:"end"`
	Boundary         string    `json:"boundary"`
	DataGapTruncated bool      `json:"data_gap_truncated"`
}

// ResolveGenerationTime never reads the wall clock or schedule.last_run_at.
// successfulEnd must come from an applied successful full generation, not
// admission, a skipped slot, a refinement, or an unapplied candidate.
func ResolveGenerationTime(selector model.SummaryTimeSelector, effectiveAt time.Time, successfulEnd *time.Time, maxWindowDays int) (ResolvedGenerationTime, error) {
	out := ResolvedGenerationTime{EffectiveAt: timezone.In(effectiveAt), Boundary: "[start,end)"}
	if selector.Timezone != timezone.Name {
		return out, contentError("unsupported_timezone", 422)
	}
	if effectiveAt.IsZero() || maxWindowDays < 1 || maxWindowDays > MaxIntervalDays {
		return out, contentError("invalid_time_selector", 400)
	}
	parse := func(value string) (time.Time, error) {
		t, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return time.Time{}, contentError("invalid_time_selector", 400)
		}
		return timezone.In(t), nil
	}
	var err error
	switch selector.Mode {
	case "absolute":
		if selector.Days != 0 || selector.Unit != "" || selector.Offset != 0 || selector.InitialStart != "" {
			return out, contentError("invalid_time_selector", 400)
		}
		out.Start, err = parse(selector.Start)
		if err != nil {
			return out, err
		}
		out.End, err = parse(selector.End)
	case "relative":
		if selector.Days <= 0 || selector.Days > MaxIntervalDays || selector.Start != "" || selector.End != "" ||
			selector.Unit != "" || selector.Offset != 0 || selector.InitialStart != "" {
			return out, contentError("invalid_time_selector", 400)
		}
		out.End = out.EffectiveAt
		out.Start = out.End.AddDate(0, 0, -selector.Days)
	case "natural_period":
		if selector.Offset >= 0 || selector.Offset < -MaxIntervalDays || selector.Days != 0 ||
			selector.Start != "" || selector.End != "" || selector.InitialStart != "" {
			return out, contentError("invalid_time_selector", 400)
		}
		anchor := time.Date(out.EffectiveAt.Year(), out.EffectiveAt.Month(), out.EffectiveAt.Day(), 0, 0, 0, 0, timezone.Location())
		switch selector.Unit {
		case "day":
			out.Start = anchor.AddDate(0, 0, selector.Offset)
			out.End = out.Start.AddDate(0, 0, 1)
		case "week":
			anchor = anchor.AddDate(0, 0, -(int(anchor.Weekday())+6)%7) // Monday boundary
			out.Start = anchor.AddDate(0, 0, selector.Offset*7)
			out.End = out.Start.AddDate(0, 0, 7)
		case "month":
			anchor = time.Date(anchor.Year(), anchor.Month(), 1, 0, 0, 0, 0, timezone.Location())
			out.Start = anchor.AddDate(0, selector.Offset, 0)
			out.End = out.Start.AddDate(0, 1, 0)
		case "year":
			anchor = time.Date(anchor.Year(), 1, 1, 0, 0, 0, 0, timezone.Location())
			out.Start = anchor.AddDate(selector.Offset, 0, 0)
			out.End = out.Start.AddDate(1, 0, 0)
		default:
			return out, contentError("unsupported_time_unit", 422)
		}
	case "incremental":
		if selector.Days != 0 || selector.Unit != "" || selector.Offset != 0 || selector.Start != "" || selector.End != "" {
			return out, contentError("invalid_time_selector", 400)
		}
		out.End = out.EffectiveAt
		if successfulEnd != nil && !successfulEnd.IsZero() {
			out.Start = timezone.In(*successfulEnd)
		} else if selector.InitialStart != "" {
			out.Start, err = parse(selector.InitialStart)
		} else {
			return out, contentError("initial_range_required", 422)
		}
		if err == nil {
			earliest := out.End.AddDate(0, 0, -maxWindowDays)
			if out.Start.Before(earliest) {
				out.Start, out.DataGapTruncated = earliest, true
			}
		}
	default:
		return out, contentError("unsupported_time_selector", 422)
	}
	if err != nil {
		return out, err
	}
	if out.Start.Year() < 1000 || out.End.Year() > 9999 || !out.Start.Before(out.End) {
		return out, contentError("invalid_time_range", 400)
	}
	if out.Start.Before(out.End.AddDate(0, 0, -maxWindowDays)) {
		return out, contentError("time_window_exceeded", 422)
	}
	return out, nil
}

// ResolveGenerationSlot coalesces missed periods to the last due occurrence
// without losing the original recurrence phase. Slot, not worker-start time,
// becomes effective_at and the unique (schedule_id, scheduled_for) identity.
func ResolveGenerationSlot(schedule model.SummarySchedule, now time.Time) (slot, next time.Time, err error) {
	if schedule.NextRunAt == nil || schedule.NextRunAt.After(now) {
		return slot, next, contentError("schedule_not_due", 409)
	}
	slot = timezone.In(*schedule.NextRunAt)
	now = timezone.In(now)
	if err := ValidateInterval(schedule.CronExpr, schedule.IntervalDays, schedule.IntervalMonths); err != nil {
		return slot, next, err
	}
	for _, err := range []error{ValidateRunTime(schedule.RunTime), ValidateDayOfWeek(schedule.DayOfWeek),
		ValidateDayOfMonth(schedule.DayOfMonth), ValidateDayOfMonth(schedule.AnchorDOM)} {
		if err != nil {
			return slot, next, contentError("invalid_schedule_recurrence", 422)
		}
	}
	// Bound pathological historical cron/backlogs. The existing recurrence
	// validator accepts minute-level cron; excessive downtime requires repair,
	// never substituting the current time for an unproven schedule slot.
	for i := 0; i < 100000; i++ {
		if schedule.IntervalMonths > 0 {
			dom := monthlyTargetDom(schedule.AnchorDOM, timezone.In(*schedule.NextRunAt), schedule.DayOfMonth)
			next = stepMonthTo(slot, schedule.IntervalMonths, schedule.RunTime, dom)
		} else {
			next, err = NextRunWithInterval(schedule.CronExpr, schedule.IntervalDays, schedule.IntervalMonths,
				schedule.RunTime, schedule.DayOfWeek, schedule.DayOfMonth, slot)
		}
		if err != nil {
			return slot, next, err
		}
		if !next.After(slot) {
			return slot, next, contentError("invalid_schedule_recurrence", 422)
		}
		if next.After(now) {
			return slot, next, nil
		}
		slot = next
	}
	return slot, next, contentError("schedule_backlog_exceeded", 422)
}

// LegacyRollingSelector preserves the old rolling-day meaning. Type 4 still
// needs a trusted watermark/explicit initial range in ResolveGenerationTime.
func LegacyRollingSelector(timeRangeType int) (model.SummaryTimeSelector, error) {
	selector := model.SummaryTimeSelector{Timezone: timezone.Name, Mode: "relative"}
	switch timeRangeType {
	case 1:
		selector.Days = 1
	case 2:
		selector.Days = 7
	case 3:
		selector.Days = 30
	case 4:
		selector.Mode = "incremental"
	default:
		return selector, contentError("unsupported_time_selector", 422)
	}
	return selector, nil
}
