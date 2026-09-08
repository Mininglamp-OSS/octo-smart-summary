//go:build cgo

package worker

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

func TestContentLegacySchedulerPreservesBodyPointersAndAnchors(t *testing.T) {
	for _, sticky := range []bool{false, true} {
		t.Run(fmt.Sprint(sticky), func(t *testing.T) {
			db := newSchedulerTestDB(t)
			last := time.Now().UTC().Add(-48 * time.Hour)
			schedule := seedDueSchedule(t, db, &last)
			task := seedBoundTask(t, db, schedule.ID, model.StatusCompleted, 1)
			pointer := int64(333)
			pr := model.PersonalResult{TaskID: task.ID, ParticipantRefID: 1, UserID: "u1",
				Content: "keep current", ContentRevision: 7, CurrentVersionID: &pointer,
				WorkerStatus: model.PersonalStatusCompleted}
			workerCheck(t, db.Create(&pr).Error)
			t.Setenv("SUMMARY_CONTENT_WRITE_SPACES", "sp")
			if sticky {
				workerCheck(t, db.Model(&task).Update("content_protocol_version", 1).Error)
				t.Setenv("SUMMARY_CONTENT_WRITE_SPACES", "")
			}
			_, claimed, err := claimAndRequeueScheduledTask(db, nil, schedule, time.Now().UTC(), 30, false)
			if !errors.Is(err, service.ErrContentProtocolRequired) || claimed {
				t.Fatalf("legacy schedule escaped: claimed=%t error=%v", claimed, err)
			}
			var saved model.PersonalResult
			workerCheck(t, db.First(&saved, pr.ID).Error)
			if saved.Content != pr.Content || saved.ContentRevision != 7 || saved.CurrentVersionID == nil ||
				*saved.CurrentVersionID != pointer || saved.WorkerStatus != model.PersonalStatusCompleted {
				t.Fatalf("legacy requeue reset current content: %+v", saved)
			}
			got := reloadSchedule(t, db, schedule.ID)
			if !got.NextRunAt.Equal(*schedule.NextRunAt) || !got.LastRunAt.Equal(*schedule.LastRunAt) || got.IsActive != 1 {
				t.Fatal("rejected schedule advanced or disabled its anchors")
			}
		})
	}
}

func TestContentLegacyPersonalCallbackAndFailureCannotMutateEnrolledTask(t *testing.T) {
	db := setupProcessorTestDB(t)
	task := model.SummaryTask{TaskNo: "guard-worker", SpaceID: "s", CreatorID: "owner",
		SummaryMode: model.ModeByPerson, Status: model.StatusCompleted, ContentProtocolVersion: 1}
	workerCheck(t, db.Create(&task).Error)
	member := model.SummaryParticipant{TaskID: task.ID, UserID: "owner", Status: model.ParticipantSubmitted}
	workerCheck(t, db.Create(&member).Error)
	pr := model.PersonalResult{TaskID: task.ID, ParticipantRefID: member.ID, UserID: "owner",
		Content: "keep", ContentRevision: 5, WorkerStatus: model.PersonalStatusCompleted}
	workerCheck(t, db.Create(&pr).Error)
	p := &Processor{db: db, cfg: &config.Config{WorkerMaxRetry: 1}}
	for _, emptyWindow := range []bool{false, true} {
		err := p.persistCompletedPersonalResult(task, pr, "late", nil, 0, 0, "fixture", time.Now(), emptyWindow)
		if !errors.Is(err, service.ErrContentProtocolRequired) {
			t.Fatalf("late callback not fenced: %v", err)
		}
	}
	p.markPersonalFailed(&pr, &member, "late provider failure")
	var saved model.PersonalResult
	workerCheck(t, db.First(&saved, pr.ID).Error)
	if saved.Content != "keep" || saved.ContentRevision != 5 || saved.RetryCount != 0 ||
		saved.WorkerStatus != model.PersonalStatusCompleted {
		t.Fatal("late failure mutated enrolled content")
	}
}
