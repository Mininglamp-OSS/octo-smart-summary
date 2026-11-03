//go:build cgo

package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type searchListItem struct {
	TaskID      int64               `json:"task_id"`
	CreatedAt   string              `json:"created_at"`
	SearchMatch *SummarySearchMatch `json:"search_match"`
	Historical  *bool               `json:"is_historical_match"`
}

type searchListData struct {
	Total          int64            `json:"total"`
	AttentionCount int64            `json:"attention_count"`
	Items          []searchListItem `json:"items"`
}

func requestSearchList(t *testing.T, router *gin.Engine, path, user, space string) searchListData {
	t.Helper()
	w := doRequestWithSpace(router, http.MethodGet, path, user, space)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Code int            `json:"code"`
		Data searchListData `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Code != 0 {
		t.Fatalf("list: %s", w.Body.String())
	}
	return resp.Data
}

func seedSearchTask(t *testing.T, db *gorm.DB, no, title, topic string, schedule *int64) model.SummaryTask {
	t.Helper()
	now := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	task := model.SummaryTask{TaskNo: no, SpaceID: "space1", CreatorID: "search-user", Title: title, Topic: topic,
		SummaryMode: model.ModeByPerson, Status: model.StatusCompleted, TriggerType: model.TriggerManual,
		ScheduleID: schedule, CreatedAt: now, UpdatedAt: now, TimeRangeStart: now.Add(-time.Hour), TimeRangeEnd: now}
	if schedule != nil {
		task.TriggerType = model.TriggerScheduled
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: task.CreatorID, UserName: "Search User", Status: model.ParticipantAccepted}).Error; err != nil {
		t.Fatal(err)
	}
	return task
}

func searchItemIDs(data searchListData) []int64 {
	ids := make([]int64, 0, len(data.Items))
	for _, item := range data.Items {
		ids = append(ids, item.TaskID)
	}
	return ids
}

func TestSummarySearchFilteringAndMetadata(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	runSummarySearchFilteringAndMetadata(t, db, imDB)
}

func runSummarySearchFilteringAndMetadata(t *testing.T, db, imDB *gorm.DB) {
	r := setupListRouter(NewTaskHandler(db, imDB, ""))
	title := seedSearchTask(t, db, "search-title", "😀ＯＣＴＯ v1.2", "visible topic", nil)
	topic := seedSearchTask(t, db, "search-topic", "other title", "octo topic", nil)
	hidden := seedSearchTask(t, db, "search-hidden", "unrelated", "visible", nil)
	secret := "octo secret-instruction"
	db.Model(&hidden).Updates(map[string]interface{}{"generation_requirement": secret, "trigger_type": model.TriggerAgent})
	db.Create(&model.SummaryResult{TaskID: hidden.ID, Content: "octo report-only", GeneratedAt: hidden.CreatedAt})
	db.Create(&model.SummarySource{TaskID: hidden.ID, SourceType: model.SourceGroup, SourceID: "hidden-source", SourceName: "octo source-only", Derived: true})
	private := seedSearchTask(t, db, "search-private", "octo", "octo", nil)
	db.Model(&private).Update("creator_id", "other-user")
	db.Where("task_id = ?", private.ID).Delete(&model.SummaryParticipant{})
	cross := seedSearchTask(t, db, "search-cross", "octo", "octo", nil)
	db.Model(&cross).Update("space_id", "space2")
	deleted := seedSearchTask(t, db, "search-deleted", "octo", "octo", nil)
	db.Model(&deleted).Update("deleted_at", deleted.CreatedAt)

	data := requestSearchList(t, r, "/api/v1/summaries?keyword="+url.QueryEscape(" ＯｃＴｏ "), "search-user", "space1")
	if data.Total != 2 || !reflect.DeepEqual(searchItemIDs(data), []int64{topic.ID, title.ID}) {
		t.Fatalf("unexpected matches: %+v", data)
	}
	if !reflect.DeepEqual(data.Items[1].SearchMatch.TitleRanges, [][2]int{{1, 5}}) || len(data.Items[1].SearchMatch.TopicRanges) != 0 {
		t.Fatalf("title offsets: %+v", data.Items[1])
	}
	if !reflect.DeepEqual(data.Items[0].SearchMatch.TopicRanges, [][2]int{{0, 4}}) || len(data.Items[0].SearchMatch.TitleRanges) != 0 {
		t.Fatalf("topic offsets: %+v", data.Items[0])
	}
	if data.Items[0].Historical == nil || *data.Items[0].Historical {
		t.Fatal("manual task marked historical")
	}

	for _, q := range []string{"v12", "%", "_", "report-only", "secret-instruction", "source-only", "titleocto"} {
		got := requestSearchList(t, r, "/api/v1/summaries?keyword="+url.QueryEscape(q), "search-user", "space1")
		if got.Total != 0 {
			t.Errorf("%q matched %v", q, searchItemIDs(got))
		}
	}
	// MySQL's default accent-insensitive collation must not broaden Go matches.
	seedSearchTask(t, db, "search-accent", "café", "Straße", nil)
	if got := requestSearchList(t, r, "/api/v1/summaries?keyword=cafe", "search-user", "space1"); got.Total != 0 {
		t.Fatalf("collation broadened literal match: %+v", got)
	}
	if got := requestSearchList(t, r, "/api/v1/summaries?keyword=STRASSE", "search-user", "space1"); got.Total != 1 {
		t.Fatalf("persisted Unicode case fold mismatch: %+v", got)
	}
	literal := seedSearchTask(t, db, "search-literal", `100%_v1.2\done`, "other", nil)
	for _, q := range []string{"%", "_", `\`, "v1.2"} {
		got := requestSearchList(t, r, "/api/v1/summaries?keyword="+url.QueryEscape(q), "search-user", "space1")
		if q != "v1.2" && !reflect.DeepEqual(searchItemIDs(got), []int64{literal.ID}) {
			t.Errorf("literal %q matched %v", q, searchItemIDs(got))
		}
	}
	for _, q := range []string{"", " \t "} {
		got := requestSearchList(t, r, "/api/v1/summaries?keyword="+url.QueryEscape(q), "search-user", "space1")
		for _, item := range got.Items {
			if item.SearchMatch != nil || item.Historical != nil {
				t.Fatal("browse carries search metadata")
			}
		}
	}
	for _, n := range []int{100, 101} {
		w := doRequestWithSpace(r, http.MethodGet, "/api/v1/summaries?keyword="+url.QueryEscape(strings.Repeat("😀", n)), "search-user", "space1")
		want := http.StatusOK
		if n == 101 {
			want = http.StatusBadRequest
		}
		if w.Code != want {
			t.Errorf("%d runes: %d %s", n, w.Code, w.Body.String())
		}
	}
}

func TestSummarySearchFoldBeforeCountAndPage(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	runSummarySearchFoldBeforeCountAndPage(t, db, imDB)
}

func runSummarySearchFoldBeforeCountAndPage(t *testing.T, db, imDB *gorm.DB) {
	h := NewTaskHandler(db, imDB, "")
	r := setupListRouter(h)
	r.GET("/api/v1/summaries/:id", h.GetSummary)
	r.GET("/api/v1/summaries/:id/result", h.GetResult)
	schedule := int64(99)
	seedSearchTask(t, db, "fold-oldest", "matching", "old", &schedule)
	old := seedSearchTask(t, db, "fold-old", "matching", "old", &schedule)
	latest := seedSearchTask(t, db, "fold-latest", "not related", "new", &schedule)
	manual := seedSearchTask(t, db, "fold-manual", "matching", "manual", nil)
	data := requestSearchList(t, r, "/api/v1/summaries?keyword=matching&page_size=1", "search-user", "space1")
	if data.Total != 2 || !reflect.DeepEqual(searchItemIDs(data), []int64{manual.ID}) {
		t.Fatalf("page 1: %+v", data)
	}
	data = requestSearchList(t, r, "/api/v1/summaries?keyword=matching&page_size=1&page=2", "search-user", "space1")
	if data.Total != 2 || !reflect.DeepEqual(searchItemIDs(data), []int64{old.ID}) || data.Items[0].Historical == nil || !*data.Items[0].Historical {
		t.Fatalf("page 2: %+v", data)
	}
	if data.Items[0].CreatedAt != old.CreatedAt.Format(time.RFC3339) {
		t.Fatal("historical creation date changed")
	}
	for _, task := range []model.SummaryTask{old, latest} {
		if err := db.Create(&model.SummaryResult{TaskID: task.ID, Content: task.TaskNo, Version: 1, GeneratedAt: task.CreatedAt}).Error; err != nil {
			t.Fatal(err)
		}
	}
	// Following the returned task_id must read the matching old run's report,
	// even though this schedule has a newer run with a different report.
	matchedID := data.Items[0].TaskID
	for _, suffix := range []string{"", "/result"} {
		w := doRequestWithSpace(r, http.MethodGet, fmt.Sprintf("/api/v1/summaries/%d%s", matchedID, suffix), "search-user", "space1")
		if w.Code != http.StatusOK {
			t.Fatalf("open matched run: %d %s", w.Code, w.Body.String())
		}
		var response struct {
			Data struct {
				TaskID  int64  `json:"task_id"`
				Content string `json:"content"`
				Result  struct {
					Content string `json:"content"`
				} `json:"result"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if suffix == "" {
			if response.Data.TaskID != old.ID || response.Data.Result.Content != old.TaskNo {
				t.Fatalf("detail opened another run: %s", w.Body.String())
			}
		} else if response.Data.Content != old.TaskNo {
			t.Fatalf("result opened another run: %s", w.Body.String())
		}
	}
	data = requestSearchList(t, r, "/api/v1/summaries?keyword=matching&page_size=1&page=3", "search-user", "space1")
	if data.Total != 2 || len(data.Items) != 0 {
		t.Fatalf("last page: %+v", data)
	}
	// A newer invisible run must neither displace a visible run nor leak its existence.
	db.Model(&latest).Update("creator_id", "other-user")
	db.Where("task_id = ?", latest.ID).Delete(&model.SummaryParticipant{})
	data = requestSearchList(t, r, "/api/v1/summaries?keyword=matching", "search-user", "space1")
	if *data.Items[1].Historical {
		t.Fatal("invisible newer run leaked into historical flag")
	}
	// Manual task id and schedule id occupy independent namespaces.
	db.Model(&old).Update("schedule_id", manual.ID)
	data = requestSearchList(t, r, "/api/v1/summaries?keyword=matching", "search-user", "space1")
	if data.Total != 3 {
		t.Fatalf("manual/schedule identity collision: %+v", data)
	}
}

