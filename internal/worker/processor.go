package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"gorm.io/gorm"
)

var callbackClient = &http.Client{Timeout: 5 * time.Second}

// Processor polls the DB for pending tasks and dispatches them to the pool.
type Processor struct {
	db          *gorm.DB
	imDB        *gorm.DB
	pool        *WorkerPool
	llm         *service.LLMClient
	cfg         *config.Config
	stopCh      chan struct{}
	triggerCh   chan model.WorkerTriggerRequest
	meta        *MetaProcessor
}

// NewProcessor creates a new task processor.
func NewProcessor(db, imDB *gorm.DB, pool *WorkerPool, llm *service.LLMClient, cfg *config.Config) *Processor {
	p := &Processor{
		db:        db,
		imDB:      imDB,
		pool:      pool,
		llm:       llm,
		cfg:       cfg,
		stopCh:    make(chan struct{}),
		triggerCh: make(chan model.WorkerTriggerRequest, 100),
	}
	p.meta = NewMetaProcessor(p)
	return p
}

// TriggerCh returns the channel for worker trigger requests.
func (p *Processor) TriggerCh() chan<- model.WorkerTriggerRequest {
	return p.triggerCh
}

// Run starts the polling loop. Call Stop() to exit.
func (p *Processor) Run() {
	interval := time.Duration(p.cfg.WorkerPollInterval) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Printf("[processor] polling every %v", interval)
	for {
		select {
		case <-p.stopCh:
			log.Println("[processor] stopping")
			return
		case <-ticker.C:
			p.poll()
		case req := <-p.triggerCh:
			p.handleTrigger(req)
		}
	}
}

func (p *Processor) handleTrigger(req model.WorkerTriggerRequest) {
	switch req.Type {
	case "personal_summary":
		p.pool.Submit(func() {
			p.processPersonalSummary(context.Background(), req.TaskID, req.ParticipantRefID)
		})
	case "meta_summary":
		p.meta.TriggerMetaSummary(req.TaskID)
	default:
		log.Printf("[processor] unknown trigger type: %s", req.Type)
	}
}

// Stop signals the processor to stop polling.
func (p *Processor) Stop() {
	close(p.stopCh)
}

func (p *Processor) poll() {
	now := time.Now().UTC()
	deadline := now.Add(time.Duration(p.cfg.WorkerLeaseMinutes) * time.Minute)

	// Claim tasks atomically: two-step select-then-claim per task.
	for i := 0; i < 10; i++ {
		// Step 1: find a candidate pending task
		var candidate model.SummaryTask
		if err := p.db.Where("status = ? AND retry_count < ? AND (processing_deadline IS NULL OR processing_deadline < ?)",
			model.StatusPending, p.cfg.WorkerMaxRetry, now).
			Order("id ASC").Limit(1).First(&candidate).Error; err != nil {
			return // no pending tasks
		}

		// Step 2: atomically claim it by ID (prevents race with other workers)
		result := p.db.Model(&model.SummaryTask{}).
			Where("id = ? AND status = ?", candidate.ID, model.StatusPending).
			Updates(map[string]interface{}{
				"status":              model.StatusProcessing,
				"processing_deadline": deadline,
			})
		if result.Error != nil {
			log.Printf("[processor] claim task %d: %v", candidate.ID, result.Error)
			return
		}
		if result.RowsAffected == 0 {
			continue // another worker claimed it, try next
		}

		// Reload to get fresh state after claim
		var task model.SummaryTask
		if err := p.db.First(&task, candidate.ID).Error; err != nil {
			log.Printf("[processor] reload task %d: %v", candidate.ID, err)
			continue
		}

		p.pool.Submit(func() {
			p.processTask(task)
		})
	}
}

