package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/timezone"
	"gorm.io/gorm"
)

type generationSourceCheck func(context.Context, string, string, []model.SummaryGenerationSource) error

func (f generationSourceCheck) AuthorizeGenerationSources(ctx context.Context, space, actor string, sources []model.SummaryGenerationSource) error {
	return f(ctx, space, actor, sources)
}

type confirmedExecutor func(context.Context, model.SummaryGenerationRun, FrozenGenerationInput) (FullGenerationOutput, error)

func (f confirmedExecutor) GenerateConfirmed(ctx context.Context, run model.SummaryGenerationRun, input FrozenGenerationInput) (FullGenerationOutput, error) {
	return f(ctx, run, input)
}

func confirmedSpec() model.SummaryGenerationSpec {
	requirement := "Summarize decisions, not the title"
	return model.SummaryGenerationSpec{
		SchemaVersion: 1, SummaryMode: model.ModeByPerson, Collaboration: "single", Participants: []string{"owner"},
		Requirement: &requirement, Sources: []model.SummaryGenerationSource{{SourceID: "group", SourceType: 1, Confirmation: "user_confirmed"}},
		TimeSelector:  model.SummaryTimeSelector{Mode: "relative", Days: 7, Timezone: timezone.Name},
		CitationRules: model.SummaryCitationRules{Policy: "message_evidence"},
	}
}

func configuredService(db *gorm.DB) *ContentService {
	return NewContentService(db).WithExecution(generationSourceCheck(func(context.Context, string, string, []model.SummaryGenerationSource) error { return nil }), 90)
}

func exerciseGenerationConfiguration(t *testing.T, db *gorm.DB) {
	t.Helper()
	ctx := context.Background()
	target, current := seedWritableContent(t, db, 1, ContentPersonal)
	s := configuredService(db)
	config, err := s.GenerationConfiguration(ctx, "s", 1, "owner", target.ID())
	requireWriteOK(t, err)
	if config.State != "incomplete" || config.Spec.Requirement != nil || config.Revision != 0 {
		t.Fatal("legacy title was promoted to a confirmed requirement")
	}
	save := SaveGenerationConfigurationRequest{Spec: confirmedSpec(), Schedule: &GenerationScheduleConfig{Enabled: true, IntervalDays: 7, RunTime: "09:00"}}
	saved, err := s.SaveGenerationConfiguration(ctx, "s", 1, "owner", target.ID(), save)
	requireWriteOK(t, err)
	if saved.Generation != nil || saved.Configuration.State != "complete" || saved.Configuration.Revision != 1 || saved.Configuration.NextRunAt == nil {
		t.Fatalf("save should configure, not generate: %+v", saved)
	}
	if contentRowCount(t, db, target) != 0 {
		t.Fatal("configuration save wrote content")
	}
	_, err = s.SaveGenerationConfiguration(ctx, "s", 1, "owner", target.ID(), save)
	requireContentCode(t, err, "configuration_conflict")
	var task model.SummaryTask
	requireWriteOK(t, db.First(&task, 1).Error)
	if task.Topic != *save.Spec.Requirement || task.ContentProtocolVersion != 1 {
		t.Fatal("missing canonical projection/enrollment")
	}
	var sources []model.SummarySource
	requireWriteOK(t, db.Where("task_id = ?", 1).Find(&sources).Error)
	if len(sources) != 1 || sources[0].SourceID != "group" || sources[0].Derived {
		t.Fatal("source projection missing")
	}
	request := RegenerateContentRequest{ContentBaseline: baselineOf(current), ExpectedConfigRevision: 1, IdempotencyKey: "manual"}
	run, err := s.QueueRegenerate(ctx, "s", 1, "owner", target.ID(), request)
	requireWriteOK(t, err)
	retry, err := s.QueueRegenerate(ctx, "s", 1, "owner", target.ID(), request)
	requireWriteOK(t, err)
	if retry.ID != run.ID || !retry.EffectiveAt.Equal(run.EffectiveAt) {
		t.Fatal("retry did not reuse frozen admission")
	}
	catalog, err := s.Catalog(ctx, "s", 1, "owner")
	requireWriteOK(t, err)
	if catalog.Contents[0].Capabilities.CanEdit || !catalog.Contents[0].Capabilities.CanConfigureSchedule {
		t.Fatal("busy capabilities are wrong")
	}
	save.ExpectedConfigRevision = 1
	changed := "Changed next-round requirement"
	save.Spec.Requirement = &changed
	save.Schedule = nil
	saved, err = s.SaveGenerationConfiguration(ctx, "s", 1, "owner", target.ID(), save)
	requireWriteOK(t, err)
	if saved.Configuration.Revision != 2 {
		t.Fatal("configuration cannot be edited during a run")
	}
	completed, err := s.ExecuteFullGeneration(ctx, run.ID, confirmedExecutor(func(_ context.Context, r model.SummaryGenerationRun, input FrozenGenerationInput) (FullGenerationOutput, error) {
		if *input.Spec.Requirement == changed || r.ConfigRevision != 1 || !input.Time.End.Equal(run.EffectiveAt) {
			t.Fatal("mutable configuration leaked into run")
		}
		return FullGenerationOutput{Content: "Fresh [1]", Citations: []model.Citation{{Index: 1, Content: "new evidence"}}, Model: "fixture", MsgCount: 2, Tokens: 3}, nil
	}))
	requireWriteOK(t, err)
	if !completed.Applied || completed.Scope != "task" {
		t.Fatal("full result did not apply to task-scoped run")
	}
	version, err := s.Version(ctx, "s", 1, "owner", target.ID(), completed.OutputVersionID)
	requireWriteOK(t, err)
	var snapshot model.SummaryGenerationSnapshot
	requireWriteOK(t, json.Unmarshal(version.GenerationSpecSnapshot, &snapshot))
	if snapshot.ConfigRevision != 1 || *snapshot.Spec.Requirement == changed || version.OperationType != "regenerate" || version.Version != 2 {
		t.Fatalf("wrong formal version snapshot: %+v", version)
	}
	// A repeat completion can never append a second output.
	_, err = s.CompleteFullGeneration(ctx, run.ID, completed.ExecutionToken, FullGenerationOutput{Content: "Fresh", Model: "fixture"})
	requireWriteOK(t, err)
	if contentRowCount(t, db, target) != 2 {
		t.Fatal("duplicate completion appended version")
	}
	request.ContentBaseline, request.ExpectedConfigRevision, request.IdempotencyKey = baselineOf(version), 2, "failure"
	failed, err := s.QueueRegenerate(ctx, "s", 1, "owner", target.ID(), request)
	requireWriteOK(t, err)
	_, err = s.ExecuteFullGeneration(ctx, failed.ID, confirmedExecutor(func(context.Context, model.SummaryGenerationRun, FrozenGenerationInput) (FullGenerationOutput, error) {
		return FullGenerationOutput{}, errors.New("synthetic provider failure")
	}))
	requireContentCode(t, err, "generation_execution_failed")
	catalog, err = s.Catalog(ctx, "s", 1, "owner")
	requireWriteOK(t, err)
	if catalog.Contents[0].CurrentVersion.VersionID != version.VersionID || catalog.Contents[0].GenerationConfig.State != "complete" {
		t.Fatal("failure lost content/configuration")
	}
}

