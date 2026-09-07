package agent

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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
// Upsert semantics allow idempotent tool re-execution. GORM renders the native
// form for the active dialect (ON DUPLICATE KEY on MySQL, ON CONFLICT on
// SQLite), which lets integration tests exercise the real fetch handler.
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

	// Keep CreatedAt from the first write; retries refresh only the snapshot and
	// UpdatedAt, matching the previous MySQL-specific statement.
	err = db.WithContext(ctx).Clauses(evidenceUpsertClause()).Create(&evidence).Error

	if err != nil {
		log.Printf("[evidence] upsert failed handle=%s session=%s: %v", handle, sessionID, err)
		return err
	}
	markSummaryCitationEvidence(ctx, messages)
	log.Printf("[evidence] persisted %d messages handle=%s session=%s", len(messages), handle, sessionID)
	return nil
}

// evidenceUpsertClause is kept as a helper so dialect-specific SQL generation
// can be pinned in tests. In particular, MySQL must preserve the original
// created_at while refreshing only the evidence snapshot and updated_at.
func evidenceUpsertClause() clause.OnConflict {
	return clause.OnConflict{
		Columns: []clause.Column{
			{Name: "user_id"},
			{Name: "session_id"},
			{Name: "handle"},
		},
		DoUpdates: clause.AssignmentColumns([]string{"evidence", "updated_at"}),
	}
}
