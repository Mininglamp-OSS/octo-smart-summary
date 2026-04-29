package worker

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

func (p *Processor) processPersonalSummary(ctx context.Context, taskID, participantRefID int64) {
	log.Printf("[personal-worker] start task=%d participant=%d", taskID, participantRefID)

	// Load participant
	var participant model.SummaryParticipant
	if err := p.db.First(&participant, participantRefID).Error; err != nil {
		log.Printf("[personal-worker] participant %d not found: %v", participantRefID, err)
		return
	}

	// Load or create personal result
	var pr model.PersonalResult
	if err := p.db.Where("task_id = ? AND participant_ref_id = ?", taskID, participantRefID).First(&pr).Error; err != nil {
		log.Printf("[personal-worker] personal result not found for task=%d participant=%d: %v", taskID, participantRefID, err)
		return
	}

	// CAS: only proceed if worker_status is still Pending (prevents duplicate runs)
	now := time.Now().UTC()
	cas := p.db.Model(&pr).
		Where("worker_status = ?", model.PersonalStatusPending).
		Update("worker_status", model.PersonalStatusProcessing)
	if cas.RowsAffected == 0 {
		log.Printf("[personal-worker] task=%d participant=%d already processing/completed, skipping", taskID, participantRefID)
		return
	}
	p.db.Model(&participant).Updates(map[string]interface{}{
		"status":            model.ParticipantProcessing,
		"worker_started_at": now,
	})

	// CAS update task status to PROCESSING (from any earlier state)
	p.db.Model(&model.SummaryTask{}).
		Where("id = ? AND status IN (?, ?)", taskID, model.StatusPending, model.StatusWaitingConfirm).
		Update("status", model.StatusProcessing)

	// Load task
	var task model.SummaryTask
	if err := p.db.First(&task, taskID).Error; err != nil {
		log.Printf("[personal-worker] task %d not found: %v", taskID, err)
		p.markPersonalFailed(&pr, &participant, "task not found")
		return
	}

	// Execute pipeline
	content, citations, msgCount, totalTokens, modelVer, err := p.executePersonalPipeline(ctx, task, participant.UserID)
	if err != nil {
		log.Printf("[personal-worker] pipeline error task=%d user=%s: %v", taskID, participant.UserID, err)
		p.markPersonalFailed(&pr, &participant, err.Error())
		return
	}
	if content == "" {
		content = "在当前范围内未找到与主题相关的聊天记录。"
	}

	// Mark completed
	pr.SetCitations(citations)
	genAt := time.Now().UTC()
	p.db.Model(&pr).Updates(map[string]interface{}{
		"content":          content,
		"citations_json":   pr.CitationsJSON,
		"msg_count":        msgCount,
		"total_token_used": totalTokens,
		"model_version":    modelVer,
		"worker_status":    model.PersonalStatusCompleted,
		"generated_at":     genAt,
	})
	p.db.Model(&participant).Updates(map[string]interface{}{
		"status": model.ParticipantCompleted,
	})

	// Send directed WS notification to the specific user
	p.sendCallback(model.TaskEvent{
		TaskID:       taskID,
		Status:       model.StatusProcessing,
		Progress:     100,
		Message:      fmt.Sprintf("personal_complete:%s", participant.UserID),
		EventType:    "PERSONAL_SUMMARY_STATUS",
		TargetUserID: participant.UserID,
	})

	// Check participant count to decide next step
	var participantCount int64
	p.db.Model(&model.SummaryParticipant{}).Where("task_id = ?", taskID).Count(&participantCount)

	if participantCount <= 1 {
		// Single-person mode: directly create SummaryResult and complete the task
		nextVer, _ := service.GetNextVersion(p.db, taskID)
		result := model.SummaryResult{
			TaskID:         taskID,
			Content:        content,
			TotalMsgCount:  msgCount,
			TotalTokenUsed: totalTokens,
			ModelVersion:   modelVer,
			Version:        nextVer,
			GeneratedAt:    genAt,
		}
		result.SetCitations(citations)
		if err := p.db.Create(&result).Error; err != nil {
			log.Printf("[personal-worker] save result error task=%d: %v", taskID, err)
			return
		}

		p.db.Model(&model.SummaryTask{}).Where("id = ?", taskID).
			Update("status", model.StatusCompleted)

		p.sendCallback(model.TaskEvent{
			TaskID:   taskID,
			Status:   model.StatusCompleted,
			Progress: 100,
			Message:  "总结完成",
		})
		log.Printf("[personal-worker] task %d single-person completed directly", taskID)
	} else {
		// Multi-person mode: trigger meta-summary to check if all participants completed
		p.meta.TriggerMetaSummary(taskID)
	}

	log.Printf("[personal-worker] completed task=%d user=%s msgs=%d", taskID, participant.UserID, msgCount)
}

