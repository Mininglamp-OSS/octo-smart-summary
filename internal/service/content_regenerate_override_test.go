package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// A one-shot topic override must run against its ad-hoc requirement without
// touching the saved configuration, and must never move the scheduled series'
// incremental watermark. A plain regenerate of the same configuration still
// does move it — that contrast is what proves the exclusion is real.
func TestQueueRegenerateOneShotOverrideIsFrozenAndExcludedFromWatermark(t *testing.T) {
	db := contentTestDB(t)
	ctx := context.Background()
	target, current := seedWritableContent(t, db, 1, ContentPersonal)
	requireWriteOK(t, db.AutoMigrate(&model.SummarySource{}))
	s := configuredService(db)

	save := SaveGenerationConfigurationRequest{Spec: confirmedSpec(), Schedule: &GenerationScheduleConfig{Enabled: true, IntervalDays: 7, RunTime: "09:00"}}
	saved, err := s.SaveGenerationConfiguration(ctx, "s", 1, "owner", target.ID(), save)
	requireWriteOK(t, err)
	if saved.Configuration.Revision != 1 {
		t.Fatalf("configuration revision = %d, want 1", saved.Configuration.Revision)
	}
	savedRequirement := *save.Spec.Requirement

	// An empty override is a client error and must not admit a run.
	empty := ""
	_, err = s.QueueRegenerate(ctx, "s", 1, "owner", target.ID(), RegenerateContentRequest{
		ContentBaseline: baselineOf(current), ExpectedConfigRevision: 1, IdempotencyKey: "empty", RequirementOverride: &empty,
	})
	requireContentCode(t, err, "invalid_generation_configuration")

	override := "只汇总本周风险，忽略日常同步"
	run, err := s.QueueRegenerate(ctx, "s", 1, "owner", target.ID(), RegenerateContentRequest{
		ContentBaseline: baselineOf(current), ExpectedConfigRevision: 1, IdempotencyKey: "override", RequirementOverride: &override,
	})
	requireWriteOK(t, err)
	if run.OperationType != regenerateOverrideOperation {
		t.Fatalf("operation_type = %q, want %q", run.OperationType, regenerateOverrideOperation)
	}
	var frozen FrozenGenerationInput
	requireWriteOK(t, json.Unmarshal(run.InputJSON, &frozen))
	if frozen.Spec.Requirement == nil || *frozen.Spec.Requirement != override {
		t.Fatalf("frozen requirement = %v, want the one-shot override", frozen.Spec.Requirement)
	}

	// The saved configuration is untouched by the one-shot topic.
	var task model.SummaryTask
	requireWriteOK(t, db.First(&task, 1).Error)
	if task.Topic != savedRequirement || task.ConfigRevision != 1 {
		t.Fatalf("override mutated the saved configuration: topic=%q revision=%d", task.Topic, task.ConfigRevision)
	}

	completed, err := s.ExecuteFullGeneration(ctx, run.ID, confirmedExecutor(func(_ context.Context, r model.SummaryGenerationRun, input FrozenGenerationInput) (FullGenerationOutput, error) {
		if input.Spec.Requirement == nil || *input.Spec.Requirement != override {
			t.Fatalf("executor did not receive the one-shot topic: %v", input.Spec.Requirement)
		}
		return FullGenerationOutput{Content: "Risk-only [1]", Citations: []model.Citation{{Index: 1, Content: "risk evidence"}}, Model: "fixture", MsgCount: 2, Tokens: 3}, nil
	}))
	requireWriteOK(t, err)
	if !completed.Applied {
		t.Fatal("override run did not apply")
	}
	overrideVersion, err := s.Version(ctx, "s", 1, "owner", target.ID(), completed.OutputVersionID)
	requireWriteOK(t, err)
	if overrideVersion.OperationType != regenerateOverrideOperation {
		t.Fatalf("version operation = %q, want %q", overrideVersion.OperationType, regenerateOverrideOperation)
	}

	// The override run is APPLIED and completed, yet contributes no watermark:
	// an ad-hoc topic cannot consume the scheduled series' incremental start.
	watermark, err := s.successfulGenerationEnd(1, 1)
	requireWriteOK(t, err)
	if watermark != nil {
		t.Fatalf("one-shot override moved the watermark to %v", watermark)
	}

	// A plain regenerate of the same configuration DOES set the watermark.
	plain, err := s.QueueRegenerate(ctx, "s", 1, "owner", target.ID(), RegenerateContentRequest{
		ContentBaseline: baselineOf(overrideVersion), ExpectedConfigRevision: 1, IdempotencyKey: "plain",
	})
	requireWriteOK(t, err)
	if plain.OperationType != "regenerate" {
		t.Fatalf("operation_type = %q, want regenerate", plain.OperationType)
	}
	plainCompleted, err := s.ExecuteFullGeneration(ctx, plain.ID, confirmedExecutor(func(context.Context, model.SummaryGenerationRun, FrozenGenerationInput) (FullGenerationOutput, error) {
		return FullGenerationOutput{Content: "Full [1]", Citations: []model.Citation{{Index: 1, Content: "full evidence"}}, Model: "fixture", MsgCount: 2, Tokens: 3}, nil
	}))
	requireWriteOK(t, err)
	if !plainCompleted.Applied {
		t.Fatal("plain regenerate did not apply")
	}
	watermark, err = s.successfulGenerationEnd(1, 1)
	requireWriteOK(t, err)
	var plainFrozen FrozenGenerationInput
	requireWriteOK(t, json.Unmarshal(plain.InputJSON, &plainFrozen))
	if watermark == nil || !watermark.Equal(plainFrozen.Time.End) {
		t.Fatalf("plain regenerate did not set the watermark: got %v want %v", watermark, plainFrozen.Time.End)
	}
}
