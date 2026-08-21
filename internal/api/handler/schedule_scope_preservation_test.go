//go:build cgo

package handler

// schedule_scope_preservation_test.go — SUM-BE1 regression per SUM-9 P1-3.
//
// This suite drives the real handler.CreateSchedule + handler.UpdateSchedule
// endpoints (in-memory sqlite via the same harness used by
// schedule_behavior_test.go) and asserts that every schedule field the
// createScheduleReq carries survives the round trip, AND that a field-level
// patch through UpdateSchedule does not zero any field the caller did not
// send. The SUM-9 review specifically rejected the previous "only-read
// helper does not mutate the pointer" style test as not covering the
// handler / persistence / patch path.
//
// CGO tag matches every other handler test in this repo — the in-memory
// sqlite driver github.com/mattn/go-sqlite3 requires cgo.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// fullScheduleReqBody is the shape createScheduleReq exposes on the wire.
// Kept as a helper so both create + update tests share the same field set.
func fullScheduleReqBody(taskID int64) map[string]interface{} {
	return map[string]interface{}{
		"title":                  "weekly review",
		"generation_instruction": "focus on delayed items",
		"cron_expr":              "",
		"interval_days":          7,
		"interval_months":        0,
		"run_time":               "09:30",
		"day_of_week":            3,
		"day_of_month":           0,
		"time_range_type":        2,
		"sources": []map[string]interface{}{
			{"source_type": 1, "source_id": "grp_a"},
			{"source_type": 1, "source_id": "grp_b"},
		},
		"participants": []map[string]interface{}{
			{"user_id": "creator1"},
		},
		"confirm_policy": 0, // AUTO
		"scope":          "task",
		"task_id":        taskID,
	}
}

// TestCreateSchedule_PersistsEveryField creates a schedule with every
// createScheduleReq field non-zero and asserts the persisted row keeps them
// all. This is the "no zero-fill on create" invariant SUM-9 P1-3 asked for.
func TestCreateSchedule_PersistsEveryField(t *testing.T) {
	db := newScheduleTestDB(t)
	r := newScheduleTestRouter(db)

	taskID := seedScheduleTask(t, db, "TSK-CREATE-FULL", "space1", "creator1")

	body := fullScheduleReqBody(taskID)
	w := scheduleReq(t, r, "creator1", "space1", http.MethodPost, "/api/v1/summary-schedules", body)
	if w.Code != http.StatusOK {
		t.Fatalf("CreateSchedule http=%d body=%s", w.Code, w.Body.String())
	}

	// Pull the row and assert every field lines up with what we sent.
	var got model.SummarySchedule
	if err := db.Where("space_id = ? AND creator_id = ?", "space1", "creator1").
		Order("id DESC").First(&got).Error; err != nil {
		t.Fatalf("load schedule: %v", err)
	}

	if got.Title != "weekly review" {
		t.Errorf("Title: got %q", got.Title)
	}
	if got.GenerationInstruction != "focus on delayed items" {
		t.Errorf("GenerationInstruction: got %q", got.GenerationInstruction)
	}
	if got.CronExpr != "" {
		t.Errorf("CronExpr: expected empty, got %q", got.CronExpr)
	}
	if got.IntervalDays != 7 {
		t.Errorf("IntervalDays: got %d", got.IntervalDays)
	}
	if got.IntervalMonths != 0 {
		t.Errorf("IntervalMonths: got %d", got.IntervalMonths)
	}
	if got.RunTime != "09:30" {
		t.Errorf("RunTime: got %q", got.RunTime)
	}
	if got.DayOfWeek != 3 {
		t.Errorf("DayOfWeek: got %d", got.DayOfWeek)
	}
	if got.DayOfMonth != 0 {
		t.Errorf("DayOfMonth: got %d", got.DayOfMonth)
	}
	if got.TimeRangeType != 2 {
		t.Errorf("TimeRangeType: got %d", got.TimeRangeType)
	}
	if len(got.SourceConfig) == 0 {
		t.Errorf("SourceConfig empty; expected the 2 sources we sent to persist")
	}
	// SourceConfig is JSON; decode to a slice and assert both source ids
	// made it through.
	var sources []map[string]interface{}
	if err := json.Unmarshal(got.SourceConfig, &sources); err != nil {
		t.Fatalf("SourceConfig not valid JSON: %v (%s)", err, string(got.SourceConfig))
	}
	if len(sources) != 2 {
		t.Errorf("SourceConfig len: got %d want 2", len(sources))
	}
	if len(got.ParticipantConfig) == 0 {
		t.Errorf("ParticipantConfig empty; expected creator1 to persist")
	}
	if got.ConfirmPolicy != model.SchedConfirmAuto {
		t.Errorf("ConfirmPolicy: got %d want AUTO(%d)", got.ConfirmPolicy, model.SchedConfirmAuto)
	}
	if got.SummaryMode != model.ModeByPerson {
		t.Errorf("SummaryMode: got %d want ModeByPerson", got.SummaryMode)
	}
	if got.IsActive != 1 {
		t.Errorf("IsActive: got %d want 1", got.IsActive)
	}
	if got.NextRunAt == nil {
		t.Errorf("NextRunAt not populated")
	}

	// The task must have been bound to this schedule (TaskID -> schedule_id
	// coupling).
	var task model.SummaryTask
	if err := db.First(&task, taskID).Error; err != nil {
		t.Fatalf("load task: %v", err)
	}
	if task.ScheduleID == nil || *task.ScheduleID != got.ID {
		var got0 int64
		if task.ScheduleID != nil {
			got0 = *task.ScheduleID
		}
		t.Errorf("task.ScheduleID: got %d want %d", got0, got.ID)
	}
}

