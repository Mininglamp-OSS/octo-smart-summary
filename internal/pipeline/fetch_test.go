package pipeline

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

func TestNormalizeDMChannelID(t *testing.T) {
	tests := []struct {
		name        string
		channelID   string
		selfUID     string
		channelType int
		want        string
	}{
		{
			name:        "peer UID only, peer > self",
			channelID:   "5904fca8",
			selfUID:     "2c56cb",
			channelType: 1,
			want:        "5904fca8@2c56cb",
		},
		{
			name:        "peer UID only, peer < self",
			channelID:   "2c56cb",
			selfUID:     "5904fca8",
			channelType: 1,
			want:        "5904fca8@2c56cb",
		},
		{
			name:        "already has @ normalized by crc32",
			channelID:   "a@b",
			selfUID:     "x",
			channelType: 1,
			want:        "a@b",
		},
		{
			name:        "already has @ reordered by crc32",
			channelID:   "b@a",
			selfUID:     "x",
			channelType: 1,
			want:        "a@b",
		},
		{
			name:        "non-DM unchanged",
			channelID:   "group123",
			selfUID:     "x",
			channelType: 2,
			want:        "group123",
		},
		{
			name:        "non-DM type 0 unchanged",
			channelID:   "something",
			selfUID:     "x",
			channelType: 0,
			want:        "something",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeDMChannelID(tt.channelID, tt.selfUID, tt.channelType)
			if got != tt.want {
				t.Errorf("NormalizeDMChannelID(%q, %q, %d) = %q, want %q",
					tt.channelID, tt.selfUID, tt.channelType, got, tt.want)
			}
		})
	}
}

// StorageChannelTypeToSourceType must be the exact inverse of
// mapFrontendSourceType on the three known channel kinds, and must reject
// unknown storage values (0 / WuKongIM-reserved 3,4 / anything else) so the
// worker never persists a garbage source_type into summary_source.
func TestStorageChannelTypeToSourceType(t *testing.T) {
	tests := []struct {
		name    string
		storage int
		want    int
		wantOK  bool
	}{
		{"DM 1 -> SourceDirect 3", model.ChannelTypeDM, model.SourceDirect, true},
		{"group 2 -> SourceGroup 1", model.ChannelTypeGroup, model.SourceGroup, true},
		{"thread 5 -> SourceThread 2", model.ChannelTypeThread, model.SourceThread, true},
		{"unknown 0", 0, 0, false},
		{"unknown 3 (WuKongIM reserved)", 3, 0, false},
		{"unknown 4 (WuKongIM reserved)", 4, 0, false},
		{"unknown 99", 99, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := StorageChannelTypeToSourceType(tt.storage)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("StorageChannelTypeToSourceType(%d) = (%d, %v), want (%d, %v)",
					tt.storage, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// Round-trip: every known frontend source type mapped to storage and back
// must land on the original value. Guards against the two enums drifting
// apart silently (they intentionally use different numbers).
func TestStorageChannelTypeToSourceType_RoundTrip(t *testing.T) {
	for _, frontend := range []int{model.SourceGroup, model.SourceThread, model.SourceDirect} {
		storage := mapFrontendSourceType(frontend)
		back, ok := StorageChannelTypeToSourceType(storage)
		if !ok || back != frontend {
			t.Errorf("round trip: frontend %d -> storage %d -> (%d, %v), want (%d, true)",
				frontend, storage, back, ok, frontend)
		}
	}
}
