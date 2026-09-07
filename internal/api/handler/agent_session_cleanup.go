package handler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// agent_message 清理策略(CHAT-REFERENCE-BASED-DESIGN 后续加固,
// 见主人 2026-07-15 决策 D1..D5):
//
//   D1: 清所有过期 Legacy session (保存成总结的 session 在 CreateAgentSummary
//       tx 里已被销毁,活着的 Legacy session = 用户没保存的临时对话残留)
//       summary workspace 由 agent_summary_session 管理生命周期,不在这里清理。
//   D2: 「过期」= 该 Legacy session 最后一条消息 created_at > 24h 前
//   D3: goroutine + time.Ticker,和 summary-api 进程同生命周期
//   D4: 日志折中 —— 只在清了 > 0 行、或耗时异常、或出错时打
//   D5: 独立分支/PR:feat/agent-session-cleanup-24h → feat/agent-framework-poc
//
// 为什么直接按 last-activity 删,不做 orphan-only 判断:
//   - 保存成总结时 CreateAgentSummary 事务里已 DELETE 该 session 全部行,
//     所以线上还活着的老 session 一定是用户没保存的
//   - 只按 "24h 未动" 判断简单可靠、SQL 快、不用关联 summary_task 表

const (
	// cleanupInterval 24h 触发一次
	cleanupInterval = 24 * time.Hour
	// cleanupAge session 最后活动超过 24h 判为过期
	cleanupAge = 24 * time.Hour
	// cleanupSlowThreshold 单次 DELETE 超过此阈值 → 打慢查询警报(D4 C)
	cleanupSlowThreshold = 1 * time.Second
	// cleanupJitter 首次执行前等一段随机时间,避免多实例撞车 & 冷启动瞬间打 DB
	cleanupInitialDelay = 30 * time.Second
	// workspaceCleanupBatchSize bounds each cleanup transaction so a large
	// backlog does not keep one scheduler pass focused on a single page.
	workspaceCleanupBatchSize = 200
	// workspaceCleanupINBatchSize keeps per-session cleanup below database
	// placeholder and packet limits even when one session has a long history.
	workspaceCleanupINBatchSize = 500
)

// StartAgentSessionCleanup 启动 24h 定时清理 goroutine。
// ctx 取消时干净退出,不阻塞进程关停(和 http.Server graceful shutdown 一致)。
// 只应在 main() 里调用一次;重复调用会开多个 ticker 但不会崩,只是浪费。
func StartAgentSessionCleanup(ctx context.Context, db *gorm.DB) {
	go func() {
		// 冷启动等一小段,避免和 migration/其他初始化并发压 DB
		select {
		case <-time.After(cleanupInitialDelay):
		case <-ctx.Done():
			return
		}
		// 首次立即跑一次(启动几十秒后),之后每 24h 一次
		runOnce(db)

		ticker := time.NewTicker(cleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				log.Printf("[agent-cleanup] shutting down")
				return
			case <-ticker.C:
				runOnce(db)
			}
		}
	}()
}

