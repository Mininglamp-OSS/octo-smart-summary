//go:build cgo

package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

// TestPeekChannelSampleTruncatedAlias covers the peek/fetch `truncated`
// ambiguity: fetch_channel's truncated means "per-channel cap hit", peek's
// means "the channel holds more than the sampled messages". Under V2
// (head/middle/tail sampling) the result additionally carries
// `sample_truncated` instead, so a planner reading both tools in one
// conversation cannot confuse it with fetch_channel.truncated. Flag-off keeps
// the legacy key byte-identical.
func TestPeekChannelSampleTruncated(t *testing.T) {
	t.Setenv("AGENT_SUMMARY_V2_MODE", "on")
	t.Cleanup(func() { t.Setenv("AGENT_SUMMARY_V2_MODE", "") })

	db := setupAgentImDB(t)
	db.Exec(`INSERT INTO "group" (group_no, name, space_id, status, creator) VALUES ('peek-alias', 'PeekAlias', 'test-space', 1, 'user-2')`)
	db.Exec(`INSERT INTO group_member (group_no, uid, is_deleted, role) VALUES ('peek-alias', 'user-2', 0, 0)`)
	// 'peek-alias' CRC32-shards to message3 (tableCount=5 default); create all
	// 5 shards so the fetch lands regardless of which shard the id hashes to.
	for _, tbl := range []string{"message", "message1", "message2", "message3", "message4"} {
		db.Exec("CREATE TABLE IF NOT EXISTS " + tbl + " (message_seq INTEGER, from_uid TEXT, channel_id TEXT, channel_type INTEGER, timestamp INTEGER, payload BLOB, is_deleted INTEGER DEFAULT 0)")
	}

	cfg := config.Config{LLMApiURL: "http://test-llm", LLMApiKey: "test-key", LLMModel: "test-model"}
	SetSummaryDeps(nil, db, nil, cfg)
	defer SetSummaryDeps(nil, nil, nil, config.Config{})

	_, handler := PeekChannelTool()
	ctx := context.WithValue(context.Background(), ContextKeyUID, "user-2")
	ctx = context.WithValue(ctx, ContextKeySessionID, "peek-sample-session")
	now := time.Now().Unix()
	payload := []byte(`{"type":1,"content":"hello"}`)
	// 'peek-alias' CRC32-shards to message3 (tableCount=5 default); seed the
	// shard the fetch actually reads.
	shard := pipeline.MessageTable("peek-alias", 5)
	seed := func(fromSeq int, n int) {
		db.Exec(`DELETE FROM ` + shard + ` WHERE channel_id = 'peek-alias'`)
		for i := 0; i < n; i++ {
			db.Exec(`INSERT INTO `+shard+` (message_seq, from_uid, channel_id, channel_type, timestamp, payload, is_deleted) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				fromSeq+i, "user-2", "peek-alias", 2, now-int64(i), payload, 0)
		}
	}

	peek := func() map[string]interface{} {
		reqJSON := []byte(`{"channel_id":"peek-alias","channel_type":2,"time_start":"` +
			time.Unix(now-3600, 0).UTC().Format(time.RFC3339) +
			`","time_end":"` + time.Unix(now+3600, 0).UTC().Format(time.RFC3339) + `"}`)
		out, err := handler(ctx, reqJSON)
		if err != nil {
			t.Fatalf("peek handler: %v", err)
		}
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(out), &m); err != nil {
			t.Fatalf("unmarshal peek result: %v", err)
		}
		return m
	}

	t.Run("more than sampled: alias true", func(t *testing.T) {
		seed(100, 10) // 10 messages > sampleSize 5
		m := peek()
		if m["sampling"] != "head_middle_tail" {
			t.Fatalf("sampling=%v, want head_middle_tail under V2", m["sampling"])
		}
		if m["sample_truncated"] != true {
			t.Errorf("sample_truncated=%v, want true (10 > 5 sampled)", m["sample_truncated"])
		}
		if _, exists := m["truncated"]; exists {
			t.Errorf("V2 peek must not emit ambiguous truncated key: %v", m)
		}
	})

	t.Run("at or under sampled: alias false", func(t *testing.T) {
		seed(200, 3) // 3 messages <= sampleSize 5
		m := peek()
		if m["sample_truncated"] != false {
			t.Errorf("sample_truncated=%v, want false (3 <= 5 sampled)", m["sample_truncated"])
		}
		if _, exists := m["truncated"]; exists {
			t.Errorf("V2 peek must not emit ambiguous truncated key: %v", m)
		}
	})
}
