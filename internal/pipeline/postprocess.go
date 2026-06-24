// Package pipeline provides post-processing utilities for the summary workflow.
package pipeline

import (
	"log"
)

// PostProcessOptions configures the post-processing behavior.
type PostProcessOptions struct {
	ContextWindow int    // Context window size for FilterWithContext
	CreatorUID    string // The UID of the user who initiated the summary request
}

// PostProcessResult contains the output of post-processing.
type PostProcessResult struct {
	Messages    []Message         // Final messages after filtering and name resolution
	NameMap     map[string]string // UID → display name mapping
	TargetUIDs  []string          // Target person UIDs (passed through)
	EarlyReturn string            // Non-empty if processing should return early (e.g., user has no messages)
}

// UserNameResolver is a function type for resolving user display names from UIDs.
// This allows the caller to inject the database lookup logic.
type UserNameResolver func(messages []Message) map[string]string

// PostProcess is the unified post-processing entry point.
//
// It performs the following steps:
//  1. Resolve sender display names via the provided resolver
//  2. Apply context-aware filtering if targetUIDs is non-empty
//  3. Decide final message set (narrow vs fallback to all)
//  4. Fill SenderName field in all messages
//
// Note: ResolveTopicTarget (LLM call) should be done BEFORE calling this function.
// PostProcess only performs deterministic processing, no LLM calls.
func PostProcess(
	messages []Message,
	targetUIDs []string,
	opts PostProcessOptions,
	resolver UserNameResolver,
) *PostProcessResult {
	// 1. Resolve user names
	var nameMap map[string]string
	if resolver != nil {
		nameMap = resolver(messages)
	} else {
		nameMap = make(map[string]string)
	}

	// 2. Context filtering + decide final messages
	var finalMessages []Message
	var earlyReturn string

	if len(targetUIDs) > 0 {
		filtered := FilterWithContext(messages, targetUIDs, opts.ContextWindow)
		log.Printf("[postprocess] FilterWithContext: %d → %d messages (targets=%v, window=%d)",
			len(messages), len(filtered), targetUIDs, opts.ContextWindow)

		finalMessages, earlyReturn = DecideMessages(targetUIDs, opts.CreatorUID, messages, filtered)
		if earlyReturn != "" {
			log.Printf("[postprocess] early return: creator %s had no messages", opts.CreatorUID)
			return &PostProcessResult{
				Messages:    nil,
				NameMap:     nameMap,
				TargetUIDs:  targetUIDs,
				EarlyReturn: earlyReturn,
			}
		}
	} else {
		finalMessages = messages
		log.Printf("[postprocess] no target UIDs, using all %d messages", len(messages))
	}

	// 3. Fill sender names
	for i := range finalMessages {
		if name, ok := nameMap[finalMessages[i].SenderUID]; ok {
			finalMessages[i].SenderName = name
		} else {
			finalMessages[i].SenderName = finalMessages[i].SenderUID
		}
	}

	return &PostProcessResult{
		Messages:    finalMessages,
		NameMap:     nameMap,
		TargetUIDs:  targetUIDs,
		EarlyReturn: "",
	}
}

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
	return all, ""
}

// NoSelfMessagesMessage is returned when the user asks about their own messages
// but has no messages in the selected source(s).
const NoSelfMessagesMessage = "你在所选范围内没有发言记录。"

// NoRelevantContentMessage is returned when no relevant content is found.
const NoRelevantContentMessage = "在当前范围内未找到与主题相关的聊天记录。"
