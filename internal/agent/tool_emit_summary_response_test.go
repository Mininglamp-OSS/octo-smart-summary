package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline"
)

func TestEmitSummaryResponseSeparatesReplyFromPreview(t *testing.T) {
	_, handler := EmitSummaryResponseTool()
	args := json.RawMessage(`{
		"result_type":"agent_preview",
		"reply":"已按默认条件生成一版。",
		"execution_target":"agent_preview",
		"preview":{"content":"# 风险总结\n正文","version":1,"assumptions":["最近 7 天"]}
	}`)

	outcome, err := handler(context.Background(), args)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if outcome.VisibleContent != "已按默认条件生成一版。" {
		t.Fatalf("VisibleContent = %q", outcome.VisibleContent)
	}
	if strings.Contains(outcome.VisibleContent, "# 风险总结") {
		t.Fatal("preview document leaked into the visible reply")
	}
	var payload SummaryResponsePayload
	if err := json.Unmarshal(outcome.Payload, &payload); err != nil {
		t.Fatalf("payload is not reusable structured JSON: %v", err)
	}
	if payload.Preview == nil || payload.Preview.Content != "# 风险总结\n正文" {
		t.Fatalf("preview payload lost: %+v", payload.Preview)
	}
}

func TestSetSummaryScopeUsesOnlyDiscoveredChannelsAndOverridesTimeRange(t *testing.T) {
	_, handler := SetSummaryScopeTool()
	ctx := context.WithValue(context.Background(), ContextKeyUID, "actor")
	ctx = WithDiscoverableChannelScopeForUser(ctx, "actor", []ChannelScope{{ChannelID: "group-a", ChannelType: 2, ChannelName: "A群"}})
	AuthorizeDiscoveredChannels(ctx, []pipeline.ChannelInfo{{ChannelID: "group-b", ChannelType: 2, ChannelName: "B群"}})

	_, err := handler(ctx, json.RawMessage(`{
		"source_mode":"replace",
		"channels":[{"channel_id":"group-b","channel_type":2}],
		"time_range":{"start":"2026-08-22T00:00:00+08:00","end":"2026-09-04T23:59:59+08:00","label":"最近两周"}
	}`))
	if err != nil {
		t.Fatalf("set scope: %v", err)
	}
	change, ok := DeclaredWorkspaceScopeChange(ctx)
	if !ok || change.SourceMode != WorkspaceSourceReplace || len(change.Channels) != 1 || change.Channels[0].ChannelID != "group-b" {
		t.Fatalf("declared change = %#v", change)
	}
	start, end := ResolveAllowedTimeRange(ctx, time.Time{}, time.Time{})
	if start.Format(time.RFC3339) != "2026-08-22T00:00:00+08:00" || end.Format(time.RFC3339) != "2026-09-04T23:59:59+08:00" {
		t.Fatalf("resolved range = %s..%s", start, end)
	}
	allowed := AllowedChannelScopes(ctx)
	if len(allowed) != 1 || allowed[0].ChannelID != "group-b" {
		t.Fatalf("allowed channels = %#v", allowed)
	}
}

func TestSetSummaryScopeRejectsUndiscoveredChannel(t *testing.T) {
	_, handler := SetSummaryScopeTool()
	ctx := context.WithValue(context.Background(), ContextKeyUID, "actor")
	ctx = WithDiscoverableChannelScopeForUser(ctx, "actor", []ChannelScope{{ChannelID: "group-a", ChannelType: 2}})
	_, err := handler(ctx, json.RawMessage(`{"source_mode":"replace","channels":[{"channel_id":"group-b","channel_type":2}]}`))
	if err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("undiscovered channel error = %v", err)
	}
}

