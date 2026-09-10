//go:build cgo

package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

func TestContentLegacyEditRejectsRolloutAndStickyEnrollment(t *testing.T) {
	for _, sticky := range []bool{false, true} {
		t.Run(fmt.Sprint(sticky), func(t *testing.T) {
			db := setupEditDB(t)
			taskID, resultID, _ := seedEditableTask(t, db)
			t.Setenv("SUMMARY_CONTENT_WRITE_SPACES", "space1")
			if sticky {
				if err := db.Model(&model.SummaryTask{}).Where("id = ?", taskID).
					Update("content_protocol_version", 1).Error; err != nil {
					t.Fatal(err)
				}
				t.Setenv("SUMMARY_CONTENT_WRITE_SPACES", "")
			}
			rec := doEditRequest(setupEditRouter(NewEditHandler(db)), taskID, "creator1",
				map[string]any{"base_result_id": resultID, "content": "legacy overwrite"})
			if rec.Code != 409 || !strings.Contains(rec.Body.String(), "content_protocol_required") {
				t.Fatalf("legacy edit: %d %s", rec.Code, rec.Body)
			}
			var result model.SummaryResult
			if err := db.First(&result, resultID).Error; err != nil {
				t.Fatal(err)
			}
			if result.Content == "legacy overwrite" || result.EditedAt != nil {
				t.Fatal("rejected edit mutated content")
			}
		})
	}
}

func TestContentLegacyRefineRechecksAfterModelReturns(t *testing.T) {
	db := setupEditDB(t)
	taskID, resultID, _ := seedEditableTask(t, db)
	t.Setenv("SUMMARY_CONTENT_WRITE_SPACES", "")
	// Admission was legacy; enrollment happens while its model call is in
	// flight. The commit-time fence must still reject the late legacy result.
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := db.Model(&model.SummaryTask{}).Where("id = ?", taskID).
			Update("content_protocol_version", 1).Error; err != nil {
			http.Error(w, "fixture enrollment failed", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"late rewrite [1]"},"finish_reason":"stop"}],"usage":{"total_tokens":1}}`)
	}))
	defer provider.Close()
	llm := service.NewLLMClient(provider.URL, "fixture-key", "fixture-model", 5, 256, false, 5, nil)
	rec := doRefineSummaryRequest(setupEditRouter(NewEditHandler(db, llm)), taskID, "creator1",
		map[string]any{"base_result_id": resultID, "feedback": "shorter"})
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "content_protocol_required") {
		t.Fatalf("late legacy commit: %d %s", rec.Code, rec.Body)
	}
	var count int64
	if err := db.Model(&model.SummaryResult{}).Where("task_id = ?", taskID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("late callback created %d versions", count)
	}
}
