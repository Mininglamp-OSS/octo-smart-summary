package handler

// snapshot_scope_input.go — SUM-BE1 helper: build the shared
// model.SnapshotScope used by service.ValidatePersonalWorkflow /
// ValidateScheduledWorkflow from the handler request structs.
//
// Revised per SUM-9: the previous parallel SnapshotScopeInput /
// SourceInput / ParticipantInput / ScheduleInput mirror types are gone.
// The shared validator now consumes model.SnapshotScope directly, so this
// file only has the small extractor that turns []sourceReq into
// []string channel IDs for the scope. Everything else the validator
// needs (title, topic, source count, origin channel, time range,
// participant count, schedule recurrence primitives) is passed as
// plain arguments; no converter layer.

import (
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// channelIDsFromSources projects a slice of sourceReq into the channel_ids
// slice model.SnapshotScope carries. Empty input -> nil (matches the shape
// SnapshotScope stores when no sources were supplied).
func channelIDsFromSources(in []sourceReq) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s.SourceID != "" {
			out = append(out, s.SourceID)
		}
	}
	return out
}

// scheduleScopeFromReq builds the shared model.SnapshotScope for a schedule
// create request. Schedule requests carry sources but no explicit time_range
// on the wire (time range is derived from time_range_type at run time), so
// the scope carries ChannelIDs only and leaves TimeRange zero-valued.
func scheduleScopeFromReq(req createScheduleReq) model.SnapshotScope {
	return model.SnapshotScope{
		ChannelIDs: channelIDsFromSources(req.Sources),
	}
}

// Explicit references so build tools do not flag helpers as unused when only
// one call site is active in a given build tag configuration.
var _ = model.SnapshotScope{}
