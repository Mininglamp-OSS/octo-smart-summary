package handler

import (
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// PR#62 r7 Blocker2: ToggleSchedule re-enable is the 4th single-person entry
// point and must apply the same stored-config subset guard.

func TestPR62Round7_Toggle_ReenableDirtyConfig_Rejected(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	sched := model.SummarySchedule{
		SpaceID:           "space1",
		CreatorID:         "creator1",
		Title:             "Dirty",
		SummaryMode:       model.ModeByPerson,
		IntervalDays:      1,
		RunTime:           "17:00",
		IsActive:          0,
		ParticipantConfig: mustParticipantConfig(t, [2]string{"creator1", "C"}, [2]string{"stranger", "S"}),
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create sched: %v", err)
	}
	// is_active has DB default 1; force the disabled start state.
	db.Model(&sched).Update("is_active", 0)

	w := doScheduleJSONRequest(t, r, http.MethodPut, "/api/v1/summary-schedules/"+itoa(sched.ID)+"/toggle", map[string]interface{}{
		"is_active": true,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got model.SummarySchedule
	db.First(&got, sched.ID)
	if got.IsActive != 0 {
		t.Fatalf("dirty schedule must stay inactive, got is_active=%d", got.IsActive)
	}
}

func TestPR62Round7_Toggle_ReenableCleanConfig_Allowed(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	cases := []struct {
		name string
		cfg  model.JSON
	}{
		{"empty config", nil},
		{"creator-only config", mustParticipantConfig(t, [2]string{"creator1", "C"})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sched := model.SummarySchedule{
				SpaceID:           "space1",
				CreatorID:         "creator1",
				Title:             "Clean",
				SummaryMode:       model.ModeByPerson,
				IntervalDays:      1,
				RunTime:           "17:00",
				IsActive:          0,
				ParticipantConfig: tc.cfg,
			}
			if err := db.Create(&sched).Error; err != nil {
				t.Fatalf("create sched: %v", err)
			}
			db.Model(&sched).Update("is_active", 0)
			w := doScheduleJSONRequest(t, r, http.MethodPut, "/api/v1/summary-schedules/"+itoa(sched.ID)+"/toggle", map[string]interface{}{
				"is_active": true,
			})
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			var got model.SummarySchedule
			db.First(&got, sched.ID)
			if got.IsActive != 1 {
				t.Fatalf("clean schedule must be active, got is_active=%d", got.IsActive)
			}
		})
	}
}

// Disable path is never guarded (lets a stuck schedule be turned off).
func TestPR62Round7_Toggle_DisableDirtyConfig_Allowed(t *testing.T) {
	db := setupScheduleDB(t)
	h := NewScheduleHandler(db)
	r := setupScheduleRouter(h)

	sched := model.SummarySchedule{
		SpaceID:           "space1",
		CreatorID:         "creator1",
		Title:             "DirtyActive",
		SummaryMode:       model.ModeByPerson,
		IntervalDays:      1,
		RunTime:           "17:00",
		IsActive:          1,
		ParticipantConfig: mustParticipantConfig(t, [2]string{"stranger", "S"}),
	}
	if err := db.Create(&sched).Error; err != nil {
		t.Fatalf("create sched: %v", err)
	}
	w := doScheduleJSONRequest(t, r, http.MethodPut, "/api/v1/summary-schedules/"+itoa(sched.ID)+"/toggle", map[string]interface{}{
		"is_active": false,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got model.SummarySchedule
	db.First(&got, sched.ID)
	if got.IsActive != 0 {
		t.Fatalf("disable must succeed, got is_active=%d", got.IsActive)
	}
}
