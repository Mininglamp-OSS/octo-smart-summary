package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

// octo_search_fetch.go implements the Layer 4 octo-search-batch data source.
//
// Pure helpers handle normalization, deduplication, reverse lookup maps,
// <=30-channel batching, and 413 bisection. fetchViaBatch orchestrates submit,
// poll, download, parse, 413 splitting/window shrinking, abort handling,
// and per-batch failure isolation. service.OctoSearchBatchClient stays
// stateless; polling and split policy live here.

// octoSearchBatchSize is the maximum channel count for one batch task.
const octoSearchBatchSize = 30

const (
	octoSearchPollInterval = 5 * time.Second  // API-recommended polling interval.
	octoSearchTotalBudget  = 20 * time.Minute // Total client-side budget; upstream ctx has no deadline.
	octoSearchDefaultConc  = 10               // Default concurrency, matching existing fetchConcurrency.
	octoSearchMinWindowSec = int64(6 * 3600)  // Minimum single-channel split window before skipping.
	octoSearchDeleteBudget = 5 * time.Second  // Independent timeout for best-effort Delete.
)

// normalizeAndIndexCandidates normalizes candidate channel IDs to the storage
// form used by the octo-search indexer and builds a normalized-ID -> ChannelInfo
// reverse lookup for SourceName / ChannelType backfill after batch export.
//
// The old MySQL helper performed this normalization internally. After switching
// Layer 4 to batch, it must happen before Submit; otherwise DM IDs can miss the
// indexed key and return empty results. Duplicate normalized IDs keep the first
// candidate, empty IDs are skipped, and ChannelInfo.ChannelID is overwritten
// with the normalized ID to avoid later use of the original form.
func normalizeAndIndexCandidates(candidates []ChannelInfo, creatorUID string) ([]string, map[string]ChannelInfo) {
	ids := make([]string, 0, len(candidates))
	byID := make(map[string]ChannelInfo, len(candidates))
	for _, ch := range candidates {
		if ch.ChannelID == "" {
			continue
		}
		norm := NormalizeDMChannelID(ch.ChannelID, creatorUID, ch.ChannelType)
		if norm == "" {
			continue
		}
		if _, seen := byID[norm]; seen {
			continue
		}
		ch.ChannelID = norm // Store only the normalized ID in the reverse map.
		byID[norm] = ch
		ids = append(ids, norm)
	}
	return ids, byID
}

// chunkChannelIDs splits normalized channel IDs into batches of at most size.
// Invalid sizes are clamped to the API limit to avoid oversized submissions.
func chunkChannelIDs(ids []string, size int) [][]string {
	if size <= 0 || size > octoSearchBatchSize {
		size = octoSearchBatchSize
	}
	var batches [][]string
	for i := 0; i < len(ids); i += size {
		end := i + size
		if end > len(ids) {
			end = len(ids)
		}
		batches = append(batches, ids[i:end])
	}
	return batches
}

// splitBatch bisects a channel batch after Submit returns 413.
//
// splittable=false means the caller cannot split by channel any further and
// should switch to time-window shrinking or failure handling. Empty input never
// produces an empty batch.
func splitBatch(chs []string) (parts [][]string, splittable bool) {
	switch {
	case len(chs) == 0:
		return nil, false
	case len(chs) == 1:
		return [][]string{chs}, false
	default:
		mid := len(chs) / 2
		return [][]string{chs[:mid], chs[mid:]}, true
	}
}

// octoSearchClient is the minimal dependency needed by fetchViaBatch. The
// concrete *service.OctoSearchBatchClient satisfies it. Keeping an interface
// here lets tests exercise orchestration without a real HTTP server.
type octoSearchClient interface {
	Submit(ctx context.Context, channelIDs []string, startTS, endTS int64) (string, error)
	Poll(ctx context.Context, taskID string) (*service.BatchStatus, error)
	Delete(ctx context.Context, taskID string) error
	DownloadAndParse(ctx context.Context, parts []service.Part) ([]service.BatchMessageRow, error)
}

