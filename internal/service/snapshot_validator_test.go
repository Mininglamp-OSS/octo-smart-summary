package service

// snapshot_validator_test.go — SUM-BE1 shared validator regression tests.
//
// These tests intentionally do NOT touch gorm / gin / any DB. They exercise
// service.ValidateSnapshotScope directly so every target-specific gate and
// every base check has a first-class assertion that survives future
// refactors. CGO-backed SQLite integration tests (see
// internal/api/handler/task_limit_test.go) continue to cover the end-to-end
// HTTP shape.

import (
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// resetValidatorGlobals restores the package-level tunables the tests touch.
// t.Cleanup keeps the test's state changes local so no ordering coupling
// leaks between tests.
func resetValidatorGlobals(t *testing.T) {
	t.Helper()
	prevLimits := scopeLimits
	prevGetter := timeRangeMaxDaysGetter
	t.Cleanup(func() {
		scopeLimits = prevLimits
		timeRangeMaxDaysGetter = prevGetter
	})
}

// TestValidateSnapshotScope_UnknownTarget verifies that a caller passing a
// misspelled target gets a graceful 40000 rather than a panic. This is the
// only branch that returns 40000 for a non-shape reason so it is worth an
// explicit assertion.
func TestValidateSnapshotScope_UnknownTarget(t *testing.T) {
	resetValidatorGlobals(t)
	err := ValidateSnapshotScope("user1", SnapshotScopeInput{Topic: "x"}, ScopeValidationTarget("nope"))
	if err == nil {
		t.Fatalf("expected error for unknown target, got nil")
	}
	if err.Code != 40000 {
		t.Errorf("unknown target: expected 40000, got %d (%s)", err.Code, err.Message)
	}
}

// TestValidateSnapshotScope_BaseChecks_ApplyToEveryTarget covers the base
// gates (title/topic rune caps, origin-channel coupling, source count) with
// one representative target each so the base branches never regress.
func TestValidateSnapshotScope_BaseChecks_ApplyToEveryTarget(t *testing.T) {
	resetValidatorGlobals(t)

	tooLong := strings.Repeat("总", 2301)

	tests := []struct {
		name     string
		input    SnapshotScopeInput
		target   ScopeValidationTarget
		wantCode int
		wantMsg  string
	}{
		{
			name:     "title over 2300 runes rejected",
			input:    SnapshotScopeInput{Title: tooLong, Topic: "x"},
			target:   TargetPersonalWorkflow,
			wantCode: 40001,
			wantMsg:  "title 不能超过 2300 字符",
		},
		{
			name:     "topic over 2300 runes rejected",
			input:    SnapshotScopeInput{Topic: tooLong},
			target:   TargetPersonalWorkflow,
			wantCode: 40001,
			wantMsg:  "topic 不能超过 2300 字符",
		},
		{
			name: "origin_channel_id set but type out of range",
			input: SnapshotScopeInput{
				Topic:             "x",
				OriginChannelID:   "grp1",
				OriginChannelType: 99,
			},
			target:   TargetPersonalWorkflow,
			wantCode: 40001,
			wantMsg:  "origin_channel_type must be 1, 2, or 3 when origin_channel_id is set",
		},
		{
			name: "origin_channel_type set but no id",
			input: SnapshotScopeInput{
				Topic:             "x",
				OriginChannelType: 1,
			},
			target:   TargetPersonalWorkflow,
			wantCode: 40001,
			wantMsg:  "origin_channel_id is required when origin_channel_type is set",
		},
		{
			name: "sources over max count",
			input: SnapshotScopeInput{
				Sources: manyValidSources(31),
			},
			target:   TargetPersonalWorkflow,
			wantCode: 40003,
			wantMsg:  "信息来源不能超过30个",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidateSnapshotScope("user1", tc.input, tc.target)
			if got == nil {
				t.Fatalf("expected error, got nil")
			}
			if got.Code != tc.wantCode {
				t.Errorf("code: want %d got %d (%s)", tc.wantCode, got.Code, got.Message)
			}
			if got.Message != tc.wantMsg {
				t.Errorf("msg:\n  want %q\n   got %q", tc.wantMsg, got.Message)
			}
		})
	}
}