// runOnce 执行一次清理。分成小函数便于单测直接调用不依赖 ticker。
//
// 权限模型对齐(SUM-158 blocker 6):agent_message 的所有权键是
// (user_id, session_id)——按 blocker 1 的设计,两个不同用户允许偶然共用同一
// session_id 字面值。因此清理粒度也必须是 (user_id, session_id) 而不是裸
// session_id,否则:
//  1. 一个 (user, session) tuple 的活跃会保护另一个 (other_user, same_session)
//     的老 tuple 过期不掉——过度保留。
//  2. 当两个 tuple 都空闲后,`WHERE session_id IN (...)` 会一次删掉整段,
//     连带把最后活动比另一 tuple 更晚的行也误删——跨用户误删。
//
// 用 (user_id, session_id) 复合筛选,精确到属主。
//
// #161 P2 (yujiawei): agent_message_evidence must be cleaned symmetrically.
// After PR #161 evidence is the sole citation-handle discovery source for
// both getSessionMessagePool (mid-run) and buildCitationsForSession
// (save-time). Without cleanup, evidence rows accumulate indefinitely for
// any reused session_id and inflate the citation pool of every subsequent
// summarize_chunk. Evidence expiry uses its own last-activity timestamp, while
// durable workspace references below prevent premature collection.
func runOnce(db *gorm.DB) {
	now := timezone.Now()
	cutoff := now.Add(-cleanupAge)
	start := timezone.Now()

	// 只在 Legacy 消息(space_id = '')中按 (user_id, session_id) 聚合
	// MAX(created_at),定位两键都过期的 tuple。workspace 消息由持久化 session
	// 引用,不能被 Legacy 24h 清理删除；同 tuple 下的新 workspace 消息也不能
	// 反过来阻止过期 Legacy 消息清理。
	// 组合 IN 子查询在 MySQL 和 SQLite 3.7+ 都支持 (`WHERE (a, b) IN (SELECT a, b ...)`).
	//
	// 单条 SQL 不用事务(DELETE 天然是原子写),避免长事务锁。
	result := db.Exec(`
		DELETE FROM agent_message
		WHERE space_id = ''
		  AND (user_id, session_id) IN (
			SELECT user_id, session_id FROM (
				SELECT user_id, session_id, MAX(created_at) AS last_at
				FROM agent_message
				WHERE space_id = ''
				GROUP BY user_id, session_id
				HAVING last_at <= ?
			) AS expired
		)
	`, cutoff)

	elapsed := time.Since(start)

	// D4 C: 折中日志策略
	//   1. 出错必打
	//   2. 清了 > 0 行才打(N=0 静默,不制造噪音)
	//   3. 慢查询(超过阈值)必打警报
	if result.Error != nil {
		log.Printf("[agent-cleanup] ERROR delete failed after %s: %v", elapsed, result.Error)
		return
	}
	if result.RowsAffected > 0 {
		log.Printf("[agent-cleanup] cleaned %d rows in %s (cutoff=%s)",
			result.RowsAffected, elapsed, cutoff.Format(time.RFC3339))
	}
	if elapsed > cleanupSlowThreshold {
		log.Printf("[agent-cleanup] SLOW delete took %s (rows=%d, cutoff=%s) — consider indexing agent_message(session_id, created_at)",
			elapsed, result.RowsAffected, cutoff.Format(time.RFC3339))
	}

	workspaceStart := timezone.Now()
	workspaceCount, workspaceErr := cleanupExpiredSummaryWorkspaces(db, now)
	workspaceElapsed := time.Since(workspaceStart)
	if workspaceErr != nil {
		log.Printf("[agent-cleanup] ERROR workspace cleanup failed after %s: %v", workspaceElapsed, workspaceErr)
	} else if workspaceCount > 0 {
		log.Printf("[agent-cleanup] cleaned %d expired workspace sessions in %s", workspaceCount, workspaceElapsed)
	}
	if workspaceElapsed > cleanupSlowThreshold {
		log.Printf("[agent-cleanup] SLOW workspace cleanup took %s (sessions=%d)", workspaceElapsed, workspaceCount)
	}

	// #161 P2 (yujiawei): symmetric evidence cleanup. Delete evidence rows
	// for (user_id, session_id) tuples whose evidence itself is older than
	// cleanupAge. Keying off evidence.created_at (not agent_message) is
	// simpler and self-contained — evidence is written synchronously by
	// PersistEvidence at fetch/peek/search/filter time, so its timestamps
	// reflect real user activity independent of AppendMessages ordering.
	// Evidence has no space_id, so workspace Agent runs use a space-derived
	// agent_session_id. Preserve every (user_id, agent_session_id) tuple referenced
	// by agent_summary_session; workspace retirement must remove that session
	// before this Legacy cleanup may collect its evidence.
	evStart := timezone.Now()
	evRows, evErr := cleanupExpiredAgentEvidence(db, cutoff)
	evElapsed := time.Since(evStart)
	if evErr != nil {
		log.Printf("[agent-cleanup] ERROR evidence delete failed after %s: %v", evElapsed, evErr)
		return
	}
	if evRows > 0 {
		log.Printf("[agent-cleanup] cleaned %d evidence rows in %s (cutoff=%s)",
			evRows, evElapsed, cutoff.Format(time.RFC3339))
	}
	if evElapsed > cleanupSlowThreshold {
		log.Printf("[agent-cleanup] SLOW evidence delete took %s (rows=%d, cutoff=%s) — consider indexing agent_message_evidence(session_id, created_at)",
			evElapsed, evRows, cutoff.Format(time.RFC3339))
	}
}