func (p *Processor) processTask(task model.SummaryTask) {
	log.Printf("[processor] processing task %d (%s)", task.ID, task.TaskNo)

	// Send progress callback
	p.sendCallback(model.TaskEvent{
		TaskID:   task.ID,
		Status:   model.StatusProcessing,
		Progress: 10,
		Message:  "开始处理",
	})

	err := p.executePipeline(task)
	if err != nil {
		log.Printf("[processor] task %d failed: %v", task.ID, err)
		errMsg := err.Error()
		newRetry := task.RetryCount + 1
		newStatus := model.StatusPending
		if newRetry >= p.cfg.WorkerMaxRetry {
			newStatus = model.StatusFailed
		}
		casResult := p.db.Model(&model.SummaryTask{}).
			Where("id = ? AND status = ?", task.ID, model.StatusProcessing).
			Updates(map[string]interface{}{
				"status":              newStatus,
				"retry_count":         newRetry,
				"error_message":       errMsg,
				"processing_deadline": nil,
			})
		if casResult.Error != nil {
			log.Printf("[processor] task %d update to failed/retry failed: %v", task.ID, casResult.Error)
			return
		}
		if casResult.RowsAffected == 0 {
			log.Printf("[processor] task %d status changed during processing (likely cancelled), skipping failure update", task.ID)
			return
		}
		p.sendCallback(model.TaskEvent{
			TaskID:  task.ID,
			Status:  newStatus,
			Message: errMsg,
		})
		return
	}

	// Success — all tasks are BY_PERSON
	var participantCount int64
	p.db.Model(&model.SummaryParticipant{}).Where("task_id = ?", task.ID).Count(&participantCount)

	if participantCount <= 1 {
		casResult := p.db.Model(&model.SummaryTask{}).
			Where("id = ? AND status = ?", task.ID, model.StatusProcessing).
			Updates(map[string]interface{}{
				"processing_deadline": nil,
			})
		if casResult.Error != nil {
			log.Printf("[processor] task %d CAS update failed: %v", task.ID, casResult.Error)
			return
		}
		if casResult.RowsAffected == 0 {
			log.Printf("[processor] task %d status changed (likely cancelled), skipping dispatch", task.ID)
			return
		}
		var participants []model.SummaryParticipant
		p.db.Where("task_id = ?", task.ID).Find(&participants)
		for _, pt := range participants {
			p.db.Model(&model.SummaryParticipant{}).Where("id = ?", pt.ID).
				Update("status", model.ParticipantAccepted)
			ptID := pt.ID
			p.pool.Submit(func() {
				p.processPersonalSummary(context.Background(), task.ID, ptID)
			})
		}
		p.sendCallback(model.TaskEvent{
			TaskID:   task.ID,
			Status:   model.StatusProcessing,
			Progress: 50,
			Message:  "单人模式，自动处理中",
		})
		log.Printf("[processor] task %d single participant, skipping WaitingConfirm", task.ID)
	} else {
		// Multi-person: Creator already triggered by API handler;
		// other participants remain WaitingConfirm at participant level.
		casResult := p.db.Model(&model.SummaryTask{}).
			Where("id = ? AND status = ?", task.ID, model.StatusProcessing).
			Updates(map[string]interface{}{
				"processing_deadline": nil,
			})
		if casResult.Error != nil {
			log.Printf("[processor] task %d CAS update failed: %v", task.ID, casResult.Error)
			return
		}
		if casResult.RowsAffected == 0 {
			log.Printf("[processor] task %d status changed (likely cancelled), skipping dispatch", task.ID)
			return
		}
		p.sendCallback(model.TaskEvent{
			TaskID:   task.ID,
			Status:   model.StatusProcessing,
			Progress: 50,
			Message:  "处理中，等待参与者确认",
		})
		log.Printf("[processor] task %d multi-person, processing (participants pending confirm)", task.ID)
	}
}

