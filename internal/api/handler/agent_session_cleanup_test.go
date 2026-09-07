package handler

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// helper: 起一个私有内存 sqlite + 迁移 cleanup 涉及的表
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
	if err := db.AutoMigrate(
		&model.AgentMessage{},
		&model.AgentMessageEvidence{},
		&model.AgentSummarySession{},
		&model.AgentSummaryTurn{},
		&model.AgentSummaryRun{},
		&model.AgentSummarySpec{},
		&model.AgentEvidenceArtifact{},
		&model.AgentCitationManifest{},
		&model.SummaryWorkflowIdempotency{},
		&model.SummaryTask{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db, false
}

func seedMsg(t *testing.T, db *gorm.DB, sessionID, userID, role string, createdAt time.Time) {
	t.Helper()
	seedMsgInSpace(t, db, "", sessionID, userID, role, createdAt)
}

func seedMsgInSpace(t *testing.T, db *gorm.DB, spaceID, sessionID, userID, role string, createdAt time.Time) {
	t.Helper()
	if err := db.Create(&model.AgentMessage{
		SpaceID:   spaceID,
		SessionID: sessionID,
		UserID:    userID,
		Role:      role,
		Content:   "test",
		CreatedAt: createdAt,
	}).Error; err != nil {
		t.Fatalf("seed msg: %v", err)
	}
}

func countMsgsInSpace(t *testing.T, db *gorm.DB, spaceID, userID, sessionID string) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&model.AgentMessage{}).
		Where("space_id = ? AND user_id = ? AND session_id = ?", spaceID, userID, sessionID).
		Count(&n).Error; err != nil {
		t.Fatalf("count for (space=%s user=%s session=%s): %v", spaceID, userID, sessionID, err)
	}
	return n
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
	if skip {
		return
	}
	// session A: 最后一条 25h 前 → 过期,该清
	seedMsg(t, db, "session-A", "user-1", "user", timezone.Now().Add(-30*time.Hour))
	seedMsg(t, db, "session-A", "user-1", "assistant", timezone.Now().Add(-25*time.Hour))

	runOnce(db)

	if got := countMsgs(t, db, "session-A"); got != 0 {
		t.Errorf("session-A should be cleaned, got %d rows", got)
	}
}

func TestRunOnce_freshSessionUntouched(t *testing.T) {
	db, skip := newCleanupTestDB(t)
	if skip {
		return
	}
	// session B: 最后一条 1h 前 → 活跃,不动
	seedMsg(t, db, "session-B", "user-1", "user", timezone.Now().Add(-2*time.Hour))
	seedMsg(t, db, "session-B", "user-1", "assistant", timezone.Now().Add(-1*time.Hour))

	runOnce(db)

	if got := countMsgs(t, db, "session-B"); got != 2 {
		t.Errorf("session-B should be untouched, got %d rows (want 2)", got)
	}
}

func TestRunOnce_borderline23_9hUntouched(t *testing.T) {
	db, skip := newCleanupTestDB(t)
	if skip {
		return
	}
	// session C: 最后一条 23h55min 前 → 还没到 24h,不动
	seedMsg(t, db, "session-C", "user-1", "user", timezone.Now().Add(-23*time.Hour-55*time.Minute))

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
	if skip {
		return
	}
	// session D: 有老消息 (30h 前) 也有新消息 (1h 前) → 整段 session 应保留
	seedMsg(t, db, "session-D", "user-1", "user", timezone.Now().Add(-30*time.Hour))
	seedMsg(t, db, "session-D", "user-1", "assistant", timezone.Now().Add(-1*time.Hour))

	runOnce(db)

	if got := countMsgs(t, db, "session-D"); got != 2 {
		t.Errorf("session-D still active (last msg 1h ago), should keep both rows, got %d", got)
	}
}

