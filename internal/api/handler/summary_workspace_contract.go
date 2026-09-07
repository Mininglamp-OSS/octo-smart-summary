package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

const (
	summaryWorkspaceContractVersion = "2"
	summaryWorkspaceProfile         = "summary_workspace"

	workspaceResultClarification        = agent.SummaryResultClarification
	workspaceResultExplanation          = agent.SummaryResultExplanation
	workspaceResultWorkflowConfirm      = agent.SummaryResultWorkflowConfirmation
	workspaceResultWorkflowStarted      = agent.SummaryResultWorkflowStarted
	workspaceResultWorkflowCompleted    = agent.SummaryResultWorkflowCompleted
	workspaceResultAgentPreview         = agent.SummaryResultAgentPreview
	workspaceResultAgentRevision        = agent.SummaryResultAgentRevision
	workspaceResultError                = agent.SummaryResultError
	workspaceActionConfirmWorkflow      = "confirm_workflow"
	workspaceActionSavePreview          = "save_preview"
	workspaceActionViewSummary          = "view_summary"
	workspaceActionViewProgress         = "view_progress"
	workspaceActionContinueChat         = "continue_chat"
	workspaceSnapshotVersion            = 1
	maxSummaryWorkspaceSelectedChannels = agent.MaxWorkspaceSelectedChannels
	maxSummaryWorkspaceParticipants     = 30
	maxSummaryWorkspaceReferencedTask   = 20
	maxSummaryWorkspaceIDLength         = 256
	maxSummaryWorkspaceLabelLength      = 256
	maxSummaryWorkspaceRequirement      = 8192
	summaryWorkspaceInputUser           = "user"
	summaryWorkspaceInputTemplate       = "template"
	summaryWorkspaceInputSystemIntent   = "system_intent"
)

var errInvalidSummaryWorkspaceContext = errors.New("invalid summary workspace context")

type summaryWorkspaceChannel struct {
	ChatID     string `json:"chat_id"`
	ChatType   string `json:"chat_type"`
	Name       string `json:"name"`
	IsArchived bool   `json:"is_archived,omitempty"`
}

type summaryWorkspaceParticipant struct {
	UserID   string `json:"user_id"`
	UserName string `json:"user_name,omitempty"`
}

type summaryWorkspaceTemplate struct {
	TemplateID  string `json:"template_id"`
	Label       string `json:"label"`
	Requirement string `json:"requirement"`
	Version     int    `json:"version,omitempty"`
}

type summaryWorkspaceTimeRange struct {
	Start  string `json:"start"`
	End    string `json:"end"`
	Label  string `json:"label"`
	Source string `json:"source"`
}

const (
	summaryWorkspaceTimeRangeSourcePicker       = "picker"
	summaryWorkspaceTimeRangeSourceDefault      = "default"
	summaryWorkspaceTimeRangeSourceConversation = "conversation"
)

// summaryWorkspaceContext is the complete, server-authoritative scope echoed
// on every turn and History response. Slice fields are normalized to [] rather
// than nil because the frontend protocol decoder intentionally rejects absent
// collection fields.
type summaryWorkspaceContext struct {
	SelectedChannels  []summaryWorkspaceChannel     `json:"selected_channels"`
	Participants      []summaryWorkspaceParticipant `json:"participants"`
	Template          *summaryWorkspaceTemplate     `json:"template"`
	TimeRange         *summaryWorkspaceTimeRange    `json:"time_range"`
	ReferencedTaskIDs []int64                       `json:"referenced_task_ids"`
}

type summaryWorkspacePreview struct {
	MessageID        int64    `json:"message_id"`
	ResultType       string   `json:"result_type"`
	ScopeVersion     int      `json:"scope_version"`
	ArtifactVersion  int      `json:"artifact_version"`
	SnapshotVersion  int      `json:"snapshot_version"`
	Content          string   `json:"content"`
	Assumptions      []string `json:"assumptions"`
	AvailableActions []string `json:"available_actions"`
}

type summaryWorkspaceProposal struct {
	MessageID        int64                         `json:"message_id"`
	ScopeVersion     int                           `json:"scope_version"`
	ProposalVersion  int                           `json:"proposal_version"`
	ProposalToken    string                        `json:"proposal_token"`
	Participants     []summaryWorkspaceParticipant `json:"participants"`
	Requirement      string                        `json:"requirement"`
	TemplateLabel    string                        `json:"template_label,omitempty"`
	TimeRangeLabel   string                        `json:"time_range_label,omitempty"`
	TimeRange        *summaryWorkspaceTimeRange    `json:"time_range,omitempty"`
	AvailableActions []string                      `json:"available_actions"`
}

type summaryWorkspaceWorkflow struct {
	MessageID        int64    `json:"message_id"`
	ResultType       string   `json:"result_type"`
	ScopeVersion     int      `json:"scope_version"`
	TaskID           int64    `json:"task_id"`
	TaskTitle        string   `json:"task_title"`
	Status           int      `json:"status"`
	Scope            string   `json:"scope"`
	Saved            bool     `json:"saved"`
	ParticipantCount *int     `json:"participant_count,omitempty"`
	AvailableActions []string `json:"available_actions"`
}

