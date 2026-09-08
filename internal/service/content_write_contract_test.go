package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type contentRefineModelFunc func(context.Context, []ChatMessage, float64) (string, int, string, error)

func (f contentRefineModelFunc) CallWithModel(ctx context.Context, messages []ChatMessage, temp float64) (string, int, string, error) {
	return f(ctx, messages, temp)
}

func seedWritableContent(t *testing.T, db *gorm.DB, taskID int64, kind string) (ContentTarget, *FormalContentVersion) {
	t.Helper()
	now := time.Now().UTC()
	task := model.SummaryTask{
		ID: taskID, TaskNo: fmt.Sprintf("write-%d", taskID), SpaceID: "s", CreatorID: "owner",
		SummaryMode: 1, Status: model.StatusCompleted, TimeRangeStart: now, TimeRangeEnd: now,
	}
	if kind == ContentPersonal {
		task.SummaryMode = model.ModeByPerson
	}
	requireWriteOK(t, db.Create(&task).Error)
	member := model.SummaryParticipant{TaskID: taskID, UserID: "owner", Status: model.ParticipantSubmitted}
	requireWriteOK(t, db.Create(&member).Error)
	target := ContentTarget{SpaceID: "s", TaskID: taskID, Kind: kind}
	if kind == ContentPersonal {
		target.UserID = "owner"
		requireWriteOK(t, db.Create(&model.PersonalResult{
			TaskID: taskID, UserID: "owner", ParticipantRefID: member.ID, Content: "original [7]",
			CitationsJSON: `[{"index":7,"content":"frozen evidence"}]`, WorkerStatus: model.PersonalStatusCompleted,
			GeneratedAt: &now,
		}).Error)
	} else {
		row := model.SummaryResult{TaskID: taskID, Content: "original [7] [P7]", Version: 1, GeneratedAt: now,
			CitationsJSON:          `[{"index":7,"content":"frozen evidence"}]`,
			TeamCitationsJSON:      `[{"index":7,"user_id":"member","user_name":"Saved Name"}]`,
			GenerationSpecSnapshot: model.JSON(`{"requirement":"original requirement"}`)}
		requireWriteOK(t, db.Create(&row).Error)
		requireWriteOK(t, db.Model(&task).Update("current_result_id", row.ID).Error)
	}
	catalog, err := NewContentService(db).Catalog(context.Background(), "s", taskID, "owner")
	requireWriteOK(t, err)
	return target, catalog.Contents[0].CurrentVersion
}

func baselineOf(v *FormalContentVersion) ContentBaseline {
	return ContentBaseline{ExpectedCurrentVersionID: v.VersionID, ExpectedContentRevision: v.ContentRevision}
}

func requireWriteOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func requireContentCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil || err.Error() != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func contentRowCount(t *testing.T, db *gorm.DB, target ContentTarget) int64 {
	t.Helper()
	var table any = &model.SummaryResult{}
	q := db.Where("task_id = ?", target.TaskID)
	if target.Kind == ContentPersonal {
		table = &model.PersonalResultVersion{}
		q = q.Where("user_id = ?", target.UserID)
	}
	var count int64
	requireWriteOK(t, q.Model(table).Count(&count).Error)
	return count
}