func TestRunOnce_multipleSessionsIsolated(t *testing.T) {
	db, skip := newCleanupTestDB(t)
	if skip {
		return
	}
	seedMsg(t, db, "session-old", "user-1", "user", timezone.Now().Add(-48*time.Hour))
	seedMsg(t, db, "session-old", "user-1", "assistant", timezone.Now().Add(-40*time.Hour))
	seedMsg(t, db, "session-new", "user-1", "user", timezone.Now().Add(-30*time.Minute))
	seedMsg(t, db, "session-new", "user-1", "assistant", timezone.Now().Add(-10*time.Minute))

	runOnce(db)

	if got := countMsgs(t, db, "session-old"); got != 0 {
		t.Errorf("session-old should be cleaned, got %d", got)
	}
	if got := countMsgs(t, db, "session-new"); got != 2 {
		t.Errorf("session-new should be untouched, got %d", got)
	}
}

func TestRunOnce_expiredWorkspaceMessagesPreserved(t *testing.T) {
	db, skip := newCleanupTestDB(t)
	if skip {
		return
	}

	seedMsgInSpace(t, db, "space-1", "workspace-old", "user-1", "user", timezone.Now().Add(-30*time.Hour))
	seedMsgInSpace(t, db, "space-1", "workspace-old", "user-1", "assistant", timezone.Now().Add(-25*time.Hour))

	runOnce(db)

	if got := countMsgsInSpace(t, db, "space-1", "user-1", "workspace-old"); got != 2 {
		t.Errorf("expired workspace messages must survive Legacy cleanup, got %d rows (want 2)", got)
	}
}

func TestRunOnce_legacyAndWorkspaceMessagesSameTupleIsolated(t *testing.T) {
	db, skip := newCleanupTestDB(t)
	if skip {
		return
	}

	// A fresh workspace message must neither be deleted nor keep the stale
	// Legacy tuple alive when both reuse the same owner/session identifiers.
	seedMsg(t, db, "shared-session", "user-1", "user", timezone.Now().Add(-30*time.Hour))
	seedMsgInSpace(t, db, "space-1", "shared-session", "user-1", "assistant", timezone.Now().Add(-1*time.Hour))

	runOnce(db)

	if got := countMsgsInSpace(t, db, "", "user-1", "shared-session"); got != 0 {
		t.Errorf("expired Legacy messages should be cleaned independently, got %d rows", got)
	}
	if got := countMsgsInSpace(t, db, "space-1", "user-1", "shared-session"); got != 1 {
		t.Errorf("fresh workspace message must survive Legacy cleanup, got %d rows", got)
	}
}

func TestRunOnce_emptyTable(t *testing.T) {
	db, skip := newCleanupTestDB(t)
	if skip {
		return
	}
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
	if skip {
		return
	}

	// Two users share the same session_id literal, both idle > 24h.
	seedMsg(t, db, "sess-shared", "user-alice", "user", timezone.Now().Add(-30*time.Hour))
	seedMsg(t, db, "sess-shared", "user-alice", "assistant", timezone.Now().Add(-25*time.Hour))
	seedMsg(t, db, "sess-shared", "user-bob", "user", timezone.Now().Add(-40*time.Hour))
	seedMsg(t, db, "sess-shared", "user-bob", "assistant", timezone.Now().Add(-26*time.Hour))

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
	if skip {
		return
	}

	// Alice: idle > 24h → must be cleaned.
	seedMsg(t, db, "sess-shared", "user-alice", "user", timezone.Now().Add(-30*time.Hour))
	seedMsg(t, db, "sess-shared", "user-alice", "assistant", timezone.Now().Add(-25*time.Hour))
	// Bob: last message 1h ago → must be untouched.
	seedMsg(t, db, "sess-shared", "user-bob", "user", timezone.Now().Add(-2*time.Hour))
	seedMsg(t, db, "sess-shared", "user-bob", "assistant", timezone.Now().Add(-1*time.Hour))

	runOnce(db)

	if got := countMsgsFor(t, db, "user-alice", "sess-shared"); got != 0 {
		t.Errorf("(alice, sess-shared) idle > 24h, expected 0 rows, got %d", got)
	}
	if got := countMsgsFor(t, db, "user-bob", "sess-shared"); got != 2 {
		t.Errorf("(bob, sess-shared) still active, expected 2 rows preserved, got %d", got)
	}
}

