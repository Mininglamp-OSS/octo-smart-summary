package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type draftTestClient func(context.Context, []Message, []Tool) (AssistantTurn, error)

func (f draftTestClient) Chat(ctx context.Context, messages []Message, tools []Tool) (AssistantTurn, error) {
	return f(ctx, messages, tools)
}

func draftTestRegistry() *Registry {
	reg := terminalRegistry(defaultTerminalHandler())
	schema, handler := PrepareSummaryDraftTool()
	reg.Register(schema, handler)
	return reg
}

func draftTestContext(client chatter) context.Context {
	ctx := withSummaryHandleStore(context.Background())
	ctx = context.WithValue(ctx, summaryDraftKey{}, &summaryDraftState{})
	return context.WithValue(ctx, summaryDraftInputKey{}, summaryDraftInput{
		client: client, messages: []Message{{Role: "system", Content: "trusted context"}, {Role: "user", Content: "move citations"}}, request: "move citations",
	})
}

func draftSubmission(handle string) string {
	data, _ := json.Marshal(map[string]interface{}{
		"result_type": SummaryResultAgentPreview, "reply": "已生成", "execution_target": "agent_preview",
		"preview": map[string]interface{}{"content_handle": handle, "version": 1},
	})
	return string(data)
}

func TestPreparedDraftToollessResponseIsNudgedWithoutRegeneration(t *testing.T) {
	for _, text := range []string{"", "Here is your summary"} {
		t.Run(text, func(t *testing.T) {
			plannerCalls, drafts := 0, 0
			client := draftTestClient(func(ctx context.Context, messages []Message, tools []Tool) (AssistantTurn, error) {
				if len(tools) == 0 {
					drafts++
					return AssistantTurn{Content: "Complete draft"}, nil
				}
				plannerCalls++
				switch plannerCalls {
				case 1:
					return AssistantTurn{ToolCalls: []ToolCall{mkToolCall("prepare", prepareSummaryDraftTool, "{}")}}, nil
				case 2:
					return AssistantTurn{Content: text}, nil
				default:
					if len(tools) != 1 || tools[0].Function.Name != "emit_summary_response" {
						t.Fatal("nudge reopened evidence tools")
					}
					return AssistantTurn{ToolCalls: []ToolCall{mkToolCall("submit", "emit_summary_response", draftSubmission(draftState(ctx).handle))}}, nil
				}
			})
			runner := NewRunner(client, draftTestRegistry(), NewPool(1), terminalPolicy(1))
			out, _, err := runner.RunWithHistoryOutcome(context.Background(), "system", nil, "request")
			if err != nil || out.Terminal == nil || drafts != 1 || plannerCalls != 3 {
				t.Fatalf("nudge lost draft: err=%v drafts=%d calls=%d", err, drafts, plannerCalls)
			}
		})
	}
}

func TestPreparedDraftToollessNudgeIsBounded(t *testing.T) {
	calls := 0
	client := draftTestClient(func(ctx context.Context, _ []Message, tools []Tool) (AssistantTurn, error) {
		if len(tools) == 0 {
			return AssistantTurn{Content: "Draft"}, nil
		}
		calls++
		if calls == 1 {
			return AssistantTurn{ToolCalls: []ToolCall{mkToolCall("prepare", prepareSummaryDraftTool, "{}")}}, nil
		}
		return AssistantTurn{}, nil
	})
	runner := NewRunner(client, draftTestRegistry(), NewPool(1), terminalPolicy(1))
	_, _, err := runner.RunWithHistoryOutcome(context.Background(), "", nil, "request")
	var draftErr *SummaryDraftError
	if !errors.As(err, &draftErr) || draftErr.Reason != "terminal_submission_missing" || calls != 4 {
		t.Fatalf("unbounded nudge: calls=%d err=%v", calls, err)
	}
}

