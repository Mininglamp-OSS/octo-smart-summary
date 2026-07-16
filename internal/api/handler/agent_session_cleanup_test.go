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

func seedMsg(t *testing.T, db *gorm.DB, sessionID string, role string, createdAt time.Time) {
	t.Helper()
	if err := db.Create(&model.AgentMessage{
		SessionID: sessionID,
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

func TestRunOnce_expiredSessionCleaned(t *testing.T) {
	db, skip := newCleanupTestDB(t)
	if skip { return }
	// session A: 最后一条 25h 前 → 过期,该清
	seedMsg(t, db, "session-A", "user", time.Now().Add(-30*time.Hour))
	seedMsg(t, db, "session-A", "assistant", time.Now().Add(-25*time.Hour))

	runOnce(db)

	if got := countMsgs(t, db, "session-A"); got != 0 {
		t.Errorf("session-A should be cleaned, got %d rows", got)
	}
}

func TestRunOnce_freshSessionUntouched(t *testing.T) {
	db, skip := newCleanupTestDB(t)
	if skip { return }
	// session B: 最后一条 1h 前 → 活跃,不动
	seedMsg(t, db, "session-B", "user", time.Now().Add(-2*time.Hour))
	seedMsg(t, db, "session-B", "assistant", time.Now().Add(-1*time.Hour))

	runOnce(db)

	if got := countMsgs(t, db, "session-B"); got != 2 {
		t.Errorf("session-B should be untouched, got %d rows (want 2)", got)
	}
}

func TestRunOnce_borderline23_9hUntouched(t *testing.T) {
	db, skip := newCleanupTestDB(t)
	if skip { return }
	// session C: 最后一条 23h55min 前 → 还没到 24h,不动
	seedMsg(t, db, "session-C", "user", time.Now().Add(-23*time.Hour-55*time.Minute))

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
	seedMsg(t, db, "session-D", "user", time.Now().Add(-30*time.Hour))
	seedMsg(t, db, "session-D", "assistant", time.Now().Add(-1*time.Hour))

	runOnce(db)

	if got := countMsgs(t, db, "session-D"); got != 2 {
		t.Errorf("session-D still active (last msg 1h ago), should keep both rows, got %d", got)
	}
}

func TestRunOnce_multipleSessionsIsolated(t *testing.T) {
	db, skip := newCleanupTestDB(t)
	if skip { return }
	seedMsg(t, db, "session-old", "user", time.Now().Add(-48*time.Hour))
	seedMsg(t, db, "session-old", "assistant", time.Now().Add(-40*time.Hour))
	seedMsg(t, db, "session-new", "user", time.Now().Add(-30*time.Minute))
	seedMsg(t, db, "session-new", "assistant", time.Now().Add(-10*time.Minute))

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