func exerciseContentOverwrite(t *testing.T, db *gorm.DB, taskID int64, kind string) {
	t.Helper()
	ctx := context.Background()
	target, old := seedWritableContent(t, db, taskID, kind)
	s := NewContentService(db)
	edited, err := s.Edit(ctx, "s", taskID, "owner", target.ID(), EditContentRequest{
		ContentBaseline: baselineOf(old), Content: "edited without markers",
	})
	requireWriteOK(t, err)
	if edited.Provisional || edited.Version != 1 || edited.ContentRevision != 2 ||
		(!old.Provisional && edited.VersionID != old.VersionID) || len(edited.Citations) != 1 {
		t.Fatalf("edit changed version identity or dropped unused evidence: %+v", edited)
	}
	if contentRowCount(t, db, target) != 1 {
		t.Fatal("edit created history beyond lazy V1")
	}
	_, err = s.Edit(ctx, "s", taskID, "owner", target.ID(), EditContentRequest{ContentBaseline: baselineOf(old), Content: "stale"})
	requireContentCode(t, err, "content_conflict")
	_, err = s.Edit(ctx, "s", taskID, "owner", target.ID(), EditContentRequest{ContentBaseline: baselineOf(edited), Content: "injected [8]"})
	requireContentCode(t, err, "invalid_citation_reference")
	// Make V2 using the actual durable coordinator, then restore V1 into V2.
	run, err := s.QueueRefine(ctx, "s", taskID, "owner", target.ID(), RefineContentRequest{
		ContentBaseline: baselineOf(edited), IdempotencyKey: "overwrite-refine", Feedback: "shorten",
	})
	requireWriteOK(t, err)
	claimed, err := s.ClaimGeneration(ctx, run.ID, time.Minute)
	requireWriteOK(t, err)
	completed, err := s.CompleteRefine(ctx, run.ID, claimed.ExecutionToken, "refined [7]", "test", 1)
	requireWriteOK(t, err)
	current, err := s.Version(ctx, "s", taskID, "owner", target.ID(), completed.OutputVersionID)
	requireWriteOK(t, err)
	restored, err := s.Restore(ctx, "s", taskID, "owner", target.ID(), RestoreContentRequest{
		ContentBaseline: baselineOf(current), SourceVersionID: edited.VersionID,
	})
	requireWriteOK(t, err)
	if restored.VersionID != current.VersionID || restored.Version != 2 || restored.ContentRevision != 4 ||
		restored.Content != edited.Content || restored.RestoredFromVersionID != edited.VersionID ||
		len(restored.Citations) != 1 || len(restored.TeamCitations) != len(edited.TeamCitations) ||
		contentRowCount(t, db, target) != 2 {
		t.Fatalf("restore switched pointer, created row or lost evidence: %+v", restored)
	}
	source, err := s.Version(ctx, "s", taskID, "owner", target.ID(), edited.VersionID)
	requireWriteOK(t, err)
	if source.Content != edited.Content || source.ContentRevision != edited.ContentRevision || source.IsCurrent {
		t.Fatalf("restore changed source: %+v", source)
	}
	if kind == ContentPersonal {
		var pr model.PersonalResult
		requireWriteOK(t, db.Where("task_id = ?", taskID).First(&pr).Error)
		id, _ := ParseContentVersionID(restored.VersionID, target)
		if pr.Content != restored.Content || pr.ContentRevision != 4 || pr.CurrentVersionID == nil || *pr.CurrentVersionID != id.RowID {
			t.Fatalf("materialized content diverged: %+v", pr)
		}
	}
	var audits []model.SummaryContentAudit
	requireWriteOK(t, db.Where("task_id = ? AND operation_type = ?", taskID, "restore").Find(&audits).Error)
	if len(audits) != 1 || audits[0].SourceContentRevision != 2 || audits[0].ContentRevision != 4 {
		t.Fatalf("missing revision audit: %+v", audits)
	}
}