func TestDraftPreparationArgumentRepairAtTokenBudget(t *testing.T) {
	calls, drafts := 0, 0
	client := draftTestClient(func(ctx context.Context, _ []Message, tools []Tool) (AssistantTurn, error) {
		if len(tools) == 0 {
			drafts++
			return AssistantTurn{Content: "Draft"}, nil
		}
		calls++
		switch calls {
		case 1:
			return AssistantTurn{Tokens: 2, ToolCalls: []ToolCall{mkToolCall("bad", prepareSummaryDraftTool, "{")}}, nil
		case 2:
			return AssistantTurn{ToolCalls: []ToolCall{mkToolCall("fixed", prepareSummaryDraftTool, "{}")}}, nil
		default:
			return AssistantTurn{ToolCalls: []ToolCall{mkToolCall("submit", "emit_summary_response", draftSubmission(draftState(ctx).handle))}}, nil
		}
	})
	policy := terminalPolicy(1)
	policy.MaxTokens = 1
	runner := NewRunner(client, draftTestRegistry(), NewPool(1), policy)
	out, _, err := runner.RunWithHistoryOutcome(context.Background(), "", nil, "request")
	if err != nil || out.Terminal == nil || drafts != 1 || calls != 3 {
		t.Fatalf("preparation repair failed: %v drafts=%d calls=%d", err, drafts, calls)
	}
}

func TestPreparedDraftHTTPKeepsBodyOutOfArgumentsAndRepairsOnlyControl(t *testing.T) {
	body := "# 项目进展\n引用跟在事实之后。[1]\n\n|列|\n|---|\n|\"双引号\"|\n路径 C:\\资料\\x；制表\t🙂\n```go\nfmt.Println(\"test\")\n```"
	requests, drafts, fetched := 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var input chatRequest
		if err := json.NewDecoder(req.Body).Decode(&input); err != nil {
			t.Error("invalid request")
			w.WriteHeader(400)
			return
		}
		requests++
		for _, message := range input.Messages {
			if !validToolArguments(message.ToolCalls) {
				t.Error("malformed tool arguments were replayed")
			}
			if strings.Contains(message.Content, body) {
				t.Error("draft body was copied back into planner context")
			}
		}
		var turn AssistantTurn
		switch requests {
		case 1:
			turn.ToolCalls = []ToolCall{mkToolCall("merge", "merge_summaries", "{}")}
		case 2:
			turn.ToolCalls = []ToolCall{mkToolCall("prepare", prepareSummaryDraftTool, "{}")}
		case 3:
			drafts++
			if len(input.Tools) != 0 || len(input.Messages) != 2 || input.Messages[0].Content != summaryDraftInstruction {
				t.Error("draft stage must be text-only")
			}
			if !strings.Contains(input.Messages[1].Content, "move citations") || !strings.Contains(input.Messages[1].Content, "已有证据") {
				t.Error("draft stage lost actual user request/evidence")
			}
			for _, message := range input.Messages {
				for _, call := range message.ToolCalls {
					if call.Function.Name == prepareSummaryDraftTool {
						t.Error("text request contains an unanswered preparation call")
					}
				}
			}
			turn.Content = body
		case 4, 5:
			if len(input.Tools) != 1 || input.Tools[0].Function.Name != "emit_summary_response" {
				t.Error("nonterminal tools remain available after preparation")
			}
			var handle string
			for _, message := range input.Messages {
				if message.Role == "tool" && message.Name == prepareSummaryDraftTool {
					var result struct {
						ContentHandle string `json:"content_handle"`
					}
					_ = json.Unmarshal([]byte(message.Content), &result)
					handle = result.ContentHandle
				}
			}
			if handle == "" {
				t.Error("prepared handle missing")
			}
			args := draftSubmission(handle)
			if requests == 4 {
				args = "{"
			}
			turn.ToolCalls = []ToolCall{mkToolCall("submit", "emit_summary_response", args)}
		default:
			t.Error("unexpected replay")
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []interface{}{map[string]interface{}{
				"finish_reason": "stop",
				"message":       map[string]interface{}{"content": turn.Content, "tool_calls": turn.ToolCalls},
			}},
		})
	}))
	defer server.Close()
	reg := draftTestRegistry()
	reg.Register(Tool{Type: "function", Function: ToolFunction{Name: "merge_summaries"}}, func(ctx context.Context, _ json.RawMessage) (string, error) {
		fetched++
		markSummaryCitationEvidence(ctx, citationTestMessages("channel", 1, 1))
		return `{"merged_summary":"已有证据。[1]"}`, nil
	})
	runner := NewRunner(NewClient(server.URL, "test", "test", 5, 2048, nil), reg, NewPool(2), terminalPolicy(2))
	out, history, err := runner.RunWithHistoryOutcome(WithSummaryCitationTracking(context.Background()), "system", nil, "move citations")
	if err != nil || out.Terminal == nil || requests != 5 || drafts != 1 || fetched != 1 {
		t.Fatalf("err=%v requests=%d drafts=%d fetched=%d", err, requests, drafts, fetched)
	}
	var payload SummaryResponsePayload
	if err := json.Unmarshal(out.Terminal.Payload, &payload); err != nil || payload.Preview.Content != body {
		t.Fatalf("body did not survive standard JSON serialization: %v", err)
	}
	if strings.Contains(string(out.Terminal.Payload), "content_handle") {
		t.Fatal("internal handle leaked into public response")
	}
	for _, message := range history {
		if message.Name == prepareSummaryDraftTool || strings.Contains(message.Content, "draft_") || strings.Contains(message.Content, body) {
			t.Fatal("draft or expiring handle entered durable transcript")
		}
		for _, call := range message.ToolCalls {
			if call.Function.Name == prepareSummaryDraftTool {
				t.Fatal("preparation call entered durable history")
			}
		}
	}
}

