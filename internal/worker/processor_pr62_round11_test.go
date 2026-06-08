package worker

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/gorm"
)

func ensureBootstrapConflictIndexes(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, stmt := range []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS uk_summary_participant_task_user ON summary_participant(task_id, user_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uk_task_participant ON summary_personal_result(task_id, participant_ref_id)`,
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("create index %q: %v", stmt, err)
		}
	}
}

func newRound11Processor(db *gorm.DB) *Processor {
	return NewProcessor(db, nil, nil, nil, &config.Config{WorkerLeaseMinutes: 20})
}

func TestPR62Round11_BootstrapCreatorParticipantIsIdempotent(t *testing.T) {
	db := setupProcessorTestDB(t)
	ensureBootstrapConflictIndexes(t, db)

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db handle: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)

	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:         "TST-R11-IDEMPOTENT",
		SpaceID:        "space1",
		CreatorID:      "creator1",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: now.Add(-time.Hour),
		TimeRangeEnd:   now,
		Status:         model.StatusProcessing,
		TriggerType:    model.TriggerScheduled,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}

	p := newRound11Processor(db)

	errCh := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := p.bootstrapCreatorParticipant(task)
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("bootstrap creator participant: %v", err)
		}
	}

	var participantCount int64
	if err := db.Model(&model.SummaryParticipant{}).Where("task_id = ?", task.ID).Count(&participantCount).Error; err != nil {
		t.Fatalf("count participants: %v", err)
	}
	if participantCount != 1 {
		t.Fatalf("participant count=%d want 1", participantCount)
	}

	var personalResultCount int64
	if err := db.Model(&model.PersonalResult{}).Where("task_id = ?", task.ID).Count(&personalResultCount).Error; err != nil {
		t.Fatalf("count personal results: %v", err)
	}
	if personalResultCount != 1 {
		t.Fatalf("personal_result count=%d want 1", personalResultCount)
	}

	var participant model.SummaryParticipant
	if err := db.Where("task_id = ? AND user_id = ?", task.ID, task.CreatorID).First(&participant).Error; err != nil {
		t.Fatalf("load participant: %v", err)
	}
	if participant.PersonalResultID == nil {
		t.Fatalf("participant missing personal_result_id after bootstrap")
	}

	var pr model.PersonalResult
	if err := db.Where("task_id = ? AND participant_ref_id = ?", task.ID, participant.ID).First(&pr).Error; err != nil {
		t.Fatalf("load personal result: %v", err)
	}
	if participant.PersonalResultID == nil || *participant.PersonalResultID != pr.ID {
		t.Fatalf("participant personal_result_id=%v want %d", participant.PersonalResultID, pr.ID)
	}
}

func TestPR62Round11_BootstrapCreatorParticipantRollsBackOnPersonalResultFailure(t *testing.T) {
	db := setupProcessorTestDB(t)
	ensureBootstrapConflictIndexes(t, db)

	now := time.Now().UTC()
	task := model.SummaryTask{
		TaskNo:         "TST-R11-ROLLBACK",
		SpaceID:        "space1",
		CreatorID:      "creator1",
		SummaryMode:    model.ModeByPerson,
		TimeRangeStart: now.Add(-time.Hour),
		TimeRangeEnd:   now,
		Status:         model.StatusProcessing,
		TriggerType:    model.TriggerScheduled,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}

	p := newRound11Processor(db)
	p.createPRFn = func(tx *gorm.DB, pr *model.PersonalResult) error {
		return errors.New("inject bootstrap personal_result failure")
	}

	_, err := p.bootstrapCreatorParticipant(task)
	if err == nil {
		t.Fatalf("bootstrap should return injected error")
	}
	if !strings.Contains(err.Error(), "inject bootstrap personal_result failure") {
		t.Fatalf("bootstrap error=%v want injected failure", err)
	}

	var participantCount int64
	if err := db.Model(&model.SummaryParticipant{}).Where("task_id = ?", task.ID).Count(&participantCount).Error; err != nil {
		t.Fatalf("count participants: %v", err)
	}
	if participantCount != 0 {
		t.Fatalf("participant count=%d want 0 after rollback", participantCount)
	}

	var personalResultCount int64
	if err := db.Model(&model.PersonalResult{}).Where("task_id = ?", task.ID).Count(&personalResultCount).Error; err != nil {
		t.Fatalf("count personal results: %v", err)
	}
	if personalResultCount != 0 {
		t.Fatalf("personal_result count=%d want 0 after rollback", personalResultCount)
	}
}
