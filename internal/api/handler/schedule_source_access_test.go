//go:build cgo

package handler

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/gin-gonic/gin"
	sqlite3 "github.com/mattn/go-sqlite3"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// The schedule source-access tests need the pipeline.GetUserChannels query set
// to run under SQLite; that query joins with COLLATE utf8mb4_unicode_ci which
// SQLite doesn't know about. Register a driver variant that knows it. Keeping
// this local (rather than importing the pipeline test-helper) avoids leaking
// _test.go symbols across packages.

var accessCollateOnce sync.Once

const accessCollateDriver = "sqlite3_handler_access_collate"

func registerAccessCollateDriver() {
	accessCollateOnce.Do(func() {
		sql.Register(accessCollateDriver, &sqlite3.SQLiteDriver{
			ConnectHook: func(conn *sqlite3.SQLiteConn) error {
				return conn.RegisterCollation("utf8mb4_unicode_ci", func(a, b string) int {
					switch {
					case a < b:
						return -1
					case a > b:
						return 1
					default:
						return 0
					}
				})
			},
		})
	})
}

// newAccessTestIMDB stands up a minimal IM schema for the source-access check.
func newAccessTestIMDB(t *testing.T) *gorm.DB {
	t.Helper()
	registerAccessCollateDriver()
	db, err := gorm.Open(sqlite.Dialector{DriverName: accessCollateDriver, DSN: ":memory:"}, &gorm.Config{})
	if err != nil {
		t.Fatalf("open access im db: %v", err)
	}
	if sqlDB, e := db.DB(); e == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	db.Exec(`CREATE TABLE "group" (group_no TEXT NOT NULL, name TEXT, space_id TEXT, status INTEGER DEFAULT 1, creator TEXT, updated_at INTEGER DEFAULT 0)`)
	db.Exec(`CREATE TABLE thread (id INTEGER PRIMARY KEY, short_id TEXT, name TEXT, group_no TEXT, status INTEGER DEFAULT 1, message_count INTEGER DEFAULT 0, creator_uid TEXT, updated_at INTEGER DEFAULT 0)`)
	db.Exec(`CREATE TABLE thread_member (thread_id INTEGER NOT NULL, uid TEXT NOT NULL)`)
	db.Exec(`CREATE TABLE group_member (group_no TEXT NOT NULL, uid TEXT NOT NULL, is_deleted INTEGER DEFAULT 0, role INTEGER DEFAULT 0)`)
	db.Exec(`CREATE TABLE conversation_extra (uid TEXT, channel_id TEXT, channel_type INTEGER, updated_at INTEGER DEFAULT 0)`)
	// Seed a single accessible group for uid1.
	db.Exec(`INSERT INTO "group" (group_no, name, status) VALUES ('grp_ok','g',1)`)
	db.Exec(`INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp_ok','u1',0)`)
	return db
}

// newAccessTestRouter wires ScheduleHandler with a real IM DB so the access
// check actually runs (in contrast to newScheduleTestRouter which uses nil imDB).
func newAccessTestRouter(db, imDB *gorm.DB) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	sh := NewScheduleHandlerWithIMDB(db, imDB, false)
	r.POST("/api/v1/summary-schedules", sh.CreateSchedule)
	r.PUT("/api/v1/summary-schedules/:id", sh.UpdateSchedule)
	return r
}

