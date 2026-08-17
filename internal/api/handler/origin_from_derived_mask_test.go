//go:build cgo

package handler

// R11 Q2 (yujiawei, review 4929031900, P1 — "Q2 needs a design decision
// rather than a patch"): tier-4 origin derivation copies a DERIVED
// summary_source row's channel id into the new task's origin_channel_id;
// list/detail then echo it back and the ?origin_channel_id= list filter
// matches on it. Concretely: Alice's auto-select summary backfills a
// private group as a derived row; Bob later joins the task and refines it,
// the refined task inherits that channel as its origin, and Bob — who is
// not a member of that group — sees the channel id in list/detail and can
// probe membership via the filter (membership oracle).
//
// Owner decision (2026-08-14, option 1 "mask"): keep the tier-3/tier-4
// origin-inheritance feature (dropping it would dead-end auto-select
// refines into 40001 again — the reason this feature exists), but an origin
// that was derived from a derived row is never echoed on the wire. The PR
// body's owner-approved invariant "Derived (auto-backfilled) sources are
// never exposed to API consumers" extends to values derived FROM derived
// rows.
//
// These RED tests are wire-level only (they never reference the provenance
// column the fix introduces) so this commit compiles against the pre-fix
// implementation and every assertion FAILs against it.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/middleware"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// setupOriginMaskWireRouter wires the two endpoints that echo
// origin_channel_id (list + detail) over the agent-summary DB so the tests
// can assert the wire shape after a refine.
func setupOriginMaskWireRouter(t *testing.T, db *gorm.DB) *gin.Engine {
	t.Helper()
	// ListSummaries' attention computation reads summary_user_read and
	// personal-result version tables that setupAgentSummaryTestDB does not
	// migrate — bring them up so list serves 200 instead of 500.
	if err := db.AutoMigrate(&model.SummaryUserRead{}, &model.PersonalResultVersion{}); err != nil {
		t.Fatalf("migrate list dependencies: %v", err)
	}
	imDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open im db: %v", err)
	}
	imDB.Exec("CREATE TABLE group_member (group_no TEXT NOT NULL, uid TEXT NOT NULL, is_deleted INTEGER DEFAULT 0)")

	h := NewTaskHandler(db, imDB, "")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.AuthMiddleware(&mockTokenResolver{}), middleware.SpaceMiddleware())
	r.GET("/api/v1/summaries", h.ListSummaries)
	r.GET("/api/v1/summaries/:id", h.GetSummary)
	return r
}

