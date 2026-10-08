package service

import (
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/llmclient"
)

func TestSharedRequestBuilderThinkingConfiguration(t *testing.T) {
	tests := []struct {
		name              string
		model             string
		enableThinking    bool
		wantDisableKwargs bool
	}{
		{name: "qwen disabled", model: "qwen3.6-max", wantDisableKwargs: true},
		{name: "deepseek disabled", model: "deepseek-v4-flash", wantDisableKwargs: true},
		{name: "deepseek enabled", model: "deepseek-v4-flash", enableThinking: true},
		{name: "claude", model: "claude-haiku-4-5"},
		{name: "qwen enabled", model: "qwen3.6-plus", enableThinking: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := llmclient.MarshalRequest(tt.model, llmclient.Request{
				Messages:       []llmclient.Message{{Role: "user", Content: "hello"}},
				Temperature:    0.3,
				MaxTokens:      4096,
				EnableThinking: tt.enableThinking,
			}, false)
			if err != nil {
				t.Fatalf("MarshalRequest: %v", err)
			}
			var parsed map[string]any
			if err := json.Unmarshal(body, &parsed); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			kwargs, present := parsed["chat_template_kwargs"].(map[string]any)
			if tt.wantDisableKwargs {
				if !present || kwargs["enable_thinking"] != false {
					t.Fatalf("chat_template_kwargs=%v, want enable_thinking=false", parsed["chat_template_kwargs"])
				}
			} else if present {
				t.Fatalf("chat_template_kwargs unexpectedly present: %v", kwargs)
			}
		})
	}
}

func TestSharedRequestBuilderForcedToolUsesCompatibilityFields(t *testing.T) {
	body, err := llmclient.MarshalRequest("qwen3.6-flash", llmclient.Request{
		Messages:       []llmclient.Message{{Role: "user", Content: "hello"}},
		Tools:          []llmclient.Tool{{Type: "function", Function: llmclient.ToolFunction{Name: "test"}}},
		ToolChoice:     llmclient.ToolChoice{Mode: llmclient.ToolChoiceForced, FunctionName: "test"},
		Temperature:    0.3,
		MaxTokens:      4096,
		EnableThinking: false,
	}, false)
	if err != nil {
		t.Fatalf("MarshalRequest: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	kwargs, ok := parsed["chat_template_kwargs"].(map[string]any)
	if !ok || kwargs["enable_thinking"] != false {
		t.Fatalf("chat_template_kwargs=%v, want enable_thinking=false", parsed["chat_template_kwargs"])
	}
	choice, ok := parsed["tool_choice"].(map[string]any)
	if !ok || choice["type"] != "function" {
		t.Fatalf("tool_choice=%v, want forced function", parsed["tool_choice"])
	}
}
