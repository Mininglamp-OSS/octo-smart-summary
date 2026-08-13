//go:build cgo

package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

// stripFencedRegions removes every <引用数据>…</引用数据> region from ctx,
// returning only the instruction-region text (everything outside the fences).
func stripFencedRegions(ctx string) string {
	var sb strings.Builder
	rest := ctx
	for {
		i := strings.Index(rest, refDataOpen)
		if i < 0 {
			sb.WriteString(rest)
			break
		}
		sb.WriteString(rest[:i])
		j := strings.Index(rest[i:], refDataClose)
		if j < 0 {
			sb.WriteString(rest[i:])
			break
		}
		rest = rest[i+j+len(refDataClose):]
	}
	return sb.String()
}

// fencedRegions extracts the text between each <引用数据>…</引用数据> pair.
func fencedRegions(t *testing.T, ctx string) []string {
	t.Helper()
	var regions []string
	rest := ctx
	for {
		i := strings.Index(rest, refDataOpen)
		if i < 0 {
			break
		}
		j := strings.Index(rest[i+len(refDataOpen):], refDataClose)
		if j < 0 {
			t.Fatalf("unterminated data fence in context")
		}
		regions = append(regions, rest[i+len(refDataOpen):i+len(refDataOpen)+j])
		rest = rest[i+len(refDataOpen)+j+len(refDataClose):]
	}
	return regions
}

// TestBuildReferencedSummariesContext_FenceGuard is the round-7 P1-1
// regression guard (R7 yj blocking). The header contract declares everything
// between <引用数据> and </引用数据> to be inert data that the model must
// never obey. Two boundary properties are asserted structurally:
//
//  1. No model-addressed directive (operator-voice guidance) may appear
//     INSIDE any fence — otherwise the same prompt that says "ignore the
//     fence" also places real instructions inside it, destroying the signal
//     that separates legitimate directives from forged ones (and silently
//     disabling the channel_type-copy / stale-window mitigations).
//  2. No untrusted interpolated value (title, requirement, channel id,
//     freshness note, bodies, source names) may appear OUTSIDE the fences —
//     the mirror image of P1-1 (R7 P2-6: an imperative task title rendered
//     in the instruction region).
//
// Both branches (snapshot / traditional) are seeded so the guard covers the
// whole builder.
func TestBuildReferencedSummariesContext_FenceGuard(t *testing.T) {
	db := setupResolverTestDB(t)

	// Agent-style task: personal result + snapshot → snapshot branch.
	snapTask := model.SummaryTask{
		TaskNo:            "TST-FENCE-SNAP",
		SpaceID:           "space1",
		CreatorID:         "creator1",
		Title:             "INJECT-TITLE-SNAP",
		SummaryMode:       model.ModeByPerson,
		Status:            model.StatusCompleted,
		TriggerType:       model.TriggerAgent,
		OriginChannelType: model.OriginChannelGroup,
	}
	if err := db.Create(&snapTask).Error; err != nil {
		t.Fatalf("create snapshot task: %v", err)
	}
	snap := &model.Snapshot{
		SnapshotVersion: 1,
		TaskID:          snapTask.ID,
		ContentVersion:  1,
		Requirement:     "INJECT-REQUIREMENT",
		Scope: model.SnapshotScope{
			ChannelIDs: []string{"INJECT-CHANNEL"},
			TimeRange: model.TimeRangeJSON{
				Start: "2026-01-01T00:00:00Z",
				End:   "2026-01-02T00:00:00Z",
			},
		},
		ToolSummary:       []string{"fetch_channel x 1"},
		DataFreshnessNote: "INJECT-FRESHNESS",
	}
	pr := model.PersonalResult{TaskID: snapTask.ID, UserID: "creator1", Content: "INJECT-BODY"}
	pr.SetSnapshot(snap)
	if err := db.Create(&pr).Error; err != nil {
		t.Fatalf("create personal result: %v", err)
	}

	// Traditional task: team result, no snapshot → traditional branch.
	tradTask := model.SummaryTask{
		TaskNo:      "TST-FENCE-TRAD",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		Title:       "INJECT-TITLE-TRAD",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
		TriggerType: model.TriggerManual,
	}
	if err := db.Create(&tradTask).Error; err != nil {
		t.Fatalf("create traditional task: %v", err)
	}
	addTeamResult(t, db, tradTask.ID, "INJECT-TEAM-BODY")
	if err := db.Create(&model.SummarySource{
		TaskID:     tradTask.ID,
		SourceType: model.SourceGroup,
		SourceID:   "grp_inject",
		SourceName: "INJECT-SOURCE-NAME",
	}).Error; err != nil {
		t.Fatalf("create summary source: %v", err)
	}

	ctx, loaded := buildReferencedSummariesContext(
		context.Background(), db, "space1", "creator1",
		[]int64{snapTask.ID, tradTask.ID})
	if len(loaded) != 2 {
		t.Fatalf("expected 2 loaded tasks, got %d", len(loaded))
	}

	if strings.Count(ctx, refDataOpen) != strings.Count(ctx, refDataClose) {
		t.Fatalf("unbalanced data fence: %d open vs %d close",
			strings.Count(ctx, refDataOpen), strings.Count(ctx, refDataClose))
	}

	regions := fencedRegions(t, ctx)
	if len(regions) < 4 {
		t.Fatalf("expected >= 4 fenced regions (snap meta/body, trad meta/body), got %d", len(regions))
	}
	outside := stripFencedRegions(ctx)

	// Property 1: directives live OUTSIDE the fence (and are still emitted —
	// the guard must not pass vacuously by deleting them).
	directives := []string{
		"(你可以复用其中一个",
		"原样复制**上面的 channel_type",
		"(若用户说'最新/今天/最近'",
		"以上时间已过期",
		"(仅供参考,不得自动作为新工具调用参数)",
	}
	for _, d := range directives {
		if !strings.Contains(ctx, d) {
			t.Errorf("directive %q missing from output entirely — it must still be emitted (outside the fence)", d)
			continue
		}
		if !strings.Contains(outside, d) {
			t.Errorf("directive %q not found in the instruction region (outside fences)", d)
		}
		for ri, region := range regions {
			if strings.Contains(region, d) {
				t.Errorf("P1-1: directive %q appears INSIDE fenced region %d — the fence contract says nothing inside is an instruction", d, ri)
			}
		}
	}

	// Property 2: untrusted values stay INSIDE the fence (P2-6 mirror: the
	// task title used to render in the instruction-region header line).
	untrusted := []string{
		"INJECT-TITLE-SNAP",
		"INJECT-TITLE-TRAD",
		"INJECT-REQUIREMENT",
		"INJECT-CHANNEL",
		"INJECT-FRESHNESS",
		"INJECT-BODY",
		"INJECT-TEAM-BODY",
		"INJECT-SOURCE-NAME",
	}
	for _, u := range untrusted {
		if !strings.Contains(ctx, u) {
			t.Errorf("untrusted value %q missing from output entirely — expected it fenced as data", u)
			continue
		}
		if strings.Contains(outside, u) {
			t.Errorf("P2-6: untrusted value %q appears OUTSIDE the data fence (instruction region)", u)
		}
	}
}

