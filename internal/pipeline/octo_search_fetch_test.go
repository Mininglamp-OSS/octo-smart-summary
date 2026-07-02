package pipeline

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

func TestChunkChannelIDs(t *testing.T) {
	mkIDs := func(n int) []string {
		ids := make([]string, n)
		for i := range ids {
			ids[i] = fmt.Sprintf("g%d", i)
		}
		return ids
	}
	cases := []struct {
		name        string
		n           int
		size        int
		wantBatches int
	}{
		{"empty", 0, 30, 0},
		{"one", 1, 30, 1},
		{"exactly 30", 30, 30, 1},
		{"31 -> 2 batches", 31, 30, 2},
		{"65 -> 3 batches", 65, 30, 3},
		{"size<=0 falls back to 30", 31, 0, 2},
		{"size>30 clamped to 30", 31, 100, 2}, // Oversized input must still be clamped.
		{"size>30 clamped, 60", 60, 50, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ids := mkIDs(c.n)
			batches := chunkChannelIDs(ids, c.size)
			if len(batches) != c.wantBatches {
				t.Fatalf("got %d batches, want %d", len(batches), c.wantBatches)
			}
			total := 0
			for _, b := range batches {
				if len(b) > octoSearchBatchSize {
					t.Fatalf("batch size %d exceeds %d (clamp failed)", len(b), octoSearchBatchSize)
				}
				total += len(b)
			}
			if total != c.n {
				t.Fatalf("total channels %d, want %d (lost or duplicated)", total, c.n)
			}
		})
	}
}

func TestNormalizeAndIndexCandidates_DedupAndBackfillMap(t *testing.T) {
	// Group channels are not changed by NormalizeDMChannelID.
	candidates := []ChannelInfo{
		{ChannelID: "g100", ChannelType: 2, ChannelName: "群A"},
		{ChannelID: "g200", ChannelType: 2, ChannelName: "群B"},
		{ChannelID: "g100", ChannelType: 2, ChannelName: "群A-dup"},
		{ChannelID: "", ChannelType: 2, ChannelName: "空"},
	}
	ids, byID := normalizeAndIndexCandidates(candidates, "self-uid")

	if len(ids) != 2 {
		t.Fatalf("got %d unique ids, want 2 (dedup/empty-skip failed): %v", len(ids), ids)
	}
	if len(byID) != 2 {
		t.Fatalf("reverse map size %d, want 2", len(byID))
	}
	if info, ok := byID["g100"]; !ok || info.ChannelName != "群A" {
		t.Fatalf("byID[g100] = %+v, want first-seen 群A", info)
	}
	if byID["g100"].ChannelName == "群A-dup" {
		t.Fatal("dedup should keep first occurrence, not the later duplicate")
	}
	// The reverse-map value should store the normalized ID.
	if byID["g100"].ChannelID != "g100" {
		t.Fatalf("map ChannelID = %q, want normalized id g100", byID["g100"].ChannelID)
	}
}

func TestNormalizeAndIndexCandidates_DMNormalizationDedup(t *testing.T) {
	// All three DM forms should normalize to the same storage ID.
	// - "bob"        : peer UID, combined with self
	// - "alice@bob"  : reversed input order
	// - "bob@alice"  : already normalized for this pair
	candidates := []ChannelInfo{
		{ChannelID: "bob", ChannelType: 1, ChannelName: "DM-bob-1"},
		{ChannelID: "alice@bob", ChannelType: 1, ChannelName: "DM-bob-2"},
		{ChannelID: "bob@alice", ChannelType: 1, ChannelName: "DM-bob-3"},
	}
	ids, byID := normalizeAndIndexCandidates(candidates, "alice")

	if len(ids) != 1 {
		t.Fatalf("3 DM forms of same peer should dedup to 1, got %d: %v", len(ids), ids)
	}
	if ids[0] != "bob@alice" {
		t.Fatalf("normalized id = %q, want bob@alice", ids[0])
	}
	if info := byID["bob@alice"]; info.ChannelName != "DM-bob-1" {
		t.Fatalf("dedup should keep first form, got %q", info.ChannelName)
	}
}

