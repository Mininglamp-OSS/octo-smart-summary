package agent

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAgentLLMRejectsLengthTruncatedToolCall(t *testing.T) {
	t.Setenv(toolDiagnosticRunEnv, "truncated-tool-call")
	var logs bytes.Buffer
	previousLogWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousLogWriter) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
            "choices":[{
                "message":{"content":"","tool_calls":[{
                    "id":"call_1","type":"function",
                    "function":{"name":"merge_summaries","arguments":"{\"summary_handles\":[\"map_1\""}
                }]},
                "finish_reason":"length"
            }],
            "usage":{"total_tokens":12000}
        }`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "key", "test-model", 5, 12000, nil)
	ctx := context.WithValue(context.Background(), ContextKeyRunID, "truncated-tool-call")
	_, err := client.Chat(ctx, []Message{{Role: "user", Content: "summarize"}}, []Tool{{
		Type: "function", Function: ToolFunction{Name: "merge_summaries"},
	}})
	if err == nil || !strings.Contains(err.Error(), "finish_reason=length") {
		t.Fatalf("error = %v, want explicit length-truncation failure", err)
	}
	for _, expected := range []string{
		"phase=response",
		"finish=length",
		"tool=merge_summaries",
		"json_valid=false",
		"syntax_offset=",
	} {
		if !strings.Contains(logs.String(), expected) {
			t.Fatalf("truncated tool response diagnostic missing %q: %s", expected, logs.String())
		}
	}
}

func TestAgentLLMDisclosesLengthTruncatedContentAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
            "choices":[{
                "message":{"content":"partial final answer","tool_calls":null},
                "finish_reason":"length"
            }],
            "usage":{"total_tokens":12000}
        }`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "key", "test-model", 5, 12000, nil)
	turn, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "summarize"}}, nil)
	if err != nil {
		t.Fatalf("content-only truncation should remain usable: %v", err)
	}
	if !strings.Contains(turn.Content, "partial final answer") || !strings.Contains(turn.Content, "输出因长度限制被截断") {
		t.Fatalf("truncated answer missing content or disclosure: %q", turn.Content)
	}
}