// TestValidateSnapshotScope_TimeRangeSpanCap covers the 40002 "时间范围不能超过N天"
// path — the caller supplies an explicit range that exceeds the pipeline
// runtime cap. When the caller omits TimeStart/TimeEnd the check is a no-op,
// matching the pre-refactor behaviour where a nil TimeRange never tripped
// the span check.
func TestValidateSnapshotScope_TimeRangeSpanCap(t *testing.T) {
	resetValidatorGlobals(t)

	// Simulate pipeline.DefaultTimeRangeDays = 31.
	SetTimeRangeMaxDaysGetter(func() int { return 31 })

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	longEnd := start.Add(40 * 24 * time.Hour) // 40 days > 31
	shortEnd := start.Add(10 * 24 * time.Hour)

	// Range too long: rejected 40002.
	err := ValidateSnapshotScope("user1", SnapshotScopeInput{
		Topic:     "x",
		TimeStart: start,
		TimeEnd:   longEnd,
	}, TargetPersonalWorkflow)
	if err == nil {
		t.Fatalf("expected 40002 for 40-day range against 31-day cap, got nil")
	}
	if err.Code != 40002 {
		t.Errorf("code: want 40002 got %d (%s)", err.Code, err.Message)
	}
	if !strings.Contains(err.Message, "时间范围不能超过31天") {
		t.Errorf("msg: want to contain \"时间范围不能超过31天\", got %q", err.Message)
	}

	// Range within cap: accepted.
	if err := ValidateSnapshotScope("user1", SnapshotScopeInput{
		Topic:     "x",
		TimeStart: start,
		TimeEnd:   shortEnd,
	}, TargetPersonalWorkflow); err != nil {
		t.Errorf("expected no error for 10-day range, got %v", err)
	}

	// No range supplied: check is a no-op (topic alone satisfies scope signal).
	if err := ValidateSnapshotScope("user1", SnapshotScopeInput{
		Topic: "x",
	}, TargetPersonalWorkflow); err != nil {
		t.Errorf("expected no error when time range is absent, got %v", err)
	}
}

// TestValidateSnapshotScope_PersonalWorkflow_ScopeSignal preserves the
// pre-refactor "至少提供 sources、topic 或 time_range 之一" contract.
func TestValidateSnapshotScope_PersonalWorkflow_ScopeSignal(t *testing.T) {
	resetValidatorGlobals(t)

	// Empty scope signal: rejected 40001.
	err := ValidateSnapshotScope("user1", SnapshotScopeInput{
		Title: "t",
	}, TargetPersonalWorkflow)
	if err == nil {
		t.Fatalf("expected 40001 for empty scope signal, got nil")
	}
	if err.Code != 40001 {
		t.Errorf("code: want 40001 got %d (%s)", err.Code, err.Message)
	}
	if err.Message != "至少提供 sources、topic 或 time_range 之一" {
		t.Errorf("msg: unexpected %q", err.Message)
	}

	// Topic alone: accepted.
	if err := ValidateSnapshotScope("user1", SnapshotScopeInput{Topic: "x"}, TargetPersonalWorkflow); err != nil {
		t.Errorf("topic alone should pass, got %v", err)
	}
	// Sources alone: accepted.
	if err := ValidateSnapshotScope("user1", SnapshotScopeInput{
		Sources: []SourceInput{{SourceType: 1, SourceID: "grp1"}},
	}, TargetPersonalWorkflow); err != nil {
		t.Errorf("sources alone should pass, got %v", err)
	}
	// TimeStart alone (as a time signal): accepted.
	if err := ValidateSnapshotScope("user1", SnapshotScopeInput{
		TimeStart: time.Now().Add(-time.Hour),
	}, TargetPersonalWorkflow); err != nil {
		t.Errorf("time signal alone should pass, got %v", err)
	}
	// TimeRange strings alone (test-only path): accepted.
	if err := ValidateSnapshotScope("user1", SnapshotScopeInput{
		Scope: model.SnapshotScope{TimeRange: model.TimeRangeJSON{Start: "2026-01-01T00:00:00Z"}},
	}, TargetPersonalWorkflow); err != nil {
		t.Errorf("time-range Scope signal alone should pass, got %v", err)
	}
}

// TestValidateSnapshotScope_TeamWorkflow_ParticipantsRequired exercises the
// team-target-only rule that a payload without participants is not a
// legitimate team workflow.
func TestValidateSnapshotScope_TeamWorkflow_ParticipantsRequired(t *testing.T) {
	resetValidatorGlobals(t)

	// Scope signal present, but no participants: team-target rejects.
	err := ValidateSnapshotScope("user1", SnapshotScopeInput{
		Topic:        "team weekly",
		Participants: nil,
	}, TargetTeamWorkflow)
	if err == nil {
		t.Fatalf("expected team-target rejection for zero participants, got nil")
	}
	if err.Code != 40001 || err.Message != "team_workflow 需要至少一个参与者" {
		t.Errorf("unexpected error: code=%d msg=%q", err.Code, err.Message)
	}

	// With one participant: accepted.
	if err := ValidateSnapshotScope("user1", SnapshotScopeInput{
		Topic:        "team weekly",
		Participants: []ParticipantInput{{UserID: "u1"}},
	}, TargetTeamWorkflow); err != nil {
		t.Errorf("team-target with 1 participant should pass, got %v", err)
	}
}

