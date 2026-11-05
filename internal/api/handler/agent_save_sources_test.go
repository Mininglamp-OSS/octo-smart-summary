//go:build cgo

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/mattn/go-sqlite3"
	"net/http"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"gorm.io/gorm"
)

// Existing legacy-save tests need actual authorized IM fixtures now, rather
// than treating a nil IM DB as permission to persist arbitrary source IDs.
func newLegacySaveIMDB(t *testing.T, groups ...string) *gorm.DB {
	t.Helper()
	db := newSummaryWorkspaceIMValidationDB(t)
	enableLegacyIMSQL(t, db)
	for _, id := range groups {
		if err := db.Exec(`INSERT INTO "group" VALUES (?,?,'test-space',1,1)`, id, id).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(`INSERT INTO group_member VALUES (?,'test-user',1,0,0)`, id).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Exec(`INSERT INTO group_member SELECT group_no,'test-uid',1,0,0 FROM "group" WHERE space_id='test-space'`).Error; err != nil {
		t.Fatal(err)
	}
	return db
}

func enableLegacyIMSQL(t *testing.T, db *gorm.DB) {
	t.Helper()
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	conn, err := sqlDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.Raw(func(raw interface{}) error {
		sqliteConn := raw.(*sqlite3.SQLiteConn)
		if err := sqliteConn.RegisterCollation("utf8mb4_unicode_ci", strings.Compare); err != nil {
			return err
		}
		return sqliteConn.RegisterFunc("CONCAT", func(parts ...string) string { return strings.Join(parts, "") }, true)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLegacySaveAuthorizesSourcesAndExplicitOrigin(t *testing.T) {
	cases := []struct {
		name, id string
		kind     int
		origin   bool
		want     int
	}{
		{"group", "group-a", 1, false, 200}, {"cross-space", "group-b", 1, false, 404},
		{"private", "private-group", 1, false, 404}, {"dm", "peer-in", 3, false, 200},
		{"dm-outside", "peer-out", 3, false, 404}, {"thread", "group-a____allowed-thread", 2, false, 200},
		{"private-thread", "private-thread", 2, false, 404},
		{"inactive-thread", "group-a____allowed-thread", 2, false, 404}, {"forged-origin", "group-b", 1, true, 404},
		{"inactive", "group-a", 1, false, 404}, {"no-im", "group-a", 1, false, 503},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := setupAgentSummaryTestDB(t)
			im := newSummaryWorkspaceIMValidationDB(t)
			enableLegacyIMSQL(t, im)
			if err := im.Exec(`INSERT INTO thread (short_id,name,group_no,status,message_count,creator_uid,updated_at) VALUES ('allowed-thread','Thread','group-a',1,1,'actor',1)`).Error; err != nil {
				t.Fatal(err)
			}
			if tc.name == "inactive" || tc.name == "inactive-thread" {
				if err := im.Exec(`UPDATE group_member SET status=0 WHERE uid='actor' AND group_no='group-a'`).Error; err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "no-im" {
				im = nil
			}
			seedAssistantMessage(t, db, "actor", "legacy-auth", "### Summary\nBody")
			h := NewAgentSummaryHandler(db, im, "", "", "", 0, 0)
			body := map[string]interface{}{"session_id": "legacy-auth", "origin_channel_id": "group-a", "origin_channel_type": 1, "sources": []sourceReq{{SourceID: tc.id, SourceType: tc.kind}}}
			if tc.origin {
				body["origin_channel_id"] = tc.id
				body["sources"] = []sourceReq{}
			}
			w := doAgentSave(t, setupAgentSummaryRouter(h), body, map[string]string{"Token": "actor", "X-Space-Id": "space-a"})
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d: %s", w.Code, tc.want, w.Body.String())
			}
			if tc.want != http.StatusOK {
				for _, table := range []interface{}{&model.SummaryTask{}, &model.SummarySource{}, &model.PersonalResult{}, &model.SummaryAgentSaveIdempotency{}} {
					var count int64
					if err := db.Model(table).Count(&count).Error; err != nil {
						t.Fatal(err)
					}
					if count != 0 {
						t.Fatalf("rejected save persisted %T", table)
					}
				}
				var count int64
				db.Model(&model.AgentMessage{}).Count(&count)
				if count != 1 {
					t.Fatal("rejected save deleted draft")
				}
			}
		})
	}
}

func TestLegacySessionOriginRequiresCurrentAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name, id string
		kind     int
		want     int
	}{
		{"group-allowed", "group-a", model.ChannelTypeGroup, 200},
		{"group-revoked", "group-a", model.ChannelTypeGroup, 404},
		{"group-cross-space", "group-b", model.ChannelTypeGroup, 404},
		{"dm-allowed", "peer-in", model.ChannelTypeDM, 200},
		{"dm-outside-space", "peer-out", model.ChannelTypeDM, 404},
		{"thread-allowed", "group-a____allowed-thread", model.ChannelTypeThread, 200},
		{"thread-parent-revoked", "group-a____allowed-thread", model.ChannelTypeThread, 404},
		{"im-unavailable", "group-a", model.ChannelTypeGroup, 503},
		{"whitespace-origin", "   ", model.ChannelTypeGroup, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupAgentSummaryTestDB(t)
			im := newSummaryWorkspaceIMValidationDB(t)
			enableLegacyIMSQL(t, im)
			if err := im.Exec(`INSERT INTO thread (short_id,name,group_no,status,message_count,creator_uid,updated_at) VALUES ('allowed-thread','Thread','group-a',1,1,'actor',1)`).Error; err != nil {
				t.Fatal(err)
			}
			args, _ := json.Marshal(map[string]interface{}{"channel_id": tc.id, "channel_type": tc.kind})
			calls, _ := json.Marshal([]map[string]interface{}{{"id": "historical-fetch", "type": "function", "function": map[string]interface{}{"name": "fetch_channel", "arguments": string(args)}}})
			transcript := string(calls)
			if err := db.Create(&model.AgentMessage{UserID: "actor", SessionID: "session-origin-auth", Role: "assistant", ToolCalls: &transcript}).Error; err != nil {
				t.Fatal(err)
			}
			seedAssistantMessage(t, db, "actor", "session-origin-auth", "### Saved summary\nExisting body")
			// Permission changes AFTER the historical tool call was persisted.
			if strings.Contains(tc.name, "revoked") {
				if err := im.Exec(`UPDATE group_member SET status=0 WHERE group_no='group-a' AND uid='actor'`).Error; err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "im-unavailable" {
				im = nil
			}
			h := NewAgentSummaryHandler(db, im, "", "", "", 0, 0)
			body := map[string]interface{}{"session_id": "session-origin-auth"} // no explicit origin or sources
			headers := map[string]string{"Token": "actor", "X-Space-Id": "space-a", "Idempotency-Key": "session-origin-key"}
			w := doAgentSave(t, setupAgentSummaryRouter(h), body, headers)
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d: %s", w.Code, tc.want, w.Body.String())
			}
			if tc.want != 200 {
				for _, table := range []interface{}{&model.SummaryTask{}, &model.SummarySource{}, &model.SummaryResult{}, &model.PersonalResult{}, &model.SummaryAgentSaveIdempotency{}} {
					var count int64
					if err := db.Model(table).Count(&count).Error; err != nil {
						t.Fatal(err)
					}
					if count != 0 {
						t.Fatalf("rejection persisted %T", table)
					}
				}
				var count int64
				db.Model(&model.AgentMessage{}).Count(&count)
				if count != 2 {
					t.Fatal("rejection destroyed transcript/draft")
				}
			} else {
				var task model.SummaryTask
				if err := db.First(&task).Error; err != nil {
					t.Fatal(err)
				}
				wantType, ok := storageChannelTypeToAppOrigin(tc.kind)
				if !ok || task.OriginChannelID != tc.id || task.OriginChannelType != wantType {
					t.Fatal("wrong resolved origin or type")
				}
				replay := doAgentSave(t, setupAgentSummaryRouter(h), body, headers)
				if replay.Code != 200 {
					t.Fatal("successful retry depended on consumed transcript: " + replay.Body.String())
				}
			}
		})
	}
}

