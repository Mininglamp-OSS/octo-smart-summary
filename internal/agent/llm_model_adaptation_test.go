package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientAppliesModelRequestAdaptation(t *testing.T) {
	tests := []struct {
		name              string
		model             string
		configuredTemp    float64
		enableThinking    bool
		wantTemperature   float64
		wantThinking      string
		wantDisableKwargs bool
	}{
		{
			name:            "kimi",
			model:           "tencent/kimi-k2.6",
			configuredTemp:  0.3,
			wantTemperature: 0.6,
			wantThinking:    "disabled",
		},
		{
			name:              "qwen",
			model:             "qwen3.6-max",
			configuredTemp:    0.3,
			wantTemperature:   0.3,
			wantDisableKwargs: true,
		},
		{
			name:            "standard model",
			model:           "ali/deepseek-v3.2",
			configuredTemp:  0.7,
			wantTemperature: 0.7,
		},
		{
			name:            "explicit thinking remains enabled",
			model:           "tencent/kimi-k2.6",
			configuredTemp:  0.3,
			enableThinking:  true,
			wantTemperature: 0.6,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body map[string]interface{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"total_tokens":1}}`))
			}))
			defer srv.Close()

			client := NewClientWithModelConfig(srv.URL, "test-key", tt.model, 5, 512, nil, tt.configuredTemp, tt.enableThinking)
			if _, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "hello"}}, nil); err != nil {
				t.Fatalf("Chat() error: %v", err)
			}

			if got := body["temperature"]; got != tt.wantTemperature {
				t.Errorf("temperature = %v, want %.1f", got, tt.wantTemperature)
			}
			thinking, hasThinking := body["thinking"].(map[string]interface{})
			if tt.wantThinking == "" {
				if hasThinking {
					t.Errorf("thinking unexpectedly present: %v", thinking)
				}
			} else if !hasThinking || thinking["type"] != tt.wantThinking {
				t.Errorf("thinking = %v, want type=%q", body["thinking"], tt.wantThinking)
			}

			kwargs, hasKwargs := body["chat_template_kwargs"].(map[string]interface{})
			if tt.wantDisableKwargs {
				if !hasKwargs || kwargs["enable_thinking"] != false {
					t.Errorf("chat_template_kwargs = %v, want enable_thinking=false", body["chat_template_kwargs"])
				}
			} else if hasKwargs {
				t.Errorf("chat_template_kwargs unexpectedly present: %v", kwargs)
			}
		})
	}
}

func TestClientAdaptsEachFallbackModel(t *testing.T) {
	var bodies []map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		bodies = append(bodies, body)
		if body["model"] == "primary" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"denied"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"total_tokens":1}}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-key", "primary", 5, 512, []string{"tencent/kimi-k2.6"})
	if _, err := client.Chat(context.Background(), []Message{{Role: "user", Content: "hello"}}, nil); err != nil {
		t.Fatalf("Chat() error: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("request count = %d, want 2", len(bodies))
	}
	if got := bodies[1]["temperature"]; got != 0.6 {
		t.Errorf("fallback temperature = %v, want 0.6", got)
	}
	thinking, ok := bodies[1]["thinking"].(map[string]interface{})
	if !ok || thinking["type"] != "disabled" {
		t.Errorf("fallback thinking = %v, want disabled", bodies[1]["thinking"])
	}
}
