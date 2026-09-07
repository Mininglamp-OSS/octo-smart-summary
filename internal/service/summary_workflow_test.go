//go:build cgo

package service

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newSummaryWorkflowTestService(t *testing.T) (*SummaryWorkflowService, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(
		&model.SummaryTask{},
		&model.SummarySource{},
		&model.SummaryParticipant{},
		&model.PersonalResult{},
		&model.SummaryWorkflowIdempotency{},
	); err != nil {
		t.Fatalf("migrate sqlite: %v", err)
	}
	return NewSummaryWorkflowService(db, nil, 31, 31), db
}

func baseSummaryWorkflowInput() LegacyCreateSummaryWorkflowInput {
	return LegacyCreateSummaryWorkflowInput{
		ActorID: "creator",
		SpaceID: "space-1",
		Title:   "Weekly summary",
		Topic:   "Summarize delivery status",
		Sources: []SummaryWorkflowSource{{SourceType: model.SourceGroup, SourceID: "group-1"}},
	}
}

func TestSummaryWorkflowCreatePersonal(t *testing.T) {
	svc, db := newSummaryWorkflowTestService(t)

	got, err := svc.CreateFromLegacyHTTP(context.Background(), baseSummaryWorkflowInput())
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if got.Replayed {
		t.Fatal("first create must not be marked replayed")
	}
	if got.Target != SummaryWorkflowPersonal {
		t.Fatalf("target = %q, want %q", got.Target, SummaryWorkflowPersonal)
	}
	if got.WorkerTrigger == nil || got.WorkerTrigger.TaskID != got.Task.ID || got.WorkerTrigger.ParticipantRefID == 0 {
		t.Fatalf("worker trigger = %#v, want committed task and creator participant", got.WorkerTrigger)
	}

	var taskCount, sourceCount, participantCount, personalCount int64
	db.Model(&model.SummaryTask{}).Count(&taskCount)
	db.Model(&model.SummarySource{}).Count(&sourceCount)
	db.Model(&model.SummaryParticipant{}).Count(&participantCount)
	db.Model(&model.PersonalResult{}).Count(&personalCount)
	if taskCount != 1 || sourceCount != 1 || participantCount != 1 || personalCount != 1 {
		t.Fatalf("row counts task/source/participant/personal = %d/%d/%d/%d, want 1/1/1/1", taskCount, sourceCount, participantCount, personalCount)
	}
}

func TestLegacyWorkflowDefaultRangeRemains31DaysWhenMaximumIs90Days(t *testing.T) {
	_, db := newSummaryWorkflowTestService(t)
	svc := NewSummaryWorkflowService(db, nil, 31, 90)

	got, err := svc.CreateFromLegacyHTTP(context.Background(), baseSummaryWorkflowInput())
	if err != nil {
		t.Fatalf("CreateFromLegacyHTTP() error: %v", err)
	}
	want := time.Duration(legacySummaryDefaultTimeRangeDays) * 24 * time.Hour
	if duration := got.Task.TimeRangeEnd.Sub(got.Task.TimeRangeStart); duration < want-time.Second || duration > want+time.Second {
		t.Fatalf("legacy default range = %s, want %s even when maximum is 90 days", duration, want)
	}
}

func TestLegacyWorkflowDefaultRangeUsesConfiguredValue(t *testing.T) {
	_, db := newSummaryWorkflowTestService(t)
	svc := NewSummaryWorkflowService(db, nil, 14, 90)

	got, err := svc.CreateFromLegacyHTTP(context.Background(), baseSummaryWorkflowInput())
	if err != nil {
		t.Fatalf("CreateFromLegacyHTTP() error: %v", err)
	}
	want := 14 * 24 * time.Hour
	if duration := got.Task.TimeRangeEnd.Sub(got.Task.TimeRangeStart); duration < want-time.Second || duration > want+time.Second {
		t.Fatalf("legacy default range = %s, want configured %s", duration, want)
	}
}

func TestLegacyWorkflowRejectsRangeBeyondConfiguredMaximum(t *testing.T) {
	_, db := newSummaryWorkflowTestService(t)
	svc := NewSummaryWorkflowService(db, nil, 31, 31)
	in := baseSummaryWorkflowInput()
	in.TimeRange = &SummaryWorkflowTimeRange{
		Start: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC),
	}

	_, err := svc.CreateFromLegacyHTTP(context.Background(), in)
	var bizErr *BizError
	if !errors.As(err, &bizErr) || bizErr.Code != 40002 {
		t.Fatalf("CreateFromLegacyHTTP() error = %v, want code 40002", err)
	}
}

