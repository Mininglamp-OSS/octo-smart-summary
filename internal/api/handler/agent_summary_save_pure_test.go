package handler

// SUM-BE2 pure-Go tests for the Agent-save helpers that do not need a DB.
// These run under CGO_ENABLED=0 so the developer can iterate without a
// working cgo toolchain (BE-1 review flagged this gap on
// snapshot_validator.go). The DB-side ownership tests live in the cgo-gated
// agent_summary_be2_test.go alongside the other handler cgo tests.

import (
	"testing"
)

func TestValidAgentSaveIdempotencyKey(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want bool
	}{
		{"empty rejected", "", false},
		{"single char accepted", "a", true},
		{"typical uuidish accepted", "5b06aa4b-de4a-4e17-a64a-76592337ad30", true},
		{"underscore/colon/dot accepted", "svc.checkout_v1:req_42", true},
		{"leading dot rejected", ".starts-with-dot", false},
		{"leading dash rejected", "-starts-with-dash", false},
		{"space rejected", "has space", false},
		{"over cap rejected", string(make([]byte, maxAgentSaveIdempotencyKeyLen+1)), false},
	}
	// Fill the over-cap case with real chars (zero bytes fail the pattern anyway).
	over := make([]byte, maxAgentSaveIdempotencyKeyLen+1)
	for i := range over {
		over[i] = 'a'
	}
	cases[len(cases)-1].key = string(over)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validAgentSaveIdempotencyKey(tc.key); got != tc.want {
				t.Errorf("validAgentSaveIdempotencyKey(%q)=%v, want %v", tc.key, got, tc.want)
			}
		})
	}
}

func TestCanonicalAgentSaveRequestHash_StableAcrossSetLikeOrdering(t *testing.T) {
	// Sources and participants are set-like, while referenced task order is
	// preserved because the first task drives origin/citation inheritance.
	origin := "chan-1"
	a := canonicalAgentSaveRequestHash("u1", createAgentSummaryReq{
		SessionID: "sess-abc", Title: "Weekly", OriginChannelID: &origin, OriginChannelType: 1,
		AgentMessageID: 42, SnapshotVersion: 1,
		Sources:           []sourceReq{{SourceType: 1, SourceID: "s1"}, {SourceType: 1, SourceID: "s2"}},
		Participants:      []participantReq{{UserID: "u3", UserName: "U3"}, {UserID: "u2", UserName: "U2"}},
		ReferencedTaskIDs: []int64{7, 3, 5},
	})
	b := canonicalAgentSaveRequestHash("u1", createAgentSummaryReq{
		SessionID: "sess-abc", Title: "Weekly", OriginChannelID: &origin, OriginChannelType: 1,
		AgentMessageID: 42, SnapshotVersion: 1,
		Sources:           []sourceReq{{SourceType: 1, SourceID: "s2"}, {SourceType: 1, SourceID: "s1"}, {SourceType: 1, SourceID: "s1", SourceName: "ignored"}},
		Participants:      []participantReq{{UserID: "u2", UserName: "U2"}, {UserID: "u3", UserName: "U3"}, {UserID: "u2", UserName: "ignored duplicate"}},
		ReferencedTaskIDs: []int64{7, 3, 5, 3},
	})
	if a != b {
		t.Fatalf("hash should normalize set-like fields and duplicate refs, got a=%s b=%s", a, b)
	}
}

func TestCanonicalAgentSaveRequestHash_ReferencedTaskOrderMatters(t *testing.T) {
	origin := "chan-1"
	a := canonicalAgentSaveRequestHash("u1", createAgentSummaryReq{
		SessionID: "sess-abc", OriginChannelID: &origin, OriginChannelType: 1,
		AgentMessageID: 42, SnapshotVersion: 1,
		ReferencedTaskIDs: []int64{7, 3},
	})
	b := canonicalAgentSaveRequestHash("u1", createAgentSummaryReq{
		SessionID: "sess-abc", OriginChannelID: &origin, OriginChannelType: 1,
		AgentMessageID: 42, SnapshotVersion: 1,
		ReferencedTaskIDs: []int64{3, 7},
	})
	if a == b {
		t.Fatal("reordering referenced tasks must change the hash because element 0 drives inheritance")
	}
}

