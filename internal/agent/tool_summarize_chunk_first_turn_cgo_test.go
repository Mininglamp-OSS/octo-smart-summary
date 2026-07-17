//go:build cgo
// +build cgo

package agent

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestGetSessionMessagePool_FirstTurn_NoAgentMessageRowsYet reproduces the
// 4-reviewer P1 regression from PR #158 (Jerry-Xin / yujiawei / lml2468 /
// mochashanyao all independently byte-verified):
//
// On the first turn of a fresh session, `agent_message` has ZERO rows for
// role='tool' — those are only written by `AppendMessages`, which runs
// AFTER `RunWithHistory` returns. When `getSessionMessagePool` is called
// mid-run by `summarize_chunk`, it must NOT depend on those not-yet-persisted
// rows; otherwise every message gets CitationIndex=0 (Go zero value), the
// LLM emits `[0]` markers, and `worker.BuildCitations` (which filters
// idx>=1) discards every citation at save time.
//
// This test seeds `agent_message_evidence` (which IS populated synchronously
// during the run by PersistEvidence inside fetch_channel / peek_channel /
// search_messages / filter_relevant) WITHOUT any matching `agent_message`
// tool rows, mirroring the exact first-turn state. It asserts every message
// gets CitationIndex >= 1.
func TestGetSessionMessagePool_FirstTurn_NoAgentMessageRowsYet(t *testing.T) {
	db := newFirstTurnTestDB(t)
	if db == nil {
		return
	}

	const uid = "test-user"
	const sessionID = "sess-first-turn"
	const handle = "msg_testuse_1"

	// The messages the tool "just fetched" — cached in memory during the run
	messages := []pipeline.Message{
		{ChannelID: "ch1", MessageSeq: 100, Timestamp: 1_000_000, Content: "hello"},
		{ChannelID: "ch1", MessageSeq: 101, Timestamp: 1_000_100, Content: "world"},
		{ChannelID: "ch1", MessageSeq: 102, Timestamp: 1_000_200, Content: "!!!"},
	}

	// PersistEvidence would have written this row synchronously before the
	// tool returned. Simulate that exact state.
	if err := seedEvidence(db, uid, sessionID, handle, messages); err != nil {
		t.Fatalf("seed evidence: %v", err)
	}

	// CRITICAL: do NOT seed any agent_message tool row. That's the whole
	// point — first-turn state means those rows don't exist yet.

	// Wire up deps (like the real handler chain would)
	SetSummaryDeps(db, nil, nil, config.Config{})
	t.Cleanup(func() { SetSummaryDeps(nil, nil, nil, config.Config{}) })

	pool, err := getSessionMessagePool(sessionID, uid)
	if err != nil {
		t.Fatalf("getSessionMessagePool: %v", err)
	}
	if len(pool) != len(messages) {
		t.Fatalf("pool size = %d, want %d — first-turn discovery via evidence failed", len(pool), len(messages))
	}
	for i, m := range pool {
		if m.CitationIndex < 1 {
			t.Errorf("message %d got CitationIndex=%d, want >= 1 (the whole regression is CitationIndex=0)", i, m.CitationIndex)
		}
	}
	// CitationIndex must be dense 1..N for BuildCitations to accept them
	for i, m := range pool {
		if m.CitationIndex != i+1 {
			t.Errorf("pool[%d] CitationIndex = %d, want %d (must be dense 1..N in timestamp order)", i, m.CitationIndex, i+1)
		}
	}
}

// TestGetSessionMessagePool_CacheHotPath verifies the cache-preferred path
// still returns the cached messages when the cache is warm (avoids the
// JSON unmarshal cost on the hot path).
func TestGetSessionMessagePool_CacheHotPath(t *testing.T) {
	db := newFirstTurnTestDB(t)
	if db == nil {
		return
	}

	const uid = "test-user"
	const sessionID = "sess-cache-hot"
	const handle = "msg_testuse_2"

	messages := []pipeline.Message{
		{ChannelID: "ch2", MessageSeq: 200, Timestamp: 2_000_000, Content: "cached"},
	}

	// Cache-warm: both evidence AND cache have it
	if err := seedEvidence(db, uid, sessionID, handle, messages); err != nil {
		t.Fatalf("seed evidence: %v", err)
	}
	messageCache.Store(messages, uid)
	// NB: messageCache.Store returns a fresh handle; overwrite the entry at
	// our known handle for the test predicate.
	messageCache.mu.Lock()
	messageCache.store[handle] = cacheEntry{messages: messages, uid: uid, createdAt: time.Now()}
	messageCache.mu.Unlock()

	SetSummaryDeps(db, nil, nil, config.Config{})
	t.Cleanup(func() {
		SetSummaryDeps(nil, nil, nil, config.Config{})
		messageCache.mu.Lock()
		delete(messageCache.store, handle)
		messageCache.mu.Unlock()
	})

	pool, err := getSessionMessagePool(sessionID, uid)
	if err != nil {
		t.Fatalf("getSessionMessagePool: %v", err)
	}
	if len(pool) != 1 {
		t.Fatalf("pool size = %d, want 1", len(pool))
	}
	if pool[0].CitationIndex != 1 {
		t.Errorf("cache-hot path CitationIndex = %d, want 1", pool[0].CitationIndex)
	}
	if pool[0].Content != "cached" {
		t.Errorf("cache-hot path content = %q, want %q", pool[0].Content, "cached")
	}
}

