//go:build cgo

package handler

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	summarydb "github.com/Mininglamp-OSS/octo-smart-summary/internal/db"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// Only use an isolated, migrated test database, as with the existing MySQL
// integration tests. Scenarios roll back fixtures or explicitly clean them up.
func TestSummarySearchMySQL(t *testing.T) {
	dsn := os.Getenv("SUMMARY_MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("SUMMARY_MYSQL_TEST_DSN is not set")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"summary_task", "summary_source", "summary_participant", "summary_result", "summary_personal_result", "summary_personal_result_version", "summary_user_read", "summary_schedule"} {
		if !db.Migrator().HasTable(name) {
			t.Fatalf("test database is not migrated: %s", name)
		}
	}
	for _, tc := range []struct {
		name string
		run  func(*testing.T, *gorm.DB, *gorm.DB)
	}{
		{"matching", runSummarySearchFilteringAndMetadata},
		{"folding", runSummarySearchFoldBeforeCountAndPage},
		{"filters_and_paging", runSummarySearchBusinessFiltersAndNoResultCap},
		{"priority_and_time", runSummarySearchPriorityGroupsAndTime},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := db.Begin()
			if tx.Error != nil {
				t.Fatal(tx.Error)
			}
			defer tx.Rollback()
			tc.run(t, tx, nil)
		})
	}
	t.Run("config_only_invitee", func(t *testing.T) {
		tx := db.Begin()
		if tx.Error != nil {
			t.Fatal(tx.Error)
		}
		defer tx.Rollback()
		schedule := model.SummarySchedule{SpaceID: "space1", CreatorID: "schedule-owner", Title: "timer", SummaryMode: model.ModeByPerson,
			CronExpr: "0 0 * * *", TimeRangeType: 1, ConfirmPolicy: model.SchedConfirmRequire, IsActive: 1,
			ParticipantConfig: model.JSON(`{"participants":[{"user_id":"config-only-user","confirmed":false}],"confirm_gate_passed":false}`)}
		if err := tx.Create(&schedule).Error; err != nil {
			t.Fatal(err)
		}
		old := seedSearchTask(t, tx, "mysql-invite-old", "ＯＣＴＯ old", "topic", &schedule.ID)
		latest := seedSearchTask(t, tx, "mysql-invite-new", "different", "topic", &schedule.ID)
		if err := tx.Model(&model.SummaryTask{}).Where("id IN ?", []int64{old.ID, latest.ID}).Update("creator_id", "schedule-owner").Error; err != nil {
			t.Fatal(err)
		}
		r := setupListRouter(NewTaskHandler(tx, nil, ""))
		data := requestSearchList(t, r, "/api/v1/summaries?keyword=octo", "config-only-user", "space1")
		if data.Total != 1 || len(data.Items) != 1 || data.Items[0].TaskID != old.ID || !*data.Items[0].Historical {
			t.Fatalf("config-only search: %+v", data)
		}
		if data.AttentionCount != 1 {
			t.Fatalf("invitation badge changed: %+v", data)
		}
	})
	// The single list statement establishes a snapshot without a long transaction.
	t.Run("single_statement_snapshot", func(t *testing.T) {
		no := "snapshot-" + time.Now().Format("150405.000000000")
		task := seedSearchTask(t, db, no, "snapshot needle", "topic", nil)
		t.Cleanup(func() {
			db.Where("task_id = ?", task.ID).Delete(&model.SummaryParticipant{})
			db.Where("id = ?", task.ID).Delete(&model.SummaryTask{})
		})
		// Commit a title edit on another connection immediately after the
		// list query establishes its snapshot. Count/page/highlight must
		// still describe the same original match, not the now-edited title.
		var once sync.Once
		var editErr error
		const hook = "f6_test_edit_after_search_scan"
		if err := db.Callback().Row().After("gorm:row").Register(hook, func(query *gorm.DB) {
			if strings.HasPrefix(query.Statement.SQL.String(), "WITH matched AS (") {
				once.Do(func() {
					editErr = db.Model(&model.SummaryTask{}).Where("id = ?", task.ID).Update("title", "edited after snapshot").Error
				})
			}
		}); err != nil {
			t.Fatal(err)
		}
		defer db.Callback().Row().Remove(hook)
		r := setupListRouter(NewTaskHandler(db, nil, ""))
		data := requestSearchList(t, r, "/api/v1/summaries?keyword=snapshot", "search-user", "space1")
		if data.Total != 1 || data.Items[0].TaskID != task.ID || len(data.Items[0].SearchMatch.TitleRanges) == 0 {
			t.Fatalf("snapshot search: %+v", data)
		}
		if editErr != nil {
			t.Fatal(editErr)
		}
		var saved model.SummaryTask
		if err := db.First(&saved, task.ID).Error; err != nil {
			t.Fatal(err)
		}
		if saved.Title != "edited after snapshot" {
			t.Fatal("concurrent title edit was not exercised")
		}
	})
	for _, mutation := range []string{"revoke", "delete"} {
		for _, timing := range []string{"before", "after"} {
			t.Run(mutation+"_"+timing+"_list_statement", func(t *testing.T) {
				keyword := "race-" + mutation + timing
				task := seedSearchTask(t, db, keyword, keyword, "topic", nil)
				if err := db.Model(&task).Update("creator_id", "another-owner").Error; err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					db.Where("task_id = ?", task.ID).Delete(&model.SummaryParticipant{})
					db.Where("id = ?", task.ID).Delete(&model.SummaryTask{})
				})
				var once sync.Once
				var mutationErr error
				const hook = "f6_permission_race"
				callback := db.Callback().Row().Before("gorm:row")
				if timing == "after" {
					callback = db.Callback().Row().After("gorm:row")
				}
				err := callback.Register(hook, func(query *gorm.DB) {
					if !strings.HasPrefix(query.Statement.SQL.String(), "WITH matched AS (") {
						return
					}
					once.Do(func() {
						if mutation == "revoke" {
							mutationErr = db.Where("task_id = ? AND user_id = ?", task.ID, "search-user").Delete(&model.SummaryParticipant{}).Error
						} else {
							mutationErr = db.Model(&task).Update("deleted_at", time.Now()).Error
						}
					})
				})
				if err != nil {
					t.Fatal(err)
				}
				defer db.Callback().Row().Remove(hook)
				h := NewTaskHandler(db, nil, "")
				r := setupListRouter(h)
				r.GET("/api/v1/summaries/:id", h.GetSummary)
				r.GET("/api/v1/summaries/:id/result", h.GetResult)
				data := requestSearchList(t, r, "/api/v1/summaries?keyword="+keyword, "search-user", "space1")
				if mutationErr != nil {
					t.Fatal(mutationErr)
				}
				want := int64(0)
				if timing == "after" {
					want = 1
				}
				if data.Total != want || int64(len(data.Items)) != want {
					t.Fatalf("snapshot total/items: %+v", data)
				}
				// An in-flight statement may complete from its original snapshot;
				// subsequent list/detail/result requests must honor the committed change.
				if next := requestSearchList(t, r, "/api/v1/summaries?keyword="+keyword, "search-user", "space1"); next.Total != 0 || len(next.Items) != 0 {
					t.Fatalf("revoked/deleted task returned: %+v", next)
				}
				for _, suffix := range []string{"", "/result"} {
					w := doRequestWithSpace(r, http.MethodGet, fmt.Sprintf("/api/v1/summaries/%d%s", task.ID, suffix), "search-user", "space1")
					wantStatus := http.StatusForbidden
					if mutation == "delete" {
						wantStatus = http.StatusNotFound
					}
					if w.Code != wantStatus {
						t.Fatalf("permission changed: %d %s", w.Code, w.Body.String())
					}
				}
			})
		}
	}
	t.Run("restartable_backfill", func(t *testing.T) {
		rows := make([]model.SummaryTask, 501)
		for i := range rows {
			rows[i] = model.SummaryTask{TaskNo: fmt.Sprintf("backfill-%d", i), Title: "Ｓtraße", Topic: " ", SpaceID: "backfill", CreatorID: "backfill", TimeRangeStart: time.Now(), TimeRangeEnd: time.Now()}
		}
		if err := db.CreateInBatches(&rows, 100).Error; err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Where("space_id = ?", "backfill").Delete(&model.SummaryTask{}) })
		// Fixed historical values expose ON UPDATE CURRENT_TIMESTAMP; freshly
		// created rows can mask a rewrite within the same second.
		if err := db.Exec("UPDATE summary_task SET search_title = NULL, search_topic = NULL, created_at = ?, updated_at = ? WHERE space_id = ?", "2019-01-02 03:04:05", "2020-02-03 04:05:06", "backfill").Error; err != nil {
			t.Fatal(err)
		}
		var before model.SummaryTask
		if err := db.First(&before, rows[0].ID).Error; err != nil {
			t.Fatal(err)
		}
		sqlDB, err := db.DB()
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if err := summarydb.BackfillSummarySearch(context.Background(), sqlDB); err != nil {
				t.Fatal(err)
			}
		}
		var count int64
		if err := db.Model(&model.SummaryTask{}).Where("space_id = ? AND search_title = ? AND search_topic = ?", "backfill", "strasse", "").Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 501 {
			t.Fatalf("backfilled %d rows", count)
		}
		var after model.SummaryTask
		if err := db.First(&after, rows[0].ID).Error; err != nil {
			t.Fatal(err)
		}
		if !after.UpdatedAt.Equal(before.UpdatedAt) || !after.CreatedAt.Equal(before.CreatedAt) {
			t.Fatal("backfill changed timestamps")
		}
		if err := db.Model(&model.SummaryTask{}).Where("space_id = ? AND (created_at <> ? OR updated_at <> ?)", "backfill", before.CreatedAt, before.UpdatedAt).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("backfill changed timestamps on %d rows", count)
		}
		// Blank-topic fallback must not retain an old title after later edits.
		if err := db.Model(&rows[0]).Update("title", "ＮＥＷ").Error; err != nil {
			t.Fatal(err)
		}
		var saved model.SummaryTask
		if err := db.First(&saved, rows[0].ID).Error; err != nil {
			t.Fatal(err)
		}
		if saved.SearchTitle != "new" || saved.SearchTopic != "" {
			t.Fatalf("backfill/update mismatch: %+v", saved)
		}
	})
}
