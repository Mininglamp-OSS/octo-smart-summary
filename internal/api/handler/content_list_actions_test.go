//go:build cgo

package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

func TestListContentActionHTTPContractAndRollout(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	if err := db.AutoMigrate(&model.SummaryGenerationRun{}, &model.PersonalResultVersion{}); err != nil {
		t.Fatal(err)
	}
	for i, trigger := range []int{model.TriggerManual, model.TriggerAgent} {
		task := model.SummaryTask{TaskNo: fmt.Sprintf("actions-%d", i), SpaceID: "space1", CreatorID: "creator1",
			SummaryMode: model.ModeByPerson, Status: model.StatusCompleted, TriggerType: trigger}
		if err := db.Create(&task).Error; err != nil {
			t.Fatal(err)
		}
		part := model.SummaryParticipant{TaskID: task.ID, UserID: "creator1", Status: model.ParticipantSubmitted}
		if err := db.Create(&part).Error; err != nil {
			t.Fatal(err)
		}
		pr := model.PersonalResult{TaskID: task.ID, UserID: "creator1", ParticipantRefID: part.ID,
			Content: "private body", WorkerStatus: model.PersonalStatusCompleted}
		if err := db.Create(&pr).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, mode := range []string{"disabled", "read", "execution"} {
		t.Run(mode, func(t *testing.T) {
			readSpaces := ""
			if mode != "disabled" {
				readSpaces = "space1"
			}
			reader := NewContentReadHandler(db, readSpaces)
			if mode == "execution" {
				reader.WithExecution("space1", pipeline.GenerationSourceAuthorizer{DB: imDB}, 90)
			}
			h := NewTaskHandler(db, imDB, "").WithContentReader(reader)
			w := doRequest(setupListRouter(h), "GET", "/api/v1/summaries", "creator1")
			if w.Code != http.StatusOK {
				t.Fatalf("list status=%d", w.Code)
			}
			var response struct {
				Data struct {
					Items []struct {
						Actions *service.ListContentActions `json:"content_actions"`
					}
				} `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || len(response.Data.Items) != 2 {
				t.Fatalf("invalid response: %v", err)
			}
			a, b := response.Data.Items[0].Actions, response.Data.Items[1].Actions
			if mode == "disabled" {
				if a != nil || b != nil {
					t.Fatal("projection exposed outside rollout")
				}
				return
			}
			if a == nil || b == nil || !reflect.DeepEqual(a.Capabilities, b.Capabilities) {
				t.Fatal("engine-dependent list capabilities")
			}
			if a.Capabilities.CanRefine != (mode == "execution") {
				t.Fatalf("read enrollment granted write actions: %+v", a)
			}
		})
	}
}