func TestSplitBatch(t *testing.T) {
	// Multiple channels are bisected without losing entries.
	got, splittable := splitBatch([]string{"a", "b", "c", "d", "e"})
	if !splittable {
		t.Fatal("multi-element batch should be splittable")
	}
	if len(got) != 2 || len(got[0])+len(got[1]) != 5 {
		t.Fatalf("split lost channels: %v", got)
	}
	if len(got[0]) != 2 || len(got[1]) != 3 {
		t.Fatalf("split halves = %d/%d, want 2/3", len(got[0]), len(got[1]))
	}
	// A single channel cannot be split further by channel.
	if parts, ok := splitBatch([]string{"solo"}); ok || len(parts) != 1 || len(parts[0]) != 1 {
		t.Fatalf("single channel: got parts=%v splittable=%v, want [[solo]] false", parts, ok)
	}
	// Empty input must not produce an empty batch.
	if parts, ok := splitBatch(nil); parts != nil || ok {
		t.Fatalf("nil input: got parts=%v splittable=%v, want nil false", parts, ok)
	}
}

// fetchViaBatch orchestration tests use a fake client instead of HTTP.

type fakeOctoClient struct {
	submitFn func(chs []string, startTS, endTS int64) (string, error)
	pollFn   func(taskID string) (*service.BatchStatus, error)
	parseFn  func(parts []service.Part) ([]service.BatchMessageRow, error)
	deleted  []string
}

func (f *fakeOctoClient) Submit(_ context.Context, chs []string, s, e int64) (string, error) {
	return f.submitFn(chs, s, e)
}
func (f *fakeOctoClient) Poll(_ context.Context, taskID string) (*service.BatchStatus, error) {
	return f.pollFn(taskID)
}
func (f *fakeOctoClient) Delete(_ context.Context, taskID string) error {
	f.deleted = append(f.deleted, taskID)
	return nil
}
func (f *fakeOctoClient) DownloadAndParse(_ context.Context, parts []service.Part) ([]service.BatchMessageRow, error) {
	return f.parseFn(parts)
}

// completedWithChannel echoes taskID as the part channel to help parseFn emit
// matching rows.
func completedWithChannel(taskID string) (*service.BatchStatus, error) {
	return &service.BatchStatus{
		Status: "completed",
		Parts:  []service.Part{{URL: "u", ChannelIDs: []string{taskID}}},
	}, nil
}

func TestFetchViaBatch_HappyPathMappingAndBackfill(t *testing.T) {
	fc := &fakeOctoClient{
		submitFn: func(chs []string, _, _ int64) (string, error) { return chs[0], nil },
		pollFn:   func(taskID string) (*service.BatchStatus, error) { return completedWithChannel(taskID) },
		parseFn: func(parts []service.Part) ([]service.BatchMessageRow, error) {
			ch := parts[0].ChannelIDs[0]
			return []service.BatchMessageRow{
				{MessageSeq: 2, FromUID: "u2", ChannelID: ch, Timestamp: 1000, Payload: "world"},
				{MessageSeq: 1, FromUID: "u1", ChannelID: ch, Timestamp: 900, Payload: "hello"},
				{MessageSeq: 3, FromUID: "u3", ChannelID: ch, Timestamp: 1100, Payload: ""}, // Empty payload should be skipped.
			}, nil
		},
	}
	candidates := []ChannelInfo{{ChannelID: "g1", ChannelType: 2, ChannelName: "群一"}}
	msgs, err := fetchViaBatch(context.Background(), fc, candidates, "self", 0, 100000, 4, time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2 (empty payload should be skipped)", len(msgs))
	}
	// Sort by (channel_id, message_seq) ascending.
	if msgs[0].MessageSeq != 1 || msgs[1].MessageSeq != 2 {
		t.Fatalf("not sorted by message_seq: %d,%d", msgs[0].MessageSeq, msgs[1].MessageSeq)
	}
	// Field mapping and reverse-map backfill.
	m := msgs[0]
	if m.SenderUID != "u1" || m.Content != "hello" || m.SourceName != "群一" || m.ChannelType != 2 {
		t.Fatalf("mapping/backfill wrong: %+v", m)
	}
	if m.SendTime == "" {
		t.Fatal("SendTime should be formatted from Timestamp")
	}
}

