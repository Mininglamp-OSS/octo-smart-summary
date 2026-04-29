package worker

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

// citationRe matches [n] citation markers (1-3 digits). Markdown links [text](url)
// are not false-matched because link text is never pure digits.
var citationRe = regexp.MustCompile(`\[(\d{1,3})\]`)
var multiSpaceRe = regexp.MustCompile(`[ \t]{2,}`)
var emptyLineRe = regexp.MustCompile(`(?m)^[ \t]*$\n`)

// extractCitationIndexes extracts all [n] citation indexes from text.
func extractCitationIndexes(text string) []int {
	matches := citationRe.FindAllStringSubmatch(text, -1)
	var indexes []int
	seen := make(map[int]bool)
	for _, m := range matches {
		if n, err := strconv.Atoi(m[1]); err == nil && !seen[n] {
			indexes = append(indexes, n)
			seen[n] = true
		}
	}
	sort.Ints(indexes)
	return indexes
}

// buildCitations builds a citation list from the summary text and original messages.
// Only messages actually referenced in the text are included.
// allMessages is the full message set (all channels) used for context lookup.
// nameMap maps sender UIDs to display names; if nil or a UID is missing, the UID is used as-is.
func buildCitations(text string, messages []pipeline.Message, allMessages []pipeline.Message, nameMap map[string]string) []model.Citation {
	indexes := extractCitationIndexes(text)
	if len(indexes) == 0 {
		return []model.Citation{}
	}

	indexSet := make(map[int]bool, len(indexes))
	for _, idx := range indexes {
		indexSet[idx] = true
	}

	channelMsgMap := buildChannelMessageMap(allMessages)

	var citations []model.Citation
	for _, msg := range messages {
		if indexSet[msg.CitationIndex] {
			content := truncateRunes(msg.Content, 200)

			sender := msg.SenderUID
			if nameMap != nil {
				if name, ok := nameMap[msg.SenderUID]; ok && name != "" {
					sender = name
				}
			}

			before, after := findContext(msg, channelMsgMap, nameMap, 3)

			citations = append(citations, model.Citation{
				Index:         msg.CitationIndex,
				Sender:        sender,
				Content:       content,
				SentAt:        msg.SendTime,
				Source:        msg.SourceName,
				ChannelID:     msg.ChannelID,
				ChannelType:   msg.ChannelType,
				MessageSeq:    msg.MessageSeq,
				ContextBefore: before,
				ContextAfter:  after,
			})
		}
	}
	if citations == nil {
		return []model.Citation{}
	}
	return citations
}

func truncateRunes(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}

func buildChannelMessageMap(allMessages []pipeline.Message) map[string][]pipeline.Message {
	m := make(map[string][]pipeline.Message)
	for _, msg := range allMessages {
		m[msg.ChannelID] = append(m[msg.ChannelID], msg)
	}
	return m
}

func findContext(target pipeline.Message, channelMsgMap map[string][]pipeline.Message, nameMap map[string]string, n int) ([]model.ContextMsg, []model.ContextMsg) {
	channelMsgs, ok := channelMsgMap[target.ChannelID]
	if !ok {
		return nil, nil
	}

	targetIdx := -1
	for i, msg := range channelMsgs {
		if msg.MessageSeq == target.MessageSeq {
			targetIdx = i
			break
		}
	}
	if targetIdx < 0 {
		return nil, nil
	}

	var before []model.ContextMsg
	start := targetIdx - n
	if start < 0 {
		start = 0
	}
	for i := start; i < targetIdx; i++ {
		before = append(before, toContextMsg(channelMsgs[i], nameMap))
	}

	var after []model.ContextMsg
	end := targetIdx + n + 1
	if end > len(channelMsgs) {
		end = len(channelMsgs)
	}
	for i := targetIdx + 1; i < end; i++ {
		after = append(after, toContextMsg(channelMsgs[i], nameMap))
	}

	return before, after
}

func toContextMsg(msg pipeline.Message, nameMap map[string]string) model.ContextMsg {
	sender := msg.SenderUID
	if nameMap != nil {
		if name, ok := nameMap[msg.SenderUID]; ok && name != "" {
			sender = name
		}
	}
	return model.ContextMsg{
		Sender:     sender,
		Content:    truncateRunes(msg.Content, 200),
		SentAt:     msg.SendTime,
		MessageSeq: msg.MessageSeq,
	}
}

