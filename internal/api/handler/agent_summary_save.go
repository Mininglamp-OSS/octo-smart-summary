package handler

// SUM-BE2 (Agent 草稿与安全落库) — save-time helpers for CreateAgentSummary.
//
// This file is the storage-side counterpart the SUM-9 review said BE-1 could
// not add: it introduces
//
//  1. a targeted assistant-message loader that reads by (id, user, session,
//     role='assistant', tool_calls IS NULL) — so the caller-supplied
//     agent_message_id is verified against server-trusted rows instead of
//     the pre-BE-2 "latest assistant on session" heuristic;
//
//  2. an idempotency binding for Agent save, shaped exactly like
//     summary_bot_create_idempotency (see bot_summary_create.go) — same
//     replay contract, same request-hash mismatch → 409 semantics — but
//     keyed on (space_id, user_id, idempotency_key) instead of bot_id
//     because Agent save is user-owned;
//
//  3. a canonical request-hash so a retry with identical semantics replays,
//     a retry with different semantics 409s.
//
// Everything Agent-save-only lives here so agent_summary.go stays focused on
// origin-channel resolution + citations, and so review can look at storage
// and hashing in isolation.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"

	"gorm.io/gorm"
)

// errAgentSaveIdempotencyConflict signals that the transaction detected a
// competing row for the same (space, user, key) tuple; the caller re-reads
// the existing binding to decide replay vs mismatch (parallels
// errBotIdempotencyConflict in bot_summary_create.go).
var errAgentSaveIdempotencyConflict = errors.New("agent save idempotency conflict")

// Reuse the bot handler's Idempotency-Key regex + length cap so the two
// user-facing endpoints share one canonical validation rule. Declared here as
// package-local aliases to keep this file self-contained on read; they point
// at the same const/var so the shared contract cannot drift.
const maxAgentSaveIdempotencyKeyLen = maxBotIdempotencyKeyLen

var agentSaveIdempotencyKeyPattern = botIdempotencyKeyPattern

// agentSaveKeyPresentPattern rejects a header the client explicitly sent but
// filled with only whitespace / bracket noise. len==0 (header absent) is the
// pre-BE-2 legacy path handled separately by the caller.
var agentSaveKeyPresentPattern = regexp.MustCompile(`\S`)

// validAgentSaveIdempotencyKey mirrors validBotIdempotencyKey.
func validAgentSaveIdempotencyKey(key string) bool {
	return len(key) > 0 &&
		len(key) <= maxAgentSaveIdempotencyKeyLen &&
		agentSaveIdempotencyKeyPattern.MatchString(key)
}

// loadAgentMessageForSave loads the assistant message the client claims is
// the draft to save, verifying every DB-side identity axis in one WHERE
// clause. Non-assistant / tool-call / cross-user / cross-session / deleted
// message ids all return errNoAgentOutput (indistinguishable from "session
// has no output" so the API surface does not leak whether the id exists —
// mirrors loadLatestAssistantContent's owner-scope 404 discipline).
//
// Callers pass a positive messageID to opt into the BE-2 targeted lookup;
// messageID == 0 keeps the pre-BE-2 "latest assistant on session" behaviour
// (see loadLatestAssistantContent). This dual mode lets FE-2 (SUM-7) roll
// out the strict form while older frontends keep working during the release
// window; once FE-2 ships, the fallback can be removed in a follow-up.
func loadAgentMessageForSave(db *gorm.DB, sessionID, userID string, messageID int64) (model.AgentMessage, error) {
	var msg model.AgentMessage
	if messageID <= 0 {
		// Legacy fallback: reuse loadLatestAssistantContent's semantics but
		// return the row (not just the content) so the caller can echo the
		// resolved AgentMessageID onto SummaryTask for audit even on the
		// legacy path.
		err := db.Where(
			"user_id = ? AND session_id = ? AND role = ? AND tool_calls IS NULL AND content <> ''",
			userID, sessionID, "assistant",
		).Order("id DESC").Limit(1).Take(&msg).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return model.AgentMessage{}, errNoAgentOutput
			}
			return model.AgentMessage{}, err
		}
		return msg, nil
	}
	err := db.Where(
		"id = ? AND user_id = ? AND session_id = ? AND role = ? AND tool_calls IS NULL AND content <> ''",
		messageID, userID, sessionID, "assistant",
	).Take(&msg).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// Wrong owner, wrong session, wrong role, tool-call message, or
			// simply not found — all collapse to the same public error so an
			// attacker cannot probe existence.
			return model.AgentMessage{}, errNoAgentOutput
		}
		return model.AgentMessage{}, err
	}
	return msg, nil
}

