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
	"sort"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/summaryrun"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"github.com/gin-gonic/gin"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// errAgentSaveIdempotencyConflict signals that the transaction detected a
// competing row for the same (space, user, key) tuple; the caller re-reads
// the existing binding to decide replay vs mismatch (parallels
// errBotIdempotencyConflict in bot_summary_create.go).
var errAgentSaveIdempotencyConflict = errors.New("agent save idempotency conflict")

// errAgentMessageRunMismatch is returned only after the message itself has
// passed the owner/session/role checks. It therefore means the selected draft's
// persisted run binding is stale or conflicts with the caller's request_id.
var errAgentMessageRunMismatch = errors.New("agent message does not match summary run")

// errWorkspacePreviewSaveStale deliberately collapses all workspace ownership,
// latest-preview and optimistic-version failures into one public conflict. The
// detailed suffix is useful in logs/tests but must not be exposed to callers,
// otherwise a message id could be used to probe another workspace.
var errWorkspacePreviewSaveStale = errors.New("workspace preview is stale")

type workspacePreviewSaveCandidate struct {
	Session model.AgentSummarySession
	Message model.AgentMessage
	Scope   summaryWorkspaceContext
	Content string
}

func isWorkspacePreviewSave(req createAgentSummaryReq) bool {
	return req.ScopeVersion != nil || req.ExpectedArtifactVersion != nil
}

func validateWorkspacePreviewSaveRequest(req createAgentSummaryReq) error {
	if !isWorkspacePreviewSave(req) {
		return nil
	}
	if req.ScopeVersion == nil || req.ExpectedArtifactVersion == nil {
		return errors.New("scope_version and expected_artifact_version must be provided together")
	}
	if *req.ScopeVersion <= 0 || *req.ExpectedArtifactVersion <= 0 {
		return errors.New("scope_version and expected_artifact_version must be positive")
	}
	if req.AgentMessageID <= 0 || req.SnapshotVersion <= 0 {
		return errors.New("workspace save requires agent_message_id and snapshot_version")
	}
	return nil
}

