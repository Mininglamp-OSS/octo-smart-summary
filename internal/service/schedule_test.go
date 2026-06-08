package service

import (
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return tm
}

func TestValidateInterval(t *testing.T) {
	cases := []struct {
		name    string
		cron    string
		days    int
		months  int
		wantErr bool
	}{
		{"cron only", "0 9 * * *", 0, 0, false},
		{"days only", "", 3, 0, false},
		{"weeks as days", "", 14, 0, false},
		{"months only", "", 0, 1, false},
		{"none", "", 0, 0, true},
		{"days+cron mutually exclusive", "0 9 * * *", 3, 0, true},
		{"days+months mutually exclusive", "", 3, 1, true},
		{"all three", "0 9 * * *", 3, 1, true},
		{"negative days", "", -1, 0, true},
		{"negative months", "", 0, -1, true},
		{"days over upper bound", "", MaxIntervalDays + 1, 0, true},
		{"days at upper bound ok", "", MaxIntervalDays, 0, false},
		{"months over upper bound", "", 0, MaxIntervalMonths + 1, true},
		{"months at upper bound ok", "", 0, MaxIntervalMonths, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateInterval(tc.cron, tc.days, tc.months)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
		})
	}
}

func TestNextRunWithInterval_Days(t *testing.T) {
	from := mustTime(t, "2026-06-04T12:00:00Z")

	// 3 days
	got, err := NextRunWithInterval("", 3, 0, "", 0, 0, from)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := mustTime(t, "2026-06-07T12:00:00Z")
	if !got.Equal(want) {
		t.Errorf("3 days: got %v want %v", got, want)
	}

	// 2 weeks = 14 days
	got, err = NextRunWithInterval("", 14, 0, "", 0, 0, from)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want = mustTime(t, "2026-06-18T12:00:00Z")
	if !got.Equal(want) {
		t.Errorf("14 days: got %v want %v", got, want)
	}
}

func TestNextRunWithInterval_DaysRunTime(t *testing.T) {
	from := mustTime(t, "2026-06-04T12:34:56Z")
	// run_time snaps the time-of-day to 09:00, seconds zeroed
	got, err := NextRunWithInterval("", 3, 0, "09:00", 0, 0, from)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := mustTime(t, "2026-06-07T09:00:00Z")
	if !got.Equal(want) {
		t.Errorf("3 days @09:00: got %v want %v", got, want)
	}
}

func TestNextRunWithInterval_Months(t *testing.T) {
	from := mustTime(t, "2026-01-31T12:00:00Z")
	// 1 month from Jan 31 -> clamp to Feb 28 (2026 non-leap), NOT Go's default
	// AddDate overflow to Mar 3.
	got, err := NextRunWithInterval("", 0, 1, "", 0, 0, from)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := mustTime(t, "2026-02-28T12:00:00Z")
	if !got.Equal(want) {
		t.Errorf("1 month: got %v want %v", got, want)
	}

	// Plain mid-month case is exact.
	from2 := mustTime(t, "2026-06-15T08:00:00Z")
	got2, err := NextRunWithInterval("", 0, 1, "", 0, 0, from2)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want2 := mustTime(t, "2026-07-15T08:00:00Z")
	if !got2.Equal(want2) {
		t.Errorf("1 month mid: got %v want %v", got2, want2)
	}
}

func TestNextRunWithInterval_MonthsRunTime(t *testing.T) {
	from := mustTime(t, "2026-06-15T23:11:00Z")
	got, err := NextRunWithInterval("", 0, 2, "07:30", 0, 0, from)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := mustTime(t, "2026-08-15T07:30:00Z")
	if !got.Equal(want) {
		t.Errorf("2 months @07:30: got %v want %v", got, want)
	}
}

func TestNextRunWithInterval_Cron(t *testing.T) {
	from := mustTime(t, "2026-06-04T12:00:00Z")
	got, err := NextRunWithInterval("0 9 * * *", 0, 0, "", 0, 0, from)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// Next 09:00 after 12:00 is the following day 09:00.
	want := mustTime(t, "2026-06-05T09:00:00Z")
	if !got.Equal(want) {
		t.Errorf("cron daily: got %v want %v", got, want)
	}
}

