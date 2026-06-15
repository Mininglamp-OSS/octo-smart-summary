package worker

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/gorm"
)

func TestMetaProcessorGetMetaLock(t *testing.T) {
	mp := &MetaProcessor{
		debounceTimers: make(map[int64]*time.Timer),
	}

	// Same taskID should return the same mutex
	mu1 := mp.getMetaLock(1)
	mu2 := mp.getMetaLock(1)
	if mu1 != mu2 {
		t.Error("expected same mutex for same task ID")
	}

	// Different taskID should return different mutex
	mu3 := mp.getMetaLock(2)
	if mu1 == mu3 {
		t.Error("expected different mutex for different task ID")
	}
}

func TestMetaProcessorTryLockMutualExclusion(t *testing.T) {
	mp := &MetaProcessor{
		debounceTimers: make(map[int64]*time.Timer),
	}

	mu := mp.getMetaLock(1)
	mu.Lock()

	// TryLock should fail since mutex is held
	if mu.TryLock() {
		t.Error("expected TryLock to fail when mutex is held")
		mu.Unlock()
	}

	mu.Unlock()

	// Now TryLock should succeed
	if !mu.TryLock() {
		t.Error("expected TryLock to succeed when mutex is free")
	}
	mu.Unlock()
}

func TestMetaProcessorDebounce(t *testing.T) {
	mp := &MetaProcessor{
		debounceTimers: make(map[int64]*time.Timer),
	}

	var callCount int32

	// Override: directly test debounce mechanics using timers
	mp.debounceMu.Lock()
	for i := 0; i < 5; i++ {
		if timer, exists := mp.debounceTimers[1]; exists {
			timer.Stop()
		}
		mp.debounceTimers[1] = time.AfterFunc(50*time.Millisecond, func() {
			atomic.AddInt32(&callCount, 1)
		})
	}
	mp.debounceMu.Unlock()

	// Wait for debounce to fire
	time.Sleep(200 * time.Millisecond)

	count := atomic.LoadInt32(&callCount)
	if count != 1 {
		t.Errorf("expected debounce to fire exactly once, got %d", count)
	}
}

func TestMetaProcessorConcurrentTryLock(t *testing.T) {
	mp := &MetaProcessor{
		debounceTimers: make(map[int64]*time.Timer),
	}

	mu := mp.getMetaLock(42)
	var locked int32
	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if mu.TryLock() {
				atomic.AddInt32(&locked, 1)
				time.Sleep(10 * time.Millisecond)
				mu.Unlock()
			}
		}()
	}

	wg.Wait()

	// At least 1 should have succeeded, but not all simultaneously
	if atomic.LoadInt32(&locked) == 0 {
		t.Error("expected at least one goroutine to acquire lock")
	}
}

// seedMetaParticipant creates an Accepted participant + a linked personal_result
// in the given worker status / submitted state, returning the personal_result id.
func seedMetaParticipant(t *testing.T, db *gorm.DB, taskID int64, userID string, workerStatus int, submitted bool) {
	t.Helper()
	now := time.Now().UTC()
	p := model.SummaryParticipant{TaskID: taskID, UserID: userID, Status: model.ParticipantAccepted, ConfirmedAt: &now}
	if err := db.Create(&p).Error; err != nil {
		t.Fatalf("seed participant: %v", err)
	}
	pr := model.PersonalResult{TaskID: taskID, ParticipantRefID: p.ID, UserID: userID, WorkerStatus: workerStatus, Content: "x", CreatedAt: now, UpdatedAt: now}
	if submitted {
		pr.SubmittedAt = &now
	}
	if err := db.Create(&pr).Error; err != nil {
		t.Fatalf("seed personal_result: %v", err)
	}
	db.Model(&p).Update("personal_result_id", pr.ID)
}

// V5 §4.4: meta completion is judged by TERMINAL state, not a pure submitted>=accepted
// count. A member whose personal_result is Failed must NOT block aggregation forever.
//
// fail-before (old pure-count gate): u2 submitted, u3 Failed (never submits) ->
// submitted(1) < accepted(2) -> meta would dead-wait. V5: u3 is terminal (Failed),
// so the round is ready with u2's single submission.
func TestMetaCompletionReady_FailedDoesNotDeadWait(t *testing.T) {
	db := newReplaceTestDB(t)
	taskID := seedProcessingTask(t, db)
	seedMetaParticipant(t, db, taskID, "u2", model.PersonalStatusCompleted, true)  // submitted
	seedMetaParticipant(t, db, taskID, "u3", model.PersonalStatusFailed, false)     // failed, never submits

	submitted, ready := metaCompletionReady(db, taskID)
	if len(submitted) != 1 {
		t.Fatalf("want 1 submitted result (u2), got %d", len(submitted))
	}
	if !ready {
		t.Fatalf("V5: a Failed member must not block aggregation; expected ready=true")
	}
}

