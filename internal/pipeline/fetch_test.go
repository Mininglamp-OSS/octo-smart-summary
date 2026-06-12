package pipeline

import "testing"

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
			want:        "2c56cb@5904fca8",
		},
		{
			name:        "peer UID only, peer < self",
			channelID:   "2c56cb",
			selfUID:     "5904fca8",
			channelType: 1,
			want:        "2c56cb@5904fca8",
		},
		{
			name:        "already has @ reorder",
			channelID:   "a@b",
			selfUID:     "x",
			channelType: 1,
			want:        "a@b",
		},
		{
			name:        "already correct order",
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
