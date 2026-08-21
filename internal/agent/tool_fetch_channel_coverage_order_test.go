package agent

import (
	"os"
	"strings"
	"testing"
)

// TestFetchChannelRecordsSuccessAfterEvidencePersist pins an ORDERING invariant
// that no behavioural test in this package can reach: fetch_channel needs a live
// IM database, a real message pool and an evidence table to exercise its
// persistence failure path, so the mutation "record the channel as succeeded
// before persisting its evidence" passes every existing test.
//
// It matters because that ordering silently produces a false COMPLETE. If the
// evidence INSERT fails, the channel is already in succeeded_channels, so the
// finish gate sees succeeded == expected and reports COMPLETE — while
// buildCitationsForSession, which discovers exclusively from
// agent_message_evidence, never sees those messages. A third of the evidence can
// be uncitable in a run reported as complete.
//
// Reading the source is the cheapest honest way to state "success is recorded
// only once the messages are durably discoverable". It is stable against
// refactors that preserve the invariant and fails on exactly the one that does
// not.
func TestFetchChannelRecordsSuccessAfterEvidencePersist(t *testing.T) {
	src, err := os.ReadFile("tool_fetch_channel.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	s := string(src)

	persistAt := strings.Index(s, "PersistEvidence(summaryDB")
	if persistAt < 0 {
		t.Fatal("PersistEvidence call not found — update this test alongside the refactor")
	}
	successAt := strings.Index(s, "recordFetch(true,")
	if successAt < 0 {
		t.Fatal("recordFetch(true, ...) not found — update this test alongside the refactor")
	}
	if successAt < persistAt {
		t.Fatal("fetch_channel records the channel as SUCCEEDED before persisting its evidence: " +
			"a failed evidence write then yields a COMPLETE verdict over messages no citation can reach")
	}

	// The failure path must mark it failed rather than leaving it unrecorded: an
	// unrecorded channel is invisible to the gate, which in open scope means no
	// gap at all.
	tail := s[persistAt:]
	end := strings.Index(tail, "result := map[string]interface{}{")
	if end < 0 {
		end = len(tail)
	}
	if !strings.Contains(tail[:end], "recordFetch(false,") {
		t.Fatal("the evidence-persist failure path must record the channel as failed")
	}
}

// TestFetchChannelArgumentErrorsAreRecorded pins the other half of the same
// finding: the three argument checks (missing channel_type, unparseable
// time_start/time_end) must sit BELOW the recordFetch closure.
//
// These are the most common fetch failures — INVALID_ARGUMENT and commit ae154c9
// exist because the model routinely sends an empty time_start — and while they
// returned early above the closure, the abandoned channel appeared in neither
// attempted_channels nor failed_channels.
func TestFetchChannelArgumentErrorsAreRecorded(t *testing.T) {
	src, err := os.ReadFile("tool_fetch_channel.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	s := string(src)

	closureAt := strings.Index(s, "recordFetch := func(")
	if closureAt < 0 {
		t.Fatal("recordFetch closure not found — update this test alongside the refactor")
	}
	for _, marker := range []string{
		"channel_type is required",
		"parse time_start",
		"parse time_end",
	} {
		at := strings.Index(s, marker)
		if at < 0 {
			t.Fatalf("argument check %q not found", marker)
		}
		if at < closureAt {
			t.Errorf("the %q check returns before recordFetch exists, so the abandoned channel leaves no trace on the run row", marker)
		}
	}
}

// TestFetchChannelCoverageWriteSurvivesCancellation pins the context choice. The
// dominant cause of the fetch errors worth recording is that this very ctx was
// canceled, and RecordChannelFetch opens a transaction on the context it is
// given — so recording the loss would fail for the same reason the fetch did.
func TestFetchChannelCoverageWriteSurvivesCancellation(t *testing.T) {
	src, err := os.ReadFile("tool_fetch_channel.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	if !strings.Contains(string(src), "context.WithoutCancel(ctx)") {
		t.Error("the coverage write must not inherit cancellation from the request context")
	}
}