func TestSummarySearchBusinessFiltersAndNoResultCap(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	runSummarySearchBusinessFiltersAndNoResultCap(t, db, imDB)
}

func runSummarySearchBusinessFiltersAndNoResultCap(t *testing.T, db, imDB *gorm.DB) {
	r := setupListRouter(NewTaskHandler(db, imDB, ""))
	schedule := int64(77)
	old := seedSearchTask(t, db, "filters-old", "needle", "topic", &schedule)
	newer := seedSearchTask(t, db, "filters-new", "needle", "topic", &schedule)
	db.Model(&old).Updates(map[string]interface{}{"status": model.StatusFailed, "origin_channel_id": "group-a"})
	db.Model(&newer).Updates(map[string]interface{}{"origin_channel_id": "group-b"})
	for _, filter := range []string{fmt.Sprintf("status=%d", model.StatusFailed), "origin_channel_id=group-a"} {
		data := requestSearchList(t, r, "/api/v1/summaries?keyword=needle&"+filter, "search-user", "space1")
		if data.Total != 1 || data.Items[0].TaskID != old.ID {
			t.Fatalf("filter %s: %+v", filter, data)
		}
	}
	db.Model(&old).Update("origin_from_derived", true)
	data := requestSearchList(t, r, "/api/v1/summaries?keyword=needle&origin_channel_id=group-a", "search-user", "space1")
	if data.Total != 0 {
		t.Fatal("derived origin became searchable")
	}
	for i := 0; i < 205; i++ {
		seedSearchTask(t, db, fmt.Sprintf("bulk-%03d", i), "bulk needle", "topic", nil)
	}
	var all []int64
	for page := 1; page <= 3; page++ {
		data := requestSearchList(t, r, fmt.Sprintf("/api/v1/summaries?keyword=bulk&page_size=100&page=%d", page), "search-user", "space1")
		if data.Total != 205 {
			t.Fatalf("total capped: %d", data.Total)
		}
		all = append(all, searchItemIDs(data)...)
	}
	if len(all) != 205 {
		t.Fatalf("missing results: %d", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i] >= all[i-1] {
			t.Fatalf("unstable/duplicate page: %v", all)
		}
	}
}

