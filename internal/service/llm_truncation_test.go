package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	// The RAW body, with no notice concatenated. The disclosure belongs to the
	// caller: while this client appended it, the caller's own emptiness check ran
	// against text the client had already added to, so it could never fire.
	if content != "partial merged summary" {
		t.Fatalf("content = %q, want the raw model body with no client-appended notice", content)
	}
}

// P1-1. A whitespace-only truncated completion is not "" but is not a
// deliverable either. Letting it take the disclose branch hands the caller a
// body that is pure whitespace; once the caller appends TruncationNotice the
// user's entire summary IS the notice, and on the agent path the completeness
// gate is cleared by a Reduce that produced nothing.
func TestCallDisclosingTruncationRejectsWhitespaceOnlyTruncation(t *testing.T) {
	for _, body := range []string{" \n\t", "   ", "\n\n"} {
		t.Run(strconv.Quote(body), func(t *testing.T) {
			payload, err := json.Marshal(map[string]interface{}{
				"choices": []map[string]interface{}{{
					"message":       map[string]interface{}{"content": body},
					"finish_reason": "length",
				}},
				"usage": map[string]interface{}{"total_tokens": 100, "completion_tokens": 4096},
			})
			if err != nil {
				t.Fatalf("marshal stub: %v", err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(payload)
			}))
			defer srv.Close()

			client := NewLLMClient(srv.URL, "key", "test-model", 5, 4096, false, 5, nil)
			content, truncated, _, err := client.CallDisclosingTruncation(
				context.Background(), []ChatMessage{{Role: "user", Content: "go"}}, 0.1)
			if err == nil || !errors.Is(err, ErrOutputTruncated) {
				t.Fatalf("err = %v, want whitespace-only truncation to fail like empty truncation", err)
			}
			if content != "" || truncated {
				t.Fatalf("content=%q truncated=%v, want nothing delivered", content, truncated)
			}
		})
	}
}

// The streaming counterpart. This path feeds live worker code, so a
// whitespace-only truncated stream would be PERSISTED as a summary whose entire
// body is the disclosure notice.
func TestCallStreamWithTruncationNoticeRejectsWhitespaceOnlyTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\" \\n\\t\"},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	client := NewLLMClient(srv.URL, "key", "test-model", 5, 4096, false, 5, nil)
	var delivered strings.Builder
	content, _, err := client.callStreamWithTruncationNotice(
		context.Background(),
		[]ChatMessage{{Role: "user", Content: "go"}},
		0.1,
		func(delta string) error {
			delivered.WriteString(delta)
			return nil
		},
	)
	if err == nil || !errors.Is(err, ErrStreamOutputTruncated) {
		t.Fatalf("err = %v, want whitespace-only streamed truncation to fail", err)
	}
	if content != "" {
		t.Fatalf("content = %q, want nothing returned for a whitespace-only stream", content)
	}
	if strings.Contains(delivered.String(), "输出因长度限制被截断") {
		t.Fatalf("delivered = %q, want no bare disclosure streamed as if it were a summary", delivered.String())
	}
}

// The terminal non-streaming Reduce entry points are user-facing deliverables.
// They used to reject any truncation via CallStrict, which reproduces the
// zero-output failure this PR fixed on the agent Reduce the moment either is
// wired up.
func TestNonStreamingReduceDisclosesTruncationInsteadOfFailing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"合并结果（正文在此被截断"},"finish_reason":"length"}],"usage":{"total_tokens":100,"completion_tokens":4096}}`))
	}))
	defer srv.Close()
	client := NewLLMClient(srv.URL, "key", "test-model", 5, 4096, false, 5, nil)

	content, _, err := client.CallReduce(context.Background(),
		[]string{"分片一", "分片二"}, "群聊", "2026-08-01", "2026-08-02", 10, "")
	if err != nil {
		t.Fatalf("CallReduce must deliver a truncated terminal summary, not fail: %v", err)
	}
	if content != "合并结果（正文在此被截断"+TruncationNotice {
		t.Fatalf("CallReduce content = %q, want the partial merge plus the disclosure", content)
	}

	content, _, err = client.CallReduceByPerson(context.Background(),
		[]struct{ Name, Summary string }{{Name: "A", Summary: "a"}, {Name: "B", Summary: "b"}},
		"2026-08-01", "2026-08-02", "")
	if err != nil {
		t.Fatalf("CallReduceByPerson must deliver a truncated terminal summary, not fail: %v", err)
	}
	if content != "合并结果（正文在此被截断"+TruncationNotice {
		t.Fatalf("CallReduceByPerson content = %q, want the partial merge plus the disclosure", content)
	}
}

// An EMPTY truncated terminal Reduce still has nothing to deliver, so the
// disclosing switch above must not turn it into a bare notice.
func TestNonStreamingReduceStillRejectsEmptyTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":" \n\t"},"finish_reason":"length"}],"usage":{"total_tokens":100,"completion_tokens":4096}}`))
	}))
	defer srv.Close()
	client := NewLLMClient(srv.URL, "key", "test-model", 5, 4096, false, 5, nil)

	content, _, err := client.CallReduce(context.Background(),
		[]string{"分片一", "分片二"}, "群聊", "2026-08-01", "2026-08-02", 10, "")
	if err == nil || !errors.Is(err, ErrOutputTruncated) {
		t.Fatalf("err = %v content = %q, want an empty truncated Reduce to still fail", err, content)
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
