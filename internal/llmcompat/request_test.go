package llmcompat

import "testing"

func TestTemperature(t *testing.T) {
	tests := []struct {
		model string
		want  float64
	}{
		{model: "tencent/kimi-k2.6", want: 0.6},
		{model: "kimi_k2.5", want: 0.6},
		{model: "ali/deepseek-v3.2", want: 0.3},
	}
	for _, tt := range tests {
		if got := Temperature(tt.model, 0.3); got != tt.want {
			t.Errorf("Temperature(%q, 0.3) = %.1f, want %.1f", tt.model, got, tt.want)
		}
	}
}

func TestThinkingConfig(t *testing.T) {
	thinking, kwargs := ThinkingConfig("tencent/kimi-k2.6", false)
	if thinking == nil || thinking.Type != "disabled" || kwargs != nil {
		t.Fatalf("Kimi config = (%v, %v), want disabled thinking", thinking, kwargs)
	}

	thinking, kwargs = ThinkingConfig("deepseek-v4-flash", false)
	if thinking != nil || kwargs["enable_thinking"] != false {
		t.Fatalf("DeepSeek config = (%v, %v), want enable_thinking=false", thinking, kwargs)
	}

	thinking, kwargs = ThinkingConfig("tencent/kimi-k2.6", true)
	if thinking != nil || kwargs != nil {
		t.Fatalf("enabled thinking config = (%v, %v), want no override", thinking, kwargs)
	}
}