// TestCreateSchedule_SourceAccessAccept: creating a schedule with an accessible
// source succeeds and persists source_config.
func TestCreateSchedule_SourceAccessAccept(t *testing.T) {
	db := newScheduleTestDB(t)
	imDB := newAccessTestIMDB(t)
	r := newAccessTestRouter(db, imDB)
	taskID := seedScheduleTask(t, db, "T-acc-c1", "s1", "u1")

	w := scheduleReq(t, r, "u1", "s1", http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"scope": "task", "task_id": taskID,
		"interval_days": 1, "run_time": "09:00",
		"sources": []map[string]interface{}{{"source_type": 1, "source_id": "grp_ok", "source_name": "g"}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	// Confirm persisted.
	var sched model.SummarySchedule
	if err := db.Where("space_id = ?", "s1").First(&sched).Error; err != nil {
		t.Fatalf("schedule not persisted: %v", err)
	}
	if len(sched.SourceConfig) == 0 {
		t.Fatalf("expected source_config persisted, got empty")
	}
}

// TestCreateSchedule_SourceAccessDenied: creating a schedule with a source the
// user has no membership on gets 403/40017 with data.missing_sources populated.
func TestCreateSchedule_SourceAccessDenied(t *testing.T) {
	db := newScheduleTestDB(t)
	imDB := newAccessTestIMDB(t)
	r := newAccessTestRouter(db, imDB)
	taskID := seedScheduleTask(t, db, "T-acc-c2", "s1", "u1")

	w := scheduleReq(t, r, "u1", "s1", http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"scope": "task", "task_id": taskID,
		"interval_days": 1, "run_time": "09:00",
		"sources": []map[string]interface{}{{"source_type": 1, "source_id": "grp_forbidden", "source_name": "?"}},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			MissingSources []map[string]interface{} `json:"missing_sources"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, w.Body.String())
	}
	if resp.Code != 40017 {
		t.Fatalf("expected code 40017, got %d", resp.Code)
	}
	if len(resp.Data.MissingSources) != 1 {
		t.Fatalf("expected 1 missing_sources entry, got %v", resp.Data.MissingSources)
	}
	if resp.Data.MissingSources[0]["source_id"] != "grp_forbidden" {
		t.Fatalf("missing_sources[0].source_id=%v", resp.Data.MissingSources[0]["source_id"])
	}
	// Nothing persisted.
	var count int64
	db.Model(&model.SummarySchedule{}).Where("space_id = ?", "s1").Count(&count)
	if count != 0 {
		t.Fatalf("expected no schedule persisted, got %d", count)
	}
}

// TestUpdateSchedule_SourceAccessAccept: editing an existing schedule to an
// accessible source succeeds and updates source_config.
func TestUpdateSchedule_SourceAccessAccept(t *testing.T) {
	db := newScheduleTestDB(t)
	imDB := newAccessTestIMDB(t)
	r := newAccessTestRouter(db, imDB)
	taskID := seedScheduleTask(t, db, "T-acc-u1", "s1", "u1")

	// Seed a schedule owned by u1 with no sources.
	sched := model.SummarySchedule{
		SpaceID: "s1", CreatorID: "u1", Title: "t",
		SummaryMode: model.ModeByPerson, IntervalDays: 1, RunTime: "09:00",
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("seed schedule: %v", err)
	}
	// Bind schedule to the seeded task so the UpdateSchedule tx path is happy.
	db.Model(&model.SummaryTask{}).Where("id = ?", taskID).Update("schedule_id", sched.ID)

	w := scheduleReq(t, r, "u1", "s1", http.MethodPut, "/api/v1/summary-schedules/"+strconv.FormatInt(sched.ID, 10), map[string]interface{}{
		"scope": "task", "task_id": taskID,
		"sources": []map[string]interface{}{{"source_type": 1, "source_id": "grp_ok", "source_name": "g"}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var reloaded model.SummarySchedule
	if err := db.First(&reloaded, sched.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded.SourceConfig) == 0 {
		t.Fatalf("expected source_config populated, got empty")
	}
}

// TestUpdateSchedule_SourceAccessDenied: editing an existing schedule to an
// unaccessible source returns 403/40017 and does NOT modify source_config.
func TestUpdateSchedule_SourceAccessDenied(t *testing.T) {
	db := newScheduleTestDB(t)
	imDB := newAccessTestIMDB(t)
	r := newAccessTestRouter(db, imDB)
	taskID := seedScheduleTask(t, db, "T-acc-u2", "s1", "u1")

	// Seed schedule with an existing (accessible) source.
	prevSourceCfg, _ := json.Marshal([]map[string]interface{}{{"source_type": 1, "source_id": "grp_ok", "source_name": "g"}})
	sched := model.SummarySchedule{
		SpaceID: "s1", CreatorID: "u1", Title: "t",
		SummaryMode: model.ModeByPerson, IntervalDays: 1, RunTime: "09:00",
		SourceConfig: model.JSON(prevSourceCfg),
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("seed schedule: %v", err)
	}
	db.Model(&model.SummaryTask{}).Where("id = ?", taskID).Update("schedule_id", sched.ID)

	w := scheduleReq(t, r, "u1", "s1", http.MethodPut, "/api/v1/summary-schedules/"+strconv.FormatInt(sched.ID, 10), map[string]interface{}{
		"scope": "task", "task_id": taskID,
		"sources": []map[string]interface{}{{"source_type": 1, "source_id": "grp_forbidden", "source_name": "?"}},
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
	var reloaded model.SummarySchedule
	if err := db.First(&reloaded, sched.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	// source_config must remain unchanged.
	if string(reloaded.SourceConfig) != string(prevSourceCfg) {
		t.Fatalf("source_config mutated on denied update: got=%s want=%s", reloaded.SourceConfig, prevSourceCfg)
	}
}

// TestCreateSchedule_SourceAccessQueryFailure500 asserts that when the IM DB
// itself errors (fail-closed strict path), the handler surfaces HTTP 500 with
// a non-40017 code instead of leaking a false 40017. Regression guard for
// reviewer thread e0640d10.
func TestCreateSchedule_SourceAccessQueryFailure500(t *testing.T) {
	db := newScheduleTestDB(t)
	imDB := newAccessTestIMDB(t)
	// Drop the DM table so the strict helper's DM query fails; group query still
	// succeeds (source is a group), but the DM sub-query error must propagate.
	imDB.Exec(`DROP TABLE conversation_extra`)
	r := newAccessTestRouter(db, imDB)
	taskID := seedScheduleTask(t, db, "T-acc-err", "s1", "u1")

	w := scheduleReq(t, r, "u1", "s1", http.MethodPost, "/api/v1/summary-schedules", map[string]interface{}{
		"scope": "task", "task_id": taskID,
		"interval_days": 1, "run_time": "09:00",
		"sources": []map[string]interface{}{{"source_type": 1, "source_id": "grp_ok", "source_name": "g"}},
	})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on IM query failure, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, w.Body.String())
	}
	if resp.Code == 40017 {
		t.Fatalf("must not surface 40017 on IM failure (would be false-positive access denial)")
	}
}
