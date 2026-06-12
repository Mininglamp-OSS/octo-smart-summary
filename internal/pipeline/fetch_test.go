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
			name:        "peer UID only, peer CRC32 > self CRC32",
			channelID:   "5904fca8",
			selfUID:     "2c56cb",
			channelType: 1,
			want:        "5904fca8@2c56cb", // crc32(5904fca8)=0xc51744c4 > crc32(2c56cb)=0x2c9da339
		},
		{
			name:        "peer UID only, peer CRC32 < self CRC32",
			channelID:   "2c56cb",
			selfUID:     "5904fca8",
			channelType: 1,
			want:        "5904fca8@2c56cb", // same pair, same result
		},
		{
			name:        "already has @, a@b already correct CRC32 order",
			channelID:   "a@b",
			selfUID:     "x",
			channelType: 1,
			want:        "a@b", // crc32(a)=0xe8b7be43 > crc32(b)=0x71beeff9
		},
		{
			name:        "already has @, b@a reordered to a@b",
			channelID:   "b@a",
			selfUID:     "x",
			channelType: 1,
			want:        "a@b", // crc32(a) > crc32(b), so a comes first
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