// loadWorkspacePreviewForSave resolves the only saveable artifact from
// server-owned workspace rows. In strict mode the visible assistant Content is
// conversational text; the deliverable is response_payload_json.preview.content.
// When lock is true the session and message rows are locked for the save
// transaction so a concurrent scope change, revision or save cannot pass the
// optimistic checks and create a second summary.
func loadWorkspacePreviewForSave(
	db *gorm.DB,
	spaceID, userID string,
	req createAgentSummaryReq,
	lock bool,
) (workspacePreviewSaveCandidate, error) {
	if err := validateWorkspacePreviewSaveRequest(req); err != nil {
		return workspacePreviewSaveCandidate{}, err
	}

	sessionQuery := db
	if lock {
		sessionQuery = sessionQuery.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var session model.AgentSummarySession
	if err := sessionQuery.Where(
		"space_id = ? AND user_id = ? AND session_id = ?",
		spaceID, userID, req.SessionID,
	).Take(&session).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return workspacePreviewSaveCandidate{}, fmt.Errorf("%w: session not found", errWorkspacePreviewSaveStale)
		}
		return workspacePreviewSaveCandidate{}, fmt.Errorf("load workspace session: %w", err)
	}
	if session.ContractVersion != summaryWorkspaceContractVersion {
		return workspacePreviewSaveCandidate{}, fmt.Errorf("%w: contract version differs", errWorkspacePreviewSaveStale)
	}
	if session.ScopeVersion != *req.ScopeVersion {
		return workspacePreviewSaveCandidate{}, fmt.Errorf("%w: session scope version differs", errWorkspacePreviewSaveStale)
	}
	if session.ArtifactVersion != *req.ExpectedArtifactVersion {
		return workspacePreviewSaveCandidate{}, fmt.Errorf("%w: session artifact version differs", errWorkspacePreviewSaveStale)
	}
	if session.LatestPreviewMessageID != req.AgentMessageID || session.LatestPreviewMessageID <= 0 {
		return workspacePreviewSaveCandidate{}, fmt.Errorf("%w: message is not the latest preview", errWorkspacePreviewSaveStale)
	}
	if session.LatestPreviewSavedTaskID > 0 {
		return workspacePreviewSaveCandidate{}, fmt.Errorf("%w: latest preview is already saved", errWorkspacePreviewSaveStale)
	}

	messageQuery := db
	if lock {
		messageQuery = messageQuery.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var message model.AgentMessage
	if err := messageQuery.Where(
		"id = ? AND space_id = ? AND user_id = ? AND session_id = ? AND role = ? AND tool_calls IS NULL",
		req.AgentMessageID, spaceID, userID, req.SessionID, "assistant",
	).Take(&message).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return workspacePreviewSaveCandidate{}, fmt.Errorf("%w: preview message not found", errWorkspacePreviewSaveStale)
		}
		return workspacePreviewSaveCandidate{}, fmt.Errorf("load workspace preview: %w", err)
	}
	if message.ResultType != agent.SummaryResultAgentPreview && message.ResultType != agent.SummaryResultAgentRevision {
		return workspacePreviewSaveCandidate{}, fmt.Errorf("%w: result type is not saveable", errWorkspacePreviewSaveStale)
	}
	if message.ScopeVersion != session.ScopeVersion || message.ScopeVersion != *req.ScopeVersion {
		return workspacePreviewSaveCandidate{}, fmt.Errorf("%w: message scope version differs", errWorkspacePreviewSaveStale)
	}
	if message.ArtifactVersion != session.ArtifactVersion || message.ArtifactVersion != *req.ExpectedArtifactVersion {
		return workspacePreviewSaveCandidate{}, fmt.Errorf("%w: message artifact version differs", errWorkspacePreviewSaveStale)
	}
	if message.SnapshotVersion != req.SnapshotVersion || message.SnapshotVersion != workspaceSnapshotVersion {
		return workspacePreviewSaveCandidate{}, fmt.Errorf("%w: snapshot version differs", errWorkspacePreviewSaveStale)
	}
	if message.SavedTaskID > 0 {
		return workspacePreviewSaveCandidate{}, fmt.Errorf("%w: preview message is already saved", errWorkspacePreviewSaveStale)
	}
	if message.ResponsePayload == nil || strings.TrimSpace(*message.ResponsePayload) == "" {
		return workspacePreviewSaveCandidate{}, fmt.Errorf("%w: response payload is missing", errWorkspacePreviewSaveStale)
	}

	var payload agent.SummaryResponsePayload
	if err := json.Unmarshal([]byte(*message.ResponsePayload), &payload); err != nil {
		return workspacePreviewSaveCandidate{}, fmt.Errorf("%w: invalid response payload", errWorkspacePreviewSaveStale)
	}
	if payload.ResultType != message.ResultType || payload.ExecutionTarget != "agent_preview" || payload.Preview == nil {
		return workspacePreviewSaveCandidate{}, fmt.Errorf("%w: response payload does not describe this preview", errWorkspacePreviewSaveStale)
	}
	if payload.Preview.Version != message.ArtifactVersion || strings.TrimSpace(payload.Preview.Content) == "" {
		return workspacePreviewSaveCandidate{}, fmt.Errorf("%w: preview payload version or content differs", errWorkspacePreviewSaveStale)
	}
	if message.ResultType == agent.SummaryResultAgentPreview {
		if message.ParentMessageID != 0 || payload.Preview.ParentMessageID != 0 {
			return workspacePreviewSaveCandidate{}, fmt.Errorf("%w: initial preview has a parent", errWorkspacePreviewSaveStale)
		}
	} else if message.ParentMessageID <= 0 || payload.Preview.ParentMessageID != message.ParentMessageID {
		return workspacePreviewSaveCandidate{}, fmt.Errorf("%w: revision parent differs", errWorkspacePreviewSaveStale)
	}

	scope := emptySummaryWorkspaceContext()
	if strings.TrimSpace(session.ScopeJSON) == "" {
		return workspacePreviewSaveCandidate{}, fmt.Errorf("%w: workspace scope is missing", errWorkspacePreviewSaveStale)
	}
	if err := json.Unmarshal([]byte(session.ScopeJSON), &scope); err != nil {
		return workspacePreviewSaveCandidate{}, fmt.Errorf("%w: workspace scope is invalid", errWorkspacePreviewSaveStale)
	}
	normalizedScope, err := normalizeSummaryWorkspaceContext(scope)
	if err != nil {
		return workspacePreviewSaveCandidate{}, fmt.Errorf("%w: workspace scope is invalid", errWorkspacePreviewSaveStale)
	}
	normalizedScope, err = hydrateSummaryWorkspaceContextFromPreview(normalizedScope, &message, userID)
	if err != nil {
		return workspacePreviewSaveCandidate{}, fmt.Errorf("%w: preview effective scope is invalid", errWorkspacePreviewSaveStale)
	}

	return workspacePreviewSaveCandidate{
		Session: session,
		Message: message,
		Scope:   normalizedScope,
		Content: payload.Preview.Content,
	}, nil
}