func TestNextRunWithInterval_InvalidRejected(t *testing.T) {
	from := mustTime(t, "2026-06-04T12:00:00Z")
	// mutual exclusivity violation must error before computing
	if _, err := NextRunWithInterval("0 9 * * *", 3, 0, "", 0, 0, from); err == nil {
		t.Fatalf("expected mutual-exclusivity error")
	}
	// over upper bound must error (overflow guard)
	if _, err := NextRunWithInterval("", MaxIntervalDays+1, 0, "", 0, 0, from); err == nil {
		t.Fatalf("expected upper-bound error")
	}
}

// TestToggleReactivateRecomputesToFuture documents the invariant the toggle
// handler relies on: when an interval schedule is re-enabled, recomputing from
// time.Now() must yield a strictly-future next_run even if the stored next_run
// is far in the past. Regression guard for the reviewer's critical bug
// (interval task firing immediately on re-enable).
func TestToggleReactivateRecomputesToFuture(t *testing.T) {
	now := time.Now().UTC()
	if got, err := NextRunWithInterval("", 3, 0, "", 0, 0, now); err != nil || !got.After(now) {
		t.Fatalf("day toggle recompute: got %v err %v, want future", got, err)
	}
	if got, err := NextRunWithInterval("", 14, 0, "", 0, 0, now); err != nil || !got.After(now) {
		t.Fatalf("week toggle recompute: got %v err %v, want future", got, err)
	}
	if got, err := NextRunWithInterval("", 0, 1, "", 0, 0, now); err != nil || !got.After(now) {
		t.Fatalf("month toggle recompute: got %v err %v, want future", got, err)
	}
}

// TestNextRunWithInterval_MonthEndClamp covers the Boss decision: month
// stepping must clamp to the last day of the target month instead of Go's
// default overflow (Jan 31 + 1 month -> Mar 3).
func TestNextRunWithInterval_MonthEndClamp(t *testing.T) {
	cases := []struct {
		name string
		from string
		n    int
		want string
	}{
		// Jan 31 + 1 month -> Feb 28 (2026 is NOT a leap year), not Mar 3.
		{"jan31 +1 non-leap", "2026-01-31T08:00:00Z", 1, "2026-02-28T08:00:00Z"},
		// Jan 31 + 1 month in a leap year (2028) -> Feb 29.
		{"jan31 +1 leap", "2028-01-31T08:00:00Z", 1, "2028-02-29T08:00:00Z"},
		// Jan 31 + 13 months (cross-year) lands on Feb of next year, clamp to 28.
		{"jan31 +13 cross-year", "2026-01-31T08:00:00Z", 13, "2027-02-28T08:00:00Z"},
		// Dec 31 + 1 month -> Jan 31 (exists), year wrap, no clamp.
		{"dec31 +1 year-wrap", "2026-12-31T08:00:00Z", 1, "2027-01-31T08:00:00Z"},
		// Dec 31 + 2 months -> Feb, clamp to 28 (2027 non-leap).
		{"dec31 +2 clamp", "2026-12-31T08:00:00Z", 2, "2027-02-28T08:00:00Z"},
		// Mar 31 + 1 month -> Apr 30 (30-day month), clamp.
		{"mar31 +1 to apr30", "2026-03-31T08:00:00Z", 1, "2026-04-30T08:00:00Z"},
		// Mid-month + 1 month is exact, no clamp.
		{"mid-month exact", "2026-06-15T08:00:00Z", 1, "2026-07-15T08:00:00Z"},
		// Jan 30 + 1 month -> Feb 28 clamp (non-leap).
		{"jan30 +1 clamp", "2026-01-30T08:00:00Z", 1, "2026-02-28T08:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from := mustTime(t, tc.from)
			got, err := NextRunWithInterval("", 0, tc.n, "", 0, 0, from)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			want := mustTime(t, tc.want)
			if !got.Equal(want) {
				t.Errorf("%s: got %v want %v", tc.name, got, want)
			}
		})
	}
}

// TestNextRunWithInterval_MonthEndClampWithRunTime verifies clamping composes
// with run_time anchoring: the day clamps to month-end, the time snaps to HH:MM.
func TestNextRunWithInterval_MonthEndClampWithRunTime(t *testing.T) {
	from := mustTime(t, "2026-01-31T23:11:00Z")
	got, err := NextRunWithInterval("", 0, 1, "09:30", 0, 0, from)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := mustTime(t, "2026-02-28T09:30:00Z")
	if !got.Equal(want) {
		t.Errorf("jan31 +1 @09:30: got %v want %v", got, want)
	}
}

