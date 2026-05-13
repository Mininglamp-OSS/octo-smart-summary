package worker

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"gorm.io/gorm"
)

func escapeCitationMarkers(content string) string {
	return citationRe.ReplaceAllString(content, "($1)")
}

const noRelevantContentMessage = "在当前范围内未找到与主题相关的聊天记录。"

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
	deadline := time.Now().UTC().Add(time.Duration(p.cfg.WorkerLeaseMinutes) * time.Minute)
	taskCAS := p.db.Model(&model.SummaryTask{}).
		Where("id = ? AND status IN (?, ?)", taskID, model.StatusPending, model.StatusWaitingConfirm).
		Updates(map[string]interface{}{
			"status":              model.StatusProcessing,
			"processing_deadline": deadline,
		})
	if taskCAS.Error != nil {
		log.Printf("[personal-worker] task=%d CAS update failed: %v", taskID, taskCAS.Error)
		return
	}
	if taskCAS.RowsAffected == 0 {
		var currentTask model.SummaryTask
		if err := p.db.Select("status").First(&currentTask, taskID).Error; err != nil || currentTask.Status != model.StatusProcessing {
			log.Printf("[personal-worker] task=%d not in processing state, aborting", taskID)
			return
		}
		// Refresh deadline for already-processing task (prevents scheduler false-positive)
		p.db.Model(&model.SummaryTask{}).Where("id = ?", taskID).
			Update("processing_deadline", deadline)
	}

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
	if strings.TrimSpace(content) == "" {
		content = noRelevantContentMessage
	}

	// Best-effort check: abort early if task is no longer Processing.
	// Final safety is guaranteed by the task-level CAS in the completion path below.
	var taskCheck model.SummaryTask
	if err := p.db.Select("status").First(&taskCheck, taskID).Error; err != nil || taskCheck.Status != model.StatusProcessing {
		log.Printf("[personal-worker] task=%d no longer processing before result write, aborting", taskID)
		return
	}

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

		casResult := p.db.Model(&model.SummaryTask{}).
			Where("id = ? AND status = ?", taskID, model.StatusProcessing).
			Update("status", model.StatusCompleted)
		if casResult.Error != nil {
			log.Printf("[personal-worker] task %d update to completed failed: %v", taskID, casResult.Error)
			return
		}
		if casResult.RowsAffected == 0 {
			log.Printf("[personal-worker] task %d status changed during processing (likely cancelled), skipping completion", taskID)
			return
		}

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
	sanitized := sanitizeErrorForUser(errMsg)

	var shouldNotify bool
	txErr := p.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(pr).Updates(map[string]interface{}{
			"worker_status": model.PersonalStatusFailed,
			"error_message": &sanitized,
		}).Error; err != nil {
			return err
		}
		if err := tx.Model(participant).Update("status", model.ParticipantAccepted).Error; err != nil {
			return err
		}

		// In single-person mode, propagate failure to task level
		var participantCount int64
		if err := tx.Model(&model.SummaryParticipant{}).Where("task_id = ?", pr.TaskID).Count(&participantCount).Error; err != nil {
			return err
		}
		if participantCount <= 1 {
			result := tx.Model(&model.SummaryTask{}).
				Where("id = ? AND status = ?", pr.TaskID, model.StatusProcessing).
				Updates(map[string]interface{}{
					"status":        model.StatusFailed,
					"error_message": &sanitized,
				})
			if result.RowsAffected == 0 {
				log.Printf("[personal-worker] task=%d CAS update skipped (not in Processing state)", pr.TaskID)
			} else {
				shouldNotify = true
			}
		}
		return nil
	})

	if txErr != nil {
		log.Printf("[personal-worker] markPersonalFailed transaction failed: task=%d err=%v", pr.TaskID, txErr)
		return
	}

	if shouldNotify {
		p.sendCallback(model.TaskEvent{
			TaskID:   pr.TaskID,
			Status:   model.StatusFailed,
			Progress: 0,
			Message:  sanitized,
		})
	}
	log.Printf("[personal-worker] task=%d marked failed, sanitizedMsg=%s", pr.TaskID, sanitized)
}