func TestLegacySharedSummaryOriginUsesTaskPermission(t *testing.T) {
	for _, derived := range []bool{false, true} {
		t.Run(fmt.Sprint(derived), func(t *testing.T) {
			db := setupAgentSummaryTestDB(t)
			im := newLegacySaveIMDB(t) // no membership in the reference's original channel
			ref := model.SummaryTask{TaskNo: "SHARED-ORIGIN", SpaceID: "test-space", CreatorID: "other-owner", Status: model.StatusCompleted, SummaryMode: model.ModeByPerson}
			if !derived {
				ref.OriginChannelID = "private-original"
				ref.OriginChannelType = model.OriginChannelGroup
				ref.OriginFromDerived = true
			}
			if err := db.Create(&ref).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&model.SummaryParticipant{TaskID: ref.ID, UserID: "test-user", Status: model.ParticipantAccepted}).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&model.SummaryResult{TaskID: ref.ID, Version: 1, Content: "Shared team body"}).Error; err != nil {
				t.Fatal(err)
			}
			if derived {
				if err := db.Create(&model.SummarySource{TaskID: ref.ID, SourceID: "private-original", SourceType: model.SourceGroup, Derived: true}).Error; err != nil {
					t.Fatal(err)
				}
			}
			seedAssistantMessage(t, db, "test-user", "shared-origin", "### Optimized shared body")
			h := NewAgentSummaryHandler(db, im, "", "", "", 0, 0)
			body := map[string]interface{}{"session_id": "shared-origin", "referenced_task_ids": []int64{ref.ID}}
			w := doAgentSave(t, setupAgentSummaryRouter(h), body, nil)
			if w.Code != 200 {
				t.Fatal("shared body requires original group membership: " + w.Body.String())
			}
			var saved model.SummaryTask
			if err := db.Where("id <> ?", ref.ID).First(&saved).Error; err != nil {
				t.Fatal(err)
			}
			if !saved.OriginFromDerived || saved.OriginChannelID != "private-original" {
				t.Fatal("inherited origin lost privacy mask")
			}
		})
	}
}
