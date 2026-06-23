package worker

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
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

// newOfflineLLM builds an LLMClient with a configured model name but no usable
// endpoint. The single-submission (no-merge) meta path only reads ModelVersion()
// and never calls the network, so this keeps those tests offline/deterministic
// while satisfying the non-nil llm dependency.
func newOfflineLLM() *service.LLMClient {
	return service.NewLLMClient("http://127.0.0.1:0", "", "test-model", 1, 0, false, 1)
}

// seedSubmittedMember creates an Accepted participant + linked, submitted
// personal_result whose Content is set to the userID (so the produced
// single-submission meta result is attributable to a specific member). It
// returns the personal_result id (the contributor's snapshot id) and the
// participant id. Unlike seedMetaParticipant it surfaces the ids the
// roster-race test needs to mutate/inspect the roster.
func seedSubmittedMember(t *testing.T, db *gorm.DB, taskID int64, userID string) (prID, participantID int64) {
	t.Helper()
	now := time.Now().UTC()
	p := model.SummaryParticipant{TaskID: taskID, UserID: userID, Status: model.ParticipantAccepted, ConfirmedAt: &now}
	if err := db.Create(&p).Error; err != nil {
		t.Fatalf("seed participant: %v", err)
	}
	pr := model.PersonalResult{
		TaskID:           taskID,
		ParticipantRefID: p.ID,
		UserID:           userID,
		WorkerStatus:     model.PersonalStatusCompleted,
		Content:          userID, // content tagged with the member so the result is attributable
		SubmittedAt:      &now,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := db.Create(&pr).Error; err != nil {
		t.Fatalf("seed personal_result: %v", err)
	}
	db.Model(&p).Update("personal_result_id", pr.ID)
	return pr.ID, p.ID
}

// P1 LIVENESS (reviewer-found): the roster guard must NOT leave the task stuck in
// Processing. When saveLatestResultAndCompleteTask aborts with
// errRosterChangedDuringMerge, processMetaSummary now `continue`s (re-reading the
// submitted snapshot with the UPDATED roster and re-aggregating in the SAME
// worker) instead of `return`ing. The earlier `return` skipped the loop-bottom
// dirty re-run check while the defer wiped the dirty flag, so a member leaving
// mid-merge could permanently deadlock the task in Processing.
//
// This exercises the full converge-in-one-worker path WITHOUT a live LLM by
// keeping every iteration on the single-submission (no-merge) branch:
//   pass 1: only A submitted -> snapshot {A}; an afterSnapshot hook then
//           simulates RemoveMember(A) landing mid-merge: A's personal_result is
//           deleted (A leaves the committed set) and B is added+submitted, so the
//           committed contributor set ({B}) no longer equals the snapshot ({A}).
//   write : saveLatestResultAndCompleteTask sees the mismatch -> aborts with
//           errRosterChangedDuringMerge -> processMetaSummary `continue`s.
//   pass 2: hook is now disarmed; submitted re-reads to {B} -> single-submission
//           result for the UPDATED roster is written -> task Completed.
// Asserts the task converges (Completed, not stuck Processing), the result
// reflects the new roster (B, not the departed A), and the loop does not spin.
func TestProcessMetaSummary_RosterShrankMidMerge_RecomputesAndCompletes(t *testing.T) {
	db := newReplaceTestDB(t)
	taskID := seedProcessingTask(t, db)

	// Initial roster: only A submitted.
	idA, _ := seedSubmittedMember(t, db, taskID, "uA")

	mp := &MetaProcessor{proc: &Processor{db: db, llm: newOfflineLLM(), cfg: &config.Config{}}, debounceTimers: make(map[int64]*time.Timer)}

	var hookFires int32
	mp.afterSnapshotFn = func(tid int64, snapshot []int64) {
		// Fire exactly once: simulate RemoveMember(A) committing mid-merge.
		if atomic.AddInt32(&hookFires, 1) != 1 {
			return
		}
		// First pass snapshot must be exactly {A}.
		if len(snapshot) != 1 || snapshot[0] != idA {
			t.Errorf("first-pass snapshot = %v, want [%d] (A)", snapshot, idA)
		}
		// A leaves: delete A's submitted personal_result + decline A's participant.
		if err := db.Where("task_id = ? AND user_id = ?", tid, "uA").Delete(&model.PersonalResult{}).Error; err != nil {
			t.Errorf("simulate remove A personal_result: %v", err)
		}
		if err := db.Model(&model.SummaryParticipant{}).
			Where("task_id = ? AND user_id = ?", tid, "uA").
			Update("status", model.ParticipantDeclined).Error; err != nil {
			t.Errorf("simulate decline A participant: %v", err)
		}
		// New roster contributor B submits.
		seedSubmittedMember(t, db, tid, "uB")
	}

	mp.processMetaSummary(context.Background(), taskID)

	// Hook must have fired (the roster-abort path was actually taken).
	if got := atomic.LoadInt32(&hookFires); got < 1 {
		t.Fatalf("afterSnapshot hook never fired; roster-abort path not exercised")
	}

	// LIVENESS: task must NOT be stuck in Processing.
	var task model.SummaryTask
	if err := db.First(&task, taskID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if task.Status == model.StatusProcessing {
		t.Fatalf("DEADLOCK: task left in Processing after a member left mid-merge; roster-abort must recompute, not strand the task")
	}
	if task.Status != model.StatusCompleted {
		t.Fatalf("task should converge to Completed with the updated roster, got status=%d", task.Status)
	}

	// Exactly one meta result, reflecting the UPDATED roster (B), never the
	// departed A.
	if n := countResults(t, db, taskID); n != 1 {
		t.Fatalf("want exactly 1 meta result after convergence, got %d", n)
	}
	var res model.SummaryResult
	if err := db.Where("task_id = ?", taskID).Order("version DESC").First(&res).Error; err != nil {
		t.Fatalf("load result: %v", err)
	}
	if res.Content != "uB" {
		t.Fatalf("result content = %q, want %q (updated roster, not departed A)", res.Content, "uB")
	}
	for _, c := range res.GetTeamCitations() {
		if c.UserID == "uA" {
			t.Fatalf("result still cites departed member uA: %+v", res.GetTeamCitations())
		}
	}
}

// Bounded-retry guard: if the roster keeps shrinking on EVERY pass (a pathological
// livelock), processMetaSummary must give up after maxRosterRetries rather than
// spin forever. Here the afterSnapshot hook re-arms every pass — it always makes
// the committed set differ from the snapshot — so every write aborts with
// errRosterChangedDuringMerge and the loop must terminate via the retry bound.
func TestProcessMetaSummary_RosterChurnHitsRetryBound(t *testing.T) {
	db := newReplaceTestDB(t)
	taskID := seedProcessingTask(t, db)

	// One initial submitted member so pass 1 has a non-empty snapshot (single
	// submission -> no LLM). The churn hook then keeps exactly one submitted member
	// per pass while swapping its identity, so the committed set differs from the
	// snapshot every time and every write aborts with errRosterChangedDuringMerge.
	seedSubmittedMember(t, db, taskID, "seed")

	mp := &MetaProcessor{proc: &Processor{db: db, llm: newOfflineLLM(), cfg: &config.Config{}}, debounceTimers: make(map[int64]*time.Timer)}

	var fires int32
	mp.afterSnapshotFn = func(tid int64, snapshot []int64) {
		// On every pass, churn the roster so committed != snapshot: remove whatever
		// was snapshotted (delete its personal_result AND decline its participant so
		// the round stays ready/terminal) and add a fresh submitted contributor. This
		// guarantees an errRosterChangedDuringMerge on every write -> exercises the
		// retry bound. Exactly one member stays submitted per pass (single-submission,
		// no LLM).
		n := atomic.AddInt32(&fires, 1)
		if len(snapshot) > 0 {
			var refIDs []int64
			db.Model(&model.PersonalResult{}).Where("id IN ?", snapshot).Pluck("participant_ref_id", &refIDs)
			if err := db.Where("id IN ?", snapshot).Delete(&model.PersonalResult{}).Error; err != nil {
				t.Errorf("churn delete snapshot: %v", err)
			}
			if len(refIDs) > 0 {
				if err := db.Model(&model.SummaryParticipant{}).Where("id IN ?", refIDs).
					Update("status", model.ParticipantDeclined).Error; err != nil {
					t.Errorf("churn decline participant: %v", err)
				}
			}
		}
		seedSubmittedMember(t, db, tid, "churn-"+strconv.Itoa(int(n)))
	}

	done := make(chan struct{})
	go func() {
		mp.processMetaSummary(context.Background(), taskID)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("processMetaSummary did not terminate under continuous roster churn (livelock); retry bound not enforced")
	}

	// Must have stopped at the bound, not spun unboundedly. maxRosterRetries=5 ->
	// the snapshot/abort cycle runs a small bounded number of times.
	if got := atomic.LoadInt32(&fires); got == 0 {
		t.Fatalf("hook never fired; churn path not exercised")
	}
	if got := atomic.LoadInt32(&fires); got > 12 {
		t.Fatalf("roster-abort retried %d times; expected bounded (~maxRosterRetries), loop is not bounded", got)
	}

	// On abort-via-bound the task is left Processing (a fresh trigger will retry
	// later); the point of THIS test is termination, not completion.
	var task model.SummaryTask
	if err := db.First(&task, taskID).Error; err != nil {
		t.Fatalf("reload task: %v", err)
	}
}