func TestSummarySearchPriorityGroupsAndTime(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	runSummarySearchPriorityGroupsAndTime(t, db, imDB)
}

func runSummarySearchPriorityGroupsAndTime(t *testing.T, db, imDB *gorm.DB) {
	r := setupListRouter(NewTaskHandler(db, imDB, ""))
	base := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	seed := func(no string, offset int) model.SummaryTask {
		task := seedSearchTask(t, db, no, "needle", "topic", nil)
		task.CreatedAt = base.Add(time.Duration(offset) * time.Hour)
		if err := db.Model(&task).Update("created_at", task.CreatedAt).Error; err != nil {
			t.Fatal(err)
		}
		return task
	}
	unread := func(task model.SummaryTask, generated time.Time, read bool) {
		result := model.SummaryResult{TaskID: task.ID, Content: "report", GeneratedAt: generated, CreatedAt: generated, UpdatedAt: generated}
		if err := db.Create(&result).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Model(&task).Update("current_result_id", result.ID).Error; err != nil {
			t.Fatal(err)
		}
		if read {
			if err := db.Create(&model.SummaryUserRead{TaskID: task.ID, UserID: "search-user", LastReadTeamResultID: &result.ID}).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	pending := func(task model.SummaryTask) {
		if err := db.Model(&model.SummaryParticipant{}).Where("task_id = ? AND user_id = ?", task.ID, "search-user").Update("status", model.ParticipantPending).Error; err != nil {
			t.Fatal(err)
		}
	}
	submit := func(task model.SummaryTask) {
		if err := db.Create(&model.SummaryParticipant{TaskID: task.ID, UserID: "teammate", UserName: "Teammate", Status: model.ParticipantAccepted}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&model.PersonalResult{TaskID: task.ID, UserID: "search-user", WorkerStatus: model.PersonalStatusCompleted, Content: "draft"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	oldInvite := seed("priority-old-invite", 0)
	pending(oldInvite)
	submit(oldInvite)
	unread(oldInvite, base, false)
	newInvite := seed("priority-new-invite", 1)
	pending(newInvite)
	oldSubmit := seed("priority-old-submit", 0)
	submit(oldSubmit)
	unread(oldSubmit, base, false)
	newSubmit := seed("priority-new-submit", 1)
	submit(newSubmit)
	unreadTask := seed("priority-unread", 2)
	unread(unreadTask, base.Add(2*time.Hour), false)
	other := seed("priority-other", 9)
	activityOld := seed("priority-activity-old", 0)
	unread(activityOld, base.Add(10*time.Hour), true)
	activityNew := seed("priority-activity-new", 1)
	unread(activityNew, base.Add(10*time.Hour), true)
	activityTie := seed("priority-activity-tie", 1)
	unread(activityTie, base.Add(10*time.Hour), true)
	want := []int64{newInvite.ID, oldInvite.ID, newSubmit.ID, oldSubmit.ID, unreadTask.ID, activityTie.ID, activityNew.ID, activityOld.ID, other.ID}
	for _, query := range []string{"", "?keyword=needle", "?keyword=needle&sort_by=updated_at&sort_order=asc"} {
		data := requestSearchList(t, r, "/api/v1/summaries"+query, "search-user", "space1")
		if !reflect.DeepEqual(searchItemIDs(data), want) {
			t.Fatalf("priority/time %s: got %v want %v", query, searchItemIDs(data), want)
		}
		if data.Total != 9 || data.AttentionCount != 5 {
			t.Fatalf("counts %s: %+v", query, data)
		}
	}
	// The navigation badge remains Space-wide, independent of search totals.
	data := requestSearchList(t, r, "/api/v1/summaries?keyword=no-match", "search-user", "space1")
	if data.Total != 0 || data.AttentionCount != 5 {
		t.Fatalf("search changed badge counts: %+v", data)
	}
}
