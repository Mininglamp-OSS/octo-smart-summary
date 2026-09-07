//go:build cgo

package handler

import (
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// ---------------------------------------------------------------------------
// Pentest 4.11 — empty-X-Space-Id fail-closed lock-in for the schedule reads.
//
// ListSchedules / GetSchedule already short-circuit spaceID=="" -> 40008/404
// (schedule.go). These tests lock that gate against regression using a genuine
// space_id='' schedule row: a header-less read must 404, never surface the
// anomalous row.
// ---------------------------------------------------------------------------

func setupScheduleReadRouter(db *gorm.DB) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	sh := NewScheduleHandler(db)
	r.GET("/api/v1/summary-schedules", sh.ListSchedules)
	r.GET("/api/v1/summary-schedules/:id", sh.GetSchedule)
	return r
}

// seedEmptySpaceSchedule creates a schedule whose SpaceID is the empty string.
func seedEmptySpaceSchedule(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	sched := model.SummarySchedule{
		SpaceID:   "", // <-- anomalous space_id='' row the gate must hide
		CreatorID: "creator1",
		Title:     "empty-space schedule",
		IsActive:  1,
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("seed empty-space schedule: %v", err)
	}
	return sched.ID
}

// 4.11: ListSchedules with NO X-Space-Id must 404/40008, never returning the
// space_id=” schedule.
func TestListSchedules_EmptySpaceHeader_Returns404(t *testing.T) {
	db := newScheduleTestDB(t)
	seedEmptySpaceSchedule(t, db)
	r := setupScheduleReadRouter(db)

	w := scheduleReq(t, r, "creator1", "", http.MethodGet, "/api/v1/summary-schedules", nil)
	assertCrossSpace404(t, w)
}

// 4.11: GetSchedule with NO X-Space-Id must 404/40008, never returning the
// space_id=” schedule.
func TestGetSchedule_EmptySpaceHeader_Returns404(t *testing.T) {
	db := newScheduleTestDB(t)
	schedID := seedEmptySpaceSchedule(t, db)
	r := setupScheduleReadRouter(db)

	w := scheduleReq(t, r, "creator1", "", http.MethodGet, "/api/v1/summary-schedules/"+sid(schedID), nil)
	assertCrossSpace404(t, w)
}

// No-regression inverse: the SAME creator reading with the CORRECT space header
// still gets a 200 (the gate does not over-block in-space schedule reads).
func TestSchedule_EmptySpaceGate_NoRegressionInSpace(t *testing.T) {
	db := newScheduleTestDB(t)
	sched := model.SummarySchedule{SpaceID: "space1", CreatorID: "creator1", Title: "s", IsActive: 1}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("seed in-space schedule: %v", err)
	}
	r := setupScheduleReadRouter(db)

	wList := scheduleReq(t, r, "creator1", "space1", http.MethodGet, "/api/v1/summary-schedules", nil)
	if wList.Code != http.StatusOK {
		t.Fatalf("in-space ListSchedules must be 200, got %d: %s", wList.Code, wList.Body.String())
	}
	wGet := scheduleReq(t, r, "creator1", "space1", http.MethodGet, "/api/v1/summary-schedules/"+sid(sched.ID), nil)
	if wGet.Code != http.StatusOK {
		t.Fatalf("in-space GetSchedule must be 200, got %d: %s", wGet.Code, wGet.Body.String())
	}
}
