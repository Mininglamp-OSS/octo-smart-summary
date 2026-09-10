package service

import "testing"

// One policy, two clients: this pins the values the gateway actually requires so
// a change here is a deliberate decision rather than a silent 400 on one path.
func TestRequestPolicyForModel(t *testing.T) {
	for _, tc := range []struct {
		name           string
		model          string
		enableThinking bool
		wantTemp       float64
		wantThinking   bool
		wantKwargs     bool
	}{
		{name: "kimi, thinking off", model: "tencent/kimi-k2.6", wantTemp: 0.6, wantThinking: true},
		{name: "kimi underscore spelling", model: "mlamp/kimi_k2.5", wantTemp: 0.6, wantThinking: true},
		{name: "kimi, thinking on", model: "kimi-k2.6", enableThinking: true, wantTemp: 1.0},
		{name: "qwen, thinking off", model: "qwen3.6-flash", wantTemp: 0.3, wantKwargs: true},
		{name: "deepseek, thinking on", model: "deepseek-v4-flash", enableThinking: true, wantTemp: 0.3},
		{name: "unknown model is left alone", model: "gpt-4.1", wantTemp: 0.3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := RequestPolicyForModel(tc.model, tc.enableThinking, 0.3)
			if policy.Temperature != tc.wantTemp {
				t.Errorf("Temperature = %v, want %v", policy.Temperature, tc.wantTemp)
			}
			if (policy.Thinking != nil) != tc.wantThinking {
				t.Errorf("Thinking = %+v, want present=%v", policy.Thinking, tc.wantThinking)
			}
			if policy.Thinking != nil && policy.Thinking.Type != "disabled" {
				t.Errorf("Thinking.Type = %q, want \"disabled\"", policy.Thinking.Type)
			}
			if _, present := policy.ChatTemplateKwargs["enable_thinking"]; present != tc.wantKwargs {
				t.Errorf("ChatTemplateKwargs = %v, want enable_thinking present=%v", policy.ChatTemplateKwargs, tc.wantKwargs)
			}
		})
	}
}
