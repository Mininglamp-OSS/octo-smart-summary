package worker

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newReplaceTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(
		&model.SummaryTask{},
		&model.SummaryResult{},
		&model.SummaryChunk{},
		&model.SummaryParticipant{},
		&model.PersonalResult{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func seedProcessingTask(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo: "T", CreatorID: "u1", SummaryMode: model.ModeByPerson,
		Status: model.StatusProcessing, TimeRangeStart: now, TimeRangeEnd: now,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("seed task: %v", err)
	}
	return task.ID
}

func countResults(t *testing.T, db *gorm.DB, taskID int64) int64 {
	t.Helper()
	var n int64
	db.Model(&model.SummaryResult{}).Where("task_id = ?", taskID).Count(&n)
	return n
}

// markTaskCompleted is a CAS: it only transitions a task that is still
// Processing, and reports a sentinel error otherwise so a stale worker cannot
// re-complete a task that was already reset/cancelled.
func TestMarkTaskCompleted_CASOnlyFromProcessing(t *testing.T) {
	db := newReplaceTestDB(t)
	taskID := seedProcessingTask(t, db)

	if err := completeTaskWithoutNewResult(db, taskID); err != nil {
		t.Fatalf("first complete should succeed: %v", err)
	}
	var task model.SummaryTask
	db.First(&task, taskID)
	if task.Status != model.StatusCompleted {
		t.Fatalf("status = %d, want Completed", task.Status)
	}
	// Second attempt: task is no longer Processing -> sentinel error.
	if err := completeTaskWithoutNewResult(db, taskID); err != errTaskNoLongerProcessing {
		t.Fatalf("second complete err = %v, want errTaskNoLongerProcessing", err)
	}
}

// Scheduled runs overwrite in place: after inserting the new result, prior
// auto-generated results are pruned so only the latest remains.
func TestSaveLatestResult_ScheduledPrunesPriorAutoVersions(t *testing.T) {
	db := newReplaceTestDB(t)
	taskID := seedProcessingTask(t, db)

	// An existing auto-generated prior-cycle result (not hand-edited).
	db.Create(&model.SummaryResult{TaskID: taskID, Content: "old", Version: 1, GeneratedAt: time.Now()})
	db.Create(&model.SummaryChunk{TaskID: taskID, ChunkSummary: "old chunk"})

	newRes := &model.SummaryResult{Content: "new", GeneratedAt: time.Now()}
	if err := saveLatestResultAndCompleteTask(db, taskID, newRes, true); err != nil {
		t.Fatalf("save scheduled: %v", err)
	}
	if got := countResults(t, db, taskID); got != 1 {
		t.Fatalf("scheduled run should keep only the latest result, got %d", got)
	}
	var remaining model.SummaryResult
	db.Where("task_id = ?", taskID).First(&remaining)
	if remaining.Content != "new" {
		t.Errorf("remaining result content = %q, want \"new\"", remaining.Content)
	}
	// Chunks are cleaned up on scheduled overwrite.
	var chunkCount int64
	db.Model(&model.SummaryChunk{}).Where("task_id = ?", taskID).Count(&chunkCount)
	if chunkCount != 0 {
		t.Errorf("scheduled run should clear stale chunks, got %d", chunkCount)
	}
}

// Hand-edited results (edited_at set) are user data and must survive a
// scheduled overwrite, even though auto versions are pruned.
func TestSaveLatestResult_ScheduledKeepsHandEditedResult(t *testing.T) {
	db := newReplaceTestDB(t)
	taskID := seedProcessingTask(t, db)

	edited := time.Now()
	db.Create(&model.SummaryResult{TaskID: taskID, Content: "user edit", Version: 1, EditedAt: &edited, GeneratedAt: time.Now()})

	newRes := &model.SummaryResult{Content: "auto new", GeneratedAt: time.Now()}
	if err := saveLatestResultAndCompleteTask(db, taskID, newRes, true); err != nil {
		t.Fatalf("save scheduled: %v", err)
	}
	// Both the hand-edited row and the new row remain.
	if got := countResults(t, db, taskID); got != 2 {
		t.Fatalf("hand-edited result must be retained alongside new, got %d rows", got)
	}
	var editedCount int64
	db.Model(&model.SummaryResult{}).Where("task_id = ? AND edited_at IS NOT NULL", taskID).Count(&editedCount)
	if editedCount != 1 {
		t.Errorf("hand-edited result count = %d, want 1", editedCount)
	}
}

// Manual (non-scheduled) runs keep full version history.
func TestSaveLatestResult_ManualKeepsVersionHistory(t *testing.T) {
	db := newReplaceTestDB(t)
	taskID := seedProcessingTask(t, db)
	db.Create(&model.SummaryResult{TaskID: taskID, Content: "v1", Version: 1, GeneratedAt: time.Now()})

	newRes := &model.SummaryResult{Content: "v2", GeneratedAt: time.Now()}
	if err := saveLatestResultAndCompleteTask(db, taskID, newRes, false); err != nil {
		t.Fatalf("save manual: %v", err)
	}
	if got := countResults(t, db, taskID); got != 2 {
		t.Fatalf("manual run should keep version history, got %d", got)
	}
	if newRes.Version != 2 {
		t.Errorf("new result version = %d, want 2", newRes.Version)
	}
}

// The scheduled participant sync always (re)materializes the creator as an
// accepted participant and de-duplicates repeated user ids in the config.
func TestSyncScheduledTaskParticipants_AlwaysIncludesCreatorAndDedups(t *testing.T) {
	db := newReplaceTestDB(t)
	now := time.Now().UTC()
	task := model.SummaryTask{TaskNo: "T", CreatorID: "creator", SummaryMode: model.ModeByPerson, Status: model.StatusProcessing, TimeRangeStart: now, TimeRangeEnd: now}
	db.Create(&task)

	// Config lists the creator twice plus duplicates; result must be unique
	// with the creator present exactly once.
	raw := model.JSON(`[{"user_id":"creator","user_name":"C"},{"user_id":"creator"}]`)
	if err := db.Transaction(func(tx *gorm.DB) error {
		return syncScheduledTaskParticipants(tx, task, raw, now)
	}); err != nil {
		t.Fatalf("sync participants: %v", err)
	}

	var parts []model.SummaryParticipant
	db.Where("task_id = ?", task.ID).Find(&parts)
	if len(parts) != 1 {
		t.Fatalf("expected 1 deduped participant, got %d", len(parts))
	}
	if parts[0].UserID != "creator" || parts[0].Status != model.ParticipantAccepted {
		t.Errorf("participant = %+v, want creator/accepted", parts[0])
	}
}

// buildScheduledTaskSources must ALWAYS re-resolve the source_name from the IM
// DB and never trust the source_name carried in the schedule config (issue #93
// / #94 follow-up: the schedule-management UI can submit a stale/dirty name,
// e.g. a raw "groupNo____shortId", so the stored name must stay consistent with
// the instant-summary path, which also ignores the client value).
func TestBuildScheduledTaskSources_IgnoresConfigSourceName(t *testing.T) {
	db := newReplaceTestDB(t)
	if err := db.AutoMigrate(&model.SummarySource{}); err != nil {
		t.Fatalf("migrate source: %v", err)
	}
	taskID := seedProcessingTask(t, db)

	// Config carries a dirty/stale name. With imDB=nil the resolver falls back
	// to the deterministic placeholder; the key assertion is that the stored
	// name is the RE-RESOLVED value and is NOT the config-supplied dirty value.
	dirty := "脏值-不可信(群聊)"
	raw := model.JSON(`[{"source_type":1,"source_id":"group-abcdef123456","source_name":"` + dirty + `"}]`)
	if err := db.Transaction(func(tx *gorm.DB) error {
		return buildScheduledTaskSources(tx, nil, taskID, raw)
	}); err != nil {
		t.Fatalf("build sources: %v", err)
	}

	var src model.SummarySource
	db.Where("task_id = ?", taskID).First(&src)
	if src.SourceName == dirty {
		t.Errorf("source_name = %q, must NOT trust config dirty value", src.SourceName)
	}
	// imDB=nil -> ResolveSourceNameWithType returns the fallback placeholder.
	want := "来源-group-ab(群聊)"
	if src.SourceName != want {
		t.Errorf("source_name = %q, want re-resolved %q", src.SourceName, want)
	}
}

// When the config has no source_name and no IM DB is available, the function
// falls back to the placeholder name (documents the legacy degradation path so
// the fallback contract is locked in).
func TestBuildScheduledTaskSources_FallbackWhenNoNameAndNoIMDB(t *testing.T) {
	db := newReplaceTestDB(t)
	if err := db.AutoMigrate(&model.SummarySource{}); err != nil {
		t.Fatalf("migrate source: %v", err)
	}
	taskID := seedProcessingTask(t, db)

	raw := model.JSON(`[{"source_type":1,"source_id":"group-abcdef123456"}]`)
	if err := db.Transaction(func(tx *gorm.DB) error {
		return buildScheduledTaskSources(tx, nil, taskID, raw)
	}); err != nil {
		t.Fatalf("build sources: %v", err)
	}

	var src model.SummarySource
	db.Where("task_id = ?", taskID).First(&src)
	want := "来源-group-ab(群聊)"
	if src.SourceName != want {
		t.Errorf("source_name = %q, want fallback %q", src.SourceName, want)
	}
}
