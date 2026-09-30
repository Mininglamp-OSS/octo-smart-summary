package llmclient_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-smart-summary/internal/agent"
	"github.com/Mininglamp-OSS/octo-smart-summary/internal/service"
)

const contractMaxTokens = 777

func TestAgentAndWorkerCompatibilityFieldsMatch(t *testing.T) {
	tests := []struct {
		name           string
		model          string
		temperature    float64
		enableThinking bool
		want           map[string]any
	}{
		{
			name:        "kimi",
			model:       "tencent/kimi-k2.6",
			temperature: 0.3,
			want: map[string]any{
				"model":       "tencent/kimi-k2.6",
				"temperature": 0.6,
				"max_tokens":  float64(contractMaxTokens),
				"thinking":    map[string]any{"type": "disabled"},
			},
		},
		{
			name:        "qwen",
			model:       "qwen3.6-max",
			temperature: 0.3,
			want: map[string]any{
				"model":                "qwen3.6-max",
				"temperature":          0.3,
				"max_tokens":           float64(contractMaxTokens),
				"chat_template_kwargs": map[string]any{"enable_thinking": false},
			},
		},
		{
			name:        "deepseek",
			model:       "deepseek-v4-flash",
			temperature: 0.4,
			want: map[string]any{
				"model":                "deepseek-v4-flash",
				"temperature":          0.4,
				"max_tokens":           float64(contractMaxTokens),
				"chat_template_kwargs": map[string]any{"enable_thinking": false},
			},
		},
		{
			name:        "standard",
			model:       "claude-sonnet-4-6",
			temperature: 0.7,
			want: map[string]any{
				"model":       "claude-sonnet-4-6",
				"temperature": 0.7,
				"max_tokens":  float64(contractMaxTokens),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, captured := newContractServer(t, nil)
			defer server.Close()

			callAgentAndWorker(t, server.URL+"/", tt.model, nil, tt.temperature, tt.enableThinking)
			bodies := captured()
			if len(bodies) != 2 {
				t.Fatalf("captured requests = %d, want Agent and Worker requests", len(bodies))
			}

			agentFields := compatibilityFields(bodies[0])
			workerFields := compatibilityFields(bodies[1])
			if !reflect.DeepEqual(agentFields, workerFields) {
				t.Fatalf("compatibility fields differ:\nAgent: %#v\nWorker: %#v", agentFields, workerFields)
			}
			if !reflect.DeepEqual(agentFields, tt.want) {
				t.Fatalf("compatibility fields = %#v, want %#v", agentFields, tt.want)
			}
		})
	}
}

func TestAgentAndWorkerAdaptActualKimiFallbackModel(t *testing.T) {
	const primary = "claude-sonnet-4-6"
	const fallback = "tencent/kimi-k2.6"
	server, captured := newContractServer(t, func(model string) int {
		if model == primary {
			return http.StatusForbidden
		}
		return http.StatusOK
	})
	defer server.Close()

	callAgentAndWorker(t, server.URL, primary, []string{fallback}, 0.3, false)
	bodies := captured()
	if len(bodies) != 4 {
		t.Fatalf("captured requests = %d, want primary/fallback for both entry points", len(bodies))
	}
	for i, wantModel := range []string{primary, fallback, primary, fallback} {
		if got := bodies[i]["model"]; got != wantModel {
			t.Fatalf("request %d model = %v, want %q", i, got, wantModel)
		}
	}

	wantFallback := map[string]any{
		"model":       fallback,
		"temperature": 0.6,
		"max_tokens":  float64(contractMaxTokens),
		"thinking":    map[string]any{"type": "disabled"},
	}
	agentFallback := compatibilityFields(bodies[1])
	workerFallback := compatibilityFields(bodies[3])
	if !reflect.DeepEqual(agentFallback, workerFallback) {
		t.Fatalf("fallback compatibility fields differ:\nAgent: %#v\nWorker: %#v", agentFallback, workerFallback)
	}
	if !reflect.DeepEqual(agentFallback, wantFallback) {
		t.Fatalf("fallback compatibility fields = %#v, want %#v", agentFallback, wantFallback)
	}
}

func callAgentAndWorker(t *testing.T, apiURL, model string, fallbacks []string, temperature float64, enableThinking bool) {
	t.Helper()
	ctx := context.Background()
	agentClient := agent.NewClientWithModelConfig(
		apiURL, "contract-key", model, 5, contractMaxTokens, fallbacks, temperature, enableThinking,
	)
	if _, err := agentClient.Chat(ctx, []agent.Message{{Role: "user", Content: "hello"}}, nil); err != nil {
		t.Fatalf("Agent.Chat: %v", err)
	}

	workerClient := service.NewLLMClient(
		apiURL, "contract-key", model, 5, contractMaxTokens, enableThinking, 5, fallbacks,
	)
	if _, _, err := workerClient.Call(ctx, []service.ChatMessage{{Role: "user", Content: "hello"}}, temperature); err != nil {
		t.Fatalf("LLMClient.Call: %v", err)
	}
}

func newContractServer(t *testing.T, statusForModel func(string) int) (*httptest.Server, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()

		status := http.StatusOK
		if statusForModel != nil {
			status = statusForModel(fmt.Sprint(body["model"]))
		}
		if status != http.StatusOK {
			http.Error(w, "denied", status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"total_tokens":1,"completion_tokens":1}}`)
	}))
	return server, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), bodies...)
	}
}

func compatibilityFields(body map[string]any) map[string]any {
	fields := make(map[string]any, 5)
	for _, key := range []string{"model", "temperature", "max_tokens", "thinking", "chat_template_kwargs"} {
		if value, ok := body[key]; ok {
			fields[key] = value
		}
	}
	return fields
}
