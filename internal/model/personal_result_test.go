package model

import (
	"testing"
)

func TestPersonalResultTableName(t *testing.T) {
	pr := PersonalResult{}
	if pr.TableName() != "summary_personal_result" {
		t.Errorf("expected table name 'summary_personal_result', got '%s'", pr.TableName())
	}
}

func TestParticipantStatusLabel(t *testing.T) {
	tests := []struct {
		status   int
		expected string
	}{
		{ParticipantPending, "pending"},
		{ParticipantAccepted, "accepted"},
		{ParticipantDeclined, "declined"},
		{ParticipantProcessing, "processing"},
		{ParticipantCompleted, "completed"},
		{ParticipantSubmitted, "submitted"},
		{99, "unknown"},
	}

	for _, tt := range tests {
		got := ParticipantStatusLabel(tt.status)
		if got != tt.expected {
			t.Errorf("ParticipantStatusLabel(%d) = %q, want %q", tt.status, got, tt.expected)
		}
	}
}

func TestPersonalStatusConstants(t *testing.T) {
	if PersonalStatusPending != 0 {
		t.Error("PersonalStatusPending should be 0")
	}
	if PersonalStatusProcessing != 1 {
		t.Error("PersonalStatusProcessing should be 1")
	}
	if PersonalStatusCompleted != 2 {
		t.Error("PersonalStatusCompleted should be 2")
	}
	if PersonalStatusFailed != 3 {
		t.Error("PersonalStatusFailed should be 3")
	}
}

func TestWorkerTriggerRequestTypes(t *testing.T) {
	req := WorkerTriggerRequest{
		Type:             "personal_summary",
		TaskID:           123,
		ParticipantRefID: 456,
	}
	if req.Type != "personal_summary" {
		t.Error("expected type personal_summary")
	}
	if req.TaskID != 123 {
		t.Error("expected task_id 123")
	}
	if req.ParticipantRefID != 456 {
		t.Error("expected participant_ref_id 456")
	}
}

func TestWorkflowStageConstants(t *testing.T) {
	tests := []string{
		WorkflowStageUnderstandQuestion,
		WorkflowStageFindRelevantChats,
		WorkflowStageFilterUsefulContent,
		WorkflowStageAnalyzeChatContent,
		WorkflowStageGenerateSummary,
	}
	for _, stage := range tests {
		if stage == "" {
			t.Fatal("workflow stage must not be empty")
		}
	}
}
