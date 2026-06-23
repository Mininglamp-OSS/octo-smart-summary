package service

import (
	"encoding/json"
	"testing"
)

func TestChatTemplateKwargs_QwenWithThinkingDisabled(t *testing.T) {
	client := NewLLMClient("http://localhost", "test-fake-api-key", "qwen3.6-max", 30, 4096, false, 30)

	reqBody := chatRequest{
		Model:       client.model,
		Messages:    []ChatMessage{{Role: "user", Content: "hello"}},
		Temperature: 0.3,
		MaxTokens:   client.maxTokens,
	}
	thinking, kwargs := client.buildThinkingConfig()
	reqBody.Thinking = thinking
	reqBody.ChatTemplateKwargs = kwargs

	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	kwargs2, ok := parsed["chat_template_kwargs"]
	if !ok {
		t.Fatal("expected chat_template_kwargs in request body for qwen model")
	}
	kwargsMap, ok := kwargs2.(map[string]interface{})
	if !ok {
		t.Fatal("chat_template_kwargs is not a map")
	}
	if val, ok := kwargsMap["enable_thinking"]; !ok || val != false {
		t.Errorf("expected enable_thinking=false, got %v", val)
	}
}

func TestChatTemplateKwargs_DeepseekV4WithThinkingDisabled(t *testing.T) {
	client := NewLLMClient("http://localhost", "test-fake-api-key", "deepseek-v4-flash", 30, 4096, false, 30)

	reqBody := chatRequest{
		Model:       client.model,
		Messages:    []ChatMessage{{Role: "user", Content: "hello"}},
		Temperature: 0.3,
		MaxTokens:   client.maxTokens,
	}
	thinking, kwargs := client.buildThinkingConfig()
	reqBody.Thinking = thinking
	reqBody.ChatTemplateKwargs = kwargs

	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	kwargs2, ok := parsed["chat_template_kwargs"]
	if !ok {
		t.Fatal("expected chat_template_kwargs in request body for deepseek-v4 model")
	}
	kwargsMap, ok := kwargs2.(map[string]interface{})
	if !ok {
		t.Fatal("chat_template_kwargs is not a map")
	}
	if val, ok := kwargsMap["enable_thinking"]; !ok || val != false {
		t.Errorf("expected enable_thinking=false, got %v", val)
	}
}

func TestChatTemplateKwargs_DeepseekV4WithThinkingEnabled(t *testing.T) {
	client := NewLLMClient("http://localhost", "test-fake-api-key", "deepseek-v4-flash", 30, 4096, true, 30)

	reqBody := chatRequest{
		Model:       client.model,
		Messages:    []ChatMessage{{Role: "user", Content: "hello"}},
		Temperature: 0.3,
		MaxTokens:   client.maxTokens,
	}
	thinking, kwargs := client.buildThinkingConfig()
	reqBody.Thinking = thinking
	reqBody.ChatTemplateKwargs = kwargs

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
	client := NewLLMClient("http://localhost", "test-fake-api-key", "claude-haiku-4-5", 30, 4096, false, 30)

	reqBody := chatRequest{
		Model:       client.model,
		Messages:    []ChatMessage{{Role: "user", Content: "hello"}},
		Temperature: 0.3,
		MaxTokens:   client.maxTokens,
	}
	thinking, kwargs := client.buildThinkingConfig()
	reqBody.Thinking = thinking
	reqBody.ChatTemplateKwargs = kwargs

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
	client := NewLLMClient("http://localhost", "test-fake-api-key", "qwen3.6-plus", 30, 4096, true, 30)

	reqBody := chatRequest{
		Model:       client.model,
		Messages:    []ChatMessage{{Role: "user", Content: "hello"}},
		Temperature: 0.3,
		MaxTokens:   client.maxTokens,
	}
	thinking, kwargs := client.buildThinkingConfig()
	reqBody.Thinking = thinking
	reqBody.ChatTemplateKwargs = kwargs

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
	client := NewLLMClient("http://localhost", "test-fake-api-key", "qwen3.6-flash", 30, 4096, false, 30)

	reqBody := chatRequestWithTools{
		Model:       client.model,
		Messages:    []ChatMessage{{Role: "user", Content: "hello"}},
		Temperature: 0.3,
		MaxTokens:   client.maxTokens,
		Tools:       []Tool{{Type: "function", Function: ToolFunction{Name: "test", Description: "test", Parameters: nil}}},
		ToolChoice:  ToolChoice{Type: "function", Function: ToolChoiceFunction{Name: "test"}},
	}
	thinking, kw := client.buildThinkingConfig()
	reqBody.Thinking = thinking
	reqBody.ChatTemplateKwargs = kw

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
