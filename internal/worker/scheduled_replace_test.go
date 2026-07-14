//go:build cgo

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

// Scheduled runs append a new result version on the same task. Prior result
// rows are retained for the version-history UI, while stale chunks are cleared.
func TestSaveLatestResult_ScheduledKeepsPriorVersions(t *testing.T) {
	db := newReplaceTestDB(t)
	taskID := seedProcessingTask(t, db)

	// An existing auto-generated prior-cycle result (not hand-edited).
	db.Create(&model.SummaryResult{TaskID: taskID, Content: "old", Version: 1, GeneratedAt: time.Now()})
	db.Create(&model.SummaryChunk{TaskID: taskID, ChunkSummary: "old chunk"})

	newRes := &model.SummaryResult{Content: "new", GeneratedAt: time.Now()}
	if err := saveLatestResultAndCompleteTask(db, taskID, newRes, true, nil); err != nil {
		t.Fatalf("save scheduled: %v", err)
	}
	if got := countResults(t, db, taskID); got != 2 {
		t.Fatalf("scheduled run should keep prior versions, got %d", got)
	}
	var latest model.SummaryResult
	db.Where("task_id = ?", taskID).Order("version DESC, id DESC").First(&latest)
	if latest.Content != "new" || latest.OperationType != "scheduled_generate" {
		t.Errorf("latest result = content %q operation %q, want new/scheduled_generate", latest.Content, latest.OperationType)
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
	if err := saveLatestResultAndCompleteTask(db, taskID, newRes, true, nil); err != nil {
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
	if err := saveLatestResultAndCompleteTask(db, taskID, newRes, false, nil); err != nil {
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

// seedSubmittedContributor inserts a submitted personal_result row (a contributor
// that participated in the meta aggregation) and returns its ID.
func seedSubmittedContributor(t *testing.T, db *gorm.DB, taskID int64, userID string) int64 {
	t.Helper()
	now := time.Now().UTC()
	pr := model.PersonalResult{
		TaskID:       taskID,
		UserID:       userID,
		WorkerStatus: model.PersonalStatusCompleted,
		SubmittedAt:  &now,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := db.Create(&pr).Error; err != nil {
		t.Fatalf("seed submitted contributor: %v", err)
	}
	return pr.ID
}

// P1 race guard happy path: when the in-memory snapshot of contributors still
// matches the committed (submitted) rows at write time, the meta result is
// written and the task is completed.
func TestSaveLatestResult_RosterUnchanged_Writes(t *testing.T) {
	db := newReplaceTestDB(t)
	taskID := seedProcessingTask(t, db)

	idA := seedSubmittedContributor(t, db, taskID, "uA")
	idB := seedSubmittedContributor(t, db, taskID, "uB")
	snapshot := []int64{idA, idB}

	newRes := &model.SummaryResult{Content: "team summary", GeneratedAt: time.Now()}
	if err := saveLatestResultAndCompleteTask(db, taskID, newRes, false, snapshot); err != nil {
		t.Fatalf("save with unchanged roster should succeed: %v", err)
	}
	if got := countResults(t, db, taskID); got != 1 {
		t.Fatalf("expected 1 written result, got %d", got)
	}
	var task model.SummaryTask
	db.First(&task, taskID)
	if task.Status != model.StatusCompleted {
		t.Errorf("task status = %d, want Completed", task.Status)
	}
}

// P1 race guard: if a contributor's personal_result is deleted (Leave/RemoveMember)
// after the snapshot was taken but before the meta write tx commits, the committed
// set no longer matches the snapshot. The write must abort with
// errRosterChangedDuringMerge, write NO result, and leave the task Processing (it
// must NOT be wrongly completed with a stale summary that still contains the
// departed member).
func TestSaveLatestResult_RosterShrank_Aborts(t *testing.T) {
	db := newReplaceTestDB(t)
	taskID := seedProcessingTask(t, db)

	idA := seedSubmittedContributor(t, db, taskID, "uA")
	idB := seedSubmittedContributor(t, db, taskID, "uB")
	snapshot := []int64{idA, idB}

	// Simulate RemoveMember: physically delete contributor B's personal_result
	// row AFTER the snapshot was captured.
	if err := db.Delete(&model.PersonalResult{}, idB).Error; err != nil {
		t.Fatalf("delete contributor B: %v", err)
	}

	newRes := &model.SummaryResult{Content: "stale summary mentioning uB", GeneratedAt: time.Now()}
	err := saveLatestResultAndCompleteTask(db, taskID, newRes, false, snapshot)
	if err != errRosterChangedDuringMerge {
		t.Fatalf("err = %v, want errRosterChangedDuringMerge", err)
	}
	// No SummaryResult written (whole tx rolled back).
	if got := countResults(t, db, taskID); got != 0 {
		t.Errorf("stale roster must write no result, got %d", got)
	}
	// Task must remain Processing (NOT completed) so the revive-triggered fresh
	// meta run can recompute from the new roster.
	var task model.SummaryTask
	db.First(&task, taskID)
	if task.Status != model.StatusProcessing {
		t.Errorf("task status = %d, want still Processing (not completed)", task.Status)
	}
}

// TestSaveLatestResult_TaskRowLock_HappyPath asserts that the FOR UPDATE task-row
// lock added at the very top of saveLatestResultAndCompleteTask's tx (seloption三:
// minimal row lock to close the residual roster-vs-merge TOCTOU window) does NOT
// break the normal save+complete flow.
//
// LIMITATION (read carefully): the test DB is sqlite, whose GORM dialector silently
// DROPS clause.Locking{Strength:"UPDATE"} -- the emitted SQL is a plain
// `SELECT id FROM summary_task WHERE id = ? LIMIT 1` with NO `FOR UPDATE` suffix.
// So this test can only prove the added statement is harmless (no error, happy path
// intact) on sqlite. The REAL serialization (save tx vs concurrent Leave/RemoveMember
// strictly serialized on the task row) only exists against MySQL in production and is
// NOT exercised here -- sqlite cannot reproduce MySQL FOR UPDATE row-lock semantics.
func TestSaveLatestResult_TaskRowLock_HappyPath(t *testing.T) {
	db := newReplaceTestDB(t)
	taskID := seedProcessingTask(t, db)

	idA := seedSubmittedContributor(t, db, taskID, "uA")
	idB := seedSubmittedContributor(t, db, taskID, "uB")
	snapshot := []int64{idA, idB}

	newRes := &model.SummaryResult{Content: "team summary", GeneratedAt: time.Now()}
	if err := saveLatestResultAndCompleteTask(db, taskID, newRes, false, snapshot); err != nil {
		t.Fatalf("save with task-row lock should succeed on the happy path: %v", err)
	}
	if got := countResults(t, db, taskID); got != 1 {
		t.Fatalf("expected 1 written result, got %d", got)
	}
	var task model.SummaryTask
	if err := db.First(&task, taskID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if task.Status != model.StatusCompleted {
		t.Errorf("task status = %d, want Completed after save", task.Status)
	}
}

// sameInt64Set is order-independent set equality. It must treat equal sets
// (regardless of order/duplicates) as equal and reject strict subsets/supersets.
func TestSameInt64Set(t *testing.T) {
	cases := []struct {
		name string
		a, b []int64
		want bool
	}{
		{"both empty", nil, []int64{}, true},
		{"equal same order", []int64{1, 2, 3}, []int64{1, 2, 3}, true},
		{"equal diff order", []int64{3, 1, 2}, []int64{1, 2, 3}, true},
		{"equal with dup", []int64{1, 2, 2, 3}, []int64{3, 2, 1}, true},
		{"subset", []int64{1, 2}, []int64{1, 2, 3}, false},
		{"superset", []int64{1, 2, 3}, []int64{1, 2}, false},
		{"disjoint", []int64{1, 2}, []int64{3, 4}, false},
		{"same size diff members", []int64{1, 2, 3}, []int64{1, 2, 4}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameInt64Set(tc.a, tc.b); got != tc.want {
				t.Errorf("sameInt64Set(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
			// Set equality is symmetric.
			if got := sameInt64Set(tc.b, tc.a); got != tc.want {
				t.Errorf("sameInt64Set(%v, %v) [reversed] = %v, want %v", tc.b, tc.a, got, tc.want)
			}
		})
	}
}

func TestScheduledResultAppendsVersionOnSameTask(t *testing.T) {
	db := newReplaceTestDB(t)
	now := time.Now()
	task := model.SummaryTask{TaskNo: "T-SCHED-VERSION", CreatorID: "creator", SummaryMode: model.ModeByPerson, Status: model.StatusProcessing, TriggerType: model.TriggerScheduled, TimeRangeStart: now, TimeRangeEnd: now}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	old := model.SummaryResult{TaskID: task.ID, Content: "old", Version: 1, OperationType: "generate", GeneratedAt: now}
	if err := db.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.SummaryTask{}).Where("id = ?", task.ID).Update("current_result_id", old.ID).Error; err != nil {
		t.Fatal(err)
	}

	result := &model.SummaryResult{Content: "scheduled", OperationNote: "scheduled instruction", GeneratedAt: now.Add(time.Minute)}
	if err := saveLatestResultAndCompleteTask(db, task.ID, result, true, nil); err != nil {
		t.Fatal(err)
	}

	var rows []model.SummaryResult
	if err := db.Where("task_id = ?", task.ID).Order("version ASC").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected old and scheduled versions to be kept, got %d", len(rows))
	}
	if rows[1].Version != 2 || rows[1].OperationType != "scheduled_generate" {
		t.Fatalf("expected scheduled version 2, got version=%d operation=%s", rows[1].Version, rows[1].OperationType)
	}
	var refreshed model.SummaryTask
	if err := db.First(&refreshed, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if refreshed.CurrentResultID == nil || *refreshed.CurrentResultID != rows[1].ID {
		t.Fatalf("current_result_id should point to scheduled version, got %v want %d", refreshed.CurrentResultID, rows[1].ID)
	}
}
