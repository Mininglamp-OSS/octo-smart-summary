package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCallUsesCallerSpecificLengthTruncationPolicy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"partial summary"},"finish_reason":"length"}],"usage":{"total_tokens":100,"completion_tokens":50}}`))
	}))
	defer srv.Close()

	client := NewLLMClient(srv.URL, "key", "test-model", 5, 4096, false, 5, nil)
	content, _, err := client.Call(context.Background(), []ChatMessage{{Role: "user", Content: "go"}}, 0.1)
	if err != nil || content != "partial summary" {
		t.Fatalf("generic Call = (%q, %v), want usable partial content", content, err)
	}
	if _, _, err := client.CallStrict(context.Background(), []ChatMessage{{Role: "user", Content: "go"}}, 0.1); err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("strict error = %v, want non-empty length truncation to fail", err)
	}
}

// A terminal deliverable (the agent Reduce) must survive a non-empty length
// truncation with an explicit disclosure. Rejecting it, as CallStrict does,
// leaves the caller with nothing to deliver and no way to shrink the retry.
func TestCallDisclosingTruncationKeepsNonEmptyPartialAndFlagsIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"partial merged summary"},"finish_reason":"length"}],"usage":{"total_tokens":100,"completion_tokens":50}}`))
	}))
	defer srv.Close()

	client := NewLLMClient(srv.URL, "key", "test-model", 5, 4096, false, 5, nil)
	content, truncated, _, err := client.CallDisclosingTruncation(context.Background(), []ChatMessage{{Role: "user", Content: "go"}}, 0.1)
	if err != nil {
		t.Fatalf("disclosed truncation should remain usable: %v", err)
	}
	if !truncated {
		t.Fatal("truncated flag = false, want the degradation reported to the caller")
	}
	if !strings.Contains(content, "partial merged summary") || !strings.Contains(content, "输出因长度限制被截断") {
		t.Fatalf("content = %q, want partial content plus the shared disclosure notice", content)
	}
	if content != "partial merged summary"+TruncationNotice {
		t.Fatalf("content = %q, want the streaming path's notice wording appended verbatim", content)
	}
}

// Empty truncation has nothing to deliver and nothing to disclose against, so
// it stays an error under every policy.
func TestCallDisclosingTruncationStillRejectsEmptyTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":""},"finish_reason":"length"}],"usage":{"total_tokens":100,"completion_tokens":4096}}`))
	}))
	defer srv.Close()

	client := NewLLMClient(srv.URL, "key", "test-model", 5, 4096, false, 5, nil)
	content, truncated, _, err := client.CallDisclosingTruncation(context.Background(), []ChatMessage{{Role: "user", Content: "go"}}, 0.1)
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("error = %v, want empty length truncation to fail", err)
	}
	if content != "" || truncated {
		t.Fatalf("content=%q truncated=%v, want no bare disclosure emitted as a deliverable", content, truncated)
	}
}

func TestCallStreamPreservesOrDisclosesNonEmptyLengthTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	client := NewLLMClient(srv.URL, "key", "test-model", 5, 4096, false, 5, nil)
	content, _, err := client.CallStream(context.Background(), []ChatMessage{{Role: "user", Content: "go"}}, 0.1, nil)
	if err != nil || content != "partial" {
		t.Fatalf("generic CallStream = (%q, %v), want historical usable partial content", content, err)
	}

	var delivered strings.Builder
	content, _, err = client.callStreamWithTruncationNotice(
		context.Background(),
		[]ChatMessage{{Role: "user", Content: "go"}},
		0.1,
		func(delta string) error {
			delivered.WriteString(delta)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("disclosed stream truncation should remain usable: %v", err)
	}
	if content != delivered.String() || !strings.Contains(content, "partial") || !strings.Contains(content, "输出因长度限制被截断") {
		t.Fatalf("returned=%q delivered=%q, want identical partial content plus disclosure", content, delivered.String())
	}
}

func TestCallWithToolsRejectsLengthTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"tool_calls":[{"function":{"name":"resolve_topic","arguments":"{\"topic\":\"partial\"}"}}]},"finish_reason":"length"}],"usage":{"total_tokens":100}}`))
	}))
	defer srv.Close()

	client := NewLLMClient(srv.URL, "key", "test-model", 5, 4096, false, 5, nil)
	_, _, err := client.CallWithTools(
		context.Background(),
		[]ChatMessage{{Role: "user", Content: "go"}},
		[]Tool{{Type: "function", Function: ToolFunction{Name: "resolve_topic"}}},
		"resolve_topic", 0.1,
	)
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("error = %v, want tool length truncation to fail", err)
	}
}
