//go:build cgo

package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

func TestRefineNumericProseAcrossSurfaces(t *testing.T) {
	for _, personal := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, cited := range []bool{false, true} {
				t.Run(fmt.Sprintf("personal=%t/stream=%t/cited=%t", personal, stream, cited), func(t *testing.T) {
					content := "规划[2024-2025]，日期[2026-09-14]，页码[100-120]。"
					want := content
					var citations []model.Citation
					if cited {
						content += "依据[1,3]。"
						want += "依据[1][3]。"
						citations = []model.Citation{{Index: 1}, {Index: 3}}
					}
					if !personal {
						content += "成员[P1]。"
						want += "成员[P1]。"
					}
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							data, _ := json.Marshal(map[string]interface{}{"choices": []interface{}{
								map[string]interface{}{"delta": map[string]string{"content": content}, "finish_reason": "stop"},
							}})
							fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
						} else {
							json.NewEncoder(w).Encode(map[string]interface{}{"choices": []interface{}{
								map[string]interface{}{"message": map[string]string{"content": content}, "finish_reason": "stop"},
							}})
						}
					}))
					defer srv.Close()
					llm := service.NewLLMClient(srv.URL, "test", "test", 5, 1024, false, 5, nil)
					if personal {
						db := setupPersonalRefineDB(t)
						task, _, pr := seedScheduledMultiPersonPersonalTask(t, db)
						// Existing rows legitimately contained this prose before
						// normalization was introduced. A preserving edit must work.
						pr.Content = content
						pr.SetCitations(citations)
						db.Save(&pr)
						h := NewPersonalHandler(db, "", nil)
						h.SetLLM(llm)
						r := setupPersonalRefineRouter(h)
						path := fmt.Sprintf("/api/v1/summaries/%d/personal-refine", task.ID)
						if stream {
							r.POST("/api/v1/summaries/:id/personal-refine-stream", h.RefinePersonalSummaryStream)
							path += "-stream"
						}
						w := doJSONRequest(r, "POST", path, "member_a", map[string]interface{}{"feedback": "保留正文", "base_version": 1})
						var saved model.PersonalResult
						db.First(&saved, pr.ID)
						if w.Code != 200 || strings.Contains(w.Body.String(), "event: error") || saved.Content != want || len(saved.GetCitations()) != len(citations) {
							t.Fatalf("status=%d body=%s content=%q", w.Code, w.Body, saved.Content)
						}
					} else {
						db := setupEditDB(t)
						id, resultID, prID := seedEditableTask(t, db)
						var base model.SummaryResult
						db.First(&base, resultID)
						base.Content = content
						base.SetCitations(citations)
						base.SetTeamCitations([]model.TeamCitation{{Index: 1, UserID: "creator1"}})
						db.Save(&base)
						// The displayed plain citations must be owned by this
						// caller, exactly as in the real single-person mirror.
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
						if w.Code != 200 || strings.Contains(w.Body.String(), "event: error") || saved.Content != want || len(saved.GetCitations()) != len(citations) {
							t.Fatalf("status=%d body=%s content=%q", w.Code, w.Body, saved.Content)
						}
						if len(saved.GetTeamCitations()) != 1 || saved.GetTeamCitations()[0].UserID != "creator1" {
							t.Fatal("team citation identity lost")
						}
					}
				})
			}
		}
	}
}
