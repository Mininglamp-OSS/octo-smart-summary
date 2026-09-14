//go:build cgo

package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

func TestScheduleSourcesInheritedAndImmutable(t *testing.T) {
	db := newScheduleTestDB(t)
	r := newScheduleTestRouter(db)
	id := seedScheduleTask(t, db, "immutable-sources", "space1", "creator1")
	seedScheduleSourceFixture(t, db, id)
	body := fullScheduleReqBody(id)
	body["sources"] = []sourceReq{{SourceType: model.SourceGroup, SourceID: "other"}}
	w := scheduleReq(t, r, "creator1", "space1", "POST", "/api/v1/summary-schedules", body)
	if w.Code != http.StatusConflict {
		t.Fatalf("create with replaced source: %d %s", w.Code, w.Body)
	}
	var count int64
	db.Model(&model.SummarySchedule{}).Count(&count)
	if count != 0 {
		t.Fatal("rejected request created a schedule")
	}
	delete(body, "sources")
	w = scheduleReq(t, r, "creator1", "space1", "POST", "/api/v1/summary-schedules", body)
	if w.Code != 200 {
		t.Fatalf("inherit sources: %d %s", w.Code, w.Body)
	}
	var schedule model.SummarySchedule
	db.First(&schedule)
	var sources []sourceReq
	if err := json.Unmarshal(schedule.SourceConfig, &sources); err != nil || len(sources) != 2 || sources[0].SourceID != "grp_a" {
		t.Fatalf("did not inherit task sources: %s err=%v", schedule.SourceConfig, err)
	}
	before := string(schedule.SourceConfig)
	path := fmt.Sprintf("/api/v1/summary-schedules/%d", schedule.ID)
	for _, requested := range [][]sourceReq{
		{}, {{SourceType: model.SourceGroup, SourceID: "other"}},
		{{SourceType: model.SourceGroup, SourceID: "grp_a"}},
		{{SourceType: model.SourceDirect, SourceID: "grp_a"}, {SourceType: model.SourceGroup, SourceID: "grp_b"}},
	} {
		w = scheduleReq(t, r, "creator1", "space1", "PUT", path, map[string]interface{}{"sources": requested, "generation_instruction": "must roll back"})
		if w.Code != http.StatusConflict {
			t.Fatalf("update source: %d %s", w.Code, w.Body)
		}
		db.First(&schedule, schedule.ID)
		if string(schedule.SourceConfig) != before || schedule.GenerationInstruction != "focus on delayed items" {
			t.Fatal("rejected update changed source/instruction")
		}
	}
	// Existing detail clients may resend the same identifiers; server-owned
	// names are retained rather than accepting client display metadata.
	w = scheduleReq(t, r, "creator1", "space1", "PUT", path, map[string]interface{}{
		"sources":                []sourceReq{{SourceType: 1, SourceID: "grp_b", SourceName: "spoof"}, {SourceType: 1, SourceID: "grp_a"}},
		"generation_instruction": "new instruction",
	})
	if w.Code != 200 {
		t.Fatalf("same source set: %d %s", w.Code, w.Body)
	}
	db.First(&schedule, schedule.ID)
	if string(schedule.SourceConfig) != before || schedule.GenerationInstruction != "new instruction" {
		t.Fatal("same-source update lost source identity or instruction")
	}
}