func TestSetSummaryScopeRejectsEmptySourceMode(t *testing.T) {
	_, handler := SetSummaryScopeTool()
	ctx := context.WithValue(context.Background(), ContextKeyUID, "actor")
	ctx = WithDiscoverableChannelScopeForUser(ctx, "actor", []ChannelScope{{ChannelID: "group-a", ChannelType: 2}})
	_, err := handler(ctx, json.RawMessage(`{"source_mode":"","time_range":{"start":"2026-09-01T00:00:00Z","end":"2026-09-02T00:00:00Z"}}`))
	if err == nil || !strings.Contains(err.Error(), "invalid workspace source mode") {
		t.Fatalf("empty source_mode error = %v", err)
	}
	if _, declared := DeclaredWorkspaceScopeChange(ctx); declared {
		t.Fatal("invalid empty source_mode froze scope declaration")
	}
}

func TestSetSummaryScopeRejectsChangesAfterMessageFetch(t *testing.T) {
	_, handler := SetSummaryScopeTool()
	ctx := context.WithValue(context.Background(), ContextKeyUID, "actor")
	ctx = WithDiscoverableChannelScopeForUser(ctx, "actor", []ChannelScope{{ChannelID: "group-a", ChannelType: 2}})
	ctx = WithSummaryCitationTracking(ctx)
	markSummaryCitationEvidence(ctx, citationTestMessages("group-a", 1, 1))

	_, err := handler(ctx, json.RawMessage(`{
		"source_mode":"keep",
		"time_range":{"start":"2026-09-01T00:00:00+08:00","end":"2026-09-04T23:59:59+08:00"}
	}`))
	if err == nil || !strings.Contains(err.Error(), "before fetching messages") {
		t.Fatalf("post-fetch scope change error = %v", err)
	}
}

func TestSetSummaryScopeExtendExcludesUnselectedDiscoveryResults(t *testing.T) {
	_, handler := SetSummaryScopeTool()
	ctx := context.WithValue(context.Background(), ContextKeyUID, "actor")
	ctx = WithDiscoverableChannelScopeForUser(ctx, "actor", []ChannelScope{{ChannelID: "group-a", ChannelType: 2}})
	AuthorizeDiscoveredChannels(ctx, []pipeline.ChannelInfo{
		{ChannelID: "group-b", ChannelType: 2},
		{ChannelID: "group-c", ChannelType: 2},
	})

	_, err := handler(ctx, json.RawMessage(`{
		"source_mode":"extend",
		"channels":[{"channel_id":"group-c","channel_type":2}]
	}`))
	if err != nil {
		t.Fatalf("set scope: %v", err)
	}
	allowed := AllowedChannelScopes(ctx)
	if len(allowed) != 2 || allowed[0].ChannelID != "group-a" || allowed[1].ChannelID != "group-c" {
		t.Fatalf("allowed channels = %#v, want initial plus final declared extension", allowed)
	}
}

func TestSetSummaryScopeRejectsSecondSuccessfulDeclaration(t *testing.T) {
	_, handler := SetSummaryScopeTool()
	ctx := context.WithValue(context.Background(), ContextKeyUID, "actor")
	ctx = WithDiscoverableChannelScopeForUser(ctx, "actor", []ChannelScope{{ChannelID: "group-a", ChannelType: 2}})
	AuthorizeDiscoveredChannels(ctx, []pipeline.ChannelInfo{{ChannelID: "group-b", ChannelType: 2}})
	if _, err := handler(ctx, json.RawMessage(`{"source_mode":"replace","channels":[{"channel_id":"group-b","channel_type":2}]}`)); err != nil {
		t.Fatalf("first declaration: %v", err)
	}
	if _, err := handler(ctx, json.RawMessage(`{"source_mode":"keep","time_range":{"start":"2026-09-01T00:00:00Z","end":"2026-09-02T00:00:00Z"}}`)); err == nil || !strings.Contains(err.Error(), "already declared") {
		t.Fatalf("second declaration error = %v", err)
	}
}

func TestSetSummaryScopeRejectsOversizedRangeBeforeItCanDriveFetch(t *testing.T) {
	_, handler := SetSummaryScopeTool()
	ctx := context.WithValue(context.Background(), ContextKeyUID, "actor")
	ctx = WithDiscoverableChannelScopeForUser(ctx, "actor", []ChannelScope{{ChannelID: "group-a", ChannelType: 2}})
	_, err := handler(ctx, json.RawMessage(`{
		"source_mode":"keep",
		"time_range":{"start":"2026-01-01T00:00:00Z","end":"2026-09-01T00:00:00Z"}
	}`))
	if err == nil || !strings.Contains(err.Error(), "cannot exceed 90 days") {
		t.Fatalf("oversized range error = %v", err)
	}
	start, end := ResolveAllowedTimeRange(ctx, time.Time{}, time.Time{})
	if !start.IsZero() || !end.IsZero() {
		t.Fatalf("rejected range became effective: %s..%s", start, end)
	}
}