// applyWorkspaceScopeToSaveRequest makes the persisted workspace scope—not
// client-repeated legacy fields—the source of truth for origin, sources and
// referenced summaries. The frontend therefore only needs to send the preview
// identity and optimistic versions.
func applyWorkspaceScopeToSaveRequest(req *createAgentSummaryReq, candidate workspacePreviewSaveCandidate) {
	req.OriginChannelID = nil
	req.OriginChannelType = 0
	if channelID, channelType := summaryWorkspaceOrigin(candidate.Scope); channelID != "" {
		req.OriginChannelID = &channelID
		req.OriginChannelType = channelType
	}
	req.Sources = make([]sourceReq, 0, len(candidate.Scope.SelectedChannels))
	for _, source := range summaryWorkspaceSources(candidate.Scope) {
		req.Sources = append(req.Sources, sourceReq{SourceID: source.SourceID, SourceType: source.SourceType})
	}
	req.Participants = nil
	req.ReferencedTaskIDs = append([]int64(nil), candidate.Scope.ReferencedTaskIDs...)
}

// workspaceAgentSaveTimeRange resolves the same authoritative time boundary
// used by workspace Workflow creation. Agent previews remain user-saveable,
// but the resulting formal summary must not lose the selected scope or fall
// back to the legacy now/now placeholder.
func workspaceAgentSaveTimeRange(scope summaryWorkspaceContext, now time.Time) (service.SummaryWorkflowTimeRange, error) {
	selected, err := workspaceWorkflowTimeRange(scope)
	if err != nil {
		return service.SummaryWorkflowTimeRange{}, err
	}
	if selected != nil {
		return *selected, nil
	}
	return service.SummaryWorkflowTimeRange{
		Start: now.Add(-service.AgentSummaryDefaultTimeRangeDays * 24 * time.Hour),
		End:   now,
	}, nil
}

func markWorkspacePreviewSaved(
	tx *gorm.DB,
	spaceID, userID, sessionID string,
	candidate workspacePreviewSaveCandidate,
	taskID int64,
) error {
	messageUpdate := tx.Model(&model.AgentMessage{}).
		Where(
			"id = ? AND space_id = ? AND user_id = ? AND session_id = ? AND saved_task_id = 0",
			candidate.Message.ID, spaceID, userID, sessionID,
		).
		Update("saved_task_id", taskID)
	if messageUpdate.Error != nil {
		return fmt.Errorf("mark workspace preview saved: %w", messageUpdate.Error)
	}
	if messageUpdate.RowsAffected != 1 {
		return fmt.Errorf("%w: preview save marker changed", errWorkspacePreviewSaveStale)
	}

	now := timezone.Now()
	sessionUpdate := tx.Model(&model.AgentSummarySession{}).
		Where(
			"space_id = ? AND user_id = ? AND session_id = ? AND scope_version = ? AND artifact_version = ? AND latest_preview_message_id = ? AND latest_preview_saved_task_id = 0",
			spaceID, userID, sessionID, candidate.Session.ScopeVersion, candidate.Session.ArtifactVersion, candidate.Message.ID,
		).
		Updates(map[string]interface{}{
			"latest_preview_saved_task_id": taskID,
			"state_version":                gorm.Expr("state_version + 1"),
			"expires_at":                   summaryWorkspaceExpiresAt(now),
			"updated_at":                   now,
		})
	if sessionUpdate.Error != nil {
		return fmt.Errorf("mark workspace session saved: %w", sessionUpdate.Error)
	}
	if sessionUpdate.RowsAffected != 1 {
		return fmt.Errorf("%w: workspace save marker changed", errWorkspacePreviewSaveStale)
	}
	return nil
}

func writeWorkspacePreviewSaveConflict(c *gin.Context) {
	c.JSON(409, apiResponse{
		Code:    40901,
		Message: "agent_draft_stale: 当前预览已变化或已保存，请刷新工作台后重试",
		Data: map[string]interface{}{
			"reason":          "workspace_preview_stale",
			"recovery_action": "reload_session",
		},
	})
}

