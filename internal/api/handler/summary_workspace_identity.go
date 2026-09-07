package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

// summaryWorkspaceAgentSessionID is the internal identity used by the legacy
// Agent run/evidence stores. Those tables predate space-aware workspaces and
// key rows by (user, session), so the public session id alone is not a safe
// tenant boundary. Hashing the trusted space + public session preserves the
// existing schemas while keeping the value deterministic and below varchar(128).
func summaryWorkspaceAgentSessionID(spaceID, sessionID string, scopeVersion int) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(spaceID) + "\x00" + strings.TrimSpace(sessionID) + "\x00" + strconv.Itoa(scopeVersion)))
	return pipeline.WorkspaceAgentSessionPrefix + hex.EncodeToString(sum[:])
}

func summaryWorkspaceReplacementAgentSessionID(spaceID, sessionID string, scopeVersion int, requestID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(spaceID) + "\x00" + strings.TrimSpace(sessionID) + "\x00" + strconv.Itoa(scopeVersion) + "\x00replace\x00" + strings.TrimSpace(requestID)))
	return pipeline.WorkspaceAgentSessionPrefix + hex.EncodeToString(sum[:])
}

func persistedOrDerivedWorkspaceAgentSessionID(persisted, spaceID, sessionID string, scopeVersion int) string {
	if persisted = strings.TrimSpace(persisted); persisted != "" {
		return persisted
	}
	return summaryWorkspaceAgentSessionID(spaceID, sessionID, scopeVersion)
}