// fetchViaBatch replaces Layer 4 per-channel MySQL reads with the async
// octo-search-batch API. The returned []Message keeps the same shape expected by
// the upper pipeline.
//
// fetchConcurrency preserves the existing configurable concurrency behavior.
// This function creates the total timeout because the upstream context has no
// deadline.
func fetchViaBatch(ctx context.Context, client octoSearchClient, candidates []ChannelInfo, creatorUID string, startTS, endTS int64, fetchConcurrency int) ([]Message, error) {
	if len(candidates) == 0 {
		return nil, nil // Do not submit an empty channel list.
	}
	normIDs, infoByID := normalizeAndIndexCandidates(candidates, creatorUID)
	if len(normIDs) == 0 {
		return nil, nil
	}
	batches := chunkChannelIDs(normIDs, octoSearchBatchSize)

	if fetchConcurrency <= 0 {
		fetchConcurrency = octoSearchDefaultConc
	}

	ctx, cancel := context.WithTimeout(ctx, octoSearchTotalBudget)
	defer cancel()

	var (
		mu       sync.Mutex
		all      []Message
		fatalErr error
		wg       sync.WaitGroup
		sem      = make(chan struct{}, fetchConcurrency)
	)

	for _, batch := range batches {
		mu.Lock()
		stop := fatalErr != nil
		mu.Unlock()
		if stop {
			break // Stop scheduling new batches after an aborting error.
		}
		wg.Add(1)
		go func(chs []string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()

			msgs, err := runBatchWithSplit(ctx, client, chs, startTS, endTS, infoByID)
			if err != nil {
				if isFatalBatchErr(err) {
					// Aborting errors stop the whole fetch.
					mu.Lock()
					if fatalErr == nil {
						fatalErr = err
					}
					mu.Unlock()
					cancel()
					return
				}
				// Isolatable errors skip only this batch.
				log.Printf("[pipeline-personal] octo-search batch (%d channels) isolated after failure: %v", len(chs), err)
				return
			}
			mu.Lock()
			all = append(all, msgs...)
			mu.Unlock()
		}(batch)
	}
	wg.Wait()

	if fatalErr != nil {
		return nil, fatalErr
	}
	// If the total budget expires or upstream cancels before a goroutine reports
	// an aborting error, still propagate ctx.Err(); otherwise a semaphore wait can
	// exit early and make a partial result look successful.
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// Keep Layer 4 ordering: (channel_id, message_seq) ascending.
	sort.Slice(all, func(i, j int) bool {
		if all[i].ChannelID != all[j].ChannelID {
			return all[i].ChannelID < all[j].ChannelID
		}
		return all[i].MessageSeq < all[j].MessageSeq
	})
	return all, nil
}

// isFatalBatchErr reports errors that abort the whole fetch rather than
// being isolated to a single batch. Context cancellation/deadline and client
// contract/config errors are returned instead of becoming partial success.
func isFatalBatchErr(err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, service.ErrFatalClient) ||
		errors.Is(err, service.ErrUnauthorized) ||
		errors.Is(err, service.ErrBadRequest) ||
		errors.Is(err, service.ErrTaskNotFound) ||
		errors.Is(err, service.ErrTimeRangeTooLong)
}

// runBatchWithSplit submits one batch. On 413 it first bisects by channel; when
// a single channel is still too large, it switches to time-window splitting.
// Other errors are returned for the caller to classify.
func runBatchWithSplit(ctx context.Context, client octoSearchClient, chs []string, startTS, endTS int64, infoByID map[string]ChannelInfo) ([]Message, error) {
	msgs, err := runOneBatch(ctx, client, chs, startTS, endTS, infoByID)
	if err == nil {
		return msgs, nil
	}
	if !errors.Is(err, service.ErrSingleTaskTooLarge) {
		return nil, err
	}

	// For 413, split by channel first.
	parts, splittable := splitBatch(chs)
	if splittable {
		var out []Message
		for _, p := range parts {
			sub, subErr := runBatchWithSplit(ctx, client, p, startTS, endTS, infoByID)
			if subErr != nil {
				if isFatalBatchErr(subErr) {
					return nil, subErr
				}
				// Isolate this sub-batch and keep successful siblings.
				log.Printf("[pipeline-personal] octo-search sub-batch (%d channels) isolated after split: %v", len(p), subErr)
				continue
			}
			out = append(out, sub...)
		}
		return out, nil
	}

	// A single channel is still too large; shrink the time window.
	return runWithTimeWindowSplit(ctx, client, chs, startTS, endTS, infoByID)
}

// runWithTimeWindowSplit bisects the time window for a single channel after
// 413. If the minimum window is still too large, the channel is skipped instead
// of retrying forever.
func runWithTimeWindowSplit(ctx context.Context, client octoSearchClient, chs []string, startTS, endTS int64, infoByID map[string]ChannelInfo) ([]Message, error) {
	if endTS-startTS <= octoSearchMinWindowSec {
		log.Printf("[pipeline-personal] octo-search: channel %v still 413 at min window (%ds), skipping", chs, endTS-startTS)
		return nil, nil
	}
	mid := startTS + (endTS-startTS)/2
	var out []Message
	// Recurse on both halves; aborting errors stop the fetch, isolatable errors skip only
	// that half.
	for _, w := range [2][2]int64{{startTS, mid}, {mid + 1, endTS}} {
		sub, err := runBatchWithSplit(ctx, client, chs, w[0], w[1], infoByID)
		if err != nil {
			if isFatalBatchErr(err) {
				return nil, err
			}
			log.Printf("[pipeline-personal] octo-search time-window [%d,%d] for %v isolated: %v", w[0], w[1], chs, err)
			continue
		}
		out = append(out, sub...)
	}
	return out, nil
}

