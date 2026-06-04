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
	got, err := NextRunWithInterval("", 3, 0, "", from)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := mustTime(t, "2026-06-07T12:00:00Z")
	if !got.Equal(want) {
		t.Errorf("3 days: got %v want %v", got, want)
	}

	// 2 weeks = 14 days
	got, err = NextRunWithInterval("", 14, 0, "", from)
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
	got, err := NextRunWithInterval("", 3, 0, "09:00", from)
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
	got, err := NextRunWithInterval("", 0, 1, "", from)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := mustTime(t, "2026-02-28T12:00:00Z")
	if !got.Equal(want) {
		t.Errorf("1 month: got %v want %v", got, want)
	}

	// Plain mid-month case is exact.
	from2 := mustTime(t, "2026-06-15T08:00:00Z")
	got2, err := NextRunWithInterval("", 0, 1, "", from2)
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
	got, err := NextRunWithInterval("", 0, 2, "07:30", from)
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
	got, err := NextRunWithInterval("0 9 * * *", 0, 0, "", from)
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
	if _, err := NextRunWithInterval("0 9 * * *", 3, 0, "", from); err == nil {
		t.Fatalf("expected mutual-exclusivity error")
	}
	// over upper bound must error (overflow guard)
	if _, err := NextRunWithInterval("", MaxIntervalDays+1, 0, "", from); err == nil {
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
	if got, err := NextRunWithInterval("", 3, 0, "", now); err != nil || !got.After(now) {
		t.Fatalf("day toggle recompute: got %v err %v, want future", got, err)
	}
	if got, err := NextRunWithInterval("", 14, 0, "", now); err != nil || !got.After(now) {
		t.Fatalf("week toggle recompute: got %v err %v, want future", got, err)
	}
	if got, err := NextRunWithInterval("", 0, 1, "", now); err != nil || !got.After(now) {
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
			got, err := NextRunWithInterval("", 0, tc.n, "", from)
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
	got, err := NextRunWithInterval("", 0, 1, "09:30", from)
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
	if _, err := NextRunWithInterval("0 9 * * *", 0, 0, "", from); err != nil {
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