// TestValidateSnapshotScope_ScheduledWorkflow_SchedulePayloadRequired covers
// section 4.1 / 7.6: schedule fields must be present when the caller declares
// a scheduled_workflow target. A nil Schedule pointer signals a caller that
// forgot to plumb schedule through — a common regression risk in the design
// (patching zero-value Schedule fields).
func TestValidateSnapshotScope_ScheduledWorkflow_SchedulePayloadRequired(t *testing.T) {
	resetValidatorGlobals(t)

	err := ValidateSnapshotScope("user1", SnapshotScopeInput{
		Title:    "daily",
		Schedule: nil,
	}, TargetScheduledWorkflow)
	if err == nil {
		t.Fatalf("expected 40010 when Schedule is nil, got nil")
	}
	if err.Code != 40010 {
		t.Errorf("code: want 40010 got %d (%s)", err.Code, err.Message)
	}

	// Present but empty recurrence -> still rejected.
	err = ValidateSnapshotScope("user1", SnapshotScopeInput{
		Title:    "daily",
		Schedule: &ScheduleInput{},
	}, TargetScheduledWorkflow)
	if err == nil || err.Code != 40010 {
		t.Errorf("expected 40010 for empty recurrence, got %v", err)
	}

	// Full Schedule payload with interval_days: accepted.
	if err := ValidateSnapshotScope("user1", SnapshotScopeInput{
		Title:    "daily",
		Schedule: &ScheduleInput{IntervalDays: 1, RunTime: "09:00", TimeRangeType: 2},
	}, TargetScheduledWorkflow); err != nil {
		t.Errorf("full schedule should pass, got %v", err)
	}
}

// TestValidateSnapshotScope_ScheduledWorkflow_PreservesEveryScheduleField
// asserts that every field the validator was given on the Schedule pointer
// makes it through untouched. Section 7.6 mandates the input Schedule must
// not be mutated on either the success or failure path — a validator that
// silently zeroed a field would be the exact regression this test guards.
func TestValidateSnapshotScope_ScheduledWorkflow_PreservesEveryScheduleField(t *testing.T) {
	resetValidatorGlobals(t)

	// Deliberately non-zero on every field so a zeroing bug pops immediately.
	full := ScheduleInput{
		CronExpr:       "0 9 * * *",
		IntervalDays:   7,
		IntervalMonths: 0,
		RunTime:        "09:30",
		DayOfWeek:      3,
		DayOfMonth:     15,
		TimeRangeType:  2,
	}
	before := full // struct copy for post-call comparison

	if err := ValidateSnapshotScope("user1", SnapshotScopeInput{
		Title:    "weekly",
		Schedule: &full,
	}, TargetScheduledWorkflow); err != nil {
		t.Fatalf("full schedule should pass, got %v", err)
	}

	if full != before {
		t.Errorf("validator mutated schedule payload\n  before: %+v\n   after: %+v", before, full)
	}
}

// TestValidateSnapshotScope_AgentGeneration_ScopeSignal — the agent-generation
// target reuses the "at least one signal" rule so authors cannot walk around
// the traditional path's gate by going through the agent.
func TestValidateSnapshotScope_AgentGeneration_ScopeSignal(t *testing.T) {
	resetValidatorGlobals(t)

	err := ValidateSnapshotScope("user1", SnapshotScopeInput{Title: "t"}, TargetAgentGeneration)
	if err == nil || err.Code != 40001 {
		t.Errorf("expected 40001 for empty scope signal on agent_generation, got %v", err)
	}

	if err := ValidateSnapshotScope("user1", SnapshotScopeInput{Topic: "x"}, TargetAgentGeneration); err != nil {
		t.Errorf("topic alone should pass agent_generation, got %v", err)
	}
}

// TestValidateSnapshotScope_AgentSave_ShapeChecks covers session_id / content
// rules for the agent-save target.
func TestValidateSnapshotScope_AgentSave_ShapeChecks(t *testing.T) {
	resetValidatorGlobals(t)

	// Missing AgentSave payload: 40000.
	err := ValidateSnapshotScope("user1", SnapshotScopeInput{}, TargetAgentSave)
	if err == nil || err.Code != 40000 {
		t.Errorf("expected 40000 for missing AgentSave, got %v", err)
	}

	// Empty session id: 40000.
	err = ValidateSnapshotScope("user1", SnapshotScopeInput{
		AgentSave: &AgentSaveInput{},
	}, TargetAgentSave)
	if err == nil || err.Code != 40000 {
		t.Errorf("expected 40000 for empty session id, got %v", err)
	}

	// Empty content: 40004.
	err = ValidateSnapshotScope("user1", SnapshotScopeInput{
		AgentSave: &AgentSaveInput{SessionID: "s1", ContentLen: 0},
	}, TargetAgentSave)
	if err == nil || err.Code != 40004 {
		t.Errorf("expected 40004 for empty content, got %v", err)
	}

	// All good.
	if err := ValidateSnapshotScope("user1", SnapshotScopeInput{
		AgentSave: &AgentSaveInput{SessionID: "s1", ContentLen: 128},
	}, TargetAgentSave); err != nil {
		t.Errorf("full agent_save payload should pass, got %v", err)
	}
}

// manyValidSources returns n structurally-valid SourceInput records — used to
// exercise the source-count cap without stubbing the whole handler stack.
func manyValidSources(n int) []SourceInput {
	out := make([]SourceInput, n)
	for i := range out {
		out[i] = SourceInput{SourceType: 1, SourceID: "grp"}
	}
	return out
}