func TestEmitSummaryResponseHonorsContextAllowlist(t *testing.T) {
	_, handler := EmitSummaryResponseTool()
	args := json.RawMessage(`{"result_type":"agent_preview","reply":"ready","execution_target":"agent_preview","preview":{"content":"body","version":1}}`)

	allowed := WithAllowedSummaryResultTypes(context.Background(), SummaryResultAgentPreview)
	if _, err := handler(allowed, args); err != nil {
		t.Fatalf("allowed result rejected: %v", err)
	}
	denied := WithAllowedSummaryResultTypes(context.Background(), SummaryResultClarification)
	if _, err := handler(denied, args); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("disallowed result accepted: %v", err)
	}
	empty := WithAllowedSummaryResultTypes(context.Background())
	if _, err := handler(empty, args); err == nil {
		t.Fatal("explicitly empty allowlist must deny all results")
	}
}

func TestEmitSummaryResponseRequiresCitationForChatBackedPreview(t *testing.T) {
	_, handler := EmitSummaryResponseTool()
	ctx := WithSummaryCitationTracking(context.Background())
	markSummaryCitationEvidence(ctx, citationTestMessages("chat", 1, 1))

	withoutCitation := json.RawMessage(`{"result_type":"agent_preview","reply":"ready","execution_target":"agent_preview","preview":{"content":"chat-backed body","version":1}}`)
	if _, err := handler(ctx, withoutCitation); err == nil || !strings.Contains(err.Error(), "must include citation markers") {
		t.Fatalf("missing citation accepted: %v", err)
	}

	withCitation := json.RawMessage(`{"result_type":"agent_preview","reply":"ready","execution_target":"agent_preview","preview":{"content":"chat-backed body [1]","version":1}}`)
	if _, err := handler(ctx, withCitation); err != nil {
		t.Fatalf("cited preview rejected: %v", err)
	}

	explanation := json.RawMessage(`{"result_type":"explanation","reply":"ready"}`)
	if _, err := handler(ctx, explanation); err != nil {
		t.Fatalf("non-preview response should not require citations: %v", err)
	}
}

// Review 5087740714 blocker 4: the guard must accept markers other than [1]
// and must not accept a bare prose "[1]" as citation coverage.
func TestEmitSummaryResponseCitationGuardAcceptsAnyMarkerNumber(t *testing.T) {
	_, handler := EmitSummaryResponseTool()
	ctx := WithSummaryCitationTracking(context.Background())
	// Evidence window of 3 messages: markers [1]..[3] are all valid citations.
	markSummaryCitationEvidence(ctx, citationTestMessages("chat", 1, 3))

	// A preview citing only [2] and [3] is legitimate — previously rejected.
	higherOnly := json.RawMessage(`{"result_type":"agent_preview","reply":"ready","execution_target":"agent_preview","preview":{"content":"chat-backed body [2] and [3]","version":1}}`)
	if _, err := handler(ctx, higherOnly); err != nil {
		t.Fatalf("preview citing [2]/[3] without [1] must be accepted: %v", err)
	}
}

func TestEmitSummaryResponseCitationGuardRejectsUnbackedLiteralMarker(t *testing.T) {
	_, handler := EmitSummaryResponseTool()
	ctx := WithSummaryCitationTracking(context.Background())
	// Evidence window of 1: marker [2] exceeds the pool, so "see [2]" is a
	// fabricated citation even though it matches the marker shape.
	markSummaryCitationEvidence(ctx, citationTestMessages("chat", 1, 1))
	prose := json.RawMessage(`{"result_type":"agent_preview","reply":"ready","execution_target":"agent_preview","preview":{"content":"details in [2] beyond the window","version":1}}`)
	if _, err := handler(ctx, prose); err == nil || !strings.Contains(err.Error(), "must include citation markers") {
		t.Fatalf("marker beyond the evidence window must not count as coverage: %v", err)
	}
}

