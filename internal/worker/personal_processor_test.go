package worker

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestDecidePersonalMessages_NoTarget_AllMessages(t *testing.T) {
	all := []pipeline.Message{
		{SenderUID: "alice", Content: "hello"},
		{SenderUID: "bob", Content: "hi"},
	}
	msgs, early := decidePersonalMessages(nil, "creator", all, nil)
	if early != "" {
		t.Fatalf("expected no early message, got %q", early)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected all 2 messages, got %d", len(msgs))
	}
}

func TestDecidePersonalMessages_FilteredNonEmpty_UsesFiltered(t *testing.T) {
	all := []pipeline.Message{
		{SenderUID: "alice", Content: "hello"},
		{SenderUID: "bob", Content: "hi"},
	}
	filtered := []pipeline.Message{
		{SenderUID: "bob", Content: "hi", IsTargetUser: true},
	}
	msgs, early := decidePersonalMessages([]string{"bob"}, "creator", all, filtered)
	if early != "" {
		t.Fatalf("expected no early message, got %q", early)
	}
	if len(msgs) != 1 || msgs[0].SenderUID != "bob" {
		t.Fatalf("expected filtered [bob], got %v", msgs)
	}
}

func TestDecidePersonalMessages_FilteredEmpty_SelfOnly_EarlyReturn(t *testing.T) {
	all := []pipeline.Message{
		{SenderUID: "alice", Content: "hello"},
		{SenderUID: "bob", Content: "hi"},
	}
	// True first-person query: target is exactly the creator, who never spoke.
	msgs, early := decidePersonalMessages([]string{"creator"}, "creator", all, nil)
	if early != noSelfMessagesMessage {
		t.Fatalf("expected noSelfMessagesMessage, got %q", early)
	}
	if msgs != nil {
		t.Fatalf("expected nil messages on self-empty early return, got %v", msgs)
	}
}

func TestDecidePersonalMessages_FilteredEmpty_NamedOther_FallsBackToAll(t *testing.T) {
	all := []pipeline.Message{
		{SenderUID: "alice", Content: "hello"},
		{SenderUID: "bob", Content: "hi"},
	}
	// Named someone (wang) who didn't speak in this source → fall back to all.
	msgs, early := decidePersonalMessages([]string{"wang"}, "creator", all, nil)
	if early != "" {
		t.Fatalf("expected no early message, got %q", early)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected fallback to all 2 messages, got %d", len(msgs))
	}
}

func TestDecidePersonalMessages_FilteredEmpty_SelfPlusOther_FallsBackToAll(t *testing.T) {
	all := []pipeline.Message{
		{SenderUID: "alice", Content: "hello"},
	}
	// "我和老王" → targets [wang, creator]; not a pure self-reference, so fall back.
	msgs, early := decidePersonalMessages([]string{"wang", "creator"}, "creator", all, nil)
	if early != "" {
		t.Fatalf("expected no early message, got %q", early)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected fallback to all 1 message, got %d", len(msgs))
	}
}

// normalizeTargetMsgCount mirrors the F1 fix in executePersonalPipeline: on
// untrimmed / fallback paths no message has IsTargetUser set, so the accumulated
// count is 0 and must be normalized to the full message count.
func normalizeTargetMsgCount(msgs []pipeline.Message) int {
	count := 0
	for _, m := range msgs {
		if m.IsTargetUser {
			count++
		}
	}
	if count == 0 {
		count = len(msgs)
	}
	return count
}

func TestTargetMsgCount_Normalization_Untrimmed(t *testing.T) {
	// No IsTargetUser anywhere (fallback / no-target path) → count = len.
	msgs := []pipeline.Message{
		{SenderUID: "alice"},
		{SenderUID: "bob"},
		{SenderUID: "carol"},
	}
	if got := normalizeTargetMsgCount(msgs); got != 3 {
		t.Fatalf("expected normalized count 3, got %d", got)
	}
}

func TestTargetMsgCount_Normalization_TrueNarrowUnaffected(t *testing.T) {
	// True narrow path has ≥1 IsTargetUser → count reflects targets only.
	msgs := []pipeline.Message{
		{SenderUID: "alice"},
		{SenderUID: "bob", IsTargetUser: true},
		{SenderUID: "carol"},
	}
	if got := normalizeTargetMsgCount(msgs); got != 1 {
		t.Fatalf("expected count 1 (true narrow unaffected), got %d", got)
	}
}

