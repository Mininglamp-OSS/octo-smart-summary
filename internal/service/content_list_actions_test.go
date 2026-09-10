//go:build cgo

package service

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/gorm"
)

func listActionTasks(t *testing.T, db *gorm.DB) []model.SummaryTask {
	t.Helper()
	var tasks []model.SummaryTask
	requireWriteOK(t, db.Order("id").Find(&tasks).Error)
	return tasks
}

func TestListContentActionsEngineParityAndBoundedQueries(t *testing.T) {
	exerciseListContentActionsParity(t, contentTestDB(t))
}

func TestListContentActionsMySQL(t *testing.T) {
	db := contentMySQLDB(t, false)
	exerciseListContentActionsParity(t, db)
	exerciseListContentRefineOriginal(t, db)
}

func exerciseListContentActionsParity(t *testing.T, db *gorm.DB) {
	t.Helper()
	requireWriteOK(t, db.AutoMigrate(&model.SummarySource{}))
	s := configuredService(db)
	ctx := context.Background()
	for i := int64(1); i <= 30; i++ {
		target, _ := seedWritableContent(t, db, i, ContentPersonal)
		trigger := model.TriggerManual
		if i%2 == 0 {
			trigger = model.TriggerAgent
		}
		requireWriteOK(t, db.Model(&model.SummaryTask{}).Where("id = ?", i).Update("trigger_type", trigger).Error)
		if i > 2 {
			_, err := s.SaveGenerationConfiguration(ctx, "s", i, "owner", target.ID(), SaveGenerationConfigurationRequest{Spec: confirmedSpec()})
			requireWriteOK(t, err)
		}
	}
	tasks := listActionTasks(t, db)
	queries := 0
	requireWriteOK(t, db.Callback().Query().Before("gorm:query").Register("count-list-actions", func(*gorm.DB) { queries++ }))
	requireWriteOK(t, db.Callback().Row().Before("gorm:row").Register("count-list-action-rows", func(*gorm.DB) { queries++ }))
	items, err := s.ListActions(ctx, "s", "owner", tasks)
	requireWriteOK(t, err)
	if queries > 6 || len(items) != 30 {
		t.Fatalf("projection queries=%d items=%d", queries, len(items))
	}
	requireWriteOK(t, db.Callback().Query().Remove("count-list-actions"))
	requireWriteOK(t, db.Callback().Row().Remove("count-list-action-rows"))
	for _, task := range tasks {
		catalog, err := s.Catalog(ctx, "s", task.ID, "owner")
		requireWriteOK(t, err)
		item := items[task.ID]
		if item.Mode != "formal" || item.BusinessScope != "single" ||
			item.ContentID != catalog.MainContentID ||
			!reflect.DeepEqual(item.Capabilities, catalog.Contents[0].Capabilities) ||
			!reflect.DeepEqual(item.GenerationConfig, catalog.Contents[0].GenerationConfig) {
			t.Fatalf("list/catalog mismatch for %d: %+v", task.ID, item)
		}
	}
	if !reflect.DeepEqual(items[1].Capabilities, items[2].Capabilities) || !items[1].Capabilities.CanRefine ||
		items[1].Capabilities.CanRegenerateDirect || !items[3].Capabilities.CanRegenerateDirect {
		t.Fatal("engine or incomplete replay configuration changed refine semantics")
	}
	wire, err := json.Marshal(items)
	requireWriteOK(t, err)
	for _, private := range []string{"original [7]", "frozen evidence", "Summarize decisions", "input_snapshot", `"source_id"`} {
		if strings.Contains(string(wire), private) {
			t.Fatalf("list leaked private field %q", private)
		}
	}
}

func TestListContentActionsRefineOriginalForBothEngines(t *testing.T) {
	db := contentTestDB(t)
	requireWriteOK(t, db.AutoMigrate(&model.SummarySource{}))
	for id, trigger := range []int{model.TriggerManual, model.TriggerAgent} {
		seedWritableContent(t, db, int64(id+1), ContentPersonal)
		requireWriteOK(t, db.Model(&model.SummaryTask{}).Where("id = ?", id+1).Update("trigger_type", trigger).Error)
	}
	exerciseListContentRefineOriginal(t, db)
}

