package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestKimiThinkingDisabled_Call(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{"choices":[{"message":{"content":"hello"}}],"usage":{"total_tokens":10}}`))
	}))
	defer srv.Close()

	client := NewLLMClient(srv.URL, "key", "tencent/kimi-k2.6", 30, 4096, false, 30)
	content, tokens, err := client.Call(context.Background(), []ChatMessage{{Role: "user", Content: "hi"}}, 0.1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if content != "hello" {
		t.Errorf("expected content=hello, got %q", content)
	}
	if tokens != 10 {
		t.Errorf("expected tokens=10, got %d", tokens)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(capturedBody, &parsed); err != nil {
		t.Fatalf("unmarshal captured body: %v", err)
	}

	thinking, ok := parsed["thinking"]
	if !ok {
		t.Fatal("expected thinking field in request body for kimi model")
	}
	thinkingMap := thinking.(map[string]interface{})
	if thinkingMap["type"] != "disabled" {
		t.Errorf("expected thinking.type=disabled, got %v", thinkingMap["type"])
	}

	if _, ok := parsed["chat_template_kwargs"]; ok {
		t.Error("kimi model should not have chat_template_kwargs")
	}

	temp := parsed["temperature"].(float64)
	if temp != 0.6 {
		t.Errorf("expected temperature=0.6 for kimi model, got %.2f", temp)
	}
}

func TestKimiThinkingDisabled_CallWithTools(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{"choices":[{"message":{"tool_calls":[{"function":{"name":"resolve_topic","arguments":"{\"topic\":\"test\"}"}}]}}],"usage":{"total_tokens":50}}`))
	}))
	defer srv.Close()

	client := NewLLMClient(srv.URL, "key", "tencent/kimi-k2.6", 30, 4096, false, 30)
	args, tokens, err := client.CallWithTools(
		context.Background(),
		[]ChatMessage{{Role: "user", Content: "analyze"}},
		[]Tool{{Type: "function", Function: ToolFunction{Name: "resolve_topic", Description: "test", Parameters: nil}}},
		"resolve_topic", 0.1,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if args != `{"topic":"test"}` {
		t.Errorf("unexpected args: %s", args)
	}
	if tokens != 50 {
		t.Errorf("expected tokens=50, got %d", tokens)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(capturedBody, &parsed); err != nil {
		t.Fatalf("unmarshal captured body: %v", err)
	}

	tc := parsed["tool_choice"]
	if tc != "auto" {
		t.Errorf("expected tool_choice=\"auto\" for kimi, got %v (%T)", tc, tc)
	}

	thinking, ok := parsed["thinking"]
	if !ok {
		t.Fatal("expected thinking field for kimi model")
	}
	thinkingMap := thinking.(map[string]interface{})
	if thinkingMap["type"] != "disabled" {
		t.Errorf("expected thinking.type=disabled, got %v", thinkingMap["type"])
	}

	if _, ok := parsed["chat_template_kwargs"]; ok {
		t.Error("kimi model should not have chat_template_kwargs")
	}

	temp := parsed["temperature"].(float64)
	if temp != 0.6 {
		t.Errorf("expected temperature=0.6 for kimi model, got %.2f", temp)
	}
}

func TestNonKimiModel_NoThinking(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{"choices":[{"message":{"content":"hello"}}],"usage":{"total_tokens":10}}`))
	}))
	defer srv.Close()

	client := NewLLMClient(srv.URL, "key", "claude-sonnet-4-6", 30, 4096, false, 30)
	_, _, err := client.Call(context.Background(), []ChatMessage{{Role: "user", Content: "hi"}}, 0.1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]interface{}
	json.Unmarshal(capturedBody, &parsed)

	if _, ok := parsed["thinking"]; ok {
		t.Error("claude model should not have thinking field")
	}
	if _, ok := parsed["chat_template_kwargs"]; ok {
		t.Error("claude model should not have chat_template_kwargs")
	}
}

func TestQwenModel_ChatTemplateKwargs(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{"choices":[{"message":{"content":"hello"}}],"usage":{"total_tokens":10}}`))
	}))
	defer srv.Close()

	client := NewLLMClient(srv.URL, "key", "qwen3.6-flash", 30, 4096, false, 30)
	_, _, err := client.Call(context.Background(), []ChatMessage{{Role: "user", Content: "hi"}}, 0.1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]interface{}
	json.Unmarshal(capturedBody, &parsed)

	if _, ok := parsed["thinking"]; ok {
		t.Error("qwen model should not have thinking field")
	}

	kwargs, ok := parsed["chat_template_kwargs"]
	if !ok {
		t.Fatal("expected chat_template_kwargs for qwen model")
	}
	kwargsMap := kwargs.(map[string]interface{})
	if kwargsMap["enable_thinking"] != false {
		t.Errorf("expected enable_thinking=false, got %v", kwargsMap["enable_thinking"])
	}
}

