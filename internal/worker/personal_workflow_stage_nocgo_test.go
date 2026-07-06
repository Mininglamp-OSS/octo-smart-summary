//go:build !cgo

package worker

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

func TestCompletedPersonalResultUpdates_WorkflowStageNoCGO(t *testing.T) {
	now := time.Now()
	full := completedPersonalResultUpdates(model.PersonalResult{}, "content", nil, 3, 10, "model", now, false)
	if full["workflow_stage"] != model.WorkflowStageGenerateSummary {
		t.Fatalf("full workflow_stage=%v", full["workflow_stage"])
	}
	skip := completedPersonalResultUpdates(model.PersonalResult{}, "content", nil, 3, 10, "model", now, true)
	if skip["workflow_stage"] != model.WorkflowStageGenerateSummary {
		t.Fatalf("skip workflow_stage=%v", skip["workflow_stage"])
	}
}
