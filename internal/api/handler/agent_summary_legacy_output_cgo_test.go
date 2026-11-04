//go:build cgo

package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

func TestCreateAgentSummary_LegacyOutputEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name, stored, want string
		withEvidence       bool
	}{
		{"old-json-draft", `{"content":"已完成联调，待验收。","citations":[]}`, "已完成联调，待验收。", false},
		{"fenced-json-draft", "```json\n{\"content\":\"# 项目进展\\n已完成联调。\",\"citations\":[]}\n```", "# 项目进展\n已完成联调。", false},
		{"plain-text", "普通总结正文。", "普通总结正文。", false},
		{"model-citations-ignored", `{"content":"已完成联调。","citations":[{"index":1,"content":"untrusted","channel_id":"private-channel","message_seq":999}]}`, "已完成联调。", false},
		{"evidence-citations-authoritative", `{"content":"已完成联调 [1]。","citations":[{"index":1,"content":"untrusted","channel_id":"private-channel","message_seq":999}]}`, "已完成联调 [1]。", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_SUMMARY_V2_MODE", "off")
			db := setupAgentSummaryTestDB(t)
			// Seed a pre-fix row directly: saving must not depend on the chat
			// endpoint already having normalized this assistant reply.
			message := seedAssistantMessage(t, db, "test-user", tc.name, tc.stored)
			if tc.withEvidence {
				seedEvidenceRow(t, db, "test-user", tc.name, "legacy-output-evidence", []pipeline.Message{{
					ChannelID: "allowed-channel", ChannelType: 1, MessageSeq: 42,
					Timestamp: 1704103200, SenderUID: "alice", Content: "trusted evidence",
				}})
			}
			h := NewAgentSummaryHandler(db, nil, "", "", "", 0, 0)
			w := doAgentSave(t, setupAgentSummaryRouter(h), map[string]interface{}{
				"session_id": tc.name, "agent_message_id": message.ID, "snapshot_version": 1,
				"origin_channel_id": "allowed-channel", "origin_channel_type": 1,
				"title": "Legacy summary",
			}, nil)
			if w.Code != http.StatusOK {
				t.Fatalf("save status=%d body=%s", w.Code, w.Body.String())
			}
			var saved model.PersonalResult
			if err := db.First(&saved).Error; err != nil {
				t.Fatal(err)
			}
			if saved.Content != tc.want {
				t.Fatalf("saved content=%q, want %q", saved.Content, tc.want)
			}
			citations := saved.GetCitations()
			if !tc.withEvidence {
				if len(citations) != 0 {
					t.Fatalf("model-supplied citations were trusted: %+v", citations)
				}
				return
			}
			if len(citations) != 1 || citations[0].Index != 1 || citations[0].ChannelID != "allowed-channel" || citations[0].MessageSeq != 42 || citations[0].Content != "trusted evidence" {
				t.Fatalf("citations must come from persisted evidence: %+v", citations)
			}
		})
	}
}

func TestCreateAgentSummary_WorkspaceDoesNotUnwrapLegacyEnvelope(t *testing.T) {
	t.Setenv("AGENT_SUMMARY_V2_MODE", "off")
	db := setupAgentSummaryTestDB(t)
	fixture := seedWorkspaceSaveFixture(t, db, "workspace-json-document")
	const content = `{"content":"A JSON document is intentional here.","citations":[]}`
	var payload agent.SummaryResponsePayload
	if err := json.Unmarshal([]byte(*fixture.Message.ResponsePayload), &payload); err != nil {
		t.Fatal(err)
	}
	payload.Preview.Content = content
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&fixture.Message).Update("response_payload_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
	h := NewAgentSummaryHandler(db, nil, "", "", "", 0, 0)
	w := doAgentSave(t, setupAgentSummaryRouter(h), fixture.Body, map[string]string{"Idempotency-Key": "workspace-json-document"})
	if w.Code != http.StatusOK {
		t.Fatalf("workspace save status=%d body=%s", w.Code, w.Body.String())
	}
	var saved model.PersonalResult
	if err := db.First(&saved).Error; err != nil {
		t.Fatal(err)
	}
	if saved.Content != content {
		t.Fatalf("workspace preview changed: got %q, want %q", saved.Content, content)
	}
}