// A still-running (non-terminal) member keeps the round NOT ready: u2 submitted,
// u3 still Pending (not submitted, not failed) -> meta must wait.
func TestMetaCompletionReady_NonTerminalStillWaits(t *testing.T) {
	db := newReplaceTestDB(t)
	taskID := seedProcessingTask(t, db)
	seedMetaParticipant(t, db, taskID, "u2", model.PersonalStatusCompleted, true) // submitted
	seedMetaParticipant(t, db, taskID, "u3", model.PersonalStatusPending, false)  // still running

	_, ready := metaCompletionReady(db, taskID)
	if ready {
		t.Fatalf("a non-terminal (Pending) accepted member must keep the round waiting; got ready=true")
	}
}

// All accepted members submitted -> ready.
func TestMetaCompletionReady_AllSubmittedReady(t *testing.T) {
	db := newReplaceTestDB(t)
	taskID := seedProcessingTask(t, db)
	seedMetaParticipant(t, db, taskID, "u1", model.PersonalStatusCompleted, true)
	seedMetaParticipant(t, db, taskID, "u2", model.PersonalStatusCompleted, true)

	submitted, ready := metaCompletionReady(db, taskID)
	if len(submitted) != 2 || !ready {
		t.Fatalf("all submitted should be ready, got submitted=%d ready=%v", len(submitted), ready)
	}
}

// seedDeclinedFailedMember mimics personal_processor.markPersonalFailed's
// multi-person branch: the permanently-failed member is Declined (so it leaves
// the accepted set) but its personal_result row remains with worker_status=Failed
// and is never submitted.
func seedDeclinedFailedMember(t *testing.T, db *gorm.DB, taskID int64, userID string) {
	t.Helper()
	now := time.Now().UTC()
	p := model.SummaryParticipant{TaskID: taskID, UserID: userID, Status: model.ParticipantDeclined, ConfirmedAt: &now}
	if err := db.Create(&p).Error; err != nil {
		t.Fatalf("seed declined participant: %v", err)
	}
	pr := model.PersonalResult{TaskID: taskID, ParticipantRefID: p.ID, UserID: userID, WorkerStatus: model.PersonalStatusFailed, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&pr).Error; err != nil {
		t.Fatalf("seed failed personal_result: %v", err)
	}
}

// V5 deadlock guard (finding 1): when EVERY confirmed member permanently failed,
// markPersonalFailed Declined them all -> the accepted set is empty -> ready=true
// while submitted is empty. metaCompletionReady must report ready=true with no
// submissions (an empty accepted roster is trivially all-terminal).
func TestMetaCompletionReady_AllFailedReadyButNoSubmitted(t *testing.T) {
	db := newReplaceTestDB(t)
	taskID := seedProcessingTask(t, db)
	seedDeclinedFailedMember(t, db, taskID, "u1")
	seedDeclinedFailedMember(t, db, taskID, "u2")

	submitted, ready := metaCompletionReady(db, taskID)
	if len(submitted) != 0 {
		t.Fatalf("want 0 submitted (all failed), got %d", len(submitted))
	}
	if !ready {
		t.Fatalf("all confirmed members failed/declined -> accepted set empty -> expected ready=true")
	}
}

// V5 deadlock guard (finding 1), end-to-end: processMetaSummary must converge a
// round where ready==true && submitted==0 to a TERMINAL state (Cancelled),
// never leaving it stuck in Processing (the old `if len(submitted)==0 { return }`
// dead-wait that the overlap guard would keep blocking forever).
func TestProcessMetaSummary_AllFailedConvergesToTerminal(t *testing.T) {
	db := newReplaceTestDB(t)
	taskID := seedProcessingTask(t, db)
	seedDeclinedFailedMember(t, db, taskID, "u1")
	seedDeclinedFailedMember(t, db, taskID, "u2")

	mp := &MetaProcessor{proc: &Processor{db: db}, debounceTimers: make(map[int64]*time.Timer)}
	mp.processMetaSummary(context.Background(), taskID)

	var task model.SummaryTask
	if err := db.First(&task, taskID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if task.Status == model.StatusProcessing {
		t.Fatalf("task must NOT stay Processing when all members failed (deadlock); status=%d", task.Status)
	}
	if task.Status != model.StatusCancelled {
		t.Fatalf("all-failed round should converge to StatusCancelled, got status=%d", task.Status)
	}
	// And no meta result should have been written.
	if n := countResults(t, db, taskID); n != 0 {
		t.Fatalf("all-failed round must not produce a meta result, got %d", n)
	}
}

// Counterpart safety: ready==false && submitted==0 (a member still Pending) must
// keep the task in Processing (return and wait), NOT converge to a terminal
// state. Only ready==true && submitted==0 triggers the cancel.
func TestProcessMetaSummary_NotReadyNoSubmittedStaysProcessing(t *testing.T) {
	db := newReplaceTestDB(t)
	taskID := seedProcessingTask(t, db)
	// One accepted member still running (Pending, not submitted, not failed) ->
	// not terminal -> ready=false, and nobody submitted yet.
	seedMetaParticipant(t, db, taskID, "u1", model.PersonalStatusPending, false)

	mp := &MetaProcessor{proc: &Processor{db: db}, debounceTimers: make(map[int64]*time.Timer)}
	mp.processMetaSummary(context.Background(), taskID)

	var task model.SummaryTask
	if err := db.First(&task, taskID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if task.Status != model.StatusProcessing {
		t.Fatalf("a not-yet-ready round must stay Processing and wait, got status=%d", task.Status)
	}
}
