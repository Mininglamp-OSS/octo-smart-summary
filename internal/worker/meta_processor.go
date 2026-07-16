package worker

import (
	"context"
	"errors"
	"log"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/streaming"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timing"
	"gorm.io/gorm"
)

// MetaProcessor handles meta-summary generation with debounce and mutex.
type MetaProcessor struct {
	proc *Processor

	// Per-task mutex to ensure only one meta worker runs per task
	metaLocks sync.Map // key=taskID, value=*sync.Mutex

	// Per-task dirty flag: set when a new submission arrives while processing
	dirtyFlags sync.Map // key=taskID, value=bool

	// Per-task debounce timer
	debounceMu     sync.Mutex
	debounceTimers map[int64]*time.Timer

	// afterSnapshotFn, when non-nil, is invoked inside processMetaSummary right after
	// the per-iteration contributor snapshot is captured but before the result write.
	// Test-only seam (mirrors Processor.createPRFn / executePipelineFn) used to
	// deterministically simulate a Leave/RemoveMember landing mid-merge — i.e. the
	// roster shrinking after the snapshot — so the errRosterChangedDuringMerge ->
	// recompute (continue) liveness path can be exercised without a live LLM. It
	// receives the captured snapshot contributor IDs. Production leaves it nil.
	afterSnapshotFn func(taskID int64, snapshotContributorIDs []int64)
}

// NewMetaProcessor creates a new MetaProcessor.
func NewMetaProcessor(proc *Processor) *MetaProcessor {
	return &MetaProcessor{
		proc:           proc,
		debounceTimers: make(map[int64]*time.Timer),
	}
}