func (p *Processor) markPersonalFailed(pr *model.PersonalResult, participant *model.SummaryParticipant, errMsg string) {
	p.db.Model(pr).Updates(map[string]interface{}{
		"worker_status": model.PersonalStatusFailed,
		"error_message": errMsg,
	})
	// Revert participant to accepted so they can retry
	p.db.Model(participant).Update("status", model.ParticipantAccepted)
}

func (p *Processor) executePersonalPipeline(ctx context.Context, task model.SummaryTask, userID string) (string, []model.Citation, int, int, string, error) {
	// Load sources
	var sources []model.SummarySource
	if err := p.db.Where("task_id = ?", task.ID).Find(&sources).Error; err != nil {
		return "", nil, 0, 0, "", fmt.Errorf("load sources: %w", err)
	}

	specifiedSources := make([]map[string]interface{}, 0, len(sources))
	for _, s := range sources {
		specifiedSources = append(specifiedSources, map[string]interface{}{
			"source_id":   s.SourceID,
			"source_type": s.SourceType,
			"source_name": s.SourceName,
		})
	}

	// Fetch messages via personal pipeline (with participant-aware filtering)
	llmFn := func(ctx context.Context, prompt string) (string, error) {
		return p.llm.CallRaw(ctx, prompt)
	}
	messages, err := pipeline.ResolveAndFetchMessagesForPersonal(
		ctx, userID, nil, nil, specifiedSources, task.Title,
		task.TimeRangeStart, task.TimeRangeEnd,
		p.imDB, llmFn, p.cfg.MsgTableCount,
	)
	if err != nil {
		return "", nil, 0, 0, "", fmt.Errorf("fetch messages: %w", err)
	}

	// Apply context window filter
	userMessages := pipeline.FilterWithContext(messages, userID, p.cfg.ContextWindow)
	if len(userMessages) == 0 {
		return "在当前范围内未找到与主题相关的聊天记录。", nil, 0, 0, p.llm.ModelVersion(), nil
	}

	// Resolve sender names
	nameMap := p.batchResolveUserNames(messages)
	for i := range userMessages {
		if name, ok := nameMap[userMessages[i].SenderUID]; ok {
			userMessages[i].SenderName = name
		} else {
			userMessages[i].SenderName = userMessages[i].SenderUID
		}
	}

	// Assign CitationIndex only to target user messages
	citIdx := 1
	targetMsgCount := 0
	for i := range userMessages {
		if userMessages[i].IsTargetUser {
			userMessages[i].CitationIndex = citIdx
			citIdx++
			targetMsgCount++
		}
	}

	// Split messages into chunks
	chunkSize := 500
	var chunks [][]pipeline.Message
	for i := 0; i < len(userMessages); i += chunkSize {
		end := i + chunkSize
		if end > len(userMessages) {
			end = len(userMessages)
		}
		chunks = append(chunks, userMessages[i:end])
	}

	startTime := task.TimeRangeStart.Format("2006-01-02 15:04")
	endTime := task.TimeRangeEnd.Format("2006-01-02 15:04")
	sourceName := "多来源"
	if len(sources) == 1 {
		sourceName = sources[0].SourceName
	}

	userName := nameMap[userID]
	if userName == "" {
		userName = userID
	}

	// Map phase
	var chunkSummaries []string
	var totalTokens int
	for i, chunk := range chunks {
		var formatted []string
		for _, m := range chunk {
			if m.IsTargetUser {
				formatted = append(formatted, fmt.Sprintf("[%d][%s] %s: %s",
					m.CitationIndex, m.SendTime, m.SenderName, m.Content))
			} else {
				formatted = append(formatted, fmt.Sprintf("[%s] %s: %s",
					m.SendTime, m.SenderName, m.Content))
			}
		}

		summary, tokens, err := p.llm.CallMap(ctx,
			joinStrings(formatted), sourceName, i, len(chunk),
			startTime, endTime, task.Title, userName,
		)
		if err != nil {
			return "", nil, 0, 0, "", fmt.Errorf("map chunk %d: %w", i, err)
		}
		chunkSummaries = append(chunkSummaries, summary)
		totalTokens += tokens
	}

	// Reduce phase
	finalContent, reduceTokens, err := p.llm.CallReduce(ctx,
		chunkSummaries, sourceName, startTime, endTime, targetMsgCount, task.Title,
	)
	if err != nil {
		return "", nil, 0, 0, "", fmt.Errorf("reduce: %w", err)
	}
	totalTokens += reduceTokens

	// Build citations from final content (only target messages have CitationIndex)
	citations := buildCitations(finalContent, userMessages, messages, nameMap)
	finalContent, citations = dedupCitations(finalContent, citations)

	return finalContent, citations, targetMsgCount, totalTokens, p.llm.ModelVersion(), nil
}
