package pipeline

import (
	"fmt"
	"hash/crc32"
)

// MessageTable returns the sharded message table name for a given channel ID.
// Uses CRC32 IEEE for consistent table sharding:
//
//	crc32.ChecksumIEEE([]byte(channelID)) % uint32(tableCount)
//
// Table naming: "message" (index 0) or "message1"..."message4" (index 1-4).
func MessageTable(channelID string, tableCount int) string {
	if tableCount <= 0 {
		tableCount = 5
	}
	idx := crc32.ChecksumIEEE([]byte(channelID)) % uint32(tableCount)
	if idx == 0 {
		return "message"
	}
	return fmt.Sprintf("message%d", idx)
}
