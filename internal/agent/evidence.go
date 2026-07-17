package agent

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"gorm.io/gorm"
)

// PersistEvidence writes citation evidence to DB for long-term recovery.
// Fixes SUM-158 Blocker C: buildCitationsForSession failing after 30-minute cache expiry.
//
// Called by fetch_channel and peek_channel tools after successful message fetch.
// Write failures are logged but do not block tool return (evidence is best-effort).
//
// Upsert semantics (ON DUPLICATE KEY UPDATE) allow idempotent tool re-execution.
func PersistEvidence(db *gorm.DB, ctx context.Context, handle string, messages []pipeline.Message) {
	if db == nil {
		log.Printf("[evidence] skipping persistence: db is nil (likely test mode)")
		return
	}

	// Extract uid and session_id from context
	uid, _ := ctx.Value(ContextKeyUID).(string)
	sessionID, _ := ctx.Value(ContextKeySessionID).(string)

	if uid == "" || sessionID == "" || handle == "" {
		log.Printf("[evidence] skip: missing uid=%q session=%q handle=%q", uid, sessionID, handle)
		return
	}

	// Serialize messages to JSON
	evidenceJSON, err := json.Marshal(messages)
	if err != nil {
		log.Printf("[evidence] marshal failed handle=%s: %v", handle, err)
		return
	}

	now := time.Now()
	evidence := model.AgentMessageEvidence{
		UserID:    uid,
		SessionID: sessionID,
		Handle:    handle,
		Evidence:  string(evidenceJSON),
		CreatedAt: now,
		UpdatedAt: now,
	}

	// Upsert: INSERT ... ON DUPLICATE KEY UPDATE
	err = db.WithContext(ctx).
		Exec(`INSERT INTO agent_message_evidence (user_id, session_id, handle, evidence, created_at, updated_at)
		      VALUES (?, ?, ?, ?, ?, ?)
		      ON DUPLICATE KEY UPDATE evidence = VALUES(evidence), updated_at = VALUES(updated_at)`,
			evidence.UserID, evidence.SessionID, evidence.Handle, evidence.Evidence, evidence.CreatedAt, evidence.UpdatedAt).Error

	if err != nil {
		log.Printf("[evidence] upsert failed handle=%s session=%s: %v", handle, sessionID, err)
	} else {
		log.Printf("[evidence] persisted %d messages handle=%s session=%s", len(messages), handle, sessionID)
	}
}