func TestRunOneBatch_RejectsUnexpectedRowChannel(t *testing.T) {
	fc := &fakeOctoClient{
		submitFn: func([]string, int64, int64) (string, error) { return "task-1", nil },
		pollFn: func(string) (*service.BatchStatus, error) {
			return &service.BatchStatus{
				Status: "completed",
				Parts:  []service.Part{{URL: "u", ChannelIDs: []string{"g1"}}},
			}, nil
		},
		parseFn: func([]service.Part) ([]service.BatchMessageRow, error) {
			return []service.BatchMessageRow{{MessageSeq: 1, ChannelID: "evil", Timestamp: 1, Payload: "bad"}}, nil
		},
	}
	_, err := runOneBatch(context.Background(), fc, []string{"g1"}, 0, 100, map[string]ChannelInfo{
		"g1": {ChannelID: "g1", ChannelType: 2},
	}, time.Second)
	if err == nil {
		t.Fatal("unexpected row channel must fail the batch")
	}
}

func TestRunOneBatch_RejectsUnexpectedPartChannel(t *testing.T) {
	fc := &fakeOctoClient{
		submitFn: func([]string, int64, int64) (string, error) { return "task-1", nil },
		pollFn: func(string) (*service.BatchStatus, error) {
			return &service.BatchStatus{
				Status: "completed",
				Parts:  []service.Part{{URL: "u", ChannelIDs: []string{"g1", "evil"}}},
			}, nil
		},
		parseFn: func([]service.Part) ([]service.BatchMessageRow, error) {
			t.Fatal("should not download parts with unexpected channel ids")
			return nil, nil
		},
	}
	_, err := runOneBatch(context.Background(), fc, []string{"g1"}, 0, 100, map[string]ChannelInfo{
		"g1": {ChannelID: "g1", ChannelType: 2},
	}, time.Second)
	if err == nil {
		t.Fatal("unexpected part channel must fail the batch")
	}
}

func TestRunOneBatch_RejectsPositiveCountWithoutParts(t *testing.T) {
	fc := &fakeOctoClient{
		submitFn: func([]string, int64, int64) (string, error) { return "task-1", nil },
		pollFn: func(string) (*service.BatchStatus, error) {
			return &service.BatchStatus{Status: "completed", ActualCount: 1}, nil
		},
		parseFn: func([]service.Part) ([]service.BatchMessageRow, error) {
			t.Fatal("should not parse empty parts when actual_count is positive")
			return nil, nil
		},
	}
	_, err := runOneBatch(context.Background(), fc, []string{"g1"}, 0, 100, map[string]ChannelInfo{
		"g1": {ChannelID: "g1", ChannelType: 2},
	}, time.Second)
	if err == nil {
		t.Fatal("positive actual_count without parts must fail the batch")
	}
}

func TestFetchViaBatch_FatalErrorAborts(t *testing.T) {
	fc := &fakeOctoClient{
		submitFn: func([]string, int64, int64) (string, error) { return "", service.ErrUnauthorized },
		pollFn:   func(string) (*service.BatchStatus, error) { return nil, nil },
		parseFn:  func([]service.Part) ([]service.BatchMessageRow, error) { return nil, nil },
	}
	candidates := []ChannelInfo{{ChannelID: "g1", ChannelType: 2}}
	_, err := fetchViaBatch(context.Background(), fc, candidates, "self", 0, 100000, 4, time.Second)
	if !errors.Is(err, service.ErrUnauthorized) {
		t.Fatalf("401 must abort whole fetch, got err=%v", err)
	}
}

