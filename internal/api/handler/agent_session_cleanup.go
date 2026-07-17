package handler

import (
	"context"
	"log"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/gorm"
)

// agent_message 清理策略(CHAT-REFERENCE-BASED-DESIGN 后续加固,
// 见主人 2026-07-15 决策 D1..D5):
//
//   D1: 清所有过期 session (保存成总结的 session 在 CreateAgentSummary
//       tx 里已被销毁,活着的 session = 用户没保存的临时对话残留)
//   D2: 「过期」= 该 session 最后一条消息 created_at > 24h 前
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
//   1. 一个 (user, session) tuple 的活跃会保护另一个 (other_user, same_session)
//      的老 tuple 过期不掉——过度保留。
//   2. 当两个 tuple 都空闲后,`WHERE session_id IN (...)` 会一次删掉整段,
//      连带把最后活动比另一 tuple 更晚的行也误删——跨用户误删。
// 用 (user_id, session_id) 复合筛选,精确到属主。
//
// #161 P2 (yujiawei): agent_message_evidence must be cleaned symmetrically.
// After PR #161 evidence is the sole citation-handle discovery source for
// both getSessionMessagePool (mid-run) and buildCitationsForSession
// (save-time). Without cleanup, evidence rows accumulate indefinitely for
// any reused session_id and inflate the citation pool of every subsequent
// summarize_chunk. The evidence table has no created_at index on its own
// key columns — the age predicate reuses agent_message's last-activity
// timestamp so both tables retire together per (user_id, session_id).
func runOnce(db *gorm.DB) {
	cutoff := time.Now().Add(-cleanupAge)
	start := time.Now()

	// 按 (user_id, session_id) 聚合 MAX(created_at),定位两键都过期的 tuple.
	// 组合 IN 子查询在 MySQL 和 SQLite 3.7+ 都支持 (`WHERE (a, b) IN (SELECT a, b ...)`).
	//
	// 单条 SQL 不用事务(DELETE 天然是原子写),避免长事务锁。
	result := db.Exec(`
		DELETE FROM agent_message
		WHERE (user_id, session_id) IN (
			SELECT user_id, session_id FROM (
				SELECT user_id, session_id, MAX(created_at) AS last_at
				FROM agent_message
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

	// #161 P2 (yujiawei): symmetric evidence cleanup. Delete evidence rows
	// for (user_id, session_id) tuples whose evidence itself is older than
	// cleanupAge. Keying off evidence.created_at (not agent_message) is
	// simpler and self-contained — evidence is written synchronously by
	// PersistEvidence at fetch/peek/search/filter time, so its timestamps
	// reflect real user activity independent of AppendMessages ordering.
	evStart := time.Now()
	evResult := db.Exec(`
		DELETE FROM agent_message_evidence
		WHERE (user_id, session_id) IN (
			SELECT user_id, session_id FROM (
				SELECT user_id, session_id, MAX(created_at) AS last_at
				FROM agent_message_evidence
				GROUP BY user_id, session_id
				HAVING last_at <= ?
			) AS expired
		)
	`, cutoff)
	evElapsed := time.Since(evStart)
	if evResult.Error != nil {
		log.Printf("[agent-cleanup] ERROR evidence delete failed after %s: %v", evElapsed, evResult.Error)
		return
	}
	if evResult.RowsAffected > 0 {
		log.Printf("[agent-cleanup] cleaned %d evidence rows in %s (cutoff=%s)",
			evResult.RowsAffected, evElapsed, cutoff.Format(time.RFC3339))
	}
	if evElapsed > cleanupSlowThreshold {
		log.Printf("[agent-cleanup] SLOW evidence delete took %s (rows=%d, cutoff=%s) — consider indexing agent_message_evidence(session_id, created_at)",
			evElapsed, evResult.RowsAffected, cutoff.Format(time.RFC3339))
	}
}

// 兜底类型检查:确保 AgentMessage 表名不变时这段代码还生效
var _ = model.AgentMessage{}
var _ = model.AgentMessageEvidence{}
