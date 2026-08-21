package handler

// snapshot_scope_input.go — SUM-BE1 helper: build the shared
// model.SnapshotScope used by service.ValidatePersonalWorkflow from the
// handler request structs.
//
// Revised per SUM-9: the previous parallel SnapshotScopeInput /
// SourceInput / ParticipantInput / ScheduleInput mirror types are gone.
// The shared validator now consumes model.SnapshotScope directly, so this
// file only has the small extractor that turns []sourceReq into
// []string channel IDs for the scope. Everything else the validator
// needs is passed as plain arguments; no converter layer.

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