func exerciseGenerationSchedule(t *testing.T, db *gorm.DB) {
	t.Helper()
	ctx := context.Background()
	target, _ := seedWritableContent(t, db, 1, ContentPersonal)
	s := configuredService(db)
	_, err := s.SaveGenerationConfiguration(ctx, "s", 1, "owner", target.ID(), SaveGenerationConfigurationRequest{
		Spec: confirmedSpec(), Schedule: &GenerationScheduleConfig{Enabled: true, IntervalDays: 7, RunTime: "09:00"},
	})
	requireWriteOK(t, err)
	var task model.SummaryTask
	requireWriteOK(t, db.First(&task, 1).Error)
	first := time.Date(2026, 8, 10, 9, 0, 0, 0, timezone.Location())
	requireWriteOK(t, db.Model(&model.SummarySchedule{}).Where("id = ?", *task.ScheduleID).Update("next_run_at", first).Error)
	now := first.AddDate(0, 0, 23)
	run, err := s.QueueScheduledGeneration(ctx, *task.ScheduleID, now)
	requireWriteOK(t, err)
	expected := first.AddDate(0, 0, 21)
	if run == nil || run.ScheduledFor == nil || !run.EffectiveAt.Equal(expected) || run.OperationType != "scheduled_generate" {
		t.Fatalf("wrong late-slot anchor: %+v", run)
	}
	var input FrozenGenerationInput
	requireWriteOK(t, json.Unmarshal(run.InputJSON, &input))
	if !input.Time.End.Equal(expected) || !input.Time.Start.Equal(expected.AddDate(0, 0, -7)) {
		t.Fatal("worker start time changed the range")
	}
	retry, err := s.QueueScheduledGeneration(ctx, *task.ScheduleID, now)
	requireWriteOK(t, err)
	if retry != nil {
		t.Fatal("same slot admitted twice")
	}
	// Busy next slot is skipped without destroying the current personal row.
	next := expected.AddDate(0, 0, 7)
	_, err = s.QueueScheduledGeneration(ctx, *task.ScheduleID, next)
	requireWriteOK(t, err)
	var rows []model.PersonalResult
	requireWriteOK(t, db.Where("task_id = ?", 1).Find(&rows).Error)
	if len(rows) != 1 || rows[0].Content != "original [7]" || rows[0].CurrentVersionID == nil {
		t.Fatal("schedule reset the stable content")
	}
	end, err := s.successfulGenerationEnd(1, 1)
	requireWriteOK(t, err)
	if end != nil {
		t.Fatal("admission/skip advanced successful watermark")
	}
	requireWriteOK(t, s.CancelGeneration(ctx, "s", 1, "owner", target.ID(), run.ID))
	_, err = s.SaveGenerationConfiguration(ctx, "s", 1, "owner", target.ID(), SaveGenerationConfigurationRequest{
		ExpectedConfigRevision: 1, Spec: confirmedSpec(), Schedule: &GenerationScheduleConfig{Enabled: false},
	})
	requireWriteOK(t, err)
	paused, err := s.QueueScheduledGeneration(ctx, *task.ScheduleID, next.AddDate(0, 0, 8))
	requireWriteOK(t, err)
	if paused != nil {
		t.Fatal("paused schedule ran")
	}
}

