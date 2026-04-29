package worker

import (
	"context"
	"log"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
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

	for {
		m.clearDirty(taskID)

		log.Printf("[meta-worker] processing task %d", taskID)

		// Count accepted (non-declined, non-pending) participants
		var totalAccepted int64
		m.proc.db.Model(&model.SummaryParticipant{}).
			Where("task_id = ? AND status NOT IN (?, ?)", taskID, model.ParticipantPending, model.ParticipantDeclined).
			Count(&totalAccepted)

		// Query all submitted personal results
		var submitted []model.PersonalResult
		m.proc.db.Where("task_id = ? AND submitted_at IS NOT NULL", taskID).Find(&submitted)

		if len(submitted) == 0 {
			log.Printf("[meta-worker] task %d: no submitted results", taskID)
			return
		}

		allSubmitted := int64(len(submitted)) >= totalAccepted
		if !allSubmitted {
			log.Printf("[meta-worker] task %d: %d/%d participants submitted, waiting for all",
				taskID, len(submitted), totalAccepted)
			return
		}

		var finalContent string
		var totalTokens int
		var teamCitations []model.TeamCitation

		if len(submitted) == 1 {
			// Single submission: copy content directly, no LLM call
			finalContent = submitted[0].Content
			totalTokens = 0

			var participant model.SummaryParticipant
			m.proc.db.First(&participant, submitted[0].ParticipantRefID)
			name := participant.UserName
			if name == "" {
				name = submitted[0].UserID
			}
			teamCitations = []model.TeamCitation{{Index: 1, UserID: submitted[0].UserID, UserName: name}}
		} else {
			// Multiple submissions: call LLM to merge
			var task model.SummaryTask
			if err := m.proc.db.First(&task, taskID).Error; err != nil {
				log.Printf("[meta-worker] task %d not found: %v", taskID, err)
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
				indexed = append(indexed, indexedParticipant{Index: i + 1, UserID: pr.UserID, Name: name})
				participantSummaries = append(participantSummaries, struct{ Name, Summary string }{
					Name:    name,
					Summary: pr.Content,
				})
			}

			content, tokens, err := m.proc.llm.CallReduceByPerson(ctx, participantSummaries, startTime, endTime)
			if err != nil {
				log.Printf("[meta-worker] reduce error task=%d: %v", taskID, err)
				return
			}
			finalContent = content
			totalTokens = tokens

			teamCitations = extractTeamCitations(finalContent, indexed)
		}

		// Save result (new version)
		nextVer, _ := service.GetNextVersion(m.proc.db, taskID)
		now := time.Now().UTC()

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
			Version:        nextVer,
			GeneratedAt:    now,
		}
		result.SetTeamCitations(teamCitations)
		if err := m.proc.db.Create(&result).Error; err != nil {
			log.Printf("[meta-worker] save result error task=%d: %v", taskID, err)
			return
		}

		// Update task status
		m.proc.db.Model(&model.SummaryTask{}).Where("id = ?", taskID).
			Update("status", model.StatusCompleted)

		// Send WS notification (META_SUMMARY_UPDATED, broadcast to all)
		m.proc.sendCallback(model.TaskEvent{
			TaskID:    taskID,
			Status:    model.StatusCompleted,
			Progress:  100,
			Message:   "meta_summary_updated",
			EventType: "META_SUMMARY_UPDATED",
		})

		log.Printf("[meta-worker] task %d meta-summary version %d created (%d participants)",
			taskID, nextVer, len(submitted))

		// Check if new submissions arrived during processing
		if !m.isDirty(taskID) {
			break
		}
		log.Printf("[meta-worker] task %d has new submissions, re-running", taskID)
	}
}

var teamCitationRe = regexp.MustCompile(`\[P(\d{1,3})\]`)

type indexedParticipant struct {
	Index  int
	UserID string
	Name   string
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
				Index:    n,
				UserID:   p.UserID,
				UserName: p.Name,
			})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Index < result[j].Index })
	if result == nil {
		return []model.TeamCitation{}
	}
	return result
}
