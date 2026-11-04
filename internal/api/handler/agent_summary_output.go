package handler

import (
	"encoding/json"
	"strings"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
)

// normalizeLegacySummaryContent accepts only the complete envelope requested by
// older summary_refine prompts. Ordinary text, JSON examples with other shapes,
// and incomplete responses are kept verbatim. Citation metadata is deliberately
// ignored: saves derive it from authorized evidence or referenced summaries.
func normalizeLegacySummaryContent(reply string) string {
	raw := strings.TrimSpace(reply)
	if strings.HasPrefix(raw, "```") {
		opening, rest, ok := strings.Cut(raw, "\n")
		if !ok || (strings.TrimSpace(opening) != "```json" && strings.TrimSpace(opening) != "```") || !strings.HasSuffix(rest, "\n```") {
			return reply
		}
		raw = strings.TrimSpace(strings.TrimSuffix(rest, "\n```"))
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &fields) != nil || len(fields) != 2 {
		return reply
	}
	var content *string
	var citations []json.RawMessage
	if json.Unmarshal(fields["content"], &content) != nil || content == nil || strings.TrimSpace(*content) == "" {
		return reply
	}
	if json.Unmarshal(fields["citations"], &citations) != nil || citations == nil {
		return reply
	}
	return *content
}

// Normalize before both persistence and delivery so restoring a conversation
// shows the same body as its original response. Do not rewrite tool traces or
// generic chat replies, where JSON can itself be the requested deliverable.
func normalizeLegacySummaryReply(profile, reply string, messages []agent.Message) string {
	if profile != "summary" && profile != "summary_refine" {
		return reply
	}
	content := normalizeLegacySummaryContent(reply)
	if content != reply && len(messages) > 0 {
		last := &messages[len(messages)-1]
		if last.Role == "assistant" && len(last.ToolCalls) == 0 && last.Content == reply {
			last.Content = content
		}
	}
	return content
}
