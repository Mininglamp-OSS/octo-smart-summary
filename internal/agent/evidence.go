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
// Called by every data-fetching tool (fetch_channel, peek_channel,
// search_messages, filter_relevant) after successful message fetch.
//
// Returns error on DB write failure so callers can propagate the failure
// out of the tool handler. Since #161 (4-reviewer P1), evidence is the sole
// discovery source for CitationIndex assignment in both getSessionMessagePool
// (mid-run) and buildCitationsForSession (save-time): a silently-dropped write
// would make the handle's messages invisible to citation building in the
// whole session. Callers must NOT swallow this error — see the tool
// handlers for the required propagation pattern (return the error out of
// the handler so the runner surfaces it in tool_call output).
//
// Missing context values (uid/sessionID/handle empty) and a nil db are
// treated as legitimate skip conditions (return nil) — the former can occur
// in test paths that don't wire the full context chain, the latter in unit
// tests.
//
// Upsert semantics (ON DUPLICATE KEY UPDATE) allow idempotent tool re-execution.
func PersistEvidence(db *gorm.DB, ctx context.Context, handle string, messages []pipeline.Message) error {
	if db == nil {
		log.Printf("[evidence] skipping persistence: db is nil (likely test mode)")
		return nil
	}

	// Extract uid and session_id from context
	uid, _ := ctx.Value(ContextKeyUID).(string)
	sessionID, _ := ctx.Value(ContextKeySessionID).(string)

	if uid == "" || sessionID == "" || handle == "" {
		log.Printf("[evidence] skip: missing uid=%q session=%q handle=%q", uid, sessionID, handle)
		return nil
	}

	// Serialize messages to JSON
	evidenceJSON, err := json.Marshal(messages)
	if err != nil {
		log.Printf("[evidence] marshal failed handle=%s: %v", handle, err)
		return err
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
		return err
	}
	log.Printf("[evidence] persisted %d messages handle=%s session=%s", len(messages), handle, sessionID)
	return nil
}
