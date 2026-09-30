package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

const (
	fetchSummaryScopeTool        = "fetch_summary_scope"
	summaryScopeFetchConcurrency = 4
)

type summaryScopeFetchStateKey struct{}

type summaryScopeFetchRecord struct {
	channelID string
	messages  []pipeline.Message
	total     int
	truncated bool
	err       string
	fatalErr  error
	succeeded bool
}

type summaryScopeFetchState struct {
	mu      sync.Mutex
	records map[string]summaryScopeFetchRecord
}

func withSummaryScopeFetchState(ctx context.Context) context.Context {
	return context.WithValue(ctx, summaryScopeFetchStateKey{}, &summaryScopeFetchState{
		records: make(map[string]summaryScopeFetchRecord),
	})
}

func summaryScopeFetchStateFromContext(ctx context.Context) *summaryScopeFetchState {
	state, _ := ctx.Value(summaryScopeFetchStateKey{}).(*summaryScopeFetchState)
	if state == nil {
		state = &summaryScopeFetchState{records: make(map[string]summaryScopeFetchRecord)}
	}
	return state
}

func summaryScopeFetchStateAvailable(ctx context.Context) bool {
	state, _ := ctx.Value(summaryScopeFetchStateKey{}).(*summaryScopeFetchState)
	return state != nil
}

// FetchSummaryScopeTool fetches the complete authoritative workspace scope in
// one planner step. The model supplies no channel IDs or time bounds: both are
// read from trusted request context. Successful channels are cached across
// repeated calls in the same run, so a retry only re-fetches failed channels.
func FetchSummaryScopeTool() (Tool, Handler) {
	_, fetchOne := FetchChannelTool()
	return newFetchSummaryScopeTool(fetchOne)
}

