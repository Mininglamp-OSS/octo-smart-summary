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
func runOnce(db *gorm.DB) {
	cutoff := time.Now().Add(-cleanupAge)
	start := time.Now()

	// 用子查询定位:哪些 session_id 的最大 created_at 都 <= cutoff.
	// 这批就是全 session 都超过 24h 未动的老 session,整段清掉。
	//
	// 单条 SQL 不用事务(DELETE 天然是原子写),避免长事务锁。
	// 用 sub-select 精确匹配"最后活动时间"而不是"某条消息很老"—— 后者会
	// 误伤当前活跃 session 里偶然被删过消息的场景(虽然现在没有软删,但
	// 保留正确语义)。
	result := db.Exec(`
		DELETE FROM agent_message
		WHERE session_id IN (
			SELECT session_id FROM (
				SELECT session_id, MAX(created_at) AS last_at
				FROM agent_message
				GROUP BY session_id
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
}

// 兜底类型检查:确保 AgentMessage 表名不变时这段代码还生效
var _ = model.AgentMessage{}
