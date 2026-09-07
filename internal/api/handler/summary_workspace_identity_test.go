package handler

import "testing"

func TestSummaryWorkspaceAgentSessionID(t *testing.T) {
	t.Parallel()

	first := summaryWorkspaceAgentSessionID("space-a", "session-1", 1)
	if first == "" || len(first) > 128 {
		t.Fatalf("derived id length = %d, want 1..128", len(first))
	}
	if sessionIDPattern.MatchString(first) {
		t.Fatalf("derived internal id %q must not be accepted as a public session id", first)
	}
	if got := summaryWorkspaceAgentSessionID("space-a", "session-1", 1); got != first {
		t.Fatalf("derived id is not deterministic: %q != %q", got, first)
	}
	if got := summaryWorkspaceAgentSessionID("space-b", "session-1", 1); got == first {
		t.Fatal("different spaces produced the same internal session id")
	}
	if got := summaryWorkspaceAgentSessionID("space-a", "session-2", 1); got == first {
		t.Fatal("different public sessions produced the same internal session id")
	}
	if got := summaryWorkspaceAgentSessionID("space-a", "session-1", 2); got == first {
		t.Fatal("different scope versions produced the same internal session id")
	}
}

func TestPersistedOrDerivedWorkspaceAgentSessionID(t *testing.T) {
	if got := persistedOrDerivedWorkspaceAgentSessionID("persisted", "space-a", "session-1", 2); got != "persisted" {
		t.Fatalf("persisted id = %q", got)
	}
	want := summaryWorkspaceAgentSessionID("space-a", "session-1", 2)
	if got := persistedOrDerivedWorkspaceAgentSessionID(" ", "space-a", "session-1", 2); got != want {
		t.Fatalf("derived fallback = %q, want %q", got, want)
	}
}