type summaryWorkspaceState struct {
	ScopeVersion    int                       `json:"scope_version"`
	SummaryContext  summaryWorkspaceContext   `json:"summary_context"`
	CurrentPreview  *summaryWorkspacePreview  `json:"current_preview"`
	PendingProposal *summaryWorkspaceProposal `json:"pending_proposal"`
	Workflow        *summaryWorkspaceWorkflow `json:"workflow"`
}

type summaryWorkspaceTurn struct {
	ContractVersion  string                `json:"contract_version"`
	SessionID        string                `json:"session_id"`
	MessageID        int64                 `json:"message_id"`
	ResultType       string                `json:"result_type"`
	Reply            string                `json:"reply"`
	ScopeVersion     int                   `json:"scope_version"`
	ArtifactVersion  int                   `json:"artifact_version,omitempty"`
	RunID            string                `json:"run_id,omitempty"`
	AvailableActions []string              `json:"available_actions"`
	State            summaryWorkspaceState `json:"state"`
}

type summaryWorkspaceHistoryMessage struct {
	ID               int64                    `json:"id"`
	Role             string                   `json:"role"`
	Content          string                   `json:"content"`
	ResultType       string                   `json:"result_type,omitempty"`
	ScopeVersion     int                      `json:"scope_version"`
	ArtifactVersion  int                      `json:"artifact_version,omitempty"`
	AvailableActions []string                 `json:"available_actions,omitempty"`
	Preview          *summaryWorkspacePreview `json:"preview,omitempty"`
}

type summaryWorkspaceHistory struct {
	ContractVersion string                           `json:"contract_version"`
	SessionID       string                           `json:"session_id"`
	Messages        []summaryWorkspaceHistoryMessage `json:"messages"`
	State           summaryWorkspaceState            `json:"state"`
}

type summaryWorkspaceConfirmRequest struct {
	ProposalToken  string                  `json:"proposal_token"`
	ScopeVersion   int                     `json:"scope_version"`
	SummaryContext summaryWorkspaceContext `json:"summary_context"`
}

func emptySummaryWorkspaceContext() summaryWorkspaceContext {
	return summaryWorkspaceContext{
		SelectedChannels:  []summaryWorkspaceChannel{},
		Participants:      []summaryWorkspaceParticipant{},
		ReferencedTaskIDs: []int64{},
	}
}

