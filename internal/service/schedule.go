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
//  1. intervalMonths > 0  -> natural-month stepping via addMonthsClamped,
//     which keeps the same day-of-month when it exists and otherwise clamps to
//     the last day of the target month (e.g. Jan 31 + 1 month -> Feb 28/29
//     instead of Go's default Mar 3 overflow), respecting variable month
//     lengths (no fixed-day approximation).
//  2. intervalDays   > 0  -> fixed N*24h interval (day = N*1, week = N*7).
//  3. otherwise           -> standard cron expression.
//
// For the two interval modes runTime ("HH:MM", Asia/Shanghai 北京时间) anchors
// the time-of-day so the run hour stays stable regardless of when the scheduler
// actually fired. An empty runTime keeps the time-of-day of `from`. runTime is
// ignored for cron (the cron expression already encodes the time). `from` is
// expected to be in Asia/Shanghai (callers pass timezone.Now()), so all derived
// times stay in 北京时间.
//
// dayOfWeek (1=Mon..7=Sun, 0=unset) aligns the WEEK mode (intervalDays multiple
// of 7) to a specific weekday; dayOfMonth (1..31, 0=unset) aligns the MONTH mode
// to a specific day-of-month (clamped to month end). Both are ignored when 0 or
// not applicable to the active mode.
//
// This is the ADVANCE form: it always steps at least one full interval into the
// future relative to `from`, then snaps to the requested weekday / day-of-month.
// Use it for the scheduler's post-run recompute and for toggle re-enable.
//
// Callers should enforce mutual exclusivity at the API boundary; this function
// only fixes the precedence so a stray field can never silently change meaning.
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

