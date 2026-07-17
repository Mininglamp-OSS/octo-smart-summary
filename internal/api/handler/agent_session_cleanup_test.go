package handler

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// helper: 起一个私有内存 sqlite + 迁移 agent_message 表
// 使用 ":memory:"(不加 file:: / ?cache=shared)确保每个测试独立 DB 不串
// 需 CGO(mattn/go-sqlite3) — CGO_ENABLED=0 环境自动 skip
func newCleanupTestDB(t *testing.T) (*gorm.DB, bool) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Skipf("CGO required for sqlite: %v", err)
		return nil, true
	}
	if err := db.AutoMigrate(&model.AgentMessage{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db, false
}

func seedMsg(t *testing.T, db *gorm.DB, sessionID, userID, role string, createdAt time.Time) {
	t.Helper()
	if err := db.Create(&model.AgentMessage{
		SessionID: sessionID,
		UserID:    userID,
		Role:      role,
		Content:   "test",
		CreatedAt: createdAt,
	}).Error; err != nil {
		t.Fatalf("seed msg: %v", err)
	}
}

func countMsgs(t *testing.T, db *gorm.DB, sessionID string) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&model.AgentMessage{}).Where("session_id = ?", sessionID).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// countMsgsFor 数指定 (user_id, session_id) 的行数，用于验证清理精确到属主。
func countMsgsFor(t *testing.T, db *gorm.DB, userID, sessionID string) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&model.AgentMessage{}).
		Where("user_id = ? AND session_id = ?", userID, sessionID).
		Count(&n).Error; err != nil {
		t.Fatalf("count for (user=%s session=%s): %v", userID, sessionID, err)
	}
	return n
}

func TestRunOnce_expiredSessionCleaned(t *testing.T) {
	db, skip := newCleanupTestDB(t)
	if skip { return }
	// session A: 最后一条 25h 前 → 过期,该清
	seedMsg(t, db, "session-A", "user-1", "user", time.Now().Add(-30*time.Hour))
	seedMsg(t, db, "session-A", "user-1", "assistant", time.Now().Add(-25*time.Hour))

	runOnce(db)

	if got := countMsgs(t, db, "session-A"); got != 0 {
		t.Errorf("session-A should be cleaned, got %d rows", got)
	}
}

func TestRunOnce_freshSessionUntouched(t *testing.T) {
	db, skip := newCleanupTestDB(t)
	if skip { return }
	// session B: 最后一条 1h 前 → 活跃,不动
	seedMsg(t, db, "session-B", "user-1", "user", time.Now().Add(-2*time.Hour))
	seedMsg(t, db, "session-B", "user-1", "assistant", time.Now().Add(-1*time.Hour))

	runOnce(db)

	if got := countMsgs(t, db, "session-B"); got != 2 {
		t.Errorf("session-B should be untouched, got %d rows (want 2)", got)
	}
}

func TestRunOnce_borderline23_9hUntouched(t *testing.T) {
	db, skip := newCleanupTestDB(t)
	if skip { return }
	// session C: 最后一条 23h55min 前 → 还没到 24h,不动
	seedMsg(t, db, "session-C", "user-1", "user", time.Now().Add(-23*time.Hour-55*time.Minute))

	runOnce(db)

	if got := countMsgs(t, db, "session-C"); got != 1 {
		t.Errorf("borderline (23h55m) should be untouched, got %d", got)
	}
}

func TestRunOnce_mixedFreshAndOldSessionPartiallyPreserved(t *testing.T) {
	// 关键 case:混合场景 —— 一个 session 里既有老消息也有新消息
	//   如果按"某条消息很老"就删,会误删活跃 session
	//   正确语义:按 session 的 MAX(created_at) 判断,只清全 session 都过期的
	db, skip := newCleanupTestDB(t)
	if skip { return }
	// session D: 有老消息 (30h 前) 也有新消息 (1h 前) → 整段 session 应保留
	seedMsg(t, db, "session-D", "user-1", "user", time.Now().Add(-30*time.Hour))
	seedMsg(t, db, "session-D", "user-1", "assistant", time.Now().Add(-1*time.Hour))

	runOnce(db)

	if got := countMsgs(t, db, "session-D"); got != 2 {
		t.Errorf("session-D still active (last msg 1h ago), should keep both rows, got %d", got)
	}
}