// TestUpdateSchedule_TitleOnlyPatch_DoesNotZeroOtherFields is the SUM-9
// requirement literally: send an update that only sets Title, then assert
// every other field the caller did NOT send retains its prior value. This
// is the "field-level patch does not clear unshown fields" invariant.
func TestUpdateSchedule_TitleOnlyPatch_DoesNotZeroOtherFields(t *testing.T) {
	db := newScheduleTestDB(t)
	r := newScheduleTestRouter(db)

	taskID := seedScheduleTask(t, db, "TSK-PATCH-1", "space1", "creator1")
	create := fullScheduleReqBody(taskID)
	w := scheduleReq(t, r, "creator1", "space1", http.MethodPost, "/api/v1/summary-schedules", create)
	if w.Code != http.StatusOK {
		t.Fatalf("CreateSchedule http=%d body=%s", w.Code, w.Body.String())
	}

	var before model.SummarySchedule
	if err := db.Where("space_id = ? AND creator_id = ?", "space1", "creator1").
		Order("id DESC").First(&before).Error; err != nil {
		t.Fatalf("load schedule: %v", err)
	}

	// Title-only patch.
	patch := map[string]interface{}{"title": "renamed schedule"}
	w = scheduleReq(t, r, "creator1", "space1", http.MethodPut,
		"/api/v1/summary-schedules/"+sid(before.ID), patch)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateSchedule http=%d body=%s", w.Code, w.Body.String())
	}

	var after model.SummarySchedule
	if err := db.First(&after, before.ID).Error; err != nil {
		t.Fatalf("reload schedule: %v", err)
	}

	// Title changed.
	if after.Title != "renamed schedule" {
		t.Errorf("Title patch failed: %q", after.Title)
	}
	// Every other user-facing field preserved.
	if after.GenerationInstruction != before.GenerationInstruction {
		t.Errorf("GenerationInstruction zeroed: before=%q after=%q",
			before.GenerationInstruction, after.GenerationInstruction)
	}
	if after.CronExpr != before.CronExpr {
		t.Errorf("CronExpr zeroed: before=%q after=%q", before.CronExpr, after.CronExpr)
	}
	if after.IntervalDays != before.IntervalDays {
		t.Errorf("IntervalDays zeroed: before=%d after=%d", before.IntervalDays, after.IntervalDays)
	}
	if after.IntervalMonths != before.IntervalMonths {
		t.Errorf("IntervalMonths zeroed: before=%d after=%d", before.IntervalMonths, after.IntervalMonths)
	}
	if after.RunTime != before.RunTime {
		t.Errorf("RunTime zeroed: before=%q after=%q", before.RunTime, after.RunTime)
	}
	if after.DayOfWeek != before.DayOfWeek {
		t.Errorf("DayOfWeek zeroed: before=%d after=%d", before.DayOfWeek, after.DayOfWeek)
	}
	if after.DayOfMonth != before.DayOfMonth {
		t.Errorf("DayOfMonth zeroed: before=%d after=%d", before.DayOfMonth, after.DayOfMonth)
	}
	if after.TimeRangeType != before.TimeRangeType {
		t.Errorf("TimeRangeType zeroed: before=%d after=%d", before.TimeRangeType, after.TimeRangeType)
	}
	if string(after.SourceConfig) != string(before.SourceConfig) {
		t.Errorf("SourceConfig zeroed:\n  before=%s\n   after=%s",
			string(before.SourceConfig), string(after.SourceConfig))
	}
	if string(after.ParticipantConfig) != string(before.ParticipantConfig) {
		t.Errorf("ParticipantConfig zeroed:\n  before=%s\n   after=%s",
			string(before.ParticipantConfig), string(after.ParticipantConfig))
	}
	if after.ConfirmPolicy != before.ConfirmPolicy {
		t.Errorf("ConfirmPolicy zeroed: before=%d after=%d", before.ConfirmPolicy, after.ConfirmPolicy)
	}
	if after.SummaryMode != before.SummaryMode {
		t.Errorf("SummaryMode zeroed: before=%d after=%d", before.SummaryMode, after.SummaryMode)
	}
}

