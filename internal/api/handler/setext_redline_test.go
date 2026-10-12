//go:build cgo

package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

// RED round-2 for PR#268 — reviewer entry-point probes codified as handler
// tests (octo-spec cr-convergence lesson 8/9 + 18续):
//
//   - M-P1 (B-2, Jerry-Xin 🔴; yujiawei §1(a); mocha C1): both personal-refine
//     transports must persist the NEUTRALIZED form of a model-emitted
//     "lead-in\n---\n" shape. At head fc98e22 neither path calls
//     NormalizeSetextHeadings, so the persisted PersonalResult.content keeps
//     the setext form. Identity-replacing the fix's normalize calls must turn
//     this test red again.
//   - M-P2 (B-3, yujiawei 2.1): the workspace-save branch must persist the
//     neutralized preview content; at head, agent_summary.go:622 overwrites
//     the :512 normalization with the raw locked payload.
//
// Each test must FAIL at head fc98e22 and PASS on the fix commit.

const setextRedlineLeadIn = "现将进展整理如下：\n---\n### 一、已完成事项\n"

func TestPersonalRefineNeutralizesSetextHeadingsBothTransports(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					delta, _ := json.Marshal(map[string]interface{}{"choices": []interface{}{
						map[string]interface{}{"delta": map[string]string{"content": setextRedlineLeadIn}},
					}})
					fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", delta)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"choices": []interface{}{
					map[string]interface{}{"message": map[string]string{"content": setextRedlineLeadIn}, "finish_reason": "stop"},
				}})
			}))
			defer srv.Close()
			db := setupPersonalRefineDB(t)
			task, _, pr := seedScheduledMultiPersonPersonalTask(t, db)
			h := NewPersonalHandler(db, "", nil)
			h.SetLLM(service.NewLLMClient(srv.URL, "test", "test", 5, 256, false, 5, nil))
			r := setupPersonalRefineRouter(h)
			r.POST("/api/v1/summaries/:id/personal-refine-stream", h.RefinePersonalSummaryStream)
			path := fmt.Sprintf("/api/v1/summaries/%d/personal-refine", task.ID)
			if stream {
				path += "-stream"
			}
			req := httptest.NewRequest("POST", path, strings.NewReader(`{"feedback":"adjust","base_version":1}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Token", "member_a")
			req.Header.Set("X-Space-Id", "space1")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			var saved model.PersonalResult
			if err := db.First(&saved, pr.ID).Error; err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(saved.Content, "如下：\n\n---") {
				t.Fatalf("M-P1 (stream=%t): persisted content not neutralized, setext form survived: %q (status=%d body=%s)",
					stream, saved.Content, w.Code, w.Body.String())
			}
		})
	}
}

func TestCreateAgentSummary_WorkspaceSavePersistsNeutralizedContent(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	fixture := seedWorkspaceSaveFixture(t, db, "workspace-setext-save")
	if err := db.Create(&model.AgentMessage{
		SpaceID: "test-space", UserID: "test-user", SessionID: fixture.Session.SessionID,
		Role: "user", Content: "请生成总结",
	}).Error; err != nil {
		t.Fatalf("seed workspace user message: %v", err)
	}
	// Point the saved preview payload at the setext shape: the deliverable
	// persisted from workspace mode must be the neutralized bytes.
	payloadJSON, err := json.Marshal(agent.SummaryResponsePayload{
		ResultType:      agent.SummaryResultAgentPreview,
		Reply:           "已生成预览。",
		ExecutionTarget: "agent_preview",
		Preview: &agent.SummaryResponsePreview{
			Content: "开场说明不应保存\n\n现将进展整理如下：\n---\n### 一、已完成事项",
			Version: 3,
		},
	})
	if err != nil {
		t.Fatalf("marshal workspace payload: %v", err)
	}
	payload := string(payloadJSON)
	fixture.Message.ResponsePayload = &payload
	if err := db.Save(&fixture.Message).Error; err != nil {
		t.Fatalf("update preview payload: %v", err)
	}

	h := NewAgentSummaryHandler(db, nil, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)
	w := doAgentSave(t, r, fixture.Body, map[string]string{"Idempotency-Key": "workspace-setext-key"})
	if w.Code != http.StatusOK {
		t.Fatalf("workspace save want 200, got %d: %s", w.Code, w.Body.String())
	}

	var task model.SummaryTask
	if err := db.Where("creator_id = ?", "test-user").Take(&task).Error; err != nil {
		t.Fatalf("load saved task: %v", err)
	}
	var result model.PersonalResult
	if err := db.Where("task_id = ? AND user_id = ?", task.ID, "test-user").Take(&result).Error; err != nil {
		t.Fatalf("load saved result: %v", err)
	}
	want := "开场说明不应保存\n\n现将进展整理如下：\n\n---\n### 一、已完成事项"
	if result.Content != want {
		t.Fatalf("M-P2: workspace-save persisted raw setext content:\n got=%q\nwant=%q", result.Content, want)
	}
}

// M6a mutation-lock (Jerry-Xin mutation battery: identity-replacing both
// edit.go normalize calls kept the full 727-test handler suite green — the
// team-refine transports had zero wiring coverage). Pins that both team
// refine transports persist the neutralized form. Passes today (edit.go
// carried the call from round 1); it exists to die under the mutation.
func TestRefineTeamNeutralizesSetextHeadingsBothTransports(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					delta, _ := json.Marshal(map[string]interface{}{"choices": []interface{}{
						map[string]interface{}{"delta": map[string]string{"content": setextRedlineLeadIn}},
					}})
					fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", delta)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"choices": []interface{}{
					map[string]interface{}{"message": map[string]string{"content": setextRedlineLeadIn}, "finish_reason": "stop"},
				}})
			}))
			defer srv.Close()
			llm := service.NewLLMClient(srv.URL, "test", "test", 5, 1024, false, 5, nil)
			db := setupEditDB(t)
			id, resultID, prID := seedEditableTask(t, db)
			var base model.SummaryResult
			db.First(&base, resultID)
			base.SetTeamCitations([]model.TeamCitation{{Index: 1, UserID: "creator1"}})
			db.Save(&base)
			if err := db.Model(&model.PersonalResult{}).Where("id = ?", prID).Update("citations_json", base.CitationsJSON).Error; err != nil {
				t.Fatal(err)
			}
			h := NewEditHandler(db, llm)
			r := setupEditRouter(h)
			path := fmt.Sprintf("/api/v1/summaries/%d/refine", id)
			if stream {
				r.POST("/api/v1/summaries/:id/refine-stream", h.RefineSummaryStream)
				path += "-stream"
			}
			w := doJSONRequest(r, "POST", path, "creator1", map[string]interface{}{"feedback": "保留正文", "base_result_id": resultID})
			var saved model.SummaryResult
			db.Order("id DESC").First(&saved)
			if !strings.Contains(saved.Content, "如下：\n\n---") {
				t.Fatalf("M6a (stream=%t): team refine persisted content not neutralized: %q (status=%d body=%s)",
					stream, saved.Content, w.Code, w.Body.String())
			}
		})
	}
}

// M7 pin (Jerry-Xin round-2 A-2, PR#268): the LEGACY (non-workspace)
// agent-save branch had no setext pin — only the workspace branch was pinned —
// so removing the pre-transaction normalization at agent_summary.go:512 kept
// the shipped suite green (r1 mutant M7 survivor). Passes today; exists to
// die under the mutation.
func TestCreateAgentSummary_LegacySavePersistsNeutralizedContent(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	h := NewAgentSummaryHandler(db, nil, "", "", "", 0, 0)
	r := setupAgentSummaryRouter(h)
	sessionID := "session-setext-legacy"
	// The setext shape sits mid-document: stripAgentPreamble (owner decision
	// Q3=A) treats text before the FIRST heading/rule as strippable preamble,
	// so a lead-in+\n--- at the very top would be stripped before the
	// normalizer ever runs. A heading-first document with a body setext shape
	// is the realistic legacy-save exposure for the :512 call.
	db.Create(&model.AgentMessage{
		UserID:    "test-user",
		SessionID: sessionID,
		Role:      "assistant",
		Content:   "### 一、已完成事项\n\n现将进展整理如下：\n---\n### 二、待办事项\n",
	})
	w := doAgentSave(t, r, map[string]interface{}{
		"session_id":          sessionID,
		"origin_channel_id":   "CH-SETEXT-LEGACY",
		"origin_channel_type": 2,
		"title":               "Legacy Setext Save",
	}, map[string]string{"Idempotency-Key": "legacy-setext-key"})
	if w.Code != http.StatusOK {
		t.Fatalf("legacy agent save want 200, got %d: %s", w.Code, w.Body.String())
	}
	var task model.SummaryTask
	if err := db.Where("creator_id = ?", "test-user").Take(&task).Error; err != nil {
		t.Fatalf("load saved task: %v", err)
	}
	var result model.PersonalResult
	if err := db.Where("task_id = ? AND user_id = ?", task.ID, "test-user").Take(&result).Error; err != nil {
		t.Fatalf("load saved result: %v", err)
	}
	if !strings.Contains(result.Content, "如下：\n\n---") {
		t.Fatalf("M7: legacy agent save persisted raw setext content: %q", result.Content)
	}
}

// A-13 boundary pins (Jerry-Xin r4 §5, yujiawei r4 P2-1, mocha r4 finding 1;
// Jerry-Xin r5 N-2 loop-over-transports fix; PR#268): the refine size gate
// must measure the STRIPPED, PRE-NORMALIZE length. At head 695e3af, gate 1
// measures the raw response (pre-strip) and gate 2 re-measures the
// post-normalize, post-citation length — both bases disagree with the
// prescription. The r5 N-2 finding was that the initial boundary pin
// exercised only the edit non-stream transport, leaving M-G1-RAW-STREAM
// alive on the three sibling sites; the pin now loops over both edit
// transports (sync + stream) to close that pin-gap.
//
//   - exactly_at_cap_with_rule: a raw response of EXACTLY maxContentBytes
//     bytes containing one normalizable rule must be accepted (kills the
//     gate-2 revert mutant).
//   - fence_overhead_at_cap: a raw response of cap + fence overhead bytes
//     whose stripped body is exactly the cap must be accepted (kills the
//     gate-1 raw-basis revert mutant, on both transports).
func TestRefineSizeGateMeasuresStrippedPreNormalizeLength(t *testing.T) {
	const cap = maxContentBytes // 500 * 1024
	leadIn := "现将进展整理如下：\n---\n### 一、已完成事项\n"

	transports := []struct {
		name   string
		stream bool
	}{
		{"sync", false},
		{"stream", true},
	}
	cases := []struct {
		name    string
		rawBody string // the body the model "returned", inside or outside a fence
		reason  string // what this leg pins (for failure diagnostics)
	}{
		{
			name:    "exactly_at_cap_with_rule",
			rawBody: buildSetextBoundaryBody(cap, leadIn),
			reason:  "raw body exactly at cap must persist (gate 2 revert mutant lives here)",
		},
		{
			name:    "fence_overhead_at_cap",
			rawBody: "```markdown\n" + buildSetextBoundaryBody(cap, leadIn) + "\n```",
			reason:  "fence-wrapped body whose stripped bytes equal cap must persist (gate 1 raw-basis revert mutant lives here)",
		},
	}
	for _, tp := range transports {
		for _, tc := range cases {
			t.Run(tp.name+"/"+tc.name, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if tp.stream {
						w.Header().Set("Content-Type", "text/event-stream")
						delta, _ := json.Marshal(map[string]interface{}{"choices": []interface{}{
							map[string]interface{}{"delta": map[string]string{"content": tc.rawBody}, "finish_reason": "stop"},
						}})
						fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", delta)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"choices": []interface{}{
						map[string]interface{}{"message": map[string]string{"content": tc.rawBody}, "finish_reason": "stop"},
					}})
				}))
				defer srv.Close()
				llm := service.NewLLMClient(srv.URL, "test", "test", 5, 1024, false, 5, nil)
				db := setupEditDB(t)
				id, resultID, _ := seedEditableTask(t, db)
				h := NewEditHandler(db, llm)
				r := setupEditRouter(h)
				path := fmt.Sprintf("/api/v1/summaries/%d/refine", id)
				if tp.stream {
					r.POST("/api/v1/summaries/:id/refine-stream", h.RefineSummaryStream)
					path += "-stream"
				}
				w := doJSONRequest(r, "POST", path, "creator1", map[string]interface{}{"feedback": "保留正文", "base_result_id": resultID})
				if w.Code != http.StatusOK {
					t.Fatalf("A-13 %s/%s: %s; refine got %d: %s", tp.name, tc.name, tc.reason, w.Code, w.Body.String())
				}
				var saved model.SummaryResult
				db.Order("id DESC").First(&saved)
				if !strings.Contains(saved.Content, "如下：\n\n---") {
					t.Fatalf("A-13 %s/%s: refined body not persisted with neutralized shape: %q", tp.name, tc.name, saved.Content)
				}
			})
		}
	}
}

// buildSetextBoundaryBody constructs a UTF-8 body of exactly want bytes whose
// head is leadIn (containing one normalizable "text\n---\n" setext shape) and
// whose tail is filler long enough to reach the target size. The filler is
// ASCII so byte length is exact; residue is absorbed by a final partial line.
func buildSetextBoundaryBody(want int, leadIn string) string {
	base := []byte(leadIn)
	if len(base) >= want {
		return string(base[:want])
	}
	filler := want - len(base)
	fullLines := filler / 100
	residue := filler % 100
	var b strings.Builder
	b.Write(base)
	for i := 0; i < fullLines; i++ {
		b.WriteString(strings.Repeat("a", 99) + "\n")
	}
	if residue > 0 {
		b.WriteString(strings.Repeat("a", residue))
	}
	out := b.String()
	if len(out) != want {
		panic(fmt.Sprintf("boundary fixture builder produced %d bytes, want %d", len(out), want))
	}
	return out
}