func TestSummaryWorkflowCreateTeamDeduplicatesParticipants(t *testing.T) {
	svc, db := newSummaryWorkflowTestService(t)
	in := baseSummaryWorkflowInput()
	in.Participants = []SummaryWorkflowParticipant{
		{UserID: "p1", UserName: "P1"},
		{UserID: "p1", UserName: "duplicate"},
		{UserID: "p2", UserName: "P2"},
		{UserID: "creator", UserName: "creator duplicate"},
	}

	got, err := svc.CreateFromLegacyHTTP(context.Background(), in)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if got.Target != SummaryWorkflowTeam {
		t.Fatalf("target = %q, want %q", got.Target, SummaryWorkflowTeam)
	}
	var participants []model.SummaryParticipant
	if err := db.Order("user_id").Find(&participants).Error; err != nil {
		t.Fatal(err)
	}
	if len(participants) != 3 {
		t.Fatalf("participants = %d, want creator + 2 unique collaborators", len(participants))
	}
	if participants[0].UserID != "creator" || participants[0].Status != model.ParticipantAccepted {
		t.Fatalf("creator participant = %#v, want accepted creator", participants[0])
	}
}

func TestSummaryWorkflowOriginAutofillsSource(t *testing.T) {
	svc, db := newSummaryWorkflowTestService(t)
	in := baseSummaryWorkflowInput()
	in.Sources = nil
	in.OriginChannelID = "group-origin"
	in.OriginChannelType = model.OriginChannelGroup

	got, err := svc.CreateFromLegacyHTTP(context.Background(), in)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if got.Inferred {
		t.Fatal("origin channel is a concrete source, not an inferred scope")
	}
	var source model.SummarySource
	if err := db.First(&source).Error; err != nil {
		t.Fatal(err)
	}
	if source.SourceID != "group-origin" || source.SourceType != model.SourceGroup {
		t.Fatalf("source = %#v, want origin group", source)
	}
}

func TestSummaryWorkflowValidationFailureWritesNothing(t *testing.T) {
	svc, db := newSummaryWorkflowTestService(t)
	in := baseSummaryWorkflowInput()
	in.ActorID = ""

	_, err := svc.CreateFromLegacyHTTP(context.Background(), in)
	var bizErr *BizError
	if !errors.As(err, &bizErr) || bizErr.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("error = %v, want unauthorized BizError", err)
	}
	var count int64
	db.Model(&model.SummaryTask{}).Count(&count)
	if count != 0 {
		t.Fatalf("task count = %d, want 0", count)
	}
}

func TestSummaryWorkflowIdempotencyReplayAndMismatch(t *testing.T) {
	svc, db := newSummaryWorkflowTestService(t)
	in := baseSummaryWorkflowInput()
	in.IdempotencyKey = "summary-create-001"

	first, err := svc.CreateFromLegacyHTTP(context.Background(), in)
	if err != nil {
		t.Fatalf("first Create() error: %v", err)
	}
	second, err := svc.CreateFromLegacyHTTP(context.Background(), in)
	if err != nil {
		t.Fatalf("replay Create() error: %v", err)
	}
	if !second.Replayed || second.Task.ID != first.Task.ID || second.WorkerTrigger == nil {
		t.Fatalf("replay = %#v, want same task and a recoverable pending worker trigger", second)
	}

	var taskCount int64
	db.Model(&model.SummaryTask{}).Count(&taskCount)
	if taskCount != 1 {
		t.Fatalf("task count after replay = %d, want 1", taskCount)
	}

	in.Topic = "different request"
	_, err = svc.CreateFromLegacyHTTP(context.Background(), in)
	var mismatch *SummaryWorkflowIdempotencyError
	if !errors.As(err, &mismatch) {
		t.Fatalf("mismatch error = %v, want SummaryWorkflowIdempotencyError", err)
	}
	if mismatch.ExistingTaskID != first.Task.ID || mismatch.BizError.Code != WorkflowIdempotencyMismatchCode || mismatch.Reason != "request_mismatch" {
		t.Fatalf("mismatch = %#v, want existing task %d and code %d", mismatch, first.Task.ID, WorkflowIdempotencyMismatchCode)
	}
}