// runOneBatch submits one batch, polls to a terminal status, downloads rows,
// then maps rows back to pipeline messages.
func runOneBatch(ctx context.Context, client octoSearchClient, chs []string, startTS, endTS int64, infoByID map[string]ChannelInfo) ([]Message, error) {
	taskID, err := client.Submit(ctx, chs, startTS, endTS)
	if err != nil {
		return nil, err
	}

	status, err := pollUntilTerminal(ctx, client, taskID)
	if err != nil {
		return nil, err
	}

	switch status.Status {
	case "completed", "partial":
		if status.Status == "partial" {
			for _, w := range status.Warnings {
				log.Printf("[pipeline-personal] octo-search partial truncation: channel=%s code=%s limit=%d seen=%d",
					w.ChannelID, w.Code, w.Limit, w.ActualSeen)
			}
		}
		if err := validateBatchStatusParts(status, chs); err != nil {
			return nil, err
		}
		rows, err := client.DownloadAndParse(ctx, status.Parts)
		if err != nil {
			return nil, err
		}
		return rowsToMessages(rows, infoByID)
	case "failed":
		return nil, fmt.Errorf("octo-search task %s failed: %s %s", taskID, status.ErrorCode, status.ErrorMsg)
	case "cancelled":
		return nil, fmt.Errorf("octo-search task %s cancelled", taskID)
	default:
		return nil, fmt.Errorf("octo-search task %s unexpected terminal status %q", taskID, status.Status)
	}
}

func validateBatchStatusParts(status *service.BatchStatus, chs []string) error {
	if status.ActualCount > 0 && len(status.Parts) == 0 {
		return fmt.Errorf("octo-search status actual_count=%d but no parts", status.ActualCount)
	}
	allowed := make(map[string]struct{}, len(chs))
	for _, ch := range chs {
		allowed[ch] = struct{}{}
	}
	for _, part := range status.Parts {
		for _, ch := range part.ChannelIDs {
			if _, ok := allowed[ch]; !ok {
				return fmt.Errorf("octo-search part contains unexpected channel_id %q", ch)
			}
		}
	}
	return nil
}

// pollUntilTerminal polls at a fixed interval until a terminal status. On
// cancellation or poll error it attempts best-effort Delete.
func pollUntilTerminal(ctx context.Context, client octoSearchClient, taskID string) (*service.BatchStatus, error) {
	ticker := time.NewTicker(octoSearchPollInterval)
	defer ticker.Stop()
	for {
		status, err := client.Poll(ctx, taskID)
		if err != nil {
			// Submit already succeeded, so clean up the queued/running task on
			// both context and non-context poll errors.
			bestEffortDelete(client, taskID)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		if status.IsTerminal() {
			return status, nil
		}
		select {
		case <-ctx.Done():
			bestEffortDelete(client, taskID)
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// bestEffortDelete uses an independent short timeout because the caller's
// context may already be cancelled.
func bestEffortDelete(client octoSearchClient, taskID string) {
	dctx, cancel := context.WithTimeout(context.Background(), octoSearchDeleteBudget)
	defer cancel()
	if err := client.Delete(dctx, taskID); err != nil {
		log.Printf("[pipeline-personal] octo-search best-effort delete %s failed: %v", taskID, err)
	}
}

// rowsToMessages maps batch rows into pipeline.Message. Payload is already plain
// text from octo-search-batch, so no ExtractText pass is needed. Empty payload
// rows are skipped, matching the old path where ExtractText returned !ok.
func rowsToMessages(rows []service.BatchMessageRow, infoByID map[string]ChannelInfo) ([]Message, error) {
	msgs := make([]Message, 0, len(rows))
	for _, r := range rows {
		info, ok := infoByID[r.ChannelID]
		if !ok {
			return nil, fmt.Errorf("octo-search row contains unexpected channel_id %q", r.ChannelID)
		}
		if r.Payload == "" {
			continue
		}
		m := Message{
			MessageSeq: r.MessageSeq,
			SenderUID:  r.FromUID,
			ChannelID:  r.ChannelID,
			Timestamp:  r.Timestamp,
			SendTime:   time.Unix(r.Timestamp, 0).Format(time.RFC3339),
			Content:    r.Payload,
		}
		m.SourceName = info.ChannelName
		m.ChannelType = info.ChannelType
		msgs = append(msgs, m)
	}
	return msgs, nil
}
