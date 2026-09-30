package llmclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCompleteNormalizesURLAndSetsHeaders(t *testing.T) {
	var path, authorization, contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		authorization = r.Header.Get("Authorization")
		contentType = r.Header.Get("Content-Type")
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	client := New(srv.URL+"/", "secret", "standard", nil, nil)
	_, err := client.Complete(context.Background(), Request{
		Messages: []Message{{Role: "user", Content: "hi"}}, MaxTokens: 10,
	}, CallOptions{MaxAttempts: 1})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if path != "/chat/completions" || authorization != "Bearer secret" || contentType != "application/json" {
		t.Fatalf("request metadata = path %q auth %q content-type %q", path, authorization, contentType)
	}
}

func TestCompleteAdaptsActualFallbackModel(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		bodies = append(bodies, body)
		if body["model"] == "primary" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	client := New(srv.URL, "key", "primary", []string{"tencent/kimi-k2.6"}, nil)
	result, err := client.Complete(context.Background(), Request{
		Messages:    []Message{{Role: "user", Content: "hi"}},
		Temperature: 0.3, MaxTokens: 10,
	}, CallOptions{MaxAttempts: 1, Backoff: func(int) time.Duration { return 0 }})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if result.Model != "tencent/kimi-k2.6" || len(bodies) != 2 {
		t.Fatalf("model=%q bodies=%d", result.Model, len(bodies))
	}
	if bodies[1]["temperature"] != 0.6 {
		t.Fatalf("fallback temperature=%v, want 0.6", bodies[1]["temperature"])
	}
	thinking, ok := bodies[1]["thinking"].(map[string]any)
	if !ok || thinking["type"] != "disabled" {
		t.Fatalf("fallback thinking=%v, want disabled", bodies[1]["thinking"])
	}
}

func TestCompleteBoundsSuccessfulResponse(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = fmt.Fprint(w, strings.Repeat("x", 33))
	}))
	defer srv.Close()

	client := New(srv.URL, "key", "primary", []string{"fallback"}, nil)
	_, err := client.Complete(context.Background(), Request{}, CallOptions{MaxAttempts: 3, MaxResponseBytes: 32})
	if err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("expected bounded response error, got %v", err)
	}
	if requests != 1 {
		t.Fatalf("oversized deterministic response requests=%d, want 1", requests)
	}
}

func TestCompleteDoesNotRetryInvalidRequestURL(t *testing.T) {
	client := New("http://bad host", "key", "primary", []string{"fallback"}, nil)
	_, err := client.Complete(context.Background(), Request{}, CallOptions{MaxAttempts: 3})
	if err == nil || !strings.Contains(err.Error(), "build LLM request") {
		t.Fatalf("expected request construction error, got %v", err)
	}
	if strings.Contains(err.Error(), "all 2 model(s) failed") {
		t.Fatalf("request construction error incorrectly reached fallback: %v", err)
	}
}

func TestStreamSetsProtocolFields(t *testing.T) {
	var accept string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept = r.Header.Get("Accept")
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":2}}\n\n")
	}))
	defer srv.Close()

	client := New(srv.URL, "key", "primary", nil, nil)
	result, err := client.Stream(context.Background(), Request{}, CallOptions{MaxAttempts: 1}, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	options, ok := body["stream_options"].(map[string]any)
	if accept != "text/event-stream" || body["stream"] != true || !ok || options["include_usage"] != true {
		t.Fatalf("stream metadata accept=%q body=%v", accept, body)
	}
	if result.Content != "ok" || result.Usage.TotalTokens != 2 {
		t.Fatalf("stream result=%+v", result)
	}
}

func TestStreamBoundsResponse(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, strings.Repeat(":", 65))
	}))
	defer srv.Close()

	client := New(srv.URL, "key", "primary", []string{"fallback"}, nil)
	_, err := client.Stream(context.Background(), Request{}, CallOptions{MaxAttempts: 1, MaxResponseBytes: 64}, nil)
	if err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("expected bounded stream error, got %v", err)
	}
	if requests != 1 {
		t.Fatalf("oversized deterministic stream requests=%d, want 1", requests)
	}
}

func TestForcedToolChoiceAdaptsForKimiFallback(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		if body["model"] == "primary" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"tool_calls":[{"function":{"name":"target","arguments":"{}"}}]}}]}`)
	}))
	defer srv.Close()

	client := New(srv.URL, "key", "primary", []string{"tencent/kimi-k2.6"}, nil)
	_, err := client.Complete(context.Background(), Request{
		Tools:      []Tool{{Type: "function", Function: ToolFunction{Name: "target"}}},
		ToolChoice: ToolChoice{Mode: ToolChoiceForced, FunctionName: "target"},
	}, CallOptions{MaxAttempts: 1})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("requests=%d, want primary and fallback", len(bodies))
	}
	if _, ok := bodies[0]["tool_choice"].(map[string]any); !ok {
		t.Fatalf("primary tool_choice=%v, want forced object", bodies[0]["tool_choice"])
	}
	if bodies[1]["tool_choice"] != "auto" {
		t.Fatalf("Kimi fallback tool_choice=%v, want auto", bodies[1]["tool_choice"])
	}
}