func TestSummaryWorkflowIdempotencyCreatorChangeIsRequestMismatch(t *testing.T) {
	svc, _ := newSummaryWorkflowTestService(t)
	in := baseSummaryWorkflowInput()
	in.IdempotencyKey = "summary-create-creator-change"

	first, err := svc.CreateFromLegacyHTTP(context.Background(), in)
	if err != nil {
		t.Fatalf("first Create() error: %v", err)
	}
	in.CreatorID = "legacy-delegated-creator"
	_, err = svc.CreateFromLegacyHTTP(context.Background(), in)
	var mismatch *SummaryWorkflowIdempotencyError
	if !errors.As(err, &mismatch) {
		t.Fatalf("creator-change error = %v, want SummaryWorkflowIdempotencyError", err)
	}
	if mismatch.ExistingTaskID != first.Task.ID || mismatch.Reason != "request_mismatch" || mismatch.RecoveryAction != "open_existing_summary" {
		t.Fatalf("creator-change mismatch = %#v, want request_mismatch for task %d", mismatch, first.Task.ID)
	}
}

func TestSummaryWorkflowIdempotencyBindingReadsBackWinner(t *testing.T) {
	_, db := newSummaryWorkflowTestService(t)
	winner := model.SummaryWorkflowIdempotency{
		SpaceID: "space-1", UserID: "creator", IdempotencyKey: "race-key", RequestHash: "same", TaskID: 11,
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return createSummaryWorkflowIdempotencyBinding(tx, &winner)
	}); err != nil {
		t.Fatalf("create winner binding: %v", err)
	}
	loser := model.SummaryWorkflowIdempotency{
		SpaceID: "space-1", UserID: "creator", IdempotencyKey: "race-key", RequestHash: "same", TaskID: 12,
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return createSummaryWorkflowIdempotencyBinding(tx, &loser)
	}); !errors.Is(err, errSummaryWorkflowIdempotencyRace) {
		t.Fatalf("loser error = %v, want idempotency race", err)
	}
}

func TestSummaryWorkflowDeletedTaskKeepsIdempotencyTombstone(t *testing.T) {
	svc, db := newSummaryWorkflowTestService(t)
	in := baseSummaryWorkflowInput()
	in.IdempotencyKey = "deleted-task-key"
	first, err := svc.CreateFromLegacyHTTP(context.Background(), in)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if err := db.Model(&model.SummaryTask{}).Where("id = ?", first.Task.ID).Update("deleted_at", time.Now()).Error; err != nil {
		t.Fatalf("soft delete task: %v", err)
	}

	_, err = svc.CreateFromLegacyHTTP(context.Background(), in)
	var stale *SummaryWorkflowIdempotencyError
	if !errors.As(err, &stale) || stale.Reason != "deleted_summary" || stale.ExistingTaskID != first.Task.ID {
		t.Fatalf("stale error = %#v (%v), want deleted task conflict", stale, err)
	}
	var count int64
	db.Model(&model.SummaryTask{}).Count(&count)
	if count != 1 {
		t.Fatalf("task count after deleted replay = %d, want 1", count)
	}
}

func TestSummaryWorkflowIdempotencyIsScopedByUserAndSpace(t *testing.T) {
	svc, db := newSummaryWorkflowTestService(t)
	base := baseSummaryWorkflowInput()
	base.IdempotencyKey = "shared-client-key"
	first, err := svc.CreateFromLegacyHTTP(context.Background(), base)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}

	otherUser := base
	otherUser.ActorID = "creator-2"
	otherUser.CreatorID = "creator-2"
	second, err := svc.CreateFromLegacyHTTP(context.Background(), otherUser)
	if err != nil {
		t.Fatalf("other-user create: %v", err)
	}
	otherSpace := base
	otherSpace.SpaceID = "space-2"
	third, err := svc.CreateFromLegacyHTTP(context.Background(), otherSpace)
	if err != nil {
		t.Fatalf("other-space create: %v", err)
	}
	if first.Task.ID == second.Task.ID || first.Task.ID == third.Task.ID || second.Task.ID == third.Task.ID {
		t.Fatalf("scoped idempotency returned duplicate ids: %d, %d, %d", first.Task.ID, second.Task.ID, third.Task.ID)
	}
	var count int64
	db.Model(&model.SummaryTask{}).Count(&count)
	if count != 3 {
		t.Fatalf("task count = %d, want 3 independently scoped creates", count)
	}
}

