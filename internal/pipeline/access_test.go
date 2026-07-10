//go:build cgo

package pipeline

import (
	"context"
	"reflect"
	"sort"
	"strconv"
	"testing"
)

// TestValidateUserAccessibleSources exercises the group/thread/DM membership
// paths on top of the shared pipeline in-memory IM schema.
//
// Note the thread contract: after review round-1 (issue #143, upstream PR
// #145) the write-path helper judges thread accessibility via group_member
// (not thread_member), to match the picker in candidates.go — any thread in
// a user's group is accessible even if the user has never posted there.
// thread_member rows are seeded here only to prove the check ignores them.
func TestValidateUserAccessibleSources(t *testing.T) {
	imDB := setupPipelineImDB(t)
	// Seed:
	//   uid1 is member of grp1 (active) but NOT grp2 (member row soft-deleted).
	//   two active threads under grp1; thread_member membership is intentionally
	//   NOT seeded so we can prove group_member alone grants access.
	//   DM: uid1 has a conversation_extra row for channel "peerA".
	imDB.Exec(`INSERT INTO "group" (group_no, name, status) VALUES ('grp1','g1',1)`)
	imDB.Exec(`INSERT INTO "group" (group_no, name, status) VALUES ('grp2','g2',1)`)
	imDB.Exec(`INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp1','uid1',0)`)
	imDB.Exec(`INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp2','uid1',1)`) // soft-deleted -> not accessible
	imDB.Exec(`INSERT INTO thread (id, short_id, name, group_no, status) VALUES (1,'sh1','th1','grp1',1)`)
	imDB.Exec(`INSERT INTO thread (id, short_id, name, group_no, status) VALUES (2,'sh2','th2','grp2',1)`)
	// DM channel_id in conversation_extra is stored in the WuKongIM canonical
	// form (CRC32-ordered "a@b"). The helper normalizes request ids the same
	// way before probing, so we seed with the normalized form to match.
	dmA := NormalizeDMChannelID("peerA", "uid1", 1)
	imDB.Exec(`INSERT INTO conversation_extra (uid, channel_id, channel_type) VALUES (?, ?, 1)`, "uid1", dmA)

	sources := []SourceRef{
		{SourceType: 1, SourceID: "grp1"},        // accessible group
		{SourceType: 1, SourceID: "grp2"},        // is_deleted=1 -> missing
		{SourceType: 2, SourceID: "grp1____sh1"}, // thread under grp1: group_member=ok -> accessible
		{SourceType: 2, SourceID: "grp2____sh2"}, // thread under grp2: not a member -> missing
		{SourceType: 3, SourceID: "peerA"},       // accessible DM
		{SourceType: 3, SourceID: "peerZ"},       // DM never seen -> missing
	}
	missing, err := ValidateUserAccessibleSources(context.Background(), "uid1", imDB, sources)
	if err != nil {
		t.Fatalf("ValidateUserAccessibleSources: %v", err)
	}

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
		{"2", "grp2____sh2"},
		{"3", "peerZ"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("missing mismatch:\n got=%v\nwant=%v", got, want)
	}
}

// TestValidateUserAccessibleSources_Empty: empty input is a fast-path pass-through.
func TestValidateUserAccessibleSources_Empty(t *testing.T) {
	imDB := setupPipelineImDB(t)
	missing, err := ValidateUserAccessibleSources(context.Background(), "uid1", imDB, nil)
	if err != nil || len(missing) != 0 {
		t.Fatalf("empty sources should pass through: missing=%v err=%v", missing, err)
	}
}

// TestValidateUserAccessibleSources_NilIMDBFailsClosed: after review round-1
// (upstream PR #145) imDB==nil MUST NOT pass through — production wires
// imDB=nil when IM DB is down (cmd/summary-api/main.go:61-64), and the old
// permissive fallback would let any Update/Create bypass the access check
// during an IM outage.
func TestValidateUserAccessibleSources_NilIMDBFailsClosed(t *testing.T) {
	_, err := ValidateUserAccessibleSources(context.Background(), "uid1", nil,
		[]SourceRef{{SourceType: 1, SourceID: "grp1"}})
	if err == nil {
		t.Fatalf("expected error when imDB is nil (fail-closed), got nil")
	}
}