func TestValidateRunTime(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"", false},
		{"00:00", false},
		{"09:00", false},
		{"23:59", false},
		{"24:00", true},
		{"09:60", true},
		{"9:00", true},  // not zero-padded -> wrong length
		{"09:0", true},  // wrong length
		{"0900", true},  // missing colon
		{"ab:cd", true}, // non-digit
		{"09-00", true}, // wrong separator
		{"-1:00", true},
		{"garbage", true},
	}
	for _, tc := range cases {
		err := ValidateRunTime(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ValidateRunTime(%q) err=%v wantErr=%v", tc.in, err, tc.wantErr)
		}
	}
}

// TestValidateIntervalForWrite verifies the interval-only write contract:
// cron writes are rejected, exactly one interval source required.
func TestValidateIntervalForWrite(t *testing.T) {
	cases := []struct {
		name    string
		cron    string
		days    int
		months  int
		wantErr bool
	}{
		{"days only ok", "", 3, 0, false},
		{"weeks as days ok", "", 14, 0, false},
		{"months only ok", "", 0, 1, false},
		{"cron rejected", "0 9 * * *", 0, 0, true},
		{"cron+days rejected", "0 9 * * *", 3, 0, true},
		{"none rejected", "", 0, 0, true},
		{"days+months mutually exclusive", "", 3, 1, true},
		{"over bound days", "", MaxIntervalDays + 1, 0, true},
		{"over bound months", "", 0, MaxIntervalMonths + 1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateIntervalForWrite(tc.cron, tc.days, tc.months)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateIntervalForWrite(%q,%d,%d) err=%v wantErr=%v", tc.cron, tc.days, tc.months, err, tc.wantErr)
			}
		})
	}
}

// TestValidateInterval_LegacyCronStillValid ensures the scheduler-facing
// ValidateInterval keeps accepting legacy cron so existing cron schedules keep
// executing even though new cron writes are blocked at the API layer.
func TestValidateInterval_LegacyCronStillValid(t *testing.T) {
	if err := ValidateInterval("0 9 * * *", 0, 0); err != nil {
		t.Fatalf("legacy cron must remain valid for scheduler: %v", err)
	}
	from := mustTime(t, "2026-06-04T12:00:00Z")
	if _, err := NextRunWithInterval("0 9 * * *", 0, 0, "", 0, 0, from); err != nil {
		t.Fatalf("legacy cron next-run must still compute: %v", err)
	}
}

func TestParseRunTime(t *testing.T) {
	cases := []struct {
		in   string
		h, m int
		ok   bool
	}{
		{"09:00", 9, 0, true},
		{"23:59", 23, 59, true},
		{"00:00", 0, 0, true},
		{"", 0, 0, false},
		{"24:00", 0, 0, false},
		{"09:60", 0, 0, false},
		{"-1:00", 0, 0, false},
		{"garbage", 0, 0, false},
	}
	for _, tc := range cases {
		h, m, ok := parseRunTime(tc.in)
		if ok != tc.ok || (ok && (h != tc.h || m != tc.m)) {
			t.Errorf("parseRunTime(%q) = %d,%d,%v want %d,%d,%v", tc.in, h, m, ok, tc.h, tc.m, tc.ok)
		}
	}
}

// ---- Need 1: first-run "today if run_time still ahead" semantics ----

// shTime parses an RFC3339 string and converts it to Asia/Shanghai, mirroring
// how the handlers feed timezone.Now() (which is already Asia/Shanghai) into
// NextRunInitial.
func shTime(t *testing.T, s string) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.FixedZone("CST", 8*60*60)
	}
	tm, err := time.ParseInLocation(time.RFC3339, s, loc)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return tm
}

// Day mode: if today's run_time is still ahead of now, fire TODAY.
func TestNextRunInitial_DayToday(t *testing.T) {
	now := shTime(t, "2026-06-04T10:00:00+08:00")
	// run_time 17:00 today is still ahead -> today 17:00.
	got, err := NextRunInitial("", 1, 0, "17:00", 0, 0, now)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := shTime(t, "2026-06-04T17:00:00+08:00")
	if !got.Equal(want) {
		t.Errorf("day today: got %v want %v", got, want)
	}
}

