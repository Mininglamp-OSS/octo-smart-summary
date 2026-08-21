package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAgentLLMRejectsLengthTruncatedToolCall(t *testing.T) {
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
	_, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "summarize"}}, []Tool{{
		Type: "function", Function: ToolFunction{Name: "merge_summaries"},
	}})
	if err == nil || !strings.Contains(err.Error(), "finish_reason=length") {
		t.Fatalf("error = %v, want explicit length-truncation failure", err)
	}
}
