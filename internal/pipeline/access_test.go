package pipeline

import (
	"context"
	"reflect"
	"sort"
	"testing"
)

// TestValidateUserAccessibleSources exercises the group/thread/DM membership
// paths and the imDB==nil / empty-input escape hatches on top of the shared
// pipeline in-memory IM schema.
func TestValidateUserAccessibleSources(t *testing.T) {
	// imDB==nil -> permissive fallback: never treat anything as missing.
	if missing, err := ValidateUserAccessibleSources(context.Background(), "uid1", nil,
		[]SourceRef{{SourceType: 1, SourceID: "grp1"}}); err != nil || len(missing) != 0 {
		t.Fatalf("nil imDB should pass through: missing=%v err=%v", missing, err)
	}

	imDB := setupPipelineImDB(t)
	// Seed:
	//   user uid1 is member of grp1 (active) but NOT grp2 (member row soft-deleted).
	//   thread t1 in grp1: uid1 is a thread_member; thread t2 in grp1: uid1 is NOT.
	//   DM: uid1 has a conversation_extra row for channel "peerA" (peer format).
	imDB.Exec(`INSERT INTO "group" (group_no, name, status) VALUES ('grp1','g1',1)`)
	imDB.Exec(`INSERT INTO "group" (group_no, name, status) VALUES ('grp2','g2',1)`)
	imDB.Exec(`INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp1','uid1',0)`)
	imDB.Exec(`INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp2','uid1',1)`) // soft-deleted -> not accessible
	imDB.Exec(`INSERT INTO thread (id, short_id, name, group_no, status) VALUES (1,'sh1','th1','grp1',1)`)
	imDB.Exec(`INSERT INTO thread (id, short_id, name, group_no, status) VALUES (2,'sh2','th2','grp1',1)`)
	imDB.Exec(`INSERT INTO thread_member (thread_id, uid) VALUES (1,'uid1')`)
	// DM: peer id stored under the WuKongIM ordering (normalized). We insert the raw
	// peer form; the helper normalizes both stored and queried ids the same way, so
	// the callers can pass the peer uid without knowing the storage convention.
	imDB.Exec(`INSERT INTO conversation_extra (uid, channel_id, channel_type) VALUES ('uid1','peerA',1)`)

	sources := []SourceRef{
		{SourceType: 1, SourceID: "grp1"},        // accessible group
		{SourceType: 1, SourceID: "grp2"},        // group with is_deleted=1 member -> missing
		{SourceType: 2, SourceID: "grp1____sh1"}, // accessible thread
		{SourceType: 2, SourceID: "grp1____sh2"}, // thread without membership -> missing
		{SourceType: 3, SourceID: "peerA"},       // accessible DM
		{SourceType: 3, SourceID: "peerZ"},       // DM never seen -> missing
	}
	missing, err := ValidateUserAccessibleSources(context.Background(), "uid1", imDB, sources)
	if err != nil {
		t.Fatalf("ValidateUserAccessibleSources: %v", err)
	}

	// Order-insensitive: sort by (source_type, source_id) then compare.
	got := make([][2]string, 0, len(missing))
	for _, m := range missing {
		got = append(got, [2]string{itoaSrcType(m.SourceType), m.SourceID})
	}
	sort.Slice(got, func(i, j int) bool {
		if got[i][0] != got[j][0] {
			return got[i][0] < got[j][0]
		}
		return got[i][1] < got[j][1]
	})
	want := [][2]string{
		{"1", "grp2"},
		{"2", "grp1____sh2"},
		{"3", "peerZ"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("missing mismatch:\n got=%v\nwant=%v", got, want)
	}
}

// TestValidateUserAccessibleSources_Empty ensures empty input is a fast-path pass-through.
func TestValidateUserAccessibleSources_Empty(t *testing.T) {
	imDB := setupPipelineImDB(t)
	missing, err := ValidateUserAccessibleSources(context.Background(), "uid1", imDB, nil)
	if err != nil || len(missing) != 0 {
		t.Fatalf("empty sources should pass through: missing=%v err=%v", missing, err)
	}
}

func itoaSrcType(n int) string {
	switch n {
	case 1:
		return "1"
	case 2:
		return "2"
	case 3:
		return "3"
	default:
		return "?"
	}
}

// TestValidateUserAccessibleSources_DMQueryFail asserts write-path fail-closed
// on IM DM query error: dropping conversation_extra makes the DM sub-query fail,
// the strict helper must surface the error instead of returning a partial
// allowed-set (which would let a legitimate DM look like "missing" and cause
// false 40017/403). Regression guard for reviewer thread e0640d10.
func TestValidateUserAccessibleSources_DMQueryFail(t *testing.T) {
	imDB := setupPipelineImDB(t)
	// Drop conversation_extra so the DM query hits "no such table".
	imDB.Exec(`DROP TABLE conversation_extra`)

	_, err := ValidateUserAccessibleSources(context.Background(), "uid1", imDB,
		[]SourceRef{{SourceType: 3, SourceID: "peerX"}})
	if err == nil {
		t.Fatalf("expected error when DM query fails, got nil")
	}
}

// TestValidateUserAccessibleSources_ThreadQueryFail: same guard for the thread
// sub-query — dropping thread_member makes the join fail.
func TestValidateUserAccessibleSources_ThreadQueryFail(t *testing.T) {
	imDB := setupPipelineImDB(t)
	imDB.Exec(`DROP TABLE thread_member`)

	_, err := ValidateUserAccessibleSources(context.Background(), "uid1", imDB,
		[]SourceRef{{SourceType: 2, SourceID: "grp1____sh1"}})
	if err == nil {
		t.Fatalf("expected error when thread query fails, got nil")
	}
}