func TestKimiThinkingEnabled_NoInjection(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{"choices":[{"message":{"content":"hello"}}],"usage":{"total_tokens":10}}`))
	}))
	defer srv.Close()

	client := NewLLMClient(srv.URL, "key", "tencent/kimi-k2.6", 30, 4096, true, 30)
	_, _, err := client.Call(context.Background(), []ChatMessage{{Role: "user", Content: "hi"}}, 0.1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]interface{}
	json.Unmarshal(capturedBody, &parsed)

	if _, ok := parsed["thinking"]; ok {
		t.Error("expected no thinking field when enableThinking=true")
	}
}

func TestKimiContentEmpty_ReasoningPresent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"","reasoning_content":"I think therefore I am..."}}],"usage":{"total_tokens":100,"completion_tokens":4096}}`))
	}))
	defer srv.Close()

	client := NewLLMClient(srv.URL, "key", "tencent/kimi-k2.6", 30, 4096, false, 30)
	_, _, err := client.Call(context.Background(), []ChatMessage{{Role: "user", Content: "hi"}}, 0.1)
	if err == nil {
		t.Fatal("expected error when content is empty but reasoning present")
	}
	expected := "LLM returned empty content: reasoning consumed entire max_tokens budget"
	if err.Error() != expected {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestKimiContentEmpty_ReasoningField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"","reasoning":"Thinking about this..."}, "finish_reason":"length"}],"usage":{"total_tokens":100,"completion_tokens":4096}}`))
	}))
	defer srv.Close()

	client := NewLLMClient(srv.URL, "key", "tencent/kimi-k2.6", 30, 4096, false, 30)
	_, _, err := client.Call(context.Background(), []ChatMessage{{Role: "user", Content: "hi"}}, 0.1)
	if err == nil {
		t.Fatal("expected error when content is empty but reasoning present")
	}
	if !strings.Contains(err.Error(), "reasoning consumed entire max_tokens budget") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestKimiCallWithTools_ReasoningBudgetExhausted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"tool_calls":[],"reasoning_content":"very long reasoning..."}}],"usage":{"total_tokens":100}}`))
	}))
	defer srv.Close()

	client := NewLLMClient(srv.URL, "key", "tencent/kimi-k2.6", 5, 4096, false, 5)
	_, _, err := client.CallWithTools(
		context.Background(),
		[]ChatMessage{{Role: "user", Content: "analyze"}},
		[]Tool{{Type: "function", Function: ToolFunction{Name: "resolve_topic", Description: "test", Parameters: nil}}},
		"resolve_topic", 0.1,
	)
	if err == nil {
		t.Fatal("expected error when reasoning consumed budget")
	}
	if !strings.Contains(err.Error(), "reasoning consumed entire max_tokens budget") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestKimiToolChoice_FunctionNameValidation(t *testing.T) {
	attempt := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt++
		w.Header().Set("Content-Type", "application/json")
		if attempt == 1 {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"choices": []map[string]interface{}{{
					"message": map[string]interface{}{
						"tool_calls": []map[string]interface{}{{
							"function": map[string]interface{}{
								"name":      "wrong_function",
								"arguments": `{"topic":"test"}`,
							},
						}},
					},
				}},
				"usage": map[string]interface{}{"total_tokens": 50},
			})
		} else {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"choices": []map[string]interface{}{{
					"message": map[string]interface{}{
						"tool_calls": []map[string]interface{}{
							{
								"function": map[string]interface{}{
									"name":      "wrong_function",
									"arguments": `{"topic":"wrong"}`,
								},
							},
							{
								"function": map[string]interface{}{
									"name":      "resolve_topic",
									"arguments": `{"topic":"test"}`,
								},
							},
						},
					},
				}},
				"usage": map[string]interface{}{"total_tokens": 80},
			})
		}
	}))
	defer srv.Close()

	client := NewLLMClient(srv.URL, "key", "tencent/kimi-k2.6", 5, 4096, false, 5)
	result, _, err := client.CallWithTools(
		context.Background(),
		[]ChatMessage{{Role: "user", Content: "analyze"}},
		[]Tool{{Type: "function", Function: ToolFunction{Name: "resolve_topic", Description: "test", Parameters: nil}}},
		"resolve_topic", 0.1,
	)

	if attempt != 2 {
		t.Errorf("expected 2 attempts (retry), got %d", attempt)
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, `"topic":"test"`) {
		t.Errorf("expected resolve_topic args, got %s", result)
	}
}