// TestGetSessionMessagePool_CacheMissEvidenceFallback verifies the JSON
// unmarshal fallback fires when the cache is cold. Mirrors the long-running
// session case (>30 min between fetch and summarize_chunk).
func TestGetSessionMessagePool_CacheMissEvidenceFallback(t *testing.T) {
	db := newFirstTurnTestDB(t)
	if db == nil {
		return
	}

	const uid = "test-user"
	const sessionID = "sess-cold-cache"
	const handle = "msg_testuse_3"

	messages := []pipeline.Message{
		{ChannelID: "ch3", MessageSeq: 300, Timestamp: 3_000_000, Content: "recovered from db"},
	}

	// Evidence only, NO cache entry — simulates cold cache
	if err := seedEvidence(db, uid, sessionID, handle, messages); err != nil {
		t.Fatalf("seed evidence: %v", err)
	}

	SetSummaryDeps(db, nil, nil, config.Config{})
	t.Cleanup(func() { SetSummaryDeps(nil, nil, nil, config.Config{}) })

	pool, err := getSessionMessagePool(sessionID, uid)
	if err != nil {
		t.Fatalf("getSessionMessagePool: %v", err)
	}
	if len(pool) != 1 {
		t.Fatalf("cold-cache pool size = %d, want 1 (evidence fallback failed)", len(pool))
	}
	if pool[0].Content != "recovered from db" {
		t.Errorf("cold-cache content = %q, want %q", pool[0].Content, "recovered from db")
	}
	if pool[0].CitationIndex != 1 {
		t.Errorf("cold-cache CitationIndex = %d, want 1", pool[0].CitationIndex)
	}
}

// TestGetSessionMessagePool_OwnerScoping verifies handles from other users'
// sessions are never mixed into the pool.
func TestGetSessionMessagePool_OwnerScoping(t *testing.T) {
	db := newFirstTurnTestDB(t)
	if db == nil {
		return
	}

	const sessionID = "sess-shared-id-across-users"

	// User A's evidence
	if err := seedEvidence(db, "user-a", sessionID, "msg_usera_1",
		[]pipeline.Message{{ChannelID: "ch", MessageSeq: 1, Timestamp: 1_000, Content: "a"}}); err != nil {
		t.Fatalf("seed a: %v", err)
	}
	// User B's evidence at the same session_id literal (permitted by design)
	if err := seedEvidence(db, "user-b", sessionID, "msg_userb_1",
		[]pipeline.Message{{ChannelID: "ch", MessageSeq: 2, Timestamp: 2_000, Content: "b"}}); err != nil {
		t.Fatalf("seed b: %v", err)
	}

	SetSummaryDeps(db, nil, nil, config.Config{})
	t.Cleanup(func() { SetSummaryDeps(nil, nil, nil, config.Config{}) })

	poolA, err := getSessionMessagePool(sessionID, "user-a")
	if err != nil {
		t.Fatalf("pool A: %v", err)
	}
	if len(poolA) != 1 || poolA[0].Content != "a" {
		t.Errorf("user-a saw pool = %+v, want only own message %q", poolA, "a")
	}

	poolB, err := getSessionMessagePool(sessionID, "user-b")
	if err != nil {
		t.Fatalf("pool B: %v", err)
	}
	if len(poolB) != 1 || poolB[0].Content != "b" {
		t.Errorf("user-b saw pool = %+v, want only own message %q", poolB, "b")
	}
}

// --- helpers ---

func newFirstTurnTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Skipf("CGO required for sqlite: %v", err)
		return nil
	}
	if err := db.AutoMigrate(&model.AgentMessage{}, &model.AgentMessageEvidence{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func seedEvidence(db *gorm.DB, uid, sessionID, handle string, messages []pipeline.Message) error {
	blob, err := json.Marshal(messages)
	if err != nil {
		return err
	}
	now := time.Now()
	return db.Create(&model.AgentMessageEvidence{
		UserID:    uid,
		SessionID: sessionID,
		Handle:    handle,
		Evidence:  string(blob),
		CreatedAt: now,
		UpdatedAt: now,
	}).Error
}
