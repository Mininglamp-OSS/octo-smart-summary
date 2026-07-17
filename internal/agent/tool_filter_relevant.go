package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

// FilterRelevantTool filters cached messages by relevance to a topic or participants.
func FilterRelevantTool() (Tool, Handler) {
	schema := Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "filter_relevant",
			Description: "从缓存消息中过滤出与主题或参与者相关的内容。复用 pipeline.FilterMessagesByRelevance 逻辑。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"messages_handle": map[string]interface{}{
						"type":        "string",
						"description": "消息缓存句柄",
					},
					"topic": map[string]interface{}{
						"type":        "string",
						"description": "总结主题，用于语义相关性过滤",
					},
					"participant_uids": map[string]interface{}{
						"type":        "array",
						"items":       map[string]interface{}{"type": "string"},
						"description": "参与者 UID 列表，用于过滤指定人的消息",
					},
					"participant_names": map[string]interface{}{
						"type":        "array",
						"items":       map[string]interface{}{"type": "string"},
						"description": "参与者姓名列表，用于名称匹配",
					},
				},
				"required": []string{"messages_handle"},
			},
		},
	}

	handler := func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			MessagesHandle   string   `json:"messages_handle"`
			Topic            string   `json:"topic,omitempty"`
			ParticipantUIDs  []string `json:"participant_uids,omitempty"`
			ParticipantNames []string `json:"participant_names,omitempty"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}

		// Extract uid from context
		uidVal := ctx.Value(ContextKeyUID)
		uid, ok := uidVal.(string)
		if !ok || uid == "" {
			return "", fmt.Errorf("missing user identity in context")
		}

		messages := messageCache.Retrieve(req.MessagesHandle, uid)
		if messages == nil {
			return "", fmt.Errorf("invalid messages_handle or access denied: %s", req.MessagesHandle)
		}

		filtered := pipeline.FilterMessagesByRelevance(messages, req.Topic, req.ParticipantUIDs, req.ParticipantNames)

		newHandle := messageCache.Store(filtered, uid)

		// Also persist to evidence table so citation recovery survives the
		// 30-min in-memory TTL. #161 P1-B (yujiawei): evidence is the sole
		// discovery source for citation building, so a write failure would
		// make this handle uncitable for the entire session — escalate as
		// a tool-level error.
		summaryDB, _, _, _ := GetSummaryDeps()
		if err := PersistEvidence(summaryDB, ctx, newHandle, filtered); err != nil {
			return "", fmt.Errorf("persist evidence: %w", err)
		}

		result := map[string]interface{}{
			"original_count":  len(messages),
			"filtered_count":  len(filtered),
			"messages_handle": newHandle,
		}
		data, err := json.Marshal(result)
		if err != nil {
			return "", fmt.Errorf("marshal result: %w", err)
		}
		return string(data), nil
	}

	return schema, handler
}