// Reuse the bot handler's Idempotency-Key regex + length cap so the two
// user-facing endpoints share one canonical validation rule. Declared here as
// package-local aliases to keep this file self-contained on read; they point
// at the same const/var so the shared contract cannot drift.
const maxAgentSaveIdempotencyKeyLen = maxBotIdempotencyKeyLen

var agentSaveIdempotencyKeyPattern = botIdempotencyKeyPattern

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
// has no output" so the API surface does not leak whether the id exists).
//
// Callers pass a positive messageID to opt into the BE-2 targeted lookup;
// messageID == 0 keeps the pre-BE-2 "latest assistant on session" behaviour.
// This dual mode lets FE-2 (SUM-7) roll
// out the strict form while older frontends keep working during the release
// window; once FE-2 ships, the fallback can be removed in a follow-up.
func loadAgentMessageForSave(db *gorm.DB, sessionID, userID string, messageID int64) (model.AgentMessage, error) {
	var msg model.AgentMessage
	if messageID <= 0 {
		// Legacy fallback returns the row (not just the content) so the caller can echo the
		// resolved AgentMessageID onto SummaryTask for audit even on the
		// legacy path.
		err := db.Where(
			"space_id = ? AND user_id = ? AND session_id = ? AND role = ? AND tool_calls IS NULL AND content <> ''",
			legacyAgentMessageSpaceID, userID, sessionID, "assistant",
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
		"id = ? AND space_id = ? AND user_id = ? AND session_id = ? AND role = ? AND tool_calls IS NULL AND content <> ''",
		messageID, legacyAgentMessageSpaceID, userID, sessionID, "assistant",
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

type agentMessageRunBinding struct {
	RequestID         string
	EvidenceSessionID string
}

const maxWorkspaceCitationParentDepth = 128

// resolveAgentMessageRunBinding makes the server-persisted message→run binding
// authoritative before any deliverable rows are written. Besides deriving the
// request id, it returns the internal session identity that the generating run
// used to persist citation evidence. Legacy messages without a run preserve the
// previous request-id and session fallback behaviour.
func resolveAgentMessageRunBinding(ctx context.Context, db *gorm.DB, userID, sessionID, requestID string, msg model.AgentMessage) (agentMessageRunBinding, error) {
	if msg.RunID == "" {
		return agentMessageRunBinding{RequestID: requestID}, nil
	}
	run, err := summaryrun.NewStore(db).GetByID(ctx, userID, msg.RunID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return agentMessageRunBinding{}, fmt.Errorf("%w: bound run not found", errAgentMessageRunMismatch)
		}
		return agentMessageRunBinding{}, err
	}
	if strings.TrimSpace(msg.SpaceID) != "" {
		if msg.TurnID <= 0 {
			return agentMessageRunBinding{}, fmt.Errorf("%w: workspace turn is missing", errAgentMessageRunMismatch)
		}
		var turn model.AgentSummaryTurn
		if err := db.WithContext(ctx).
			Where("id = ? AND space_id = ? AND user_id = ? AND session_id = ?", msg.TurnID, msg.SpaceID, userID, sessionID).
			Take(&turn).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return agentMessageRunBinding{}, fmt.Errorf("%w: workspace turn not found", errAgentMessageRunMismatch)
			}
			return agentMessageRunBinding{}, err
		}
		if turn.RunID != run.RunID || turn.ScopeVersion != msg.ScopeVersion || turn.RequestID != run.RequestID {
			return agentMessageRunBinding{}, fmt.Errorf("%w: workspace turn differs", errAgentMessageRunMismatch)
		}
	} else if run.SessionID != sessionID {
		return agentMessageRunBinding{}, fmt.Errorf("%w: session differs", errAgentMessageRunMismatch)
	}
	if requestID != "" && requestID != run.RequestID {
		return agentMessageRunBinding{}, fmt.Errorf("%w: request differs", errAgentMessageRunMismatch)
	}
	return agentMessageRunBinding{
		RequestID:         run.RequestID,
		EvidenceSessionID: run.SessionID,
	}, nil
}

func resolveAgentMessageRequestID(ctx context.Context, db *gorm.DB, userID, sessionID, requestID string, msg model.AgentMessage) (string, error) {
	binding, err := resolveAgentMessageRunBinding(ctx, db, userID, sessionID, requestID, msg)
	return binding.RequestID, err
}

