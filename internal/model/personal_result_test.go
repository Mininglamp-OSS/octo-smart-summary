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

func TestPersonalResult_GetSetSnapshot(t *testing.T) {
	pr := PersonalResult{}

	// Test nil snapshot
	pr.SetSnapshot(nil)
	if pr.SnapshotJSON != nil {
		t.Errorf("expected empty SnapshotJSON for nil snapshot, got %v", pr.SnapshotJSON)
	}
	if snap := pr.GetSnapshot(); snap != nil {
		t.Errorf("expected GetSnapshot to return nil for empty SnapshotJSON, got %+v", snap)
	}

	// Test valid snapshot
	snap := &Snapshot{
		SnapshotVersion: 1,
		TaskID:          100,
		ContentVersion:  1,
		Requirement:     "test requirement",
		Scope: SnapshotScope{
			ChannelIDs:   []string{"ch1", "ch2"},
			ChannelNames: []string{},
			TimeRange: TimeRangeJSON{
				Start: "2026-01-01T00:00:00Z",
				End:   "2026-01-02T00:00:00Z",
			},
		},
		ToolSummary:           []string{"fetch_channel x 2"},
		DataFreshnessNote:     "test note",
		ParentSnapshotVersion: nil,
		UserInstruction:       nil,
	}

	pr.SetSnapshot(snap)
	if pr.SnapshotJSON == nil || *pr.SnapshotJSON == "" {
		t.Error("expected non-empty SnapshotJSON after SetSnapshot")
	}

	retrieved := pr.GetSnapshot()
	if retrieved == nil {
		t.Fatal("expected non-nil snapshot from GetSnapshot")
	}
	if retrieved.SnapshotVersion != 1 {
		t.Errorf("expected snapshot_version 1, got %d", retrieved.SnapshotVersion)
	}
	if retrieved.TaskID != 100 {
		t.Errorf("expected task_id 100, got %d", retrieved.TaskID)
	}
	if retrieved.ContentVersion != 1 {
		t.Errorf("expected content_version 1, got %d", retrieved.ContentVersion)
	}
	if retrieved.Requirement != "test requirement" {
		t.Errorf("expected requirement 'test requirement', got %q", retrieved.Requirement)
	}
	if len(retrieved.Scope.ChannelIDs) != 2 {
		t.Errorf("expected 2 channel_ids, got %d", len(retrieved.Scope.ChannelIDs))
	}
	if len(retrieved.ToolSummary) != 1 || retrieved.ToolSummary[0] != "fetch_channel x 2" {
		t.Errorf("expected tool_summary ['fetch_channel x 2'], got %v", retrieved.ToolSummary)
	}
}

func TestSnapshot_SerializationSymmetry(t *testing.T) {
	// Verify that SetSnapshot followed by GetSnapshot returns equivalent data
	original := &Snapshot{
		SnapshotVersion: 1,
		TaskID:          999,
		ContentVersion:  2,
		Requirement:     "summarize recent conversations",
		Scope: SnapshotScope{
			ChannelIDs:   []string{"id1", "id2", "id3"},
			ChannelNames: []string{"Channel One", "Channel Two", "Channel Three"},
			TimeRange: TimeRangeJSON{
				Start: "2026-07-01T00:00:00+08:00",
				End:   "2026-07-13T23:59:59+08:00",
			},
		},
		ToolSummary:           []string{"search_messages x 5", "fetch_channel x 3"},
		DataFreshnessNote:     "data freshness note",
		ParentSnapshotVersion: intPtr(1),
		UserInstruction:       strPtr("make it shorter"),
	}

	pr := PersonalResult{}
	pr.SetSnapshot(original)

	retrieved := pr.GetSnapshot()
	if retrieved == nil {
		t.Fatal("expected non-nil snapshot")
	}

	if retrieved.SnapshotVersion != original.SnapshotVersion {
		t.Errorf("snapshot_version mismatch: expected %d, got %d", original.SnapshotVersion, retrieved.SnapshotVersion)
	}
	if retrieved.TaskID != original.TaskID {
		t.Errorf("task_id mismatch: expected %d, got %d", original.TaskID, retrieved.TaskID)
	}
	if retrieved.ContentVersion != original.ContentVersion {
		t.Errorf("content_version mismatch: expected %d, got %d", original.ContentVersion, retrieved.ContentVersion)
	}
	if retrieved.ParentSnapshotVersion == nil || *retrieved.ParentSnapshotVersion != 1 {
		t.Errorf("parent_snapshot_version mismatch: expected 1, got %v", retrieved.ParentSnapshotVersion)
	}
	if retrieved.UserInstruction == nil || *retrieved.UserInstruction != "make it shorter" {
		t.Errorf("user_instruction mismatch: expected 'make it shorter', got %v", retrieved.UserInstruction)
	}
}

func intPtr(v int) *int       { return &v }
func strPtr(s string) *string { return &s }
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