func cleanupExpiredAgentEvidence(db *gorm.DB, cutoff time.Time) (int64, error) {
	if db == nil {
		return 0, fmt.Errorf("evidence cleanup database is required")
	}
	// Workspace session identifiers are binary-collated in MySQL, while the
	// older evidence table uses utf8mb4_unicode_ci. Make the comparison explicit
	// so MySQL does not raise error 1267 and identifier matching remains
	// case-sensitive. SQLite uses the portable equality expression in tests.
	sessionMatch := "agent_summary_session.agent_session_id = agent_message_evidence.session_id"
	runSessionMatch := "live_run.session_id = agent_message_evidence.session_id"
	if db.Dialector.Name() == "mysql" {
		sessionMatch = "agent_summary_session.agent_session_id = agent_message_evidence.session_id COLLATE utf8mb4_0900_bin"
		runSessionMatch = "live_run.session_id COLLATE utf8mb4_0900_bin = agent_message_evidence.session_id COLLATE utf8mb4_0900_bin"
	}
	result := db.Exec(fmt.Sprintf(`
		DELETE FROM agent_message_evidence
		WHERE (user_id, session_id) IN (
			SELECT user_id, session_id FROM (
				SELECT user_id, session_id, MAX(created_at) AS last_at
				FROM agent_message_evidence
				GROUP BY user_id, session_id
				HAVING last_at <= ?
			) AS expired
		)
		  AND NOT EXISTS (
			SELECT 1
			FROM agent_summary_session
			WHERE agent_summary_session.user_id = agent_message_evidence.user_id
			  AND %s
		)
		  AND NOT EXISTS (
			SELECT 1
			FROM agent_summary_session AS live_session
			JOIN agent_summary_turn AS live_turn
			  ON live_turn.space_id = live_session.space_id
			 AND live_turn.user_id = live_session.user_id
			 AND live_turn.session_id = live_session.session_id
			JOIN agent_summary_run AS live_run
			  ON live_run.user_id = live_turn.user_id
			 AND live_run.run_id = live_turn.run_id
			WHERE live_session.user_id = agent_message_evidence.user_id
			  AND %s
		)
	`, sessionMatch, runSessionMatch), cutoff)
	return result.RowsAffected, result.Error
}

// cleanupExpiredSummaryWorkspaces retires inactive workspace state after the
// 30-day sliding retention window maintained by AgentWorkspaceStore. Rows
// created before expires_at was populated fall back to updated_at so rollout
// does not leave permanent NULL tombstones.
func cleanupExpiredSummaryWorkspaces(db *gorm.DB, now time.Time) (int64, error) {
	if db == nil {
		return 0, fmt.Errorf("workspace cleanup database is required")
	}
	legacyCutoff := now.Add(-summaryWorkspaceRetention)
	var cleaned int64
	var afterID int64
	var cleanupErrs []error
	for {
		var sessions []model.AgentSummarySession
		if err := db.Where("id > ? AND (expires_at <= ? OR (expires_at IS NULL AND updated_at <= ?))", afterID, now, legacyCutoff).
			Order("id ASC").Limit(workspaceCleanupBatchSize).Find(&sessions).Error; err != nil {
			return cleaned, fmt.Errorf("load expired workspace sessions: %w", err)
		}
		if len(sessions) == 0 {
			break
		}
		for _, session := range sessions {
			afterID = session.ID
			deleted, err := deleteExpiredWorkspaceSessionState(db, session.ID, now, legacyCutoff)
			if err != nil {
				cleanupErrs = append(cleanupErrs, err)
				log.Printf("[agent-cleanup] workspace session %d cleanup failed: %v", session.ID, err)
				continue
			}
			if deleted {
				cleaned++
			}
		}
		if len(sessions) < workspaceCleanupBatchSize {
			break
		}
	}

	// Idempotency bindings only need to outlive their live task. Retire old
	// orphan/tombstone rows after the same window while preserving bindings for
	// summaries that still exist.
	if err := db.Exec(`
		DELETE FROM summary_workflow_idempotency
		WHERE created_at <= ?
		  AND NOT EXISTS (
			SELECT 1 FROM summary_task
			WHERE summary_task.id = summary_workflow_idempotency.task_id
			  AND summary_task.deleted_at IS NULL
		  )
	`, legacyCutoff).Error; err != nil {
		cleanupErrs = append(cleanupErrs, fmt.Errorf("clean workflow idempotency tombstones: %w", err))
	}
	return cleaned, errors.Join(cleanupErrs...)
}