func TestRunOnce_multipleSessionsIsolated(t *testing.T) {
	db, skip := newCleanupTestDB(t)
	if skip { return }
	seedMsg(t, db, "session-old", "user-1", "user", time.Now().Add(-48*time.Hour))
	seedMsg(t, db, "session-old", "user-1", "assistant", time.Now().Add(-40*time.Hour))
	seedMsg(t, db, "session-new", "user-1", "user", time.Now().Add(-30*time.Minute))
	seedMsg(t, db, "session-new", "user-1", "assistant", time.Now().Add(-10*time.Minute))

	runOnce(db)

	if got := countMsgs(t, db, "session-old"); got != 0 {
		t.Errorf("session-old should be cleaned, got %d", got)
	}
	if got := countMsgs(t, db, "session-new"); got != 2 {
		t.Errorf("session-new should be untouched, got %d", got)
	}
}

func TestRunOnce_emptyTable(t *testing.T) {
	db, skip := newCleanupTestDB(t)
	if skip { return }
	// 表空,不应 panic 也不应报错
	runOnce(db)

	var total int64
	db.Model(&model.AgentMessage{}).Count(&total)
	if total != 0 {
		t.Errorf("empty table stays empty, got %d", total)
	}
}

// TestRunOnce_sameSessionIDDifferentUsers_scopedByOwner covers SUM-158 blocker 6:
// two different users happen to reuse the same session_id literal (allowed by
// the ownership model — (user_id, session_id) is the effective key). Both
// sessions have been idle > 24h so both must be cleaned, but the aggregation
// key had to switch from bare session_id to (user_id, session_id) or the
// bulk DELETE would either over-retain (any active tuple keeps the other's
// old tuple alive) or cross-user delete (`WHERE session_id IN (...)` sweeps
// both users' rows). Verify BOTH users' rows disappear when BOTH are expired.
func TestRunOnce_sameSessionIDDifferentUsers_scopedByOwner(t *testing.T) {
	db, skip := newCleanupTestDB(t)
	if skip { return }

	// Two users share the same session_id literal, both idle > 24h.
	seedMsg(t, db, "sess-shared", "user-alice", "user", time.Now().Add(-30*time.Hour))
	seedMsg(t, db, "sess-shared", "user-alice", "assistant", time.Now().Add(-25*time.Hour))
	seedMsg(t, db, "sess-shared", "user-bob", "user", time.Now().Add(-40*time.Hour))
	seedMsg(t, db, "sess-shared", "user-bob", "assistant", time.Now().Add(-26*time.Hour))

	runOnce(db)

	if got := countMsgsFor(t, db, "user-alice", "sess-shared"); got != 0 {
		t.Errorf("(alice, sess-shared) expired, expected 0 rows, got %d", got)
	}
	if got := countMsgsFor(t, db, "user-bob", "sess-shared"); got != 0 {
		t.Errorf("(bob, sess-shared) expired, expected 0 rows, got %d", got)
	}
}

// TestRunOnce_sameSessionIDDifferentUsers_activeTuplePreserved covers the
// dangerous inverse case: two users share a session_id literal, one is idle
// (should be cleaned), the other is active (must not be touched). Before
// blocker 6's (user_id, session_id) aggregation, either the active tuple
// would protect the stale one (over-retention) OR the bulk delete would
// sweep both (cross-user data loss). The correct behavior is precise:
// stale tuple gone, active tuple preserved.
func TestRunOnce_sameSessionIDDifferentUsers_activeTuplePreserved(t *testing.T) {
	db, skip := newCleanupTestDB(t)
	if skip { return }

	// Alice: idle > 24h → must be cleaned.
	seedMsg(t, db, "sess-shared", "user-alice", "user", time.Now().Add(-30*time.Hour))
	seedMsg(t, db, "sess-shared", "user-alice", "assistant", time.Now().Add(-25*time.Hour))
	// Bob: last message 1h ago → must be untouched.
	seedMsg(t, db, "sess-shared", "user-bob", "user", time.Now().Add(-2*time.Hour))
	seedMsg(t, db, "sess-shared", "user-bob", "assistant", time.Now().Add(-1*time.Hour))

	runOnce(db)

	if got := countMsgsFor(t, db, "user-alice", "sess-shared"); got != 0 {
		t.Errorf("(alice, sess-shared) idle > 24h, expected 0 rows, got %d", got)
	}
	if got := countMsgsFor(t, db, "user-bob", "sess-shared"); got != 2 {
		t.Errorf("(bob, sess-shared) still active, expected 2 rows preserved, got %d", got)
	}
}
