package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/summaryspec"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

func TestFetchSummaryScopeAggregatesAndRetriesOnlyFailures(t *testing.T) {
	ResetForTest()
	const uid, sessionID = "user-1", "summaryws:test"
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	ctx := context.WithValue(context.Background(), ContextKeyUID, uid)
	ctx = context.WithValue(ctx, ContextKeySessionID, sessionID)
	ctx = WithAllowedChannelScope(ctx, []ChannelScope{
		{ChannelID: "group-a", ChannelType: model.ChannelTypeGroup},
		{ChannelID: "group-b", ChannelType: model.ChannelTypeGroup},
	})
	ctx = WithAllowedTimeRange(ctx, start, end)
	ctx = withSummaryScopeFetchState(ctx)

	var mu sync.Mutex
	calls := map[string]int{}
	fetchOne := func(_ context.Context, args json.RawMessage) (string, error) {
		var req struct {
			ChannelID string `json:"channel_id"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", err
		}
		mu.Lock()
		calls[req.ChannelID]++
		attempt := calls[req.ChannelID]
		mu.Unlock()
		if req.ChannelID == "group-b" && attempt == 1 {
			return "", errors.New("temporary failure")
		}
		messages := []pipeline.Message{{ChannelID: req.ChannelID, MessageSeq: 1, Timestamp: int64(attempt), Content: req.ChannelID}}
		handle := messageCache.Store(messages, uid, sessionID)
		data, _ := json.Marshal(map[string]interface{}{"total": 1, "messages_handle": handle})
		return string(data), nil
	}
	_, handler := newFetchSummaryScopeTool(fetchOne)

	first := runScopeFetch(t, ctx, handler)
	if first.SuccessCount != 1 || first.FailureCount != 1 || first.Total != 1 {
		t.Fatalf("first result = %+v", first)
	}
	if len(first.Channels) != 2 || channelFetchError(first.Channels, "group-b") != "temporary failure" {
		t.Fatalf("first channel results = %+v, want disclosed failure reason", first.Channels)
	}
	second := runScopeFetch(t, ctx, handler)
	if second.SuccessCount != 2 || second.FailureCount != 0 || second.Total != 2 {
		t.Fatalf("retry result = %+v", second)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls["group-a"] != 1 || calls["group-b"] != 2 {
		t.Fatalf("calls = %#v, want successful channel skipped on retry", calls)
	}
	if got := messageCache.Retrieve(second.MessagesHandle, uid, sessionID); len(got) != 2 {
		t.Fatalf("aggregate messages = %#v", got)
	}
}

func TestFetchSummaryScopeTreatsQuietChannelsAsSuccessfulAndCachesThem(t *testing.T) {
	ResetForTest()
	const uid, sessionID = "user-quiet", "summaryws:quiet"
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	ctx := scopeFetchTestContext(uid, sessionID, start, start.Add(24*time.Hour), []ChannelScope{
		{ChannelID: "quiet-a", ChannelType: model.ChannelTypeGroup},
		{ChannelID: "quiet-b", ChannelType: model.ChannelTypeGroup},
	})

	var mu sync.Mutex
	calls := 0
	fetchOne := func(_ context.Context, _ json.RawMessage) (string, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		// FetchChannelTool stores a nil slice for a channel with no text messages.
		handle := messageCache.Store(nil, uid, sessionID)
		data, _ := json.Marshal(map[string]interface{}{"total": 0, "messages_handle": handle})
		return string(data), nil
	}
	_, handler := newFetchSummaryScopeTool(fetchOne)

	first := runScopeFetch(t, ctx, handler)
	second := runScopeFetch(t, ctx, handler)
	for i, result := range []scopeFetchResult{first, second} {
		if result.SuccessCount != 2 || result.FailureCount != 0 || result.Total != 0 || result.MessagesHandle == "" {
			t.Fatalf("result[%d] = %+v, want two successful quiet channels", i, result)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("fetch calls = %d, want one per quiet channel across retries", calls)
	}
}

func TestFetchSummaryScopeRecoversChildPanic(t *testing.T) {
	ResetForTest()
	const uid, sessionID = "user-panic", "summaryws:panic"
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	ctx := scopeFetchTestContext(uid, sessionID, start, start.Add(24*time.Hour), []ChannelScope{
		{ChannelID: "panic-channel", ChannelType: model.ChannelTypeGroup},
	})
	_, handler := newFetchSummaryScopeTool(func(context.Context, json.RawMessage) (string, error) {
		panic("boom")
	})

	_, err := handler(ctx, json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "fetch_channel panicked: boom") {
		t.Fatalf("panic error = %v", err)
	}
	if env := classifyToolError(fetchSummaryScopeTool, err); env.ErrorCode != "INTERNAL_ERROR" || !env.Fatal {
		t.Fatalf("panic classification = %+v", env)
	}
}

func TestFetchSummaryScopeCacheIncludesTimeRange(t *testing.T) {
	ResetForTest()
	const uid, sessionID = "user-range", "summaryws:range"
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	channels := []ChannelScope{{ChannelID: "group-a", ChannelType: model.ChannelTypeGroup}}
	base := context.WithValue(context.Background(), ContextKeyUID, uid)
	base = context.WithValue(base, ContextKeySessionID, sessionID)
	base = WithAllowedChannelScope(base, channels)
	base = withSummaryScopeFetchState(base)

	var mu sync.Mutex
	calls := 0
	fetchOne := func(_ context.Context, _ json.RawMessage) (string, error) {
		mu.Lock()
		calls++
		attempt := calls
		mu.Unlock()
		handle := messageCache.Store([]pipeline.Message{{ChannelID: "group-a", MessageSeq: int64(attempt)}}, uid, sessionID)
		data, _ := json.Marshal(map[string]interface{}{"total": 1, "messages_handle": handle})
		return string(data), nil
	}
	_, handler := newFetchSummaryScopeTool(fetchOne)

	runScopeFetch(t, WithAllowedTimeRange(base, start, start.Add(24*time.Hour)), handler)
	runScopeFetch(t, WithAllowedTimeRange(base, start.Add(24*time.Hour), start.Add(48*time.Hour)), handler)
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("fetch calls = %d, want refetch after authoritative range changes", calls)
	}
}

func scopeFetchTestContext(uid, sessionID string, start, end time.Time, channels []ChannelScope) context.Context {
	ctx := context.WithValue(context.Background(), ContextKeyUID, uid)
	ctx = context.WithValue(ctx, ContextKeySessionID, sessionID)
	ctx = WithAllowedChannelScope(ctx, channels)
	ctx = WithAllowedTimeRange(ctx, start, end)
	return withSummaryScopeFetchState(ctx)
}

type scopeFetchResult struct {
	MessagesHandle string               `json:"messages_handle"`
	Total          int                  `json:"total"`
	SuccessCount   int                  `json:"success_count"`
	FailureCount   int                  `json:"failure_count"`
	Channels       []scopeChannelResult `json:"channels"`
}

type scopeChannelResult struct {
	ChannelID string `json:"channel_id"`
	Status    string `json:"status"`
	Error     string `json:"error"`
}

func channelFetchError(channels []scopeChannelResult, channelID string) string {
	for _, channel := range channels {
		if channel.ChannelID == channelID {
			return channel.Error
		}
	}
	return ""
}

func runScopeFetch(t *testing.T, ctx context.Context, handler Handler) scopeFetchResult {
	t.Helper()
	out, err := handler(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var result scopeFetchResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode result %q: %v", out, err)
	}
	return result
}

func TestScopeBatchCoverageInstructionRetriesBatchTool(t *testing.T) {
	got := buildScopeBatchCoverageGateInstruction([]summaryspec.Channel{{ChannelID: "group-b", Name: "项目群"}})
	if !strings.Contains(got, "fetch_summary_scope({})") || strings.Contains(got, "调用 fetch_channel") {
		t.Fatalf("batch repair instruction = %q", got)
	}
}