func newFetchSummaryScopeTool(fetchOne Handler) (Tool, Handler) {
	schema := Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        fetchSummaryScopeTool,
			Description: "批量抓取服务端已确认的全部总结频道和时间范围。只传空对象{}；返回一个聚合messages_handle及逐频道覆盖状态。若failure_count大于0应重试，重试只处理失败频道；每次返回的messages_handle都会取代上一次，只使用最新handle。",
			Parameters: map[string]interface{}{
				"type":                 "object",
				"additionalProperties": false,
				"properties":           map[string]interface{}{},
			},
		},
	}

	handler := func(ctx context.Context, args json.RawMessage) (string, error) {
		var empty map[string]interface{}
		if err := json.Unmarshal(args, &empty); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if len(empty) != 0 {
			return "", errors.New("fetch_summary_scope requires an empty JSON object")
		}

		uid, _ := ctx.Value(ContextKeyUID).(string)
		sessionID, _ := ctx.Value(ContextKeySessionID).(string)
		if uid == "" || sessionID == "" {
			return "", errors.New("missing user or session identity in context")
		}
		channels := AllowedChannelScopes(ctx)
		if len(channels) == 0 {
			return "", errors.New("summary scope has no channels")
		}
		// Keep archived-thread discovery narrow: every authoritative thread in
		// the scope is eligible for the selected-thread query path, while the DB
		// membership join remains the access-control boundary. Do not forward the
		// client-originated IsArchived bit as include_archived=true, which would
		// widen discovery to all archived threads.
		ctx = withSummaryScopeSelectedThreads(ctx, channels)
		start, end := ResolveAllowedTimeRange(ctx, time.Time{}, time.Time{})
		if start.IsZero() || end.IsZero() || !end.After(start) {
			return "", errors.New("summary scope has no valid authoritative time range")
		}

		state := summaryScopeFetchStateFromContext(ctx)
		var wg sync.WaitGroup
		// Keep fan-out below the DB-heavy legacy fetch path's wider worker pool.
		sem := make(chan struct{}, summaryScopeFetchConcurrency)
		for _, channel := range channels {
			channel := channel
			key := summaryScopeChannelKey(channel, start, end)
			state.mu.Lock()
			alreadySucceeded := state.records[key].succeeded
			state.mu.Unlock()
			if alreadySucceeded {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				record := summaryScopeFetchRecord{channelID: channel.ChannelID}
				defer func() {
					if p := recover(); p != nil {
						record.err = fmt.Sprintf("fetch_channel panicked: %v", p)
						record.fatalErr = errors.New(record.err)
					}
					state.mu.Lock()
					existing := state.records[key]
					if record.succeeded || !existing.succeeded {
						state.records[key] = record
					}
					state.mu.Unlock()
				}()
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-ctx.Done():
					record.err = ctx.Err().Error()
					return
				}
				callArgs, _ := json.Marshal(map[string]interface{}{
					"channel_id":   channel.ChannelID,
					"channel_type": channel.ChannelType,
					"time_start":   start.Format(time.RFC3339),
					"time_end":     end.Format(time.RFC3339),
				})
				out, err := fetchOne(ctx, callArgs)
				if err != nil {
					record.err = err.Error()
					if classifyToolError("fetch_channel", err).Fatal {
						record.fatalErr = err
					}
				} else {
					var result struct {
						Total          int    `json:"total"`
						MessagesHandle string `json:"messages_handle"`
						Truncated      bool   `json:"truncated"`
					}
					if decodeErr := json.Unmarshal([]byte(out), &result); decodeErr != nil || result.MessagesHandle == "" {
						record.err = "invalid fetch_channel result"
					} else if messages, ok := messageCache.RetrieveOK(result.MessagesHandle, uid, sessionID); !ok {
						record.err = "fetch_channel returned an unavailable messages_handle"
					} else {
						record.messages = append([]pipeline.Message(nil), messages...)
						record.total = result.Total
						record.truncated = result.Truncated
						record.succeeded = true
					}
				}
			}()
		}
		wg.Wait()
		if err := ctx.Err(); err != nil {
			return "", err
		}

		type channelResult struct {
			ChannelID   string `json:"channel_id"`
			ChannelType int    `json:"channel_type"`
			Status      string `json:"status"`
			Total       int    `json:"total,omitempty"`
			Truncated   bool   `json:"truncated,omitempty"`
			Error       string `json:"error,omitempty"`
		}
		results := make([]channelResult, 0, len(channels))
		aggregate := make([]pipeline.Message, 0)
		seenMessages := make(map[string]struct{})
		successCount := 0
		truncated := false
		var fatalErr error
		state.mu.Lock()
		for _, channel := range channels {
			record := state.records[summaryScopeChannelKey(channel, start, end)]
			item := channelResult{ChannelID: channel.ChannelID, ChannelType: channel.ChannelType}
			if record.succeeded {
				item.Status = "succeeded"
				item.Total = record.total
				item.Truncated = record.truncated
				successCount++
				truncated = truncated || record.truncated
				for _, message := range record.messages {
					messageKey := fmt.Sprintf("%s:%d", message.ChannelID, message.MessageSeq)
					if _, exists := seenMessages[messageKey]; exists {
						continue
					}
					seenMessages[messageKey] = struct{}{}
					aggregate = append(aggregate, message)
				}
			} else {
				item.Status = "failed"
				item.Error = record.err
				if fatalErr == nil && record.fatalErr != nil {
					fatalErr = record.fatalErr
				}
			}
			results = append(results, item)
		}
		state.mu.Unlock()
		if fatalErr != nil && successCount == 0 {
			return "", fatalErr
		}
		if successCount == 0 {
			reasons := make([]string, 0, len(results))
			for _, result := range results {
				reasons = append(reasons, fmt.Sprintf("channel %s: %s", result.ChannelID, result.Error))
			}
			return "", fmt.Errorf("fetch_summary_scope failed for every channel: %s", strings.Join(reasons, "; "))
		}
		sort.Slice(aggregate, func(i, j int) bool {
			if aggregate[i].Timestamp != aggregate[j].Timestamp {
				return aggregate[i].Timestamp < aggregate[j].Timestamp
			}
			if aggregate[i].ChannelID != aggregate[j].ChannelID {
				return aggregate[i].ChannelID < aggregate[j].ChannelID
			}
			return aggregate[i].MessageSeq < aggregate[j].MessageSeq
		})
		handle := messageCache.Store(aggregate, uid, sessionID)
		payload := map[string]interface{}{
			"messages_handle": handle,
			"total":           len(aggregate),
			"channel_count":   len(channels),
			"success_count":   successCount,
			"failure_count":   len(channels) - successCount,
			"truncated":       truncated,
			"channels":        results,
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return "", fmt.Errorf("marshal result: %w", err)
		}
		return string(data), nil
	}
	return schema, handler
}

func summaryScopeChannelKey(channel ChannelScope, start, end time.Time) string {
	return fmt.Sprintf("%d:%s:%s:%s", channel.ChannelType, strings.TrimSpace(channel.ChannelID),
		start.UTC().Format(time.RFC3339Nano), end.UTC().Format(time.RFC3339Nano))
}

func withSummaryScopeSelectedThreads(ctx context.Context, channels []ChannelScope) context.Context {
	selected := make(map[string]bool)
	for _, id := range SelectedArchivedChannelIDs(ctx) {
		selected[id] = true
	}
	for _, channel := range channels {
		if channel.ChannelType == model.ChannelTypeThread {
			if id := strings.TrimSpace(channel.ChannelID); id != "" {
				selected[id] = true
			}
		}
	}
	if len(selected) == 0 {
		return ctx
	}
	return context.WithValue(ctx, ContextKeyAllowedArchivedChannels, selected)
}

func invalidateSummaryScopeFetchChannels(ctx context.Context, channelIDs []string) {
	state, _ := ctx.Value(summaryScopeFetchStateKey{}).(*summaryScopeFetchState)
	if state == nil || len(channelIDs) == 0 {
		return
	}
	missing := make(map[string]struct{}, len(channelIDs))
	for _, id := range channelIDs {
		if id = strings.TrimSpace(id); id != "" {
			missing[id] = struct{}{}
		}
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	for key, record := range state.records {
		if _, ok := missing[record.channelID]; ok {
			delete(state.records, key)
		}
	}
}
