//go:build cgo

package handler

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// This integration test is opt-in because the schedule roster predicate uses
// MySQL JSON_TABLE, which SQLite cannot parse. Point SUMMARY_MYSQL_TEST_DSN at
// an isolated database to exercise the production dialect.
func TestListSummaries_MySQL_ConfigOnlyScheduleInviteeGetsAttention(t *testing.T) {
	dsn := os.Getenv("SUMMARY_MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("SUMMARY_MYSQL_TEST_DSN is not set")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatalf("open mysql: %v", err)
	}
	for _, table := range []string{
		"summary_task", "summary_source", "summary_participant", "summary_result",
		"summary_personal_result", "summary_personal_result_version", "summary_user_read", "summary_schedule",
	} {
		if !db.Migrator().HasTable(table) {
			t.Fatalf("mysql integration database is not migrated: missing %s", table)
		}
	}

	tx := db.Begin()
	if tx.Error != nil {
		t.Fatalf("begin transaction: %v", tx.Error)
	}
	defer tx.Rollback()

	schedule := model.SummarySchedule{
		SpaceID: "attention-mysql-space", CreatorID: "schedule-creator", Title: "scheduled",
		SummaryMode: model.ModeByPerson, CronExpr: "0 0 * * *", TimeRangeType: 1,
		ParticipantConfig: model.JSON(`{"participants":[{"user_id":"config-only-user","confirmed":false}],"confirm_gate_passed":false}`),
		ConfirmPolicy:     model.SchedConfirmRequire, IsActive: 1,
	}
	if err := tx.Create(&schedule).Error; err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	task := model.SummaryTask{
		TaskNo: "MYSQL-CONFIG-ONLY-INVITEE", SpaceID: schedule.SpaceID,
		CreatorID: schedule.CreatorID, SummaryMode: model.ModeByPerson,
		Status: model.StatusCompleted, TriggerType: model.TriggerScheduled, ScheduleID: &schedule.ID,
		TimeRangeStart: time.Now().Add(-time.Hour), TimeRangeEnd: time.Now(),
	}
	if err := tx.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	// CONFIRM intentionally materializes only confirmed users. The invitee has
	// no summary_participant row and must still be admitted through the schedule
	// roster predicate.
	if err := tx.Create(&model.SummaryParticipant{
		TaskID: task.ID, UserID: schedule.CreatorID, Status: model.ParticipantAccepted,
	}).Error; err != nil {
		t.Fatalf("create creator participant: %v", err)
	}

	r := setupListRouter(NewTaskHandler(tx, nil, ""))
	w := doRequestWithSpace(r, http.MethodGet, "/api/v1/summaries", "config-only-user", schedule.SpaceID)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Code int `json:"code"`
		Data struct {
			Total                  int `json:"total"`
			AttentionCount         int `json:"attention_count"`
			PendingInvitationCount int `json:"pending_invitation_count"`
			Items                  []struct {
				TaskID               int64 `json:"task_id"`
				HasPendingInvitation bool  `json:"has_pending_invitation"`
				NeedsAttention       bool  `json:"needs_attention"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v; body=%s", err, w.Body.String())
	}
	if resp.Data.Total != 1 || len(resp.Data.Items) != 1 || resp.Data.Items[0].TaskID != task.ID {
		t.Fatalf("config-only invitee did not receive scheduled task: %+v", resp.Data)
	}
	if !resp.Data.Items[0].HasPendingInvitation || !resp.Data.Items[0].NeedsAttention {
		t.Fatalf("scheduled invitation was not marked for attention: %+v", resp.Data.Items[0])
	}
	if resp.Data.AttentionCount != 1 || resp.Data.PendingInvitationCount != 1 {
		t.Fatalf("unexpected attention counts: attention=%d pending=%d", resp.Data.AttentionCount, resp.Data.PendingInvitationCount)
	}
}
