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
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timing"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var callbackClient = &http.Client{Timeout: 5 * time.Second}

// Processor polls the DB for pending tasks and dispatches them to the pool.
type Processor struct {
	db         *gorm.DB
	imDB       *gorm.DB
	pool       *WorkerPool
	llm        *service.LLMClient
	cfg        *config.Config
	stopCh     chan struct{}
	triggerCh  chan model.WorkerTriggerRequest
	meta       *MetaProcessor
	notifier   Notifier
	createPRFn func(tx *gorm.DB, pr *model.PersonalResult) error
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
	p.notifier = NewNotifier(db, cfg)
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
	now := timezone.Now()
	deadline := now.Add(time.Duration(p.cfg.WorkerLeaseMinutes) * time.Minute)

	// Claim tasks atomically: two-step select-then-claim per task.
	for i := 0; i < 10; i++ {
		// Step 1: find a candidate pending task
		var candidate model.SummaryTask
		if err := p.db.Where("status = ? AND retry_count < ? AND (processing_deadline IS NULL OR processing_deadline < ?) AND deleted_at IS NULL",
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
	if err := p.db.Model(&model.SummaryParticipant{}).Where("task_id = ?", task.ID).Count(&participantCount).Error; err != nil {
		log.Printf("[processor] task %d count participants failed: %v", task.ID, err)
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

	// Defensive bootstrap: scheduled tasks (and legacy tasks reset for re-run)
	// may have no participant rows. Without at least the creator participant +
	// its personal_result, the single-participant branch below has nothing to
	// dispatch and the task is stuck in Processing forever. Create them here
	// (idempotently) so the personal pipeline can run end-to-end.
	if participantCount == 0 {
		creatorParticipantID, err := p.bootstrapCreatorParticipant(task)
		if err != nil {
			log.Printf("[processor] task %d bootstrap creator artifacts failed: %v", task.ID, err)
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
		participantCount = 1
		log.Printf("[processor] task %d bootstrapped creator participant %d", task.ID, creatorParticipantID)
	}

	if participantCount <= 1 {
		casResult := p.db.Model(&model.SummaryTask{}).
			Where("id = ? AND status = ?", task.ID, model.StatusProcessing).
			Updates(map[string]interface{}{
				"processing_deadline": timezone.Now().Add(time.Duration(p.cfg.WorkerLeaseMinutes) * time.Minute),
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
				"processing_deadline": timezone.Now().Add(time.Duration(p.cfg.WorkerLeaseMinutes) * time.Minute),
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

func (p *Processor) bootstrapCreatorParticipant(task model.SummaryTask) (int64, error) {
	now := timezone.Now()
	creatorName := service.ResolveUserName(task.CreatorID)
	var participant model.SummaryParticipant

	err := p.db.Transaction(func(tx *gorm.DB) error {
		participant = model.SummaryParticipant{
			TaskID:      task.ID,
			UserID:      task.CreatorID,
			UserName:    creatorName,
			Status:      model.ParticipantAccepted,
			ConfirmedAt: &now,
		}
		result := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "task_id"}, {Name: "user_id"}},
			DoNothing: true,
		}).Create(&participant)
		if result.Error != nil {
			return fmt.Errorf("upsert participant: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			if err := tx.Where("task_id = ? AND user_id = ?", task.ID, task.CreatorID).First(&participant).Error; err != nil {
				return fmt.Errorf("load existing participant: %w", err)
			}
		}

		participantUpdates := map[string]interface{}{}
		if participant.UserName == "" {
			participantUpdates["user_name"] = creatorName
		}
		if participant.Status != model.ParticipantAccepted {
			participantUpdates["status"] = model.ParticipantAccepted
		}
		if participant.ConfirmedAt == nil {
			participantUpdates["confirmed_at"] = now
		}
		if len(participantUpdates) > 0 {
			if err := tx.Model(&model.SummaryParticipant{}).Where("id = ?", participant.ID).Updates(participantUpdates).Error; err != nil {
				return fmt.Errorf("normalize participant: %w", err)
			}
		}

		pr := model.PersonalResult{
			TaskID:           task.ID,
			ParticipantRefID: participant.ID,
			UserID:           task.CreatorID,
			WorkerStatus:     model.PersonalStatusPending,
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		result = tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "task_id"}, {Name: "participant_ref_id"}},
			DoNothing: true,
		})
		if p.createPRFn != nil {
			if err := p.createPRFn(result, &pr); err != nil {
				return fmt.Errorf("upsert personal_result: %w", err)
			}
		} else {
			result = result.Create(&pr)
			if err := result.Error; err != nil {
				return fmt.Errorf("upsert personal_result: %w", err)
			}
		}
		if result.RowsAffected == 0 {
			if err := tx.Where("task_id = ? AND participant_ref_id = ?", task.ID, participant.ID).First(&pr).Error; err != nil {
				return fmt.Errorf("load existing personal_result: %w", err)
			}
		}

		if participant.PersonalResultID == nil || *participant.PersonalResultID != pr.ID {
			if err := tx.Model(&model.SummaryParticipant{}).
				Where("id = ? AND (personal_result_id IS NULL OR personal_result_id <> ?)", participant.ID, pr.ID).
				Update("personal_result_id", pr.ID).Error; err != nil {
				return fmt.Errorf("link participant personal_result: %w", err)
			}
		}

		return nil
	})
	if err != nil {
		return 0, err
	}
	return participant.ID, nil
}

func (p *Processor) executePipeline(task model.SummaryTask) error {
	pipelineStart := time.Now()
	defer func() { timing.Observe(task.TaskNo, "execute_pipeline_total", pipelineStart) }()
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

	// Fetch messages via pipeline. Tool-call / raw LLM uses in this (fetch) path
	// are accounted under the same task_no, so they appear in the same per-run
	// report that personal_pipeline flushes at the end.
	toolCallFn := func(ctx context.Context, messages []service.ChatMessage, tools []service.Tool, forceFn string) (string, error) {
		callStart := time.Now()
		args, tokens, err := p.llm.CallWithTools(ctx, messages, tools, forceFn, p.cfg.LLMTemperature)
		purpose := "检索预处理(tool-call)"
		if forceFn != "" {
			purpose = "检索预处理: " + forceFn
		}
		timing.RecordLLMSince(task.TaskNo, purpose, callStart, tokens)
		return args, err
	}
	llmFn := func(ctx context.Context, prompt string) (string, error) {
		callStart := time.Now()
		out, err := p.llm.CallRaw(ctx, prompt)
		timing.RecordLLMSince(task.TaskNo, "检索后裁剪 PostRetrievalNarrow", callStart, 0)
		return out, err
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

	var channelScopeOpts *pipeline.ChannelScopeOptions
	if p.cfg.ChannelScopeEnabled {
		channelScopeOpts = &pipeline.ChannelScopeOptions{
			Enabled: true,
		}
	}

	fetchStart := time.Now()
	messages, err = pipeline.ResolveAndFetchMessagesForPersonal(
		ctx, task.CreatorID, participantUIDs, participantNames, specifiedSources, task.Title,
		task.TimeRangeStart, task.TimeRangeEnd,
		p.imDB, toolCallFn, llmFn, p.cfg.MsgTableCount, p.cfg.MaxMessagesPerChannel, p.cfg.FetchConcurrency,
		channelScopeOpts,
	)
	timing.Observe(task.TaskNo, "fetch_messages", fetchStart)
	if err != nil {
		return fmt.Errorf("fetch messages: %w", err)
	}

	if len(messages) == 0 {
		log.Printf("[processor] task %d: 0 messages fetched", task.ID)
		return nil
	}

	// Personal summaries will be generated by personal_processor.
	// NOTE: total message count is persisted on SummaryResult.TotalMsgCount
	// by personal_processor; summary_task has no total_msg_count column.
	log.Printf("[processor] task %d: %d messages fetched, personal_processor will handle summaries", task.ID, len(messages))
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