// NextRunScheduledAdvance computes the next_run for the scheduler's post-run
// recompute. Unlike NextRunWithInterval (which steps a single interval from
// `from`), this advances from the schedule's ORIGINAL due time (`anchor`,
// i.e. *sched.NextRunAt) by whole periods until the result is strictly after
// `now`, preserving the weekday / day-of-month phase.
//
// Why anchor, not now: if the scheduler fires late (e.g. a weekly Monday 09:00
// schedule is only scanned Tuesday 10:00) computing from `now` would jump to
// the week AFTER next and silently skip the just-missed Monday. Anchoring on
// the original due time keeps the cadence: from Monday 09:00 we step +1 week to
// next Monday 09:00, which is the correct nearest future occurrence.
//
// For interval modes a single full step is normally enough to pass `now`; the
// loop only iterates more when the scheduler was down for multiple periods. We
// cap iterations defensively so a pathological anchor far in the past cannot
// spin forever. Cron mode is unchanged (cron.Next already returns the next
// occurrence strictly after its argument, so we feed it `now`).
func NextRunScheduledAdvance(cronExpr string, intervalDays int, intervalMonths int, runTime string, dayOfWeek int, dayOfMonth int, anchor time.Time, now time.Time) (time.Time, error) {
	if err := ValidateInterval(cronExpr, intervalDays, intervalMonths); err != nil {
		return time.Time{}, err
	}
	// Cron mode: cron.Next is inherently "strictly after" semantics; anchoring
	// is irrelevant because the cron expression fully defines the cadence.
	if intervalMonths == 0 && intervalDays == 0 {
		return NextRun(cronExpr, now)
	}

	// Defensive cap: at most ~ (10 years / shortest period) steps. Each loop
	// advances at least one day, so 4000 iterations covers >10y of downtime.
	const maxSteps = 4000
	if intervalMonths > 0 {
		// Bug2 (PR#62 yujiawei r4): step whole months while keeping the target
		// day-of-month anchored to the ORIGINAL anchor's day-of-month, not the
		// clamped result of the previous step. See monthlyTargetDom + stepMonthTo
		// for why feeding the clamped day back (the old behaviour) made the
		// day-of-month monotonically decrease and never recover.
		targetDom := monthlyTargetDom(anchor, dayOfMonth)
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

// NextRunInitial computes the FIRST next_run at create/update time. Unlike the
// ADVANCE form, if the user-selected time-of-day for the next valid day is still
// ahead of `from` (now), it fires that day rather than waiting a full interval.
// Concretely:
//
//   - Day mode (intervalDays not a multiple of 7, no weekday): candidate =
//     today's run_time. If candidate > now -> run today; else advance N days.
//   - Week mode (intervalDays multiple of 7): candidate = the next occurrence of
//     dayOfWeek (or today's run_time if no weekday selected) at run_time. If
//     that candidate (today, when today matches the weekday) is still ahead of
//     now -> use it; otherwise the next matching weekday.
//   - Month mode: candidate = this month's dayOfMonth (or today's day if unset)
//     at run_time. If candidate > now -> this month; otherwise next month.
//   - Cron mode: unchanged (cron already encodes the schedule).
//
// `from` must be in Asia/Shanghai.
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
			// aligned candidate is today but the time already passed, push a week.
			if !candidate.After(from) {
				candidate = candidate.Add(7 * 24 * time.Hour)
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

// monthlyTargetDom (PR#62 r4 Bug2) resolves the stable "target day-of-month"
// used by month-mode stepping. The scheduler overwrites next_run_at with each
// computed (possibly clamped) result, so across cycles the `anchor` passed in is
// the PREVIOUS step's output, not the original creation day. The target dom must
// therefore be reconstructable from the (drifting) anchor alone and still
// recover the intended day in long months.
//
//   - dayOfMonth in 1..31 (explicitly selected): that is the target, always.
//     alignDayOfMonth clamps it to month end in short months but the target
//     itself never drifts, so 1/31 -> 2/28 -> 3/31 recovers.
//   - dayOfMonth == 0 ("不限" / unset) AND the anchor is the LAST day of its
//     month: use end-of-month semantics (target 31). This is the case that was
//     monotonically decreasing: a Jan-31 start clamps to Feb-28, and Feb-28 is
//     itself the last day of February, so we keep targeting month-end and
//     recover to Mar-31, Apr-30, May-31, ... The detection is self-consistent
//     under next_run_at drift: every clamped month-end result is again a
//     month-end day, so each subsequent cycle re-derives target 31.
//   - dayOfMonth == 0 AND the anchor is NOT the last day of its month: the day
//     exists in every month (1..28), so it never clamps and the anchor's own day
//     is a stable target (e.g. the 15th stays the 15th).
func monthlyTargetDom(anchor time.Time, dayOfMonth int) int {
	if dayOfMonth >= 1 && dayOfMonth <= 31 {
		return dayOfMonth
	}
	if anchor.Day() == daysInMonth(anchor.Year(), int(anchor.Month())) {
		// Anchor sits on month end with no explicit day selected: treat as
		// "last day of each month" so we recover to 31 in long months instead of
		// sticking at a shorter month's length forever.
		return 31
	}
	return anchor.Day()
}

// stepMonthTo (PR#62 r4 Bug2) advances `from` by n calendar months and snaps the
// result to `targetDom` (clamped to the target month's length), then applies
// runTime. Crucially it computes the day RELATIVE TO targetDom on every call, so
// repeated stepping never carries a clamped day forward: from Jan 31 with
// targetDom=31 we get Feb 28/29, then Mar 31, then Apr 30, ... Leap-year
// February and December year-wrap fall out naturally from addMonthsClamped /
// daysInMonth. With targetDom in 1..31 the alignDayOfMonth clamp is a no-op for
// months that have the day and clamps to month-end otherwise.
func stepMonthTo(from time.Time, n int, runTime string, targetDom int) time.Time {
	// Move to the target month first (addMonthsClamped keeps from's day-of-month
	// or clamps to month end), then forcibly re-anchor to targetDom for that
	// month so the previous step's clamped day is never propagated.
	moved := addMonthsClamped(from, n)
	aligned := alignDayOfMonth(moved, targetDom)
	return applyRunTime(aligned, runTime)
}

// addMonthsClamped advances t by n calendar months. Go's time.AddDate rolls a
// non-existent day over into the following month (e.g. Jan 31 + 1 month yields
// Mar 3, because Feb 31 normalizes forward). That is surprising for a recurring
// schedule: "every month on the 31st" should fire on the last day of months
// that have no 31st. We detect the overflow (the resulting month is not the
// expected target month) and clamp back to the last day of the target month,
// preserving the time-of-day. This naturally handles Feb 28/29 (leap years) and
// December year-wrap.
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