func deleteExpiredWorkspaceSessionState(db *gorm.DB, sessionID int64, now, legacyCutoff time.Time) (bool, error) {
	deleted := false
	err := db.Transaction(func(tx *gorm.DB) error {
		var session model.AgentSummarySession
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", sessionID).Take(&session).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return fmt.Errorf("lock workspace session %d: %w", sessionID, err)
		}
		if !workspaceSessionExpired(session, now, legacyCutoff) {
			return nil
		}
		if session.ActiveTurnID > 0 {
			var turn model.AgentSummaryTurn
			err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("id = ? AND space_id = ? AND user_id = ? AND session_id = ?", session.ActiveTurnID, session.SpaceID, session.UserID, session.SessionID).
				Take(&turn).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("lock active workspace turn %d: %w", session.ActiveTurnID, err)
			}
			if err == nil && turn.Status == "running" && turn.LeaseExpiresAt != nil && turn.LeaseExpiresAt.After(now) {
				return nil
			}
		}
		if err := deleteWorkspaceSessionState(tx, session); err != nil {
			return err
		}
		deleted = true
		return nil
	})
	return deleted, err
}

func workspaceSessionExpired(session model.AgentSummarySession, now, legacyCutoff time.Time) bool {
	if session.ExpiresAt != nil {
		return !session.ExpiresAt.After(now)
	}
	return !session.UpdatedAt.After(legacyCutoff)
}