func TestCanonicalAgentSaveRequestHash_DifferentBodyDiffers(t *testing.T) {
	origin1 := "chan-1"
	origin2 := "chan-2"
	baseReq := createAgentSummaryReq{
		SessionID: "sess-abc", RequestID: "turn-1", Title: "Weekly",
		OriginChannelID: &origin1, OriginChannelType: 1,
		AgentMessageID: 42, SnapshotVersion: 1,
		Sources: []sourceReq{{SourceType: 1, SourceID: "s1"}},
	}
	base := canonicalAgentSaveRequestHash("u1", baseReq)
	// Changing ANY of the identity axes must produce a different hash so a
	// same-key-different-body retry hits the 409 path.
	shifts := map[string]string{
		"session":    canonicalAgentSaveRequestHash("u1", func() createAgentSummaryReq { r := baseReq; r.SessionID = "sess-xyz"; return r }()),
		"request":    canonicalAgentSaveRequestHash("u1", func() createAgentSummaryReq { r := baseReq; r.RequestID = "turn-2"; return r }()),
		"title":      canonicalAgentSaveRequestHash("u1", func() createAgentSummaryReq { r := baseReq; r.Title = "Daily"; return r }()),
		"channel_id": canonicalAgentSaveRequestHash("u1", func() createAgentSummaryReq { r := baseReq; r.OriginChannelID = &origin2; return r }()),
		"channel_tp": canonicalAgentSaveRequestHash("u1", func() createAgentSummaryReq { r := baseReq; r.OriginChannelType = 2; return r }()),
		"msg_id":     canonicalAgentSaveRequestHash("u1", func() createAgentSummaryReq { r := baseReq; r.AgentMessageID = 43; return r }()),
		"snap_ver":   canonicalAgentSaveRequestHash("u1", func() createAgentSummaryReq { r := baseReq; r.SnapshotVersion = 2; return r }()),
		"sources": canonicalAgentSaveRequestHash("u1", func() createAgentSummaryReq {
			r := baseReq
			r.Sources = []sourceReq{{SourceType: 2, SourceID: "s1"}}
			return r
		}()),
		"participants": canonicalAgentSaveRequestHash("u1", func() createAgentSummaryReq {
			r := baseReq
			r.Participants = []participantReq{{UserID: "u2"}}
			return r
		}()),
		"refs": canonicalAgentSaveRequestHash("u1", func() createAgentSummaryReq { r := baseReq; r.ReferencedTaskIDs = []int64{99}; return r }()),
	}
	for axis, h := range shifts {
		if h == base {
			t.Errorf("axis %q did not change the hash", axis)
		}
	}
}

func TestCanonicalAgentSaveRequestHash_TitleTrimmed(t *testing.T) {
	// Whitespace-only title differences should NOT split idempotency
	// (mirrors bot hash contract on trimmed title/topic).
	origin := "c"
	a := canonicalAgentSaveRequestHash("u1", createAgentSummaryReq{SessionID: "sess", Title: "Weekly", OriginChannelID: &origin, OriginChannelType: 1, AgentMessageID: 1, SnapshotVersion: 1})
	b := canonicalAgentSaveRequestHash("u1", createAgentSummaryReq{SessionID: "sess", Title: "  Weekly  ", OriginChannelID: &origin, OriginChannelType: 1, AgentMessageID: 1, SnapshotVersion: 1})
	if a != b {
		t.Errorf("title whitespace should be trimmed for hashing")
	}
}

func TestCanonicalAgentSaveRequestHash_EmptySourceIDDropped(t *testing.T) {
	// A source row with empty id is skipped up-front (matches what the tx
	// insert loop does — it too skips empty ids). Client accidentally
	// sending {source_type:1, source_id:""} on one retry vs skipping it on
	// the next must replay cleanly.
	origin := "c"
	a := canonicalAgentSaveRequestHash("u1", createAgentSummaryReq{SessionID: "sess", Title: "t", OriginChannelID: &origin, OriginChannelType: 1, AgentMessageID: 1, SnapshotVersion: 1, Sources: []sourceReq{{SourceType: 1, SourceID: "real"}}})
	b := canonicalAgentSaveRequestHash("u1", createAgentSummaryReq{SessionID: "sess", Title: "t", OriginChannelID: &origin, OriginChannelType: 1, AgentMessageID: 1, SnapshotVersion: 1, Sources: []sourceReq{{SourceType: 1, SourceID: ""}, {SourceType: 1, SourceID: "real"}}})
	if a != b {
		t.Errorf("empty source id should be dropped, got a=%s b=%s", a, b)
	}
}