func TestNonKimiModel_ForcedToolChoice(t *testing.T) {
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{"choices":[{"message":{"tool_calls":[{"function":{"name":"resolve_topic","arguments":"{\"topic\":\"test\"}"}}]}}],"usage":{"total_tokens":100}}`))
	}))
	defer srv.Close()

	client := NewLLMClient(srv.URL, "key", "qwen3.6-flash", 30, 4096, false, 30)
	_, _, err := client.CallWithTools(
		context.Background(),
		[]ChatMessage{{Role: "user", Content: "analyze"}},
		[]Tool{{Type: "function", Function: ToolFunction{Name: "resolve_topic", Description: "test", Parameters: nil}}},
		"resolve_topic", 0.1,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var parsed map[string]interface{}
	json.Unmarshal(capturedBody, &parsed)

	tc, ok := parsed["tool_choice"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected tool_choice to be object for non-kimi model, got %T", parsed["tool_choice"])
	}
	if tc["type"] != "function" {
		t.Errorf("expected tool_choice.type=function, got %v", tc["type"])
	}
	fn := tc["function"].(map[string]interface{})
	if fn["name"] != "resolve_topic" {
		t.Errorf("expected tool_choice.function.name=resolve_topic, got %v", fn["name"])
	}
}

func TestKimiToolChoice_AllRetriesFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"tool_calls":[]}}],"usage":{"total_tokens":10}}`))
	}))
	defer srv.Close()

	client := NewLLMClient(srv.URL, "key", "tencent/kimi-k2.6", 5, 4096, false, 5)
	_, _, err := client.CallWithTools(
		context.Background(),
		[]ChatMessage{{Role: "user", Content: "analyze"}},
		[]Tool{{Type: "function", Function: ToolFunction{Name: "resolve_topic", Description: "test", Parameters: nil}}},
		"resolve_topic", 0.1,
	)
	if err == nil {
		t.Fatal("expected error when all retries fail")
	}
}

func TestCallWithTools_ReasoningFieldGatewayProxy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"tool_calls":[],"reasoning":"long reasoning via gateway proxy..."}}],"usage":{"total_tokens":100}}`))
	}))
	defer srv.Close()

	client := NewLLMClient(srv.URL, "key", "tencent/kimi-k2.6", 5, 4096, false, 5)
	_, _, err := client.CallWithTools(
		context.Background(),
		[]ChatMessage{{Role: "user", Content: "analyze"}},
		[]Tool{{Type: "function", Function: ToolFunction{Name: "resolve_topic", Description: "test", Parameters: nil}}},
		"resolve_topic", 0.1,
	)
	if err == nil {
		t.Fatal("expected error when reasoning field (gateway proxy variant) consumed budget")
	}
	if !strings.Contains(err.Error(), "reasoning consumed entire max_tokens budget") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestCallMap_PropagatesReasoningBudgetError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"","reasoning_content":"very long reasoning..."}}],"usage":{"total_tokens":100,"completion_tokens":4096}}`))
	}))
	defer srv.Close()

	client := NewLLMClient(srv.URL, "key", "tencent/kimi-k2.6", 5, 4096, false, 5)
	_, _, err := client.CallMap(
		context.Background(),
		"[1] user1: hello\n[2] user2: world",
		"test-source", 0, 2,
		"2024-01-01 00:00", "2024-01-01 23:59",
		"test topic", "testuser",
	)
	if err == nil {
		t.Fatal("expected error to propagate for reasoning budget exhaustion")
	}
	if !strings.Contains(err.Error(), "reasoning budget exhausted") {
		t.Errorf("expected reasoning budget error, got: %v", err)
	}
	if strings.Contains(err.Error(), "Kimi") || strings.Contains(err.Error(), "kimi") {
		t.Errorf("error message should not contain model name, got: %v", err)
	}
}

func TestCallMap_PropagatesTokenLimitError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":""},"finish_reason":"length"}],"usage":{"total_tokens":100,"completion_tokens":4096}}`))
	}))
	defer srv.Close()

	client := NewLLMClient(srv.URL, "key", "tencent/kimi-k2.6", 5, 4096, false, 5)
	_, _, err := client.CallMap(
		context.Background(),
		"[1] user1: hello",
		"test-source", 0, 1,
		"2024-01-01 00:00", "2024-01-01 23:59",
		"test topic", "testuser",
	)
	if err == nil {
		t.Fatal("expected error to propagate for token limit exhaustion")
	}
	if !strings.Contains(err.Error(), "reasoning budget exhausted") {
		t.Errorf("expected reasoning budget exhausted error, got: %v", err)
	}
}

func TestCallMap_TransientErrorFallsBackToMarker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`internal server error`))
	}))
	defer srv.Close()

	client := NewLLMClient(srv.URL, "key", "tencent/kimi-k2.6", 5, 4096, false, 5)
	result, _, err := client.CallMap(
		context.Background(),
		"[1] user1: hello",
		"test-source", 3, 1,
		"2024-01-01 00:00", "2024-01-01 23:59",
		"test topic", "testuser",
	)
	if err != nil {
		t.Fatalf("transient errors should fall back to marker, got error: %v", err)
	}
	if !strings.Contains(result, MapFailedMarker) {
		t.Errorf("expected marker string in result, got: %s", result)
	}
}
