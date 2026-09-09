//go:build cgo

package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
	"gorm.io/gorm"
)

func seedSourceNameDMUsers(t *testing.T, db *gorm.DB, creator string) {
	t.Helper()
	if err := db.Exec(`CREATE TABLE "user" (uid TEXT, name TEXT)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO "user" (uid, name) VALUES (?, '总结创建者'), ('peer-user', 'momo_test')`, creator).Error; err != nil {
		t.Fatal(err)
	}
}

func TestCreateAgentSummary_WorkspaceSaveCanonicalDMSourceName(t *testing.T) {
	db := setupAgentSummaryTestDB(t)
	imDB := setupSourceNameImDB(t)
	seedSourceNameDMUsers(t, imDB, "test-user")
	fixture := seedWorkspaceSaveFixture(t, db, "workspace-save-dm-name")
	channelID := pipeline.NormalizeDMChannelID("peer-user", "test-user", model.ChannelTypeDM)
	scope := summaryWorkspaceContext{
		SelectedChannels: []summaryWorkspaceChannel{{ChatID: channelID, ChatType: "direct", Name: "不可信的客户端名字"}},
	}
	scopeJSON, _, err := marshalSummaryWorkspaceContext(scope)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.AgentSummarySession{}).Where("id = ?", fixture.Session.ID).
		Update("scope_json", string(scopeJSON)).Error; err != nil {
		t.Fatal(err)
	}
	h := NewAgentSummaryHandler(db, imDB, "", "", "", 0, 0)
	w := doAgentSave(t, setupAgentSummaryRouter(h), fixture.Body, map[string]string{"Idempotency-Key": "workspace-dm-name"})
	if w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	var source model.SummarySource
	if err := db.Take(&source).Error; err != nil {
		t.Fatal(err)
	}
	if source.SourceType != model.SourceDirect || source.SourceID != channelID || source.SourceName != "momo_test(私聊)" {
		t.Fatalf("source = %+v; want canonical ID %q and peer name momo_test(私聊)", source, channelID)
	}
}

func TestSummarySourceName_CanonicalDMHistoricalListAndDetail(t *testing.T) {
	db, imDB := setupListTestDBs(t)
	seedSourceNameDMUsers(t, imDB, "creator1")
	sourceID := "creator1@peer-user"
	taskID := seedEmptyNameSourceTask(t, db, "TST-DM-HISTORY", sourceID)
	if err := db.Model(&model.SummarySource{}).Where("task_id = ?", taskID).
		Updates(map[string]interface{}{"source_type": model.SourceDirect, "source_name": "来源-creator1(私聊)"}).Error; err != nil {
		t.Fatal(err)
	}
	// A participant sees the creator's peer name, not the creator's name.
	if err := db.Create(&model.SummaryParticipant{TaskID: taskID, UserID: "peer-user", Status: model.ParticipantAccepted}).Error; err != nil {
		t.Fatal(err)
	}
	h := NewTaskHandler(db, imDB, "")
	for _, viewer := range []string{"creator1", "peer-user"} {
		t.Run(viewer, func(t *testing.T) {
			w := doRequest(setupRouter(h), "GET", fmt.Sprintf("/api/v1/summaries/%d", taskID), viewer)
			if w.Code != http.StatusOK {
				t.Fatalf("detail: %d %s", w.Code, w.Body.String())
			}
			var detail struct {
				Data struct {
					Sources []model.SummarySource `json:"sources"`
				} `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
				t.Fatal(err)
			}
			if len(detail.Data.Sources) != 1 || detail.Data.Sources[0].SourceName != "momo_test(私聊)" || detail.Data.Sources[0].SourceID != sourceID {
				t.Fatalf("detail sources = %+v", detail.Data.Sources)
			}
			w = doRequest(setupListRouter(h), "GET", "/api/v1/summaries", viewer)
			if w.Code != http.StatusOK {
				t.Fatalf("list: %d %s", w.Code, w.Body.String())
			}
			resp := parseListResponse(t, w)
			if len(resp.Data.Items) != 1 {
				t.Fatalf("list items = %+v", resp.Data.Items)
			}
			sources := resp.Data.Items[0].(map[string]interface{})["sources"].([]interface{})
			if len(sources) != 1 || sources[0].(map[string]interface{})["source_name"] != "momo_test(私聊)" {
				t.Fatalf("list sources = %+v", sources)
			}
		})
	}
	var stored model.SummarySource
	if err := db.Where("task_id = ?", taskID).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.SourceName != "来源-creator1(私聊)" || stored.SourceID != sourceID {
		t.Fatalf("read must not rewrite historical rows: %+v", stored)
	}
}

func TestSummaryShare_CanonicalDMHistoricalSourceName(t *testing.T) {
	db, imDB, r, taskID := setupShareTest(t)
	seedSourceNameDMUsers(t, imDB, "creator")
	if err := db.Model(&model.SummarySource{}).Where("task_id = ?", taskID).Updates(map[string]interface{}{
		"source_type": model.SourceDirect, "source_id": "peer-user@creator", "source_name": "来源-peer-use(私聊)",
	}).Error; err != nil {
		t.Fatal(err)
	}
	w := shareRequest(t, r, "POST", fmt.Sprintf("/api/v1/summaries/%d/shares", taskID), "creator", "space1", map[string]interface{}{
		"idempotency_key": "share-dm-name",
		"targets":         []map[string]interface{}{{"channel_id": "group1", "channel_type": model.ChannelTypeGroup}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("share: %d %s", w.Code, w.Body.String())
	}
	var snapshot model.SummaryShareSnapshot
	if err := db.Where("task_id = ?", taskID).Take(&snapshot).Error; err != nil {
		t.Fatal(err)
	}
	if snapshot.SourceName != "momo_test(私聊)" {
		t.Fatalf("share source name = %s", snapshot.SourceName)
	}
}