// Day mode: if today's run_time already passed, advance one interval.
func TestNextRunInitial_DayPassed(t *testing.T) {
	now := shTime(t, "2026-06-04T18:00:00+08:00")
	got, err := NextRunInitial("", 1, 0, "17:00", 0, 0, now)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := shTime(t, "2026-06-05T17:00:00+08:00")
	if !got.Equal(want) {
		t.Errorf("day passed: got %v want %v", got, want)
	}
}

// Week mode without explicit weekday: today if time not yet passed.
func TestNextRunInitial_WeekTodayNoDOW(t *testing.T) {
	now := shTime(t, "2026-06-04T10:00:00+08:00")
	got, err := NextRunInitial("", 7, 0, "17:00", 0, 0, now)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := shTime(t, "2026-06-04T17:00:00+08:00")
	if !got.Equal(want) {
		t.Errorf("week today (no dow): got %v want %v", got, want)
	}
}

// Month mode: this month's day if still ahead.
func TestNextRunInitial_MonthThisMonth(t *testing.T) {
	now := shTime(t, "2026-06-04T10:00:00+08:00")
	// No explicit day_of_month -> use today's day (4th) at 17:00, still ahead.
	got, err := NextRunInitial("", 0, 1, "17:00", 0, 0, now)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := shTime(t, "2026-06-04T17:00:00+08:00")
	if !got.Equal(want) {
		t.Errorf("month this month: got %v want %v", got, want)
	}
}

// ---- Need 4: day_of_week / day_of_month alignment ----

// Week mode with explicit weekday: 2026-06-04 is a Thursday (ISO 4).
// Selecting Monday (1) at 09:00 should land on the next Monday, 2026-06-08.
func TestNextRunInitial_WeekDOW(t *testing.T) {
	now := shTime(t, "2026-06-04T10:00:00+08:00") // Thu
	got, err := NextRunInitial("", 7, 0, "09:00", 1 /*Mon*/, 0, now)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := shTime(t, "2026-06-08T09:00:00+08:00") // Mon
	if !got.Equal(want) {
		t.Errorf("week dow Mon: got %v want %v (weekday=%v)", got, want, got.Weekday())
	}
}

// Week mode, weekday is today and time still ahead -> run today.
func TestNextRunInitial_WeekDOWTodayAhead(t *testing.T) {
	now := shTime(t, "2026-06-04T08:00:00+08:00") // Thu = ISO 4
	got, err := NextRunInitial("", 7, 0, "09:00", 4 /*Thu*/, 0, now)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := shTime(t, "2026-06-04T09:00:00+08:00")
	if !got.Equal(want) {
		t.Errorf("week dow today ahead: got %v want %v", got, want)
	}
}

// Week mode, weekday is today but time already passed -> next week same weekday.
func TestNextRunInitial_WeekDOWTodayPassed(t *testing.T) {
	now := shTime(t, "2026-06-04T10:00:00+08:00") // Thu
	got, err := NextRunInitial("", 7, 0, "09:00", 4 /*Thu*/, 0, now)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := shTime(t, "2026-06-11T09:00:00+08:00") // next Thu
	if !got.Equal(want) {
		t.Errorf("week dow today passed: got %v want %v", got, want)
	}
}

// Month mode with explicit day-of-month ahead in the same month.
func TestNextRunInitial_MonthDOM(t *testing.T) {
	now := shTime(t, "2026-06-04T10:00:00+08:00")
	got, err := NextRunInitial("", 0, 1, "09:00", 0, 15, now)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := shTime(t, "2026-06-15T09:00:00+08:00")
	if !got.Equal(want) {
		t.Errorf("month dom 15: got %v want %v", got, want)
	}
}

// Month mode, day-of-month already passed this month -> next month.
func TestNextRunInitial_MonthDOMPassed(t *testing.T) {
	now := shTime(t, "2026-06-20T10:00:00+08:00")
	got, err := NextRunInitial("", 0, 1, "09:00", 0, 15, now)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := shTime(t, "2026-07-15T09:00:00+08:00")
	if !got.Equal(want) {
		t.Errorf("month dom passed: got %v want %v", got, want)
	}
}

// Month mode, day-of-month clamps to month end (Feb 31 -> Feb 28 in 2026).
func TestNextRunInitial_MonthDOMClamp(t *testing.T) {
	now := shTime(t, "2026-02-10T10:00:00+08:00")
	got, err := NextRunInitial("", 0, 1, "09:00", 0, 31, now)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := shTime(t, "2026-02-28T09:00:00+08:00")
	if !got.Equal(want) {
		t.Errorf("month dom clamp: got %v want %v", got, want)
	}
}

