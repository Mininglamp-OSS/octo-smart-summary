//go:build cgo

package pipeline

import (
	"context"
	"testing"
	"time"
)

// TestFetchCoverageProbe pins the +1-probe semantics of FetchCoverage.Truncated:
// the query fetches effectiveMax+1 rows and the probe row is dropped, so
// Truncated is true only when the window holds MORE than the cap — a window
// with exactly effectiveMax messages is a fully covered window, not truncation.
// Before the probe, LIMIT effectiveMax made "exactly the cap" indistinguishable
// from "cut off at the cap" (false has_more).
func TestFetchCoverageProbe(t *testing.T) {
	db := setupPipelineImDB(t)
	now := time.Now().Unix()
	const ch = "probe-ch"
	payload := []byte(`{"type":1,"content":"hello"}`)
	for i := 1; i <= 5; i++ {
		if err := db.Exec(`INSERT INTO message (message_seq, from_uid, channel_id, channel_type, timestamp, payload, is_deleted) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			i, "user1", ch, 2, now, payload, 0).Error; err != nil {
			t.Fatalf("seed message %d: %v", i, err)
		}
	}

	ctx := context.Background()

	t.Run("window exceeds cap: truncated=true, probe row dropped", func(t *testing.T) {
		msgs, cov, err := FetchMessagesFromChannelWithCoverage(ctx, ch, 2, now-10, now+10, db, 1, "", 3)
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if len(msgs) != 3 {
			t.Fatalf("returned %d messages, want 3 (probe row must be dropped)", len(msgs))
		}
		if !cov.Truncated {
			t.Errorf("Truncated=false, want true: window holds 5 > cap 3")
		}
		if cov.RequestedMax != 3 {
			t.Errorf("RequestedMax=%d, want 3", cov.RequestedMax)
		}
		if cov.RowsScanned != 3 {
			t.Errorf("RowsScanned=%d, want 3 (after probe drop)", cov.RowsScanned)
		}
		if cov.Returned != 3 {
			t.Errorf("Returned=%d, want 3", cov.Returned)
		}
		// Rows are fetched DESC and reversed to ascending, so the slice holds
		// the newest cap-worth capped at exactly the requested messages.
		if msgs[0].MessageSeq != 3 || msgs[2].MessageSeq != 5 {
			t.Errorf("expected ascending seqs [3 4 5], got [%d .. %d]", msgs[0].MessageSeq, msgs[2].MessageSeq)
		}
	})

	t.Run("window exactly at cap: fully covered, truncated=false", func(t *testing.T) {
		msgs, cov, err := FetchMessagesFromChannelWithCoverage(ctx, ch, 2, now-10, now+10, db, 1, "", 5)
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if len(msgs) != 5 {
			t.Fatalf("returned %d messages, want 5", len(msgs))
		}
		if cov.Truncated {
			t.Errorf("Truncated=true for a window with exactly cap messages — false has_more regression")
		}
		if cov.Returned != 5 {
			t.Errorf("Returned=%d, want 5", cov.Returned)
		}
	})

	t.Run("window under cap: truncated=false", func(t *testing.T) {
		_, cov, err := FetchMessagesFromChannelWithCoverage(ctx, ch, 2, now-10, now+10, db, 1, "", 10)
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if cov.Truncated {
			t.Errorf("Truncated=true for an under-cap window")
		}
	})
}
