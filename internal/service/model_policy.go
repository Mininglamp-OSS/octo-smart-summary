package service

import "github.com/Mininglamp-OSS/octo-smart-summary/internal/config"

// Temperatures the LLM gateway *requires* for the Kimi K2 series — it is not a
// quality knob here. Verified against the gateway (POST /chat/completions,
// model kimi-k2.6): with `thinking:{"type":"disabled"}` only 0.6 is accepted
// (0.3 and 1.0 both come back 400 "invalid temperature: only 1 is allowed for
// this model"), and with thinking left on only 1.0 is accepted (0.3 and 0.6
// come back 400 "temperature must be 1.0 in thinking mode"). Temperature and
// the thinking switch therefore have to be chosen together, which is why they
// live in one policy instead of at each request site.
const (
	kimiRequiredTemperature         = 0.6
	kimiThinkingRequiredTemperature = 1.0
)

// ModelRequestPolicy carries the parts of a chat-completions request that depend
// on which model is being called rather than on what the caller asked for.
type ModelRequestPolicy struct {
	Temperature        float64
	Thinking           *ThinkingParam
	ChatTemplateKwargs map[string]interface{}
}

// RequestPolicyForModel is the single source of truth for those model-specific
// parts. Both LLM clients go through it: this package's LLMClient (summary
// pipeline) and internal/agent's Client (agent chat / summary workspace). They
// used to decide independently, and the agent one decided nothing at all — on a
// Kimi deployment every agent turn was rejected with a terminal HTTP 400, which
// surfaced to the user as `50000 summary workspace failed`.
//
// enableThinking mirrors LLM_ENABLE_THINKING: false (the default) means "ask the
// provider to skip reasoning", which for the models that need an explicit switch
// is expressed differently per family.
func RequestPolicyForModel(model string, enableThinking bool, temperature float64) ModelRequestPolicy {
	policy := ModelRequestPolicy{Temperature: temperature}
	switch {
	case config.IsKimiModel(model):
		if enableThinking {
			policy.Temperature = kimiThinkingRequiredTemperature
			return policy
		}
		policy.Temperature = kimiRequiredTemperature
		policy.Thinking = &ThinkingParam{Type: "disabled"}
	case config.IsQwenOrDeepSeekModel(model):
		if enableThinking {
			return policy
		}
		policy.ChatTemplateKwargs = map[string]interface{}{"enable_thinking": false}
	}
	return policy
}
