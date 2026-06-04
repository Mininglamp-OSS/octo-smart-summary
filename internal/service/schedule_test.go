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
	// 1 month from Jan 31 -> Go AddDate normalizes Feb 31 to Mar 3 (2026 non-leap).
	got, err := NextRunWithInterval("", 0, 1, "", from)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := from.AddDate(0, 1, 0)
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
