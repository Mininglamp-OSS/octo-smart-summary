package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

// citationMarkerRE matches numbered citation markers ([1], [2], ...) in
// preview content. Same shape as the handler-side authority
// (internal/api/handler/agent_summary_citations.go citationMarkerRE).
var citationMarkerRE = regexp.MustCompile(`\[(\d+)\]`)

const (
	SummaryResultClarification        = "clarification"
	SummaryResultWorkflowConfirmation = "workflow_confirmation"
	SummaryResultWorkflowStarted      = "workflow_started"
	SummaryResultWorkflowCompleted    = "workflow_completed"
	SummaryResultAgentPreview         = "agent_preview"
	SummaryResultAgentRevision        = "agent_revision"
	SummaryResultExplanation          = "explanation"
	SummaryResultError                = "error"
)

var validSummaryResultTypes = map[string]struct{}{
	SummaryResultClarification:        {},
	SummaryResultWorkflowConfirmation: {},
	SummaryResultWorkflowStarted:      {},
	SummaryResultWorkflowCompleted:    {},
	SummaryResultAgentPreview:         {},
	SummaryResultAgentRevision:        {},
	SummaryResultExplanation:          {},
	SummaryResultError:                {},
}

// SummaryResponsePayload is the validated, typed shape carried by
// emit_summary_response. It is exported so the HTTP/application layer can
// perform authoritative checks (task ownership, proposal version, latest
// preview version) after the Agent loop returns it.
type SummaryResponsePayload struct {
	ResultType      string                     `json:"result_type"`
	Reply           string                     `json:"reply"`
	ExecutionTarget string                     `json:"execution_target,omitempty"`
	Workflow        *SummaryResponseWorkflow   `json:"workflow,omitempty"`
	Preview         *SummaryResponsePreview    `json:"preview,omitempty"`
	Confirmation    map[string]json.RawMessage `json:"confirmation,omitempty"`
	MissingFields   []string                   `json:"missing_fields,omitempty"`
}

type SummaryResponseWorkflow struct {
	TaskID int64  `json:"task_id"`
	Status string `json:"status"`
	Saved  bool   `json:"saved"`
}

type SummaryResponsePreview struct {
	Content         string                         `json:"content"`
	Version         int                            `json:"version"`
	ParentMessageID int64                          `json:"parent_message_id,omitempty"`
	Assumptions     []string                       `json:"assumptions,omitempty"`
	EffectiveScope  *SummaryResponseEffectiveScope `json:"effective_scope,omitempty"`
}

// SummaryResponseEffectiveScope is server-authored lineage for an Agent
// preview. It records an inferred channel/time window without mutating the UI
// scope hash. The terminal tool schema intentionally does not expose these
// fields to the model; the workspace handler stamps them after validation.
type SummaryResponseEffectiveScope struct {
	Channels  []SummaryResponseChannel  `json:"channels,omitempty"`
	TimeRange *SummaryResponseTimeRange `json:"time_range,omitempty"`
}

type SummaryResponseChannel struct {
	ChannelID   string `json:"channel_id"`
	ChannelType int    `json:"channel_type"`
	ChannelName string `json:"channel_name,omitempty"`
	IsArchived  bool   `json:"is_archived,omitempty"`
}

type SummaryResponseTimeRange struct {
	Start  string `json:"start"`
	End    string `json:"end"`
	Label  string `json:"label,omitempty"`
	Source string `json:"source,omitempty"`
}

type allowedSummaryResultTypesContextKey struct{}

type summaryCitationTrackingContextKey struct{}

type summaryCitationTrackingState struct {
	mu                  sync.RWMutex
	evidenceKeys        map[summaryCitationEvidenceKey]struct{}
	citationWindowKnown bool
	citationWindowMax   int64
}

type summaryCitationEvidenceKey struct {
	channelID  string
	messageSeq int64
}