func TestValidSummaryWorkflowIdempotencyKey(t *testing.T) {
	for _, tt := range []struct {
		key  string
		want bool
	}{
		{"request-1", true},
		{"", false},
		{" leading-space", false},
		{"bad/key", false},
	} {
		if got := ValidSummaryWorkflowIdempotencyKey(tt.key); got != tt.want {
			t.Errorf("ValidSummaryWorkflowIdempotencyKey(%q) = %t, want %t", tt.key, got, tt.want)
		}
	}
}

func baseAgentWorkflowInput() AgentCreateSummaryWorkflowInput {
	return AgentCreateSummaryWorkflowInput{
		ActorID:        "creator",
		SpaceID:        "space-1",
		Title:          "Agent weekly summary",
		Requirement:    "Summarize delivery status",
		Sources:        []SummaryWorkflowSource{{SourceType: model.SourceGroup, SourceID: "group-1"}},
		IdempotencyKey: "agent-workflow-001",
	}
}

func TestAgentPersonalWorkflowRequiresSafeBoundary(t *testing.T) {
	svc, db := newSummaryWorkflowTestService(t)
	in := baseAgentWorkflowInput()

	got, err := svc.CreatePersonalFromAgent(context.Background(), in)
	if err != nil {
		t.Fatalf("CreatePersonalFromAgent() error: %v", err)
	}
	if got.Target != SummaryWorkflowPersonal || got.Task.CreatorID != in.ActorID {
		t.Fatalf("result = %#v, want personal task owned by actor", got)
	}
	if got.Task.TimeRangeEnd.Sub(got.Task.TimeRangeStart) < (AgentSummaryDefaultTimeRangeDays*24*time.Hour)-time.Second ||
		got.Task.TimeRangeEnd.Sub(got.Task.TimeRangeStart) > (AgentSummaryDefaultTimeRangeDays*24*time.Hour)+time.Second {
		t.Fatalf("default range = %s, want %d days", got.Task.TimeRangeEnd.Sub(got.Task.TimeRangeStart), AgentSummaryDefaultTimeRangeDays)
	}

	replay, err := svc.CreatePersonalFromAgent(context.Background(), in)
	if err != nil || !replay.Replayed || replay.Task.ID != got.Task.ID || replay.WorkerTrigger == nil {
		t.Fatalf("replay = %#v, err=%v", replay, err)
	}
	var count int64
	db.Model(&model.SummaryTask{}).Count(&count)
	if count != 1 {
		t.Fatalf("task count = %d, want 1", count)
	}
}

func TestAgentWorkflowRejectsWrongTargetMissingSourceOrKey(t *testing.T) {
	svc, _ := newSummaryWorkflowTestService(t)

	personalWithParticipant := baseAgentWorkflowInput()
	personalWithParticipant.Participants = []SummaryWorkflowParticipant{{UserID: "p1"}}
	if _, err := svc.CreatePersonalFromAgent(context.Background(), personalWithParticipant); err == nil {
		t.Fatal("personal workflow accepted another participant")
	}

	teamWithoutParticipant := baseAgentWorkflowInput()
	if _, err := svc.CreateTeamFromAgent(context.Background(), teamWithoutParticipant); err == nil {
		t.Fatal("team workflow accepted no other participant")
	}

	missingSource := baseAgentWorkflowInput()
	missingSource.Sources = nil
	if _, err := svc.CreatePersonalFromAgent(context.Background(), missingSource); err == nil {
		t.Fatal("personal agent workflow accepted no source")
	}

	teamMissingRequirement := baseAgentWorkflowInput()
	teamMissingRequirement.Requirement = "  "
	teamMissingRequirement.Participants = []SummaryWorkflowParticipant{{UserID: "p1"}}
	if _, err := svc.CreateTeamFromAgent(context.Background(), teamMissingRequirement); err == nil {
		t.Fatal("team workflow accepted no requirement")
	}

	invalidSource := baseAgentWorkflowInput()
	invalidSource.Sources = []SummaryWorkflowSource{{SourceType: model.SourceGroup}}
	if _, err := svc.CreatePersonalFromAgent(context.Background(), invalidSource); err == nil {
		t.Fatal("personal agent workflow accepted an invalid source")
	}

	invalidTeamSource := baseAgentWorkflowInput()
	invalidTeamSource.Sources = []SummaryWorkflowSource{{SourceType: 99, SourceID: "group-1"}}
	invalidTeamSource.Participants = []SummaryWorkflowParticipant{{UserID: "p1"}}
	if _, err := svc.CreateTeamFromAgent(context.Background(), invalidTeamSource); err == nil {
		t.Fatal("team agent workflow accepted an invalid selected source")
	}

	emptyTeamParticipant := baseAgentWorkflowInput()
	emptyTeamParticipant.Participants = []SummaryWorkflowParticipant{{UserID: ""}}
	if _, err := svc.CreateTeamFromAgent(context.Background(), emptyTeamParticipant); err == nil {
		t.Fatal("team agent workflow accepted an empty participant id")
	}

	missingKey := baseAgentWorkflowInput()
	missingKey.IdempotencyKey = ""
	if _, err := svc.CreatePersonalFromAgent(context.Background(), missingKey); err == nil {
		t.Fatal("agent workflow accepted no idempotency key")
	}
}

