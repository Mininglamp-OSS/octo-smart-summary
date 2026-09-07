package pipeline

import (
	"context"
	"reflect"
	"testing"
)

func TestGetPeerUIDRequiresActorParticipation(t *testing.T) {
	tests := []struct {
		name      string
		channelID string
		selfUID   string
		want      string
	}{
		{name: "actor first", channelID: "actor@peer", selfUID: "actor", want: "peer"},
		{name: "actor second", channelID: "peer@actor", selfUID: "actor", want: "peer"},
		{name: "malformed", channelID: "peer", selfUID: "actor", want: ""},
		{name: "actor absent", channelID: "peer-a@peer-b", selfUID: "actor", want: ""},
		{name: "self dm", channelID: "actor@actor", selfUID: "actor", want: "actor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := getPeerUID(tt.channelID, tt.selfUID); got != tt.want {
				t.Fatalf("getPeerUID(%q, %q) = %q, want %q", tt.channelID, tt.selfUID, got, tt.want)
			}
		})
	}
}

func TestDMSourceCoverageUsesCanonicalIDFromBareConversationPeer(t *testing.T) {
	canonical := NormalizeDMChannelID("peer", "actor", 1)
	channels := []ChannelInfo{{ChannelID: canonical, ChannelType: 1, PeerUID: "peer"}}
	sources := []map[string]interface{}{{"source_id": "peer", "source_type": 3}}

	if err := validateExplicitSourceCoverage(channels, sources, "actor"); err != nil {
		t.Fatalf("legacy/team source coverage rejected a discovered DM: %v", err)
	}
	filtered := filterAvailableSpecifiedSources(channels, sources, "actor")
	if len(filtered) != 1 {
		t.Fatalf("participant source subset dropped a discovered DM: %#v", filtered)
	}
}

func TestApplyParticipantChannelScopePreservesWorkspaceUnionSources(t *testing.T) {
	channels := []ChannelInfo{{ChannelID: "group-a"}, {ChannelID: "group-b"}}
	got, err := applyParticipantChannelScope(
		context.Background(), channels, []string{"member-a", "member-b"}, nil,
		&ChannelScopeOptions{ParticipantSourceUnion: true},
	)
	if err != nil {
		t.Fatalf("applyParticipantChannelScope() error = %v", err)
	}
	if !reflect.DeepEqual(got, channels) {
		t.Fatalf("channels = %#v, want %#v", got, channels)
	}
}

func TestValidateExplicitSourceCoverageRejectsFilteredSource(t *testing.T) {
	channels := []ChannelInfo{{ChannelID: "group-visible"}}
	sources := []map[string]interface{}{
		{"source_id": "group-visible", "source_type": 1},
		{"source_id": "group-filtered", "source_type": 1},
	}

	if err := validateExplicitSourceCoverage(channels, sources, "user-1"); err == nil {
		t.Fatal("expected explicitly selected filtered source to be rejected")
	}
}

func TestValidateExplicitSourceCoverageAcceptsAllSources(t *testing.T) {
	channels := []ChannelInfo{{ChannelID: "group-a"}, {ChannelID: "group-b"}}
	sources := []map[string]interface{}{
		{"source_id": "group-a", "source_type": 1},
		{"source_id": "group-b", "source_type": 1},
	}

	if err := validateExplicitSourceCoverage(channels, sources, "user-1"); err != nil {
		t.Fatalf("validateExplicitSourceCoverage() error = %v", err)
	}
}

func TestFilterAvailableSpecifiedSourcesKeepsParticipantSubset(t *testing.T) {
	channels := []ChannelInfo{{ChannelID: "group-a"}}
	sources := []map[string]interface{}{
		{"source_id": "group-a", "source_type": 1, "source_name": "A"},
		{"source_id": "group-b", "source_type": 1, "source_name": "B"},
	}

	got := filterAvailableSpecifiedSources(channels, sources, "member-a")
	if len(got) != 1 || got[0]["source_id"] != "group-a" {
		t.Fatalf("filtered sources = %#v, want only group-a", got)
	}
	if len(sources) != 2 {
		t.Fatalf("input sources mutated: len = %d, want 2", len(sources))
	}
}