func exerciseContentGeneration(t *testing.T, db *gorm.DB, taskID int64, kind string) {
	t.Helper()
	ctx := context.Background()
	target, current := seedWritableContent(t, db, taskID, kind)
	s := NewContentService(db)
	request := RefineContentRequest{ContentBaseline: baselineOf(current), IdempotencyKey: "one", Feedback: "short"}
	run, err := s.QueueRefine(ctx, "s", taskID, "owner", target.ID(), request)
	requireWriteOK(t, err)
	replay, err := s.QueueRefine(ctx, "s", taskID, "owner", target.ID(), request)
	requireWriteOK(t, err)
	if replay.ID != run.ID || !replay.EffectiveAt.Equal(run.EffectiveAt.Truncate(time.Microsecond)) &&
		!replay.EffectiveAt.Equal(run.EffectiveAt) {
		t.Fatal("retry changed identity or frozen time")
	}
	conflictingKey := request
	conflictingKey.Feedback = "different"
	_, err = s.QueueRefine(ctx, "s", taskID, "owner", target.ID(), conflictingKey)
	requireContentCode(t, err, "idempotency_conflict")
	catalog, err := s.Catalog(ctx, "s", taskID, "owner")
	requireWriteOK(t, err)
	current = catalog.Contents[0].CurrentVersion
	second := RefineContentRequest{ContentBaseline: baselineOf(current), IdempotencyKey: "second", Feedback: "short"}
	_, err = s.QueueRefine(ctx, "s", taskID, "owner", target.ID(), second)
	requireContentCode(t, err, "generation_busy")
	claimed, err := s.ClaimGeneration(ctx, run.ID, time.Minute)
	requireWriteOK(t, err)
	// Simulate another tab/edit writer. UI disables this while running, but CAS
	// still has to protect a request already in flight.
	edited, err := s.Edit(ctx, "s", taskID, "owner", target.ID(), EditContentRequest{
		ContentBaseline: baselineOf(current), Content: "concurrent edit",
	})
	requireWriteOK(t, err)
	conflict, err := s.CompleteRefine(ctx, run.ID, claimed.ExecutionToken, "candidate [7]", "test", 2)
	requireWriteOK(t, err)
	if conflict.Status != "conflict" || conflict.Applied || conflict.OutputVersionID == "" {
		t.Fatalf("conflict lost candidate: %+v", conflict)
	}
	again, err := s.CompleteRefine(ctx, run.ID, claimed.ExecutionToken, "duplicate callback", "test", 2)
	requireWriteOK(t, err)
	if again.OutputVersionID != conflict.OutputVersionID || contentRowCount(t, db, target) != 2 {
		t.Fatal("callback appended twice")
	}
	applied, err := s.ApplyCandidate(ctx, "s", taskID, "owner", target.ID(), run.ID, baselineOf(edited))
	requireWriteOK(t, err)
	if applied.VersionID != conflict.OutputVersionID || applied.Content != "candidate [7]" ||
		applied.ContentRevision != edited.ContentRevision+1 || contentRowCount(t, db, target) != 2 {
		t.Fatalf("apply did not switch existing candidate: %+v", applied)
	}
	// Retain more than five versions through subsequent successful refinements.
	for i := 0; i < 5; i++ {
		next, err := s.QueueRefine(ctx, "s", taskID, "owner", target.ID(), RefineContentRequest{
			ContentBaseline: baselineOf(applied), IdempotencyKey: fmt.Sprintf("next-%d", i), Feedback: "short",
		})
		requireWriteOK(t, err)
		claim, err := s.ClaimGeneration(ctx, next.ID, time.Minute)
		requireWriteOK(t, err)
		done, err := s.CompleteRefine(ctx, next.ID, claim.ExecutionToken, "next", "test", 1)
		requireWriteOK(t, err)
		applied, err = s.Version(ctx, "s", taskID, "owner", target.ID(), done.OutputVersionID)
		requireWriteOK(t, err)
	}
	if contentRowCount(t, db, target) != 7 || applied.Version != 7 {
		t.Fatal("history pruned or hidden edit version appended")
	}
	// Expired lease is recoverable; an old token cannot commit after takeover.
	next, err := s.QueueRefine(ctx, "s", taskID, "owner", target.ID(), RefineContentRequest{
		ContentBaseline: baselineOf(applied), IdempotencyKey: "recover", Feedback: "short",
	})
	requireWriteOK(t, err)
	first, err := s.ClaimGeneration(ctx, next.ID, time.Minute)
	requireWriteOK(t, err)
	requireWriteOK(t, db.Model(&model.SummaryGenerationRun{}).Where("id = ?", next.ID).
		Update("lease_until", time.Now().UTC().Add(-time.Minute)).Error)
	pending, err := NewContentService(db).RecoverableGenerations(ctx, []string{"s"}, 20)
	requireWriteOK(t, err)
	if len(pending) != 1 || pending[0].ID != next.ID {
		t.Fatalf("restart did not recover expired run: %+v", pending)
	}
	secondClaim, err := NewContentService(db).ClaimGeneration(ctx, next.ID, time.Minute)
	requireWriteOK(t, err)
	_, err = s.CompleteRefine(ctx, next.ID, first.ExecutionToken, "late", "test", 1)
	requireContentCode(t, err, "generation_lease_lost")
	requireWriteOK(t, s.CancelGeneration(ctx, "s", taskID, "owner", target.ID(), next.ID))
	_, err = s.CompleteRefine(ctx, next.ID, secondClaim.ExecutionToken, "cancelled late", "test", 1)
	requireContentCode(t, err, "generation_lease_lost")
	if contentRowCount(t, db, target) != 7 {
		t.Fatal("cancelled/late run saved an output")
	}
	public, _ := json.Marshal(next)
	if contains := string(public); stringsContainsAny(contains, "frozen evidence", "input_json", "idempotency_hash", "execution_token") {
		t.Fatal("private run input escaped public serialization")
	}
}