func TestCanonicalAgentSaveRequestHash_ImplicitOriginDiffersFromExplicitEmpty(t *testing.T) {
	empty := ""
	implicit := canonicalAgentSaveRequestHash("u1", createAgentSummaryReq{SessionID: "sess"})
	explicit := canonicalAgentSaveRequestHash("u1", createAgentSummaryReq{SessionID: "sess", OriginChannelID: &empty})
	if implicit == explicit {
		t.Fatal("implicit origin and explicitly empty origin must hash differently")
	}
}

func TestCanonicalAgentSaveRequestHash_WorkspaceVersionsMatter(t *testing.T) {
	scope1, scope2 := 1, 2
	artifact1, artifact2 := 3, 4
	baseReq := createAgentSummaryReq{
		SessionID:               "workspace-session",
		AgentMessageID:          42,
		SnapshotVersion:         1,
		ScopeVersion:            &scope1,
		ExpectedArtifactVersion: &artifact1,
	}
	base := canonicalAgentSaveRequestHash("u1", baseReq)

	changedScope := baseReq
	changedScope.ScopeVersion = &scope2
	if got := canonicalAgentSaveRequestHash("u1", changedScope); got == base {
		t.Fatal("scope_version did not change the idempotency hash")
	}
	changedArtifact := baseReq
	changedArtifact.ExpectedArtifactVersion = &artifact2
	if got := canonicalAgentSaveRequestHash("u1", changedArtifact); got == base {
		t.Fatal("expected_artifact_version did not change the idempotency hash")
	}
}

func TestCanonicalAgentSaveRequestHash_WorkspaceIgnoresDiscardedClientScope(t *testing.T) {
	scope, artifact := 2, 3
	originA, originB := "client-a", "client-b"
	base := createAgentSummaryReq{
		SessionID: "workspace-session", AgentMessageID: 42, SnapshotVersion: 1,
		ScopeVersion: &scope, ExpectedArtifactVersion: &artifact, Title: "Weekly",
		OriginChannelID: &originA, OriginChannelType: 1,
		Sources:           []sourceReq{{SourceType: 1, SourceID: "source-a"}},
		Participants:      []participantReq{{UserID: "member-a", UserName: "A"}},
		ReferencedTaskIDs: []int64{10},
	}
	changed := base
	changed.OriginChannelID = &originB
	changed.OriginChannelType = 2
	changed.Sources = []sourceReq{{SourceType: 2, SourceID: "source-b"}}
	changed.Participants = []participantReq{{UserID: "member-b", UserName: "B"}}
	changed.ReferencedTaskIDs = []int64{20}
	if first, second := canonicalAgentSaveRequestHash("u1", base), canonicalAgentSaveRequestHash("u1", changed); first != second {
		t.Fatalf("workspace hash changed for discarded client scope: %s != %s", first, second)
	}
}

func TestValidateWorkspacePreviewSaveRequest_LegacyAndStrictShapes(t *testing.T) {
	if err := validateWorkspacePreviewSaveRequest(createAgentSummaryReq{SessionID: "legacy"}); err != nil {
		t.Fatalf("legacy request rejected: %v", err)
	}
	scope, artifact := 1, 1
	if err := validateWorkspacePreviewSaveRequest(createAgentSummaryReq{
		SessionID: "workspace", ScopeVersion: &scope,
	}); err == nil {
		t.Fatal("half-supplied workspace versions were accepted")
	}
	if err := validateWorkspacePreviewSaveRequest(createAgentSummaryReq{
		SessionID:               "workspace",
		AgentMessageID:          1,
		SnapshotVersion:         1,
		ScopeVersion:            &scope,
		ExpectedArtifactVersion: &artifact,
	}); err != nil {
		t.Fatalf("valid strict workspace request rejected: %v", err)
	}
}