// ADVANCE form aligns week mode to the selected weekday.
func TestNextRunWithInterval_WeekDOWAdvance(t *testing.T) {
	from := shTime(t, "2026-06-04T09:00:00+08:00") // Thu
	// +7 days = next Thu (06-11), then align to Monday(1) -> following Mon 06-15.
	got, err := NextRunWithInterval("", 7, 0, "09:00", 1, 0, from)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := shTime(t, "2026-06-15T09:00:00+08:00") // Mon
	if !got.Equal(want) {
		t.Errorf("advance week dow: got %v want %v (weekday=%v)", got, want, got.Weekday())
	}
}

// ADVANCE form aligns month mode to the selected day-of-month.
func TestNextRunWithInterval_MonthDOMAdvance(t *testing.T) {
	from := shTime(t, "2026-06-15T09:00:00+08:00")
	got, err := NextRunWithInterval("", 0, 1, "09:00", 0, 20, from)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := shTime(t, "2026-07-20T09:00:00+08:00")
	if !got.Equal(want) {
		t.Errorf("advance month dom: got %v want %v", got, want)
	}
}

// Need 2 sanity: run_time=17:00 with from in Asia/Shanghai yields a 17:00
// Beijing-time next_run (no 8h skew).
func TestNextRunInitial_TimezoneBeijing(t *testing.T) {
	now := shTime(t, "2026-06-04T10:00:00+08:00")
	got, err := NextRunInitial("", 1, 0, "17:00", 0, 0, now)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.Hour() != 17 || got.Minute() != 0 {
		t.Errorf("expected 17:00 local, got %02d:%02d", got.Hour(), got.Minute())
	}
	// Offset must be +08:00 (Beijing), not UTC.
	_, off := got.Zone()
	if off != 8*60*60 {
		t.Errorf("expected +08:00 offset, got %d seconds", off)
	}
}

// ---------------------------------------------------------------------------
// Bug2: NextRunScheduledAdvance anchors recompute on the ORIGINAL due time
// (*sched.NextRunAt), not on `now`, so a late scan does not skip a cycle.
// ---------------------------------------------------------------------------

// Weekly Monday 09:00 schedule, scanned LATE on Tuesday 10:00. Recompute must
// land on the NEXT Monday (one week from the missed anchor), NOT skip to the
// week after next. Anchoring on `now` (the old bug) would have produced
// 2026-06-15+ behavior; anchoring on the Monday anchor yields 2026-06-15.
func TestNextRunScheduledAdvance_WeekLateScanNoSkip(t *testing.T) {
	anchor := shTime(t, "2026-06-08T09:00:00+08:00") // Mon (the missed due time)
	now := shTime(t, "2026-06-09T10:00:00+08:00")    // Tue, scanned late
	got, err := NextRunScheduledAdvance("", 7, 0, "09:00", 1, 0, anchor, now)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := shTime(t, "2026-06-15T09:00:00+08:00") // next Mon, not the one after
	if !got.Equal(want) {
		t.Fatalf("week late scan: got %v want %v (weekday=%v)", got, want, got.Weekday())
	}
	if got.Weekday() != time.Monday {
		t.Fatalf("expected Monday phase preserved, got %v", got.Weekday())
	}
	if !got.After(now) {
		t.Fatalf("result %v must be strictly after now %v", got, now)
	}
}

// On-time weekly scan: anchor==due, now slightly after; one step lands on the
// following same weekday.
func TestNextRunScheduledAdvance_WeekOnTime(t *testing.T) {
	anchor := shTime(t, "2026-06-08T09:00:00+08:00") // Mon
	now := shTime(t, "2026-06-08T09:00:30+08:00")    // 30s late
	got, err := NextRunScheduledAdvance("", 7, 0, "09:00", 1, 0, anchor, now)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := shTime(t, "2026-06-15T09:00:00+08:00")
	if !got.Equal(want) {
		t.Fatalf("week on time: got %v want %v", got, want)
	}
}

