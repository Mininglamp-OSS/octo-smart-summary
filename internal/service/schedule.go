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

// NextRunWithInterval computes the next run time using one global priority order
// (create/update/toggle all behave identically):
//  1. intervalMonths>0 -> month stepping, clamped to month end (Jan31+1mo -> Feb28/29).
//  2. intervalDays>0   -> fixed N*24h (day=N*1, week=N*7).
//  3. otherwise        -> cron.
//
// runTime ("HH:MM", Asia/Shanghai) anchors time-of-day for interval modes (empty keeps
// from's); dayOfWeek (1..7,0=unset) aligns week mode, dayOfMonth (1..31,0=unset) month mode.
// ADVANCE form: always steps at least one full interval past `from`.
func NextRunWithInterval(cronExpr string, intervalDays int, intervalMonths int, runTime string, dayOfWeek int, dayOfMonth int, from time.Time) (time.Time, error) {
	if err := ValidateInterval(cronExpr, intervalDays, intervalMonths); err != nil {
		return time.Time{}, err
	}
	if intervalMonths > 0 {
		next := applyRunTime(addMonthsClamped(from, intervalMonths), runTime)
		return alignDayOfMonth(next, dayOfMonth), nil
	}
	if intervalDays > 0 {
		next := applyRunTime(from.Add(time.Duration(intervalDays)*24*time.Hour), runTime)
		if intervalDays%7 == 0 {
			next = alignDayOfWeek(next, dayOfWeek)
		}
		return next, nil
	}
	return NextRun(cronExpr, from)
}

// NextRunScheduledAdvance computes next_run for the scheduler's post-run recompute.
// It advances from the schedule's ORIGINAL due time (`anchor`) by whole periods until
// strictly after `now`, preserving weekday/day-of-month phase. Anchoring (not `now`)
// keeps cadence when the scheduler fires late instead of skipping the missed occurrence.
func NextRunScheduledAdvance(cronExpr string, intervalDays int, intervalMonths int, runTime string, dayOfWeek int, dayOfMonth int, anchorDOM int, anchor time.Time, now time.Time) (time.Time, error) {
	if err := ValidateInterval(cronExpr, intervalDays, intervalMonths); err != nil {
		return time.Time{}, err
	}
	if intervalMonths == 0 && intervalDays == 0 {
		return NextRun(cronExpr, now)
	}

	const maxSteps = 4000 // ~>10y of downtime; each loop advances >=1 day
	if intervalMonths > 0 {
		targetDom := monthlyTargetDom(anchorDOM, anchor, dayOfMonth)
		next := anchor
		for i := 0; i < maxSteps; i++ {
			stepped := stepMonthTo(next, intervalMonths, runTime, targetDom)
			if !stepped.After(next) {
				return time.Time{}, fmt.Errorf("next_run did not advance from %s (interval_months=%d)", next, intervalMonths)
			}
			next = stepped
			if next.After(now) {
				return next, nil
			}
		}
		return time.Time{}, fmt.Errorf("next_run exceeded %d advance steps from anchor %s (now=%s)", maxSteps, anchor, now)
	}

	next := anchor
	for i := 0; i < maxSteps; i++ {
		stepped, err := NextRunWithInterval(cronExpr, intervalDays, intervalMonths, runTime, dayOfWeek, dayOfMonth, next)
		if err != nil {
			return time.Time{}, err
		}
		// Guard against a non-advancing step (would otherwise infinite-loop).
		if !stepped.After(next) {
			return time.Time{}, fmt.Errorf("next_run did not advance from %s (interval_days=%d interval_months=%d)", next, intervalDays, intervalMonths)
		}
		next = stepped
		if next.After(now) {
			return next, nil
		}
	}
	return time.Time{}, fmt.Errorf("next_run exceeded %d advance steps from anchor %s (now=%s)", maxSteps, anchor, now)
}

