package service

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
)

func generationTestTime(value string) time.Time {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return t
}

func TestGenerationTimeSelectors(t *testing.T) {
	effective := generationTestTime("2026-09-08T10:00:00+08:00")
	for _, test := range []struct {
		name     string
		selector model.SummaryTimeSelector
		start    string
		end      string
	}{
		{"absolute", model.SummaryTimeSelector{Mode: "absolute", Start: "2026-07-01T00:00:00+08:00", End: "2026-09-01T00:00:00+08:00"}, "2026-07-01T00:00:00+08:00", "2026-09-01T00:00:00+08:00"},
		{"relative60", model.SummaryTimeSelector{Mode: "relative", Days: 60}, "2026-07-10T10:00:00+08:00", "2026-09-08T10:00:00+08:00"},
		{"previousWeek", model.SummaryTimeSelector{Mode: "natural_period", Unit: "week", Offset: -1}, "2026-08-31T00:00:00+08:00", "2026-09-07T00:00:00+08:00"},
		{"previousMonth", model.SummaryTimeSelector{Mode: "natural_period", Unit: "month", Offset: -1}, "2026-08-01T00:00:00+08:00", "2026-09-01T00:00:00+08:00"},
		{"previousDay", model.SummaryTimeSelector{Mode: "natural_period", Unit: "day", Offset: -1}, "2026-09-07T00:00:00+08:00", "2026-09-08T00:00:00+08:00"},
		{"previousYear", model.SummaryTimeSelector{Mode: "natural_period", Unit: "year", Offset: -1}, "2025-01-01T00:00:00+08:00", "2026-01-01T00:00:00+08:00"},
		{"firstIncremental", model.SummaryTimeSelector{Mode: "incremental", InitialStart: "2026-09-01T12:00:00+08:00"}, "2026-09-01T12:00:00+08:00", "2026-09-08T10:00:00+08:00"},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.selector.Timezone = timezone.Name
			resolved, err := ResolveGenerationTime(test.selector, effective, nil, 366)
			requireWriteOK(t, err)
			if resolved.Start.Format(time.RFC3339) != test.start || resolved.End.Format(time.RFC3339) != test.end ||
				resolved.Boundary != "[start,end)" || resolved.DataGapTruncated || !resolved.EffectiveAt.Equal(effective) {
				t.Fatalf("unexpected frozen range: %+v", resolved)
			}
		})
	}
	// A rolling seven days remains different from a complete natural week.
	legacy, err := LegacyRollingSelector(2)
	requireWriteOK(t, err)
	resolved, err := ResolveGenerationTime(legacy, effective, nil, 60)
	requireWriteOK(t, err)
	if resolved.Start.Format(time.RFC3339) != "2026-09-01T10:00:00+08:00" {
		t.Fatal("legacy recent seven days became a natural week")
	}
}

func TestGenerationIncrementalWatermarkAndValidation(t *testing.T) {
	now := generationTestTime("2026-09-08T10:00:00+08:00")
	selector := model.SummaryTimeSelector{Mode: "incremental", Timezone: timezone.Name}
	_, err := ResolveGenerationTime(selector, now, nil, 30)
	requireContentCode(t, err, "initial_range_required")
	old := now.AddDate(0, 0, -90)
	resolved, err := ResolveGenerationTime(selector, now, &old, 30)
	requireWriteOK(t, err)
	if !resolved.DataGapTruncated || !resolved.Start.Equal(now.AddDate(0, 0, -30)) || !old.Equal(now.AddDate(0, 0, -90)) {
		t.Fatal("truncation did not record gap or mutated caller watermark")
	}
	future := now.Add(time.Hour)
	_, err = ResolveGenerationTime(selector, now, &future, 30)
	requireContentCode(t, err, "invalid_time_range")
	selector.Timezone = "America/New_York"
	_, err = ResolveGenerationTime(selector, now, &old, 30)
	requireContentCode(t, err, "unsupported_timezone")
	_, err = ResolveGenerationTime(model.SummaryTimeSelector{Mode: "relative", Timezone: timezone.Name, Days: 60}, now, nil, 30)
	requireContentCode(t, err, "time_window_exceeded")
	_, err = ResolveGenerationTime(model.SummaryTimeSelector{Mode: "relative", Timezone: timezone.Name, Days: 1, Start: "ignored?"}, now, nil, 30)
	requireContentCode(t, err, "invalid_time_selector")
}

func TestGenerationNaturalBoundariesAndLateSlots(t *testing.T) {
	for _, test := range []struct{ effective, start, end string }{
		{"2024-03-01T00:00:00+08:00", "2024-02-01T00:00:00+08:00", "2024-03-01T00:00:00+08:00"},
		{"2026-01-01T00:00:00+08:00", "2025-12-01T00:00:00+08:00", "2026-01-01T00:00:00+08:00"},
	} {
		out, err := ResolveGenerationTime(model.SummaryTimeSelector{Mode: "natural_period", Timezone: timezone.Name, Unit: "month", Offset: -1},
			generationTestTime(test.effective), nil, 60)
		requireWriteOK(t, err)
		if out.Start.Format(time.RFC3339) != test.start || out.End.Format(time.RFC3339) != test.end {
			t.Fatalf("calendar boundary: %+v", out)
		}
	}
	due := generationTestTime("2026-08-31T09:00:00+08:00")
	schedule := model.SummarySchedule{NextRunAt: &due, IntervalDays: 7, RunTime: "09:00", DayOfWeek: 1}
	slot, next, err := ResolveGenerationSlot(schedule, generationTestTime("2026-09-08T13:00:00+08:00"))
	requireWriteOK(t, err)
	if slot.Format(time.RFC3339) != "2026-09-07T09:00:00+08:00" || next.Format(time.RFC3339) != "2026-09-14T09:00:00+08:00" {
		t.Fatalf("late weekly run lost phase: %v / %v", slot, next)
	}
	due = generationTestTime("2026-01-31T09:00:00+08:00")
	schedule = model.SummarySchedule{NextRunAt: &due, IntervalMonths: 1, RunTime: "09:00", AnchorDOM: 31}
	slot, next, err = ResolveGenerationSlot(schedule, generationTestTime("2026-03-31T10:00:00+08:00"))
	requireWriteOK(t, err)
	if slot.Format(time.RFC3339) != "2026-03-31T09:00:00+08:00" || next.Format(time.RFC3339) != "2026-04-30T09:00:00+08:00" {
		t.Fatalf("monthly anchor drifted after February: %v / %v", slot, next)
	}
	due = generationTestTime("2026-09-01T09:00:00+08:00")
	schedule = model.SummarySchedule{NextRunAt: &due, CronExpr: "0 9 * * 1"}
	slot, next, err = ResolveGenerationSlot(schedule, generationTestTime("2026-09-08T13:00:00+08:00"))
	requireWriteOK(t, err)
	if slot.Format(time.RFC3339) != "2026-09-07T09:00:00+08:00" || next.Format(time.RFC3339) != "2026-09-14T09:00:00+08:00" {
		t.Fatalf("legacy cron lost phase: %v / %v", slot, next)
	}
}
