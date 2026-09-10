package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The agent client used to send its own temperature and no thinking switch at
// all, so on a Kimi deployment (LLM_MODEL=tencent/kimi-k2.6, what the dev stack
// runs) the gateway rejected every turn with HTTP 400 "invalid temperature" —
// classified terminal, so no retry and no fallback, and the user saw
// `50000 summary workspace failed`. Assert the wire shape, not just that a call
// succeeds: the request body is the thing the provider validates.
func TestAgentChatAppliesPerModelRequestPolicy(t *testing.T) {
	for _, tc := range []struct {
		name           string
		model          string
		enableThinking bool
		wantTemp       float64
		wantThinking   string // "" = field must be absent
		wantKwargs     bool
	}{
		{name: "kimi disables thinking at the temperature it requires", model: "tencent/kimi-k2.6", wantTemp: 0.6, wantThinking: "disabled"},
		{name: "kimi in thinking mode requires 1.0", model: "tencent/kimi-k2.6", enableThinking: true, wantTemp: 1.0},
		{name: "qwen switches thinking off through the template kwargs", model: "qwen3.6-max", wantTemp: 0.3, wantKwargs: true},
		{name: "other models keep the agent's own temperature", model: "claude-sonnet-4-6", wantTemp: 0.3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got struct {
				Temperature float64 `json:"temperature"`
				Thinking    *struct {
					Type string `json:"type"`
				} `json:"thinking"`
				ChatTemplateKwargs map[string]interface{} `json:"chat_template_kwargs"`
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if err := json.Unmarshal(body, &got); err != nil {
					t.Errorf("request body is not JSON: %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"total_tokens":5}}`))
			}))
			defer srv.Close()

			client := NewClient(srv.URL, "key", tc.model, 5, 4096, nil, tc.enableThinking)
			if _, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "hi"}}, nil); err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if got.Temperature != tc.wantTemp {
				t.Errorf("temperature = %v, want %v", got.Temperature, tc.wantTemp)
			}
			switch {
			case tc.wantThinking == "" && got.Thinking != nil:
				t.Errorf("thinking = %+v, want the field omitted", *got.Thinking)
			case tc.wantThinking != "" && (got.Thinking == nil || got.Thinking.Type != tc.wantThinking):
				t.Errorf("thinking = %+v, want type %q", got.Thinking, tc.wantThinking)
			}
			if enabled, present := got.ChatTemplateKwargs["enable_thinking"]; tc.wantKwargs != present || (present && enabled != false) {
				t.Errorf("chat_template_kwargs = %v, want enable_thinking=false present=%v", got.ChatTemplateKwargs, tc.wantKwargs)
			}
		})
	}
}