// 14/21/28-day intervals must keep their multi-week cadence and weekday phase
// when scanned late by more than one of the sub-weeks but less than a full
// interval.
func TestNextRunScheduledAdvance_MultiWeekIntervals(t *testing.T) {
	cases := []struct {
		name     string
		interval int
		anchor   string
		now      string
		want     string
	}{
		{"14d late 3 days", 14, "2026-06-08T09:00:00+08:00", "2026-06-11T10:00:00+08:00", "2026-06-22T09:00:00+08:00"},
		{"21d late 5 days", 21, "2026-06-08T09:00:00+08:00", "2026-06-13T10:00:00+08:00", "2026-06-29T09:00:00+08:00"},
		{"28d late 10 days", 28, "2026-06-08T09:00:00+08:00", "2026-06-18T10:00:00+08:00", "2026-07-06T09:00:00+08:00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			anchor := shTime(t, tc.anchor)
			now := shTime(t, tc.now)
			got, err := NextRunScheduledAdvance("", tc.interval, 0, "09:00", 1, 0, anchor, now)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			want := shTime(t, tc.want)
			if !got.Equal(want) {
				t.Fatalf("%s: got %v want %v", tc.name, got, want)
			}
			if got.Weekday() != time.Monday {
				t.Fatalf("%s: weekday phase drifted to %v", tc.name, got.Weekday())
			}
		})
	}
}

// Cross-MULTIPLE-period downtime: scheduler was down ~3 weeks for a weekly
// schedule; result is the nearest FUTURE Monday, phase preserved.
func TestNextRunScheduledAdvance_MultiPeriodDowntime(t *testing.T) {
	anchor := shTime(t, "2026-06-08T09:00:00+08:00") // Mon
	now := shTime(t, "2026-06-25T10:00:00+08:00")    // ~2.5 weeks later
	got, err := NextRunScheduledAdvance("", 7, 0, "09:00", 1, 0, anchor, now)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := shTime(t, "2026-06-29T09:00:00+08:00") // next Monday strictly after now
	if !got.Equal(want) {
		t.Fatalf("multi-period downtime: got %v want %v", got, want)
	}
}

// Day mode (not a multiple of 7) keeps fixed N*24h cadence from the anchor.
func TestNextRunScheduledAdvance_DayMode(t *testing.T) {
	anchor := shTime(t, "2026-06-08T09:00:00+08:00")
	now := shTime(t, "2026-06-09T10:00:00+08:00") // late by ~1 day
	got, err := NextRunScheduledAdvance("", 1, 0, "09:00", 0, 0, anchor, now)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// +1d from anchor = 06-09 09:00 (<= now), +1d again = 06-10 09:00 (> now).
	want := shTime(t, "2026-06-10T09:00:00+08:00")
	if !got.Equal(want) {
		t.Fatalf("day mode: got %v want %v", got, want)
	}
}

// Month mode keeps day-of-month phase and month-end clamp under late scan.
func TestNextRunScheduledAdvance_MonthClampLate(t *testing.T) {
	anchor := shTime(t, "2026-01-31T09:00:00+08:00") // monthly on the 31st
	now := shTime(t, "2026-02-05T10:00:00+08:00")    // scanned in Feb
	got, err := NextRunScheduledAdvance("", 0, 1, "09:00", 0, 31, anchor, now)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// +1 month from Jan 31 clamps to Feb 28 (2026 not leap); strictly after now.
	want := shTime(t, "2026-02-28T09:00:00+08:00")
	if !got.Equal(want) {
		t.Fatalf("month clamp late: got %v want %v", got, want)
	}
}

// Cron mode is unchanged: it returns the next occurrence strictly after now.
func TestNextRunScheduledAdvance_CronUsesNow(t *testing.T) {
	anchor := shTime(t, "2026-06-01T09:00:00+08:00")
	now := shTime(t, "2026-06-09T10:00:00+08:00")
	got, err := NextRunScheduledAdvance("0 9 * * *", 0, 0, "", 0, 0, anchor, now)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := shTime(t, "2026-06-10T09:00:00+08:00")
	if !got.Equal(want) {
		t.Fatalf("cron: got %v want %v", got, want)
	}
}