// WithAllowedSummaryResultTypes constrains which result types the terminal tool
// may accept for this request. Absence means structural validation only; an
// explicitly empty allowlist denies every result. The application layer should
// derive this list from trusted route/session state, never from model output.
func WithAllowedSummaryResultTypes(ctx context.Context, resultTypes ...string) context.Context {
	allowed := make(map[string]struct{}, len(resultTypes))
	for _, resultType := range resultTypes {
		resultType = strings.TrimSpace(resultType)
		if resultType != "" {
			allowed[resultType] = struct{}{}
		}
	}
	return context.WithValue(ctx, allowedSummaryResultTypesContextKey{}, allowed)
}

// WithSummaryCitationTracking enables a run-scoped citation guard. Fetch tools
// mark the shared state only after they persist at least one message, so quiet
// chats may still produce an explicit "no messages" preview without inventing
// a citation. When evidence exists, an uncited terminal preview is rejected and
// the Agent loop gets a chance to repair it.
func WithSummaryCitationTracking(ctx context.Context) context.Context {
	return context.WithValue(ctx, summaryCitationTrackingContextKey{}, &summaryCitationTrackingState{
		evidenceKeys: make(map[summaryCitationEvidenceKey]struct{}),
	})
}

func markSummaryCitationEvidence(ctx context.Context, messages []pipeline.Message) {
	if len(messages) == 0 {
		return
	}
	if state, ok := ctx.Value(summaryCitationTrackingContextKey{}).(*summaryCitationTrackingState); ok && state != nil {
		state.mu.Lock()
		defer state.mu.Unlock()
		for _, message := range messages {
			state.evidenceKeys[summaryCitationEvidenceKey{
				channelID:  message.ChannelID,
				messageSeq: message.MessageSeq,
			}] = struct{}{}
		}
	}
}

// setSummaryCitationWindow records the exact marker space exposed to the model
// by summarize_chunk. It supersedes the fetched-message fallback because a
// frozen manifest may exclude post-freeze evidence or reuse older session rows.
func setSummaryCitationWindow(ctx context.Context, messages []pipeline.Message) {
	state, ok := ctx.Value(summaryCitationTrackingContextKey{}).(*summaryCitationTrackingState)
	if !ok || state == nil {
		return
	}
	var maxIndex int64
	for _, message := range messages {
		if int64(message.CitationIndex) > maxIndex {
			maxIndex = int64(message.CitationIndex)
		}
	}
	state.mu.Lock()
	state.citationWindowKnown = true
	state.citationWindowMax = maxIndex
	state.mu.Unlock()
}

func summaryCitationEvidenceWindow(ctx context.Context) (bool, int64) {
	state, ok := ctx.Value(summaryCitationTrackingContextKey{}).(*summaryCitationTrackingState)
	if !ok || state == nil {
		return false, 0
	}
	state.mu.RLock()
	defer state.mu.RUnlock()
	if state.citationWindowKnown {
		return len(state.evidenceKeys) > 0, state.citationWindowMax
	}
	return len(state.evidenceKeys) > 0, int64(len(state.evidenceKeys))
}

// citationMarkersWithinEvidence reports whether every [N] marker in content
// refers to an index in [1, evidenceCount] (the persisted evidence window) and
// at least one marker exists. Bounding by the evidence pool is what stops a
// stray prose "[1]" from spoofing coverage (review 5087740714 blocker 4).
func citationMarkersWithinEvidence(content string, evidenceCount int64) bool {
	markers := citationMarkerRE.FindAllStringSubmatch(content, -1)
	if len(markers) == 0 {
		return false
	}
	for _, marker := range markers {
		index, err := strconv.ParseInt(marker[1], 10, 64)
		if err != nil || index < 1 || index > evidenceCount {
			return false
		}
	}
	return true
}

