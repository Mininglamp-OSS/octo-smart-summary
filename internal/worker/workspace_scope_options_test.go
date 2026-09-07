package worker

import "testing"

func TestChannelScopeOptionsForLegacyTaskNeverAddsSpaceFilter(t *testing.T) {
	if got := channelScopeOptionsForTask(false, "space-a", "legacy-session", false, true); got != nil {
		t.Fatalf("flag-off legacy options = %#v, want nil", got)
	}
	got := channelScopeOptionsForTask(true, "space-a", "legacy-session", false, true)
	if got == nil || got.SpaceID != "" || got.WorkspaceTask || got.ParticipantSourceSubset {
		t.Fatalf("flag-on legacy options = %#v, want legacy-compatible unscoped options", got)
	}
}

func TestChannelScopeOptionsForWorkspaceTaskCarriesSpaceAndParticipantPolicy(t *testing.T) {
	got := channelScopeOptionsForTask(false, "space-a", "summaryws:session", true, false)
	if got == nil || got.SpaceID != "space-a" || !got.WorkspaceTask || !got.ParticipantSourceUnion {
		t.Fatalf("workspace options = %#v", got)
	}
}