// --- Bug2 (PR#62 r4 yujiawei): month-mode day-of-month must not monotonically
// decrease and must recover to the original day-of-month in long months. -------
//
// Repro: anchor Jan 31, interval_months=1, day_of_month=0 ("不限"). The r3 code
// fed each step's clamped result back as the next base, so Feb 28 became the
// permanent target and the schedule stuck at the 28th forever. The fix re-anchors
// every step to the ORIGINAL anchor's day-of-month (31) so it recovers in 31-day
// months and falls to month-end only in short months.
func TestNextRunScheduledAdvance_MonthDom0_NoMonotonicDecrease(t *testing.T) {
	// dom=0 (unset): target day is the anchor's own day (31).
	anchor := shTime(t, "2026-01-31T09:00:00+08:00")
	// Expected sequence stepping one month at a time, dom anchored at 31.
	want := []string{
		"2026-02-28T09:00:00+08:00", // 2026 not leap
		"2026-03-31T09:00:00+08:00", // recover to 31
		"2026-04-30T09:00:00+08:00", // 30-day month
		"2026-05-31T09:00:00+08:00", // recover to 31
		"2026-06-30T09:00:00+08:00",
		"2026-07-31T09:00:00+08:00",
		"2026-08-31T09:00:00+08:00",
		"2026-09-30T09:00:00+08:00",
		"2026-10-31T09:00:00+08:00",
		"2026-11-30T09:00:00+08:00",
		"2026-12-31T09:00:00+08:00", // year-wrap setup
		"2027-01-31T09:00:00+08:00", // December -> January year wrap
		"2027-02-28T09:00:00+08:00",
	}
	cur := anchor
	for i, w := range want {
		// now just below cur so exactly one step is taken each iteration.
		now := cur.Add(time.Second)
		got, err := NextRunScheduledAdvance("", 0, 1, "09:00", 0, 0, cur, now)
		if err != nil {
			t.Fatalf("step %d err: %v", i, err)
		}
		wantT := shTime(t, w)
		if !got.Equal(wantT) {
			t.Fatalf("step %d: got %v want %v", i, got, wantT)
		}
		// Assert never monotonically dropping below original dom permanently:
		// the day-of-month must be 31 whenever the month has 31 days.
		cur = got
	}
}

// Leap-year February: anchor Jan 31 in 2028 (leap) -> Feb 29, then Mar 31.
func TestNextRunScheduledAdvance_MonthDom0_LeapFebruary(t *testing.T) {
	anchor := shTime(t, "2028-01-31T09:00:00+08:00")
	now := anchor.Add(time.Second)
	got, err := NextRunScheduledAdvance("", 0, 1, "09:00", 0, 0, anchor, now)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := shTime(t, "2028-02-29T09:00:00+08:00")
	if !got.Equal(want) {
		t.Fatalf("leap feb: got %v want %v", got, want)
	}
	// Next step from Feb 29 must recover to Mar 31, not stick at 29.
	got2, err := NextRunScheduledAdvance("", 0, 1, "09:00", 0, 0, got, got.Add(time.Second))
	if err != nil {
		t.Fatalf("err2: %v", err)
	}
	want2 := shTime(t, "2028-03-31T09:00:00+08:00")
	if !got2.Equal(want2) {
		t.Fatalf("recover after leap feb: got %v want %v", got2, want2)
	}
}

// Explicit day_of_month=31 must behave identically (anchored at 31, recovering).
func TestNextRunScheduledAdvance_MonthExplicitDom31_Recovers(t *testing.T) {
	anchor := shTime(t, "2026-01-31T09:00:00+08:00")
	now := anchor.Add(time.Second)
	got, err := NextRunScheduledAdvance("", 0, 1, "09:00", 0, 31, anchor, now)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if want := shTime(t, "2026-02-28T09:00:00+08:00"); !got.Equal(want) {
		t.Fatalf("dom31 feb: got %v want %v", got, want)
	}
	got2, err := NextRunScheduledAdvance("", 0, 1, "09:00", 0, 31, got, got.Add(time.Second))
	if err != nil {
		t.Fatalf("err2: %v", err)
	}
	if want := shTime(t, "2026-03-31T09:00:00+08:00"); !got2.Equal(want) {
		t.Fatalf("dom31 recover: got %v want %v", got2, want)
	}
}

// Explicit day_of_month=15 (mid-month, never clamped) must step cleanly without
// regression and not be affected by the Bug2 fix.
func TestNextRunScheduledAdvance_MonthExplicitDom15_Stable(t *testing.T) {
	anchor := shTime(t, "2026-01-15T09:00:00+08:00")
	now := anchor.Add(time.Second)
	got, err := NextRunScheduledAdvance("", 0, 1, "09:00", 0, 15, anchor, now)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if want := shTime(t, "2026-02-15T09:00:00+08:00"); !got.Equal(want) {
		t.Fatalf("dom15: got %v want %v", got, want)
	}
}