func TestShouldSkipScheduledPlaceholder_BothPlaceholdersSkipped(t *testing.T) {
	// Both empty-placeholder messages must be skipped for scheduled tasks so a
	// transient empty window never overwrites a previous valid result.
	cases := []struct {
		name        string
		triggerType int
		content     string
		want        bool
	}{
		{"scheduled + no-relevant", model.TriggerScheduled, noRelevantContentMessage, true},
		{"scheduled + no-self-messages", model.TriggerScheduled, noSelfMessagesMessage, true},
		{"scheduled + real content", model.TriggerScheduled, "正常的总结内容", false},
		{"manual + no-self-messages", model.TriggerManual, noSelfMessagesMessage, false},
	}
	for _, c := range cases {
		if got := shouldSkipScheduledPlaceholderResult(c.triggerType, c.content); got != c.want {
			t.Fatalf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// §4.4-2 system back-fill of submitted_at (merged from team_schedule_p0_test.go).
// ---------------------------------------------------------------------------

func TestBackfillSubmittedAt_SchedAccepted_BackfillsSystemSource(t *testing.T) {
	db := newSchedulerTestDB(t)
	now := time.Now().UTC()

	task := model.SummaryTask{
		TaskNo: "T1", SpaceID: "sp", CreatorID: "u1", SummaryMode: model.ModeByPerson,
		Status: model.StatusProcessing, TriggerType: model.TriggerScheduled,
		TimeRangeStart: now, TimeRangeEnd: now,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	part := model.SummaryParticipant{TaskID: task.ID, UserID: "u2", Status: model.ParticipantCompleted}
	if err := db.Create(&part).Error; err != nil {
		t.Fatalf("create participant: %v", err)
	}
	pr := model.PersonalResult{
		TaskID: task.ID, ParticipantRefID: part.ID, UserID: "u2",
		WorkerStatus: model.PersonalStatusCompleted,
	}
	if err := db.Create(&pr).Error; err != nil {
		t.Fatalf("create personal_result: %v", err)
	}

	p := &Processor{db: db}
	p.backfillScheduledSubmittedAt(task.ID, &pr)

	var got model.PersonalResult
	if err := db.First(&got, pr.ID).Error; err != nil {
		t.Fatalf("reload pr: %v", err)
	}
	if got.SubmittedAt == nil {
		t.Fatalf("submitted_at should be back-filled, got nil")
	}
	if got.SubmitSource != model.SubmitSourceSystem {
		t.Errorf("submit_source=%d, want SubmitSourceSystem(2)", got.SubmitSource)
	}
}

func TestBackfillSubmittedAt_DeclinedParticipant_NotBackfilled(t *testing.T) {
	db := newSchedulerTestDB(t)
	now := time.Now().UTC()

	task := model.SummaryTask{
		TaskNo: "T2", SpaceID: "sp", CreatorID: "u1", SummaryMode: model.ModeByPerson,
		Status: model.StatusProcessing, TriggerType: model.TriggerScheduled,
		TimeRangeStart: now, TimeRangeEnd: now,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	part := model.SummaryParticipant{TaskID: task.ID, UserID: "u2", Status: model.ParticipantDeclined}
	if err := db.Create(&part).Error; err != nil {
		t.Fatalf("create participant: %v", err)
	}
	pr := model.PersonalResult{
		TaskID: task.ID, ParticipantRefID: part.ID, UserID: "u2",
		WorkerStatus: model.PersonalStatusCompleted,
	}
	if err := db.Create(&pr).Error; err != nil {
		t.Fatalf("create personal_result: %v", err)
	}

	p := &Processor{db: db}
	p.backfillScheduledSubmittedAt(task.ID, &pr)

	var got model.PersonalResult
	if err := db.First(&got, pr.ID).Error; err != nil {
		t.Fatalf("reload pr: %v", err)
	}
	if got.SubmittedAt != nil {
		t.Errorf("declined participant must NOT be back-filled, got submitted_at=%v", got.SubmittedAt)
	}
	if got.SubmitSource != model.SubmitSourceNone {
		t.Errorf("declined participant submit_source=%d, want 0", got.SubmitSource)
	}
}

func TestBackfillSubmittedAt_AlreadySubmitted_Idempotent(t *testing.T) {
	db := newSchedulerTestDB(t)
	now := time.Now().UTC()
	manualTime := now.Add(-time.Hour)

	task := model.SummaryTask{
		TaskNo: "T3", SpaceID: "sp", CreatorID: "u1", SummaryMode: model.ModeByPerson,
		Status: model.StatusProcessing, TriggerType: model.TriggerScheduled,
		TimeRangeStart: now, TimeRangeEnd: now,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	part := model.SummaryParticipant{TaskID: task.ID, UserID: "u2", Status: model.ParticipantSubmitted}
	if err := db.Create(&part).Error; err != nil {
		t.Fatalf("create participant: %v", err)
	}
	// Already submitted manually (submit_source=1, submitted_at set).
	pr := model.PersonalResult{
		TaskID: task.ID, ParticipantRefID: part.ID, UserID: "u2",
		WorkerStatus: model.PersonalStatusCompleted,
		SubmittedAt:  &manualTime, SubmitSource: model.SubmitSourceManual,
	}
	if err := db.Create(&pr).Error; err != nil {
		t.Fatalf("create personal_result: %v", err)
	}

	p := &Processor{db: db}
	p.backfillScheduledSubmittedAt(task.ID, &pr)

	var got model.PersonalResult
	if err := db.First(&got, pr.ID).Error; err != nil {
		t.Fatalf("reload pr: %v", err)
	}
	// Manual submit must NOT be overwritten by the system back-fill.
	if got.SubmitSource != model.SubmitSourceManual {
		t.Errorf("manual submit overwritten: submit_source=%d, want SubmitSourceManual(1)", got.SubmitSource)
	}
	if got.SubmittedAt == nil || !got.SubmittedAt.Equal(manualTime) {
		t.Errorf("manual submitted_at overwritten: got %v want %v", got.SubmittedAt, manualTime)
	}
}

// ---------------------------------------------------------------------------
// Blocker-3: personal failure self-heal vs terminal failure.
// ---------------------------------------------------------------------------

// seedFailFixture builds a Processing task with one participant + a Processing
// personal_result at the given retry_count, and a configured maxRetry.
func seedFailFixture(t *testing.T, db *gorm.DB, taskNo string, participantCount, retryCount int) (*Processor, *model.PersonalResult, *model.SummaryParticipant) {
	t.Helper()
	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo: taskNo, SpaceID: "sp", CreatorID: "u1", SummaryMode: model.ModeByPerson,
		Status: model.StatusProcessing, TriggerType: model.TriggerScheduled,
		TimeRangeStart: now, TimeRangeEnd: now,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	var firstPart model.SummaryParticipant
	var firstPR model.PersonalResult
	for i := 0; i < participantCount; i++ {
		part := model.SummaryParticipant{
			TaskID: task.ID, UserID: userIDForIdx(i), Status: model.ParticipantProcessing,
			WorkerStartedAt: &now,
		}
		if err := db.Create(&part).Error; err != nil {
			t.Fatalf("create participant: %v", err)
		}
		pr := model.PersonalResult{
			TaskID: task.ID, ParticipantRefID: part.ID, UserID: userIDForIdx(i),
			WorkerStatus: model.PersonalStatusProcessing, RetryCount: retryCount,
		}
		if err := db.Create(&pr).Error; err != nil {
			t.Fatalf("create personal_result: %v", err)
		}
		if i == 0 {
			firstPart, firstPR = part, pr
		}
	}
	// pool=nil so the immediate re-trigger path is a no-op in tests (we assert state,
	// not the re-run); meta=nil so the declined-multi path does not require LLM.
	p := &Processor{db: db, cfg: &config.Config{WorkerMaxRetry: 3}}
	return p, &firstPR, &firstPart
}

// Under max retry, a transient personal failure resets worker_status to Pending
// and increments retry_count so the stuck scanner re-runs it (self-heal).
func TestMarkPersonalFailed_UnderMaxRetry_ResetsToPending(t *testing.T) {
	db := newSchedulerTestDB(t)
	p, pr, part := seedFailFixture(t, db, "T-FAIL-1", 2, 0)

	p.markPersonalFailed(pr, part, "LLM API error: boom")

	var gotPR model.PersonalResult
	db.First(&gotPR, pr.ID)
	if gotPR.WorkerStatus != model.PersonalStatusPending {
		t.Errorf("under max retry: worker_status=%d, want Pending(0) for self-heal", gotPR.WorkerStatus)
	}
	if gotPR.RetryCount != 1 {
		t.Errorf("retry_count=%d, want 1", gotPR.RetryCount)
	}
	var gotPart model.SummaryParticipant
	db.First(&gotPart, part.ID)
	if gotPart.Status != model.ParticipantAccepted {
		t.Errorf("participant status=%d, want Accepted(1) so stuck-scan re-runs it", gotPart.Status)
	}
	if gotPart.WorkerStartedAt != nil {
		t.Errorf("worker_started_at should be cleared on retry reset, got %v", gotPart.WorkerStartedAt)
	}
}

// At max retry in multi-person mode, the participant is Declined (terminal) so
// meta's totalAccepted excludes it and the task does not dead-wait.
func TestMarkPersonalFailed_MaxRetry_MultiPerson_DeclinesParticipant(t *testing.T) {
	db := newSchedulerTestDB(t)
	// retryCount=2, maxRetry=3 -> newRetry=3 >= 3 -> terminal.
	p, pr, part := seedFailFixture(t, db, "T-FAIL-2", 2, 2)

	p.markPersonalFailed(pr, part, "LLM API error: boom")

	var gotPR model.PersonalResult
	db.First(&gotPR, pr.ID)
	if gotPR.WorkerStatus != model.PersonalStatusFailed {
		t.Errorf("at max retry: worker_status=%d, want Failed(3)", gotPR.WorkerStatus)
	}
	if gotPR.RetryCount != 3 {
		t.Errorf("retry_count=%d, want 3", gotPR.RetryCount)
	}
	var gotPart model.SummaryParticipant
	db.First(&gotPart, part.ID)
	if gotPart.Status != model.ParticipantDeclined {
		t.Errorf("multi-person permanent fail: participant status=%d, want Declined(2) so meta excludes it", gotPart.Status)
	}
	// meta gate: totalAccepted (status NOT IN Pending,Declined) must now exclude the
	// failed member so the remaining member can complete the task.
	var totalAccepted int64
	db.Model(&model.SummaryParticipant{}).
		Where("task_id = ? AND status NOT IN (?, ?)", pr.TaskID, model.ParticipantPending, model.ParticipantDeclined).
		Count(&totalAccepted)
	if totalAccepted != 1 {
		t.Errorf("totalAccepted after permanent fail of 1/2 members = %d, want 1 (no dead-wait)", totalAccepted)
	}
}

// At max retry in single-person mode, the task itself is failed (unchanged
// behavior, no regression).
func TestMarkPersonalFailed_MaxRetry_SinglePerson_FailsTask(t *testing.T) {
	db := newSchedulerTestDB(t)
	p, pr, part := seedFailFixture(t, db, "T-FAIL-3", 1, 2)

	p.markPersonalFailed(pr, part, "LLM API error: boom")

	var gotPR model.PersonalResult
	db.First(&gotPR, pr.ID)
	if gotPR.WorkerStatus != model.PersonalStatusFailed {
		t.Errorf("single-person max retry: worker_status=%d, want Failed(3)", gotPR.WorkerStatus)
	}
	var gotTask model.SummaryTask
	db.First(&gotTask, pr.TaskID)
	if gotTask.Status != model.StatusFailed {
		t.Errorf("single-person permanent fail must fail the task, got status=%d", gotTask.Status)
	}
}

// ---------------------------------------------------------------------------
// 🟠 Closeout: retry_count atomic increment (no lost updates under concurrency).
// ---------------------------------------------------------------------------

// newFileBackedTestDB builds a file-backed sqlite (WAL + busy_timeout) so multiple
// goroutines can run concurrent write transactions for real -- ":memory:" sqlite
// shares a single in-process db that defeats true write concurrency. Mirrors
// newSchedulerTestDB's schema.
func newFileBackedTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "retry.db") + "?_busy_timeout=10000&_journal_mode=WAL"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(
		&model.SummarySchedule{},
		&model.SummaryTask{},
		&model.SummaryParticipant{},
		&model.PersonalResult{},
		&model.SummarySource{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// Concurrent failure workers must NOT lose retry_count increments. With the old
// "Select retry_count -> +1 -> write back" (no FOR UPDATE / no CAS), two workers
// reading the same value would both write the same newRetry, swallowing a failure.
// The atomic UPDATE retry_count = retry_count + 1 guarantees strict accumulation:
// N concurrent permanent-failures -> retry_count advances by exactly N.
func TestMarkPersonalFailed_ConcurrentRetry_NoLostIncrement(t *testing.T) {
	db := newFileBackedTestDB(t)
	// High maxRetry so every concurrent call takes the "willRetry" increment branch
	// (resets to Pending), keeping all N workers contending on retry_count.
	const workers = 8
	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo: "T-FAIL-CC", SpaceID: "sp", CreatorID: "u1", SummaryMode: model.ModeByPerson,
		Status: model.StatusProcessing, TriggerType: model.TriggerScheduled,
		TimeRangeStart: now, TimeRangeEnd: now,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	// 2 participants so the multi-person path is taken even if a call reaches terminal
	// (avoids touching the single-person task-fail branch); but maxRetry is large so
	// they stay in the increment branch.
	part := model.SummaryParticipant{
		TaskID: task.ID, UserID: "u1", Status: model.ParticipantProcessing, WorkerStartedAt: &now,
	}
	if err := db.Create(&part).Error; err != nil {
		t.Fatalf("create participant: %v", err)
	}
	part2 := model.SummaryParticipant{
		TaskID: task.ID, UserID: "u2", Status: model.ParticipantAccepted,
	}
	if err := db.Create(&part2).Error; err != nil {
		t.Fatalf("create participant2: %v", err)
	}
	pr := model.PersonalResult{
		TaskID: task.ID, ParticipantRefID: part.ID, UserID: "u1",
		WorkerStatus: model.PersonalStatusProcessing, RetryCount: 0,
	}
	if err := db.Create(&pr).Error; err != nil {
		t.Fatalf("create personal_result: %v", err)
	}

	// pool=nil/meta=nil so the re-trigger/meta-kick side effects are no-ops in the test.
	p := &Processor{db: db, cfg: &config.Config{WorkerMaxRetry: 1000}}

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			prCopy := pr           // each worker gets its own struct (mirrors real workers)
			partCopy := part
			p.markPersonalFailed(&prCopy, &partCopy, "LLM API error: boom")
		}()
	}
	wg.Wait()

	var got model.PersonalResult
	if err := db.First(&got, pr.ID).Error; err != nil {
		t.Fatalf("reload pr: %v", err)
	}
	if got.RetryCount != workers {
		t.Fatalf("concurrent retry lost an increment: retry_count=%d, want exactly %d (atomic accumulation)",
			got.RetryCount, workers)
	}
}

// Strict accumulation + terminal guarantee: repeated failures must increment by
// exactly 1 each time, and once retry_count reaches WorkerMaxRetry the row must be
// terminal Failed (never re-run forever). This pins the DB-derived newRetry branch.
func TestMarkPersonalFailed_SequentialRetry_AccumulatesThenTerminal(t *testing.T) {
	db := newSchedulerTestDB(t)
	p, pr, part := seedFailFixture(t, db, "T-FAIL-SEQ", 2, 0) // maxRetry=3, multi-person

	// Fail #1: 0 -> 1, willRetry (1 < 3), back to Pending.
	p.markPersonalFailed(pr, part, "LLM API error: boom")
	var g1 model.PersonalResult
	db.First(&g1, pr.ID)
	if g1.RetryCount != 1 || g1.WorkerStatus != model.PersonalStatusPending {
		t.Fatalf("after fail #1: retry_count=%d status=%d, want 1/Pending", g1.RetryCount, g1.WorkerStatus)
	}

	// Fail #2: 1 -> 2, willRetry (2 < 3), back to Pending.
	p.markPersonalFailed(pr, part, "LLM API error: boom")
	var g2 model.PersonalResult
	db.First(&g2, pr.ID)
	if g2.RetryCount != 2 || g2.WorkerStatus != model.PersonalStatusPending {
		t.Fatalf("after fail #2: retry_count=%d status=%d, want 2/Pending", g2.RetryCount, g2.WorkerStatus)
	}

	// Fail #3: 2 -> 3, 3 >= maxRetry(3) -> terminal Failed (must NOT loop forever).
	p.markPersonalFailed(pr, part, "LLM API error: boom")
	var g3 model.PersonalResult
	db.First(&g3, pr.ID)
	if g3.RetryCount != 3 {
		t.Fatalf("after fail #3: retry_count=%d, want 3 (strict +1 accumulation)", g3.RetryCount)
	}
	if g3.WorkerStatus != model.PersonalStatusFailed {
		t.Fatalf("after reaching maxRetry: worker_status=%d, want Failed(terminal) -- no infinite re-run", g3.WorkerStatus)
	}
}