// TestValidateUserAccessibleSources_ThreadGroupOnly asserts the picker/validator
// contract: a user who is a group_member but NOT a thread_member can still
// select a thread inside that group and have it judged accessible.
// (Regression guard for upstream review P1: thread strictness misalignment
// with candidates.go picker.)
func TestValidateUserAccessibleSources_ThreadGroupOnly(t *testing.T) {
	imDB := setupPipelineImDB(t)
	imDB.Exec(`INSERT INTO "group" (group_no, name, status) VALUES ('grp1','g1',1)`)
	imDB.Exec(`INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp1','uid1',0)`)
	imDB.Exec(`INSERT INTO thread (id, short_id, name, group_no, status) VALUES (1,'sh1','th1','grp1',1)`)
	// Deliberately NOT seeding thread_member; the old strict helper's join on
	// thread_member would have rejected this thread. Group-only join must pass.

	missing, err := ValidateUserAccessibleSources(context.Background(), "uid1", imDB,
		[]SourceRef{{SourceType: 2, SourceID: "grp1____sh1"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("thread in user's group must be accessible without thread_member row; missing=%v", missing)
	}
}

// TestValidateUserAccessibleSources_DMBeyond200 asserts DM validation is not
// truncated by the 200-row read-path cap: seed 250 DMs and probe one of the
// later rows. Regression guard for upstream review Major: DM LIMIT 200.
func TestValidateUserAccessibleSources_DMBeyond200(t *testing.T) {
	imDB := setupPipelineImDB(t)
	const n = 250
	for i := 0; i < n; i++ {
		peer := "peer_" + strconv.Itoa(i)
		// updated_at ascending so peer_249 is newest, peer_0 oldest — mimics the
		// distribution that made a "recency top-200" query drop the middle rows.
		stored := NormalizeDMChannelID(peer, "uid1", 1)
		imDB.Exec(`INSERT INTO conversation_extra (uid, channel_id, channel_type, updated_at) VALUES (?, ?, 1, ?)`,
			"uid1", stored, i)
	}
	// Probe an entry that would fall outside the top-200-by-recency (peer_0 is
	// oldest -> would be #250 if the query had a LIMIT). It must still be
	// judged accessible now that the DM check is a targeted existence query.
	missing, err := ValidateUserAccessibleSources(context.Background(), "uid1", imDB,
		[]SourceRef{{SourceType: 3, SourceID: "peer_0"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("DM #250 by recency must be accessible (no LIMIT truncation); missing=%v", missing)
	}
}

// TestValidateUserAccessibleSources_DMQueryFail asserts write-path fail-closed
// on IM DM query error. (Regression guard for reviewer thread e0640d10.)
func TestValidateUserAccessibleSources_DMQueryFail(t *testing.T) {
	imDB := setupPipelineImDB(t)
	imDB.Exec(`DROP TABLE conversation_extra`)

	_, err := ValidateUserAccessibleSources(context.Background(), "uid1", imDB,
		[]SourceRef{{SourceType: 3, SourceID: "peerX"}})
	if err == nil {
		t.Fatalf("expected error when DM query fails, got nil")
	}
}

// TestValidateUserAccessibleSources_ThreadQueryFail: same guard for the thread
// sub-query. After the group_member switch, drop \`thread\` itself (the query
// no longer references thread_member).
func TestValidateUserAccessibleSources_ThreadQueryFail(t *testing.T) {
	imDB := setupPipelineImDB(t)
	imDB.Exec(`DROP TABLE thread`)

	_, err := ValidateUserAccessibleSources(context.Background(), "uid1", imDB,
		[]SourceRef{{SourceType: 2, SourceID: "grp1____sh1"}})
	if err == nil {
		t.Fatalf("expected error when thread query fails, got nil")
	}
}

// TestValidateUserAccessibleSources_ArchivedThreadSelected exercises the
// selectedThreadIDs archived-thread relaxation on the write path: an archived
// thread (t.status=2) is normally excluded from GetUserChannels output, but
// when the caller explicitly names it (its id shows up in the current sources
// list), it must still be judged accessible. Regression guard so a legitimate
// edit that preserves an archived thread source doesn't get false-403'd.
func TestValidateUserAccessibleSources_ArchivedThreadSelected(t *testing.T) {
	imDB := setupPipelineImDB(t)
	imDB.Exec(`INSERT INTO "group" (group_no, name, status) VALUES ('grp1','g1',1)`)
	imDB.Exec(`INSERT INTO group_member (group_no, uid, is_deleted) VALUES ('grp1','uid1',0)`)
	// status=2 (archived) — excluded by the default t.status=1 predicate,
	// only admitted when the id appears in selectedThreadIDs.
	imDB.Exec(`INSERT INTO thread (id, short_id, name, group_no, status) VALUES (10,'sh_arch','archived','grp1',2)`)

	missing, err := ValidateUserAccessibleSources(context.Background(), "uid1", imDB,
		[]SourceRef{{SourceType: 2, SourceID: "grp1____sh_arch"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("archived thread named in sources must be accessible; missing=%v", missing)
	}
}

// TestValidateUserAccessibleSources_UnknownSourceTypeFailsClosed pins the
// fail-closed default for sourceKey's ct==0 branch: an unknown source_type
// bypasses every allowed-set lookup and must land in the missing slice
// (caller returns 403/40017), not be silently accepted.
func TestValidateUserAccessibleSources_UnknownSourceTypeFailsClosed(t *testing.T) {
	imDB := setupPipelineImDB(t)
	// No seed needed: the check should reject before any allowed-set lookup.

	missing, err := ValidateUserAccessibleSources(context.Background(), "uid1", imDB,
		[]SourceRef{{SourceType: 99, SourceID: "whatever"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(missing) != 1 || missing[0].SourceType != 99 || missing[0].SourceID != "whatever" {
		t.Fatalf("unknown source_type must fail closed into missing; got=%v", missing)
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