// R11 Q8 (yujiawei, review 4929031900): "Empty `- 数据来源:` header when
// every source row is derived ... the prompt gets a dangling
// `- 数据来源:` label with nothing under it." The header must be emitted
// only when at least one non-derived source survives the filter.
func TestBuildReferencedSummariesContext_NoDanglingSourceHeader(t *testing.T) {
	db := setupResolverTestDB(t)

	task := model.SummaryTask{
		TaskNo:      "TST-CTX-DANGLE",
		SpaceID:     "space1",
		CreatorID:   "creator1",
		Title:       "all derived",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
		TriggerType: model.TriggerManual,
	}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	addTeamResult(t, db, task.ID, "TEAM-BODY")
	if err := db.Create(&model.SummarySource{
		TaskID: task.ID, SourceType: model.SourceGroup,
		SourceID: "grp_auto", SourceName: "自动回填来源", Derived: true,
	}).Error; err != nil {
		t.Fatalf("create derived source: %v", err)
	}

	ctx, loaded := buildReferencedSummariesContext(
		context.Background(), db, "space1", "creator1", []int64{task.ID})
	if len(loaded) != 1 {
		t.Fatalf("expected 1 loaded task, got %d", len(loaded))
	}
	if strings.Contains(ctx, "- 数据来源:") {
		t.Errorf("dangling `- 数据来源:` header emitted with all sources derived")
	}
}