func (p *Processor) executePersonalPipeline(ctx context.Context, task model.SummaryTask, userID string) (string, []model.Citation, int, int, string, error) {
	totalStart := time.Now()

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

	// Unified LLM tool-call callback (shared by all Function Call sites)
	toolCallFn := func(ctx context.Context, messages []service.ChatMessage, tools []service.Tool, forceFn string) (string, error) {
		args, _, err := p.llm.CallWithTools(ctx, messages, tools, forceFn, p.cfg.LLMTemperature)
		return args, err
	}

	// Legacy callback for PostRetrievalNarrow (still uses CallRaw)
	llmFn := func(ctx context.Context, prompt string) (string, error) {
		return p.llm.CallRaw(ctx, prompt)
	}

	// Fetch messages via personal pipeline (Layer 0-5)
	var channelScopeOpts *pipeline.ChannelScopeOptions
	if p.cfg.ChannelScopeEnabled {
		channelScopeOpts = &pipeline.ChannelScopeOptions{
			Enabled: true,
		}
	}

	messages, err := pipeline.ResolveAndFetchMessagesForPersonal(
		ctx, userID, nil, nil, specifiedSources, task.Title,
		task.TimeRangeStart, task.TimeRangeEnd,
		p.imDB, toolCallFn, llmFn,
		p.cfg.MsgTableCount, p.cfg.MaxMessagesPerChannel, p.cfg.FetchConcurrency,
		channelScopeOpts,
	)
	if err != nil {
		return "", nil, 0, 0, "", fmt.Errorf("fetch messages: %w", err)
	}

	// Resolve sender names (moved before FilterWithContext for ResolveTopicTarget)
	resolveStart := time.Now()
	nameMap := p.batchResolveUserNames(messages)
	log.Printf("[personal-worker] batchResolveUserNames took %dms (%d names)",
		time.Since(resolveStart).Milliseconds(), len(nameMap))

	// Resolve topic target via LLM Function Call
	targetUIDs := pipeline.ResolveTopicTarget(ctx, task.Title, nameMap, userID, toolCallFn)
	log.Printf("[personal-worker] topic target resolved: %v (creator=%s)", targetUIDs, userID)

	// Apply context window filter (signature changed: userID → targetUIDs)
	filterStart := time.Now()
	var userMessages []pipeline.Message
	if len(targetUIDs) > 0 {
		userMessages = pipeline.FilterWithContext(messages, targetUIDs, p.cfg.ContextWindow)
		log.Printf("[personal-worker] FilterWithContext took %dms (%d → %d messages, targets=%v)",
			time.Since(filterStart).Milliseconds(), len(messages), len(userMessages), targetUIDs)
	} else {
		userMessages = messages
		log.Printf("[personal-worker] no specific target, using all %d messages (took %dms)",
			len(messages), time.Since(filterStart).Milliseconds())
	}
	if len(userMessages) == 0 {
		return noRelevantContentMessage, nil, 0, 0, p.llm.ModelVersion(), nil
	}

	for i := range userMessages {
		if name, ok := nameMap[userMessages[i].SenderUID]; ok {
			userMessages[i].SenderName = name
		} else {
			userMessages[i].SenderName = userMessages[i].SenderUID
		}
	}

	// Assign CitationIndex to all messages (evidence pool)
	citIdx := 1
	targetMsgCount := 0
	for i := range userMessages {
		userMessages[i].CitationIndex = citIdx
		citIdx++
		if userMessages[i].IsTargetUser {
			targetMsgCount++
		}
	}

	// Token-aware chunking — resolve budget via explicit config / per-model default / global fallback
	maxTokens := p.cfg.ResolveMapMaxTokens()
	if maxTokens < 10000 {
		log.Printf("[config] resolved MapMaxTokens=%d too small, using default 100000", maxTokens)
		maxTokens = 100000
	}
	const systemPromptTokens = 3000
	const maxMsgsPerChunk = 800
	effectiveMax := maxTokens - systemPromptTokens

	var chunks [][]pipeline.Message
	var currentChunk []pipeline.Message
	currentTokens := 0

	for _, m := range userMessages {
		msgTokens := estimateTokens(m.Content, p.cfg.ResolveCharsPerTokenCJK(), p.cfg.CharsPerTokenASCII)
		if msgTokens > effectiveMax {
			log.Printf("[chunking] WARNING: single message exceeds token budget: %d > %d", msgTokens, effectiveMax)
		}
		if len(currentChunk) > 0 && (currentTokens+msgTokens > effectiveMax || len(currentChunk) >= maxMsgsPerChunk) {
			chunks = append(chunks, currentChunk)
			currentChunk = nil
			currentTokens = 0
		}
		currentChunk = append(currentChunk, m)
		currentTokens += msgTokens
		// Force flush if this single message already exceeds budget
		if msgTokens > effectiveMax {
			chunks = append(chunks, currentChunk)
			currentChunk = nil
			currentTokens = 0
		}
	}
	if len(currentChunk) > 0 {
		chunks = append(chunks, currentChunk)
	}
	log.Printf("[personal-worker] Chunking: %d messages → %d chunks (maxTokens=%d)",
		len(userMessages), len(chunks), effectiveMax)

	startTime := task.TimeRangeStart.Format("2006-01-02 15:04")
	endTime := task.TimeRangeEnd.Format("2006-01-02 15:04")
	sourceName := "多来源"
	if len(sources) == 1 {
		sourceName = sources[0].SourceName
	}

	// Determine userName: use target's name when topic points to someone else
	var userName string
	if len(targetUIDs) == 1 && targetUIDs[0] != userID {
		userName = nameMap[targetUIDs[0]]
	}
	if userName == "" {
		userName = nameMap[userID]
	}
	if userName == "" {
		userName = userID
	}

	// Map phase（并发）
	type chunkResult struct {
		summary string
		tokens  int
		failed  bool
		fatal   bool
	}

	concurrency := p.cfg.WorkerMapConcurrency
	if concurrency <= 0 {
		concurrency = 1
	}

	results := make([]chunkResult, len(chunks))
	mapSem := make(chan struct{}, concurrency)
	var mapWg sync.WaitGroup

	mapStart := time.Now()
	for i, chunk := range chunks {
		mapWg.Add(1)
		go func(idx int, c []pipeline.Message) {
			defer mapWg.Done()

			select {
			case mapSem <- struct{}{}:
			case <-ctx.Done():
				results[idx] = chunkResult{failed: true}
				return
			}
			defer func() { <-mapSem }()

			var formatted []string
			for _, m := range c {
				formatted = append(formatted, fmt.Sprintf("[%d][%s] %s: %s",
					m.CitationIndex, m.SendTime, m.SenderName,
					escapeCitationMarkers(m.Content)))
			}

			summary, tokens, err := p.llm.CallMap(ctx,
				joinStrings(formatted), sourceName, idx, len(c),
				startTime, endTime, task.Title, userName,
			)
			if err != nil {
				log.Printf("[personal-worker] Map chunk %d failed: %v", idx, err)
				isFatal := strings.Contains(err.Error(), "reasoning budget exhausted")
				results[idx] = chunkResult{failed: true, fatal: isFatal}
			} else {
				results[idx] = chunkResult{summary: summary, tokens: tokens}
			}
		}(i, chunk)
	}
	mapWg.Wait()
	log.Printf("[personal-worker] Map phase took %dms (%d chunks, concurrency=%d)",
		time.Since(mapStart).Milliseconds(), len(chunks), concurrency)

	if ctx.Err() != nil {
		return "", nil, 0, 0, "", fmt.Errorf("map phase cancelled: %w", ctx.Err())
	}

	var fatalChunks []int
	for i, r := range results {
		if r.fatal {
			fatalChunks = append(fatalChunks, i)
		}
	}
	if len(fatalChunks) > 0 {
		return "", nil, 0, 0, "", fmt.Errorf(
			"Map phase aborted: reasoning budget exhausted on chunk(s) %v", fatalChunks)
	}

	var chunkSummaries []string
	var totalTokens int
	for _, r := range results {
		if !r.failed && !strings.Contains(r.summary, service.MapFailedMarker) {
			chunkSummaries = append(chunkSummaries, r.summary)
			totalTokens += r.tokens
		}
	}
	if len(chunkSummaries) == 0 && len(results) > 0 {
		return "", nil, 0, 0, "", fmt.Errorf("all %d chunk(s) failed during Map phase (LLM unreachable)", len(results))
	}

	// Reduce phase
	reduceStart := time.Now()
	var finalContent string
	var reduceTokens int

	if len(chunkSummaries) == 1 {
		// Single chunk fast path: skip Reduce, use Map result directly
		finalContent = chunkSummaries[0]
		reduceTokens = 0
		log.Printf("[pipeline] single chunk — skipping Reduce")
	} else {
		// Multiple chunks: execute Reduce to merge
		var err error
		finalContent, reduceTokens, err = p.llm.CallReduce(ctx,
			chunkSummaries, sourceName, startTime, endTime, targetMsgCount, task.Title,
		)
		if err != nil {
			return "", nil, 0, 0, "", fmt.Errorf("reduce: %w", err)
		}
	}
	totalTokens += reduceTokens
	log.Printf("[personal-worker] Reduce phase took %dms", time.Since(reduceStart).Milliseconds())

	// Build citations from final content
	citationStart := time.Now()
	citations := buildCitations(finalContent, userMessages, messages, nameMap)
	finalContent, citations = dedupCitations(finalContent, citations)
	finalContent = stripOrphanCitations(finalContent, citations)
	log.Printf("[personal-worker] Citation build took %dms (%d citations)",
		time.Since(citationStart).Milliseconds(), len(citations))

	log.Printf("[personal-worker] Total executePersonalPipeline took %dms",
		time.Since(totalStart).Milliseconds())

	return finalContent, citations, targetMsgCount, totalTokens, p.llm.ModelVersion(), nil
}