func TestGenerationConfigurationMySQL(t *testing.T) {
	exerciseGenerationConfiguration(t, contentMySQLDB(t, false))
}

func TestGenerationScheduleMySQL(t *testing.T) {
	exerciseGenerationSchedule(t, contentMySQLDB(t, false))
}

func TestGenerationConfigurationMySQLAtomicCAS(t *testing.T) {
	db := contentMySQLDB(t, false)
	target, current := seedWritableContent(t, db, 1, ContentPersonal)
	s, ctx := configuredService(db), context.Background()
	request := SaveGenerationConfigurationRequest{Spec: confirmedSpec(), Generate: &RegenerateContentRequest{
		ContentBaseline: baselineOf(current), ExpectedConfigRevision: 0, IdempotencyKey: "atomic-save-generate",
	}}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			saved, err := s.SaveGenerationConfiguration(ctx, "s", 1, "owner", target.ID(), request)
			if err != nil || saved.Generation == nil {
				t.Errorf("idempotent atomic save: %v", err)
				return
			}
			successes.Add(1)
		}()
	}
	wg.Wait()
	var task model.SummaryTask
	requireWriteOK(t, db.First(&task, 1).Error)
	var count int64
	requireWriteOK(t, db.Model(&model.SummaryGenerationRun{}).Count(&count).Error)
	if successes.Load() != 12 || task.ConfigRevision != 1 || count != 1 {
		t.Fatalf("save/generate not atomic: success=%d revision=%d runs=%d", successes.Load(), task.ConfigRevision, count)
	}
}

func TestGenerationScheduleMySQLPreservesPhaseAndLegacyCron(t *testing.T) {
	db := contentMySQLDB(t, false)
	target, _ := seedWritableContent(t, db, 1, ContentPersonal)
	s, ctx := configuredService(db), context.Background()
	request := SaveGenerationConfigurationRequest{Spec: confirmedSpec(),
		Schedule: &GenerationScheduleConfig{Enabled: true, IntervalDays: 7, RunTime: "09:00"}}
	saved, err := s.SaveGenerationConfiguration(ctx, "s", 1, "owner", target.ID(), request)
	requireWriteOK(t, err)
	var task model.SummaryTask
	requireWriteOK(t, db.First(&task, 1).Error)
	due := timezone.Now().AddDate(0, 0, -14).Truncate(time.Second)
	requireWriteOK(t, db.Model(&model.SummarySchedule{}).Where("id = ?", *task.ScheduleID).Update("next_run_at", due).Error)
	request.ExpectedConfigRevision = saved.Configuration.Revision
	changed := "Updated requirements; recurrence unchanged"
	request.Spec.Requirement = &changed
	saved, err = s.SaveGenerationConfiguration(ctx, "s", 1, "owner", target.ID(), request)
	requireWriteOK(t, err)
	if !saved.Configuration.NextRunAt.Equal(due) {
		t.Fatal("configuration-only edit reset an overdue schedule")
	}
	requireWriteOK(t, db.Model(&model.SummarySchedule{}).Where("id = ?", *task.ScheduleID).
		Updates(map[string]any{"interval_days": 0, "cron_expr": "0 9 * * 1"}).Error)
	config, err := s.GenerationConfiguration(ctx, "s", 1, "owner", target.ID())
	requireWriteOK(t, err)
	request.ExpectedConfigRevision, request.Schedule = config.Revision, config.Schedule
	saved, err = s.SaveGenerationConfiguration(ctx, "s", 1, "owner", target.ID(), request)
	requireWriteOK(t, err)
	if saved.Configuration.Schedule.CronExpr != "0 9 * * 1" || !saved.Configuration.NextRunAt.Equal(due) {
		t.Fatal("legacy cron was changed by configuration save")
	}
	request.ExpectedConfigRevision = saved.Configuration.Revision
	request.Schedule.CronExpr = "0 8 * * 1"
	_, err = s.SaveGenerationConfiguration(ctx, "s", 1, "owner", target.ID(), request)
	requireContentCode(t, err, "invalid_schedule_recurrence")
}
