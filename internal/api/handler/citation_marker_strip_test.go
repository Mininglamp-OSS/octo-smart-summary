package handler

import "testing"

func TestStripUnresolvedCitationMarkers_UsesArtifactMarkerSet(t *testing.T) {
	markers := citationMarkerSet{"1": {}, "P2": {}}
	input := "结论 [1]，HTTP [200]，其他编号 [2]，团队引用 [P2]。"
	want := "结论 ，HTTP [200]，其他编号 [2]，团队引用 。"
	if got := stripUnresolvedCitationMarkers(input, markers); got != want {
		t.Fatalf("stripUnresolvedCitationMarkers() = %q, want %q", got, want)
	}
}

func TestStripUnresolvedCitationMarkers_NoKnownMarkersIsNoOp(t *testing.T) {
	input := "HTTP [200] and unresolved-looking [1] remain ordinary content."
	if got := stripUnresolvedCitationMarkers(input, nil); got != input {
		t.Fatalf("stripUnresolvedCitationMarkers() = %q, want unchanged %q", got, input)
	}
}