// workspaceRevisionAncestorBindings returns the persisted run bindings for the
// selected revision's parent chain, nearest first. The chain is owner-, space-,
// session-, and scope-bound so a no-fetch wording revision can reuse only the
// evidence that its visible parent preview was grounded in.
func workspaceRevisionAncestorBindings(
	ctx context.Context,
	db *gorm.DB,
	userID, sessionID string,
	message model.AgentMessage,
) ([]agentMessageRunBinding, error) {
	if message.ResultType != agent.SummaryResultAgentRevision || message.ParentMessageID <= 0 {
		return nil, nil
	}
	bindings := make([]agentMessageRunBinding, 0, 4)
	seenMessages := map[int64]struct{}{message.ID: {}}
	seenBindings := make(map[string]struct{})
	parentID := message.ParentMessageID
	for depth := 0; parentID > 0 && depth < maxWorkspaceCitationParentDepth; depth++ {
		if _, exists := seenMessages[parentID]; exists {
			return nil, fmt.Errorf("%w: preview parent cycle", errWorkspacePreviewSaveStale)
		}
		seenMessages[parentID] = struct{}{}
		var parent model.AgentMessage
		if err := db.WithContext(ctx).Where(
			"id = ? AND space_id = ? AND user_id = ? AND session_id = ? AND scope_version = ? AND role = ? AND tool_calls IS NULL",
			parentID, message.SpaceID, userID, sessionID, message.ScopeVersion, "assistant",
		).Take(&parent).Error; err != nil {
			return nil, fmt.Errorf("%w: preview parent not found", errWorkspacePreviewSaveStale)
		}
		if parent.ResultType != agent.SummaryResultAgentPreview && parent.ResultType != agent.SummaryResultAgentRevision {
			return nil, fmt.Errorf("%w: preview parent is not saveable", errWorkspacePreviewSaveStale)
		}
		binding, err := resolveAgentMessageRunBinding(ctx, db, userID, sessionID, "", parent)
		if err != nil {
			if errors.Is(err, errAgentMessageRunMismatch) {
				return nil, fmt.Errorf("%w: preview parent run binding changed", errWorkspacePreviewSaveStale)
			}
			return nil, err
		}
		if binding.EvidenceSessionID != "" {
			key := binding.EvidenceSessionID + "\x00" + binding.RequestID
			if _, exists := seenBindings[key]; !exists {
				seenBindings[key] = struct{}{}
				bindings = append(bindings, binding)
			}
		}
		parentID = parent.ParentMessageID
	}
	if parentID > 0 {
		return nil, fmt.Errorf("%w: preview parent chain is too deep", errWorkspacePreviewSaveStale)
	}
	return bindings, nil
}