func estimateTokens(content string, charsPerTokenCJK, charsPerTokenASCII int) int {
	const overheadPerMsg = 50
	// Defensive: avoid divide-by-zero or pathological values
	if charsPerTokenCJK <= 0 {
		charsPerTokenCJK = 1
	}
	if charsPerTokenASCII <= 0 {
		charsPerTokenASCII = 4
	}
	cjkCount := 0
	asciiCount := 0
	for _, r := range content {
		if r > 0x7F {
			cjkCount++
		} else {
			asciiCount++
		}
	}
	return cjkCount/charsPerTokenCJK + asciiCount/charsPerTokenASCII + overheadPerMsg
}

func sanitizeErrorForUser(errMsg string) string {
	switch {
	case strings.Contains(errMsg, "LLM API error"):
		return "AI 服务暂时不可用，请稍后重试"
	case strings.Contains(errMsg, "context deadline exceeded"):
		return "AI 处理超时，请稍后重试"
	case strings.Contains(errMsg, "all") && strings.Contains(errMsg, "chunk(s) failed"):
		return "AI 服务暂时不可用，所有分片处理失败"
	default:
		// Do not leak raw internal errors (may contain DSN, IPs, stack traces).
		// Raw error is already logged by the caller via log.Printf.
		return "AI 处理失败，请稍后重试"
	}
}