// seedEvidence writes an agent_message_evidence row, mirroring what
// PersistEvidence does in production. Used by the #161 P2 (yujiawei)
// symmetric-cleanup regression tests below.
func seedEvidence(t *testing.T, db *gorm.DB, userID, sessionID, handle string, createdAt time.Time) {
	t.Helper()
	if err := db.Create(&model.AgentMessageEvidence{
		UserID:    userID,
		SessionID: sessionID,
		Handle:    handle,
		Evidence:  "[]", // JSON-empty; content shape is irrelevant to cleanup
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}).Error; err != nil {
		t.Fatalf("seed evidence (user=%s session=%s handle=%s): %v", userID, sessionID, handle, err)
	}
}

func countEvidence(t *testing.T, db *gorm.DB, userID, sessionID string) int64 {
	t.Helper()
	var n int64
	if err := db.Model(&model.AgentMessageEvidence{}).
		Where("user_id = ? AND session_id = ?", userID, sessionID).
		Count(&n).Error; err != nil {
		t.Fatalf("count evidence for (user=%s session=%s): %v", userID, sessionID, err)
	}
	return n
}

func seedWorkspaceSession(t *testing.T, db *gorm.DB, spaceID, userID, sessionID string) {
	t.Helper()
	now := timezone.Now()
	pendingProposalJSON := "{}"
	if err := db.Create(&model.AgentSummarySession{
		SpaceID:             spaceID,
		UserID:              userID,
		SessionID:           sessionID,
		AgentSessionID:      summaryWorkspaceAgentSessionID(spaceID, sessionID, 1),
		ContractVersion:     "1",
		State:               "idle",
		StateVersion:        1,
		ScopeVersion:        1,
		ScopeJSON:           "{}",
		PendingProposalJSON: &pendingProposalJSON,
		CreatedAt:           now,
		UpdatedAt:           now,
	}).Error; err != nil {
		t.Fatalf("seed workspace session (space=%s user=%s session=%s): %v", spaceID, userID, sessionID, err)
	}
}

// TestRunOnce_expiredEvidenceCleaned is the #161 P2 (yujiawei) regression:
// agent_message_evidence must be cleaned symmetrically with agent_message so
// stale evidence rows don't accumulate indefinitely and inflate the citation
// pool of every subsequent summarize_chunk. Without symmetric cleanup,
// evidence rows written 30+ days ago for a reused session_id would still be
// pulled into today's pool by getSessionMessagePool / buildCitationsForSession.
func TestRunOnce_expiredEvidenceCleaned(t *testing.T) {
	db, skip := newCleanupTestDB(t)
	if skip {
		return
	}
	// evidence-A: 30h old → expired → should be cleaned
	seedEvidence(t, db, "user-1", "session-A", "msg_u1_1", timezone.Now().Add(-30*time.Hour))

	runOnce(db)

	if got := countEvidence(t, db, "user-1", "session-A"); got != 0 {
		t.Errorf("expired evidence-A should be cleaned, got %d rows", got)
	}
}

// TestRunOnce_freshEvidenceUntouched asserts the cutoff is honored — recent
// evidence (< 24h) must NOT be cleaned even if the message table for the
// same session has already been expired.
func TestRunOnce_freshEvidenceUntouched(t *testing.T) {
	db, skip := newCleanupTestDB(t)
	if skip {
		return
	}
	// evidence-B: 1h old → fresh → keep
	seedEvidence(t, db, "user-1", "session-B", "msg_u1_2", timezone.Now().Add(-1*time.Hour))

	runOnce(db)

	if got := countEvidence(t, db, "user-1", "session-B"); got != 1 {
		t.Errorf("fresh evidence-B should NOT be cleaned, got %d rows", got)
	}
}