// NextRunInitial computes the FIRST next_run at create/update time. Unlike ADVANCE,
// if the selected time-of-day for the next valid day is still ahead of `from`, it fires
// that day rather than waiting a full interval (day/week/month each pick the nearest
// valid run_time today/this period, else advance). Cron unchanged. `from` is Asia/Shanghai.
func NextRunInitial(cronExpr string, intervalDays int, intervalMonths int, runTime string, dayOfWeek int, dayOfMonth int, from time.Time) (time.Time, error) {
	if err := ValidateInterval(cronExpr, intervalDays, intervalMonths); err != nil {
		return time.Time{}, err
	}

	// MONTH mode
	if intervalMonths > 0 {
		// Build this month's candidate at run_time, aligned to dayOfMonth (or
		// the current day-of-month if unset).
		base := applyRunTime(from, runTime)
		candidate := alignDayOfMonth(base, dayOfMonth)
		if candidate.After(from) {
			return candidate, nil
		}
		// Already passed today/this cycle -> advance one full month and align.
		next := applyRunTime(addMonthsClamped(from, intervalMonths), runTime)
		return alignDayOfMonth(next, dayOfMonth), nil
	}

	// DAY / WEEK mode
	if intervalDays > 0 {
		if intervalDays%7 == 0 && dayOfWeek >= 1 && dayOfWeek <= 7 {
			// Week mode with an explicit weekday: find the nearest run_time
			// occurrence on that weekday that is still in the future (today
			// counts when today is the weekday and the time hasn't passed).
			candidate := alignDayOfWeek(applyRunTime(from, runTime), dayOfWeek)
			// alignDayOfWeek moves forward to the weekday (could be today). If the
			// aligned candidate is today but the time already passed, advance by
			// the configured interval first, then re-align weekday. Using a fixed
			// +7d here breaks multi-week intervals (14/21/28...) by starting one
			// week too early and permanently skewing cadence.
			if !candidate.After(from) {
				base := applyRunTime(from.Add(time.Duration(intervalDays)*24*time.Hour), runTime)
				candidate = alignDayOfWeek(base, dayOfWeek)
			}
			return candidate, nil
		}
		// Day mode (or week mode without an explicit weekday): today's run_time
		// if still ahead, else advance N days.
		candidate := applyRunTime(from, runTime)
		if candidate.After(from) {
			return candidate, nil
		}
		return applyRunTime(from.Add(time.Duration(intervalDays)*24*time.Hour), runTime), nil
	}

	return NextRun(cronExpr, from)
}

// alignDayOfWeek snaps t forward to the next occurrence of the target ISO
// weekday (1=Mon..7=Sun), preserving t's time-of-day. dow<=0 or >7 means "no
// alignment" and returns t unchanged. If t is already on the target weekday it
// is returned unchanged (the caller decides whether "today" is acceptable).
func alignDayOfWeek(t time.Time, dow int) time.Time {
	if dow < 1 || dow > 7 {
		return t
	}
	// Go's Weekday: Sunday=0..Saturday=6. Convert to ISO 1=Mon..7=Sun.
	cur := int(t.Weekday())
	if cur == 0 {
		cur = 7
	}
	delta := (dow - cur + 7) % 7
	if delta == 0 {
		return t
	}
	return t.AddDate(0, 0, delta)
}

