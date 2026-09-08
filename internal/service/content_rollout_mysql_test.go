package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestContentMySQLLegacyWriterAndCleanerSerializeWithEnrollment(t *testing.T) {
	db := contentMySQLDB(t, false)
	t.Setenv("SUMMARY_CONTENT_WRITE_SPACES", "")
	target, _ := seedWritableContent(t, db, 1, ContentResult)
	for i := 2; i <= 7; i++ {
		requireWriteOK(t, db.Create(&model.SummaryResult{TaskID: 1, Version: i, Content: "history", GeneratedAt: time.Now()}).Error)
	}
	tx := db.Begin()
	requireWriteOK(t, tx.Error)
	defer tx.Rollback()
	var task model.SummaryTask
	requireWriteOK(t, tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&task, 1).Error)
	requireWriteOK(t, tx.Model(&task).Update("content_protocol_version", ContentContractVersion).Error)
	writerStarted, cleanerStarted := make(chan struct{}), make(chan struct{})
	writerResult, cleanerResult := make(chan error, 1), make(chan error, 1)
	go func() {
		close(writerStarted)
		writerResult <- WithLegacyContentWrite(db, 1, func(tx *gorm.DB) error {
			return tx.Model(&model.SummaryResult{}).Where("task_id = ?", 1).Update("content", "late legacy write").Error
		})
	}()
	go func() {
		close(cleanerStarted)
		cleanerResult <- PruneSummaryResultVersions(db, 1, 5)
	}()
	<-writerStarted
	<-cleanerStarted
	requireWriteOK(t, tx.Commit().Error)
	if err := <-writerResult; !errors.Is(err, ErrContentProtocolRequired) {
		t.Fatalf("late writer escaped enrollment: %v", err)
	}
	requireWriteOK(t, <-cleanerResult)
	if contentRowCount(t, db, target) != 7 {
		t.Fatal("concurrent cleaner deleted formal history")
	}
	var changed int64
	requireWriteOK(t, db.Model(&model.SummaryResult{}).Where("content = ?", "late legacy write").Count(&changed).Error)
	if changed != 0 {
		t.Fatal("rejected legacy write changed content")
	}
}

func TestContentMySQLDeletionCancelsRunAtomicallyAndFencesLateOutput(t *testing.T) {
	db := contentMySQLDB(t, false)
	target, current := seedWritableContent(t, db, 1, ContentPersonal)
	s := NewContentService(db)
	ctx := context.Background()
	run, err := s.QueueRefine(ctx, "s", 1, "owner", target.ID(), RefineContentRequest{
		ContentBaseline: baselineOf(current), IdempotencyKey: "delete", Feedback: "shorter",
	})
	requireWriteOK(t, err)
	run, err = s.ClaimGeneration(ctx, run.ID, time.Minute)
	requireWriteOK(t, err)
	failedDelete := errors.New("fixture delete failed")
	for _, rollback := range []bool{true, false} {
		err = db.Transaction(func(tx *gorm.DB) error {
			var task model.SummaryTask
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&task, 1).Error; err != nil {
				return err
			}
			if err := CancelContentGenerationsForDeletion(tx, task); err != nil {
				return err
			}
			if rollback {
				return failedDelete
			}
			return tx.Model(&task).Update("deleted_at", time.Now()).Error
		})
		var saved model.SummaryGenerationRun
		requireWriteOK(t, db.First(&saved, "id = ?", run.ID).Error)
		if rollback {
			if !errors.Is(err, failedDelete) || saved.CancelRequested || saved.Status != "running" || saved.ActiveSlot == nil {
				t.Fatal("failed deletion did not roll back cancellation")
			}
		} else {
			requireWriteOK(t, err)
			if !saved.CancelRequested || saved.Status != "cancelled" || saved.ActiveSlot != nil {
				t.Fatal("deletion did not cancel/release active run")
			}
		}
	}
	_, err = s.CompleteRefine(ctx, run.ID, run.ExecutionToken, "late [7]", "fixture", 1)
	requireContentCode(t, err, "generation_lease_lost")
	if contentRowCount(t, db, target) != 1 {
		t.Fatal("late callback appended output after deletion")
	}
}

func TestContentMySQLLegacyScopeRejectsMarkedAndExactAllowlistOnly(t *testing.T) {
	db := contentMySQLDB(t, false)
	seedWritableContent(t, db, 1, ContentPersonal)
	seedWritableContent(t, db, 2, ContentPersonal)
	seedWritableContent(t, db, 3, ContentPersonal)
	requireWriteOK(t, db.Model(&model.SummaryTask{}).Where("id = ?", 2).Update("space_id", "S").Error)
	requireWriteOK(t, db.Model(&model.SummaryTask{}).Where("id = ?", 3).Update("content_protocol_version", 1).Error)
	t.Setenv("SUMMARY_CONTENT_WRITE_SPACES", "s")
	var ids []int64
	requireWriteOK(t, db.Scopes(LegacyTaskScope).Model(&model.SummaryTask{}).Pluck("id", &ids).Error)
	if len(ids) != 1 || ids[0] != 2 {
		t.Fatalf("legacy scanner crossed exact rollout boundary: %v", ids)
	}
}