// EmitSummaryResponseTool returns the only successful termination mechanism
// used by the summary_workspace profile. It validates shape only; authoritative
// workflow/proposal/preview state checks remain an application-layer concern.
func EmitSummaryResponseTool() (Tool, TerminalHandler) {
	schema := Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        "emit_summary_response",
			Description: "提交本轮智能总结的结构化结果并结束回合。必须单独调用；reply 是对话气泡，预览正文只能放在 preview.content。",
			Parameters: map[string]interface{}{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]interface{}{
					"result_type": map[string]interface{}{
						"type": "string",
						"enum": []string{
							SummaryResultClarification,
							SummaryResultWorkflowConfirmation,
							SummaryResultWorkflowStarted,
							SummaryResultWorkflowCompleted,
							SummaryResultAgentPreview,
							SummaryResultAgentRevision,
							SummaryResultExplanation,
							SummaryResultError,
						},
					},
					"reply": map[string]interface{}{
						"type":        "string",
						"description": "展示在对话气泡中的简短回复；不要把总结正文放在这里。",
					},
					"execution_target": map[string]interface{}{
						"type": "string",
						"enum": []string{"personal_workflow", "team_workflow", "agent_preview"},
					},
					"workflow": map[string]interface{}{
						"type":                 "object",
						"additionalProperties": false,
						"properties": map[string]interface{}{
							"task_id": map[string]interface{}{"type": "integer"},
							"status":  map[string]interface{}{"type": "string"},
							"saved":   map[string]interface{}{"type": "boolean"},
						},
						"required": []string{"task_id", "status", "saved"},
					},
					"preview": map[string]interface{}{
						"type":                 "object",
						"additionalProperties": false,
						"properties": map[string]interface{}{
							"content":           map[string]interface{}{"type": "string"},
							"version":           map[string]interface{}{"type": "integer"},
							"parent_message_id": map[string]interface{}{"type": "integer"},
							"assumptions": map[string]interface{}{
								"type":  "array",
								"items": map[string]interface{}{"type": "string"},
							},
						},
						"required": []string{"content", "version"},
					},
					"confirmation": map[string]interface{}{
						"type":        "object",
						"description": "服务端可验证的多人协作提案；不得表示任务已经创建。",
					},
					"missing_fields": map[string]interface{}{
						"type":  "array",
						"items": map[string]interface{}{"type": "string"},
					},
				},
				"required": []string{"result_type", "reply"},
			},
		},
	}

	handler := func(ctx context.Context, args json.RawMessage) (TerminalOutcome, error) {
		payload, canonical, err := parseSummaryResponsePayload(args)
		if err != nil {
			return TerminalOutcome{}, err
		}
		if allowed, ok := ctx.Value(allowedSummaryResultTypesContextKey{}).(map[string]struct{}); ok {
			if _, accepted := allowed[payload.ResultType]; !accepted {
				return TerminalOutcome{}, fmt.Errorf("result_type %q is not allowed for this request", payload.ResultType)
			}
		}
		hasEvidence, evidenceCount := summaryCitationEvidenceWindow(ctx)
		if hasEvidence &&
			(payload.ResultType == SummaryResultAgentPreview || payload.ResultType == SummaryResultAgentRevision) &&
			(payload.Preview == nil || !citationMarkersWithinEvidence(payload.Preview.Content, evidenceCount)) {
			// Evidence-bounded marker guard (review 5087740714 blocker 4):
			// every [N] must refer to an index inside the persisted evidence
			// window, and at least one marker must exist. This accepts a
			// legitimate preview citing only [2]/[3] and rejects prose whose
			// "[1]" is not citation syntax at all. Marker shape matches the
			// handler-side authority citationMarkerRE.
			return TerminalOutcome{}, errors.New("preview.content must include citation markers such as [1] for chat-backed summaries")
		}
		return TerminalOutcome{
			VisibleContent: payload.Reply,
			ResultType:     payload.ResultType,
			Payload:        canonical,
		}, nil
	}

	return schema, handler
}