// alignDayOfMonth snaps t to the given day-of-month within t's own month,
// clamping to the last day of the month when dom exceeds the month length
// (e.g. dom=31 in February -> 28/29). dom<=0 or >31 means "no alignment".
func alignDayOfMonth(t time.Time, dom int) time.Time {
	if dom < 1 || dom > 31 {
		return t
	}
	last := daysInMonth(t.Year(), int(t.Month()))
	day := dom
	if day > last {
		day = last
	}
	return time.Date(t.Year(), t.Month(), day, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
}

// monthlyTargetDom resolves the stable target day-of-month for month stepping.
// Only explicit monthly intent may upgrade to "month end": dayOfMonth/anchorDOM
// in [1,31] win. Legacy rows with anchorDOM<=0 are treated conservatively and
// keep anchoring on the current anchor's own day, even if that day happens to
// be the month's last day due to a previous clamp. This prevents unknown legacy
// rows clamped to Feb 28 / Apr 30 from being mis-upgraded into permanent
// month-end schedules.
func ResolveAnchorDOM(dayOfMonth int, from time.Time) int {
	if dayOfMonth >= 1 && dayOfMonth <= 31 {
		return dayOfMonth
	}
	return from.Day()
}

// EffectiveMonthlyDOM resolves the day-of-month that initial/recompute paths
// should pass into NextRunInitial/NextRunWithInterval for monthly schedules.
// Explicit DOM wins; otherwise a trusted persisted anchorDOM restores the
// original monthly intent for rows that store day_of_month=0.
func EffectiveMonthlyDOM(dayOfMonth int, anchorDOM int) int {
	if dayOfMonth == 0 && anchorDOM >= 1 && anchorDOM <= 31 {
		return anchorDOM
	}
	return dayOfMonth
}

func monthlyTargetDom(anchorDOM int, anchor time.Time, dayOfMonth int) int {
	if dayOfMonth >= 1 && dayOfMonth <= 31 {
		return dayOfMonth
	}
	if anchorDOM >= 1 && anchorDOM <= 31 {
		return anchorDOM
	}
	return anchor.Day()
}

// stepMonthTo advances `from` by n months, snaps to targetDom (clamped to month length),
// then applies runTime. The day is computed relative to targetDom every call, so a clamped
// day is never carried forward (Jan31,targetDom=31 -> Feb28/29 -> Mar31 -> Apr30).
func stepMonthTo(from time.Time, n int, runTime string, targetDom int) time.Time {
	moved := addMonthsClamped(from, n)
	aligned := alignDayOfMonth(moved, targetDom)
	return applyRunTime(aligned, runTime)
}

// addMonthsClamped advances t by n calendar months, clamping a non-existent day to the
// target month's last day (Jan31+1mo -> Feb28/29, not Go's Mar3 overflow). Handles leap
// February and December year-wrap.
func addMonthsClamped(t time.Time, n int) time.Time {
	naive := t.AddDate(0, n, 0)

	// Expected target month if no overflow occurred.
	targetYear, targetMonth := normalizeYearMonth(t.Year(), int(t.Month())+n)

	if naive.Year() == targetYear && int(naive.Month()) == targetMonth {
		// Day-of-month existed in the target month; no clamping needed.
		return naive
	}

	// Overflow: target month had no such day. Clamp to last day of target month,
	// keeping the original time-of-day.
	lastDay := daysInMonth(targetYear, targetMonth)
	return time.Date(targetYear, time.Month(targetMonth), lastDay,
		t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
}

// normalizeYearMonth converts a possibly out-of-range month (1-based, may be
// >12) into a normalized (year, month 1..12) pair.
func normalizeYearMonth(year, month int) (int, int) {
	// month is 1-based; convert to 0-based for modular arithmetic.
	m0 := month - 1
	year += m0 / 12
	m0 %= 12
	if m0 < 0 {
		m0 += 12
		year--
	}
	return year, m0 + 1
}

// daysInMonth returns the number of days in the given month (1..12) of year.
func daysInMonth(year, month int) int {
	// Day 0 of the next month is the last day of this month.
	return time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day()
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

// ValidateRunTime enforces a strict "HH:MM" 24h format (00:00..23:59). An empty
// string is accepted (means "keep base time-of-day"). Any other malformed value
// is rejected so the API never silently falls back to the trigger instant.
func ValidateRunTime(runTime string) error {
	if runTime == "" {
		return nil
	}
	// Must be exactly HH:MM with a colon at index 2 and digits elsewhere.
	if len(runTime) != 5 || runTime[2] != ':' {
		return fmt.Errorf("run_time 必须为 HH:MM 格式")
	}
	for i := 0; i < 5; i++ {
		if i == 2 {
			continue
		}
		if runTime[i] < '0' || runTime[i] > '9' {
			return fmt.Errorf("run_time 必须为 HH:MM 格式")
		}
	}
	h, m, ok := parseRunTime(runTime)
	if !ok || h < 0 || h > 23 || m < 0 || m > 59 {
		return fmt.Errorf("run_time 超出范围 (00:00..23:59)")
	}
	return nil
}

// ValidateDayOfWeek accepts 0 (unset / no constraint) or 1..7 (Mon..Sun).
func ValidateDayOfWeek(dow int) error {
	if dow == 0 {
		return nil
	}
	if dow < 1 || dow > 7 {
		return fmt.Errorf("day_of_week 必须为 1..7 (周一..周日) 或 0(不限)")
	}
	return nil
}

// ValidateDayOfMonth accepts 0 (unset / no constraint) or 1..31. Days beyond a
// given month's length are clamped at runtime (see alignDayOfMonth).
func ValidateDayOfMonth(dom int) error {
	if dom == 0 {
		return nil
	}
	if dom < 1 || dom > 31 {
		return fmt.Errorf("day_of_month 必须为 1..31 或 0(不限)")
	}
	return nil
}

func ValidateTimeRangeType(rangeType int) error {
	if rangeType >= 1 && rangeType <= 4 {
		return nil
	}
	return fmt.Errorf("time_range_type 必须为 1..4")
}

// ValidateIntervalForWrite is the stricter create/update gate. Cron is now a
// legacy, read+execute-only mode: the public write contract is interval-only
// (exactly one of day/week via interval_days, or month via interval_months).
// New cron writes are rejected so interval becomes the single outward-facing
// vocabulary, while ValidateInterval (used by the scheduler / NextRunWithInterval)
// stays cron-tolerant so existing legacy cron schedules keep executing.
func ValidateIntervalForWrite(cronExpr string, intervalDays int, intervalMonths int) error {
	if cronExpr != "" {
		return fmt.Errorf("不再支持新建/修改为自定义 cron 模式, 请选择间隔(天/周/月)")
	}
	if err := ValidateInterval(cronExpr, intervalDays, intervalMonths); err != nil {
		return err
	}
	// After the cron guard, exactly one interval source must be present.
	if intervalDays == 0 && intervalMonths == 0 {
		return fmt.Errorf("必须提供 interval_days 或 interval_months 其一(天/周/月)")
	}
	return nil
}

// ValidateScheduleAnchors enforces that weekday/month-day selectors match the
// active recurrence mode instead of being silently ignored.
func ValidateScheduleAnchors(cronExpr string, intervalDays int, intervalMonths int, dayOfWeek int, dayOfMonth int) error {
	if cronExpr != "" {
		if dayOfWeek != 0 || dayOfMonth != 0 {
			return fmt.Errorf("cron 模式不支持 day_of_week/day_of_month")
		}
		return nil
	}
	if intervalMonths > 0 {
		if dayOfWeek != 0 {
			return fmt.Errorf("月模式不支持 day_of_week")
		}
		return nil
	}
	if intervalDays > 0 {
		if dayOfMonth != 0 {
			return fmt.Errorf("天/周模式不支持 day_of_month")
		}
		if intervalDays%7 != 0 && dayOfWeek != 0 {
			return fmt.Errorf("仅周模式(interval_days 为 7 的倍数)支持 day_of_week")
		}
		return nil
	}
	return nil
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

func cadenceWindowStart(now time.Time, lastRunAt *time.Time, cronExpr string, intervalDays int, intervalMonths int) (time.Time, error) {
	if lastRunAt != nil {
		return *lastRunAt, nil
	}
	switch {
	case intervalMonths > 0:
		return addMonthsClamped(now, -intervalMonths), nil
	case intervalDays > 0:
		return now.Add(-time.Duration(intervalDays) * 24 * time.Hour), nil
	case cronExpr != "":
		return now.Add(-24 * time.Hour), nil
	default:
		return time.Time{}, fmt.Errorf("invalid recurrence for time_range_type=4")
	}
}

// ComputeTimeRange returns (start, end) based on time_range_type.
func ComputeTimeRange(rangeType int, now time.Time, lastRunAt *time.Time, cronExpr string, intervalDays int, intervalMonths int) (time.Time, time.Time, error) {
	end := now
	var start time.Time
	switch rangeType {
	case 1:
		start = now.Add(-24 * time.Hour)
	case 2:
		start = now.Add(-7 * 24 * time.Hour)
	case 3:
		start = now.Add(-30 * 24 * time.Hour)
	case 4:
		var err error
		start, err = cadenceWindowStart(now, lastRunAt, cronExpr, intervalDays, intervalMonths)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
	default:
		return time.Time{}, time.Time{}, fmt.Errorf("unsupported time_range_type=%d", rangeType)
	}
	return start, end, nil
}