func TestPreparedDraftHandleIsScopedImmutableAndValidated(t *testing.T) {
	generations := 0
	ctx := draftTestContext(draftTestClient(func(context.Context, []Message, []Tool) (AssistantTurn, error) {
		generations++
		return AssistantTurn{Content: "保留引用。[1]"}, nil
	}))
	_, prepare := PrepareSummaryDraftTool()
	_, emit := EmitSummaryResponseTool()
	first, err := prepare(ctx, json.RawMessage("{}"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := prepare(ctx, json.RawMessage("{}"))
	if err != nil || first != second || generations != 1 {
		t.Fatal("duplicate preparation did not reuse immutable draft")
	}
	handle := draftState(ctx).handle
	if _, err := emit(ctx, json.RawMessage(draftSubmission(handle))); err != nil {
		t.Fatal(err)
	}
	for _, badContext := range []context.Context{context.Background(), draftTestContext(nil)} {
		if _, err := emit(badContext, json.RawMessage(draftSubmission(handle))); err == nil {
			t.Fatal("cross-run or expired handle accepted")
		}
	}
	for _, args := range []string{
		draftSubmission("invented"),
		validPreviewTerminalArgs,
		strings.Replace(draftSubmission(handle), `"content_handle":`, `"content":"forged","content_handle":`, 1),
		strings.Replace(draftSubmission(handle), `"version":1`, `"version":0`, 1),
		strings.Replace(draftSubmission(handle), `"version":1`, `"version":1,"effective_scope":{}`, 1),
	} {
		if _, err := emit(ctx, json.RawMessage(args)); err == nil {
			t.Fatal("forged/invalid terminal arguments accepted")
		}
	}
	denied := WithAllowedSummaryResultTypes(ctx, SummaryResultExplanation)
	if _, err := emit(denied, json.RawMessage(draftSubmission(handle))); err == nil {
		t.Fatal("handle bypassed route allowlist")
	}
	// The standard citation guard still runs after handle resolution.
	guarded := WithSummaryCitationTracking(ctx)
	markSummaryCitationEvidence(guarded, citationTestMessages("channel", 1, 1))
	draftState(ctx).text = "不合法引用。[2]"
	if _, err := emit(guarded, json.RawMessage(draftSubmission(handle))); err == nil {
		t.Fatal("handle bypassed final citation validation")
	}
}

func TestPreparedDraftRejectsIncompleteEvidenceAndMixedBatches(t *testing.T) {
	calls := 0
	client := draftTestClient(func(context.Context, []Message, []Tool) (AssistantTurn, error) {
		calls++
		return AssistantTurn{Content: "body"}, nil
	})
	_, prepare := PrepareSummaryDraftTool()
	for _, pending := range []bool{true, false} {
		ctx := draftTestContext(client)
		store, _ := summaryHandleStoreFromContext(ctx)
		if pending {
			store.MarkMapFailed("chunk", 1)
		} else {
			_, _ = store.Put("map result", 1)
		}
		if _, err := prepare(ctx, json.RawMessage("{}")); err == nil {
			t.Fatal("incomplete Map/Reduce allowed preparation")
		}
	}
	reg := draftTestRegistry()
	reg.Register(Tool{Function: ToolFunction{Name: "echo"}}, func(context.Context, json.RawMessage) (string, error) {
		calls++
		return "", nil
	})
	runner := NewRunner(client, reg, NewPool(2), terminalPolicy(2))
	results := runner.runTools(draftTestContext(client), []ToolCall{
		mkToolCall("draft", prepareSummaryDraftTool, "{}"), mkToolCall("fetch", "echo", "{}"),
	}, 1, 2)
	if calls != 0 || len(results) != 2 {
		t.Fatalf("mixed/incomplete batch executed: calls=%d", calls)
	}
}

func TestPreparedDraftFailureNeverReopensTools(t *testing.T) {
	for _, test := range []struct {
		name string
		turn AssistantTurn
		err  error
	}{
		{"empty", AssistantTurn{}, nil},
		{"truncated", AssistantTurn{Content: "part [1]", Truncated: true}, nil},
		{"tools", AssistantTurn{ToolCalls: []ToolCall{mkToolCall("bad", "echo", "{}")}}, nil},
		{"protocol", AssistantTurn{Content: "我将生成\n```tool_code\nprepare_summary_draft({})\n```"}, nil},
		{"citations", AssistantTurn{Content: "bad [2]"}, nil},
		{"transport", AssistantTurn{}, context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			client := draftTestClient(func(_ context.Context, _ []Message, tools []Tool) (AssistantTurn, error) {
				requests++
				if len(tools) != 0 {
					if requests == 1 {
						return AssistantTurn{ToolCalls: []ToolCall{mkToolCall("merge", "merge_summaries", "{}")}}, nil
					}
					return AssistantTurn{ToolCalls: []ToolCall{mkToolCall("prepare", prepareSummaryDraftTool, "{}")}}, nil
				}
				return test.turn, test.err
			})
			ctx := WithSummaryCitationTracking(context.Background())
			markSummaryCitationEvidence(ctx, citationTestMessages("channel", 1, 1))
			reg := draftTestRegistry()
			reg.Register(Tool{Function: ToolFunction{Name: "merge_summaries"}}, func(context.Context, json.RawMessage) (string, error) {
				return `{"merged_summary":"evidence [1]"}`, nil
			})
			runner := NewRunner(client, reg, NewPool(2), terminalPolicy(5))
			_, _, err := runner.RunWithHistoryOutcome(ctx, "system", nil, "request")
			var draftErr *SummaryDraftError
			if !errors.As(err, &draftErr) || requests != 3 {
				t.Fatalf("err=%v requests=%d", err, requests)
			}
		})
	}
}

func TestPreparedDraftDoesNotAllowFetchingAfterTextGeneration(t *testing.T) {
	client := &fakeClient{turns: []AssistantTurn{
		{ToolCalls: []ToolCall{mkToolCall("prepare", prepareSummaryDraftTool, "{}")}},
		{Content: "body"},
		{ToolCalls: []ToolCall{mkToolCall("unexpected", "echo", "{}")}},
	}}
	calls := 0
	reg := draftTestRegistry()
	reg.Register(Tool{Function: ToolFunction{Name: "echo"}}, func(context.Context, json.RawMessage) (string, error) {
		calls++
		return "", nil
	})
	runner := NewRunner(client, reg, NewPool(2), terminalPolicy(3))
	_, _, err := runner.RunWithHistoryOutcome(context.Background(), "system", nil, "request")
	var draftErr *SummaryDraftError
	if !errors.As(err, &draftErr) || calls != 0 {
		t.Fatalf("prepared draft reopened fetching: err=%v calls=%d", err, calls)
	}
}

func TestPreparedDraftSchemaDoesNotRequestInlineBody(t *testing.T) {
	schema, _ := EmitSummaryResponseTool()
	params := schema.Function.Parameters.(map[string]interface{})
	preview := params["properties"].(map[string]interface{})["preview"].(map[string]interface{})
	props := preview["properties"].(map[string]interface{})
	if _, inline := props["content"]; inline || props["content_handle"] == nil {
		t.Fatal("terminal tool still asks model to JSON-encode the body")
	}
}

func TestDraftWriterDoesNotReceivePlannerInstructions(t *testing.T) {
	client := draftTestClient(func(_ context.Context, messages []Message, tools []Tool) (AssistantTurn, error) {
		if len(tools) != 0 || len(messages) != 2 {
			t.Fatal("writer received tools or planner message roles")
		}
		text := messages[1].Content
		if strings.Contains(text, "PLANNER_ONLY") || strings.Contains(text, "prepare_summary_draft") {
			t.Fatal("planner control instructions leaked into text stage")
		}
		for _, want := range []string{"authorized reference", "completed analysis", "move citations", "previous explanation"} {
			if !strings.Contains(text, want) {
				t.Errorf("writer lost source data %q", want)
			}
		}
		return AssistantTurn{Content: "final text"}, nil
	})
	ctx := draftTestContext(client)
	ctx = WithSummaryDraftSource(ctx, SummaryDraftSource{References: []SummaryDraftReference{
		{Content: "authorized reference"},
	}})
	ctx = context.WithValue(ctx, summaryDraftInputKey{}, summaryDraftInput{
		client: client, request: "move citations",
		history: []Message{{Role: "assistant", Content: "previous explanation"}},
		messages: []Message{
			{Role: "system", Content: "PLANNER_ONLY prepare_summary_draft"},
			{Role: "user", Content: "PLANNER_ONLY budget instruction"},
			{Role: "tool", Name: "merge_summaries", Content: `{"merged_summary":"completed analysis"}`},
		},
	})
	_, prepare := PrepareSummaryDraftTool()
	if _, err := prepare(ctx, json.RawMessage("{}")); err != nil {
		t.Fatal(err)
	}
}

func TestDraftWriterReceivesAuthorizedEffectiveScope(t *testing.T) {
	client := draftTestClient(func(_ context.Context, messages []Message, _ []Tool) (AssistantTurn, error) {
		if !strings.Contains(messages[1].Content, `"effective_scope"`) || !strings.Contains(messages[1].Content, "authorized-channel") {
			t.Fatal("writer lost the server-declared scope")
		}
		return AssistantTurn{Content: "body"}, nil
	})
	ctx := draftTestContext(client)
	channels := []ChannelScope{{ChannelID: "authorized-channel", ChannelType: 2}}
	ctx = WithDiscoverableChannelScopeForUser(ctx, "actor", channels)
	if err := DeclareWorkspaceScopeChange(ctx, WorkspaceScopeChange{
		SourceMode: WorkspaceSourceReplace, Channels: channels,
	}); err != nil {
		t.Fatal(err)
	}
	_, prepare := PrepareSummaryDraftTool()
	if _, err := prepare(ctx, json.RawMessage("{}")); err != nil {
		t.Fatal(err)
	}
}