func workspaceEvidenceSessionHasRows(ctx context.Context, db *gorm.DB, userID, sessionID string) (bool, error) {
	if strings.TrimSpace(sessionID) == "" {
		return false, nil
	}
	var count int64
	if err := db.WithContext(ctx).Model(&model.AgentMessageEvidence{}).
		Where("user_id = ? AND session_id = ?", userID, sessionID).
		Limit(1).Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// buildWorkspacePreviewCitations keeps per-turn evidence isolation while
// preserving citations for a wording-only revision. The selected run remains
// authoritative when it fetched anything. Only an evidence-empty revision may
// walk its explicit parent preview lineage, and the first grounded ancestor
// must resolve the current [n] sequence or the save is rejected.
func (h *AgentSummaryHandler) buildWorkspacePreviewCitations(
	ctx context.Context,
	db *gorm.DB,
	userID, sessionID, content string,
	candidate workspacePreviewSaveCandidate,
	binding agentMessageRunBinding,
) ([]model.Citation, error) {
	evidenceSessionID := binding.EvidenceSessionID
	if evidenceSessionID == "" {
		evidenceSessionID = persistedOrDerivedWorkspaceAgentSessionID(
			candidate.Session.AgentSessionID,
			candidate.Session.SpaceID,
			sessionID,
			candidate.Session.ScopeVersion,
		)
	}
	citations, err := h.buildCitationsForSessionWithDB(ctx, db, evidenceSessionID, content, userID, binding.RequestID)
	if err != nil {
		return nil, err
	}
	if !contentHasCitationSequence(content) {
		return citations, nil
	}
	if len(citations) > 0 {
		if citationsValid(content, citations, true) {
			return citations, nil
		}
		return nil, fmt.Errorf("%w: preview citation evidence does not resolve", errWorkspacePreviewSaveStale)
	}
	currentHasEvidence, err := workspaceEvidenceSessionHasRows(ctx, db, userID, evidenceSessionID)
	if err != nil {
		return nil, err
	}
	if currentHasEvidence || candidate.Message.ResultType != agent.SummaryResultAgentRevision {
		return nil, fmt.Errorf("%w: preview citation evidence does not resolve", errWorkspacePreviewSaveStale)
	}

	ancestors, err := workspaceRevisionAncestorBindings(ctx, db, userID, sessionID, candidate.Message)
	if err != nil {
		return nil, err
	}
	for _, ancestor := range ancestors {
		hasEvidence, err := workspaceEvidenceSessionHasRows(ctx, db, userID, ancestor.EvidenceSessionID)
		if err != nil {
			return nil, err
		}
		if !hasEvidence {
			continue
		}
		citations, err := h.buildCitationsForSessionWithDB(
			ctx, db, ancestor.EvidenceSessionID, content, userID, ancestor.RequestID,
		)
		if err != nil {
			return nil, err
		}
		if len(citations) > 0 && citationsValid(content, citations, true) {
			return citations, nil
		}
		return nil, fmt.Errorf("%w: parent preview citation evidence does not resolve", errWorkspacePreviewSaveStale)
	}
	return nil, fmt.Errorf("%w: preview citation evidence is unavailable", errWorkspacePreviewSaveStale)
}

// canonicalAgentSaveRequestHash returns a deterministic sha256 fingerprint of
// the semantic content of an Agent-save request. Fields deliberately EXCLUDE
// the ambient user/space identity (already keyed in the unique index) and
// the assistant content (server-trusted, would defeat the hash-vs-body-drift
// contract because the client can't see server-canonicalised text).
//
// The hash is intentionally computed from request-owned fields only, before
// any session/message read. A successful save deletes agent_message rows, so
// server-resolved values cannot be part of a replay key.
func canonicalAgentSaveRequestHash(userID string, req createAgentSummaryReq) string {
	type canonicalSource struct {
		SourceType int    `json:"source_type"`
		SourceID   string `json:"source_id"`
	}
	type canonicalParticipant struct {
		UserID   string `json:"user_id"`
		UserName string `json:"user_name"`
	}

	sourceInput := req.Sources
	participantInput := req.Participants
	referenceInput := req.ReferencedTaskIDs
	originInput := req.OriginChannelID
	originTypeInput := req.OriginChannelType
	if isWorkspacePreviewSave(req) {
		// Workspace persistence replaces these client fields with the
		// server-authoritative scope. They are not semantic request inputs and
		// therefore must not turn an otherwise identical retry into a mismatch.
		sourceInput = nil
		participantInput = nil
		referenceInput = nil
		originInput = nil
		originTypeInput = 0
	}

	sortedSources := make([]canonicalSource, 0, len(sourceInput))
	seenSources := make(map[string]struct{}, len(sourceInput))
	for _, s := range sourceInput {
		if s.SourceID == "" {
			continue
		}
		key := fmt.Sprintf("%d:%s", s.SourceType, s.SourceID)
		if _, exists := seenSources[key]; exists {
			continue
		}
		seenSources[key] = struct{}{}
		sortedSources = append(sortedSources, canonicalSource{SourceType: s.SourceType, SourceID: s.SourceID})
	}
	sort.SliceStable(sortedSources, func(i, j int) bool {
		if sortedSources[i].SourceType != sortedSources[j].SourceType {
			return sortedSources[i].SourceType < sortedSources[j].SourceType
		}
		return sortedSources[i].SourceID < sortedSources[j].SourceID
	})

	participants := make([]canonicalParticipant, 0, len(participantInput))
	seenParticipants := map[string]struct{}{userID: {}}
	for _, p := range participantInput {
		if p.UserID == "" {
			continue
		}
		if _, exists := seenParticipants[p.UserID]; exists {
			continue
		}
		seenParticipants[p.UserID] = struct{}{}
		participants = append(participants, canonicalParticipant{UserID: p.UserID, UserName: p.UserName})
	}
	sort.Slice(participants, func(i, j int) bool { return participants[i].UserID < participants[j].UserID })

	// ReferencedTaskIDs are order-sensitive: element 0 is the origin/citation
	// inheritance source. Preserve first-occurrence order while removing
	// duplicates, matching the normalization performed at handler entry.
	refCopy := dedupReferencedTaskIDs(referenceInput)

	originProvided := originInput != nil
	originID := ""
	originType := 0
	if originProvided {
		originID = *originInput
		originType = originTypeInput
	}
	payload := struct {
		SessionID               string                 `json:"session_id"`
		RequestID               string                 `json:"request_id"`
		AgentMessageID          int64                  `json:"agent_message_id"`
		SnapshotVersion         int                    `json:"snapshot_version"`
		ScopeVersion            *int                   `json:"scope_version"`
		ExpectedArtifactVersion *int                   `json:"expected_artifact_version"`
		Title                   string                 `json:"title"`
		OriginProvided          bool                   `json:"origin_provided"`
		OriginChannelID         string                 `json:"origin_channel_id"`
		OriginChannelType       int                    `json:"origin_channel_type"`
		Sources                 []canonicalSource      `json:"sources"`
		Participants            []canonicalParticipant `json:"participants"`
		ReferencedTaskIDs       []int64                `json:"referenced_task_ids"`
	}{
		SessionID:               strings.TrimSpace(req.SessionID),
		RequestID:               strings.TrimSpace(req.RequestID),
		AgentMessageID:          req.AgentMessageID,
		SnapshotVersion:         req.SnapshotVersion,
		ScopeVersion:            req.ScopeVersion,
		ExpectedArtifactVersion: req.ExpectedArtifactVersion,
		Title:                   strings.TrimSpace(req.Title),
		OriginProvided:          originProvided,
		OriginChannelID:         originID,
		OriginChannelType:       originType,
		Sources:                 sortedSources,
		Participants:            participants,
		ReferencedTaskIDs:       refCopy,
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

// createAgentSaveIdempotencyBinding inserts the unique save binding and then
// reads the tuple back to determine whether this transaction won. Do not use
// RowsAffected for this decision: GORM maps DoNothing to a MySQL no-op update,
// and clientFoundRows=true reports that conflict as one affected row.
func createAgentSaveIdempotencyBinding(tx *gorm.DB, binding *model.SummaryAgentSaveIdempotency) error {
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(binding).Error; err != nil {
		return fmt.Errorf("create agent save idempotency: %w", err)
	}

	var persisted model.SummaryAgentSaveIdempotency
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("space_id = ? AND user_id = ? AND idempotency_key = ?", binding.SpaceID, binding.UserID, binding.IdempotencyKey).
		First(&persisted).Error; err != nil {
		return fmt.Errorf("read back agent save idempotency: %w", err)
	}
	if persisted.TaskID != binding.TaskID {
		return errAgentSaveIdempotencyConflict
	}
	return nil
}

// findAgentSaveIdempotentTaskWithHash mirrors
// findBotIdempotentTaskWithHash. See there for the return-tuple contract;
// the only axis change is the unique key (space, user, key) instead of
// (space, bot, key).
func findAgentSaveIdempotentTaskWithHash(
	ctx context.Context, db *gorm.DB,
	spaceID, userID, key, requestHash string,
) (model.SummaryTask, bool, bool, bool, error) {
	var binding model.SummaryAgentSaveIdempotency
	if err := db.WithContext(ctx).
		Where("space_id = ? AND user_id = ? AND idempotency_key = ?", spaceID, userID, key).
		First(&binding).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return model.SummaryTask{}, false, false, false, nil
		}
		return model.SummaryTask{}, false, false, false, err
	}
	// Load only a live referenced task. SummaryTask.DeletedAt is *time.Time,
	// not gorm.DeletedAt, so the predicate must be explicit.
	var task model.SummaryTask
	if err := db.WithContext(ctx).
		Where("id = ? AND space_id = ? AND creator_id = ? AND deleted_at IS NULL", binding.TaskID, spaceID, userID).
		First(&task).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// Keep the binding as a tombstone. Reusing an idempotency key after
			// its resource was deleted must not recreate a side effect, and must
			// not fall into the UNIQUE-conflict -> 500 loop either.
			return model.SummaryTask{ID: binding.TaskID}, binding.RequestHash != requestHash, true, true, nil
		}
		return model.SummaryTask{}, false, false, false, err
	}
	if binding.RequestHash != requestHash {
		return task, true, true, false, nil
	}
	return task, false, true, false, nil
}