func parseSummaryResponsePayload(args json.RawMessage) (SummaryResponsePayload, json.RawMessage, error) {
	var payload SummaryResponsePayload
	decoder := json.NewDecoder(bytes.NewReader(args))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return SummaryResponsePayload{}, nil, fmt.Errorf("parse emit_summary_response args: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return SummaryResponsePayload{}, nil, fmt.Errorf("parse emit_summary_response args: %w", err)
	}

	payload.ResultType = strings.TrimSpace(payload.ResultType)
	payload.Reply = strings.TrimSpace(payload.Reply)
	payload.ExecutionTarget = strings.TrimSpace(payload.ExecutionTarget)
	if _, ok := validSummaryResultTypes[payload.ResultType]; !ok {
		return SummaryResponsePayload{}, nil, fmt.Errorf("invalid result_type %q", payload.ResultType)
	}
	if payload.Reply == "" {
		return SummaryResponsePayload{}, nil, fmt.Errorf("reply is required")
	}
	if err := validateSummaryResponseShape(payload); err != nil {
		return SummaryResponsePayload{}, nil, err
	}
	canonical, err := json.Marshal(payload)
	if err != nil {
		return SummaryResponsePayload{}, nil, fmt.Errorf("marshal emit_summary_response payload: %w", err)
	}
	return payload, json.RawMessage(canonical), nil
}

func validateSummaryResponseShape(payload SummaryResponsePayload) error {
	hasWorkflow := payload.Workflow != nil
	hasPreview := payload.Preview != nil
	hasConfirmation := payload.Confirmation != nil
	if payload.Preview != nil && payload.Preview.EffectiveScope != nil {
		return fmt.Errorf("preview.effective_scope is reserved for the server")
	}

	switch payload.ResultType {
	case SummaryResultAgentPreview, SummaryResultAgentRevision:
		if payload.ExecutionTarget != "agent_preview" {
			return fmt.Errorf("execution_target must be agent_preview for %s", payload.ResultType)
		}
		if !hasPreview || strings.TrimSpace(payload.Preview.Content) == "" || payload.Preview.Version <= 0 {
			return fmt.Errorf("preview content and positive version are required for %s", payload.ResultType)
		}
		if payload.ResultType == SummaryResultAgentPreview && payload.Preview.ParentMessageID != 0 {
			return fmt.Errorf("agent_preview cannot have parent_message_id")
		}
		if payload.ResultType == SummaryResultAgentRevision && payload.Preview.ParentMessageID <= 0 {
			return fmt.Errorf("agent_revision requires parent_message_id")
		}
		if hasWorkflow || hasConfirmation {
			return fmt.Errorf("%s cannot include workflow or confirmation", payload.ResultType)
		}
	case SummaryResultWorkflowConfirmation:
		if payload.ExecutionTarget != "team_workflow" {
			return fmt.Errorf("execution_target must be team_workflow for workflow_confirmation")
		}
		if !hasConfirmation || len(payload.Confirmation) == 0 {
			return fmt.Errorf("confirmation is required for workflow_confirmation")
		}
		if hasWorkflow || hasPreview {
			return fmt.Errorf("workflow_confirmation cannot include workflow or preview")
		}
	case SummaryResultWorkflowStarted, SummaryResultWorkflowCompleted:
		if payload.ExecutionTarget != "personal_workflow" && payload.ExecutionTarget != "team_workflow" {
			return fmt.Errorf("workflow result requires a workflow execution_target")
		}
		if !hasWorkflow || payload.Workflow.TaskID <= 0 || strings.TrimSpace(payload.Workflow.Status) == "" {
			return fmt.Errorf("workflow task_id and status are required for %s", payload.ResultType)
		}
		if payload.ResultType == SummaryResultWorkflowCompleted && !payload.Workflow.Saved {
			return fmt.Errorf("workflow_completed requires saved=true")
		}
		if hasPreview || hasConfirmation {
			return fmt.Errorf("%s cannot include preview or confirmation", payload.ResultType)
		}
	case SummaryResultClarification, SummaryResultExplanation, SummaryResultError:
		if hasWorkflow || hasPreview || hasConfirmation {
			return fmt.Errorf("%s cannot include workflow, preview, or confirmation", payload.ResultType)
		}
	}
	return nil
}
