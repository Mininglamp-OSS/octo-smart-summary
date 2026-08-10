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

func TestCanonicalAgentSaveRequestHash_StableAcrossOrdering(t *testing.T) {
	// Same semantic body, different source/reference ORDER — must hash the
	// same (client retry with re-sorted maps must replay, not 409).
	base := "sess-abc"
	a := canonicalAgentSaveRequestHash(
		base, "Weekly", "chan-1", 1, 42, 1,
		[]sourceReq{{SourceType: 1, SourceID: "s1"}, {SourceType: 1, SourceID: "s2"}},
		[]int64{7, 3, 5},
	)
	b := canonicalAgentSaveRequestHash(
		base, "Weekly", "chan-1", 1, 42, 1,
		[]sourceReq{{SourceType: 1, SourceID: "s2"}, {SourceType: 1, SourceID: "s1"}},
		[]int64{5, 3, 7, 3}, // includes a duplicate — de-dup contract
	)
	if a != b {
		t.Fatalf("hash should be order+dedup invariant, got a=%s b=%s", a, b)
	}
}

func TestCanonicalAgentSaveRequestHash_DifferentBodyDiffers(t *testing.T) {
	base := canonicalAgentSaveRequestHash(
		"sess-abc", "Weekly", "chan-1", 1, 42, 1,
		[]sourceReq{{SourceType: 1, SourceID: "s1"}}, nil,
	)
	// Changing ANY of the identity axes must produce a different hash so a
	// same-key-different-body retry hits the 409 path.
	shifts := map[string]string{
		"session":    canonicalAgentSaveRequestHash("sess-xyz", "Weekly", "chan-1", 1, 42, 1, []sourceReq{{SourceType: 1, SourceID: "s1"}}, nil),
		"title":      canonicalAgentSaveRequestHash("sess-abc", "Daily", "chan-1", 1, 42, 1, []sourceReq{{SourceType: 1, SourceID: "s1"}}, nil),
		"channel_id": canonicalAgentSaveRequestHash("sess-abc", "Weekly", "chan-2", 1, 42, 1, []sourceReq{{SourceType: 1, SourceID: "s1"}}, nil),
		"channel_tp": canonicalAgentSaveRequestHash("sess-abc", "Weekly", "chan-1", 2, 42, 1, []sourceReq{{SourceType: 1, SourceID: "s1"}}, nil),
		"msg_id":     canonicalAgentSaveRequestHash("sess-abc", "Weekly", "chan-1", 1, 43, 1, []sourceReq{{SourceType: 1, SourceID: "s1"}}, nil),
		"snap_ver":   canonicalAgentSaveRequestHash("sess-abc", "Weekly", "chan-1", 1, 42, 2, []sourceReq{{SourceType: 1, SourceID: "s1"}}, nil),
		"sources":    canonicalAgentSaveRequestHash("sess-abc", "Weekly", "chan-1", 1, 42, 1, []sourceReq{{SourceType: 2, SourceID: "s1"}}, nil),
		"refs":       canonicalAgentSaveRequestHash("sess-abc", "Weekly", "chan-1", 1, 42, 1, []sourceReq{{SourceType: 1, SourceID: "s1"}}, []int64{99}),
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
	a := canonicalAgentSaveRequestHash("sess", "Weekly", "c", 1, 1, 1, nil, nil)
	b := canonicalAgentSaveRequestHash("sess", "  Weekly  ", "c", 1, 1, 1, nil, nil)
	if a != b {
		t.Errorf("title whitespace should be trimmed for hashing")
	}
}

func TestCanonicalAgentSaveRequestHash_EmptySourceIDDropped(t *testing.T) {
	// A source row with empty id is skipped up-front (matches what the tx
	// insert loop does — it too skips empty ids). Client accidentally
	// sending {source_type:1, source_id:""} on one retry vs skipping it on
	// the next must replay cleanly.
	a := canonicalAgentSaveRequestHash("sess", "t", "c", 1, 1, 1,
		[]sourceReq{{SourceType: 1, SourceID: "real"}}, nil)
	b := canonicalAgentSaveRequestHash("sess", "t", "c", 1, 1, 1,
		[]sourceReq{{SourceType: 1, SourceID: ""}, {SourceType: 1, SourceID: "real"}}, nil)
	if a != b {
		t.Errorf("empty source id should be dropped, got a=%s b=%s", a, b)
	}
}