func TestFetchViaBatch_IsolatableSkipsBadBatchKeepsRest(t *testing.T) {
	// 31 channels become [30, 1]; the large batch is isolated, the small one succeeds.
	fc := &fakeOctoClient{
		submitFn: func(chs []string, _, _ int64) (string, error) {
			if len(chs) == 1 {
				return chs[0], nil
			}
			return "", errors.New("server error 503: exhausted") // Non-typed error is isolatable.
		},
		pollFn: func(taskID string) (*service.BatchStatus, error) { return completedWithChannel(taskID) },
		parseFn: func(parts []service.Part) ([]service.BatchMessageRow, error) {
			ch := parts[0].ChannelIDs[0]
			return []service.BatchMessageRow{{MessageSeq: 1, ChannelID: ch, Timestamp: 1, Payload: "ok"}}, nil
		},
	}
	candidates := make([]ChannelInfo, 31)
	for i := range candidates {
		candidates[i] = ChannelInfo{ChannelID: fmt.Sprintf("g%02d", i), ChannelType: 2}
	}
	msgs, err := fetchViaBatch(context.Background(), fc, candidates, "self", 0, 100000, 4, time.Second)
	if err != nil {
		t.Fatalf("isolatable failure must NOT abort, got err=%v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("bad batch isolated, good batch kept → want 1 message, got %d", len(msgs))
	}
}

func TestFetchViaBatch_413SplitsByChannel(t *testing.T) {
	// 2 channels -> 413 -> split into single-channel batches -> both succeed.
	fc := &fakeOctoClient{
		submitFn: func(chs []string, _, _ int64) (string, error) {
			if len(chs) >= 2 {
				return "", service.ErrSingleTaskTooLarge
			}
			return chs[0], nil
		},
		pollFn: func(taskID string) (*service.BatchStatus, error) { return completedWithChannel(taskID) },
		parseFn: func(parts []service.Part) ([]service.BatchMessageRow, error) {
			ch := parts[0].ChannelIDs[0]
			return []service.BatchMessageRow{{MessageSeq: 1, ChannelID: ch, Timestamp: 1, Payload: "x"}}, nil
		},
	}
	candidates := []ChannelInfo{
		{ChannelID: "c1", ChannelType: 2},
		{ChannelID: "c2", ChannelType: 2},
	}
	msgs, err := fetchViaBatch(context.Background(), fc, candidates, "self", 0, 100000, 4, time.Second)
	if err != nil {
		t.Fatalf("413 should split, not fail: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("413 split should yield 2 messages (one per channel), got %d", len(msgs))
	}
}

func TestFetchViaBatch_EmptyCandidates(t *testing.T) {
	fc := &fakeOctoClient{
		submitFn: func([]string, int64, int64) (string, error) {
			t.Fatal("should not submit on empty candidates")
			return "", nil
		},
		pollFn:  func(string) (*service.BatchStatus, error) { return nil, nil },
		parseFn: func([]service.Part) ([]service.BatchMessageRow, error) { return nil, nil },
	}
	msgs, err := fetchViaBatch(context.Background(), fc, nil, "self", 0, 100000, 4, time.Second)
	if err != nil || msgs != nil {
		t.Fatalf("empty candidates → (nil,nil), got msgs=%v err=%v", msgs, err)
	}
}

// A non-context Poll error happens after Submit succeeded, so Delete must run.
func TestFetchViaBatch_PollErrorTriggersDelete(t *testing.T) {
	fc := &fakeOctoClient{
		submitFn: func([]string, int64, int64) (string, error) { return "task-leak-check", nil },
		pollFn:   func(string) (*service.BatchStatus, error) { return nil, errors.New("network boom") }, // Non-context error.
		parseFn:  func([]service.Part) ([]service.BatchMessageRow, error) { return nil, nil },
	}
	candidates := []ChannelInfo{{ChannelID: "g1", ChannelType: 2}}
	// Plain errors are isolatable, but the submitted task must still be deleted.
	_, err := fetchViaBatch(context.Background(), fc, candidates, "self", 0, 100000, 4, time.Second)
	if err != nil {
		t.Fatalf("plain poll error should be isolatable (nil err), got %v", err)
	}
	if len(fc.deleted) != 1 || fc.deleted[0] != "task-leak-check" {
		t.Fatalf("Poll error must trigger best-effort Delete, deleted=%v", fc.deleted)
	}
}

// One failed leaf must not discard successful sibling data.
func TestFetchViaBatch_413SplitKeepsSuccessfulSiblingOnIsolatableFailure(t *testing.T) {
	fc := &fakeOctoClient{
		submitFn: func(chs []string, _, _ int64) (string, error) {
			if len(chs) >= 2 {
				return "", service.ErrSingleTaskTooLarge // Whole batch 413 -> split into [c1] and [c2].
			}
			if chs[0] == "c2" {
				return "", errors.New("server error 503: exhausted") // Isolatable non-sentinel error.
			}
			return chs[0], nil // c1 succeeds.
		},
		pollFn: func(taskID string) (*service.BatchStatus, error) { return completedWithChannel(taskID) },
		parseFn: func(parts []service.Part) ([]service.BatchMessageRow, error) {
			ch := parts[0].ChannelIDs[0]
			return []service.BatchMessageRow{{MessageSeq: 1, ChannelID: ch, Timestamp: 1, Payload: "kept"}}, nil
		},
	}
	candidates := []ChannelInfo{
		{ChannelID: "c1", ChannelType: 2},
		{ChannelID: "c2", ChannelType: 2},
	}
	msgs, err := fetchViaBatch(context.Background(), fc, candidates, "self", 0, 100000, 4, time.Second)
	if err != nil {
		t.Fatalf("isolatable sibling failure must NOT fail the whole batch: %v", err)
	}
	if len(msgs) != 1 || msgs[0].ChannelID != "c1" || msgs[0].Content != "kept" {
		t.Fatalf("successful sibling c1 must be kept, got %+v", msgs)
	}
}

// Unknown 4xx errors are wrapped as service.ErrFatalClient and must abort the
// whole fetch instead of being isolated.
func TestFetchViaBatch_UnknownClientErrorAborts(t *testing.T) {
	fc := &fakeOctoClient{
		submitFn: func([]string, int64, int64) (string, error) {
			return "", fmt.Errorf("%w (Submit: unexpected status 403)", service.ErrFatalClient)
		},
		pollFn:  func(string) (*service.BatchStatus, error) { return nil, nil },
		parseFn: func([]service.Part) ([]service.BatchMessageRow, error) { return nil, nil },
	}
	candidates := []ChannelInfo{{ChannelID: "g1", ChannelType: 2}}
	_, err := fetchViaBatch(context.Background(), fc, candidates, "self", 0, 100000, 4, time.Second)
	if !errors.Is(err, service.ErrFatalClient) {
		t.Fatalf("unknown 4xx must abort whole fetch, got err=%v", err)
	}
}

// Context cancellation/deadline must propagate instead of becoming partial
// success.
func TestFetchViaBatch_ContextDeadlinePropagates(t *testing.T) {
	fc := &fakeOctoClient{
		// Simulate a stuck Submit that returns only after the context is done.
		submitFn: func([]string, int64, int64) (string, error) {
			return "", context.DeadlineExceeded
		},
		pollFn:  func(string) (*service.BatchStatus, error) { return nil, nil },
		parseFn: func([]service.Part) ([]service.BatchMessageRow, error) { return nil, nil },
	}
	candidates := []ChannelInfo{{ChannelID: "g1", ChannelType: 2}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := fetchViaBatch(ctx, fc, candidates, "self", 0, 100000, 4, time.Second)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ctx deadline must propagate (not be swallowed as isolatable), got err=%v", err)
	}
}