func TestRunOnce_expiredEvidenceReferencedByWorkspacePreserved(t *testing.T) {
	db, skip := newCleanupTestDB(t)
	if skip {
		return
	}

	seedEvidence(t, db, "user-1", summaryWorkspaceAgentSessionID("space-1", "workspace-session", 1), "msg_u1_3", timezone.Now().Add(-30*time.Hour))
	seedWorkspaceSession(t, db, "space-1", "user-1", "workspace-session")

	runOnce(db)

	if got := countEvidence(t, db, "user-1", summaryWorkspaceAgentSessionID("space-1", "workspace-session", 1)); got != 1 {
		t.Errorf("workspace-referenced evidence must survive Legacy cleanup, got %d rows", got)
	}
}

func TestRunOnce_workspaceEvidenceProtectionScopedByOwner(t *testing.T) {
	db, skip := newCleanupTestDB(t)
	if skip {
		return
	}

	internalSessionID := summaryWorkspaceAgentSessionID("space-1", "shared-session", 1)
	seedEvidence(t, db, "user-1", internalSessionID, "msg_u1_4", timezone.Now().Add(-30*time.Hour))
	seedEvidence(t, db, "user-2", internalSessionID, "msg_u2_2", timezone.Now().Add(-30*time.Hour))
	seedWorkspaceSession(t, db, "space-1", "user-1", "shared-session")

	runOnce(db)

	if got := countEvidence(t, db, "user-1", internalSessionID); got != 1 {
		t.Errorf("matching owner's workspace evidence must survive, got %d rows", got)
	}
	if got := countEvidence(t, db, "user-2", internalSessionID); got != 0 {
		t.Errorf("another user's unreferenced expired evidence should be cleaned, got %d rows", got)
	}
}

func TestRunOnce_expiredWorkspaceSessionAndEvidenceCleaned(t *testing.T) {
	db, skip := newCleanupTestDB(t)
	if skip {
		return
	}

	seedWorkspaceSession(t, db, "space-1", "user-1", "workspace-expired")
	var session model.AgentSummarySession
	if err := db.Where("space_id = ? AND user_id = ? AND session_id = ?", "space-1", "user-1", "workspace-expired").Take(&session).Error; err != nil {
		t.Fatalf("load workspace session: %v", err)
	}
	now := timezone.Now()
	expiredAt := now.Add(-time.Hour)
	if err := db.Model(&session).Updates(map[string]interface{}{"expires_at": expiredAt, "updated_at": now.Add(-summaryWorkspaceRetention - time.Hour)}).Error; err != nil {
		t.Fatalf("expire workspace session: %v", err)
	}
	seedMsgInSpace(t, db, "space-1", "workspace-expired", "user-1", "assistant", now.Add(-48*time.Hour))
	seedEvidence(t, db, "user-1", session.AgentSessionID, "msg_workspace_expired", now.Add(-48*time.Hour))
	if err := db.Create(&model.AgentSummaryTurn{
		SpaceID: "space-1", UserID: "user-1", SessionID: "workspace-expired", RequestID: "req-1",
		RequestHash: "hash", ScopeVersion: 1, Status: "completed", CreatedAt: expiredAt, UpdatedAt: expiredAt,
	}).Error; err != nil {
		t.Fatalf("seed workspace turn: %v", err)
	}

	runOnce(db)

	for name, value := range map[string]int64{
		"sessions": countModelRows(t, db, &model.AgentSummarySession{}, "session_id = ?", "workspace-expired"),
		"turns":    countModelRows(t, db, &model.AgentSummaryTurn{}, "session_id = ?", "workspace-expired"),
		"messages": countMsgsInSpace(t, db, "space-1", "user-1", "workspace-expired"),
		"evidence": countEvidence(t, db, "user-1", session.AgentSessionID),
	} {
		if value != 0 {
			t.Errorf("expired workspace %s = %d, want 0", name, value)
		}
	}
}

