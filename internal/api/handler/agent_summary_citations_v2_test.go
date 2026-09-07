package handler

// Package handler — PR #197 handler-side regression tests:
// the save-time citation path may adopt frozen manifest ordinals ONLY when
// the request carries the request_id of a run that actually froze a manifest.
// Any miss — no request_id, unknown request_id, V2 off — must fall back to
// the byte-identical legacy recompute, never a partial override.

import (
	"context"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/artifact"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// setupV2TestDB is setupTestDB plus the Stage-2 tables the override path
// reads (agent_summary_run, agent_evidence_artifact, agent_citation_manifest).
func setupV2TestDB(t *testing.T) (*gorm.DB, bool) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Skipf("CGO required for sqlite: %v", err)
		return nil, true
	}
	if err := db.AutoMigrate(
		&model.AgentMessage{},
		&model.AgentMessageEvidence{},
		&model.AgentSummaryRun{},
		&model.AgentEvidenceArtifact{},
		&model.AgentCitationManifest{},
	); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}
	return db, false
}

// seedV2Scenario builds the shared fixture:
//   - evidence pool of THREE messages (Alice, Bob, Charlie) discovered at
//     save time via agent_message_evidence;
//   - a run for request_id "req-1" whose frozen manifest covers only Alice
//     and Bob (Charlie arrived after the freeze);
//
// so the two paths diverge: legacy recompute numbers Charlie [3], the frozen
// path drops Charlie entirely.
func seedV2Scenario(t *testing.T, db *gorm.DB) {
	t.Helper()
	agent.ResetForTest()
	cache := agent.GetMessageCache()

	alice := pipeline.Message{ChannelID: "channel-1", MessageSeq: 100, SenderUID: "user-1", SenderName: "Alice", Content: "Hello", Timestamp: 1704103200}
	bob := pipeline.Message{ChannelID: "channel-1", MessageSeq: 101, SenderUID: "user-2", SenderName: "Bob", Content: "Hi there", Timestamp: 1704103300}
	charlie := pipeline.Message{ChannelID: "channel-1", MessageSeq: 102, SenderUID: "user-3", SenderName: "Charlie", Content: "Late arrival", Timestamp: 1704103400}

	// Save-time discovery sees all three.
	all := []pipeline.Message{alice, bob, charlie}
	handle := cache.Store(all, "test-user", "session-1")
	seedEvidenceRow(t, db, "test-user", "session-1", handle, all)

	// The run for this request froze BEFORE Charlie arrived: 2-message pool,
	// ordinals Alice=[1] Bob=[2].
	run := model.AgentSummaryRun{
		RunID:       "run-v2-1",
		UserID:      "test-user",
		SessionID:   "session-1",
		RequestID:   "req-1",
		Status:      model.RunStatusFinished,
		ScopePolicy: model.ScopePolicyClosed,
	}
	if err := db.Create(&run).Error; err != nil {
		t.Fatalf("seed run: %v", err)
	}
	frozenPool := []pipeline.Message{alice, bob}
	if _, _, _, err := artifact.NewStore(db).FreezeFromPool(context.Background(), run.RunID, "test-user", "session-1", frozenPool, artifact.FreezeMeta{}); err != nil {
		t.Fatalf("seed freeze: %v", err)
	}
}

// V2 on + matching request_id → frozen ordinals adopted; the post-freeze
// message (Charlie) is NOT citable and must not leak under a recomputed [3].
func TestBuildCitationsForSession_V2On_RequestIDAdoptsFrozenOrdinals(t *testing.T) {
	db, skip := setupV2TestDB(t)
	if skip {
		return
	}
	seedV2Scenario(t, db)
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")
	h := &AgentSummaryHandler{db: db}

	// Frozen ordinals resolve: Alice [1], Bob [2].
	cits, err := h.buildCitationsForSession(context.Background(), "session-1", "Alice said [1] and Bob [2]", "test-user", "req-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cits) != 2 {
		t.Fatalf("expected 2 citations from frozen ordinals, got %d", len(cits))
	}
	if cits[0].Sender != "Alice" || cits[1].Sender != "Bob" {
		t.Errorf("frozen ordinals mismatch: got %q / %q", cits[0].Sender, cits[1].Sender)
	}

	// Charlie was fetched after the freeze: [3] must resolve to NOTHING
	// (dropped from the citable set), not to a recomputed ordinal.
	cits2, err := h.buildCitationsForSession(context.Background(), "session-1", "Charlie said [3]", "test-user", "req-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cits2) != 0 {
		t.Errorf("post-freeze message must not be citable, got %d citations", len(cits2))
	}
}

// V2 on + NO request_id (any legacy client) → must keep the recomputed
// indexes: a turn-2 legacy save must not silently adopt turn-1's frozen
// manifest.
func TestBuildCitationsForSession_V2On_NoRequestIDKeepsRecompute(t *testing.T) {
	db, skip := setupV2TestDB(t)
	if skip {
		return
	}
	seedV2Scenario(t, db)
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")
	h := &AgentSummaryHandler{db: db}

	// Legacy recompute numbers Charlie [3]; the frozen path would drop him.
	cits, err := h.buildCitationsForSession(context.Background(), "session-1", "Charlie said [3]", "test-user", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cits) != 1 {
		t.Fatalf("no-request_id save must use recomputed indexes: got %d citations, want 1", len(cits))
	}
	if cits[0].Sender != "Charlie" {
		t.Errorf("expected recomputed [3]=Charlie, got %q", cits[0].Sender)
	}
}

// V2 on + UNKNOWN request_id (run never existed — replay, or V2 was off at
// chat time) → same fallback as no request_id: byte-identical recompute.
func TestBuildCitationsForSession_V2On_UnknownRequestIDFallsBack(t *testing.T) {
	db, skip := setupV2TestDB(t)
	if skip {
		return
	}
	seedV2Scenario(t, db)
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")
	h := &AgentSummaryHandler{db: db}

	cits, err := h.buildCitationsForSession(context.Background(), "session-1", "Charlie said [3]", "test-user", "req-does-not-exist")
	if err != nil {
		t.Fatalf("unknown request_id must not error: %v", err)
	}
	if len(cits) != 1 || cits[0].Sender != "Charlie" {
		t.Errorf("unknown request_id must fall back to recompute, got %d citations", len(cits))
	}
}

// V2 off + request_id provided → the gate stays closed; the run/manifest
// tables are never consulted and the answer is the legacy recompute.
func TestBuildCitationsForSession_V2Off_RequestIDIgnored(t *testing.T) {
	db, skip := setupV2TestDB(t)
	if skip {
		return
	}
	seedV2Scenario(t, db)
	t.Setenv("AGENT_SUMMARY_V2_MODE", "off")
	h := &AgentSummaryHandler{db: db}

	cits, err := h.buildCitationsForSession(context.Background(), "session-1", "Charlie said [3]", "test-user", "req-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cits) != 1 || cits[0].Sender != "Charlie" {
		t.Errorf("V2 off must stay byte-identical to legacy even with request_id, got %d citations", len(cits))
	}
}