// seedDerivedOnlyRefTaskAndRefine seeds a pipeline-style task whose ONLY
// source row is a worker-backfilled derived row (a channel the refiner may
// not be a member of), then refines it through the agent endpoint. Returns
// the created (refined) task.
func seedDerivedOnlyRefTaskAndRefine(t *testing.T, db *gorm.DB, r *gin.Engine, taskNo, sessionID, channelID string) model.SummaryTask {
	t.Helper()
	now := mustSeedPipelineTask(t, db, taskNo)
	if err := db.Create(&model.SummarySource{
		TaskID:     now.ID,
		SourceType: model.SourceGroup,
		SourceID:   channelID,
		Derived:    true, // worker backfill row — never user-specified
	}).Error; err != nil {
		t.Fatalf("seed derived source: %v", err)
	}

	db.Create(&model.AgentMessage{
		UserID:    "test-user",
		SessionID: sessionID,
		Role:      "assistant",
		Content:   "Refined content.",
	})

	w := postAgentSave(r, map[string]interface{}{
		"session_id":          sessionID,
		"title":               "refine of derived-only task",
		"referenced_task_ids": []int64{now.ID},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("agent save: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var created model.SummaryTask
	if err := db.Order("id DESC").First(&created).Error; err != nil {
		t.Fatalf("load created task: %v", err)
	}
	// The feature itself must keep working (owner decision: keep tier-4):
	// internally the refined task still carries the inherited origin.
	if created.OriginChannelID != channelID {
		t.Fatalf("tier-4 stopped inheriting: origin_channel_id = %q, want %q", created.OriginChannelID, channelID)
	}
	return created
}

// mustSeedPipelineTask seeds a completed pipeline task with NO origin (the
// shape tier-3 misses and tier-4 exists for).
func mustSeedPipelineTask(t *testing.T, db *gorm.DB, taskNo string) model.SummaryTask {
	t.Helper()
	ref := model.SummaryTask{
		TaskNo:      taskNo,
		SpaceID:     "test-space",
		CreatorID:   "test-user",
		Title:       "pipeline summary",
		Topic:       "pipeline summary",
		SummaryMode: model.ModeByPerson,
		Status:      model.StatusCompleted,
		TriggerType: model.TriggerScheduled,
	}
	if err := db.Create(&ref).Error; err != nil {
		t.Fatalf("seed ref task: %v", err)
	}
	return ref
}

func detailOrigin(t *testing.T, r *gin.Engine, taskID int64) (string, int) {
	t.Helper()
	w := doRequestWithSpace(r, "GET", fmt.Sprintf("/api/v1/summaries/%d", taskID), "test-user", "test-space")
	if w.Code != http.StatusOK {
		t.Fatalf("detail: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Code int `json:"code"`
		Data struct {
			OriginChannelID   string `json:"origin_channel_id"`
			OriginChannelType int    `json:"origin_channel_type"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal detail: %v; body=%s", err, w.Body.String())
	}
	return resp.Data.OriginChannelID, resp.Data.OriginChannelType
}

// TestOriginFromDerived_DetailMasksDerivedInheritedOrigin (Q2, shape 1):
// the detail endpoint must not echo an origin that was inherited from a
// derived source row — the refiner may not be a member of that channel.
func TestOriginFromDerived_DetailMasksDerivedInheritedOrigin(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	r := setupAgentSummaryRouter(NewAgentSummaryHandler(db, nil, "", "", "", 0, 0))

	created := seedDerivedOnlyRefTaskAndRefine(t, db, r, "ST-Q2-DETAIL", "session-q2-detail", "CH-PRIVATE-ALICE")

	wire := setupOriginMaskWireRouter(t, db)
	gotID, gotType := detailOrigin(t, wire, created.ID)
	if gotID != "" || gotType != 0 {
		t.Errorf("detail leaked derived-inherited origin: (%q, %d), want (\"\", 0)", gotID, gotType)
	}
}

// TestOriginFromDerived_ListMasksDerivedInheritedOrigin (Q2, shape 2):
// same mask on the list projection.
func TestOriginFromDerived_ListMasksDerivedInheritedOrigin(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	r := setupAgentSummaryRouter(NewAgentSummaryHandler(db, nil, "", "", "", 0, 0))

	created := seedDerivedOnlyRefTaskAndRefine(t, db, r, "ST-Q2-LIST", "session-q2-list", "CH-PRIVATE-ALICE")

	wire := setupOriginMaskWireRouter(t, db)
	w := doRequestWithSpace(wire, "GET", "/api/v1/summaries", "test-user", "test-space")
	if w.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := parseListResponse(t, w)
	for _, item := range resp.Data.Items {
		m, ok := item.(map[string]interface{})
		if !ok {
			t.Fatalf("item is not an object: %v", item)
		}
		if m["task_id"] == nil || int64(m["task_id"].(float64)) != created.ID {
			continue
		}
		id, _ := m["origin_channel_id"].(string)
		typ := 0
		if v, ok := m["origin_channel_type"].(float64); ok {
			typ = int(v)
		}
		if id != "" || typ != 0 {
			t.Errorf("list leaked derived-inherited origin: (%q, %d), want (\"\", 0)", id, typ)
		}
		return
	}
	t.Fatalf("created task %d missing from list", created.ID)
}

// TestOriginFromDerived_ListFilterDoesNotMatchMaskedOrigin (Q2, shape 3):
// ?origin_channel_id= must not match a task whose origin is derived-derived
// — otherwise the filter is a membership oracle ("does some task I can see
// carry channel X as its origin?" reveals X's presence without membership).
func TestOriginFromDerived_ListFilterDoesNotMatchMaskedOrigin(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	r := setupAgentSummaryRouter(NewAgentSummaryHandler(db, nil, "", "", "", 0, 0))

	seedDerivedOnlyRefTaskAndRefine(t, db, r, "ST-Q2-FILTER", "session-q2-filter", "CH-PRIVATE-ALICE")

	wire := setupOriginMaskWireRouter(t, db)
	w := doRequestWithSpace(wire, "GET", "/api/v1/summaries?origin_channel_id=CH-PRIVATE-ALICE", "test-user", "test-space")
	if w.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	resp := parseListResponse(t, w)
	if resp.Data.Total != 0 {
		t.Errorf("filter matched a masked-origin task: total = %d, want 0 (membership oracle)", resp.Data.Total)
	}
}

// TestOriginFromDerived_FlagPropagatesThroughTier3Borrow (Q2, shape 4):
// refining the already-refined task borrows its origin via tier-3; the mask
// must propagate — the second-generation task must not re-expose the
// channel either.
func TestOriginFromDerived_FlagPropagatesThroughTier3Borrow(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	r := setupAgentSummaryRouter(NewAgentSummaryHandler(db, nil, "", "", "", 0, 0))

	first := seedDerivedOnlyRefTaskAndRefine(t, db, r, "ST-Q2-PROP", "session-q2-prop-1", "CH-PRIVATE-ALICE")

	// Second refine: references the first refined task (tier-3 borrow path —
	// the first task now HAS origin_channel_id in the DB).
	db.Create(&model.AgentMessage{
		UserID:    "test-user",
		SessionID: "session-q2-prop-2",
		Role:      "assistant",
		Content:   "Second refine.",
	})
	w := postAgentSave(r, map[string]interface{}{
		"session_id":          "session-q2-prop-2",
		"title":               "second refine",
		"referenced_task_ids": []int64{first.ID},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("second agent save: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var second model.SummaryTask
	if err := db.Order("id DESC").First(&second).Error; err != nil {
		t.Fatalf("load second task: %v", err)
	}
	if second.OriginChannelID != "CH-PRIVATE-ALICE" {
		t.Fatalf("tier-3 borrow broke: origin_channel_id = %q, want CH-PRIVATE-ALICE", second.OriginChannelID)
	}

	wire := setupOriginMaskWireRouter(t, db)
	gotID, gotType := detailOrigin(t, wire, second.ID)
	if gotID != "" || gotType != 0 {
		t.Errorf("second-generation task re-exposed masked origin: (%q, %d), want (\"\", 0)", gotID, gotType)
	}
}