func TestRunOnce_expiredWorkspaceCleansFailedTurnRunWithoutMessages(t *testing.T) {
	db, skip := newCleanupTestDB(t)
	if skip {
		return
	}

	const (
		spaceID   = "space-1"
		userID    = "user-1"
		sessionID = "workspace-failed-run"
		runID     = "run-failed-without-message"
	)
	seedWorkspaceSession(t, db, spaceID, userID, sessionID)
	var session model.AgentSummarySession
	if err := db.Where("space_id = ? AND user_id = ? AND session_id = ?", spaceID, userID, sessionID).Take(&session).Error; err != nil {
		t.Fatalf("load workspace session: %v", err)
	}
	now := timezone.Now()
	expiredAt := now.Add(-time.Hour)
	if err := db.Model(&session).Updates(map[string]interface{}{"expires_at": expiredAt, "updated_at": now.Add(-summaryWorkspaceRetention - time.Hour)}).Error; err != nil {
		t.Fatalf("expire workspace session: %v", err)
	}
	if err := db.Create(&model.AgentSummaryTurn{
		SpaceID: spaceID, UserID: userID, SessionID: sessionID, RequestID: "failed-request",
		RequestHash: "failed-hash", ScopeVersion: 2, Status: "failed", Attempt: 1,
		ErrorCode: "AGENT_FAILED", CreatedAt: expiredAt, UpdatedAt: expiredAt,
	}).Error; err != nil {
		t.Fatalf("seed failed workspace turn: %v", err)
	}
	internalSessionID := summaryWorkspaceReplacementAgentSessionID(spaceID, sessionID, 2, "failed-request")
	if err := db.Create(&model.AgentSummaryRun{
		RunID: runID, UserID: userID, SessionID: internalSessionID, RequestID: "failed-request",
		ScopePolicy: model.ScopePolicyOpen, Status: "created", AttemptedChannels: "[]", SucceededChannels: "[]", FailedChannels: "[]", DiscoveredChannels: "[]",
		CreatedAt: expiredAt, UpdatedAt: expiredAt,
	}).Error; err != nil {
		t.Fatalf("seed failed workspace run: %v", err)
	}
	if err := db.Create(&model.AgentSummarySpec{
		SpecID: "spec-failed", RunID: runID, Version: 1, SpecHash: "hash",
		SpecJSON: "{}", FieldSources: "{}", UserRequest: "sensitive failed request", CreatedAt: expiredAt,
	}).Error; err != nil {
		t.Fatalf("seed failed workspace spec: %v", err)
	}

	runOnce(db)

	if got := countModelRows(t, db, &model.AgentSummaryRun{}, "run_id = ?", runID); got != 0 {
		t.Fatalf("failed workspace runs = %d, want 0", got)
	}
	if got := countModelRows(t, db, &model.AgentSummarySpec{}, "run_id = ?", runID); got != 0 {
		t.Fatalf("failed workspace specs = %d, want 0", got)
	}
}

func TestRunOnce_activeWorkspaceSessionPreserved(t *testing.T) {
	db, skip := newCleanupTestDB(t)
	if skip {
		return
	}

	seedWorkspaceSession(t, db, "space-1", "user-1", "workspace-active")
	var session model.AgentSummarySession
	if err := db.Where("space_id = ? AND user_id = ? AND session_id = ?", "space-1", "user-1", "workspace-active").Take(&session).Error; err != nil {
		t.Fatalf("load workspace session: %v", err)
	}
	now := timezone.Now()
	future := now.Add(time.Hour)
	if err := db.Model(&session).Update("expires_at", future).Error; err != nil {
		t.Fatalf("extend workspace session: %v", err)
	}
	seedMsgInSpace(t, db, "space-1", "workspace-active", "user-1", "assistant", now.Add(-48*time.Hour))
	seedEvidence(t, db, "user-1", session.AgentSessionID, "msg_workspace_active", now.Add(-48*time.Hour))

	runOnce(db)

	if got := countModelRows(t, db, &model.AgentSummarySession{}, "session_id = ?", "workspace-active"); got != 1 {
		t.Errorf("active workspace sessions = %d, want 1", got)
	}
	if got := countMsgsInSpace(t, db, "space-1", "user-1", "workspace-active"); got != 1 {
		t.Errorf("active workspace messages = %d, want 1", got)
	}
	if got := countEvidence(t, db, "user-1", session.AgentSessionID); got != 1 {
		t.Errorf("active workspace evidence = %d, want 1", got)
	}
}

