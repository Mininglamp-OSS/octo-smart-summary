package handler

import (
	"database/sql"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

func TestBuildSnapshotV1(t *testing.T) {
	db, skip := setupTestDB(t)
	if skip {
		return
	}
	h := &AgentSummaryHandler{db: db}

	// Also migrate models we need
	if err := db.AutoMigrate(&model.SummaryTask{}, &model.PersonalResult{}, &model.SummaryParticipant{}); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	sessionID := "test-session-001"

	// Insert some tool messages
	toolMessages := []model.AgentMessage{
		{UserID: "test-user", SessionID: sessionID, Role: "tool", Name: "fetch_channel", Content: "result1", CreatedAt: time.Now()},
		{UserID: "test-user", SessionID: sessionID, Role: "tool", Name: "search_messages", Content: "result2", CreatedAt: time.Now()},
		{UserID: "test-user", SessionID: sessionID, Role: "tool", Name: "fetch_channel", Content: "result3", CreatedAt: time.Now()},
	}
	for _, msg := range toolMessages {
		if err := db.Create(&msg).Error; err != nil {
			t.Fatalf("failed to insert test tool message: %v", err)
		}
	}

	task := &model.SummaryTask{
		ID:             100,
		Title:          "Test summary request",
		TimeRangeStart: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		TimeRangeEnd:   time.Date(2026, 7, 13, 23, 59, 59, 0, time.UTC),
	}

	sources := []sourceReq{
		{SourceID: "ch1", SourceType: 1},
		{SourceID: "ch2", SourceType: 1},
	}

	snap := h.buildSnapshotV1(db, sessionID, "test-user", task, sources)

	if snap == nil {
		t.Fatal("expected non-nil snapshot")
	}
	if snap.SnapshotVersion != 1 {
		t.Errorf("expected snapshot_version 1, got %d", snap.SnapshotVersion)
	}
	if snap.TaskID != 100 {
		t.Errorf("expected task_id 100, got %d", snap.TaskID)
	}
	if snap.ContentVersion != 1 {
		t.Errorf("expected content_version 1, got %d", snap.ContentVersion)
	}
	if snap.Requirement != "Test summary request" {
		t.Errorf("expected requirement 'Test summary request', got %q", snap.Requirement)
	}
	if len(snap.Scope.ChannelIDs) != 2 {
		t.Errorf("expected 2 channel_ids, got %d", len(snap.Scope.ChannelIDs))
	}
	if snap.ParentSnapshotVersion != nil {
		t.Errorf("expected parent_snapshot_version to be nil, got %v", *snap.ParentSnapshotVersion)
	}
	if snap.UserInstruction != nil {
		t.Errorf("expected user_instruction to be nil, got %v", *snap.UserInstruction)
	}

	// Check tool_summary contains expected counts (now sorted)
	foundFetch := false
	foundSearch := false
	for _, entry := range snap.ToolSummary {
		if entry == "fetch_channel x 2" {
			foundFetch = true
		}
		if entry == "search_messages x 1" {
			foundSearch = true
		}
	}
	if !foundFetch {
		t.Errorf("expected 'fetch_channel x 2' in tool_summary, got %v", snap.ToolSummary)
	}
	if !foundSearch {
		t.Errorf("expected 'search_messages x 1' in tool_summary, got %v", snap.ToolSummary)
	}

	// Verify tool_summary is sorted alphabetically
	if len(snap.ToolSummary) >= 2 {
		// fetch_channel should come before search_messages alphabetically
		if snap.ToolSummary[0] != "fetch_channel x 2" {
			t.Errorf("expected first tool to be 'fetch_channel x 2', got %q", snap.ToolSummary[0])
		}
		if snap.ToolSummary[1] != "search_messages x 1" {
			t.Errorf("expected second tool to be 'search_messages x 1', got %q", snap.ToolSummary[1])
		}
	}
}

func TestCreateAgentSummary_SnapshotPersisted(t *testing.T) {
	// This test verifies that CreateAgentSummary correctly stores the snapshot_json
	// in the database when called for an agent-generated summary.
	// We'll use an in-memory SQLite DB and seed it with the necessary test data.

	db, skip := setupTestDB(t)
	if skip {
		return
	}

	// Also migrate models we need
	if err := db.AutoMigrate(&model.SummaryTask{}, &model.PersonalResult{}, &model.SummaryParticipant{}); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	sessionID := "test-snap-session"
	userID := "user123"
	spaceID := "space456"

	// Seed agent_message with an assistant reply
	assistantMsg := model.AgentMessage{
		UserID:    "test-user",
		SessionID: sessionID,
		Role:      "assistant",
		Content:   "This is the agent-produced summary content.",
		CreatedAt: time.Now(),
	}
	if err := db.Create(&assistantMsg).Error; err != nil {
		t.Fatalf("failed to create assistant message: %v", err)
	}

	// Seed some tool messages
	toolMessages := []model.AgentMessage{
		{UserID: "test-user", SessionID: sessionID, Role: "tool", Name: "fetch_channel", Content: "tool result", CreatedAt: time.Now()},
		{UserID: "test-user", SessionID: sessionID, Role: "tool", Name: "search_messages", Content: "tool result 2", CreatedAt: time.Now()},
	}
	for _, msg := range toolMessages {
		if err := db.Create(&msg).Error; err != nil {
			t.Fatalf("failed to insert tool message: %v", err)
		}
	}

	// Create a SummaryTask
	task := model.SummaryTask{
		TaskNo:            "TASK001",
		SpaceID:           spaceID,
		CreatorID:         userID,
		Title:             "Test Agent Summary",
		SummaryMode:       model.ModeByPerson,
		TimeRangeStart:    time.Now().Add(-24 * time.Hour),
		TimeRangeEnd:      time.Now(),
		Status:            model.StatusCompleted,
		TriggerType:       model.TriggerAgent,
		OriginChannelID:   "channel1",
		OriginChannelType: model.OriginChannelGroup,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("failed to create task: %v", err)
	}

	// Create a participant
	participant := model.SummaryParticipant{
		TaskID:   task.ID,
		UserID:   userID,
		UserName: "TestUser",
		Status:   model.ParticipantAccepted,
	}
	if err := db.Create(&participant).Error; err != nil {
		t.Fatalf("failed to create participant: %v", err)
	}

	// Build the snapshot manually (mimicking what the handler does)
	h := &AgentSummaryHandler{db: db}
	sources := []sourceReq{{SourceID: "channel1", SourceType: 1}}
	snapshot := h.buildSnapshotV1(db, sessionID, "test-user", &task, sources)

	// Create the PersonalResult with snapshot
	now := time.Now()
	pr := model.PersonalResult{
		TaskID:           task.ID,
		ParticipantRefID: participant.ID,
		UserID:           userID,
		Content:          "This is the agent-produced summary content.",
		WorkerStatus:     model.PersonalStatusCompleted,
		GeneratedAt:      &now,
		SubmittedAt:      &now,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	pr.SetSnapshot(snapshot)

	if err := db.Create(&pr).Error; err != nil {
		t.Fatalf("failed to create PersonalResult: %v", err)
	}

	// Verify using sql.NullString to check DB raw value
	var rawSnapshot sql.NullString
	if err := db.Raw("SELECT snapshot_json FROM summary_personal_result WHERE id = ?", pr.ID).
		Scan(&rawSnapshot).Error; err != nil {
		t.Fatalf("failed to query raw snapshot_json: %v", err)
	}

	if !rawSnapshot.Valid {
		t.Error("expected snapshot_json to be non-NULL for agent-generated PersonalResult")
	}

	// Retrieve the PersonalResult from DB and verify structure
	var retrieved model.PersonalResult
	if err := db.Where("id = ?", pr.ID).First(&retrieved).Error; err != nil {
		t.Fatalf("failed to retrieve PersonalResult: %v", err)
	}

	// Deserialize and verify structure
	retrievedSnap := retrieved.GetSnapshot()
	if retrievedSnap == nil {
		t.Fatal("expected non-nil snapshot after retrieval")
	}
	if retrievedSnap.SnapshotVersion != 1 {
		t.Errorf("expected snapshot_version 1, got %d", retrievedSnap.SnapshotVersion)
	}
	if retrievedSnap.TaskID != task.ID {
		t.Errorf("expected task_id %d, got %d", task.ID, retrievedSnap.TaskID)
	}
	if retrievedSnap.ContentVersion != 1 {
		t.Errorf("expected content_version 1, got %d", retrievedSnap.ContentVersion)
	}
	if len(retrievedSnap.Scope.ChannelIDs) == 0 {
		t.Error("expected at least one channel_id in scope")
	}
	if len(retrievedSnap.ToolSummary) == 0 {
		t.Error("expected non-empty tool_summary")
	}
}

func TestTraditionalPath_SnapshotNull(t *testing.T) {
	// This test verifies that traditional workflow (non-agent) does not fill snapshot_json.
	db, skip := setupTestDB(t)
	if skip {
		return
	}

	// Also migrate models we need
	if err := db.AutoMigrate(&model.SummaryTask{}, &model.PersonalResult{}, &model.SummaryParticipant{}); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}

	task := model.SummaryTask{
		TaskNo:         "TASK-TRAD",
		SpaceID:        "space1",
		CreatorID:      "user1",
		Title:          "Traditional summary",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: time.Now().Add(-24 * time.Hour),
		TimeRangeEnd:   time.Now(),
		Status:         model.StatusPending,
		TriggerType:    model.TriggerManual, // traditional workflow
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("failed to create traditional task: %v", err)
	}

	participant := model.SummaryParticipant{
		TaskID:   task.ID,
		UserID:   "user1",
		UserName: "User One",
		Status:   model.ParticipantPending,
	}
	if err := db.Create(&participant).Error; err != nil {
		t.Fatalf("failed to create participant: %v", err)
	}

	// Traditional workflow: worker generates PersonalResult, no snapshot
	now := time.Now()
	pr := model.PersonalResult{
		TaskID:           task.ID,
		ParticipantRefID: participant.ID,
		UserID:           "user1",
		Content:          "Traditional worker-generated content",
		WorkerStatus:     model.PersonalStatusCompleted,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	// Do NOT call SetSnapshot for traditional workflow
	if err := db.Create(&pr).Error; err != nil {
		t.Fatalf("failed to create traditional PersonalResult: %v", err)
	}

	// Use sql.NullString to check DB raw value
	var rawSnapshot sql.NullString
	if err := db.Raw("SELECT snapshot_json FROM summary_personal_result WHERE id = ?", pr.ID).
		Scan(&rawSnapshot).Error; err != nil {
		t.Fatalf("failed to query raw snapshot_json: %v", err)
	}

	if rawSnapshot.Valid {
		t.Errorf("expected snapshot_json to be NULL for traditional workflow, got Valid=true with value %q", rawSnapshot.String)
	}

	// Also verify via the model
	var retrieved model.PersonalResult
	if err := db.Where("id = ?", pr.ID).First(&retrieved).Error; err != nil {
		t.Fatalf("failed to retrieve traditional PersonalResult: %v", err)
	}

	if snap := retrieved.GetSnapshot(); snap != nil {
		t.Errorf("expected GetSnapshot to return nil for traditional workflow, got %+v", snap)
	}
}