func TestEmitSummaryResponseCitationGuardCombinesDistinctEvidenceHandles(t *testing.T) {
	_, handler := EmitSummaryResponseTool()
	ctx := WithSummaryCitationTracking(context.Background())
	markSummaryCitationEvidence(ctx, citationTestMessages("chat-a", 1, 2))
	markSummaryCitationEvidence(ctx, citationTestMessages("chat-b", 1, 2))

	args := json.RawMessage(`{"result_type":"agent_preview","reply":"ready","execution_target":"agent_preview","preview":{"content":"cross-channel body [4]","version":1}}`)
	if _, err := handler(ctx, args); err != nil {
		t.Fatalf("global citation marker [4] rejected after two evidence handles: %v", err)
	}
}

func TestEmitSummaryResponseCitationGuardUsesFrozenWindow(t *testing.T) {
	_, handler := EmitSummaryResponseTool()
	ctx := WithSummaryCitationTracking(context.Background())
	markSummaryCitationEvidence(ctx, citationTestMessages("chat", 1, 3))
	setSummaryCitationWindow(ctx, []pipeline.Message{{ChannelID: "chat", MessageSeq: 1, CitationIndex: 1}})

	args := json.RawMessage(`{"result_type":"agent_preview","reply":"ready","execution_target":"agent_preview","preview":{"content":"post-freeze body [2]","version":1}}`)
	if _, err := handler(ctx, args); err == nil || !strings.Contains(err.Error(), "must include citation markers") {
		t.Fatalf("marker outside frozen citation window accepted: %v", err)
	}
}

func citationTestMessages(channelID string, firstSeq int64, count int) []pipeline.Message {
	messages := make([]pipeline.Message, count)
	for i := range messages {
		messages[i] = pipeline.Message{ChannelID: channelID, MessageSeq: firstSeq + int64(i)}
	}
	return messages
}

func TestEmitSummaryResponseAllowsUncitedPreviewForQuietChat(t *testing.T) {
	_, handler := EmitSummaryResponseTool()
	ctx := WithSummaryCitationTracking(context.Background())
	args := json.RawMessage(`{"result_type":"agent_preview","reply":"ready","execution_target":"agent_preview","preview":{"content":"No messages were found.","version":1}}`)
	if _, err := handler(ctx, args); err != nil {
		t.Fatalf("quiet-chat preview rejected: %v", err)
	}
}

func TestEmitSummaryResponseRejectsInvalidShapes(t *testing.T) {
	_, handler := EmitSummaryResponseTool()
	tests := []struct {
		name string
		args string
		want string
	}{
		{name: "unknown type", args: `{"result_type":"draft","reply":"x"}`, want: "invalid result_type"},
		{name: "missing reply", args: `{"result_type":"explanation","reply":" "}`, want: "reply is required"},
		{name: "preview missing content", args: `{"result_type":"agent_preview","reply":"x","execution_target":"agent_preview","preview":{"content":"","version":1}}`, want: "preview content"},
		{name: "revision missing parent", args: `{"result_type":"agent_revision","reply":"x","execution_target":"agent_preview","preview":{"content":"body","version":2}}`, want: "parent_message_id"},
		{name: "completed not saved", args: `{"result_type":"workflow_completed","reply":"x","execution_target":"personal_workflow","workflow":{"task_id":1,"status":"completed","saved":false}}`, want: "saved=true"},
		{name: "plain response carries preview", args: `{"result_type":"explanation","reply":"x","preview":{"content":"body","version":1}}`, want: "cannot include"},
		{name: "unknown field", args: `{"result_type":"explanation","reply":"x","surprise":true}`, want: "unknown field"},
		{name: "two values", args: `{"result_type":"explanation","reply":"x"} {}`, want: "multiple JSON values"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := handler(context.Background(), json.RawMessage(tt.args)); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
}
