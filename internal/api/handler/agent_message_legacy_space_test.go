package handler

import (
	"context"
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

func TestAgentMessageRepoLegacySpaceIsolation(t *testing.T) {
	db, skip := setupResolveTestDB(t)
	if skip {
		return
	}
	const sessionID = "shared-legacy-workspace-session"

	rows := []model.AgentMessage{
		{UserID: "test-user", SessionID: sessionID, Role: "user", Content: "legacy question"},
		{SpaceID: "space-1", UserID: "test-user", SessionID: sessionID, Role: "assistant", Content: "workspace reply"},
		{UserID: "test-user", SessionID: sessionID, Role: "assistant", Content: "legacy reply"},
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatalf("seed messages: %v", err)
	}

	repo := newAgentMessageRepo(db)
	history, err := repo.LoadHistory(context.Background(), sessionID, "test-user")
	if err != nil {
		t.Fatalf("load Legacy history: %v", err)
	}
	if len(history) != 2 || history[0].Content != "legacy question" || history[1].Content != "legacy reply" {
		t.Fatalf("Legacy history crossed workspace boundary: %+v", history)
	}

	if err := repo.AppendMessages(context.Background(), sessionID, "test-user", []agent.Message{{Role: "user", Content: "legacy follow-up"}}); err != nil {
		t.Fatalf("append Legacy message: %v", err)
	}
	var appended model.AgentMessage
	if err := db.Where("user_id = ? AND session_id = ? AND content = ?", "test-user", sessionID, "legacy follow-up").Take(&appended).Error; err != nil {
		t.Fatalf("reload appended Legacy message: %v", err)
	}
	if appended.SpaceID != legacyAgentMessageSpaceID {
		t.Fatalf("appended space_id=%q, want Legacy namespace", appended.SpaceID)
	}
}

func TestLoadAgentMessageForSaveLegacySpaceIsolation(t *testing.T) {
	db, skip := setupResolveTestDB(t)
	if skip {
		return
	}
	const sessionID = "shared-save-session"

	legacy := model.AgentMessage{UserID: "test-user", SessionID: sessionID, Role: "assistant", Content: "legacy draft"}
	if err := db.Create(&legacy).Error; err != nil {
		t.Fatalf("seed Legacy draft: %v", err)
	}
	workspace := model.AgentMessage{SpaceID: "space-1", UserID: "test-user", SessionID: sessionID, Role: "assistant", Content: "newer workspace preview"}
	if err := db.Create(&workspace).Error; err != nil {
		t.Fatalf("seed workspace preview: %v", err)
	}

	got, err := loadAgentMessageForSave(db, sessionID, "test-user", 0)
	if err != nil {
		t.Fatalf("load Legacy fallback: %v", err)
	}
	if got.ID != legacy.ID {
		t.Fatalf("Legacy fallback selected message id=%d, want %d", got.ID, legacy.ID)
	}

	if _, err := loadAgentMessageForSave(db, sessionID, "test-user", workspace.ID); !errors.Is(err, errNoAgentOutput) {
		t.Fatalf("targeted workspace message error=%v, want errNoAgentOutput", err)
	}
}