func stringsContainsAny(value string, parts ...string) bool {
	for _, part := range parts {
		if strings.Contains(value, part) {
			return true
		}
	}
	return false
}

func TestContentMySQLTransactionalWrites(t *testing.T) {
	db := contentMySQLDB(t, false)
	for i, kind := range []string{ContentResult, ContentPersonal} {
		t.Run(kind, func(t *testing.T) {
			exerciseContentOverwrite(t, db, int64(i+1), kind)
			exerciseContentGeneration(t, db, int64(i+11), kind)
		})
	}
}

func TestContentMySQLRuntimeCASAndAdmissionRace(t *testing.T) {
	db := contentMySQLDB(t, false)
	ctx := context.Background()
	target, current := seedWritableContent(t, db, 1, ContentPersonal)
	var accepted, unexpected atomic.Int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := NewContentService(db).Edit(ctx, "s", 1, "owner", target.ID(), EditContentRequest{
				ContentBaseline: baselineOf(current), Content: fmt.Sprintf("racing edit %d", i),
			})
			if err == nil {
				accepted.Add(1)
			} else if err.Error() != "content_conflict" {
				t.Logf("unexpected edit error: %v", err)
				unexpected.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if accepted.Load() != 1 || unexpected.Load() != 0 || contentRowCount(t, db, target) != 1 {
		t.Fatalf("runtime CAS accepted=%d unexpected=%d", accepted.Load(), unexpected.Load())
	}
	catalog, err := NewContentService(db).Catalog(ctx, "s", 1, "owner")
	requireWriteOK(t, err)
	current = catalog.Contents[0].CurrentVersion
	start = make(chan struct{})
	accepted.Store(0)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := NewContentService(db).QueueRefine(ctx, "s", 1, "owner", target.ID(), RefineContentRequest{
				ContentBaseline: baselineOf(current), IdempotencyKey: fmt.Sprint(i), Feedback: "short",
			})
			if err == nil {
				accepted.Add(1)
			} else if err.Error() != "generation_busy" {
				t.Logf("unexpected admission error: %v", err)
				unexpected.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if accepted.Load() != 1 || unexpected.Load() != 0 {
		t.Fatalf("runtime admission accepted=%d unexpected=%d", accepted.Load(), unexpected.Load())
	}
}

func TestContentMySQLRollbackAndDuplicateCompletion(t *testing.T) {
	db := contentMySQLDB(t, false)
	ctx := context.Background()
	target, current := seedWritableContent(t, db, 1, ContentResult)
	personal, legacy := seedWritableContent(t, db, 2, ContentPersonal)
	// Force the last audit write to fail: all earlier changes in the command,
	// including a lazy baseline, must roll back with it.
	requireWriteOK(t, db.Exec(`CREATE TRIGGER reject_content_audit BEFORE INSERT ON summary_content_audit
		FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'fixture audit unavailable'`).Error)
	for _, test := range []struct {
		target  ContentTarget
		current *FormalContentVersion
	}{{target, current}, {personal, legacy}} {
		before := contentRowCount(t, db, test.target)
		_, err := NewContentService(db).Edit(ctx, "s", test.target.TaskID, "owner", test.target.ID(), EditContentRequest{
			ContentBaseline: baselineOf(test.current), Content: "must roll back",
		})
		if err == nil {
			t.Fatal("audit failure reported success")
		}
		catalog, err := NewContentService(db).Catalog(ctx, "s", test.target.TaskID, "owner")
		requireWriteOK(t, err)
		after := catalog.Contents[0].CurrentVersion
		if after.Content != test.current.Content || after.VersionID != test.current.VersionID ||
			after.ContentRevision != test.current.ContentRevision || contentRowCount(t, db, test.target) != before {
			t.Fatal("transaction did not roll back content/baseline/revision")
		}
	}
	s := NewContentService(db)
	run, err := s.QueueRefine(ctx, "s", 1, "owner", target.ID(), RefineContentRequest{
		ContentBaseline: baselineOf(current), IdempotencyKey: "duplicate-completion", Feedback: "short",
	})
	requireWriteOK(t, err)
	claimed, err := s.ClaimGeneration(ctx, run.ID, time.Minute)
	requireWriteOK(t, err)
	var accepted, failures atomic.Int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := NewContentService(db).CompleteRefine(ctx, run.ID, claimed.ExecutionToken, "once [7]", "test", 1)
			if err != nil || result == nil || !result.Applied {
				failures.Add(1)
				t.Logf("completion: %+v %v", result, err)
			} else {
				accepted.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if accepted.Load() != 12 || failures.Load() != 0 || contentRowCount(t, db, target) != 2 {
		t.Fatalf("duplicate completions accepted=%d failed=%d", accepted.Load(), failures.Load())
	}
}

func TestContentMySQLTaskSlotAllowsOnlyItsDistinctChildren(t *testing.T) {
	db := contentMySQLDB(t, false)
	_, _ = seedWritableContent(t, db, 1, ContentResult)
	makeRun := func(scope, owner string, parent *string) model.SummaryGenerationRun {
		run := mysqlGenerationFixture(uuid.NewString(), uuid.NewString(), "placeholder")
		run.Scope, run.ParentGenerationID = scope, parent
		if owner != "" {
			run.ContentID = (ContentTarget{SpaceID: "s", TaskID: 1, Kind: ContentPersonal, UserID: owner}).ID()
		}
		return run
	}
	reserve := func(run *model.SummaryGenerationRun) error {
		return db.Transaction(func(tx *gorm.DB) error {
			var task model.SummaryTask
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&task, 1).Error; err != nil {
				return err
			}
			return NewContentService(tx).reserveGeneration(run)
		})
	}
	parent := makeRun("task", "", nil)
	requireWriteOK(t, reserve(&parent))
	a, b := makeRun("content", "a", &parent.ID), makeRun("content", "b", &parent.ID)
	requireWriteOK(t, reserve(&a))
	requireWriteOK(t, reserve(&b)) // both children remain live concurrently
	duplicate := makeRun("content", "a", &parent.ID)
	requireContentCode(t, reserve(&duplicate), "generation_busy")
	independent := makeRun("content", "c", nil)
	requireContentCode(t, reserve(&independent), "generation_busy")
	secondRound := makeRun("task", "", nil)
	requireContentCode(t, reserve(&secondRound), "generation_busy")
}

func TestContentMySQLExecutorKeepsTeamEvidencePrivate(t *testing.T) {
	db := contentMySQLDB(t, false)
	ctx := context.Background()
	target, current := seedWritableContent(t, db, 1, ContentResult)
	requireWriteOK(t, db.Model(&model.SummaryTask{}).Where("id = ?", 1).Update("summary_mode", model.ModeByPerson).Error)
	requireWriteOK(t, db.Create(&model.SummaryParticipant{TaskID: 1, UserID: "member", Status: model.ParticipantSubmitted}).Error)
	s := NewContentService(db)
	run, err := s.QueueRefine(ctx, "s", 1, "owner", target.ID(), RefineContentRequest{
		ContentBaseline: baselineOf(current), IdempotencyKey: "team-executor", Feedback: "shorten",
	})
	requireWriteOK(t, err)
	completed, err := s.ExecuteRefine(ctx, run.ID, contentRefineModelFunc(func(_ context.Context, messages []ChatMessage, _ float64) (string, int, string, error) {
		if len(messages) != 2 || strings.Contains(messages[1].Content, "frozen evidence") ||
			strings.Contains(messages[1].Content, "[7]") || !strings.Contains(messages[1].Content, "[P7]") {
			t.Fatalf("team rewrite received private message evidence: %+v", messages)
		}
		return "team rewrite [P7]", 2, "fixture-model", nil
	}))
	requireWriteOK(t, err)
	v, err := s.Version(ctx, "s", 1, "owner", target.ID(), completed.OutputVersionID)
	requireWriteOK(t, err)
	if v.CitationVisibility != "permission_hidden" || len(v.Citations) != 0 || len(v.TeamCitations) != 1 || !completed.Applied {
		t.Fatalf("team rewrite citation boundary changed: %+v", v)
	}
	memberTarget := ContentTarget{SpaceID: "s", TaskID: 1, Kind: ContentPersonal, UserID: "member"}
	_, err = s.Generation(ctx, "s", 1, "owner", memberTarget.ID(), run.ID)
	requireContentCode(t, err, "content_forbidden")
}