func TestRunOnce_activeWorkspaceSessionPreservesEvidenceAcrossIdentityRotation(t *testing.T) {
	db, skip := newCleanupTestDB(t)
	if skip {
		return
	}

	const (
		spaceID   = "space-1"
		userID    = "user-1"
		sessionID = "workspace-rotated"
		requestID = "replace-request"
	)
	seedWorkspaceSession(t, db, spaceID, userID, sessionID)
	baseID := summaryWorkspaceAgentSessionID(spaceID, sessionID, 1)
	replacementID := summaryWorkspaceReplacementAgentSessionID(spaceID, sessionID, 1, requestID)
	now := timezone.Now()
	future := now.Add(time.Hour)
	if err := db.Model(&model.AgentSummarySession{}).
		Where("space_id = ? AND user_id = ? AND session_id = ?", spaceID, userID, sessionID).
		Updates(map[string]interface{}{"agent_session_id": replacementID, "expires_at": future}).Error; err != nil {
		t.Fatalf("rotate workspace identity: %v", err)
	}
	runs := []model.AgentSummaryRun{
		{RunID: "run-base", UserID: userID, SessionID: baseID, RequestID: "base-request", ScopePolicy: model.ScopePolicyClosed, Status: "completed", AttemptedChannels: "[]", SucceededChannels: "[]", FailedChannels: "[]", DiscoveredChannels: "[]", CreatedAt: now, UpdatedAt: now},
		{RunID: "run-replacement", UserID: userID, SessionID: replacementID, RequestID: requestID, ScopePolicy: model.ScopePolicyOpen, Status: "completed", AttemptedChannels: "[]", SucceededChannels: "[]", FailedChannels: "[]", DiscoveredChannels: "[]", CreatedAt: now, UpdatedAt: now},
	}
	if err := db.Create(&runs).Error; err != nil {
		t.Fatalf("seed workspace runs: %v", err)
	}
	turns := []model.AgentSummaryTurn{
		{SpaceID: spaceID, UserID: userID, SessionID: sessionID, RequestID: "base-request", RequestHash: "base-hash", ScopeVersion: 1, Status: "completed", Attempt: 1, RunID: "run-base", CreatedAt: now, UpdatedAt: now},
		{SpaceID: spaceID, UserID: userID, SessionID: sessionID, RequestID: requestID, RequestHash: "replace-hash", ScopeVersion: 1, Status: "completed", Attempt: 1, RunID: "run-replacement", CreatedAt: now, UpdatedAt: now},
	}
	if err := db.Create(&turns).Error; err != nil {
		t.Fatalf("seed workspace turns: %v", err)
	}
	seedEvidence(t, db, userID, baseID, "base-evidence", now.Add(-48*time.Hour))
	seedEvidence(t, db, userID, replacementID, "replacement-evidence", now.Add(-48*time.Hour))

	runOnce(db)

	if got := countEvidence(t, db, userID, baseID); got != 1 {
		t.Fatalf("base evidence rows = %d, want 1", got)
	}
	if got := countEvidence(t, db, userID, replacementID); got != 1 {
		t.Fatalf("replacement evidence rows = %d, want 1", got)
	}
}

func TestDeleteExpiredWorkspaceSessionState_RechecksRenewalAfterScan(t *testing.T) {
	db, skip := newCleanupTestDB(t)
	if skip {
		return
	}

	now := timezone.Now()
	seedWorkspaceSession(t, db, "space-1", "user-1", "workspace-renewed")
	var session model.AgentSummarySession
	if err := db.Where("space_id = ? AND user_id = ? AND session_id = ?", "space-1", "user-1", "workspace-renewed").Take(&session).Error; err != nil {
		t.Fatalf("load workspace session: %v", err)
	}
	if err := db.Model(&session).Updates(map[string]interface{}{
		"expires_at": now.Add(-time.Minute),
		"updated_at": now.Add(-summaryWorkspaceRetention - time.Hour),
	}).Error; err != nil {
		t.Fatalf("expire workspace session: %v", err)
	}

	// Simulate BeginTurn renewing the row after the cleanup scan captured its ID
	// but before the deletion transaction locks and re-checks it.
	if err := db.Model(&session).Updates(map[string]interface{}{
		"expires_at": now.Add(time.Hour),
		"updated_at": now,
	}).Error; err != nil {
		t.Fatalf("renew workspace session: %v", err)
	}
	deleted, err := deleteExpiredWorkspaceSessionState(db, session.ID, now, now.Add(-summaryWorkspaceRetention))
	if err != nil {
		t.Fatalf("delete expired workspace session: %v", err)
	}
	if deleted {
		t.Fatal("renewed workspace session must not be deleted")
	}
	if got := countModelRows(t, db, &model.AgentSummarySession{}, "id = ?", session.ID); got != 1 {
		t.Fatalf("renewed workspace sessions = %d, want 1", got)
	}
}

