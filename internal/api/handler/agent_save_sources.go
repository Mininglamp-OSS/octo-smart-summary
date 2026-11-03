package handler

import (
	"context"
	"log"
	"net/http"
	"strings"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

// Legacy clients can supply provenance directly. Type/name validation is not
// authorization: use the workspace's membership + space checks before storing
// any client-selected source, explicit origin or transcript-derived origin.
// Server-owned workspace scopes and permission-checked reference origins have
// their own validation paths.
func (h *AgentSummaryHandler) authorizeLegacySaveSources(ctx context.Context, spaceID, userID string, req createAgentSummaryReq, sessionOrigin *sourceReq) error {
	sources := append([]sourceReq(nil), req.Sources...)
	if sessionOrigin != nil {
		sources = append(sources, *sessionOrigin)
	}
	if req.OriginChannelID != nil {
		sources = append(sources, sourceReq{SourceID: *req.OriginChannelID, SourceType: req.OriginChannelType})
	}
	channels := make([]summaryWorkspaceChannel, 0, len(sources))
	for _, source := range sources {
		if source.SourceID == "" {
			continue
		}
		kind := ""
		switch source.SourceType {
		case model.SourceGroup:
			kind = "group"
		case model.SourceThread:
			kind = "thread"
		case model.SourceDirect:
			kind = "direct"
		default:
			return service.NewBizError(40001, "invalid source type", http.StatusBadRequest)
		}
		channels = append(channels, summaryWorkspaceChannel{ChatID: source.SourceID, ChatType: kind})
		if kind == "thread" {
			parts := strings.SplitN(source.SourceID, "____", 2)
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				return service.NewBizError(40008, "source unavailable", http.StatusNotFound)
			}
			// Thread discovery checks group membership, but its legacy query
			// does not filter inactive memberships. Also validate the parent.
			channels = append(channels, summaryWorkspaceChannel{ChatID: parts[0], ChatType: "group"})
		}
	}
	if len(channels) == 0 {
		return nil
	}
	validator := summaryWorkspaceCoordinator{imDB: h.imDB}
	valid, err := validator.validateSources(ctx, spaceID, userID, channels)
	if err != nil {
		log.Printf("[agent-save] source authorization unavailable: %v", err)
		return service.NewBizError(50000, "source validation unavailable", http.StatusServiceUnavailable)
	}
	if !valid {
		return service.NewBizError(40008, "source unavailable", http.StatusNotFound)
	}
	return nil
}