// TestUpdateSchedule_GenerationInstructionOnly is a companion to the
// title-only case: the instruction path has its own normalize step (see
// normalizeScheduleGenerationInstruction in schedule.go) and used to be
// prone to writing an empty string on any patch. Assert it round-trips and
// leaves everything else alone.
func TestUpdateSchedule_GenerationInstructionOnly(t *testing.T) {
	db := newScheduleTestDB(t)
	r := newScheduleTestRouter(db)

	taskID := seedScheduleTask(t, db, "TSK-PATCH-2", "space1", "creator1")
	w := scheduleReq(t, r, "creator1", "space1", http.MethodPost, "/api/v1/summary-schedules",
		fullScheduleReqBody(taskID))
	if w.Code != http.StatusOK {
		t.Fatalf("CreateSchedule http=%d body=%s", w.Code, w.Body.String())
	}

	var before model.SummarySchedule
	if err := db.Where("space_id = ? AND creator_id = ?", "space1", "creator1").
		Order("id DESC").First(&before).Error; err != nil {
		t.Fatalf("load schedule: %v", err)
	}

	patch := map[string]interface{}{"generation_instruction": "focus on incidents only"}
	w = scheduleReq(t, r, "creator1", "space1", http.MethodPut,
		"/api/v1/summary-schedules/"+sid(before.ID), patch)
	if w.Code != http.StatusOK {
		t.Fatalf("UpdateSchedule http=%d body=%s", w.Code, w.Body.String())
	}

	var after model.SummarySchedule
	if err := db.First(&after, before.ID).Error; err != nil {
		t.Fatalf("reload schedule: %v", err)
	}

	if after.GenerationInstruction != "focus on incidents only" {
		t.Errorf("instruction patch failed: %q", after.GenerationInstruction)
	}
	if after.Title != before.Title {
		t.Errorf("Title zeroed by instruction-only patch: before=%q after=%q", before.Title, after.Title)
	}
	if after.IntervalDays != before.IntervalDays || after.RunTime != before.RunTime ||
		after.DayOfWeek != before.DayOfWeek || after.TimeRangeType != before.TimeRangeType {
		t.Errorf("recurrence fields zeroed by instruction-only patch:\n  before=%+v\n   after=%+v", before, after)
	}
	if string(after.SourceConfig) != string(before.SourceConfig) ||
		string(after.ParticipantConfig) != string(before.ParticipantConfig) {
		t.Errorf("source/participant configs zeroed by instruction-only patch")
	}
}
