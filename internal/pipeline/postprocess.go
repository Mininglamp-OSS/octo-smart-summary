// Package pipeline provides post-processing utilities for the summary workflow.
package pipeline

import (
	"log"
)

// DecideMessages determines the final message set based on filtering results.
//
// Decision logic:
//   - len(targetUIDs)==0           → all messages, no early message (no target)
//   - filtered non-empty           → filtered messages (normal narrow)
//   - filtered empty + targetUIDs==[creator] → nil + NoSelfMessagesMessage
//     (true first-person query, creator never spoke → tell the user plainly)
//   - filtered empty + other targets → all messages
//     (named someone who didn't speak in this source; whole source beats "no data")
func DecideMessages(targetUIDs []string, creatorUID string, all, filtered []Message) (msgs []Message, earlyMsg string) {
	if len(targetUIDs) == 0 {
		return all, ""
	}
	if len(filtered) > 0 {
		return filtered, ""
	}
	// Filtered is empty
	if len(targetUIDs) == 1 && targetUIDs[0] == creatorUID {
		return nil, NoSelfMessagesMessage
	}
	// Named other person(s) who didn't speak → fallback to all
	log.Printf("[postprocess] target UIDs %v had no messages, falling back to all %d messages", targetUIDs, len(all))
	return all, ""
}

// NoSelfMessagesMessage is returned when the user asks about their own messages
// but has no messages in the selected source(s).
const NoSelfMessagesMessage = "你在所选范围内没有发言记录。"

// NoRelevantContentMessage is returned when no relevant content is found.
const NoRelevantContentMessage = "在当前范围内未找到与主题相关的聊天记录。"