func (p *Processor) executePipeline(task model.SummaryTask) error {
	ctx := context.Background()

	// Load sources
	var sources []model.SummarySource
	if err := p.db.Where("task_id = ?", task.ID).Find(&sources).Error; err != nil {
		return fmt.Errorf("load sources: %w", err)
	}

	// Build specified sources for pipeline
	specifiedSources := make([]map[string]interface{}, 0, len(sources))
	for _, s := range sources {
		specifiedSources = append(specifiedSources, map[string]interface{}{
			"source_id":   s.SourceID,
			"source_type": s.SourceType,
			"source_name": s.SourceName,
		})
	}

	// Fetch messages via pipeline
	llmFn := func(ctx context.Context, prompt string) (string, error) {
		return p.llm.CallRaw(ctx, prompt)
	}

	var messages []pipeline.Message
	var err error

	// Load participants for this task
	var participants []model.SummaryParticipant
	if err := p.db.Where("task_id = ?", task.ID).Find(&participants).Error; err != nil {
		return fmt.Errorf("load participants: %w", err)
	}
	var participantUIDs []string
	var participantNames []string
	for _, pt := range participants {
		if pt.UserID != task.CreatorID {
			participantUIDs = append(participantUIDs, pt.UserID)
			participantNames = append(participantNames, pt.UserName)
		}
	}
	messages, err = pipeline.ResolveAndFetchMessagesForPersonal(
		ctx, task.CreatorID, participantUIDs, participantNames, specifiedSources, task.Title,
		task.TimeRangeStart, task.TimeRangeEnd,
		p.imDB, llmFn, p.cfg.MsgTableCount,
	)
	if err != nil {
		return fmt.Errorf("fetch messages: %w", err)
	}

	if len(messages) == 0 {
		log.Printf("[processor] task %d: 0 messages fetched", task.ID)
		p.db.Model(&model.SummaryTask{}).Where("id = ?", task.ID).Update("total_msg_count", 0)
		return nil
	}

	// Personal summaries will be generated by personal_processor
	log.Printf("[processor] task %d: %d messages fetched, personal_processor will handle summaries", task.ID, len(messages))
	p.db.Model(&model.SummaryTask{}).Where("id = ?", task.ID).Update("total_msg_count", len(messages))
	return nil
}

func (p *Processor) sendCallback(event model.TaskEvent) {
	body, err := json.Marshal(event)
	if err != nil {
		log.Printf("[processor] marshal callback: %v", err)
		return
	}

	resp, err := callbackClient.Post(p.cfg.WorkerCallbackURL, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("[processor] callback POST failed: %v", err)
		// Fallback: save event to DB
		p.db.Create(&model.SummaryEvent{
			TaskID:   event.TaskID,
			Status:   event.Status,
			Progress: event.Progress,
			Message:  event.Message,
		})
		return
	}
	resp.Body.Close()
}

func joinStrings(ss []string) string {
	result := ""
	for i, s := range ss {
		if i > 0 {
			result += "\n"
		}
		result += s
	}
	return result
}

// batchResolveUserNames queries the IM DB for display names of the given message senders.
// Returns a map from UID to display name. UIDs that cannot be resolved are omitted.
func (p *Processor) batchResolveUserNames(messages []pipeline.Message) map[string]string {
	nameMap := make(map[string]string)
	if p.imDB == nil || len(messages) == 0 {
		return nameMap
	}

	// Collect unique UIDs
	uidSet := make(map[string]bool)
	for _, msg := range messages {
		if msg.SenderUID != "" {
			uidSet[msg.SenderUID] = true
		}
	}
	if len(uidSet) == 0 {
		return nameMap
	}

	uids := make([]string, 0, len(uidSet))
	for uid := range uidSet {
		uids = append(uids, uid)
	}

	// Batch query from user table
	type userRow struct {
		UID  string `gorm:"column:uid"`
		Name string `gorm:"column:name"`
	}
	var rows []userRow
	if err := p.imDB.Raw("SELECT uid, name FROM `user` WHERE uid IN ?", uids).Scan(&rows).Error; err != nil {
		log.Printf("[processor] batch resolve user names: %v", err)
		return nameMap
	}
	for _, r := range rows {
		if r.Name != "" {
			nameMap[r.UID] = r.Name
		}
	}
	log.Printf("[processor] resolved %d/%d user names", len(nameMap), len(uids))
	return nameMap
}