// dedupCitations merges citations that share the same (sender, content) pair.
// For each group of duplicates, the smallest index is kept as the representative.
// All occurrences of duplicate indexes in text are replaced with the representative,
// and consecutive identical markers (e.g. [1][1][1]) are collapsed to a single one.
func dedupCitations(text string, citations []model.Citation) (string, []model.Citation) {
	if len(citations) == 0 {
		return text, citations
	}

	// Group by (sender, content) — keep the smallest index as representative.
	type key struct{ sender, content string }
	mainIdx := make(map[key]int) // key -> smallest index
	remap := make(map[int]int)   // oldIdx -> mainIdx

	for _, c := range citations {
		k := key{c.Sender, c.Content}
		if existing, ok := mainIdx[k]; ok {
			if c.Index < existing {
				// New one is smaller; remap old main to new.
				remap[existing] = c.Index
				mainIdx[k] = c.Index
			} else {
				remap[c.Index] = existing
			}
		} else {
			mainIdx[k] = c.Index
		}
	}

	newText := text

	if len(remap) > 0 {
		// Replace remapped indexes in text.
		newText = citationRe.ReplaceAllStringFunc(newText, func(match string) string {
			sub := citationRe.FindStringSubmatch(match)
			n, _ := strconv.Atoi(sub[1])
			if target, ok := remap[n]; ok {
				return fmt.Sprintf("[%d]", target)
			}
			return match
		})

		// Collapse consecutive identical markers: [1][1][1] -> [1]
		newText = collapseConsecutiveMarkers(newText)
	}

	// Global dedup: for each [n], keep only the first occurrence.
	// Runs after remap so duplicates created by remap are also caught.
	seen := make(map[string]bool)
	newText = citationRe.ReplaceAllStringFunc(newText, func(match string) string {
		if seen[match] {
			return ""
		}
		seen[match] = true
		return match
	})
	newText = multiSpaceRe.ReplaceAllString(newText, " ")
	newText = emptyLineRe.ReplaceAllString(newText, "")
	newText = strings.TrimSpace(newText)

	if len(remap) == 0 {
		return newText, citations
	}

	// Build deduplicated citation list (only keep non-remapped).
	kept := make(map[int]bool)
	for _, idx := range mainIdx {
		kept[idx] = true
	}
	var result []model.Citation
	for _, c := range citations {
		if kept[c.Index] {
			result = append(result, c)
			delete(kept, c.Index) // avoid duplicates if same index appears twice
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Index < result[j].Index })

	if result == nil {
		return newText, []model.Citation{}
	}
	return newText, result
}

// collapseConsecutiveMarkers collapses runs of identical consecutive citation
// markers (optionally separated by whitespace) into a single occurrence.
// Example: "[1][1][1]" -> "[1]", "[2] [2]" -> "[2]". Different markers are
// preserved: "[1][2][1]" -> "[1][2][1]".
//
// Go's regexp engine (RE2) has no backreferences, so we process manually.
func collapseConsecutiveMarkers(text string) string {
	locs := citationRe.FindAllStringIndex(text, -1)
	if len(locs) < 2 {
		return text
	}

	type span struct{ start, end int }
	var toRemove []span

	for i := 0; i < len(locs); i++ {
		current := text[locs[i][0]:locs[i][1]]
		j := i + 1
		for j < len(locs) {
			// Only allow whitespace between markers.
			between := text[locs[j-1][1]:locs[j][0]]
			if !isOnlyWhitespace(between) {
				break
			}
			next := text[locs[j][0]:locs[j][1]]
			if next != current {
				break
			}
			// Remove whitespace + this duplicate marker.
			toRemove = append(toRemove, span{locs[j-1][1], locs[j][1]})
			j++
		}
		i = j - 1
	}

	if len(toRemove) == 0 {
		return text
	}

	// Apply removals from the end to keep earlier indexes valid.
	result := text
	for k := len(toRemove) - 1; k >= 0; k-- {
		r := toRemove[k]
		result = result[:r.start] + result[r.end:]
	}
	return result
}

func isOnlyWhitespace(s string) bool {
	for _, r := range s {
		if r != ' ' && r != '\t' && r != '\n' && r != '\r' {
			return false
		}
	}
	return true
}
