package service

import (
	"strings"
	"testing"
)

func TestContentIdentityNamespaces(t *testing.T) {
	targets := []ContentTarget{
		{SpaceID: "space", TaskID: 12, Kind: ContentResult},
		{SpaceID: "space", TaskID: 12, Kind: ContentPersonal, UserID: "a/?#用户"},
		{SpaceID: "space", TaskID: 12, Kind: ContentPersonal, UserID: "b"},
		{SpaceID: "other", TaskID: 12, Kind: ContentResult},
	}
	seen := map[string]bool{}
	for _, target := range targets {
		id := target.ID()
		parsed, err := ParseContentID(id)
		if err != nil || parsed != target || seen[id] || strings.ContainsAny(id, "/?&#") {
			t.Fatalf("invalid round-trip for %+v: %q %v", target, id, err)
		}
		seen[id] = true
		version := ContentVersionIdentity{Target: target, RowID: 1}
		got, err := ParseContentVersionID(version.ID(), target)
		if err != nil || got != version {
			t.Fatalf("version round-trip: %+v %v", got, err)
		}
		for _, other := range targets {
			if other != target {
				if _, err := ParseContentVersionID(version.ID(), other); err == nil {
					t.Fatal("version from another namespace accepted")
				}
			}
		}
	}
}

func TestContentIdentityRejectsMalformedTokens(t *testing.T) {
	for _, token := range []string{"1", "", "sc1_%%%%", "sc1_bnVsbA", strings.Repeat("x", 1600),
		encodeContentToken("sc1_", ContentTarget{SpaceID: "s", TaskID: 1, Kind: ContentResult, UserID: "u"}),
		encodeContentToken("sc1_", ContentTarget{SpaceID: "s", TaskID: -1, Kind: ContentResult})} {
		if _, err := ParseContentID(token); err == nil {
			t.Fatalf("accepted %q", token)
		}
	}
	target := ContentTarget{SpaceID: "s", TaskID: 1, Kind: ContentPersonal, UserID: "u"}
	for _, version := range []ContentVersionIdentity{
		{Target: target},
		{Target: target, RowID: 1, Revision: 1},
		{Target: target, Provisional: true, Revision: 1, Digest: "short"},
	} {
		if _, err := ParseContentVersionID(version.ID(), target); err == nil {
			t.Fatalf("accepted invalid identity %+v", version)
		}
	}
}

func TestContentReadAllowlistIsExact(t *testing.T) {
	got := ParseContentReadSpaces("alpha, beta,alpha,, * ")
	if len(got) != 2 || !got["alpha"] || !got["beta"] || got["*"] {
		t.Fatalf("unexpected allowlist: %#v", got)
	}
}