func deleteWorkspaceSessionState(tx *gorm.DB, session model.AgentSummarySession) error {
	var runIDs []string
	if err := tx.Model(&model.AgentMessage{}).
		Where("space_id = ? AND user_id = ? AND session_id = ? AND run_id <> ''", session.SpaceID, session.UserID, session.SessionID).
		Distinct().Pluck("run_id", &runIDs).Error; err != nil {
		return fmt.Errorf("load workspace run ids for session %d: %w", session.ID, err)
	}

	// Failed Agent turns have no agent_message row, but maybePersistSummaryRun
	// has already written their run/spec. Derive every internal Agent session
	// identity used by this workspace's turns so cleanup covers those runs too.
	var turns []model.AgentSummaryTurn
	if err := tx.Select("scope_version", "request_id").Model(&model.AgentSummaryTurn{}).
		Where("space_id = ? AND user_id = ? AND session_id = ?", session.SpaceID, session.UserID, session.SessionID).
		Find(&turns).Error; err != nil {
		return fmt.Errorf("load workspace turn identities for session %d: %w", session.ID, err)
	}
	evidenceSessions := make([]string, 0, len(turns)*2+1)
	seenEvidenceSessions := make(map[string]struct{}, len(turns)*2+1)
	addEvidenceSession := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, exists := seenEvidenceSessions[value]; exists {
			return
		}
		seenEvidenceSessions[value] = struct{}{}
		evidenceSessions = append(evidenceSessions, value)
	}
	addEvidenceSession(session.AgentSessionID)
	for _, turn := range turns {
		addEvidenceSession(summaryWorkspaceAgentSessionID(session.SpaceID, session.SessionID, turn.ScopeVersion))
		addEvidenceSession(summaryWorkspaceReplacementAgentSessionID(session.SpaceID, session.SessionID, turn.ScopeVersion, turn.RequestID))
	}

	seenRunIDs := make(map[string]struct{}, len(runIDs))
	for _, runID := range runIDs {
		seenRunIDs[runID] = struct{}{}
	}
	var runs []model.AgentSummaryRun
	seenLoadedRuns := make(map[string]struct{})
	appendRuns := func(batch []model.AgentSummaryRun) {
		for _, run := range batch {
			if _, exists := seenLoadedRuns[run.RunID]; exists {
				continue
			}
			seenLoadedRuns[run.RunID] = struct{}{}
			runs = append(runs, run)
		}
	}
	for start := 0; start < len(evidenceSessions); start += workspaceCleanupINBatchSize {
		end := min(start+workspaceCleanupINBatchSize, len(evidenceSessions))
		var batch []model.AgentSummaryRun
		if err := tx.Select("run_id", "session_id").
			Where("user_id = ? AND session_id IN ?", session.UserID, evidenceSessions[start:end]).
			Find(&batch).Error; err != nil {
			return fmt.Errorf("load workspace runs by session for session %d: %w", session.ID, err)
		}
		appendRuns(batch)
	}
	for start := 0; start < len(runIDs); start += workspaceCleanupINBatchSize {
		end := min(start+workspaceCleanupINBatchSize, len(runIDs))
		var batch []model.AgentSummaryRun
		if err := tx.Select("run_id", "session_id").
			Where("user_id = ? AND run_id IN ?", session.UserID, runIDs[start:end]).
			Find(&batch).Error; err != nil {
			return fmt.Errorf("load workspace runs by id for session %d: %w", session.ID, err)
		}
		appendRuns(batch)
	}
	for _, run := range runs {
		addEvidenceSession(run.SessionID)
		if _, exists := seenRunIDs[run.RunID]; !exists {
			seenRunIDs[run.RunID] = struct{}{}
			runIDs = append(runIDs, run.RunID)
		}
	}
	if len(runIDs) > 0 {
		for start := 0; start < len(runIDs); start += workspaceCleanupINBatchSize {
			end := min(start+workspaceCleanupINBatchSize, len(runIDs))
			batch := runIDs[start:end]
			if err := tx.Where("run_id IN ?", batch).Delete(&model.AgentCitationManifest{}).Error; err != nil {
				return fmt.Errorf("delete workspace citation manifests for session %d: %w", session.ID, err)
			}
			if err := tx.Where("run_id IN ?", batch).Delete(&model.AgentEvidenceArtifact{}).Error; err != nil {
				return fmt.Errorf("delete workspace evidence artifacts for session %d: %w", session.ID, err)
			}
			if err := tx.Where("run_id IN ?", batch).Delete(&model.AgentSummarySpec{}).Error; err != nil {
				return fmt.Errorf("delete workspace specs for session %d: %w", session.ID, err)
			}
			if err := tx.Where("user_id = ? AND run_id IN ?", session.UserID, batch).Delete(&model.AgentSummaryRun{}).Error; err != nil {
				return fmt.Errorf("delete workspace runs for session %d: %w", session.ID, err)
			}
		}
	}
	if len(evidenceSessions) > 0 {
		for start := 0; start < len(evidenceSessions); start += workspaceCleanupINBatchSize {
			end := min(start+workspaceCleanupINBatchSize, len(evidenceSessions))
			if err := tx.Where("user_id = ? AND session_id IN ?", session.UserID, evidenceSessions[start:end]).Delete(&model.AgentMessageEvidence{}).Error; err != nil {
				return fmt.Errorf("delete workspace evidence for session %d: %w", session.ID, err)
			}
		}
	}
	if err := tx.Where("space_id = ? AND user_id = ? AND session_id = ?", session.SpaceID, session.UserID, session.SessionID).
		Delete(&model.AgentMessage{}).Error; err != nil {
		return fmt.Errorf("delete workspace messages for session %d: %w", session.ID, err)
	}
	if err := tx.Where("space_id = ? AND user_id = ? AND session_id = ?", session.SpaceID, session.UserID, session.SessionID).
		Delete(&model.AgentSummaryTurn{}).Error; err != nil {
		return fmt.Errorf("delete workspace turns for session %d: %w", session.ID, err)
	}
	if err := tx.Where("id = ?", session.ID).Delete(&model.AgentSummarySession{}).Error; err != nil {
		return fmt.Errorf("delete workspace session %d: %w", session.ID, err)
	}
	return nil
}

// 兜底类型检查:确保 AgentMessage 表名不变时这段代码还生效
var _ = model.AgentMessage{}
var _ = model.AgentMessageEvidence{}