func TestDeleteExpiredWorkspaceSessionState_PreservesLiveTurnLease(t *testing.T) {
	db, skip := newCleanupTestDB(t)
	if skip {
		return
	}

	now := timezone.Now()
	seedWorkspaceSession(t, db, "space-1", "user-1", "workspace-live-turn")
	var session model.AgentSummarySession
	if err := db.Where("space_id = ? AND user_id = ? AND session_id = ?", "space-1", "user-1", "workspace-live-turn").Take(&session).Error; err != nil {
		t.Fatalf("load workspace session: %v", err)
	}
	leaseUntil := now.Add(time.Minute)
	turn := model.AgentSummaryTurn{
		SpaceID: "space-1", UserID: "user-1", SessionID: "workspace-live-turn", RequestID: "request-live-turn",
		RequestHash: "hash", ScopeVersion: 1, Status: "running", Attempt: 1, LeaseExpiresAt: &leaseUntil,
		CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute),
	}
	if err := db.Create(&turn).Error; err != nil {
		t.Fatalf("seed live workspace turn: %v", err)
	}
	if err := db.Model(&session).Updates(map[string]interface{}{
		"active_turn_id": turn.ID,
		"expires_at":     now.Add(-time.Minute),
		"updated_at":     now.Add(-summaryWorkspaceRetention - time.Hour),
	}).Error; err != nil {
		t.Fatalf("expire workspace session with live turn: %v", err)
	}

	deleted, err := deleteExpiredWorkspaceSessionState(db, session.ID, now, now.Add(-summaryWorkspaceRetention))
	if err != nil {
		t.Fatalf("delete expired workspace session: %v", err)
	}
	if deleted {
		t.Fatal("workspace session with a live turn lease must not be deleted")
	}
	if got := countModelRows(t, db, &model.AgentSummarySession{}, "id = ?", session.ID); got != 1 {
		t.Fatalf("workspace sessions with live turn = %d, want 1", got)
	}
}

func countModelRows(t *testing.T, db *gorm.DB, modelValue interface{}, query string, args ...interface{}) int64 {
	t.Helper()
	var count int64
	if err := db.Model(modelValue).Where(query, args...).Count(&count).Error; err != nil {
		t.Fatalf("count model rows: %v", err)
	}
	return count
}

// TestRunOnce_evidenceOwnerScoped verifies the (user_id, session_id) predicate
// is honored on evidence cleanup too — two different users sharing the same
// literal session_id string must be cleaned independently.
func TestRunOnce_evidenceOwnerScoped(t *testing.T) {
	db, skip := newCleanupTestDB(t)
	if skip {
		return
	}
	// user-1's evidence expired, user-2's still fresh; same session_id literal
	seedEvidence(t, db, "user-1", "shared-session", "msg_u1_3", timezone.Now().Add(-30*time.Hour))
	seedEvidence(t, db, "user-2", "shared-session", "msg_u2_1", timezone.Now().Add(-1*time.Hour))

	runOnce(db)

	if got := countEvidence(t, db, "user-1", "shared-session"); got != 0 {
		t.Errorf("user-1's expired evidence should be cleaned, got %d rows", got)
	}
	if got := countEvidence(t, db, "user-2", "shared-session"); got != 1 {
		t.Errorf("user-2's fresh evidence must survive, got %d rows (cross-user clobber?)", got)
	}
}