func (m *MetaProcessor) getMetaLock(taskID int64) *sync.Mutex {
	v, _ := m.metaLocks.LoadOrStore(taskID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func (m *MetaProcessor) markDirty(taskID int64)  { m.dirtyFlags.Store(taskID, true) }
func (m *MetaProcessor) clearDirty(taskID int64) { m.dirtyFlags.Store(taskID, false) }
func (m *MetaProcessor) isDirty(taskID int64) bool {
	v, ok := m.dirtyFlags.Load(taskID)
	return ok && v.(bool)
}

// TriggerMetaSummary schedules a meta-summary with 100ms debounce.
func (m *MetaProcessor) TriggerMetaSummary(taskID int64) {
	m.debounceMu.Lock()
	defer m.debounceMu.Unlock()

	// Cancel existing timer for this task
	if timer, exists := m.debounceTimers[taskID]; exists {
		timer.Stop()
	}

	m.debounceTimers[taskID] = time.AfterFunc(100*time.Millisecond, func() {
		m.debounceMu.Lock()
		delete(m.debounceTimers, taskID)
		m.debounceMu.Unlock()
		m.processMetaSummary(context.Background(), taskID)
	})
}

func (m *MetaProcessor) processMetaSummary(ctx context.Context, taskID int64) {
	mu := m.getMetaLock(taskID)
	if !mu.TryLock() {
		m.markDirty(taskID)
		log.Printf("[meta-worker] task %d already running, marked dirty", taskID)
		return
	}
	defer func() {
		mu.Unlock()
		// Clean up locks and dirty flags after processing
		m.metaLocks.Delete(taskID)
		m.dirtyFlags.Delete(taskID)
	}()

	// Bounded retries for the roster-recompute loop. When saveLatestResultAndCompleteTask
	// aborts with errRosterChangedDuringMerge we `continue` to re-read the submitted
	// snapshot at loop top and re-aggregate with the updated roster (rather than `return`,
	// which would drop the task in Processing forever after the defer wipes the dirty flag).
	// This bound only guards the (theoretical) livelock of someone deleting members on
	// every iteration; normal convergence happens in a single extra pass.
	const maxRosterRetries = 5
	rosterRetries := 0

	for {
		m.clearDirty(taskID)

		log.Printf("[meta-worker] processing task %d", taskID)

		// V5 §4.4 completion gate (terminal-state based, prevents Failed dead-wait):
		// instead of a pure submitted>=accepted count, require every IN-ROSTER
		// (accepted, i.e. status NOT IN Pending/Declined) participant to have reached
		// a TERMINAL state — submitted_at IS NOT NULL OR its personal_result
		// worker_status==Failed. A Failed member just doesn't contribute its part this
		// round; it must NOT block aggregation forever (old pure-count code dead-waited).
		submitted, ready := metaCompletionReady(m.proc.db, taskID)
		if !ready {
			// Not every accepted participant is terminal yet. Whether or not anyone
			// has submitted, we must keep the task in Processing and wait for the
			// stragglers (or for their personal_result to flip to Failed).
			log.Printf("[meta-worker] task %d: %d submitted but not all accepted participants reached terminal state, waiting",
				taskID, len(submitted))
			return
		}
		if len(submitted) == 0 {
			// ready==true && submitted==0: every accepted participant reached a
			// terminal state yet NOBODY produced a usable part this round. The only
			// way to get here is that all confirmed members permanently Failed
			// (markPersonalFailed Declined them, emptying the accepted set). There is
			// nothing to aggregate and never will be — DO NOT just return, or the
			// task stays Processing forever and the overlap guard keeps blocking
			// later rounds (the deadlock). Converge the round to a terminal state.
			//
			// We use StatusCancelled to match the "zero-confirmation whole round
			// skip" semantics (scheduler.go uses StatusCancelled for a round that
			// produced no result): no usable contribution, no meta result, round
			// ends cleanly so subsequent rounds are not blocked.
			res := m.proc.db.Model(&model.SummaryTask{}).
				Where("id = ? AND status = ?", taskID, model.StatusProcessing).
				Update("status", model.StatusCancelled)
			if res.Error != nil {
				log.Printf("[meta-worker] task %d: failed to cancel all-failed round: %v", taskID, res.Error)
				return
			}
			if res.RowsAffected == 0 {
				log.Printf("[meta-worker] task %d: all accepted participants failed but task no longer Processing, nothing to do", taskID)
			} else {
				log.Printf("[meta-worker] task %d: all accepted participants permanently failed and none submitted; round cancelled (no meta result) to avoid Processing dead-wait", taskID)
			}
			return
		}

		runID := newSummaryRunID(taskID, "team")
		teamStream := newSummaryStreamSender(ctx, m.proc.cfg, taskID, "", streaming.ScopeTeam, runID)
		teamStreamFinalized := false
		finishTeamDone := func() {
			if teamStream != nil && !teamStreamFinalized {
				teamStream.Done(model.StatusCompleted)
				teamStreamFinalized = true
			}
		}
		finishTeamError := func(message string) {
			if teamStream != nil && !teamStreamFinalized {
				teamStream.Error(message)
				teamStreamFinalized = true
			}
		}
		closeTeamStream := func() {
			if teamStream != nil {
				teamStream.Close()
			}
		}
		teamStream.Stage(model.WorkflowStageGenerateSummary)

		var finalContent string
		var totalTokens int
		var teamCitations []model.TeamCitation

		if len(submitted) == 1 {
			// Single submission: copy content directly, no LLM call
			finalContent = submitted[0].Content
			totalTokens = 0
			_ = teamStream.Delta(finalContent)

			var participant model.SummaryParticipant
			m.proc.db.First(&participant, submitted[0].ParticipantRefID)
			name := participant.UserName
			if name == "" {
				name = submitted[0].UserID
			}
			teamCitations = []model.TeamCitation{{
				Index:            1,
				UserID:           submitted[0].UserID,
				UserName:         name,
				PersonalResultID: submitted[0].ID,
				TaskID:           taskID,
			}}
		} else {
			// Multiple submissions: call LLM to merge
			var task model.SummaryTask
			if err := m.proc.db.First(&task, taskID).Error; err != nil {
				log.Printf("[meta-worker] task %d not found: %v", taskID, err)
				finishTeamError("team summary task not found")
				closeTeamStream()
				return
			}

			startTime := task.TimeRangeStart.Format("2006-01-02 15:04")
			endTime := task.TimeRangeEnd.Format("2006-01-02 15:04")

			// Build participant summaries with [Pn] numbering
			var indexed []indexedParticipant
			var participantSummaries []struct{ Name, Summary string }

			for i, pr := range submitted {
				var participant model.SummaryParticipant
				m.proc.db.First(&participant, pr.ParticipantRefID)
				name := participant.UserName
				if name == "" {
					name = pr.UserID
				}
				indexed = append(indexed, indexedParticipant{Index: i + 1, UserID: pr.UserID, Name: name, PersonalResultID: pr.ID, TaskID: taskID})
				participantSummaries = append(participantSummaries, struct{ Name, Summary string }{
					Name:    name,
					Summary: pr.Content,
				})
			}

			generationTopic := m.proc.generationTopic(task)
			reduceStart := time.Now()
			content, tokens, err := m.proc.llm.CallReduceByPersonStream(ctx, participantSummaries, startTime, endTime, generationTopic, teamStream.Delta)
			reportKey := "team#" + strconv.FormatInt(taskID, 10)
			timing.RecordLLMSince(reportKey, "团队汇总: 合并各成员总结", reduceStart, tokens)
			timing.FlushReport(reportKey, time.Since(reduceStart).Milliseconds(), nil)
			if err != nil {
				log.Printf("[meta-worker] reduce error task=%d: %v", taskID, err)
				finishTeamError("team summary generation failed")
				closeTeamStream()
				return
			}
			finalContent = content
			totalTokens = tokens

			teamCitations = extractTeamCitations(finalContent, indexed)
		}

		now := timezone.Now()

		// Count total messages across all submitted personal results
		var totalMsgCount int
		for _, pr := range submitted {
			totalMsgCount += pr.MsgCount
		}

		result := model.SummaryResult{
			TaskID:         taskID,
			Content:        finalContent,
			TotalMsgCount:  totalMsgCount,
			TotalTokenUsed: totalTokens,
			ModelVersion:   m.proc.llm.ModelVersion(),
			GeneratedAt:    now,
		}
		result.SetTeamCitations(teamCitations)

		// P1 race guard: capture the personal_result IDs that actually entered this
		// aggregation (the `submitted` snapshot taken at loop top). The write tx in
		// saveLatestResultAndCompleteTask re-derives the committed contributor set and
		// aborts if a Leave/RemoveMember shrank the roster after this snapshot.
		snapshotContributorIDs := make([]int64, 0, len(submitted))
		for _, pr := range submitted {
			snapshotContributorIDs = append(snapshotContributorIDs, pr.ID)
		}

		// Test-only hook: simulate a member Leave/RemoveMember committing AFTER the
		// snapshot was captured but BEFORE the write tx (the exact race the roster
		// guard defends against). Production leaves afterSnapshotFn nil.
		if m.afterSnapshotFn != nil {
			m.afterSnapshotFn(taskID, snapshotContributorIDs)
		}

		// Best-effort check: don't write result if task is no longer Processing.
		// This is a best-effort guard; final safety is guaranteed by the task-level CAS below.
		var taskCheck model.SummaryTask
		if err := m.proc.db.Select("status").First(&taskCheck, taskID).Error; err != nil || taskCheck.Status != model.StatusProcessing {
			log.Printf("[meta-worker] task %d no longer processing before result write, aborting", taskID)
			finishTeamError("team summary task stopped before persistence")
			closeTeamStream()
			return
		}

		// Bug3: scheduled multi-person tasks prune old auto versions in place (same as
		// the single-person scheduled path); manual/human-driven multi-person runs retain
		// full version history. Determine isScheduled from the task's trigger type.
		var metaTask model.SummaryTask
		isScheduled := false
		if err := m.proc.db.Select("trigger_type", "schedule_id", "title").First(&metaTask, taskID).Error; err != nil {
			log.Printf("[meta-worker] task %d trigger_type lookup failed (defaulting isScheduled=false): %v", taskID, err)
		} else {
			isScheduled = metaTask.TriggerType == model.TriggerScheduled
			if isScheduled {
				result.OperationNote = m.proc.scheduledOperationNote(metaTask)
			}
		}
		if err := saveLatestResultAndCompleteTask(m.proc.db, taskID, &result, isScheduled, snapshotContributorIDs); err != nil {
			if errors.Is(err, errTaskNoLongerProcessing) {
				log.Printf("[meta-worker] task %d status changed during processing (likely cancelled), skipping completion", taskID)
				finishTeamError("team summary task stopped during persistence")
				closeTeamStream()
				return
			}
			if errors.Is(err, errRosterChangedDuringMerge) {
				rosterRetries++
				if rosterRetries > maxRosterRetries {
					log.Printf("[meta-worker] task %d: exceeded roster-recompute retries (%d), aborting to avoid livelock", taskID, maxRosterRetries)
					finishTeamError("team summary roster changed too often")
					closeTeamStream()
					return
				}
				log.Printf("[meta-worker] task %d: contributor roster changed during merge (member left/removed); recomputing with the updated roster (attempt %d)", taskID, rosterRetries)
				closeTeamStream()
				continue // re-read submitted snapshot at loop top and re-aggregate with the new roster
			}
			log.Printf("[meta-worker] save result error task=%d: %v", taskID, err)
			finishTeamError("team summary persistence failed")
			closeTeamStream()
			return
		}
		finishTeamDone()
		closeTeamStream()

		// Send WS notification (META_SUMMARY_UPDATED, broadcast to all)
		m.proc.sendCallback(model.TaskEvent{
			TaskID:    taskID,
			Status:    model.StatusCompleted,
			Progress:  100,
			Message:   "meta_summary_updated",
			EventType: "META_SUMMARY_UPDATED",
		})

		// Task durably reached Completed (saveLatestResultAndCompleteTask succeeded).
		// The notifier dedups on UNIQUE(task_id, completed), so a meta re-run that
		// produces a new version still only sends ONE completed notification.
		m.proc.notifyTaskTerminal(taskID, model.StatusCompleted)

		log.Printf("[meta-worker] task %d meta-summary version %d created (%d participants)",
			taskID, result.Version, len(submitted))

		// Check if new submissions arrived during processing
		if !m.isDirty(taskID) {
			break
		}
		log.Printf("[meta-worker] task %d has new submissions, re-running", taskID)
	}
}

var teamCitationRe = regexp.MustCompile(`\[P(\d{1,3})\]`)

// metaCompletionReady implements the V5 §4.4 terminal-state completion gate.
//
// It returns the submitted personal_results plus a `ready` flag that is true iff
// EVERY accepted (in-roster) participant has reached a terminal state:
//   - submitted_at IS NOT NULL (contributes its part), OR
//   - its personal_result.worker_status == Failed (this round drops its part,
//     does NOT block aggregation).
//
// "Accepted" = participant.status NOT IN (Pending, Declined): under V5 CONFIRM,
// un-confirmed members are never materialized as Accepted (方案乙), so they are
// naturally excluded here and never make meta wait. The old code keyed purely on
// submitted>=accepted, so a single Failed personal kept submitted below the count
// forever and meta dead-waited. Keying on terminal state fixes that.
func metaCompletionReady(db *gorm.DB, taskID int64) ([]model.PersonalResult, bool) {
	// Submitted personal results (the actual contributors this round).
	var submitted []model.PersonalResult
	db.Where("task_id = ? AND submitted_at IS NOT NULL", taskID).Find(&submitted)

	// Accepted (in-roster) participants for this round.
	var accepted []model.SummaryParticipant
	db.Where("task_id = ? AND status NOT IN (?, ?)", taskID, model.ParticipantPending, model.ParticipantDeclined).
		Find(&accepted)

	// Personal results keyed by participant_ref_id, for terminal-state lookup.
	var prs []model.PersonalResult
	db.Where("task_id = ?", taskID).Find(&prs)
	prByRef := make(map[int64]model.PersonalResult, len(prs))
	for _, pr := range prs {
		prByRef[pr.ParticipantRefID] = pr
	}

	// Every accepted participant must be terminal: submitted OR its personal Failed.
	for _, p := range accepted {
		pr, ok := prByRef[p.ID]
		terminal := ok && (pr.SubmittedAt != nil || pr.WorkerStatus == model.PersonalStatusFailed)
		if !terminal {
			return submitted, false
		}
	}
	return submitted, true
}

type indexedParticipant struct {
	Index            int
	UserID           string
	Name             string
	PersonalResultID int64
	TaskID           int64
}

func extractTeamCitations(text string, participants []indexedParticipant) []model.TeamCitation {
	matches := teamCitationRe.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return []model.TeamCitation{}
	}

	pMap := make(map[int]indexedParticipant, len(participants))
	for _, p := range participants {
		pMap[p.Index] = p
	}

	seen := make(map[int]bool)
	var result []model.TeamCitation
	for _, m := range matches {
		n, err := strconv.Atoi(m[1])
		if err != nil || seen[n] {
			continue
		}
		seen[n] = true
		if p, ok := pMap[n]; ok {
			result = append(result, model.TeamCitation{
				Index:            n,
				UserID:           p.UserID,
				UserName:         p.Name,
				PersonalResultID: p.PersonalResultID,
				TaskID:           p.TaskID,
			})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Index < result[j].Index })
	if result == nil {
		return []model.TeamCitation{}
	}
	return result
}
