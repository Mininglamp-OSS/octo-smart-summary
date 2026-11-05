package service

import (
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
)

func TestRebasePersonalSnapshotPreservesEvidenceAndUnknown(t *testing.T) {
	parent := 3
	if RebasePersonalSnapshot(nil, 42, 4, &parent) != nil {
		t.Fatal("NULL provenance became invented data")
	}
	for _, raw := range []string{`{broken`, `{"task_id":99,"content_version":1,"scope":{"channel_ids":["foreign"]}}`} {
		var got model.Snapshot
		json.Unmarshal([]byte(*RebasePersonalSnapshot(&raw, 42, 4, &parent)), &got)
		if got.ProvenanceStatus != "unknown" || got.ContentVersion != 4 || len(got.Scope.ChannelIDs) != 0 {
			t.Fatalf("invalid provenance exposed: %+v", got)
		}
	}
	raw := `{"snapshot_version":1,"task_id":42,"content_version":1,"requirement":"Original request","scope":{"channel_ids":["evidence"]},"tool_summary":["fetch"],"parent_snapshot_version":0}`
	before := raw
	var got model.Snapshot
	json.Unmarshal([]byte(*RebasePersonalSnapshot(&raw, 42, 4, &parent)), &got)
	if raw != before || got.ContentVersion != 4 || got.ParentSnapshotVersion == nil || *got.ParentSnapshotVersion != 3 || got.Requirement != "Original request" || got.Scope.ChannelIDs[0] != "evidence" || got.ToolSummary[0] != "fetch" {
		t.Fatalf("bad lineage/scope: %+v", got)
	}
}