func TestAgentTeamWorkflowAllowsNoExplicitSourceWithRequirement(t *testing.T) {
	svc, db := newSummaryWorkflowTestService(t)
	in := baseAgentWorkflowInput()
	in.IdempotencyKey = "agent-team-source-free-001"
	in.Sources = nil
	in.Participants = []SummaryWorkflowParticipant{{UserID: "p1", UserName: "P1"}}

	got, err := svc.CreateTeamFromAgent(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateTeamFromAgent() error: %v", err)
	}
	if got.Target != SummaryWorkflowTeam || !got.Inferred {
		t.Fatalf("result = %#v, want inferred team workflow", got)
	}
	if got.Task.Topic != in.Requirement {
		t.Fatalf("topic = %q, want %q", got.Task.Topic, in.Requirement)
	}

	var sourceCount int64
	if err := db.Model(&model.SummarySource{}).Where("task_id = ?", got.Task.ID).Count(&sourceCount).Error; err != nil {
		t.Fatal(err)
	}
	if sourceCount != 0 {
		t.Fatalf("source count = %d, want 0", sourceCount)
	}

	var participantCount int64
	if err := db.Model(&model.SummaryParticipant{}).Where("task_id = ?", got.Task.ID).Count(&participantCount).Error; err != nil {
		t.Fatal(err)
	}
	if participantCount != 2 {
		t.Fatalf("participant count = %d, want creator + collaborator", participantCount)
	}
}

func TestAgentTeamWorkflowCreatesCollaborators(t *testing.T) {
	svc, db := newSummaryWorkflowTestService(t)
	in := baseAgentWorkflowInput()
	in.IdempotencyKey = "agent-team-001"
	in.AgentSessionID = "workspace-session-v1"
	in.Sources = append(in.Sources, SummaryWorkflowSource{SourceType: model.SourceGroup, SourceID: "group-2"})
	in.Participants = []SummaryWorkflowParticipant{{UserID: "p1", UserName: "P1"}}

	got, err := svc.CreateTeamFromAgent(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateTeamFromAgent() error: %v", err)
	}
	if got.Target != SummaryWorkflowTeam {
		t.Fatalf("target = %q, want team", got.Target)
	}
	if got.Task.AgentSessionID != in.AgentSessionID {
		t.Fatalf("agent session id = %q, want %q", got.Task.AgentSessionID, in.AgentSessionID)
	}
	var participants []model.SummaryParticipant
	if err := db.Order("user_id").Find(&participants).Error; err != nil {
		t.Fatal(err)
	}
	if len(participants) != 2 {
		t.Fatalf("participants = %d, want creator + collaborator", len(participants))
	}
	var sources []model.SummarySource
	if err := db.Where("task_id = ?", got.Task.ID).Order("source_id").Find(&sources).Error; err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 || sources[0].SourceID != "group-1" || sources[1].SourceID != "group-2" {
		t.Fatalf("sources = %#v, want both selected groups", sources)
	}
}