func exerciseListContentRefineOriginal(t *testing.T, db *gorm.DB) {
	t.Helper()
	ctx := context.Background()
	s := configuredService(db)
	tasks := listActionTasks(t, db)
	items, err := s.ListActions(ctx, "s", "owner", tasks)
	requireWriteOK(t, err)
	for _, id := range []int64{1, 2} {
		item := items[id]
		if !item.Capabilities.CanRefine {
			t.Fatalf("task %d cannot refine", id)
		}
		before, err := s.Catalog(ctx, "s", id, "owner")
		requireWriteOK(t, err)
		if item.ContentID != before.MainContentID {
			t.Fatal("list changed the original target")
		}
		run, err := s.QueueRefine(ctx, "s", id, "owner", item.ContentID, RefineContentRequest{
			ContentBaseline: baselineOf(before.Contents[0].CurrentVersion),
			IdempotencyKey:  "list-to-original-refine", Feedback: "shorten",
		})
		requireWriteOK(t, err)
		claimed, err := s.ClaimGeneration(ctx, run.ID, time.Minute)
		requireWriteOK(t, err)
		done, err := s.CompleteRefine(ctx, run.ID, claimed.ExecutionToken, "refined [7]", "test", 1)
		requireWriteOK(t, err)
		after, err := s.Catalog(ctx, "s", id, "owner")
		requireWriteOK(t, err)
		current := after.Contents[0].CurrentVersion
		if !done.Applied || current.Version != 2 || current.ContentRevision != 2 ||
			current.Content != "refined [7]" || len(current.Citations) != 1 ||
			after.MainContentID != item.ContentID || run.TaskID != id {
			t.Fatalf("refine did not append V2 to the original target: %+v", current)
		}
		target, err := ParseContentID(item.ContentID)
		requireWriteOK(t, err)
		if contentRowCount(t, db, target) != 2 {
			t.Fatal("refine did not retain exactly V1 and V2")
		}
	}
	var count int64
	requireWriteOK(t, db.Model(&model.SummaryTask{}).Count(&count).Error)
	if count != int64(len(tasks)) {
		t.Fatal("refine created another task")
	}
}

func TestListContentActionsBusyRepairAndAuthorization(t *testing.T) {
	db := contentTestDB(t)
	requireWriteOK(t, db.AutoMigrate(&model.SummarySource{}))
	ctx := context.Background()
	s := configuredService(db)
	target, current := seedWritableContent(t, db, 1, ContentPersonal)
	_, err := s.QueueRefine(ctx, "s", 1, "owner", target.ID(), RefineContentRequest{
		ContentBaseline: baselineOf(current), IdempotencyKey: "list-actions-run", Feedback: "shorten",
	})
	requireWriteOK(t, err)
	items, err := s.ListActions(ctx, "s", "owner", listActionTasks(t, db))
	requireWriteOK(t, err)
	item := items[1]
	if item.Mode != "formal" || item.Capabilities.CanEdit || item.Capabilities.CanRefine ||
		item.ActiveGeneration == nil || !item.ActiveGeneration.CanCancel {
		t.Fatalf("busy actions: %+v", item)
	}
	items, err = s.ListActions(ctx, "s", "stranger", listActionTasks(t, db))
	requireWriteOK(t, err)
	if items[1].Mode != "unavailable" || items[1].ContentID != "" || items[1].ActiveGeneration != nil {
		t.Fatal("stranger received content/run identity")
	}
	items, err = s.ListActions(ctx, "S", "owner", listActionTasks(t, db))
	requireWriteOK(t, err)
	if items[1].ContentID != "" {
		t.Fatal("tenant identity was not exact")
	}
	seedWritableContent(t, db, 2, ContentPersonal)
	requireWriteOK(t, db.Model(&model.PersonalResult{}).Where("task_id = 2").Update("current_version_id", 9999).Error)
	items, err = s.ListActions(ctx, "s", "owner", listActionTasks(t, db))
	requireWriteOK(t, err)
	if items[2].Mode != "unavailable" || items[2].Capabilities.CanEdit {
		t.Fatal("broken pointer enabled legacy fallback")
	}
}

func TestListContentActionsHistoricalPointerParity(t *testing.T) {
	db := contentTestDB(t)
	requireWriteOK(t, db.AutoMigrate(&model.SummarySource{}))
	s := configuredService(db)
	ctx := context.Background()
	target, current := seedWritableContent(t, db, 1, ContentPersonal)
	_, err := s.Edit(ctx, "s", 1, "owner", target.ID(), EditContentRequest{ContentBaseline: baselineOf(current), Content: "changed"})
	requireWriteOK(t, err)
	for _, clearPointer := range []bool{false, true} {
		if clearPointer {
			requireWriteOK(t, db.Model(&model.PersonalResult{}).Where("task_id = 1").Update("current_version_id", nil).Error)
		}
		items, err := s.ListActions(ctx, "s", "owner", listActionTasks(t, db))
		requireWriteOK(t, err)
		catalog, err := s.Catalog(ctx, "s", 1, "owner")
		requireWriteOK(t, err)
		if !reflect.DeepEqual(items[1].Capabilities, catalog.Contents[0].Capabilities) {
			t.Fatal("canonical history match differs from catalog")
		}
	}
}
