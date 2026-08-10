package service

// snapshot_validator_test.go — SUM-BE1 shared validator tests (revised per
// SUM-9). These tests exercise each of the three exported entry points
// (ValidatePersonalWorkflow / ValidateScheduledWorkflow / ValidateAgentSave)
// directly, with no gorm / gin / DB coupling. handler-level regression tests
// that exercise Schedule field preservation across create + patch live in
// internal/api/handler/schedule_scope_preservation_test.go (CGO-gated,
// matching every other handler test in this repo).

import (
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// ------------------------------ actor gate ------------------------------

func TestValidatePersonalWorkflow_ActorRequired(t *testing.T) {
	err := ValidatePersonalWorkflow(
		"", "title", "topic",
		model.SnapshotScope{ChannelIDs: []string{"grp1"}},
		1,
		"", 0,
		time.Time{}, time.Time{},
		31,
	)
	if err == nil || err.Code != 40100 || err.HTTPStatus != 401 {
		t.Fatalf("expected 40100/401 for empty actor, got %v", err)
	}
}

func TestValidateScheduledWorkflow_ActorRequired(t *testing.T) {
	err := ValidateScheduledWorkflow(
		"", "daily",
		model.SnapshotScope{},
		"", 1, 0,
	)
	if err == nil || err.Code != 40100 {
		t.Fatalf("expected 40100 for empty actor on schedule, got %v", err)
	}
}

func TestValidateAgentSave_ActorRequired(t *testing.T) {
	err := ValidateAgentSave("", "title", "sess1", "hi", "", 0, 0, 0)
	if err == nil || err.Code != 40100 {
		t.Fatalf("expected 40100 for empty actor on agent_save, got %v", err)
	}
}

// ------------------------------ base checks -----------------------------

func TestValidatePersonalWorkflow_BaseChecks(t *testing.T) {
	tooLong := strings.Repeat("总", MaxSummaryTitleRunes+1)

	tests := []struct {
		name     string
		title    string
		topic    string
		scope    model.SnapshotScope
		srcCount int
		origID   string
		origType int
		start    time.Time
		end      time.Time
		maxDays  int
		wantCode int
		wantMsg  string
	}{
		{
			name:  "title over 2300 runes rejected",
			title: tooLong, topic: "x",
			scope:    model.SnapshotScope{},
			maxDays:  31,
			wantCode: 40001, wantMsg: "title 不能超过 2300 字符",
		},
		{
			name:  "topic over 2300 runes rejected",
			title: "t", topic: tooLong,
			scope:    model.SnapshotScope{},
			maxDays:  31,
			wantCode: 40001, wantMsg: "topic 不能超过 2300 字符",
		},
		{
			name:  "origin_channel_id set but type out of range",
			title: "t", topic: "x",
			scope:  model.SnapshotScope{},
			origID: "grp1", origType: 99,
			maxDays:  31,
			wantCode: 40001, wantMsg: "origin_channel_type must be 1, 2, or 3 when origin_channel_id is set",
		},
		{
			name:  "origin_channel_type set but no id",
			title: "t", topic: "x",
			scope:    model.SnapshotScope{},
			origType: 1,
			maxDays:  31,
			wantCode: 40001, wantMsg: "origin_channel_id is required when origin_channel_type is set",
		},
		{
			name:  "sources over max count",
			title: "t", topic: "x",
			scope:    model.SnapshotScope{},
			srcCount: MaxSummarySourceCount + 1,
			maxDays:  31,
			wantCode: 40003, wantMsg: "信息来源不能超过30个",
		},
		{
			name:  "time range over cap",
			title: "t", topic: "x",
			scope:    model.SnapshotScope{ChannelIDs: []string{"g1"}},
			start:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			end:      time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), // ~59 days > 31
			maxDays:  31,
			wantCode: 40002, wantMsg: "时间范围不能超过31天",
		},
		{
			name:  "scope has no signal, no topic, no time range",
			title: "t", topic: "",
			scope:    model.SnapshotScope{},
			maxDays:  31,
			wantCode: 40001, wantMsg: "至少提供 sources、topic 或 time_range 之一",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidatePersonalWorkflow(
				"u1",
				tc.title, tc.topic,
				tc.scope,
				tc.srcCount,
				tc.origID, tc.origType,
				tc.start, tc.end,
				tc.maxDays,
			)
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

// TestValidatePersonalWorkflow_SignalSources verifies the three shapes that
// satisfy the scope-signal gate: channel_ids in the shared scope, topic
// string, or an explicit time range on the scope.
func TestValidatePersonalWorkflow_SignalSources(t *testing.T) {
	cases := []struct {
		name  string
		topic string
		scope model.SnapshotScope
		start time.Time
		end   time.Time
	}{
		{
			name:  "channel_ids in scope alone",
			scope: model.SnapshotScope{ChannelIDs: []string{"g1"}},
		},
		{
			name:  "topic alone",
			topic: "hello",
			scope: model.SnapshotScope{},
		},
		{
			name: "time range in scope alone",
			scope: model.SnapshotScope{
				TimeRange: model.TimeRangeJSON{Start: "2026-01-01T00:00:00Z", End: "2026-01-02T00:00:00Z"},
			},
		},
		{
			name:  "explicit time endpoints alone (no scope signal, no topic)",
			scope: model.SnapshotScope{},
			start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			end:   time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The "explicit time endpoints alone" case relies on scope-signal
			// being satisfied by the caller providing a time range on the
			// shared scope. Populate the scope's TimeRange to reflect what
			// handler.CreateSummary does today.
			scope := tc.scope
			if !tc.start.IsZero() {
				scope.TimeRange = model.TimeRangeJSON{
					Start: tc.start.Format(time.RFC3339),
					End:   tc.end.Format(time.RFC3339),
				}
			}
			err := ValidatePersonalWorkflow(
				"u1", "title", tc.topic,
				scope, 0, "", 0,
				tc.start, tc.end,
				31,
			)
			if err != nil {
				t.Errorf("expected nil, got %v", err)
			}
		})
	}
}

// TestValidatePersonalWorkflow_NoTimeRangeSpanCapWhenAbsent guarantees the
// pre-refactor behaviour: when the caller does NOT provide an explicit
// range (both endpoints zero), the validator does not enforce the cap.
// This is why CreateSummary's default range (end=now, start=now-maxDays)
// still passes for topic-only payloads.
func TestValidatePersonalWorkflow_NoTimeRangeSpanCapWhenAbsent(t *testing.T) {
	err := ValidatePersonalWorkflow(
		"u1", "t", "topic",
		model.SnapshotScope{},
		0, "", 0,
		time.Time{}, time.Time{},
		31,
	)
	if err != nil {
		t.Errorf("expected nil when no explicit range and topic supplied, got %v", err)
	}
}

// ------------------------- scheduled_workflow ---------------------------

func TestValidateScheduledWorkflow_RecurrenceRequired(t *testing.T) {
	err := ValidateScheduledWorkflow(
		"u1", "daily",
		model.SnapshotScope{},
		"", 0, 0,
	)
	if err == nil || err.Code != 40010 {
		t.Fatalf("expected 40010 for empty recurrence, got %v", err)
	}

	// interval_days > 0 passes.
	if err := ValidateScheduledWorkflow("u1", "daily", model.SnapshotScope{}, "", 1, 0); err != nil {
		t.Errorf("interval_days=1 should pass, got %v", err)
	}
	// cron_expr passes.
	if err := ValidateScheduledWorkflow("u1", "daily", model.SnapshotScope{}, "0 9 * * *", 0, 0); err != nil {
		t.Errorf("cron_expr should pass, got %v", err)
	}
	// interval_months passes.
	if err := ValidateScheduledWorkflow("u1", "monthly", model.SnapshotScope{}, "", 0, 1); err != nil {
		t.Errorf("interval_months=1 should pass, got %v", err)
	}
}

func TestValidateScheduledWorkflow_TitleCapEnforced(t *testing.T) {
	err := ValidateScheduledWorkflow(
		"u1", strings.Repeat("总", MaxSummaryTitleRunes+1),
		model.SnapshotScope{},
		"", 1, 0,
	)
	if err == nil || err.Code != 40001 || err.Message != "title 不能超过 2300 字符" {
		t.Fatalf("expected 40001 title over cap, got %v", err)
	}
}

// ---------------------------- agent_save --------------------------------

func TestValidateAgentSave_ContentAndSession(t *testing.T) {
	// empty session id -> 40000
	err := ValidateAgentSave("u1", "t", "", "content", "", 0, 0, 0)
	if err == nil || err.Code != 40000 {
		t.Fatalf("expected 40000 for empty session id, got %v", err)
	}
	// empty content -> 40004
	err = ValidateAgentSave("u1", "t", "sess1", "", "", 0, 0, 0)
	if err == nil || err.Code != 40004 {
		t.Fatalf("expected 40004 for empty content, got %v", err)
	}
	if err.Message != "session 无有效产出,请先在对话中生成总结再保存" {
		t.Errorf("agent_save empty-content msg mismatch: %q", err.Message)
	}
	// valid -> nil
	if err := ValidateAgentSave("u1", "t", "sess1", "hello", "", 0, 0, 0); err != nil {
		t.Errorf("expected nil for valid agent_save, got %v", err)
	}
}

func TestValidateAgentSave_OriginCoupling(t *testing.T) {
	// origin_channel_type out of range with id -> 40001
	err := ValidateAgentSave("u1", "t", "sess1", "hello", "grp1", 99, 0, 0)
	if err == nil || err.Code != 40001 {
		t.Fatalf("expected 40001, got %v", err)
	}
	// type without id -> 40001
	err = ValidateAgentSave("u1", "t", "sess1", "hello", "", 2, 0, 0)
	if err == nil || err.Code != 40001 {
		t.Fatalf("expected 40001, got %v", err)
	}
	// both zero -> nil
	if err := ValidateAgentSave("u1", "t", "sess1", "hello", "", 0, 0, 0); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	// coupled correctly -> nil
	if err := ValidateAgentSave("u1", "t", "sess1", "hello", "grp1", 1, 0, 0); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

// ---------------------------- agent_save (BE-2 additions) ---------------
//
// BE-2 extends the signature with agent_message_id + expected_snapshot_version
// so the caller can supply the trusted draft reference the design (section
// 6.5) requires. The tests below cover the four invariants the validator now
// enforces at the shape level; DB-side ownership / role / session-identity
// checks stay in the handler (loadAgentMessageForSave) and are exercised by
// the handler cgo tests.

func TestValidateAgentSave_MessageIDAndSnapshotVersionMustBePaired(t *testing.T) {
	// message id without version -> 40001
	err := ValidateAgentSave("u1", "t", "sess1", "hello", "", 0, 42, 0)
	if err == nil || err.Code != 40001 {
		t.Fatalf("expected 40001 for msg_id without version, got %v", err)
	}
	// version without message id -> 40001
	err = ValidateAgentSave("u1", "t", "sess1", "hello", "", 0, 0, 1)
	if err == nil || err.Code != 40001 {
		t.Fatalf("expected 40001 for version without msg_id, got %v", err)
	}
	// both zero (legacy) -> nil
	if err := ValidateAgentSave("u1", "t", "sess1", "hello", "", 0, 0, 0); err != nil {
		t.Errorf("expected nil for both-zero legacy path, got %v", err)
	}
	// both set correctly (v1 == AgentSaveExpectedSnapshotVersion) -> nil
	if err := ValidateAgentSave("u1", "t", "sess1", "hello", "", 0, 42, AgentSaveExpectedSnapshotVersion); err != nil {
		t.Errorf("expected nil for paired msg_id+v1, got %v", err)
	}
}

func TestValidateAgentSave_NegativeMessageIDRejected(t *testing.T) {
	err := ValidateAgentSave("u1", "t", "sess1", "hello", "", 0, -1, 0)
	if err == nil || err.Code != 40001 {
		t.Fatalf("expected 40001 for negative msg_id, got %v", err)
	}
}

func TestValidateAgentSave_SnapshotVersionMustBeV1(t *testing.T) {
	// version 2 while BE-2 only writes v1 -> AGENT_DRAFT_STALE (40901 / 409)
	err := ValidateAgentSave("u1", "t", "sess1", "hello", "", 0, 42, 2)
	if err == nil {
		t.Fatalf("expected error for version=2, got nil")
	}
	if err.Code != 40901 {
		t.Errorf("expected AGENT_DRAFT_STALE code 40901, got %d", err.Code)
	}
	if err.HTTPStatus != 409 {
		t.Errorf("expected HTTP 409 for stale draft, got %d", err.HTTPStatus)
	}
	// version 1 -> nil
	if err := ValidateAgentSave("u1", "t", "sess1", "hello", "", 0, 42, AgentSaveExpectedSnapshotVersion); err != nil {
		t.Errorf("expected nil for v=%d, got %v", AgentSaveExpectedSnapshotVersion, err)
	}
}

func TestValidateAgentSave_ActorStillRequiredWithNewParams(t *testing.T) {
	err := ValidateAgentSave("", "t", "sess1", "hello", "", 0, 42, 1)
	if err == nil || err.Code != 40100 {
		t.Fatalf("expected 40100 for empty actor even with paired msg_id, got %v", err)
	}
}