// canonicalAgentSaveRequestHash returns a deterministic sha256 fingerprint of
// the semantic content of an Agent-save request. Fields deliberately EXCLUDE
// the ambient user/space identity (already keyed in the unique index) and
// the assistant content (server-trusted, would defeat the hash-vs-body-drift
// contract because the client can't see server-canonicalised text).
//
// Included:
//   - session_id + agent_message_id + snapshot_version — the exact draft
//     the client thinks it is saving;
//   - title (post-trim), origin channel (id + type after resolution);
//   - referenced_task_ids (sorted, deduped) — same task retried should hash
//     the same regardless of ordering;
//   - sources (sorted by (type, id)).
func canonicalAgentSaveRequestHash(
	sessionID, title, originID string,
	originType int,
	messageID int64,
	snapshotVersion int,
	sources []sourceReq,
	referencedTaskIDs []int64,
) string {
	sortedSources := make([]sourceReq, 0, len(sources))
	for _, s := range sources {
		if s.SourceID == "" {
			continue
		}
		sortedSources = append(sortedSources, s)
	}
	sort.SliceStable(sortedSources, func(i, j int) bool {
		if sortedSources[i].SourceType != sortedSources[j].SourceType {
			return sortedSources[i].SourceType < sortedSources[j].SourceType
		}
		return sortedSources[i].SourceID < sortedSources[j].SourceID
	})

	refCopy := append([]int64(nil), referencedTaskIDs...)
	sort.Slice(refCopy, func(i, j int) bool { return refCopy[i] < refCopy[j] })
	// De-dup adjacent equal ids after sort.
	if len(refCopy) > 1 {
		w := 1
		for i := 1; i < len(refCopy); i++ {
			if refCopy[i] != refCopy[i-1] {
				refCopy[w] = refCopy[i]
				w++
			}
		}
		refCopy = refCopy[:w]
	}

	payload := struct {
		SessionID         string      `json:"session_id"`
		AgentMessageID    int64       `json:"agent_message_id"`
		SnapshotVersion   int         `json:"snapshot_version"`
		Title             string      `json:"title"`
		OriginChannelID   string      `json:"origin_channel_id"`
		OriginChannelType int         `json:"origin_channel_type"`
		Sources           []sourceReq `json:"sources"`
		ReferencedTaskIDs []int64     `json:"referenced_task_ids"`
	}{
		SessionID:         strings.TrimSpace(sessionID),
		AgentMessageID:    messageID,
		SnapshotVersion:   snapshotVersion,
		Title:             strings.TrimSpace(title),
		OriginChannelID:   originID,
		OriginChannelType: originType,
		Sources:           sortedSources,
		ReferencedTaskIDs: refCopy,
	}
	// json.Marshal on a struct is field-order deterministic — same layout in
	// two processes hashes identically.
	buf, err := json.Marshal(payload)
	if err != nil {
		// Marshal only fails on unmarshalable types (chan, func); the
		// payload here is a concrete struct of plain types so this is
		// unreachable in practice. Fall back to Sprintf so we never panic
		// and the hash still differs across different inputs.
		buf = []byte(fmt.Sprintf("%#v", payload))
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])
}

// findAgentSaveIdempotentTaskWithHash mirrors
// findBotIdempotentTaskWithHash. See there for the return-tuple contract;
// the only axis change is the unique key (space, user, key) instead of
// (space, bot, key).
func findAgentSaveIdempotentTaskWithHash(
	ctx context.Context, db *gorm.DB,
	spaceID, userID, key, requestHash string,
) (model.SummaryTask, bool, bool, error) {
	var binding model.SummaryAgentSaveIdempotency
	if err := db.WithContext(ctx).
		Where("space_id = ? AND user_id = ? AND idempotency_key = ?", spaceID, userID, key).
		First(&binding).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return model.SummaryTask{}, false, false, nil
		}
		return model.SummaryTask{}, false, false, err
	}
	// Load the referenced task (space + creator scoped so a stale binding
	// whose task was hard-deleted falls through to a fresh create instead
	// of silently pointing at another user's row).
	var task model.SummaryTask
	if err := db.WithContext(ctx).
		Where("id = ? AND space_id = ? AND creator_id = ?", binding.TaskID, spaceID, userID).
		First(&task).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return model.SummaryTask{}, false, false, nil
		}
		return model.SummaryTask{}, false, false, err
	}
	if binding.RequestHash != requestHash {
		return task, true, true, nil
	}
	return task, false, true, nil
}