func normalizeSummaryWorkspaceContext(in summaryWorkspaceContext) (summaryWorkspaceContext, error) {
	out := emptySummaryWorkspaceContext()
	if len(in.SelectedChannels) > maxSummaryWorkspaceSelectedChannels {
		return out, fmt.Errorf("%w: 最多选择 %d 个聊天", errInvalidSummaryWorkspaceContext, maxSummaryWorkspaceSelectedChannels)
	}
	seenChannels := make(map[string]struct{}, len(in.SelectedChannels))
	for _, channel := range in.SelectedChannels {
		channel.ChatID = strings.TrimSpace(channel.ChatID)
		channel.ChatType = strings.ToLower(strings.TrimSpace(channel.ChatType))
		channel.Name = strings.TrimSpace(channel.Name)
		if channel.ChatID == "" || channel.Name == "" || len([]rune(channel.ChatID)) > maxSummaryWorkspaceIDLength || len([]rune(channel.Name)) > maxSummaryWorkspaceLabelLength {
			return out, fmt.Errorf("%w: selected channel id and name are required", errInvalidSummaryWorkspaceContext)
		}
		switch channel.ChatType {
		case "group", "direct", "thread":
		default:
			return out, fmt.Errorf("%w: invalid chat_type", errInvalidSummaryWorkspaceContext)
		}
		key := channel.ChatType + ":" + channel.ChatID
		if _, exists := seenChannels[key]; exists {
			continue
		}
		seenChannels[key] = struct{}{}
		out.SelectedChannels = append(out.SelectedChannels, channel)
	}

	if len(in.Participants) > maxSummaryWorkspaceParticipants {
		return out, fmt.Errorf("%w: 最多选择 %d 位参与者", errInvalidSummaryWorkspaceContext, maxSummaryWorkspaceParticipants)
	}
	seenParticipants := make(map[string]struct{}, len(in.Participants))
	for _, participant := range in.Participants {
		participant.UserID = strings.TrimSpace(participant.UserID)
		participant.UserName = strings.TrimSpace(participant.UserName)
		if participant.UserID == "" || len([]rune(participant.UserID)) > maxSummaryWorkspaceIDLength || len([]rune(participant.UserName)) > maxSummaryWorkspaceLabelLength {
			return out, fmt.Errorf("%w: participant user_id is required", errInvalidSummaryWorkspaceContext)
		}
		if _, exists := seenParticipants[participant.UserID]; exists {
			continue
		}
		seenParticipants[participant.UserID] = struct{}{}
		out.Participants = append(out.Participants, participant)
	}

	if in.Template != nil {
		template := *in.Template
		template.TemplateID = strings.TrimSpace(template.TemplateID)
		template.Label = strings.TrimSpace(template.Label)
		template.Requirement = strings.TrimSpace(template.Requirement)
		if template.TemplateID == "" || template.Label == "" || template.Requirement == "" || template.Version < 0 ||
			len([]rune(template.TemplateID)) > maxSummaryWorkspaceIDLength ||
			len([]rune(template.Label)) > maxSummaryWorkspaceLabelLength ||
			len([]rune(template.Requirement)) > maxSummaryWorkspaceRequirement {
			return out, fmt.Errorf("%w: invalid template", errInvalidSummaryWorkspaceContext)
		}
		out.Template = &template
	}

	if in.TimeRange != nil {
		timeRange := *in.TimeRange
		timeRange.Start = strings.TrimSpace(timeRange.Start)
		timeRange.End = strings.TrimSpace(timeRange.End)
		timeRange.Label = strings.TrimSpace(timeRange.Label)
		timeRange.Source = strings.TrimSpace(timeRange.Source)
		if timeRange.Source == "" {
			timeRange.Source = summaryWorkspaceTimeRangeSourcePicker
		}
		start, startErr := time.Parse(time.RFC3339, timeRange.Start)
		end, endErr := time.Parse(time.RFC3339, timeRange.End)
		if startErr != nil || endErr != nil || !end.After(start) || timeRange.Label == "" || len([]rune(timeRange.Label)) > maxSummaryWorkspaceLabelLength ||
			(timeRange.Source != summaryWorkspaceTimeRangeSourcePicker && timeRange.Source != summaryWorkspaceTimeRangeSourceDefault && timeRange.Source != summaryWorkspaceTimeRangeSourceConversation) {
			return out, fmt.Errorf("%w: invalid time_range", errInvalidSummaryWorkspaceContext)
		}
		if end.Sub(start) > time.Duration(pipeline.MaxTimeRangeDays)*24*time.Hour {
			return out, fmt.Errorf("%w: time_range exceeds %d days", errInvalidSummaryWorkspaceContext, pipeline.MaxTimeRangeDays)
		}
		out.TimeRange = &timeRange
	}

	if len(in.ReferencedTaskIDs) > maxSummaryWorkspaceReferencedTask {
		return out, fmt.Errorf("%w: referenced_task_ids exceeds %d", errInvalidSummaryWorkspaceContext, maxSummaryWorkspaceReferencedTask)
	}
	seenTasks := make(map[int64]struct{}, len(in.ReferencedTaskIDs))
	for _, taskID := range in.ReferencedTaskIDs {
		if taskID <= 0 {
			return out, fmt.Errorf("%w: referenced task id must be positive", errInvalidSummaryWorkspaceContext)
		}
		if _, exists := seenTasks[taskID]; exists {
			continue
		}
		seenTasks[taskID] = struct{}{}
		out.ReferencedTaskIDs = append(out.ReferencedTaskIDs, taskID)
	}
	return out, nil
}

func marshalSummaryWorkspaceContext(context summaryWorkspaceContext) ([]byte, string, error) {
	data, err := json.Marshal(context)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
}

func summaryWorkspaceRequestHash(action, message string, scopeVersion int, scopeHash string, inputOrigin ...string) string {
	origin := ""
	if len(inputOrigin) > 0 {
		origin = strings.TrimSpace(inputOrigin[0])
	}
	payload, _ := json.Marshal(struct {
		Action       string `json:"action"`
		Message      string `json:"message"`
		ScopeVersion int    `json:"scope_version"`
		ScopeHash    string `json:"scope_hash"`
		InputOrigin  string `json:"input_origin,omitempty"`
	}{
		Action:       strings.TrimSpace(action),
		Message:      strings.TrimSpace(message),
		ScopeVersion: scopeVersion,
		ScopeHash:    scopeHash,
		InputOrigin:  origin,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func workspaceActionsForResult(resultType string, saved bool) []string {
	switch resultType {
	case workspaceResultWorkflowConfirm:
		return []string{workspaceActionConfirmWorkflow, workspaceActionContinueChat}
	case workspaceResultWorkflowStarted:
		return []string{workspaceActionViewProgress, workspaceActionContinueChat}
	case workspaceResultWorkflowCompleted:
		return []string{workspaceActionViewSummary}
	case workspaceResultAgentPreview, workspaceResultAgentRevision:
		if saved {
			return []string{workspaceActionContinueChat}
		}
		return []string{workspaceActionSavePreview, workspaceActionContinueChat}
	case workspaceResultClarification, workspaceResultExplanation:
		return []string{workspaceActionContinueChat}
	default:
		return []string{}
	}
}
