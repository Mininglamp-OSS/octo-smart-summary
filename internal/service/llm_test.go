package service

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChatTemplateKwargs_QwenWithThinkingDisabled(t *testing.T) {
	client := NewLLMClient("http://localhost", "key", "qwen3.6-max", 30, 4096, false)

	reqBody := chatRequest{
		Model:       client.model,
		Messages:    []ChatMessage{{Role: "user", Content: "hello"}},
		Temperature: 0.3,
		MaxTokens:   client.maxTokens,
	}
	if !client.enableThinking && (strings.Contains(client.model, "qwen3.6") || strings.Contains(client.model, "deepseek-v4")) {
		reqBody.ChatTemplateKwargs = map[string]interface{}{"enable_thinking": false}
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	kwargs, ok := parsed["chat_template_kwargs"]
	if !ok {
		t.Fatal("expected chat_template_kwargs in request body for qwen model")
	}
	kwargsMap, ok := kwargs.(map[string]interface{})
	if !ok {
		t.Fatal("chat_template_kwargs is not a map")
	}
	if val, ok := kwargsMap["enable_thinking"]; !ok || val != false {
		t.Errorf("expected enable_thinking=false, got %v", val)
	}
}

func TestChatTemplateKwargs_DeepseekV4WithThinkingDisabled(t *testing.T) {
	client := NewLLMClient("http://localhost", "key", "deepseek-v4-flash", 30, 4096, false)

	reqBody := chatRequest{
		Model:       client.model,
		Messages:    []ChatMessage{{Role: "user", Content: "hello"}},
		Temperature: 0.3,
		MaxTokens:   client.maxTokens,
	}
	if !client.enableThinking && (strings.Contains(client.model, "qwen3.6") || strings.Contains(client.model, "deepseek-v4")) {
		reqBody.ChatTemplateKwargs = map[string]interface{}{"enable_thinking": false}
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	kwargs, ok := parsed["chat_template_kwargs"]
	if !ok {
		t.Fatal("expected chat_template_kwargs in request body for deepseek-v4 model")
	}
	kwargsMap, ok := kwargs.(map[string]interface{})
	if !ok {
		t.Fatal("chat_template_kwargs is not a map")
	}
	if val, ok := kwargsMap["enable_thinking"]; !ok || val != false {
		t.Errorf("expected enable_thinking=false, got %v", val)
	}
}

func TestChatTemplateKwargs_DeepseekV4WithThinkingEnabled(t *testing.T) {
	client := NewLLMClient("http://localhost", "key", "deepseek-v4-flash", 30, 4096, true)

	reqBody := chatRequest{
		Model:       client.model,
		Messages:    []ChatMessage{{Role: "user", Content: "hello"}},
		Temperature: 0.3,
		MaxTokens:   client.maxTokens,
	}
	if !client.enableThinking && (strings.Contains(client.model, "qwen3.6") || strings.Contains(client.model, "deepseek-v4")) {
		reqBody.ChatTemplateKwargs = map[string]interface{}{"enable_thinking": false}
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if _, ok := parsed["chat_template_kwargs"]; ok {
		t.Error("expected no chat_template_kwargs when thinking is enabled for deepseek-v4")
	}
}

func TestChatTemplateKwargs_ClaudeModel(t *testing.T) {
	client := NewLLMClient("http://localhost", "key", "claude-haiku-4-5", 30, 4096, false)

	reqBody := chatRequest{
		Model:       client.model,
		Messages:    []ChatMessage{{Role: "user", Content: "hello"}},
		Temperature: 0.3,
		MaxTokens:   client.maxTokens,
	}
	if !client.enableThinking && (strings.Contains(client.model, "qwen3.6") || strings.Contains(client.model, "deepseek-v4")) {
		reqBody.ChatTemplateKwargs = map[string]interface{}{"enable_thinking": false}
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if _, ok := parsed["chat_template_kwargs"]; ok {
		t.Error("expected no chat_template_kwargs in request body for claude model")
	}
}

func TestChatTemplateKwargs_QwenWithThinkingEnabled(t *testing.T) {
	client := NewLLMClient("http://localhost", "key", "qwen3.6-plus", 30, 4096, true)

	reqBody := chatRequest{
		Model:       client.model,
		Messages:    []ChatMessage{{Role: "user", Content: "hello"}},
		Temperature: 0.3,
		MaxTokens:   client.maxTokens,
	}
	if !client.enableThinking && (strings.Contains(client.model, "qwen3.6") || strings.Contains(client.model, "deepseek-v4")) {
		reqBody.ChatTemplateKwargs = map[string]interface{}{"enable_thinking": false}
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if _, ok := parsed["chat_template_kwargs"]; ok {
		t.Error("expected no chat_template_kwargs when thinking is enabled")
	}
}

func TestChatTemplateKwargs_CallWithTools_Qwen(t *testing.T) {
	client := NewLLMClient("http://localhost", "key", "qwen3.6-flash", 30, 4096, false)

	reqBody := chatRequestWithTools{
		Model:       client.model,
		Messages:    []ChatMessage{{Role: "user", Content: "hello"}},
		Temperature: 0.3,
		MaxTokens:   client.maxTokens,
		Tools:       []Tool{{Type: "function", Function: ToolFunction{Name: "test", Description: "test", Parameters: nil}}},
		ToolChoice:  ToolChoice{Type: "function", Function: ToolChoiceFunction{Name: "test"}},
	}
	if !client.enableThinking && (strings.Contains(client.model, "qwen3.6") || strings.Contains(client.model, "deepseek-v4")) {
		reqBody.ChatTemplateKwargs = map[string]interface{}{"enable_thinking": false}
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	kwargs, ok := parsed["chat_template_kwargs"]
	if !ok {
		t.Fatal("expected chat_template_kwargs in request body for qwen model with tools")
	}
	kwargsMap, ok := kwargs.(map[string]interface{})
	if !ok {
		t.Fatal("chat_template_kwargs is not a map")
	}
	if val, ok := kwargsMap["enable_thinking"]; !ok || val != false {
		t.Errorf("expected enable_thinking=false, got %v", val)
	}
}
