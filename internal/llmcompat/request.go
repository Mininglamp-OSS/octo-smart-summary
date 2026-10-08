// Package llmcompat centralizes model-specific Chat Completions request rules.
package llmcompat

import "github.com/Mininglamp-OSS/octo-smart-summary/internal/config"

const kimiRequiredTemperature = 0.6

// ThinkingParam controls provider-specific reasoning behavior.
type ThinkingParam struct {
	Type string `json:"type"`
}

// Temperature returns the temperature accepted by the selected model.
func Temperature(model string, requested float64) float64 {
	if config.IsKimiModel(model) {
		return kimiRequiredTemperature
	}
	return requested
}

// ThinkingConfig returns the request fields needed to disable model reasoning.
// When thinking is enabled explicitly, providers receive no disabling override.
func ThinkingConfig(model string, enableThinking bool) (*ThinkingParam, map[string]interface{}) {
	if enableThinking {
		return nil, nil
	}
	if config.IsKimiModel(model) {
		return &ThinkingParam{Type: "disabled"}, nil
	}
	if config.IsQwenOrDeepSeekModel(model) {
		return nil, map[string]interface{}{"enable_thinking": false}
	}
	return nil, nil
}
