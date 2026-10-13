//go:build cgo

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent/summaryrun"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/config"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/model"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

// Stub only the LLM call: splitting, capping, accounting, JSON serialization,
// handle storage and run-row disclosure all use the production handler.
func TestSummarizeChunkHandlerDisclosesCap(t *testing.T) {
	for _, mode := range []string{"off", "on"} {
		for _, inputCount := range []int{2, 260} {
			for _, blank := range []bool{false, true} {
				t.Run(fmt.Sprintf("v2=%s/input=%d/blank=%t", mode, inputCount, blank), func(t *testing.T) {
					t.Setenv("AGENT_SUMMARY_V2_MODE", mode)
					db := newFirstTurnTestDB(t)
					const uid, sessionID = "cap-user", "cap-session"
					ctx := context.Background()
					run, _, err := summaryrun.NewStore(db).CreateOrGetRun(ctx, uid, sessionID, "cap-request", model.ScopePolicyClosed)
					if err != nil {
						t.Fatal(err)
					}
					cfg := config.Config{MapMaxTokens: 6000, LLMMaxToken: 1000, CharsPerTokenASCII: 4, CharsPerTokenCJK: 1, AgentMapConcurrency: 3}
					depsMu.RLock()
					oldDeps, oldSet := summDeps, depsSet
					depsMu.RUnlock()
					SetSummaryDeps(db, nil, nil, cfg)
					t.Cleanup(func() { depsMu.Lock(); summDeps, depsSet = oldDeps, oldSet; depsMu.Unlock() })

					// Two messages exceed the 2000-token input budget, so token pressure
					// produces one chunk per message despite a 200-message count limit.
					keptCount := inputCount
					if keptCount > 256 {
						keptCount = 256
					}
					droppedCount := inputCount - keptCount
					capped := droppedCount > 0
					wantChunkCount := keptCount
					if blank {
						wantChunkCount = 0
					}
					messages := make([]pipeline.Message, inputCount)
					for i := range messages {
						messages[i] = pipeline.Message{ChannelID: "cap-channel", MessageSeq: int64(i + 1), Timestamp: int64(1000 + i), Content: strings.Repeat("x", 6000)}
					}
					handle := messageCache.Store(messages, uid, sessionID)
					t.Cleanup(func() { messageCache.mu.Lock(); delete(messageCache.store, handle); messageCache.mu.Unlock() })
					if err := seedEvidence(db, uid, sessionID, handle, messages); err != nil {
						t.Fatal(err)
					}
					var mu sync.Mutex
					seen := map[int]bool{}
					withStubMapCall(t, func(_ context.Context, chunk []map[string]interface{}, _ string) (string, int, int, error) {
						if len(chunk) != 1 {
							return "", 0, 0, fmt.Errorf("chunk contains %d messages, want 1", len(chunk))
						}
						index := chunk[0]["citation_index"].(int)
						mu.Lock()
						seen[index] = true
						mu.Unlock()
						if blank {
							return " ", 1, 0, nil
						}
						return fmt.Sprintf("summary [%d]", index), 1, 0, nil
					})
					toolCtx := withSummaryHandleStore(context.WithValue(ctx, ContextKeyUID, uid))
					toolCtx = context.WithValue(toolCtx, ContextKeySessionID, sessionID)
					toolCtx = context.WithValue(toolCtx, ContextKeyRunID, run.RunID)
					_, handler := SummarizeChunkTool()
					args, _ := json.Marshal(map[string]string{"messages_handle": handle})
					out, err := handler(toolCtx, args)
					if err != nil {
						t.Fatal(err)
					}
					var result summarizeChunkToolResult
					if err := json.Unmarshal([]byte(out), &result); err != nil {
						t.Fatal(err)
					}
					if result.InputCount != inputCount || result.ProcessedCount != keptCount || result.ChunkCount != wantChunkCount || result.DroppedCount != 0 || result.CappedDroppedCount != droppedCount || result.ChunkCallsCapped != capped || result.Truncated != capped {
						t.Fatalf("incorrect cap disclosure: %+v", result)
					}
					if len(seen) != keptCount {
						t.Fatalf("Map called on %d messages, want %d", len(seen), keptCount)
					}
					for i := 1; i <= inputCount; i++ {
						if seen[i] != (i > droppedCount) {
							t.Fatalf("citation %d kept=%t; cap must retain newest messages", i, seen[i])
						}
					}
					wantBlank := 0
					if blank {
						wantBlank = keptCount
					}
					if result.BlankChunkCount != wantBlank || result.FailedChunkCount != 0 {
						t.Fatalf("incorrect chunk counters: %+v", result)
					}
					store, err := summaryHandleStoreFromContext(toolCtx)
					if err != nil {
						t.Fatal(err)
					}
					resolved, err := store.ResolveAll([]string{result.SummaryHandle})
					if err != nil {
						t.Fatal(err)
					}
					if len(resolved.Entries) != 1 || strings.HasSuffix(resolved.Entries[0].Text, mapCoverageGapNotice) != capped {
						t.Fatalf("stored Map disclosure mismatch (capped=%t): %+v", capped, resolved.Entries)
					}
					wantBlankBody := allBlankMapBody
					if capped {
						wantBlankBody += mapCoverageGapNotice
					}
					if blank && resolved.Entries[0].Text != wantBlankBody {
						t.Fatalf("all-blank cap body=%q", resolved.Entries[0].Text)
					}
					reloaded, err := summaryrun.NewStore(db).GetByID(ctx, uid, run.RunID)
					if err != nil {
						t.Fatal(err)
					}
					wantDropped := 0
					if mode == "on" {
						wantDropped = droppedCount
					}
					if reloaded.DroppedMessages != wantDropped {
						t.Fatalf("run dropped_messages=%d, want %d", reloaded.DroppedMessages, wantDropped)
					}
				})
			}
		}
	}
}
