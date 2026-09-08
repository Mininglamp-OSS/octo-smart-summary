//go:build cgo

package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

func TestContentTransactionalOverwriteAndGeneration(t *testing.T) {
	for _, kind := range []string{ContentResult, ContentPersonal} {
		t.Run(kind, func(t *testing.T) {
			db := contentTestDB(t)
			exerciseContentOverwrite(t, db, 1, kind)
			exerciseContentGeneration(t, db, 2, kind)
		})
	}
}

func TestContentWriteAuthorizationAndAuditedNormalization(t *testing.T) {
	db := contentTestDB(t)
	ctx := context.Background()
	target, current := seedWritableContent(t, db, 1, ContentPersonal)
	s := NewContentService(db)
	_, err := s.Edit(ctx, "other", 1, "owner", target.ID(), EditContentRequest{ContentBaseline: baselineOf(current), Content: "no"})
	requireContentCode(t, err, "content_not_found")
	_, err = s.Edit(ctx, "s", 1, "intruder", target.ID(), EditContentRequest{ContentBaseline: baselineOf(current), Content: "no"})
	requireContentCode(t, err, "content_forbidden")
	edited, err := s.Edit(ctx, "s", 1, "owner", target.ID(), EditContentRequest{ContentBaseline: baselineOf(current), Content: "first"})
	requireWriteOK(t, err)
	now := time.Now()
	requireWriteOK(t, db.Model(&model.PersonalResult{}).Where("task_id = ?", 1).
		Updates(map[string]any{"content": "legacy edit", "edited_at": now}).Error)
	catalog, err := s.Catalog(ctx, "s", 1, "owner")
	requireWriteOK(t, err)
	edited, err = s.Edit(ctx, "s", 1, "owner", target.ID(), EditContentRequest{
		ContentBaseline: baselineOf(catalog.Contents[0].CurrentVersion), Content: "normalized",
	})
	requireWriteOK(t, err)
	if edited.Content != "normalized" || contentRowCount(t, db, target) != 1 {
		t.Fatal("normalization created an edit version")
	}
	var count int64
	requireWriteOK(t, db.Model(&model.SummaryContentAudit{}).Where("operation_type = ?", "normalize").Count(&count).Error)
	if count != 2 { // one lazy V1, one evidenced legacy edit
		t.Fatalf("normalization audit missing: %d", count)
	}
	requireWriteOK(t, db.Model(&model.PersonalResult{}).Where("task_id = ?", 1).
		Updates(map[string]any{"content": "unexplained divergence", "edited_at": nil}).Error)
	_, err = s.Edit(ctx, "s", 1, "owner", target.ID(), EditContentRequest{ContentBaseline: baselineOf(edited), Content: "no"})
	requireContentCode(t, err, "content_repair_required")
}

func TestContentRefineExecutorUsesFrozenInputAndPreservesFailure(t *testing.T) {
	db := contentTestDB(t)
	ctx := context.Background()
	target, current := seedWritableContent(t, db, 1, ContentPersonal)
	s := NewContentService(db)
	run, err := s.QueueRefine(ctx, "s", 1, "owner", target.ID(), RefineContentRequest{
		ContentBaseline: baselineOf(current), IdempotencyKey: "executor", Feedback: "short",
	})
	requireWriteOK(t, err)
	// Change content after admission to prove the model never reconstructs its
	// input from a mutable parent version.
	catalog, err := s.Catalog(ctx, "s", 1, "owner")
	requireWriteOK(t, err)
	if catalog.Contents[0].ActiveGeneration == nil || catalog.Contents[0].ActiveGeneration.ID != run.ID {
		t.Fatal("reload cannot recover the active run")
	}
	current, err = s.Edit(ctx, "s", 1, "owner", target.ID(), EditContentRequest{
		ContentBaseline: baselineOf(catalog.Contents[0].CurrentVersion), Content: "new edit",
	})
	requireWriteOK(t, err)
	completed, err := s.ExecuteRefine(ctx, run.ID, contentRefineModelFunc(func(_ context.Context, messages []ChatMessage, _ float64) (string, int, string, error) {
		if len(messages) != 2 || !strings.Contains(messages[1].Content, "original [7]") ||
			!strings.Contains(messages[1].Content, "frozen evidence") || strings.Contains(messages[1].Content, "new edit") {
			t.Fatalf("executor did not use frozen evidence: %+v", messages)
		}
		return "```markdown\nshort [7]\n```", 3, "fixture-model", nil
	}))
	requireWriteOK(t, err)
	candidate, err := s.Version(ctx, "s", 1, "owner", target.ID(), completed.OutputVersionID)
	requireWriteOK(t, err)
	if !candidate.PendingApplication || candidate.IsCurrent || completed.Status != "conflict" ||
		!strings.Contains(string(candidate.GenerationSpecSnapshot), run.ID) {
		t.Fatalf("missing candidate/provenance: %+v", candidate)
	}
	failed, err := s.QueueRefine(ctx, "s", 1, "owner", target.ID(), RefineContentRequest{
		ContentBaseline: baselineOf(current), IdempotencyKey: "failure", Feedback: "short",
	})
	requireWriteOK(t, err)
	_, err = s.ExecuteRefine(ctx, failed.ID, contentRefineModelFunc(func(context.Context, []ChatMessage, float64) (string, int, string, error) {
		return "", 0, "", errors.New("provider error with sensitive details")
	}))
	requireContentCode(t, err, "model_failed")
	status, err := s.Generation(ctx, "s", 1, "owner", target.ID(), failed.ID)
	requireWriteOK(t, err)
	if status.Status != "failed" || status.ActiveSlot != nil || status.ErrorCode != "model_failed" || contentRowCount(t, db, target) != 2 {
		t.Fatal("failure changed content or kept the active slot")
	}
}

func TestContentRevokedMemberCannotClaimOrComplete(t *testing.T) {
	for _, afterClaim := range []bool{false, true} {
		name := "before_claim"
		if afterClaim {
			name = "before_complete"
		}
		t.Run(name, func(t *testing.T) {
			db := contentTestDB(t)
			ctx := context.Background()
			target, current := seedWritableContent(t, db, 1, ContentPersonal)
			s := NewContentService(db)
			run, err := s.QueueRefine(ctx, "s", 1, "owner", target.ID(), RefineContentRequest{
				ContentBaseline: baselineOf(current), IdempotencyKey: "revoke", Feedback: "short",
			})
			requireWriteOK(t, err)
			if afterClaim {
				run, err = s.ClaimGeneration(ctx, run.ID, time.Minute)
				requireWriteOK(t, err)
			}
			requireWriteOK(t, db.Model(&model.SummaryParticipant{}).Where("task_id = ?", 1).Update("status", model.ParticipantDeclined).Error)
			if afterClaim {
				_, err = s.CompleteRefine(ctx, run.ID, run.ExecutionToken, "late", "test", 1)
			} else {
				_, err = s.ClaimGeneration(ctx, run.ID, time.Minute)
			}
			requireContentCode(t, err, "content_forbidden")
			var persisted model.SummaryGenerationRun
			requireWriteOK(t, db.First(&persisted, "id = ?", run.ID).Error)
			if persisted.Status != "cancelled" || persisted.ActiveSlot != nil || contentRowCount(t, db, target) != 1 {
				t.Fatalf("revoked run not fenced/released: %+v", persisted)
			}
		})
	}
}
